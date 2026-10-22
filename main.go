package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"google.golang.org/genai"
)

const (
	defaultModel     = "gemini-3.8-flash"
	discordMaxMsgLen = 2000
)

func main() {
	discordToken := strings.TrimSpace(os.Getenv("KLAUS_DISCORD_BOT_TOKEN"))
	geminiAPIKey := strings.TrimSpace(os.Getenv("KLAUS_GEMINI_API_KEY"))
	modelName := strings.TrimSpace(os.Getenv("KLAUS_GEMINI_MODEL"))
	if modelName == "" {
		modelName = defaultModel
	}

	if discordToken == "" {
		log.Fatal("Environment variable KLAUS_DISCORD_BOT_TOKEN is missing!")
	}
	if geminiAPIKey == "" {
		log.Fatal("Environment variable KLAUS_GEMINI_API_KEY is missing!")
	}

	ctx := context.Background()

	// Initialize gemini client
	geminiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  geminiAPIKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatalf("Error initializing Gemini client: %v", err)
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
		handleMessage(ctx, s, m, geminiClient, modelName)
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
	geminiClient *genai.Client,
	modelName string,
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

	// Send request to Gemini API
	resp, err := geminiClient.Models.GenerateContent(ctx, modelName, genai.Text(prompt), nil)
	if err != nil {
		log.Printf("Gemini-Fehler: %v", err)
		_, _ = s.ChannelMessageSendReply(m.ChannelID, "kein plan", m.Reference())
		return
	}

	replyText := strings.TrimSpace(resp.Text())
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
