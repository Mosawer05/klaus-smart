package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Einstellungen (alle per Umgebungsvariable ueberschreibbar, siehe loadConfig)
// ---------------------------------------------------------------------------

const (
	defaultEndpoint = "http://localhost:8080/v1/chat/completions"
	defaultModel    = "qwen"

	discordMaxMsgLen = 2000
	queueBufferSize  = 100
	editInterval     = 2500 * time.Millisecond // Discord erlaubt ca. 5 Edits / 5 s

	maxLogMsgRunes     = 400   // Mitlesen: pro aeltere Nachricht im Kanalverlauf (kurz = schnell)
	passiveNewMsgRunes = 3000  // Mitlesen: pro neue Nachricht
	chatLogMsgRunes    = 6000  // Ping: pro Nachricht im Kanalverlauf (neueste zuerst)
	chatLogTotalRunes  = 18000 // Ping: ganzer Kanalverlauf, aeltere Nachrichten werden gekuerzt
	maxChainMsgRunes   = 6000  // pro Nachricht in einer Antwortkette
	maxChainDepth      = 6     // wie weit eine Antwortkette zurueckverfolgt wird
	maxCallsPerRound   = 3     // Tool-Calls pro Modell-Runde
	answerReserve      = 1500  // Tokens, die im Kontext fuer die Antwort frei bleiben
	imageTokens        = 1100  // grob, pro Bild
	shrunkToolResults  = 800   // Kuerzung, wenn der Kontext voll laeuft

	statusThinking = "💭"
	statusQueued   = "⏳"
)

// Befehle am Anfang einer Nachricht (koennen kombiniert werden: "!neu !denk ...").
var (
	resetPrefixes = []string{"!neu", "neu:", "!new", "new:", "!reset"}
	fastPrefixes  = []string{"!schnell", "!fast", "!kurz"}
	deepPrefixes  = []string{"!denk", "!tief", "!deep", "!think"} // ohne Denk-Budget
)

// Denk-Stufen des Qwen3.8-Templates.
const (
	effortOff    = "off"
	effortLow    = "low"
	effortMedium = "medium"
	effortXHigh  = "xhigh"
	effortDeep   = "deep" // xhigh ohne Denk-Budget (Befehl !denk)
)

// Kurze Nachrichten ohne Frage, bei denen Nachdenken nichts bringt.
var chitChat = []string{
	"hi", "hey", "hallo", "moin", "servus", "yo", "danke", "thx", "thanks", "dankeschön",
	"gute nacht", "gn", "nacht", "bis morgen", "bye", "tschüss", "ciao", "lol", "haha", "xd",
	"ok", "okay", "alles klar", "nice", "cool", "top", "geil", "krass", "jo",
	"guten morgen", "morgen", "gm", "gute arbeit", "good job", "gg",
}

const defaultSystemPrompt = `You are Klaus, a smart and knowledgeable member of this Discord server. People ping you mainly for harder questions: explanations, coding, math, tech, decisions, facts.

How to answer:
- Think carefully before answering. Correctness matters more than speed. If you are unsure or something cannot be known, say so plainly instead of guessing.
- Always reply in the language of the user's latest message.
- Match the effort to the question: small talk or a quick question gets a short, casual reply; a complex question gets a complete, well-structured answer.
- Write like a helpful person in a Discord chat, not like a corporate assistant: no filler, no "Great question!", no repeating the question back.
- Discord formatting: **bold**, bullet lists, short headings (## at most) and code blocks with a language tag are fine. Discord does NOT render tables or LaTeX, so never use them; write math in plain text or Unicode (x², √, ≤, π).
- Keep answers under about 1800 characters when possible. Only go longer when the question truly needs it.
- Never ping anyone and never write @everyone or @here.`

// selfInfo: damit Klaus auf "was kannst du / welche Befehle gibt es?" richtig antwortet.
const selfInfo = `About yourself (use this only when someone asks what you can do, how to use you, or which commands exist; answer in their language):
- People talk to you by pinging you (@klaus) or by replying to one of your messages. In DMs no ping is needed.
- Commands at the very start of a message (can be combined, e.g. "!neu !denk ..."): !neu = ignore the chat history and answer fresh; !schnell = answer immediately without thinking; !denk = think as long as needed, for very hard problems. There are no slash commands.
- You can: search the web and read web pages and PDFs; look at images and GIFs; read attached text, code and PDF files; summarize YouTube videos (if they have subtitles); calculate exactly by running small programs; remember things only when explicitly asked ("merk dir ...") and forget them again ("vergiss ..."), each channel has its own memory; set reminders ("erinner mich morgen um 8 an ..."), list and delete them; summarize what someone missed ("was hab ich verpasst?").
- You also read the chat silently and only jump in uninvited when you are very sure that an answer really helps.
- You run locally on your owner's PC: Qwen 3.8 27B on an AMD Radeon RX 9070 XT. When the PC is off, you are offline.`

const contextRules = `About the <chat_log> inside <context> at the start of the user's message:
- It shows the most recent messages in this channel, oldest first, with how long ago each was sent. It is background only.
- Before answering, decide whether the user's new message continues that conversation or starts a new topic.
- If it continues it (follow-up question, "and what about...", "why?", "that", "it", "he" without a clear subject), use the log to understand what is meant.
- If it is a new or unrelated topic, ignore the log completely and answer the new message on its own. Do not mention the old topic.
- Never answer old questions from the log. Only answer the newest message.
- When in doubt, treat the message as standalone.
- If the user's message is marked as a direct reply to an earlier message, that message is exactly what they are talking about. Reply to it in that sense, even if their text is very short ("why?", "wrong", a GIF, an emoji).`

type config struct {
	discordToken  string
	endpoint      string
	apiKey        string
	model         string
	systemPrompt  string
	contextCount  int
	contextGap    time.Duration
	contextMaxAge time.Duration
	timeout       time.Duration
	showPreview   bool

	announceChannels []string

	// Mitlesen (ungefragt antworten)
	passiveAll      bool
	passiveChannels map[string]bool
	passiveWait     time.Duration
	passiveCooldown time.Duration
	passiveMinConf  int
	passiveBudget   int
	passiveDryRun   bool

	webSearch      bool
	maxToolRounds  int
	searxngURL     string
	tavilyKey      string
	searchRegion   string
	reasoning      string // auto (Standard), stufen, off, low, medium, xhigh
	thinkingBudget int    // max. Denk-Tokens pro Antwort, 0 = unbegrenzt

	reviewEnabled bool // zweite Pruefung
	reviewBudget  int
}

func envFirst(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func envInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		log.Printf("Ungueltiger Wert fuer %s: %q, nehme %d", name, v, def)
	}
	return def
}

func loadConfig() config {
	c := config{
		discordToken:  envFirst("KLAUS_DISCORD_BOT_TOKEN"),
		endpoint:      envFirst("KLAUS_API_URL", "KLAUS_POLLINATIONS_URL"),
		apiKey:        envFirst("KLAUS_API_KEY", "KLAUS_POLLINATIONS_API_KEY"),
		model:         envFirst("KLAUS_MODEL", "KLAUS_POLLINATIONS_MODEL"),
		systemPrompt:  envFirst("KLAUS_SYSTEM_PROMPT"),
		contextCount:  envInt("KLAUS_CONTEXT_MESSAGES", 20),
		contextGap:    time.Duration(envInt("KLAUS_CONTEXT_GAP_MINUTES", 30)) * time.Minute,
		contextMaxAge: time.Duration(envInt("KLAUS_CONTEXT_MAX_AGE_MINUTES", 180)) * time.Minute,
		timeout:       time.Duration(envInt("KLAUS_TIMEOUT_MINUTES", 30)) * time.Minute,
		showPreview:   envFirst("KLAUS_THINKING_PREVIEW") != "0",

		webSearch:      envFirst("KLAUS_WEB_SEARCH") != "0",
		maxToolRounds:  envInt("KLAUS_MAX_TOOL_ROUNDS", 6),
		searxngURL:     envFirst("KLAUS_SEARXNG_URL"),
		tavilyKey:      envFirst("KLAUS_TAVILY_API_KEY"),
		searchRegion:   envFirst("KLAUS_SEARCH_REGION"),
		reasoning:      strings.ToLower(envFirst("KLAUS_REASONING")),
		thinkingBudget: envInt("KLAUS_THINKING_BUDGET", 24576),
		reviewEnabled:  envFirst("KLAUS_REVIEW") != "0",
		reviewBudget:   envInt("KLAUS_REVIEW_THINKING_BUDGET", 4096),
	}
	c.passiveChannels = map[string]bool{}
	for _, id := range strings.Split(envFirst("KLAUS_PASSIVE_CHANNELS"), ",") {
		switch id = strings.ToLower(strings.TrimSpace(id)); id {
		case "":
		case "alle", "all":
			c.passiveAll = true
		default:
			c.passiveChannels[id] = true
		}
	}
	c.passiveWait = time.Duration(envInt("KLAUS_PASSIVE_WAIT_SECONDS", 8)) * time.Second
	c.passiveCooldown = time.Duration(envInt("KLAUS_PASSIVE_COOLDOWN_MINUTES", 10)) * time.Minute
	c.passiveMinConf = envInt("KLAUS_PASSIVE_CONFIDENCE", 95)
	c.passiveBudget = envInt("KLAUS_PASSIVE_THINKING_BUDGET", 4096)
	c.passiveDryRun = envFirst("KLAUS_PASSIVE_TESTMODE") == "1"
	for _, id := range strings.Split(envFirst("KLAUS_ANNOUNCE_CHANNELS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.announceChannels = append(c.announceChannels, id)
		}
	}
	if c.endpoint == "" {
		c.endpoint = defaultEndpoint
	}
	if c.model == "" {
		c.model = defaultModel
	}
	if c.systemPrompt == "" {
		c.systemPrompt = defaultSystemPrompt
	}
	switch c.reasoning {
	case effortOff, effortLow, effortMedium, effortXHigh, "stufen":
	default:
		c.reasoning = "auto" // Smalltalk ohne Denken, sonst xhigh
	}
	if c.searchRegion == "" {
		c.searchRegion = "wt-wt" // DuckDuckGo: keine Region
	}
	if c.contextCount > 100 {
		c.contextCount = 100 // Discord-API-Limit pro Abruf
	}
	return c
}

func (c config) searchProvider() string {
	switch {
	case !c.webSearch:
		return "aus"
	case c.searxngURL != "":
		return "SearXNG"
	case c.tavilyKey != "":
		return "Tavily"
	}
	return "DuckDuckGo"
}

// ---------------------------------------------------------------------------
// Discord-Bot
// ---------------------------------------------------------------------------

type job struct {
	m           *discordgo.MessageCreate
	statusMsgID string
	prompt      string
	resetCtx    bool
	effort      string
}

type bot struct {
	cfg       config
	s         *discordgo.Session
	llm       *llmClient
	web       *webClient
	queue     chan *job
	pending   atomic.Int32
	passive   *passiveState
	memory    *memoryStore
	reminders *reminderStore
}

var noPings = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, RepliedUser: true}

// setupLogFile schreibt alles zusaetzlich in klaus.log (neben der .exe),
// damit man spaeter nachlesen kann, was Klaus entschieden hat.
func setupLogFile() {
	path := "klaus.log"
	if exe, err := os.Executable(); err == nil {
		path = filepath.Join(filepath.Dir(exe), "klaus.log")
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(path, path+".alt")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("Log-Datei: %v", err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

func main() {
	setupLogFile()
	cfg := loadConfig()
	if cfg.discordToken == "" {
		log.Fatal("Umgebungsvariable KLAUS_DISCORD_BOT_TOKEN fehlt!")
	}

	dg, err := discordgo.New("Bot " + cfg.discordToken)
	if err != nil {
		log.Fatalf("Discord-Session: %v", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuilds | // Kanalnamen fuers Log
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b := &bot{
		cfg:       cfg,
		s:         dg,
		llm:       &llmClient{cfg: cfg, httpClient: &http.Client{}}, // Timeout pro Anfrage via Context
		web:       newWebClient(cfg),
		queue:     make(chan *job, queueBufferSize),
		passive:   newPassiveState(),
		memory:    loadMemory(dataPath("memory.json")),
		reminders: loadReminders(dataPath("reminders.json")),
	}
	go b.worker(ctx)
	dg.AddHandler(b.onMessage)

	if err := dg.Open(); err != nil {
		log.Fatalf("Verbindung zu Discord: %v", err)
	}
	defer dg.Close()

	brainOK := b.llm.healthy()
	fmt.Printf("Klaus Smart ist online (User: %s)\n  API: %s (erreichbar: %v, Bilder: %v)\n  Websuche: %s, Kontext: %d Nachrichten\nSTRG+C zum Beenden.\n",
		dg.State.User.Username, cfg.endpoint, brainOK, b.llm.vision.Load(), cfg.searchProvider(), cfg.contextCount)

	switch {
	case cfg.passiveAll:
		fmt.Println("  Mitlesen: in allen Kanälen")
	case len(cfg.passiveChannels) > 0:
		fmt.Printf("  Mitlesen: in %d Kanal/Kanälen\n", len(cfg.passiveChannels))
	default:
		fmt.Println("  Mitlesen: aus")
	}
	if cfg.passiveDryRun {
		fmt.Println("  Mitlesen im TESTMODUS: schreibt nichts, zeigt nur hier, was er gesagt hätte")
	}
	log.Printf("[start] Klaus online als %s | Qwen erreichbar: %v | Bilder: %v | Websuche: %s | Denk-Stufe: %s, Budget %d | Kontext: %d Nachrichten, Pause %s | Mitlesen: alle=%v, Kanäle=%d, Wartezeit %s, Schwelle %d%%, Pause %s, Testmodus %v | Ankündigungen: %d Kanäle",
		dg.State.User.Username, brainOK, b.llm.vision.Load(), cfg.searchProvider(), cfg.reasoning, cfg.thinkingBudget, cfg.contextCount, cfg.contextGap,
		cfg.passiveAll, len(cfg.passiveChannels), cfg.passiveWait, cfg.passiveMinConf, cfg.passiveCooldown, cfg.passiveDryRun, len(cfg.announceChannels))
	log.Printf("[start] Kontextgröße von Qwen: %d Tokens | zweite Prüfung: %v | yt-dlp: %q", b.llm.contextSize(), cfg.reviewEnabled, findYtDlp())
	b.updateStatus(brainOK)
	go b.runReminders(ctx)
	b.announce(startupLine(brainOK, b.llm.vision.Load()))
	go b.watchBrain(ctx, brainOK)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-stop
	fmt.Println("\nKlaus faehrt herunter...")
	b.announce("bin dann mal weg 👋")
}

// announce schreibt in alle Kanaele aus KLAUS_ANNOUNCE_CHANNELS (kommagetrennte IDs).
func (b *bot) announce(text string) {
	for _, id := range b.cfg.announceChannels {
		if _, err := b.s.ChannelMessageSendComplex(id, &discordgo.MessageSend{Content: text, AllowedMentions: noPings}); err != nil {
			log.Printf("Ankuendigung in %s: %v", id, err)
		}
	}
}

func startupLine(brainOK, vision bool) string {
	if !brainOK {
		return "bin wach, aber mein gehirn (llama-server) läuft noch nicht – gleich gehts los"
	}
	lines := []string{
		"bin wieder da 👋 pingt mich, wenn ihr was wissen wollt",
		"klaus ist online. stellt mir eure schweren fragen",
		"wieder hochgefahren 🧠 wer hat was auf dem herzen?",
		"bin da. `!neu` vor der frage, wenn ich den chatverlauf ignorieren soll",
	}
	line := lines[time.Now().UnixNano()%int64(len(lines))]
	if vision {
		line += " – bilder kann ich auch 👀"
	}
	return line
}

func (b *bot) updateStatus(ok bool) {
	if ok {
		_ = b.s.UpdateCustomStatus("🧠 frag mich was – @" + b.s.State.User.Username)
	} else {
		_ = b.s.UpdateCustomStatus("😴 gehirn offline")
	}
}

// watchBrain aktualisiert den Discord-Status, wenn der llama-server kommt oder geht.
func (b *bot) watchBrain(ctx context.Context, ok bool) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := b.llm.healthy()
			if now == ok {
				continue
			}
			ok = now
			b.updateStatus(ok)
			log.Printf("llama-server erreichbar: %v (Bilder: %v)", ok, b.llm.vision.Load())
		}
	}
}

func (b *bot) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if s.State.User == nil || m.Author == nil || m.Author.ID == s.State.User.ID {
		return
	}
	label := b.channelLabel(m.ChannelID)
	if m.GuildID == "" {
		label = "DM"
	}
	if m.Author.Bot {
		log.Printf("[nachricht] %s %s [bot]: %s -> ignoriert (Bot)", label, m.Author.Username, logText(m.Message))
		return
	}
	botID := s.State.User.ID
	isDM := m.GuildID == ""
	replyToKlaus := m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == botID
	if !isDM && !isBotMentioned(m.Message, botID) && !replyToKlaus {
		if b.cfg.passiveEnabled(m.ChannelID) {
			log.Printf("[nachricht] %s %s: %s -> mitlesen in %s", label, displayName(m.Message), logText(m.Message), b.cfg.passiveWait)
			b.schedulePassive(m.ChannelID)
		} else {
			log.Printf("[nachricht] %s %s: %s -> ignoriert (kein Ping, Mitlesen aus)", label, displayName(m.Message), logText(m.Message))
		}
		return
	}
	how := "PING"
	if replyToKlaus {
		how = "ANTWORT AUF KLAUS: " + logText(m.ReferencedMessage)
	}
	log.Printf("[nachricht] %s %s: %s -> %s", label, displayName(m.Message), logText(m.Message), how)
	b.preemptPassive() // Pings haben Vorrang vor stillem Mitlesen

	prompt := cleanContent(m.Message, botID)
	prompt, resetCtx, forced := parsePrefixes(prompt)
	effort := forced
	if effort == "" {
		effort = b.cfg.reasoning
		hasImages := len(imageAttachments(m.Message)) > 0
		switch effort {
		case "auto":
			effort = chooseEffort(prompt, hasImages)
		case "stufen":
			effort = chooseEffortTiered(prompt, hasImages)
		}
	}
	if prompt == "" && m.ReferencedMessage == nil && len(imageAttachments(m.Message)) == 0 && len(fileAttachments(m.Message)) == 0 {
		_, _ = s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content: "was", Reference: m.Reference(), AllowedMentions: noPings,
		})
		return
	}

	ahead := b.pending.Add(1) - 1
	status := statusThinking + " **denkt nach…**"
	if ahead > 0 {
		status = fmt.Sprintf("%s **warteschlange** – platz %d", statusQueued, ahead)
	}
	msg, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content: status, Reference: m.Reference(), AllowedMentions: noPings,
	})
	if err != nil {
		b.pending.Add(-1)
		log.Printf("Status-Nachricht senden: %v", err)
		return
	}

	j := &job{m: m, statusMsgID: msg.ID, prompt: prompt, resetCtx: resetCtx, effort: effort}
	log.Printf("[ping] eingereiht: Denk-Stufe %s, Chatverlauf %v, Warteschlange vor ihm: %d", effort, !resetCtx, ahead)
	select {
	case b.queue <- j:
	default:
		log.Printf("[ping] Warteschlange voll, abgelehnt")
		b.pending.Add(-1)
		b.edit(m.ChannelID, msg.ID, "zu viel los gerade, frag gleich nochmal")
	}
}

// worker arbeitet alles nacheinander ab (eine GPU). Pings kommen immer
// zuerst, stilles Mitlesen nur, wenn keine Pings warten.
func (b *bot) worker(ctx context.Context) {
	for {
		select {
		case j := <-b.queue:
			b.handle(ctx, j)
			b.pending.Add(-1)
			continue
		case <-ctx.Done():
			return
		default:
		}
		select {
		case j := <-b.queue:
			b.handle(ctx, j)
			b.pending.Add(-1)
		case ch := <-b.passive.queue:
			b.runPassive(ctx, ch)
		case <-ctx.Done():
			return
		}
	}
}

func (b *bot) edit(channelID, msgID, content string) {
	_, err := b.s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: msgID, Channel: channelID, Content: &content, AllowedMentions: noPings,
	})
	if err != nil {
		log.Printf("Nachricht bearbeiten: %v", err)
	}
}

// liveState ist das, was die Status-Nachricht gerade anzeigt.
type liveState struct {
	mu        sync.Mutex
	reasoning string
	answer    string
	activity  string // z.B. "🔎 sucht: „...“" waehrend ein Tool laeuft
	searches  int
	pages     int
}

func (b *bot) toolCtxFor(j *job) toolCtx {
	m := j.m
	return toolCtx{
		scope:     b.memoryScope(m.GuildID, m.ChannelID, m.Author.ID),
		channelID: m.ChannelID, guildID: m.GuildID, userID: m.Author.ID, userName: displayName(m.Message), messageID: m.ID,
		request: j.prompt,
	}
}

func (l *liveState) set(f func(*liveState)) {
	l.mu.Lock()
	f(l)
	l.mu.Unlock()
}

func (l *liveState) snapshot() (reasoning, answer, activity string, searches, pages int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reasoning, l.answer, l.activity, l.searches, l.pages
}

func (b *bot) handle(ctx context.Context, j *job) {
	m := j.m
	start := time.Now()
	state := &liveState{}
	log.Printf("[ping] starte Antwort für %s (Stufe %s)", displayName(m.Message), j.effort)

	reqCtx, cancel := context.WithTimeout(ctx, b.cfg.timeout)
	defer cancel()

	// Live-Status: Denkzeit, Vorschau aufs Denken / aktuelle Suche, dann die Antwort.
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(editInterval)
		defer t.Stop()
		last := ""
		for {
			select {
			case <-done:
				return
			case <-t.C:
				text := b.progressText(state, time.Since(start))
				if text != last {
					b.edit(m.ChannelID, j.statusMsgID, text)
					last = text
				}
			}
		}
	}()

	// Discord haengt GIF-/Link-Vorschauen oft erst kurz nach dem Senden an.
	if len(findLinks(m.Content)) > 0 && len(m.Embeds) == 0 {
		time.Sleep(1500 * time.Millisecond)
		if fresh, err := b.s.ChannelMessage(m.ChannelID, m.ID); err == nil && len(fresh.Embeds) > 0 {
			m.Embeds = fresh.Embeds
			log.Printf("[ping] %d Link-Vorschau(en) nachgeladen", len(fresh.Embeds))
		}
	}

	// Bilder laden (nur wenn der Server sie versteht).
	var images []string
	var imageNote string
	if atts := imageAttachments(m.Message); len(atts) > 0 {
		if !b.llm.vision.Load() {
			b.llm.refreshVision()
		}
		if b.llm.vision.Load() {
			state.set(func(l *liveState) { l.activity = "🖼️ schaut sich das bild an…" })
			for _, a := range atts {
				t0 := time.Now()
				dataURL, err := loadImage(reqCtx, b.web.client, a)
				if err != nil {
					log.Printf("[bild] %s: Fehler: %v", a.Filename, err)
					imageNote += fmt.Sprintf("\n[image %s could not be loaded]", a.Filename)
					continue
				}
				log.Printf("[bild] %s geladen (%dx%d, %d KB, %.1f s)", a.Filename, a.Width, a.Height, len(dataURL)*3/4/1024, time.Since(t0).Seconds())
				images = append(images, dataURL)
			}
			state.set(func(l *liveState) { l.activity = "" })
		} else {
			log.Printf("[bild] %d Bild(er) angehängt, aber Server hat keine Bilderkennung (mmproj fehlt)", len(atts))
			imageNote = "\n[The user attached an image, but image input is not enabled on your server (no mmproj loaded). Tell them briefly you can't see images right now, then answer what you can from the text.]"
		}
	}

	// Dateien lesen (Text, Code, PDF).
	fileNote := ""
	if atts := fileAttachments(m.Message); len(atts) > 0 {
		state.set(func(l *liveState) { l.activity = "📎 liest die datei…" })
		fileNote = b.loadFiles(reqCtx, atts)
		state.set(func(l *liveState) { l.activity = "" })
	}

	messages := b.buildMessages(j, images, imageNote+fileNote)
	questionText := ""
	if q, ok := messages[len(messages)-1].Content.(string); ok {
		questionText = q // inkl. Chatverlauf, Gedaechtnis, Dateien – fuer die zweite Pruefung
	}
	log.Printf("[ping] Kontext: %d Nachrichten an Qwen, ca. %d Zeichen", len(messages), messagesChars(messages))

	tools := b.toolDefs()
	tctx := b.toolCtxFor(j)
	var evidence strings.Builder

	var res llmResult
	var err error
	var sources []string
	shrunk := false
	for round := 0; ; round++ {
		useTools := tools
		if round >= b.cfg.maxToolRounds && len(tools) > 0 {
			useTools = nil
			messages = append(messages, chatMessage{Role: "user", Content: "(Tool limit reached. Answer now with the information you have, and say if something is still unclear.)"})
		}
		state.set(func(l *liveState) { l.reasoning, l.answer = "", "" })
		log.Printf("[ping] Runde %d: frage Qwen (Werkzeuge: %v)", round+1, useTools != nil)
		res, err = b.llm.chat(reqCtx, messages, useTools, b.optionsFor(j.effort, messages, useTools, len(images)), func(r, a string) {
			state.set(func(l *liveState) { l.reasoning, l.answer = r, a })
		})
		if err != nil && !shrunk && round > 0 && isContextError(err) {
			log.Printf("[ping] Kontext voll – kürze Suchergebnisse und versuche nochmal")
			shrunk = true
			shrinkToolResults(messages)
			round--
			continue
		}
		if err != nil || len(res.toolCalls) == 0 || useTools == nil {
			break
		}

		calls := res.toolCalls
		if len(calls) > maxCallsPerRound {
			calls = calls[:maxCallsPerRound]
		}
		messages = append(messages, chatMessage{Role: "assistant", Content: res.answer, ToolCalls: calls})
		for _, tc := range calls {
			activity := toolActivity(tc)
			state.set(func(l *liveState) { l.activity = activity })
			result, source := b.runTool(reqCtx, tctx, tc, func(a string) {
				state.set(func(l *liveState) { l.activity = a })
			})
			if tc.Function.Name != "remember" && tc.Function.Name != "forget" {
				fmt.Fprintf(&evidence, "[%s %s]\n%s\n\n", tc.Function.Name, truncateRunes(oneLine(tc.Function.Arguments), 200), truncateRunes(result, 2500))
			}
			state.set(func(l *liveState) {
				l.activity = ""
				if tc.Function.Name == "web_search" {
					l.searches++
				} else if tc.Function.Name == "fetch_page" && source != "" {
					l.pages++
				}
			})
			if source != "" && !contains(sources, source) {
				sources = append(sources, source)
			}
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
	close(done)
	took := time.Since(start).Round(time.Second)

	if err != nil {
		log.Printf("[ping] FEHLER nach %s: %v", took, err)
		b.edit(m.ChannelID, j.statusMsgID, "⚠️ "+friendlyError(err, b.cfg))
		return
	}

	answer := strings.TrimSpace(res.answer)
	if answer == "" {
		if res.finishReason == "length" || strings.TrimSpace(res.reasoning) != "" {
			answer = "⚠️ hab zu lange nachgedacht und der kontext ist voll gelaufen. frag kürzer, oder mit `!neu` davor ohne chatverlauf."
		} else {
			answer = "hm, mir fällt dazu nix ein"
		}
	} else if res.finishReason == "length" {
		answer += "\n\n*(abgeschnitten – kontext voll)*"
	}

	// Zweite Pruefung (nicht bei Smalltalk, Bildern oder Fehlermeldungen).
	reviewed, corrected := false, false
	if b.cfg.reviewEnabled && j.effort != effortOff && len(images) == 0 && res.finishReason != "length" &&
		strings.TrimSpace(res.answer) != "" && reqCtx.Err() == nil {
		b.edit(m.ChannelID, j.statusMsgID, truncateRunes(answer, discordMaxMsgLen-60)+"\n-# 🔍 prüft die antwort nochmal…")
		question := questionText
		if m.ReferencedMessage != nil {
			question = "(replying to: " + truncateRunes(oneLine(m.ReferencedMessage.Content), 800) + ")\n" + question
		}
		rv := b.reviewAnswer(reqCtx, question, answer, evidence.String())
		reviewed, corrected = true, rv.changed
		answer = rv.answer
		took = time.Since(start).Round(time.Second)
	}

	_, _, _, searches, _ := state.snapshot()
	footer := buildFooter(took, strings.TrimSpace(res.reasoning) != "" || j.effort != effortOff, searches, sources)
	if corrected {
		footer = strings.Replace(footer, " nachgedacht", " nachgedacht · 🔍 nach prüfung korrigiert", 1)
	} else if reviewed {
		footer = strings.Replace(footer, " nachgedacht", " nachgedacht · 🔍 geprüft", 1)
	}

	if utf8.RuneCountInString(answer+footer) <= discordMaxMsgLen {
		b.edit(m.ChannelID, j.statusMsgID, answer+footer)
	} else {
		// Lang: eine einzige Nachricht mit Embed-Box statt mehrerer Nachrichten.
		log.Printf("[ping] lange Antwort (%d Zeichen) – sende als Embed", utf8.RuneCountInString(answer))
		if err := b.editLong(m.ChannelID, j.statusMsgID, answer, footer, sources); err != nil {
			log.Printf("[ping] Embed senden fehlgeschlagen: %v – schicke gekürzt als Text", err)
			b.edit(m.ChannelID, j.statusMsgID, truncateRunes(answer, discordMaxMsgLen-40)+"\n*(… gekürzt)*")
		}
	}
	log.Printf("Antwort an %s nach %s [denken: %s] (%d Zeichen Denken, %d Zeichen Antwort, %d Suchen, %d Quellen, %d Bilder)",
		m.Author.Username, took, j.effort, utf8.RuneCountInString(res.reasoning), utf8.RuneCountInString(answer), searches, len(sources), len(images))
}

// buildFooter: "-# 💭 47 s nachgedacht · 🔎 2 suchen" und darunter die gelesenen Quellen.
func buildFooter(took time.Duration, thought bool, searches int, sources []string) string {
	var parts []string
	if thought && took >= 3*time.Second {
		parts = append(parts, fmt.Sprintf("%s %s nachgedacht", statusThinking, formatDuration(took)))
	}
	if searches == 1 {
		parts = append(parts, "🔎 1 suche")
	} else if searches > 1 {
		parts = append(parts, fmt.Sprintf("🔎 %d suchen", searches))
	}
	footer := ""
	if len(parts) > 0 {
		footer = "\n-# " + strings.Join(parts, " · ")
	}
	if len(sources) > 0 {
		var links []string
		for i, s := range sources {
			if i == 4 {
				break
			}
			// <...> verhindert die grosse Link-Vorschau von Discord.
			links = append(links, fmt.Sprintf("[%s](<%s>)", hostOf(s), s))
		}
		footer += "\n-# Quellen: " + strings.Join(links, " · ")
	}
	return footer
}

func (b *bot) progressText(state *liveState, elapsed time.Duration) string {
	reasoning, answer, activity, searches, pages := state.snapshot()
	if strings.TrimSpace(answer) != "" && activity == "" {
		preview := truncateRunes(answer, discordMaxMsgLen-60)
		if strings.Count(preview, "```")%2 == 1 {
			preview += "\n```"
		}
		return preview + "\n-# ✍️ schreibt…"
	}
	text := fmt.Sprintf("%s **denkt nach…** `%s`", statusThinking, formatDuration(elapsed.Round(time.Second)))
	if searches > 0 {
		text += fmt.Sprintf(" · 🔎 %d", searches)
	}
	if pages > 0 {
		text += fmt.Sprintf(" · 📄 %d", pages)
	}
	switch {
	case activity != "":
		text += "\n-# " + activity
	case b.cfg.showPreview:
		if p := thinkingPreview(reasoning, 160); p != "" {
			text += "\n-# " + p
		}
	}
	return text
}

// thinkingPreview zeigt die letzten Worte des Denkprozesses als einzeilige Vorschau.
func thinkingPreview(reasoning string, maxRunes int) string {
	r := strings.Join(strings.Fields(reasoning), " ")
	r = strings.NewReplacer("`", "'", "*", "", "_", " ", "~", "", "|", "/", "#", "", ">", "", "[", "(", "]", ")").Replace(r)
	rs := []rune(r)
	if len(rs) == 0 {
		return ""
	}
	if len(rs) > maxRunes {
		return "…" + string(rs[len(rs)-maxRunes:])
	}
	return string(rs)
}

func isContextError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context") && (strings.Contains(msg, "exceed") || strings.Contains(msg, "too long") || strings.Contains(msg, "size"))
}

// shrinkToolResults kuerzt alle Tool-Ergebnisse, wenn der Kontext voll laeuft.
func shrinkToolResults(messages []chatMessage) {
	for i := range messages {
		if messages[i].Role == "tool" {
			if s, ok := messages[i].Content.(string); ok {
				messages[i].Content = truncateRunes(s, shrunkToolResults)
			}
		}
	}
}

func friendlyError(err error, cfg config) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("hat länger als %s gedauert, abgebrochen", formatDuration(cfg.timeout))
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "actively refused") ||
		strings.Contains(msg, "No connection could be made"):
		return "komm nicht an mein gehirn ran – läuft der llama-server?"
	case isContextError(err):
		return "zu viel text für meinen kontext. versuch's mit `!neu` davor."
	case strings.Contains(strings.ToLower(msg), "image") || strings.Contains(strings.ToLower(msg), "mmproj"):
		return "mit dem bild komm ich nicht klar (" + truncateRunes(msg, 150) + ")"
	}
	return "kein plan, da ist was schiefgelaufen (" + truncateRunes(msg, 150) + ")"
}

// ---------------------------------------------------------------------------
// Kontext bauen
// ---------------------------------------------------------------------------

func (b *bot) buildMessages(j *job, images []string, imageNote string) []chatMessage {
	m := j.m
	botID := b.s.State.User.ID
	now := time.Now()

	// 1) Antwortkette: Wer direkt auf Klaus (oder eine Nachricht) antwortet, fuehrt
	//    ein echtes Gespraech fort. Das ist das staerkste Signal fuer "gleiches Thema".
	chain := b.replyChain(m.Message, botID)
	inChain := map[string]bool{}
	for _, c := range chain {
		inChain[c.ID] = true
	}

	// 2) Kanalverlauf: letzte N Nachrichten, aber nur bis zur ersten grossen
	//    Pause. Eine lange Pause ist fast immer ein Themenwechsel.
	var logLines []string
	if !j.resetCtx && b.cfg.contextCount > 0 {
		recent, err := b.s.ChannelMessages(m.ChannelID, b.cfg.contextCount, m.ID, "", "")
		if err != nil {
			log.Printf("Kanalverlauf laden: %v", err)
		}
		prev := m.Timestamp
		var kept []*discordgo.Message
		var entries []logEntry
		for _, msg := range recent { // neueste zuerst
			if b.cfg.contextGap > 0 && prev.Sub(msg.Timestamp) > b.cfg.contextGap {
				break
			}
			if b.cfg.contextMaxAge > 0 && now.Sub(msg.Timestamp) > b.cfg.contextMaxAge {
				break
			}
			prev = msg.Timestamp
			kept = append(kept, msg)
		}
		for i := len(kept) - 1; i >= 0; i-- {
			msg := kept[i]
			if inChain[msg.ID] || msg.Author == nil {
				continue
			}
			if msg.Type != discordgo.MessageTypeDefault && msg.Type != discordgo.MessageTypeReply {
				continue
			}
			text := cleanContent(msg, botID)
			if msg.Author.ID == botID && isStatusText(text) {
				continue // noch laufende "denkt nach"-Nachrichten
			}
			text = stripFooter(text)
			text = strings.TrimSpace(text + attachmentHint(msg))
			if text == "" {
				continue
			}
			name := displayName(msg)
			if msg.Author.ID == botID {
				name = "Klaus (you)"
			}
			entries = append(entries, logEntry{when: msg.Timestamp, name: name, text: text})
		}
		logLines = formatChatLog(entries, now)
	}

	// System-Prompt bleibt immer gleich (nur Regeln). So kann llama-server ihn
	// samt Tool-Beschreibungen im Cache behalten und muss ihn nicht jedes Mal
	// neu einlesen. Alles, was sich aendert (Uhrzeit, Chatverlauf), kommt in
	// die Nutzer-Nachricht.
	var sys strings.Builder
	sys.WriteString(b.cfg.systemPrompt)
	if b.cfg.webSearch {
		sys.WriteString("\n\n" + toolPrompt)
	}
	sys.WriteString("\n\n" + toolRulesPrompt)
	sys.WriteString("\n\n" + selfInfo)
	sys.WriteString("\n\n" + contextRules)

	var ctxInfo strings.Builder
	fmt.Fprintf(&ctxInfo, "<context>\nCurrent date and time: %s.", now.Format("Monday, 2 January 2006, 15:04 MST"))
	if m.GuildID == "" {
		ctxInfo.WriteString(" This is a private DM.")
	}
	fmt.Fprintf(&ctxInfo, "\nThe person asking: %s.", displayName(m.Message))
	if mem := b.memory.promptBlock(b.toolCtxFor(j).memoryScope()); mem != "" {
		ctxInfo.WriteString("\n\n" + mem)
	}
	if len(logLines) > 0 {
		ctxInfo.WriteString("\n\n<chat_log>\n" + strings.Join(logLines, "\n") + "\n</chat_log>")
	} else if len(chain) == 0 {
		ctxInfo.WriteString("\nThere is no earlier conversation. Treat the message as a fresh, standalone question.")
	}
	ctxInfo.WriteString("\n</context>\n\n")

	messages := []chatMessage{{Role: "system", Content: sys.String()}}
	for _, c := range chain {
		text := strings.TrimSpace(stripFooter(cleanContent(c, botID)) + attachmentHint(c))
		if text == "" {
			continue
		}
		if c.Author != nil && c.Author.ID == botID {
			messages = append(messages, chatMessage{Role: "assistant", Content: truncateRunes(text, maxChainMsgRunes)})
		} else {
			messages = append(messages, chatMessage{Role: "user", Content: fmt.Sprintf("%s: %s", displayName(c), truncateRunes(text, maxChainMsgRunes))})
		}
	}

	prompt := j.prompt
	if prompt == "" && len(images) > 0 {
		prompt = "(no text – look at the image and respond to it)"
	} else if prompt == "" {
		prompt = "(no text – refers to the message they replied to)"
	}
	var otherFiles []string
	for _, a := range m.Attachments {
		if !isImageAttachment(a) {
			otherFiles = append(otherFiles, a.Filename)
		}
	}
	if len(otherFiles) > 0 {
		prompt += fmt.Sprintf("\n[attached files you cannot open: %s]", strings.Join(otherFiles, ", "))
	}
	prompt += imageNote
	if links := pageLinks(j.prompt); len(links) > 0 {
		prompt += "\n[The message contains links: " + strings.Join(links, " ") + ". If the question is about them, open them with youtube_transcript (YouTube) or fetch_page (other pages).]"
	}
	speaker := displayName(m.Message)
	if ref := m.ReferencedMessage; ref != nil && ref.Author != nil {
		quote := strings.TrimSpace(stripFooter(cleanContent(ref, botID)) + attachmentHint(ref))
		who := displayName(ref) + "'s message"
		if ref.Author.ID == botID {
			who = "YOUR (Klaus') earlier message"
		}
		// Die Nachricht selbst steht schon vollstaendig in der Antwortkette
		// (Nachrichten vor dieser); hier nur der Verweis mit Anfang.
		speaker += fmt.Sprintf(" (this is a direct reply to %s, its full text is in the conversation above: %q)", who, truncateRunes(oneLine(quote), 300))
	}
	userText := ctxInfo.String() + fmt.Sprintf("%s: %s", speaker, prompt)

	if len(images) == 0 {
		messages = append(messages, chatMessage{Role: "user", Content: userText})
		return messages
	}
	parts := []contentPart{{Type: "text", Text: userText}}
	for _, img := range images {
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: img}})
	}
	messages = append(messages, chatMessage{Role: "user", Content: parts})
	return messages
}

// attachmentHint markiert im Verlauf, dass eine Nachricht Bilder/Dateien hatte.
func attachmentHint(m *discordgo.Message) string {
	imgs, files := 0, 0
	for _, a := range m.Attachments {
		if isImageAttachment(a) {
			imgs++
		} else {
			files++
		}
	}
	hint := ""
	if imgs > 0 {
		hint += " [image]"
	}
	if files > 0 {
		hint += " [file]"
	}
	return hint
}

// replyChain folgt den Discord-Antworten zurueck und liefert sie (aelteste zuerst).
func (b *bot) replyChain(m *discordgo.Message, botID string) []*discordgo.Message {
	var chain []*discordgo.Message
	cur := m.ReferencedMessage
	if cur == nil && m.MessageReference != nil && m.MessageReference.MessageID != "" {
		cur, _ = b.s.ChannelMessage(m.ChannelID, m.MessageReference.MessageID)
	}
	seen := map[string]bool{}
	for cur != nil && len(chain) < maxChainDepth && !seen[cur.ID] {
		seen[cur.ID] = true
		chain = append(chain, cur)
		var next *discordgo.Message
		if cur.ReferencedMessage != nil {
			next = cur.ReferencedMessage
		} else if cur.MessageReference != nil && cur.MessageReference.MessageID != "" {
			chID := cur.MessageReference.ChannelID
			if chID == "" {
				chID = cur.ChannelID
			}
			if chID == "" {
				chID = m.ChannelID
			}
			var err error
			next, err = b.s.ChannelMessage(chID, cur.MessageReference.MessageID)
			if err != nil {
				break
			}
		}
		cur = next
	}
	for i, k := 0, len(chain)-1; i < k; i, k = i+1, k-1 {
		chain[i], chain[k] = chain[k], chain[i]
	}
	return chain
}

// ---------------------------------------------------------------------------
// Hilfsfunktionen
// ---------------------------------------------------------------------------

func isBotMentioned(m *discordgo.Message, botID string) bool {
	for _, u := range m.Mentions {
		if u.ID == botID {
			return true
		}
	}
	return false
}

// cleanContent entfernt den Bot-Ping und ersetzt andere Pings durch @name.
func cleanContent(m *discordgo.Message, botID string) string {
	raw := m.Content
	if m.Author != nil && m.Author.ID == botID && strings.TrimSpace(raw) == "" {
		raw = embedText(m) // lange Klaus-Antworten stehen in Embeds
	}
	content := strings.ReplaceAll(raw, "<@"+botID+">", "")
	content = strings.ReplaceAll(content, "<@!"+botID+">", "")
	for _, u := range m.Mentions {
		if u.ID == botID {
			continue
		}
		content = strings.ReplaceAll(content, "<@"+u.ID+">", "@"+u.Username)
		content = strings.ReplaceAll(content, "<@!"+u.ID+">", "@"+u.Username)
	}
	return strings.TrimSpace(content)
}

// parsePrefixes erkennt !neu, !schnell und !denk am Anfang (auch kombiniert).
func parsePrefixes(prompt string) (rest string, reset bool, effort string) {
	rest = prompt
	for changed := true; changed; {
		changed = false
		lower := strings.ToLower(rest)
		for _, group := range []struct {
			list []string
			act  func()
		}{
			{resetPrefixes, func() { reset = true }},
			{fastPrefixes, func() { effort = effortOff }},
			{deepPrefixes, func() { effort = effortDeep }},
		} {
			for _, p := range group.list {
				if strings.HasPrefix(lower, p) {
					group.act()
					rest = strings.TrimSpace(rest[len(p):])
					changed = true
					break
				}
			}
			if changed {
				break
			}
		}
	}
	return rest, reset, effort
}

// chooseEffort: Nur reiner Smalltalk wird ohne Nachdenken beantwortet.
// Alles andere bekommt die volle Stufe (xhigh). Benchmarks zeigen: "low"
// verliert Genauigkeit, "medium" ist bei leichten Fragen nicht schneller als
// xhigh. Das Denk-Budget (siehe genOptions) verhindert endloses Nachdenken.
func chooseEffort(prompt string, hasImages bool) string {
	if hasImages || strings.ContainsAny(prompt, "?0123456789`") {
		return effortXHigh
	}
	words := strings.Fields(strings.ToLower(prompt))
	if len(words) == 0 || len(words) > 4 {
		return effortXHigh
	}
	// Satzzeichen und Emojis am Rand ignorieren: "danke!!", "gute nacht 🌙"
	text := strings.Join(words, " ")
	text = strings.TrimFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	// Anrede am Ende weglassen: "danke dir", "gute nacht klaus"
	for _, w := range []string{" klaus", " dir", " euch", " alle", " leute", " dich", " du"} {
		text = strings.TrimSuffix(text, w)
	}
	for _, c := range chitChat {
		if text == c {
			return effortOff
		}
	}
	return effortXHigh
}

// Woerter, bei denen sich gruendliches Nachdenken lohnt (Modus "stufen").
var hardHints = []string{
	"beweis", "prove", "berechn", "rechne", "calculate", "erklär", "erklaer", "explain",
	"warum", "wieso", "weshalb", "why", "vergleich", "compare", "unterschied", "difference",
	"code", "programm", "funktion", "function", "skript", "script", "algorith", "debug",
	"fehler", "error", "bug", "löse", "loese", "solve", "analys", "optimier", "optimiz",
	"schreib mir", "write me", "strategie", "pro und contra", "vor- und nachteile",
}

// chooseEffortTiered (Modus "stufen"):
//   - Smalltalk und kurze Fragen (bis 12 Woerter): low
//   - grosse Fragen (Code, erklaeren/warum/rechnen/vergleichen, ueber 40 Woerter): xhigh
//   - alles andere (auch Bilder): medium
func chooseEffortTiered(prompt string, hasImages bool) string {
	lower := strings.ToLower(prompt)
	words := len(strings.Fields(prompt))
	if strings.Contains(prompt, "```") || words > 40 {
		return effortXHigh
	}
	for _, h := range hardHints {
		if strings.Contains(lower, h) {
			return effortXHigh
		}
	}
	if hasImages {
		return effortMedium
	}
	if words <= 12 {
		return effortLow
	}
	return effortMedium
}

func ptr[T any](v T) *T { return &v }

// genOptions baut Denk-Stufe, Budget und Sampling nach Qwens Empfehlung.
func (b *bot) genOptions(effort string) genOptions {
	if effort == effortOff {
		return genOptions{
			ChatTemplateKwargs: map[string]any{"enable_thinking": false},
			Temperature:        ptr(0.7), TopP: ptr(0.8), TopK: ptr(20), MinP: ptr(0.0), PresencePenalty: ptr(1.5),
		}
	}
	o := genOptions{
		ChatTemplateKwargs: map[string]any{"reasoning_effort": effort},
		Temperature:        ptr(1.0), TopP: ptr(0.95), TopK: ptr(20), MinP: ptr(0.0), PresencePenalty: ptr(0.0),
	}
	if effort == effortDeep {
		o.ChatTemplateKwargs["reasoning_effort"] = effortXHigh
	} else if budget := b.cfg.thinkingBudget; budget > 0 {
		o.ReasoningBudget = budget
		o.ReasoningMessage = "\n\nOkay, I have thought about this enough. Time to write the final answer with what I have.\n"
	}
	return o
}

func isStatusText(text string) bool {
	return strings.HasPrefix(text, statusThinking+" **denkt nach") || strings.HasPrefix(text, statusQueued)
}

// stripFooter entfernt die "-# ..."-Zeilen (Denkzeit, Quellen) aus alten Klaus-Antworten.
func stripFooter(text string) string {
	for _, marker := range []string{"\n-# " + statusThinking, "\n-# 🔎", "\n-# Quellen:", "\n-# ✍️"} {
		if i := strings.Index(text, marker); i >= 0 {
			text = text[:i]
		}
	}
	return strings.TrimSpace(text)
}

func displayName(m *discordgo.Message) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	if m.Author == nil {
		return "someone"
	}
	if m.Author.GlobalName != "" {
		return m.Author.GlobalName
	}
	return m.Author.Username
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// logText: Nachrichtentext fuers Log (einzeilig, gekuerzt, mit Anhaengen).
func logText(m *discordgo.Message) string {
	t := oneLine(truncateRunes(m.Content, 150))
	if n := len(m.Attachments); n > 0 {
		t += fmt.Sprintf(" [%d Anhang/Anhänge]", n)
	}
	return fmt.Sprintf("%q", t)
}

func messagesChars(msgs []chatMessage) int {
	n := 0
	for _, m := range msgs {
		switch c := m.Content.(type) {
		case string:
			n += utf8.RuneCountInString(c)
		case []contentPart:
			for _, p := range c {
				n += utf8.RuneCountInString(p.Text)
			}
		}
	}
	return n
}

type logEntry struct {
	when time.Time
	name string
	text string
}

// formatChatLog: neueste Nachrichten bekommen den vollen Platz (bis
// chatLogMsgRunes), aeltere werden gekuerzt, sobald chatLogTotalRunes
// verbraucht ist. Zeilenumbrueche bleiben erhalten (eingerueckt), damit
// Listen und Formatierung lesbar bleiben.
func formatChatLog(entries []logEntry, now time.Time) []string {
	limits := make([]int, len(entries))
	remaining := chatLogTotalRunes
	for i := len(entries) - 1; i >= 0; i-- {
		lim := min(chatLogMsgRunes, max(maxLogMsgRunes, remaining))
		limits[i] = lim
		remaining -= min(utf8.RuneCountInString(entries[i].text), lim)
	}
	lines := make([]string, 0, len(entries))
	for i, e := range entries {
		lines = append(lines, fmt.Sprintf("[%s ago] %s: %s", ago(now.Sub(e.when)), e.name, blockText(e.text, limits[i], "    ")))
	}
	return lines
}

// blockText kuerzt auf max Zeichen (mit Hinweis) und behaelt Zeilenumbrueche.
// Folgezeilen bekommen indent vorangestellt, leere Zeilen fallen weg.
func blockText(s string, max int, indent string) string {
	s = strings.TrimSpace(s)
	total := utf8.RuneCountInString(s)
	cut := false
	if total > max {
		s = string([]rune(s)[:max])
		cut = true
	}
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	res := strings.Join(out, "\n"+indent)
	if cut {
		res += fmt.Sprintf(" […gekürzt, %d von %d Zeichen]", max, total)
	}
	return res
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1 min"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh %dmin", int(d.Hours()), int(d.Minutes())%60)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return fmt.Sprintf("%d min %02d s", int(d.Minutes()), int(d.Seconds())%60)
}

// splitMessage teilt lange Texte an Absaetzen/Zeilen/Woertern und haelt
// Codebloecke ueber Nachrichtengrenzen hinweg korrekt geoeffnet/geschlossen.
func splitMessage(text string, maxLen int) []string {
	const reserve = 16 // Platz fuer ``` zum Schliessen/Oeffnen
	var chunks []string
	openFence := ""
	rest := text
	for {
		candidate := openFence + rest
		if utf8.RuneCountInString(candidate) <= maxLen {
			chunks = append(chunks, candidate)
			break
		}
		runes := []rune(candidate)
		limit := maxLen - reserve
		cut := limit
		window := string(runes[:limit])
		for _, sep := range []string{"\n\n", "\n", " "} {
			if i := strings.LastIndex(window, sep); i > len(window)/2 {
				cut = utf8.RuneCountInString(window[:i])
				break
			}
		}
		chunk := strings.TrimRight(string(runes[:cut]), " \n")
		rest = strings.TrimLeft(string(runes[cut:]), " \n")

		openFence = ""
		if fences := strings.Count(chunk, "```"); fences%2 == 1 {
			lang := ""
			if i := strings.LastIndex(chunk, "```"); i >= 0 {
				after := chunk[i+3:]
				if nl := strings.IndexAny(after, "\n "); nl >= 0 {
					after = after[:nl]
				}
				lang = strings.TrimSpace(after)
			}
			chunk += "\n```"
			openFence = "```" + lang + "\n"
		}
		chunks = append(chunks, chunk)
		if rest == "" {
			break
		}
	}
	return chunks
}

var reLink = regexp.MustCompile(`https?://[^\s<>()]+`)

func findLinks(s string) []string {
	var out []string
	for _, l := range reLink.FindAllString(s, 5) {
		out = append(out, strings.TrimRight(l, ".,!?;:'\")"))
	}
	return out
}

// optionsFor: genOptions plus Denk-Budget, das in den freien Kontext passt.
func (b *bot) optionsFor(effort string, msgs []chatMessage, tools []toolDef, images int) genOptions {
	o := b.genOptions(effort)
	if effort == effortOff {
		return o
	}
	est := estimateTokens(msgs, tools) + images*imageTokens
	free := b.llm.contextSize() - est - answerReserve
	limit := o.ReasoningBudget
	if limit == 0 || limit > free {
		if free < 512 {
			free = 512
		}
		if limit != free {
			log.Printf("[kontext] ca. %d Tokens belegt von %d – Denk-Budget %d statt %d", est, b.llm.contextSize(), free, limit)
		}
		o.ReasoningBudget = free
		o.ReasoningMessage = "\n\nOkay, I have thought about this enough. Time to write the final answer with what I have.\n"
	}
	return o
}

// estimateTokens: grob 3 Zeichen pro Token (vorsichtig fuer Deutsch und Code).
func estimateTokens(msgs []chatMessage, tools []toolDef) int {
	n := messagesChars(msgs) / 3
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)/3 + 10
		}
		n += 6
	}
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			n += len(b)/3 + 400 // + Tool-Anleitung im Template
		}
	}
	return n
}

// loadFiles liest Text-/Code-/PDF-Anhaenge fuer den Prompt.
func (b *bot) loadFiles(ctx context.Context, atts []*discordgo.MessageAttachment) string {
	var sb strings.Builder
	budget := maxAllFilesRunes
	for _, a := range atts {
		t0 := time.Now()
		data, err := downloadAttachment(ctx, b.web.client, a.URL)
		if err != nil {
			log.Printf("[datei] %s: Fehler: %v", a.Filename, err)
			fmt.Fprintf(&sb, "\n[file %s could not be downloaded: %v]", a.Filename, err)
			continue
		}
		var text, kind string
		if isPDFAttachment(a) {
			var pages int
			text, pages, err = pdfToText(data)
			kind = fmt.Sprintf("PDF, %d Seiten", pages)
			if err != nil {
				log.Printf("[datei] %s: %v", a.Filename, err)
				fmt.Fprintf(&sb, "\n[file %s: %v]", a.Filename, err)
				continue
			}
		} else {
			text, kind = decodeText(data), "Text"
		}
		total := utf8.RuneCountInString(text)
		limit := min(maxFileRunes, budget)
		if limit < 500 {
			fmt.Fprintf(&sb, "\n[file %s skipped: too much file content in one message]", a.Filename)
			continue
		}
		shown, cut := shortenMiddle(text, limit)
		budget -= utf8.RuneCountInString(shown)
		note := ""
		if cut {
			note = fmt.Sprintf(" (long file: %d characters, only beginning and end are shown – say so if it matters)", total)
		}
		fmt.Fprintf(&sb, "\n\n<file name=%q%s>\n%s\n</file>", a.Filename, note, shown)
		log.Printf("[datei] %s gelesen (%s, %d Zeichen, gezeigt %d, %.1f s)", a.Filename, kind, total, utf8.RuneCountInString(shown), time.Since(t0).Seconds())
	}
	return sb.String()
}

// pageLinks: Links ohne GIF-Seiten (die sieht Klaus als Bild).
func pageLinks(s string) []string {
	var out []string
	for _, l := range findLinks(s) {
		h := hostOf(l)
		if strings.HasSuffix(h, "tenor.com") || strings.HasSuffix(h, "giphy.com") {
			continue
		}
		out = append(out, l)
	}
	return out
}
