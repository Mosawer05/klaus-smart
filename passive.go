package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Mitlesen: Klaus liest still mit und antwortet nur, wenn er sich sehr sicher
// ist, dass eine Antwort wirklich etwas bringt. Kein "denkt nach", nichts.
// ---------------------------------------------------------------------------

const (
	passiveMaxAge   = 15 * time.Minute // aeltere Nachrichten beantwortet er nicht mehr ungefragt
	passiveQueueLen = 50
)

const passiveGatePrompt = `You silently watch a Discord chat. Decide quickly whether the messages marked (new) contain something where a knowledgeable helper could add real value: an unanswered question, a request for help, a problem someone is stuck on, or a clear factual mistake that matters.

Answer NO for: small talk, greetings, jokes, memes, banter, venting, personal conversations between people, opinions and taste, plans between friends, or questions that were already answered correctly.

Reply with exactly one word: MAYBE or NO.`

const passiveRules = `Right now you are silently reading this Discord channel. Nobody pinged you or asked you anything. Decide whether to jump in.

Reply ONLY if you are certain that a reply from you right now adds clear, significant value. Typical good reasons:
- Someone asked a question that nobody has answered (or that was answered wrong), and you know the answer for sure.
- Someone states a clear factual error that matters, and you can correct it with certainty.
- Someone is stuck on a concrete problem (code, tech, math, how-to) and you can solve it.

Do NOT reply when:
- People are just chatting, joking, greeting, venting, or talking personally to each other.
- The question is directed at a specific other person, or was already answered correctly.
- It is about opinion or taste, or you would only add a small remark, agreement, or a joke.
- You are not completely sure about the facts.
- The conversation has already moved on.

Silence is the default. When in any doubt, SKIP. An unnecessary reply is much worse than a missed one.

If you reply: answer like a normal member who happens to know this. Do not say that you were reading along, do not greet, get straight to the point, keep it short.

Output format. The first line is mandatory and must be exactly one of:
SKIP
REPLY #<message number> CONFIDENCE <0-100>
If REPLY, write your Discord message on the following lines. You can only reply to messages marked (new).`

type passiveRun struct {
	cancel context.CancelFunc
	id     int
}

type passiveState struct {
	mu            sync.Mutex
	queue         chan string
	timers        map[string]*time.Timer
	queued        map[string]bool
	running       map[string]passiveRun
	nextRun       int
	lastEvaluated map[string]string    // Kanal -> neueste gepruefte Nachricht
	lastSpoke     map[string]time.Time // Kanal -> wann Klaus zuletzt geschrieben hat
}

func newPassiveState() *passiveState {
	return &passiveState{
		queue:         make(chan string, passiveQueueLen),
		timers:        map[string]*time.Timer{},
		queued:        map[string]bool{},
		running:       map[string]passiveRun{},
		lastEvaluated: map[string]string{},
		lastSpoke:     map[string]time.Time{},
	}
}

func (c config) passiveEnabled(channelID string) bool {
	return c.passiveAll || c.passiveChannels[channelID]
}

// noticeSpoke merkt sich, dass Klaus in diesem Kanal geschrieben hat (Cooldown).
func (b *bot) noticeSpoke(channelID string) {
	b.passive.mu.Lock()
	b.passive.lastSpoke[channelID] = time.Now()
	b.passive.mu.Unlock()
}

// schedulePassive wird bei jeder neuen Nachricht aufgerufen. Eine laufende
// Pruefung fuer den Kanal wird abgebrochen und kurz danach neu gestartet.
func (b *bot) schedulePassive(channelID string) {
	ps := b.passive
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.running[channelID]; ok {
		log.Printf("[mitlesen] %s: neue Nachricht – breche laufende Prüfung ab und starte neu", b.channelLabel(channelID))
		r.cancel()
	}
	if t := ps.timers[channelID]; t != nil {
		t.Stop()
	}
	ps.timers[channelID] = time.AfterFunc(b.cfg.passiveWait, func() {
		ps.mu.Lock()
		defer ps.mu.Unlock()
		if ps.queued[channelID] {
			return
		}
		select {
		case ps.queue <- channelID:
			ps.queued[channelID] = true
			log.Printf("[mitlesen] %s: Ruhe erkannt, Prüfung eingereiht", b.channelLabel(channelID))
		default:
			log.Printf("[mitlesen] %s: Warteschlange voll, übersprungen", b.channelLabel(channelID))
		}
	})
}

// preemptPassive bricht alle stillen Pruefungen ab, damit ein Ping sofort
// drankommt. Die Kanaele werden danach neu eingeplant.
func (b *bot) preemptPassive() {
	ps := b.passive
	ps.mu.Lock()
	var chans []string
	for ch, r := range ps.running {
		log.Printf("[mitlesen] %s: unterbrochen, weil ein Ping Vorrang hat", b.channelLabel(ch))
		r.cancel()
		chans = append(chans, ch)
	}
	ps.mu.Unlock()
	for _, ch := range chans {
		b.schedulePassive(ch)
	}
}

type passiveCandidate struct {
	num int
	msg *discordgo.Message
}

// passiveLog baut den nummerierten Chatverlauf (aelteste zuerst) und liefert
// die Nachrichten, auf die Klaus ungefragt antworten duerfte.
func passiveLog(recent []*discordgo.Message, lastEvaluated, botID string, now time.Time, gap, maxAge time.Duration) (lines []string, cands []passiveCandidate) {
	// recent: neueste zuerst. Bis zur ersten grossen Pause zuruecklaufen.
	var kept []*discordgo.Message
	prev := now
	for _, m := range recent {
		if gap > 0 && prev.Sub(m.Timestamp) > gap && len(kept) > 0 {
			break
		}
		if maxAge > 0 && now.Sub(m.Timestamp) > maxAge {
			break
		}
		prev = m.Timestamp
		kept = append(kept, m)
	}
	isNew := map[string]bool{}
	for _, m := range kept {
		if m.ID == lastEvaluated {
			break
		}
		isNew[m.ID] = true
	}
	num := 0
	for i := len(kept) - 1; i >= 0; i-- {
		m := kept[i]
		if m.Author == nil || (m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply) {
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
		num++
		name := displayName(m)
		if m.Author.ID == botID {
			name = "Klaus (you)"
		} else if m.Author.Bot {
			name += " [bot]"
		}
		reply := ""
		if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil {
			reply = fmt.Sprintf(" (replying to %s)", displayName(m.ReferencedMessage))
		}
		mark := ""
		if isNew[m.ID] && m.Author.ID != botID && !m.Author.Bot && now.Sub(m.Timestamp) <= passiveMaxAge &&
			!isBotMentioned(m, botID) && !pingsSomeoneElse(m, botID) {
			mark = " (new)"
			cands = append(cands, passiveCandidate{num: num, msg: m})
		}
		body := oneLine(truncateRunes(text, maxLogMsgRunes))
		if mark != "" { // neue Nachrichten ganz lesen, aeltere reichen kurz
			body = blockText(text, passiveNewMsgRunes, "    ")
		}
		lines = append(lines, fmt.Sprintf("#%d [%s ago] %s%s%s: %s", num, ago(now.Sub(m.Timestamp)), name, reply, mark, body))
	}
	return lines, cands
}

var (
	reDecisionLine = regexp.MustCompile(`(?i)^[\s*_>#-]*(SKIP|REPLY)\b(.*)$`)
	reDecisionNum  = regexp.MustCompile(`#\s*(\d+)`)
	reDecisionConf = regexp.MustCompile(`(?i)CONFIDENCE\D{0,3}(\d{1,3})`)
)

// parseDecision liest "SKIP" oder "REPLY #n CONFIDENCE c" + Antworttext.
func parseDecision(out string) (reply bool, num, conf int, text string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		m := reDecisionLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil || strings.ToUpper(m[1]) != "REPLY" {
			return false, 0, 0, ""
		}
		if n := reDecisionNum.FindStringSubmatch(m[2]); n != nil {
			num, _ = strconv.Atoi(n[1])
		}
		if c := reDecisionConf.FindStringSubmatch(m[2]); c != nil {
			conf, _ = strconv.Atoi(c[1])
		}
		text = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		return text != "", num, conf, text
	}
	return false, 0, 0, ""
}

func (b *bot) channelLabel(channelID string) string {
	if ch, err := b.s.State.Channel(channelID); err == nil && ch.Name != "" {
		return "#" + ch.Name
	}
	return channelID
}

func (b *bot) runPassive(ctx context.Context, channelID string) {
	ps := b.passive
	ps.mu.Lock()
	ps.queued[channelID] = false
	if b.cfg.passiveCooldown > 0 && time.Since(ps.lastSpoke[channelID]) < b.cfg.passiveCooldown {
		left := b.cfg.passiveCooldown - time.Since(ps.lastSpoke[channelID])
		ps.mu.Unlock()
		log.Printf("[mitlesen] %s: Pause nach eigener Antwort, noch %s", b.channelLabel(channelID), left.Round(time.Second))
		return
	}
	pctx, cancel := context.WithTimeout(ctx, b.cfg.timeout)
	ps.nextRun++
	runID := ps.nextRun
	ps.running[channelID] = passiveRun{cancel: cancel, id: runID}
	lastEval := ps.lastEvaluated[channelID]
	ps.mu.Unlock()
	defer func() {
		ps.mu.Lock()
		if r, ok := ps.running[channelID]; ok && r.id == runID {
			delete(ps.running, channelID)
		}
		ps.mu.Unlock()
		cancel()
	}()

	label := b.channelLabel(channelID)
	botID := b.s.State.User.ID
	log.Printf("[mitlesen] %s: starte Prüfung", label)
	recent, err := b.s.ChannelMessages(channelID, b.cfg.contextCount, "", "", "")
	if err != nil || len(recent) == 0 {
		log.Printf("[mitlesen] %s: Verlauf laden fehlgeschlagen: %v (Recht 'Nachrichtenverlauf anzeigen'?)", label, err)
		return
	}
	newest := recent[0].ID
	markEvaluated := func() {
		ps.mu.Lock()
		ps.lastEvaluated[channelID] = newest
		ps.mu.Unlock()
	}
	if newest == lastEval {
		log.Printf("[mitlesen] %s: nichts Neues seit letzter Prüfung", label)
		return
	}
	now := time.Now()
	lines, cands := passiveLog(recent, lastEval, botID, now, b.cfg.contextGap, b.cfg.contextMaxAge)
	if len(cands) == 0 {
		markEvaluated()
		log.Printf("[mitlesen] %s: keine Nachricht, auf die er antworten dürfte (%d im Verlauf)", label, len(lines))
		return
	}
	log.Printf("[mitlesen] %s: %d Nachrichten im Verlauf, %d neu: %s", label, len(lines), len(cands), candidatesText(cands))
	chatLog := "<chat_log>\n" + strings.Join(lines, "\n") + "\n</chat_log>"
	start := time.Now()

	// Schritt 1: Blitz-Check ohne Nachdenken.
	gate, err := b.llm.chat(pctx, []chatMessage{
		{Role: "system", Content: passiveGatePrompt},
		{Role: "user", Content: chatLog + "\n\nMAYBE or NO?"},
	}, nil, b.genOptions(effortOff), nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[mitlesen] %s: Fehler beim Vorfilter: %v", label, err)
		}
		return
	}
	log.Printf("[mitlesen] %s: Vorfilter antwortet %q", label, oneLine(truncateRunes(gate.answer, 60)))
	if !strings.Contains(strings.ToUpper(gate.answer), "MAYBE") {
		markEvaluated()
		log.Printf("[mitlesen] %s: Vorfilter sagt nein (%.1f s) zu: %s", label, time.Since(start).Seconds(), candidatesText(cands))
		return
	}

	// Schritt 2: Gruendlich nachdenken, ob und was er antwortet.
	var nums []string
	for _, c := range cands {
		nums = append(nums, fmt.Sprintf("#%d", c.num))
	}
	guildID := ""
	if ch, err := b.s.State.Channel(channelID); err == nil {
		guildID = ch.GuildID
	} else if ch, err := b.s.Channel(channelID); err == nil {
		guildID = ch.GuildID
	}
	scope := ""
	if guildID != "" {
		scope = b.memoryScope(guildID, channelID, "")
	}
	mem := ""
	if scope != "" {
		mem = b.memory.promptBlock(scope)
	}
	if mem != "" {
		mem = "\n\n" + mem
	}
	user := fmt.Sprintf("<context>\nCurrent date and time: %s.%s\n\n%s\n</context>\n\nMessages you could reply to: %s. Decide now: SKIP or REPLY.",
		now.Format("Monday, 2 January 2006, 15:04 MST"), mem, chatLog, strings.Join(nums, ", "))
	opts := b.genOptions(effortXHigh)
	opts.ReasoningBudget = b.cfg.passiveBudget
	log.Printf("[mitlesen] %s: Vorfilter sagt vielleicht – denke gründlich nach", label)
	res, err := b.llm.chat(pctx, []chatMessage{
		{Role: "system", Content: b.cfg.systemPrompt + "\n\n" + passiveRules},
		{Role: "user", Content: user},
	}, nil, opts, nil)
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[mitlesen] %s: Fehler: %v", label, err)
		}
		return
	}
	if pctx.Err() != nil { // inzwischen neue Nachricht -> wird neu geprueft
		log.Printf("[mitlesen] %s: abgebrochen (neue Nachricht oder Ping)", label)
		return
	}
	markEvaluated()
	first := strings.SplitN(strings.TrimSpace(res.answer), "\n", 2)[0]
	log.Printf("[mitlesen] %s: Entscheidung von Qwen: %q", label, oneLine(truncateRunes(first, 100)))

	reply, num, conf, text := parseDecision(res.answer)
	if !reply {
		log.Printf("[mitlesen] %s: geprüft, schweigt (%s) – Gedanke: %s", label, took, thoughtTail(res.reasoning))
		return
	}
	var target *discordgo.Message
	for _, c := range cands {
		if c.num == num {
			target = c.msg
		}
	}
	if target == nil && len(cands) == 1 {
		target = cands[0].msg
	}
	if target == nil || conf < b.cfg.passiveMinConf {
		log.Printf("[mitlesen] %s: wollte antworten, aber nur %d%% sicher (Grenze %d%%) – schweigt (%s). Entwurf: %s",
			label, conf, b.cfg.passiveMinConf, took, oneLine(truncateRunes(text, 200)))
		return
	}
	if b.cfg.passiveDryRun {
		log.Printf("[mitlesen] %s: TESTMODUS – hätte geantwortet (%d%% sicher, %s) auf %s: %s",
			label, conf, took, displayName(target), oneLine(truncateRunes(text, 300)))
		return
	}

	quiet := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}} // ungefragt: niemanden anpingen
	var msgs []*discordgo.MessageSend
	if utf8.RuneCountInString(text) <= discordMaxMsgLen {
		msgs = append(msgs, &discordgo.MessageSend{Content: text})
	} else {
		for _, box := range buildBoxes(text, "", nil) {
			msgs = append(msgs, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{box}})
		}
	}
	msgs[0].Reference = target.Reference()
	for _, msg := range msgs {
		msg.AllowedMentions = quiet
		if _, err := b.s.ChannelMessageSendComplex(channelID, msg); err != nil {
			log.Printf("[mitlesen] %s: senden fehlgeschlagen: %v", label, err)
			return
		}
	}
	b.noticeSpoke(channelID)
	log.Printf("[mitlesen] %s: hat geantwortet auf %s (%d%% sicher, %s)", label, displayName(target), conf, took)
}

// thoughtTail: das Ende des Denkprozesses, einzeilig (fuers Log).
func thoughtTail(reasoning string) string {
	r := []rune(oneLine(reasoning))
	if len(r) > 250 {
		return "…" + string(r[len(r)-250:])
	}
	return string(r)
}

func candidatesText(cands []passiveCandidate) string {
	var parts []string
	for _, c := range cands {
		parts = append(parts, fmt.Sprintf("%s: %q", displayName(c.msg), truncateRunes(oneLine(c.msg.Content), 80)))
	}
	return strings.Join(parts, " | ")
}

// pingsSomeoneElse: Nachricht pingt gezielt eine andere Person oder einen
// anderen Bot – die ist nicht an Klaus gerichtet.
func pingsSomeoneElse(m *discordgo.Message, botID string) bool {
	for _, u := range m.Mentions {
		if u.ID != botID && (m.Author == nil || u.ID != m.Author.ID) {
			return true
		}
	}
	return false
}
