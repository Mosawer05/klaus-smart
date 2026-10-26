package main

import (
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Lange Antworten (ueber 2000 Zeichen): als Box. Eine Box fasst hoechstens
// 4096 Zeichen (Discord-Limit). Ist die Antwort laenger, kommt sie aufgeteilt
// in mehrere Nachrichten mit je einer Box.
// ---------------------------------------------------------------------------

const (
	embedDescMax = 4096
	embedColor   = 0x5865F2
)

// buildBoxes teilt die Antwort in Boxen. info landet in der Fusszeile der
// letzten Box (z.B. "💭 47 s nachgedacht · 🔎 2 suchen"), Quellen als Feld.
func buildBoxes(answer, info string, sources []string) []*discordgo.MessageEmbed {
	var boxes []*discordgo.MessageEmbed
	for _, part := range splitMessage(answer, embedDescMax) {
		boxes = append(boxes, &discordgo.MessageEmbed{Description: part, Color: embedColor})
	}
	last := boxes[len(boxes)-1]
	if len(sources) > 0 {
		var links []string
		for i, s := range sources {
			if i == 4 {
				break
			}
			links = append(links, fmt.Sprintf("[%s](%s)", hostOf(s), s))
		}
		last.Fields = []*discordgo.MessageEmbedField{{Name: "Quellen", Value: truncateRunes(strings.Join(links, " · "), 1024)}}
	}
	if info != "" {
		last.Footer = &discordgo.MessageEmbedFooter{Text: info}
	}
	return boxes
}

// footerInfo: einzeiliger Text fuer die Embed-Fusszeile (ohne Markdown).
func footerInfo(footer string) string {
	var parts []string
	for _, line := range strings.Split(strings.TrimSpace(footer), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-#"))
		if line == "" || strings.HasPrefix(line, "Quellen:") {
			continue
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " · ")
}

// editLong: erste Box ersetzt die Status-Nachricht, weitere Boxen kommen als
// eigene Nachrichten direkt danach.
func (b *bot) editLong(channelID, msgID, answer, footer string, sources []string) error {
	boxes := buildBoxes(answer, footerInfo(footer), sources)
	empty := ""
	first := []*discordgo.MessageEmbed{boxes[0]}
	if _, err := b.s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: msgID, Channel: channelID, Content: &empty, Embeds: &first, AllowedMentions: noPings,
	}); err != nil {
		return err
	}
	for _, box := range boxes[1:] {
		if _, err := b.s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Embeds: []*discordgo.MessageEmbed{box}, AllowedMentions: noPings,
		}); err != nil {
			return err
		}
	}
	return nil
}

// embedText: der Text aus Klaus' eigenen Embed-Antworten (fuer Verlauf und Antwortketten).
func embedText(m *discordgo.Message) string {
	var parts []string
	for _, e := range m.Embeds {
		if e != nil && e.Description != "" && e.Type == discordgo.EmbedTypeRich {
			parts = append(parts, e.Description)
		}
	}
	return strings.Join(parts, "\n")
}
