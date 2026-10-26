package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// YouTube: Titel + Untertitel ueber yt-dlp (liegt neben klaus-smart.exe oder
// ist im PATH). Lange Transkripte werden vorher zusammengefasst.
// ---------------------------------------------------------------------------

const (
	ytTimeout        = 2 * time.Minute
	maxTranscriptRaw = 25000 // bis hierhin geht das Transkript direkt ans Modell
)

var reYouTubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// youtubeID erkennt youtube.com/watch?v=, youtu.be/, /shorts/, /live/, /embed/.
func youtubeID(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	host = strings.TrimPrefix(host, "m.")
	var id string
	switch host {
	case "youtu.be":
		id = strings.Trim(u.Path, "/")
	case "youtube.com", "music.youtube.com", "youtube-nocookie.com":
		if v := u.Query().Get("v"); v != "" {
			id = v
		} else {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed") {
				id = parts[1]
			}
		}
	}
	if reYouTubeID.MatchString(id) {
		return id
	}
	return ""
}

func findYtDlp() string {
	for _, name := range []string{"yt-dlp.exe", "yt-dlp"} {
		if p := dataPath(name); fileExists(p) {
			return p
		}
	}
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func (b *bot) youtubeTranscript(ctx context.Context, raw string, progress func(string)) (string, string) {
	id := youtubeID(raw)
	if id == "" {
		return "Error: not a YouTube video URL.", ""
	}
	ytdlp := findYtDlp()
	if ytdlp == "" {
		return "Error: yt-dlp is not installed on the server, so you cannot read YouTube videos right now. Tell the user.", ""
	}
	videoURL := "https://www.youtube.com/watch?v=" + id
	dir, err := os.MkdirTemp("", "klaus-yt-")
	if err != nil {
		return "Error: " + err.Error(), ""
	}
	defer os.RemoveAll(dir)

	cctx, cancel := context.WithTimeout(ctx, ytTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, ytdlp,
		"--no-simulate", "--skip-download", "--no-playlist", "--no-warnings",
		"--write-subs", "--write-auto-subs",
		"--sub-langs", "de,de-orig,de-DE,en,en-orig,en-US,en-GB",
		"--sub-format", "json3",
		"--print", "%(title)s\n%(channel)s\n%(duration_string)s\n%(upload_date)s",
		"-o", filepath.Join(dir, "v"),
		videoURL)
	cmd.Env = cleanEnv() // ohne Discord-Token
	t0 := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[youtube] %s: yt-dlp Fehler: %v: %s", id, err, oneLine(truncateRunes(string(out), 300)))
		return "Error: could not read the video (" + oneLine(truncateRunes(string(out), 200)) + ")", ""
	}
	info := strings.Split(strings.TrimSpace(string(out)), "\n")
	for len(info) < 4 {
		info = append(info, "")
	}
	title, channel, duration, date := info[0], info[1], info[2], info[3]

	text, lang := readJSON3(dir)
	log.Printf("[youtube] %s: %q von %s, %s, Untertitel %q, %d Zeichen, %.1f s", id, title, channel, duration, lang, len([]rune(text)), time.Since(t0).Seconds())
	head := fmt.Sprintf("Video: %s\nChannel: %s\nDuration: %s\nUploaded: %s\nURL: %s\n", title, channel, duration, date, videoURL)
	if text == "" {
		return head + "\nThis video has no subtitles, so its content is unknown. You only know the title.", videoURL
	}
	if len([]rune(text)) > maxTranscriptRaw {
		sum := b.condense(ctx, text, "This is the transcript of a YouTube video titled \""+title+"\". Summarize this part: main points, key facts, numbers, conclusions. Keep timestamps for important moments. Write in the transcript's language.", "📺 schaut das video", progress)
		return head + fmt.Sprintf("\nTranscript (%s) was long, condensed summary of all parts:\n%s", lang, sum), videoURL
	}
	return head + fmt.Sprintf("\nTranscript (%s):\n%s", lang, text), videoURL
}

// readJSON3 liest die beste Untertitel-Datei (Deutsch vor Englisch, echte vor automatischen).
func readJSON3(dir string) (string, string) {
	files, _ := filepath.Glob(filepath.Join(dir, "v.*.json3"))
	if len(files) == 0 {
		return "", ""
	}
	rank := func(f string) int {
		lang := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "v."), ".json3")
		order := []string{"de", "de-DE", "de-orig", "en", "en-US", "en-GB", "en-orig"}
		for i, o := range order {
			if lang == o {
				return i
			}
		}
		return 100
	}
	sort.Slice(files, func(i, j int) bool { return rank(files[i]) < rank(files[j]) })
	lang := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(files[0]), "v."), ".json3")
	data, err := os.ReadFile(files[0])
	if err != nil {
		return "", ""
	}
	return parseJSON3(data), lang
}

func parseJSON3(data []byte) string {
	var j struct {
		Events []struct {
			TStartMs int64 `json:"tStartMs"`
			Segs     []struct {
				UTF8 string `json:"utf8"`
			} `json:"segs"`
		} `json:"events"`
	}
	if json.Unmarshal(data, &j) != nil {
		return ""
	}
	var sb strings.Builder
	nextStamp := int64(0)
	for _, ev := range j.Events {
		var line strings.Builder
		for _, s := range ev.Segs {
			line.WriteString(s.UTF8)
		}
		t := strings.TrimSpace(strings.ReplaceAll(line.String(), "\n", " "))
		if t == "" {
			continue
		}
		if ev.TStartMs >= nextStamp {
			sec := ev.TStartMs / 1000
			fmt.Fprintf(&sb, "\n[%d:%02d] ", sec/60, sec%60)
			nextStamp = ev.TStartMs + 60_000 // ein Zeitstempel pro Minute
		}
		sb.WriteString(t)
		sb.WriteByte(' ')
	}
	return strings.TrimSpace(sb.String())
}

// cleanEnv gibt externen Programmen die Umgebung ohne Klaus-Geheimnisse.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "KLAUS_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}
