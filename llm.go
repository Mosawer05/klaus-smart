package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// OpenAI-kompatibler Client (llama-server) mit Streaming und Tool-Calls
// ---------------------------------------------------------------------------

// chatMessage.Content ist entweder ein string oder []contentPart (Text + Bilder).
type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []toolDef     `json:"tools,omitempty"`
	genOptions
}

// genOptions: Denk-Stufe, Denk-Budget und Sampling fuer eine Anfrage.
type genOptions struct {
	// z.B. {"reasoning_effort": "xhigh"} oder {"enable_thinking": false}
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
	// llama-server: maximale Denk-Tokens, danach wird das Denken beendet
	ReasoningBudget  int    `json:"reasoning_budget_tokens,omitempty"`
	ReasoningMessage string `json:"reasoning_budget_message,omitempty"`

	// Von Qwen empfohlene Sampling-Werte (siehe Modellkarte)
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	MinP            *float64 `json:"min_p,omitempty"`
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string     `json:"content"`
			ReasoningContent string     `json:"reasoning_content"`
			ToolCalls        []toolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Timings *llmTimings `json:"timings,omitempty"`
}

// llmTimings: Messwerte, die llama-server mitschickt.
type llmTimings struct {
	PromptN            int     `json:"prompt_n"`
	PromptMS           float64 `json:"prompt_ms"`
	PromptPerSecond    float64 `json:"prompt_per_second"`
	PredictedN         int     `json:"predicted_n"`
	PredictedMS        float64 `json:"predicted_ms"`
	PredictedPerSecond float64 `json:"predicted_per_second"`
	CacheN             int     `json:"cache_n"`
}

type llmClient struct {
	cfg        config
	httpClient *http.Client
	vision     atomic.Bool  // kann der Server Bilder (mmproj geladen)?
	nCtx       atomic.Int64 // Kontextgroesse laut Server
}

// contextSize: Kontext in Tokens (vom Server, sonst 16384).
func (c *llmClient) contextSize() int {
	if n := c.nCtx.Load(); n > 0 {
		return int(n)
	}
	return 16384
}

// progressFunc wird waehrend des Streamens mit dem bisherigen Stand aufgerufen.
type progressFunc func(reasoning, answer string)

type llmResult struct {
	reasoning    string
	answer       string
	finishReason string
	toolCalls    []toolCall
	timings      *llmTimings
}

// chat fuehrt genau eine Modell-Antwort aus (eine "Runde").
func (c *llmClient) chat(ctx context.Context, messages []chatMessage, tools []toolDef, opts genOptions, onProgress progressFunc) (llmResult, error) {
	var res llmResult

	body, err := json.Marshal(chatRequest{Model: c.cfg.model, Messages: messages, Stream: true, Tools: tools, genOptions: opts})
	if err != nil {
		return res, fmt.Errorf("request bauen: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return res, fmt.Errorf("request bauen: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.apiKey)
	}

	started := time.Now()
	defer func() {
		logLLM(started, opts, len(tools) > 0, &res)
	}()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return res, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	// Manche APIs ignorieren stream=true und schicken normales JSON.
	if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return res, err
		}
		var chunk streamChunk
		if json.Unmarshal(b, &chunk) == nil && len(chunk.Choices) > 0 {
			msg := chunk.Choices[0].Message
			res.reasoning, res.answer = splitThinkTags(msg.ReasoningContent, msg.Content)
			res.toolCalls = msg.ToolCalls
			if chunk.Choices[0].FinishReason != nil {
				res.finishReason = *chunk.Choices[0].FinishReason
			}
			res.answer, res.toolCalls = extractInlineToolCalls(res.answer, res.toolCalls)
			return res, nil
		}
		if chunk.Error != nil {
			return res, errors.New(chunk.Error.Message)
		}
		res.answer = strings.TrimSpace(string(b))
		return res, nil
	}

	var reasoning, raw strings.Builder
	calls := map[int]*toolCall{}
	var order []int

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return res, errors.New(chunk.Error.Message)
		}
		if chunk.Timings != nil {
			res.timings = chunk.Timings
		}
		for _, ch := range chunk.Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
			reasoning.WriteString(ch.Delta.Reasoning)
			raw.WriteString(ch.Delta.Content)
			for _, tc := range ch.Delta.ToolCalls {
				cur, ok := calls[tc.Index]
				if !ok {
					cur = &toolCall{Type: "function"}
					calls[tc.Index] = cur
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				cur.Function.Name += tc.Function.Name
				cur.Function.Arguments += tc.Function.Arguments
			}
			if ch.FinishReason != nil {
				res.finishReason = *ch.FinishReason
			}
		}
		if onProgress != nil {
			r, a := splitThinkTags(reasoning.String(), raw.String())
			onProgress(r, hideToolMarkup(a))
		}
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}
	res.reasoning, res.answer = splitThinkTags(reasoning.String(), raw.String())
	for i, idx := range order {
		tc := *calls[idx]
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano()%1e6, i)
		}
		res.toolCalls = append(res.toolCalls, tc)
	}
	res.answer, res.toolCalls = extractInlineToolCalls(res.answer, res.toolCalls)
	return res, nil
}

// splitThinkTags trennt <think>...</think> aus dem Antworttext, falls der
// Server das Denken nicht schon separat liefert.
func splitThinkTags(reasoning, content string) (string, string) {
	trimmed := strings.TrimLeft(content, " \n\r\t")
	if !strings.HasPrefix(trimmed, "<think>") {
		// Qwen-Templates oeffnen <think> manchmal schon im Prompt, dann kommt nur </think>.
		if i := strings.Index(content, "</think>"); i >= 0 && reasoning == "" {
			return content[:i], strings.TrimSpace(content[i+len("</think>"):])
		}
		return reasoning, content
	}
	inner := strings.TrimPrefix(trimmed, "<think>")
	if i := strings.Index(inner, "</think>"); i >= 0 {
		return reasoning + inner[:i], strings.TrimSpace(inner[i+len("</think>"):])
	}
	return reasoning + inner, "" // denkt noch
}

// hideToolMarkup schneidet rohe <tool_call>-Bloecke aus der Live-Vorschau.
func hideToolMarkup(s string) string {
	if i := strings.Index(s, "<tool_call>"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

var (
	reToolCallBlock = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*(?:</tool_call>|$)`)
	reXMLFunction   = regexp.MustCompile(`(?s)<function=([^>\s]+)>(.*?)(?:</function>|$)`)
	reXMLParameter  = regexp.MustCompile(`(?s)<parameter=([^>\s]+)>\s*(.*?)\s*</parameter>`)
)

// extractInlineToolCalls ist ein Notfall-Parser: Falls der Server die
// Tool-Calls nicht selbst erkennt, stehen sie als <tool_call>...</tool_call>
// im Text (JSON- oder Qwen-XML-Format). Dann werden sie hier herausgeholt.
func extractInlineToolCalls(answer string, calls []toolCall) (string, []toolCall) {
	if len(calls) > 0 || !strings.Contains(answer, "<tool_call>") {
		return answer, calls
	}
	for i, m := range reToolCallBlock.FindAllStringSubmatch(answer, -1) {
		body := m[1]
		var tc toolCall
		tc.Type = "function"
		tc.ID = fmt.Sprintf("inline_%d", i)

		var js struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(body), &js) == nil && js.Name != "" {
			tc.Function.Name = js.Name
			args := strings.TrimSpace(string(js.Arguments))
			if strings.HasPrefix(args, `"`) { // Argumente als String-kodiertes JSON
				_ = json.Unmarshal(js.Arguments, &args)
			}
			if args == "" {
				args = "{}"
			}
			tc.Function.Arguments = args
		} else if fm := reXMLFunction.FindStringSubmatch(body); fm != nil {
			tc.Function.Name = fm[1]
			params := map[string]string{}
			for _, pm := range reXMLParameter.FindAllStringSubmatch(fm[2], -1) {
				params[pm[1]] = pm[2]
			}
			b, _ := json.Marshal(params)
			tc.Function.Arguments = string(b)
		} else {
			continue
		}
		calls = append(calls, tc)
	}
	if len(calls) > 0 {
		answer = hideToolMarkup(answer)
	}
	return answer, calls
}

// ---------------------------------------------------------------------------
// Server-Status (llama-server /health und /props)
// ---------------------------------------------------------------------------

func (c *llmClient) baseURL() string {
	base := c.cfg.endpoint
	if i := strings.Index(base, "/v1/"); i >= 0 {
		base = base[:i]
	}
	return strings.TrimRight(base, "/")
}

// healthy prueft, ob der llama-server erreichbar ist, und merkt sich nebenbei,
// ob er Bilder versteht.
func (c *llmClient) healthy() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	c.refreshVision()
	return true
}

func (c *llmClient) refreshVision() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/props", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var props struct {
		Modalities struct {
			Vision bool `json:"vision"`
		} `json:"modalities"`
		DefaultGenerationSettings struct {
			NCtx int64 `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&props) == nil {
		c.vision.Store(props.Modalities.Vision)
		if props.DefaultGenerationSettings.NCtx > 0 {
			c.nCtx.Store(props.DefaultGenerationSettings.NCtx)
		}
	}
}

// logLLM schreibt eine Zeile pro Qwen-Anfrage: Stufe, Dauer, Tokens, Tempo.
func logLLM(started time.Time, opts genOptions, tools bool, res *llmResult) {
	stage := "?"
	if v, ok := opts.ChatTemplateKwargs["reasoning_effort"]; ok {
		stage = fmt.Sprint(v)
	} else if v, ok := opts.ChatTemplateKwargs["enable_thinking"]; ok && v == false {
		stage = "ohne denken"
	}
	line := fmt.Sprintf("[qwen] %s, %.1f s, Stufe %s, Budget %d, Werkzeuge %v -> Denken %d Zeichen, Antwort %d Zeichen, Ende %q, %d Werkzeug-Aufrufe",
		time.Now().Format("15:04:05"), time.Since(started).Seconds(), stage, opts.ReasoningBudget, tools,
		len([]rune(res.reasoning)), len([]rune(res.answer)), res.finishReason, len(res.toolCalls))
	if t := res.timings; t != nil {
		line += fmt.Sprintf(" | Prompt %d Tokens (%d aus Cache) in %.1f s, erzeugt %d Tokens mit %.1f Tokens/s",
			t.PromptN, t.CacheN, t.PromptMS/1000, t.PredictedN, t.PredictedPerSecond)
	}
	log.Print(line)
}
