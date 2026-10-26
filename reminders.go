package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ---------------------------------------------------------------------------
// Erinnerungen: gespeichert in reminders.json, ueberleben Neustarts.
// ---------------------------------------------------------------------------

const maxRemindersPerUser = 25

type reminder struct {
	ID        int       `json:"id"`
	ChannelID string    `json:"channel_id"`
	UserID    string    `json:"user_id"`
	UserName  string    `json:"user_name"`
	Text      string    `json:"text"`
	Due       time.Time `json:"due"`
	Created   time.Time `json:"created"`
	Fails     int       `json:"fails,omitempty"`
}

type reminderStore struct {
	mu     sync.Mutex
	path   string
	NextID int         `json:"next_id"`
	Items  []*reminder `json:"items"`
	now    func() time.Time
}

func loadReminders(path string) *reminderStore {
	r := &reminderStore{path: path, NextID: 1, now: time.Now}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, r); err != nil {
			log.Printf("[erinnerung] %s kaputt, fange leer an: %v", path, err)
			_ = os.Rename(path, path+".kaputt")
			r.Items, r.NextID = nil, 1
		}
	}
	log.Printf("[erinnerung] %d offene Erinnerungen geladen", len(r.Items))
	return r
}

func (r *reminderStore) saveLocked() {
	if r.path == "" {
		return
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, r.path)
	} else {
		log.Printf("[erinnerung] speichern fehlgeschlagen: %v", err)
	}
}

// parseReminderTime versteht "2026-10-26 08:00", ISO/RFC3339 und "08:00"
// (naechstes Mal, dass es 08:00 ist). Lokale Zeitzone.
func parseReminderTime(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "T", " "))
	s = strings.TrimSuffix(s, " Uhr")
	if t, err := time.Parse(time.RFC3339, strings.Replace(s, " ", "T", 1)); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "02.01.2006 15:04", "2.1.2006 15:04"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	for _, layout := range []string{"15:04", "15.04"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			d := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if !d.After(now) {
				d = d.AddDate(0, 0, 1)
			}
			return d, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown time format %q, use 2006-01-02 15:04", s)
}

func (r *reminderStore) add(tc toolCtx, when, text string) string {
	now := r.now()
	due, err := parseReminderTime(when, now)
	if err != nil {
		return "Error: " + err.Error()
	}
	text = truncateRunes(oneLine(text), 300)
	switch {
	case text == "":
		return "Error: empty reminder text."
	case due.Before(now.Add(-time.Minute)):
		return fmt.Sprintf("Error: %s is in the past (now is %s).", due.Format("2006-01-02 15:04"), now.Format("2006-01-02 15:04"))
	case due.After(now.AddDate(2, 0, 0)):
		return "Error: more than 2 years in the future."
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	open := 0
	for _, it := range r.Items {
		if it.UserID == tc.userID {
			open++
		}
	}
	if open >= maxRemindersPerUser {
		return fmt.Sprintf("Error: this person already has %d open reminders.", open)
	}
	it := &reminder{ID: r.NextID, ChannelID: tc.channelID, UserID: tc.userID, UserName: tc.userName, Text: text, Due: due, Created: now}
	r.NextID++
	r.Items = append(r.Items, it)
	r.saveLocked()
	log.Printf("[erinnerung] #%d für %s am %s: %s", it.ID, tc.userName, due.Format("02.01.2006 15:04"), text)
	return fmt.Sprintf("Reminder #%d set for %s (%s from now).", it.ID, due.Format("Monday 2006-01-02 15:04"), humanDuration(due.Sub(now)))
}

func (r *reminderStore) list(tc toolCtx) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var mine []*reminder
	for _, it := range r.Items {
		if it.UserID == tc.userID {
			mine = append(mine, it)
		}
	}
	if len(mine) == 0 {
		return "No open reminders."
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Due.Before(mine[j].Due) })
	var sb strings.Builder
	for _, it := range mine {
		fmt.Fprintf(&sb, "#%d %s: %s\n", it.ID, it.Due.Format("Mon 2006-01-02 15:04"), it.Text)
	}
	return sb.String()
}

func (r *reminderStore) cancel(tc toolCtx, id string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(id), "#"))
	if err != nil {
		return "Error: give the reminder number, e.g. #2."
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, it := range r.Items {
		if it.ID == n {
			if it.UserID != tc.userID {
				return "Error: that reminder belongs to someone else."
			}
			r.Items = append(r.Items[:i], r.Items[i+1:]...)
			r.saveLocked()
			log.Printf("[erinnerung] #%d gelöscht von %s", n, tc.userName)
			return fmt.Sprintf("Reminder #%d cancelled.", n)
		}
	}
	return fmt.Sprintf("Error: no reminder #%d.", n)
}

// due nimmt alle faelligen Erinnerungen heraus.
func (r *reminderStore) takeDue() []*reminder {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var due, rest []*reminder
	for _, it := range r.Items {
		if !it.Due.After(now) {
			due = append(due, it)
		} else {
			rest = append(rest, it)
		}
	}
	if len(due) > 0 {
		r.Items = rest
		r.saveLocked()
	}
	return due
}

func (r *reminderStore) putBack(it *reminder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Items = append(r.Items, it)
	r.saveLocked()
}

// runReminders verschickt faellige Erinnerungen (alle 15 Sekunden pruefen).
func (b *bot) runReminders(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		for _, it := range b.reminders.takeDue() {
			text := fmt.Sprintf("⏰ <@%s> erinnerung: %s", it.UserID, it.Text)
			if late := time.Since(it.Due); late > 2*time.Minute {
				text += fmt.Sprintf("\n-# verspätet – war für %s, da war ich offline", it.Due.Format("02.01. 15:04"))
			}
			_, err := b.s.ChannelMessageSendComplex(it.ChannelID, &discordgo.MessageSend{
				Content:         text,
				AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{it.UserID}},
			})
			if err != nil {
				it.Fails++
				log.Printf("[erinnerung] #%d senden fehlgeschlagen (%d. Versuch): %v", it.ID, it.Fails, err)
				if it.Fails < 5 {
					it.Due = time.Now().Add(time.Minute)
					b.reminders.putBack(it)
				}
				continue
			}
			log.Printf("[erinnerung] #%d an %s verschickt: %s", it.ID, it.UserName, it.Text)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func humanDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dmin", h, m)
	}
	return fmt.Sprintf("%dmin", m)
}
