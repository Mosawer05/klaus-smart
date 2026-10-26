package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/ledongthuc/pdf"
)

// ---------------------------------------------------------------------------
// Dateien lesen: Text, Code und PDFs aus Discord-Anhaengen.
// ---------------------------------------------------------------------------

const (
	maxFileDownload  = 20 << 20 // 20 MB
	maxFileRunes     = 20000    // pro Datei im Prompt (48k Kontext)
	maxAllFilesRunes = 30000    // alle Dateien zusammen
	maxFiles         = 3
)

var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".log": true, ".csv": true, ".tsv": true, ".json": true,
	".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".xml": true,
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true,
	".py": true, ".go": true, ".java": true, ".kt": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	".cc": true, ".cs": true, ".rs": true, ".rb": true, ".php": true, ".swift": true, ".lua": true, ".sh": true,
	".bat": true, ".ps1": true, ".sql": true, ".r": true, ".m": true, ".tex": true, ".srt": true, ".vtt": true,
	".gradle": true, ".properties": true, ".env.example": true, ".dart": true, ".scala": true, ".vue": true,
	".svelte": true,
}

func isPDFAttachment(a *discordgo.MessageAttachment) bool {
	return strings.EqualFold(path.Ext(a.Filename), ".pdf") || strings.Contains(strings.ToLower(a.ContentType), "pdf")
}

func isTextAttachment(a *discordgo.MessageAttachment) bool {
	if isImageAttachment(a) {
		return false
	}
	ct := strings.ToLower(a.ContentType)
	if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml") {
		return true
	}
	return textExts[strings.ToLower(path.Ext(a.Filename))]
}

// fileAttachments: lesbare Dateien der Nachricht und der beantworteten Nachricht.
func fileAttachments(m *discordgo.Message) []*discordgo.MessageAttachment {
	var out []*discordgo.MessageAttachment
	add := func(msg *discordgo.Message) {
		if msg == nil {
			return
		}
		for _, a := range msg.Attachments {
			if len(out) < maxFiles && (isTextAttachment(a) || isPDFAttachment(a)) {
				out = append(out, a)
			}
		}
	}
	add(m)
	add(m.ReferencedMessage)
	return out
}

func downloadAttachment(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFileDownload+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFileDownload {
		return nil, errors.New("Datei zu groß (über 20 MB)")
	}
	return b, nil
}

// pdfToText holt den Text aus einer PDF. Die Bibliothek kann bei kaputten
// PDFs abstuerzen, deshalb mit recover.
func pdfToText(data []byte) (text string, pages int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF nicht lesbar (%v)", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, err
	}
	pages = r.NumPage()
	var sb strings.Builder
	for i := 1; i <= pages; i++ {
		p := r.Page(i)
		if p.V.IsNull() {
			continue
		}
		t, perr := p.GetPlainText(nil)
		if perr != nil {
			continue
		}
		fmt.Fprintf(&sb, "\n--- Seite %d ---\n%s", i, strings.TrimSpace(t))
		if sb.Len() > 400_000 {
			break
		}
	}
	text = strings.TrimSpace(sb.String())
	if text == "" {
		return "", pages, errors.New("PDF enthält keinen Text (vermutlich eingescannt)")
	}
	return text, pages, nil
}

func decodeText(b []byte) string {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
	if utf8.Valid(b) {
		return string(b)
	}
	// Vermutlich Windows-1252/Latin-1 (alte Windows-Textdateien)
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// shortenMiddle kuerzt langen Text, behaelt Anfang und Ende.
func shortenMiddle(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	head := max * 2 / 3
	tail := max - head
	return string(r[:head]) + fmt.Sprintf("\n\n[… %d Zeichen ausgelassen …]\n\n", len(r)-max) + string(r[len(r)-tail:]), true
}
