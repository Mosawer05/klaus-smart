package main

import (
	"context"
	"log"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Zweite Pruefung: Qwen liest seine Antwort nochmal kritisch gegen und
// korrigiert nur echte Fehler.
// ---------------------------------------------------------------------------

const reviewPrompt = `You are a careful reviewer. Below is a question from a Discord user and a draft answer written by an assistant.

Check the draft for real problems: factual errors, wrong calculations, wrong code, logic mistakes, missing the actual question, or claims that contradict the <evidence>.

Rules:
- Facts in <evidence> (web results, tool outputs) are more recent than your own knowledge. Never "correct" the draft against <evidence>.
- Do not change style, tone, length or wording if the content is fine. Small imperfections are fine.
- Only fix things that are actually wrong or misleading.

Output format, first line is mandatory:
OK
(if the draft is fine), or:
FIXED
<the complete corrected answer, in the same language and style as the draft, ready to post>`

type reviewResult struct {
	changed bool
	answer  string
	took    time.Duration
}

// reviewAnswer prueft den Entwurf. Fehler beim Pruefen lassen den Entwurf unveraendert.
func (b *bot) reviewAnswer(ctx context.Context, question, draft, evidence string) reviewResult {
	start := time.Now()
	var user strings.Builder
	user.WriteString("<question>\n" + truncateRunes(question, 9000) + "\n</question>\n\n")
	if strings.TrimSpace(evidence) != "" {
		user.WriteString("<evidence>\n" + truncateRunes(evidence, 5000) + "\n</evidence>\n\n")
	}
	user.WriteString("<draft>\n" + draft + "\n</draft>\n\nReview the draft now. First line: OK or FIXED.")

	opts := b.genOptions(effortXHigh)
	opts.ReasoningBudget = b.cfg.reviewBudget
	res, err := b.llm.chat(ctx, []chatMessage{
		{Role: "system", Content: reviewPrompt},
		{Role: "user", Content: user.String()},
	}, nil, opts, nil)
	took := time.Since(start)
	if err != nil {
		log.Printf("[prüfung] Fehler (%s), behalte Entwurf: %v", took.Round(time.Second), err)
		return reviewResult{answer: draft, took: took}
	}
	changed, fixed := parseReview(res.answer)
	if !changed {
		log.Printf("[prüfung] OK nach %s", took.Round(time.Second))
		return reviewResult{answer: draft, took: took}
	}
	if len([]rune(fixed)) < len([]rune(draft))/4 {
		log.Printf("[prüfung] Korrektur verdächtig kurz (%d statt %d Zeichen), behalte Entwurf", len([]rune(fixed)), len([]rune(draft)))
		return reviewResult{answer: draft, took: took}
	}
	log.Printf("[prüfung] KORRIGIERT nach %s. Gedanke: %s", took.Round(time.Second), thoughtTail(res.reasoning))
	return reviewResult{changed: true, answer: fixed, took: took}
}

// parseReview: "OK" -> unveraendert, "FIXED\n<text>" -> korrigiert.
func parseReview(out string) (bool, string) {
	out = strings.TrimSpace(out)
	lines := strings.SplitN(out, "\n", 2)
	first := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[0]), "*#:_ "))
	if strings.HasPrefix(first, "FIXED") && len(lines) == 2 {
		if fixed := strings.TrimSpace(lines[1]); fixed != "" {
			return true, fixed
		}
	}
	return false, ""
}
