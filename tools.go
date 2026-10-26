package main

import (
	"context"
	"encoding/json"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Werkzeuge: Was Klaus waehrend einer Antwort selbst tun kann.
// ---------------------------------------------------------------------------

// toolCtx: Wer fragt wo? (fuer Gedaechtnis, Erinnerungen, Zusammenfassung)
type toolCtx struct {
	scope     string // Gedaechtnis-Bereich: "ch:<Kanal>" oder "dm:<User>"
	channelID string
	guildID   string
	userID    string
	userName  string
	messageID string
	request   string // die Nachricht der fragenden Person (fuer die Merk-Sperre)
}

// reExplicitRemember: Nur wenn die Nachricht so etwas enthaelt, darf Klaus sich etwas merken.
var reExplicitRemember = regexp.MustCompile(`(?i)\b(merk|merke|merken|merkt|gemerkt|speicher|speichere|speichern|notier|notiere|notieren|vergiss\s+nicht|nicht\s+vergessen|behalt|behalte|behalten|remember|save|note\s+(that|this|down)|keep\s+in\s+mind|don'?t\s+forget)`)

func (t toolCtx) memoryScope() string {
	if t.scope != "" {
		return t.scope
	}
	if t.guildID == "" {
		return "dm:" + t.userID
	}
	return "ch:" + t.channelID
}

// memoryScope: Gedaechtnis gilt pro Kanal (Threads zaehlen zum Eltern-Kanal),
// in DMs pro Person. So rutscht nichts aus privaten Kanaelen in andere.
func (b *bot) memoryScope(guildID, channelID, userID string) string {
	if guildID == "" {
		return "dm:" + userID
	}
	if b.s != nil && b.s.State != nil {
		if ch, err := b.s.State.Channel(channelID); err == nil && ch.IsThread() && ch.ParentID != "" {
			channelID = ch.ParentID
		}
	}
	return "ch:" + channelID
}

const toolRulesPrompt = `Tools you can use (call them only when they really help):
- run_code: for any calculation beyond trivial mental math (arithmetic with big or many numbers, percentages, dates, statistics, unit conversions, counting letters). Write a short Starlark (Python-like) program and print() the result. Use it instead of calculating in your head whenever the exact result matters. Never use it to count, measure or check your own answer text (length, words, characters) – answer lengths never need to be exact, just write the answer.
- remember / forget: long-term memory for this channel (each channel has its own memory). Call remember ONLY when the person explicitly asks you to remember, save or note something ("merk dir", "speicher", "remember"). Never save anything on your own, not even facts people mention about themselves. Save one short, self-contained sentence (who + what). Call forget when asked to forget something.
- set_reminder / list_reminders / cancel_reminder: when someone asks to be reminded. Compute the exact time from the current date and time in the context. Times are in the user's local time zone (Europe/Berlin).
- summarize_channel: when someone asks what they missed, what was discussed, or for a summary of the chat.
- youtube_transcript: when someone asks about a YouTube video (summary, content, questions about it).`

func (b *bot) toolDefs() []toolDef {
	defs := []toolDef{
		mkTool("run_code", "Run a short Starlark program (Python dialect: def, for, if, while, lists, dicts, big integers, floats; no imports, no files, no network). No ** operator: use pow(a, b) (exact for big integers). Also available: math (sqrt, floor, ceil, log, exp, sin, cos, tan, pi, e, ...), sum, round, abs, min, max, sorted. Use print() for output. Returns the printed output or the error.",
			map[string]any{"code": strDesc("The Starlark program")}, "code"),
		mkTool("remember", "Save a lasting fact to long-term memory for this channel.",
			map[string]any{"fact": strDesc("One short self-contained sentence, e.g. 'Max studiert Informatik in Ulm.'")}, "fact"),
		mkTool("forget", "Delete facts from long-term memory that match the given text (or a fact number from <memory>).",
			map[string]any{"match": strDesc("Words of the fact to delete, or its number like '#3'")}, "match"),
		mkTool("set_reminder", "Create a reminder. At the given time you will ping the user in this channel with the text.",
			map[string]any{
				"time": strDesc("Exact local date and time, format 2006-01-02 15:04 (Europe/Berlin)"),
				"text": strDesc("What to remind about, short"),
				"for":  strDesc("Optional: 'me' (default, the person asking)"),
			}, "time", "text"),
		mkTool("list_reminders", "List the open reminders of the person asking.", map[string]any{}),
		mkTool("cancel_reminder", "Cancel one of the asking person's reminders.",
			map[string]any{"id": strDesc("Reminder number from list_reminders, e.g. '#2'")}, "id"),
		mkTool("summarize_channel", "Read many earlier messages of this channel and return a condensed summary of what was discussed.",
			map[string]any{
				"scope": map[string]any{"type": "string", "enum": []string{"since_my_last_message", "last_messages"},
					"description": "since_my_last_message: everything since the asking person last wrote (for 'what did I miss'). last_messages: the last N messages."},
				"count": map[string]any{"type": "integer", "description": "For last_messages: how many (max 500, default 150)"},
			}, "scope"),
		mkTool("youtube_transcript", "Get title, channel and the transcript (subtitles) of a YouTube video.",
			map[string]any{"url": strDesc("YouTube video URL")}, "url"),
	}
	if b.cfg.webSearch {
		defs = append(webTools(), defs...)
	}
	return defs
}

func mkTool(name, desc string, props map[string]any, required ...string) toolDef {
	var t toolDef
	t.Type = "function"
	t.Function.Name = name
	t.Function.Description = desc
	params := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		params["required"] = required
	}
	t.Function.Parameters = params
	return t
}

func strDesc(d string) map[string]any { return map[string]any{"type": "string", "description": d} }

// toolActivity beschreibt kurz, was ein Tool-Call tut (fuer die Discord-Nachricht).
func toolActivity(tc toolCall) string {
	var a map[string]any
	_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
	str := func(k string) string { s, _ := a[k].(string); return strings.TrimSpace(s) }
	switch tc.Function.Name {
	case "web_search":
		return "🔎 sucht: „" + truncateRunes(oneLine(str("query")), 80) + "“"
	case "fetch_page":
		return "📄 liest: " + hostOf(str("url"))
	case "run_code":
		return "🧮 rechnet…"
	case "remember":
		return "📝 merkt sich das…"
	case "forget":
		return "🧽 vergisst…"
	case "set_reminder", "list_reminders", "cancel_reminder":
		return "⏰ erinnerungen…"
	case "summarize_channel":
		return "📜 liest den chatverlauf…"
	case "youtube_transcript":
		return "📺 schaut sich das video an…"
	}
	return "🔧 " + tc.Function.Name
}

// runTool fuehrt einen Tool-Call aus. source ist eine gelesene URL fuer die
// Quellenzeile (oder leer).
func (b *bot) runTool(ctx context.Context, tc toolCtx, call toolCall, progress func(string)) (result, source string) {
	start := time.Now()
	var a map[string]any
	if err := json.Unmarshal([]byte(call.Function.Arguments), &a); err != nil && strings.TrimSpace(call.Function.Arguments) != "" {
		result = "Error: arguments are not valid JSON: " + err.Error()
	}
	str := func(k string) string { s, _ := a[k].(string); return strings.TrimSpace(s) }
	num := func(k string) int {
		switch v := a[k].(type) {
		case float64:
			return int(v)
		case string:
			n := 0
			for _, r := range v {
				if r >= '0' && r <= '9' {
					n = n*10 + int(r-'0')
				}
			}
			return n
		}
		return 0
	}
	if result == "" {
		switch call.Function.Name {
		case "web_search", "fetch_page":
			result, source = b.web.run(ctx, call)
		case "run_code":
			result = runStarlark(str("code"))
		case "remember":
			if !reExplicitRemember.MatchString(tc.request) {
				log.Printf("[gedächtnis] abgelehnt (nicht ausdrücklich gewünscht): %s", str("fact"))
				result = "Not saved: the person did not explicitly ask you to remember anything. Do not save things on your own and do not mention this."
			} else {
				result = b.memory.add(tc.memoryScope(), str("fact"), tc.userName)
			}
		case "forget":
			result = b.memory.forget(tc.memoryScope(), str("match"))
		case "set_reminder":
			result = b.reminders.add(tc, str("time"), str("text"))
		case "list_reminders":
			result = b.reminders.list(tc)
		case "cancel_reminder":
			result = b.reminders.cancel(tc, str("id"))
		case "summarize_channel":
			result = b.summarizeChannel(ctx, tc, str("scope"), num("count"), progress)
		case "youtube_transcript":
			result, source = b.youtubeTranscript(ctx, str("url"), progress)
		default:
			result = "Error: unknown tool " + call.Function.Name
		}
	}
	log.Printf("[werkzeug] %s %s -> %d Zeichen in %.1f s: %s", call.Function.Name, truncateRunes(oneLine(call.Function.Arguments), 140),
		utf8.RuneCountInString(result), time.Since(start).Seconds(), oneLine(truncateRunes(result, 160)))
	return result, source
}
