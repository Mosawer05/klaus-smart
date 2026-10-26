package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// "Was hab ich verpasst?" und Zusammenfassen langer Texte in Stuecken.
// ---------------------------------------------------------------------------

const (
	maxSummaryMessages = 500
	maxMissedMessages  = 1000
	summaryChunkRunes  = 15000
	summaryDirectRunes = 20000 // so viel Chat geht direkt ans Modell
)

func (b *bot) summarizeChannel(ctx context.Context, tc toolCtx, scope string, count int, progress func(string)) string {
	var msgs []*discordgo.Message // neueste zuerst
	before := tc.messageID
	limit := count
	if scope == "since_my_last_message" {
		limit = maxMissedMessages
	} else {
		if limit <= 0 {
			limit = 150
		}
		if limit > maxSummaryMessages {
			limit = maxSummaryMessages
		}
	}
	foundOwn := false
	cutoff := time.Now().Add(-14 * 24 * time.Hour)
	for len(msgs) < limit {
		n := min(100, limit-len(msgs))
		page, err := b.s.ChannelMessages(tc.channelID, n, before, "", "")
		if err != nil {
			log.Printf("[zusammenfassung] Verlauf laden: %v", err)
			break
		}
		if len(page) == 0 {
			break
		}
		stop := false
		for _, m := range page {
			if scope == "since_my_last_message" && m.Author != nil && m.Author.ID == tc.userID {
				foundOwn = true
				stop = true
				break
			}
			if m.Timestamp.Before(cutoff) {
				stop = true
				break
			}
			msgs = append(msgs, m)
		}
		before = page[len(page)-1].ID
		if stop || len(page) < n {
			break
		}
		if progress != nil {
			progress(fmt.Sprintf("📜 liest den chatverlauf… (%d nachrichten)", len(msgs)))
		}
	}
	if len(msgs) == 0 {
		if scope == "since_my_last_message" && foundOwn {
			return "Nothing happened since this person's last message."
		}
		return "No earlier messages found in this channel."
	}

	botID := b.s.State.User.ID
	var lines []string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Author == nil {
			continue
		}
		text := cleanContent(m, botID)
		if m.Author.ID == botID {
			if isStatusText(text) {
				continue
			}
			text = stripFooter(text)
		}
		text = strings.TrimSpace(text + attachmentHint(m))
		if text == "" {
			continue
		}
		name := displayName(m)
		if m.Author.ID == botID {
			name = "Klaus (you)"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", m.Timestamp.Local().Format("02.01. 15:04"), name, oneLine(truncateRunes(text, 600))))
	}
	first, last := msgs[len(msgs)-1].Timestamp.Local(), msgs[0].Timestamp.Local()
	head := fmt.Sprintf("%d messages from %s to %s", len(lines), first.Format("02.01. 15:04"), last.Format("02.01. 15:04"))
	if scope == "since_my_last_message" {
		if foundOwn {
			head += " (everything since the asking person's last message)"
		} else {
			head += " (the asking person did not write in the checked range, so this is the recent history)"
		}
	}
	all := strings.Join(lines, "\n")
	log.Printf("[zusammenfassung] %s, %d Zeichen", head, len([]rune(all)))
	if len([]rune(all)) <= summaryDirectRunes {
		return head + ":\n" + all
	}
	sum := b.condense(ctx, all, "This is part of a Discord chat log. Summarize it as short bullet points in German: topics, important statements (with who said them), decisions, plans with dates, open questions, links shared. Skip small talk.", "📜 fasst zusammen", progress)
	return head + ". The log was long, here are condensed notes of all parts in order:\n" + sum
}

// condense fasst langen Text in Stuecken zusammen (ohne Nachdenken, schnell).
func (b *bot) condense(ctx context.Context, text, instruction, label string, progress func(string)) string {
	chunks := chunkRunes(text, summaryChunkRunes)
	var parts []string
	for i, c := range chunks {
		if ctx.Err() != nil {
			break
		}
		if progress != nil {
			progress(fmt.Sprintf("%s… (teil %d/%d)", label, i+1, len(chunks)))
		}
		res, err := b.llm.chat(ctx, []chatMessage{
			{Role: "system", Content: instruction + " Output only the notes, at most 12 bullet points."},
			{Role: "user", Content: c},
		}, nil, b.genOptions(effortOff), nil)
		if err != nil {
			log.Printf("[zusammenfassung] Teil %d: %v", i+1, err)
			parts = append(parts, fmt.Sprintf("(Teil %d konnte nicht zusammengefasst werden)", i+1))
			continue
		}
		parts = append(parts, fmt.Sprintf("Part %d/%d:\n%s", i+1, len(chunks), strings.TrimSpace(res.answer)))
	}
	return strings.Join(parts, "\n\n")
}

// chunkRunes teilt Text an Zeilengrenzen in Stuecke von hoechstens max Zeichen.
func chunkRunes(s string, max int) []string {
	var chunks []string
	var cur strings.Builder
	curLen := 0
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // abschliessender Zeilenumbruch
	}
	for _, line := range lines {
		l := len([]rune(line)) + 1
		if curLen+l > max && curLen > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
			curLen = 0
		}
		for l > max { // einzelne Riesenzeile
			r := []rune(line)
			chunks = append(chunks, string(r[:max]))
			line = string(r[max:])
			l = len([]rune(line)) + 1
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		curLen += l
	}
	if curLen > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}
