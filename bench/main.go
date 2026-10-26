// Denk-Stufen-Test fuer Klaus: stellt Qwen kurze Fragen mit eindeutiger
// Antwort in jeder Stufe (off, low, medium, xhigh) und misst Zeit, Tokens
// und Richtigkeit. Ergebnis: bench-ergebnis.txt
//
// Start: bench.bat (llama-server muss laufen)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type question struct {
	q      string
	accept []string // Regex (klein geschrieben) fuer richtige Antworten
	reject []string // Regex, die eine falsche Antwort verraten
}

var questions = []question{
	{"Was ist 17 mal 23?", []string{`\b391\b`}, nil},
	{"Wie viele r sind im Wort strawberry?", []string{`\b3\b`, `\bdrei\b`}, nil},
	{"Was ist die Hauptstadt von Australien?", []string{`canberra`}, nil},
	{"Welcher Wochentag war der 1. Januar 2000?", []string{`samstag`}, nil},
	{"Was ist größer: 9,11 oder 9,9?", []string{`9[.,]9(?:[^0-9]|$)`}, []string{`9[.,]11 ist größer`, `^9[.,]11$`}},
	{"Wie viele Tage hat der Februar im Jahr 2100?", []string{`\b28\b`}, []string{`\b29\b`}},
	{"Was ist die Wurzel aus 144 plus 3 hoch 3?", []string{`\b39\b`}, nil},
	{"Welches chemische Element hat das Symbol W?", []string{`wolfram`, `tungsten`}, nil},
	{"Heute ist Dienstag. Welcher Wochentag ist in 100 Tagen?", []string{`donnerstag`}, nil},
	{"Wie viele Minuten haben drei Tage?", []string{`4[.,]?320`}, nil},
	{"Wer hat 'Die Verwandlung' geschrieben?", []string{`kafka`}, nil},
	{"Wie viele Buchstaben hat das Wort Donaudampfschifffahrt?", []string{`\b21\b`}, nil},
	{"Ist 221 eine Primzahl?", []string{`\bnein\b`, `keine primzahl`, `13`}, []string{`^ja\b`}},
	{"Was sind 15 Prozent von 240?", []string{`\b36\b`}, nil},
	{"In welchem Jahr fiel die Berliner Mauer?", []string{`1989`}, nil},
	{"Ein Zug fährt 120 km in 1,5 Stunden. Wie schnell im Schnitt?", []string{`\b80\b`}, nil},
	{"Wie viele Kanten hat ein Würfel?", []string{`\b12\b`, `zwölf`}, nil},
	{"Welcher Planet ist der Sonne am nächsten?", []string{`merkur`, `mercury`}, nil},
	{"Wie oft kommt die Ziffer 1 in den Zahlen von 1 bis 20 vor?", []string{`\b12\b`, `zwölf`}, nil},
	{"Was ist 2 hoch 10 minus 24?", []string{`\b1[.,]?000\b`, `tausend`}, nil},
}

var levels = []string{"off", "low", "medium", "xhigh"}

const systemPrompt = `You are Klaus, a helpful assistant in a Discord chat. Reply in the language of the question. Keep it short. End your reply with a final line in the form "Antwort: <result>".`

type result struct {
	level, q       string
	ok             bool
	secs           float64
	completion     int
	reasoningChars int
	answer         string
	err            string
}

func main() {
	url := os.Getenv("KLAUS_API_URL")
	if url == "" {
		url = "http://localhost:8080/v1/chat/completions"
	}
	runs := 1
	if v, err := strconv.Atoi(os.Getenv("BENCH_RUNS")); err == nil && v > 0 {
		runs = v
	}

	out, err := os.Create("bench-ergebnis.txt")
	if err != nil {
		panic(err)
	}
	defer out.Close()
	logf := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		fmt.Print(s)
		out.WriteString(s)
		out.Sync()
	}

	logf("Denk-Stufen-Test, %s, %d Fragen x %d Stufen x %d Durchlauf\n\n", time.Now().Format("02.01.2006 15:04"), len(questions), len(levels), runs)
	logf("%-7s %-4s %7s %6s %6s  %s\n", "stufe", "ok", "sek", "tokens", "denken", "frage -> antwort")

	var all []result
	total := len(questions) * len(levels) * runs
	n := 0
	for r := 0; r < runs; r++ {
		for qi, q := range questions {
			// Reihenfolge der Stufen pro Frage rotieren, damit keine Stufe
			// immer vom Cache der vorherigen profitiert.
			for li := range levels {
				level := levels[(li+qi+r)%len(levels)]
				n++
				res := ask(url, level, q)
				all = append(all, res)
				mark := "ok"
				if !res.ok {
					mark = "FALSCH"
				}
				if res.err != "" {
					mark = "FEHLER"
				}
				logf("%-7s %-4s %7.1f %6d %6d  [%d/%d] %s -> %s\n", level, mark, res.secs, res.completion, res.reasoningChars, n, total, q.q, oneLine(res.answer, res.err))
			}
		}
	}

	logf("\n=== Zusammenfassung ===\n")
	logf("%-7s %9s %10s %12s %13s %14s\n", "stufe", "richtig", "zeit Ø", "zeit median", "tokens Ø", "denk-zeichen Ø")
	for _, l := range levels {
		var ok, cnt, tok, think int
		var secs []float64
		var sum float64
		for _, r := range all {
			if r.level != l || r.err != "" {
				continue
			}
			cnt++
			if r.ok {
				ok++
			}
			sum += r.secs
			secs = append(secs, r.secs)
			tok += r.completion
			think += r.reasoningChars
		}
		if cnt == 0 {
			continue
		}
		sort.Float64s(secs)
		logf("%-7s %4d/%-4d %8.1f s %10.1f s %13d %14d\n", l, ok, cnt, sum/float64(cnt), secs[len(secs)/2], tok/cnt, think/cnt)
	}
	logf("\nFertig.\n")
}

func ask(url, level string, q question) result {
	res := result{level: level, q: q.q}
	body := map[string]any{
		"model": "qwen",
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": q.q},
		},
		"stream": false,
		"top_k":  20,
		"min_p":  0.0,
	}
	if level == "off" {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
		body["temperature"], body["top_p"], body["presence_penalty"] = 0.7, 0.8, 1.5
	} else {
		body["chat_template_kwargs"] = map[string]any{"reasoning_effort": level}
		body["temperature"], body["top_p"], body["presence_penalty"] = 1.0, 0.95, 0.0
		body["reasoning_budget_tokens"] = 8192
		body["reasoning_budget_message"] = "\n\nOkay, I have thought about this enough. Time to write the final answer with what I have.\n"
	}
	b, _ := json.Marshal(body)

	start := time.Now()
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	res.secs = time.Since(start).Seconds()
	if resp.StatusCode != 200 {
		res.err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, oneLine(string(raw), ""))
		return res
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		res.err = "Antwort nicht lesbar"
		return res
	}
	msg := out.Choices[0].Message
	content := msg.Content
	reasoning := msg.ReasoningContent
	if i := strings.Index(content, "</think>"); i >= 0 { // falls nicht getrennt
		reasoning += content[:i]
		content = content[i+len("</think>"):]
	}
	res.answer = strings.TrimSpace(content)
	res.completion = out.Usage.CompletionTokens
	res.reasoningChars = len([]rune(reasoning))
	res.ok = grade(res.answer, q)
	return res
}

var reFinal = regexp.MustCompile(`(?i)antwort\s*:\s*(.+)`)

func grade(answer string, q question) bool {
	text := strings.ToLower(answer)
	if m := reFinal.FindAllStringSubmatch(answer, -1); m != nil {
		text = strings.ToLower(strings.TrimSpace(m[len(m)-1][1]))
	}
	text = strings.NewReplacer("**", "", "*", "", "`", "").Replace(text)
	for _, r := range q.reject {
		if regexp.MustCompile(r).MatchString(text) {
			return false
		}
	}
	for _, a := range q.accept {
		if regexp.MustCompile(a).MatchString(text) {
			return true
		}
	}
	return false
}

func oneLine(s, errText string) string {
	if errText != "" {
		s = errText
	}
	if m := reFinal.FindAllStringSubmatch(s, -1); m != nil {
		s = "Antwort: " + m[len(m)-1][1]
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 90 {
		s = string(r[:89]) + "…"
	}
	return s
}
