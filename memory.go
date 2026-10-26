package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Gedaechtnis: Fakten pro Server (bzw. pro DM), gespeichert in memory.json.
// ---------------------------------------------------------------------------

const (
	maxMemoryFacts     = 150  // pro Server
	maxMemoryFactRunes = 300  // pro Fakt
	maxMemoryPrompt    = 3500 // so viele Zeichen Gedaechtnis gehen hoechstens in den Prompt
)

type memoryFact struct {
	Text    string    `json:"text"`
	By      string    `json:"by"`
	Created time.Time `json:"created"`
}

type memoryStore struct {
	mu    sync.Mutex
	path  string
	Facts map[string][]memoryFact `json:"facts"` // Scope ("ch:<Kanal>" oder "dm:<User>") -> Fakten
}

func dataPath(name string) string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), name)
	}
	return name
}

func loadMemory(path string) *memoryStore {
	m := &memoryStore{path: path, Facts: map[string][]memoryFact{}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, m); err != nil {
			log.Printf("[gedächtnis] %s kaputt, fange leer an: %v", path, err)
			_ = os.Rename(path, path+".kaputt")
			m.Facts = map[string][]memoryFact{}
		}
	}
	if m.Facts == nil {
		m.Facts = map[string][]memoryFact{}
	}
	n := 0
	for _, f := range m.Facts {
		n += len(f)
	}
	log.Printf("[gedächtnis] %d Fakten geladen aus %s", n, path)
	return m
}

func (m *memoryStore) saveLocked() {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("[gedächtnis] speichern fehlgeschlagen: %v", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		log.Printf("[gedächtnis] speichern fehlgeschlagen: %v", err)
	}
}

func (m *memoryStore) add(scope, fact, by string) string {
	fact = oneLine(fact)
	if fact == "" {
		return "Error: empty fact."
	}
	fact = truncateRunes(fact, maxMemoryFactRunes)
	m.mu.Lock()
	defer m.mu.Unlock()
	facts := m.Facts[scope]
	for _, f := range facts {
		if strings.EqualFold(f.Text, fact) {
			return "Already saved."
		}
	}
	facts = append(facts, memoryFact{Text: fact, By: by, Created: time.Now()})
	if len(facts) > maxMemoryFacts {
		facts = facts[len(facts)-maxMemoryFacts:] // aelteste fliegen raus
	}
	m.Facts[scope] = facts
	m.saveLocked()
	log.Printf("[gedächtnis] gemerkt (%s, von %s): %s", scope, by, fact)
	return fmt.Sprintf("Saved as #%d.", len(facts))
}

// forget loescht per Nummer ("#3") oder alle Fakten, die alle Woerter enthalten.
func (m *memoryStore) forget(scope, match string) string {
	match = strings.TrimSpace(match)
	if match == "" {
		return "Error: say what to forget."
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	facts := m.Facts[scope]
	var keep []memoryFact
	var removed []string
	if n, err := strconv.Atoi(strings.TrimPrefix(match, "#")); err == nil && strings.HasPrefix(match, "#") {
		for i, f := range facts {
			if i+1 == n {
				removed = append(removed, f.Text)
			} else {
				keep = append(keep, f)
			}
		}
	} else {
		words := strings.Fields(strings.ToLower(match))
		for _, f := range facts {
			low := strings.ToLower(f.Text)
			all := true
			for _, w := range words {
				if !strings.Contains(low, w) {
					all = false
					break
				}
			}
			if all {
				removed = append(removed, f.Text)
			} else {
				keep = append(keep, f)
			}
		}
	}
	if len(removed) == 0 {
		return "Nothing matched. Nothing was deleted."
	}
	m.Facts[scope] = keep
	m.saveLocked()
	log.Printf("[gedächtnis] vergessen (%s): %s", scope, strings.Join(removed, " | "))
	return "Deleted: " + strings.Join(removed, " | ")
}

// promptBlock liefert die Fakten fuer den Prompt (neueste zuerst, wenn zu viele).
func (m *memoryStore) promptBlock(scope string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	facts := m.Facts[scope]
	if len(facts) == 0 {
		return ""
	}
	var lines []string
	total := 0
	for i := len(facts) - 1; i >= 0; i-- {
		l := fmt.Sprintf("#%d %s", i+1, facts[i].Text)
		if total+len(l) > maxMemoryPrompt {
			break
		}
		total += len(l) + 1
		lines = append([]string{l}, lines...)
	}
	return "<memory>\nThings you remember from this channel (may be outdated):\n" + strings.Join(lines, "\n") + "\n</memory>"
}
