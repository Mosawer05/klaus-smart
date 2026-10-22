package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	defaultModel     = "openai"
	defaultEndpoint  = "https://text.pollinations.ai/openai"
	discordMaxMsgLen = 2000
)

type pollinationsClient struct {
	apiKey     string
	endpoint   string
	model      string
	httpClient *http.Client
}

type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func (c *pollinationsClient) generateReply(ctx context.Context, prompt string) (string, error) {
	reqBody, err := json.Marshal(chatCompletionRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var completion chatCompletionResponse
	if err := json.Unmarshal(bodyBytes, &completion); err == nil {
		if completion.Error != nil {
			return "", fmt.Errorf("API error (%s): %s", completion.Error.Type, completion.Error.Message)
		}
		if len(completion.Choices) > 0 {
			return completion.Choices[0].Message.Content, nil
		}
	}

	// Fallback if Pollinations returns plain text directly
	plainText := strings.TrimSpace(string(bodyBytes))
	if plainText == "" {
		return "", fmt.Errorf("empty response from Pollinations")
	}
	return plainText, nil
}

func main() {
	discordToken := strings.TrimSpace(os.Getenv("KLAUS_DISCORD_BOT_TOKEN"))
	pollinationsAPIKey := strings.TrimSpace(os.Getenv("KLAUS_POLLINATIONS_API_KEY"))
	modelName := strings.TrimSpace(os.Getenv("KLAUS_POLLINATIONS_MODEL"))
	if modelName == "" {
		modelName = defaultModel
	}
	endpoint := strings.TrimSpace(os.Getenv("KLAUS_POLLINATIONS_URL"))
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	if discordToken == "" {
		log.Fatal("Environment variable KLAUS_DISCORD_BOT_TOKEN is missing!")
	}

	ctx := context.Background()

	// Initialize Pollinations client
	aiClient := &pollinationsClient{
		apiKey:   pollinationsAPIKey,
		endpoint: endpoint,
		model:    modelName,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}

	// Create discord session
	dg, err := discordgo.New("Bot " + discordToken)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	// Set discord intents
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	// Event handler for triggering with every new message
	dg.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		handleMessage(ctx, s, m, aiClient)
	})

	// Open websocket to discord
	if err := dg.Open(); err != nil {
		log.Fatalf("Error connecting to Discord: %v", err)
	}
	defer dg.Close()

	fmt.Printf("Klaus is online (User: %s, Modell: %s). Press CTRL+C to stop.\n", dg.State.User.Username, modelName)

	// wait until CTRL+C (SIGINT) or SIGTERM
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop

	fmt.Println("\nKlaus is shutting down...")
}

func handleMessage(
	ctx context.Context,
	s *discordgo.Session,
	m *discordgo.MessageCreate,
	aiClient *pollinationsClient,
) {
	// Ignore own messages
	if s.State.User == nil || m.Author.ID == s.State.User.ID {
		return
	}

	botID := s.State.User.ID
	if !isBotMentioned(m, botID) {
		return
	}

	// Remove ping from the message
	prompt := stripBotMention(m.Content, botID)
	if prompt == "" {
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "was", m.Reference())
		return
	}

	// Show "Klaus is typing..." while thinking
	_ = s.ChannelTyping(m.ChannelID)

	// Send request to Pollinations API
	respText, err := aiClient.generateReply(ctx, prompt)
	if err != nil {
		log.Printf("Pollinations-Fehler: %v", err)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "kein plan", m.Reference())
		return
	}

	replyText := strings.TrimSpace(respText)
	if replyText == "" {
		replyText = ""
	}

	// Send reply in discord, split if necessary (Discord allows max. 2000 characters per message)
	for i, chunk := range splitMessage(replyText, discordMaxMsgLen) {
		var sendErr error
		if i == 0 {
			_, sendErr = s.ChannelMessageSendReply(m.ChannelID, chunk, m.Reference())
		} else {
			_, sendErr = s.ChannelMessageSend(m.ChannelID, chunk)
		}
		if sendErr != nil {
			log.Printf("Error sending message on discord: %v", sendErr)
			return
		}
	}
}

func isBotMentioned(m *discordgo.MessageCreate, botID string) bool {
	for _, user := range m.Mentions {
		if user.ID == botID {
			return true
		}
	}
	return false
}

func stripBotMention(content, botID string) string {
	content = strings.ReplaceAll(content, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	return strings.TrimSpace(content)
}

func splitMessage(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(runes) > maxLen {
		chunks = append(chunks, string(runes[:maxLen]))
		runes = runes[maxLen:]
	}
	if len(runes) > 0 {
		chunks = append(chunks, string(runes))
	}
	return chunks
}
