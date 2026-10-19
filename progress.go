package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type progressEntry struct {
	Token              json.RawMessage
	Title, Message     string
	Percentage         *int
	Cancellable, Begun bool
	Serial             uint64
}

func progressToken(raw json.RawMessage) (string, json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return "", nil, false
	}
	var text string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &text) == nil && len(text) <= 1024 {
		canonical, _ := json.Marshal(text)
		return "s:" + text, canonical, true
	}
	number, err := strconv.ParseInt(string(raw), 10, 32)
	if err != nil {
		return "", nil, false
	}
	canonical := json.RawMessage(strconv.FormatInt(number, 10))
	return "i:" + string(canonical), canonical, true
}

func statusText(text string) string {
	text = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, text)
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) > 240 {
		text = string(runes[:240]) + "..."
	}
	return text
}

func (l *lsp) initStatusLocked() {
	if l.statusActive == nil {
		l.statusActive = map[string]uint64{}
	}
	if l.progressTokens == nil {
		l.progressTokens = map[uint64]map[string]*progressEntry{}
	}
}

func (l *lsp) activateStatus(s *server) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	l.initStatusLocked()
	delete(l.progressTokens, l.statusActive[s.lang])
	l.statusGeneration++
	s.generation = l.statusGeneration
	l.statusActive[s.lang] = s.generation
	l.progressTokens[s.generation] = map[string]*progressEntry{}
}

func (l *lsp) retireStatus(s *server) {
	l.statusMu.Lock()
	defer l.statusMu.Unlock()
	if l.statusActive[s.lang] != s.generation {
		return
	}
	delete(l.statusActive, s.lang)
	delete(l.progressTokens, s.generation)
	l.publishStatusLocked(s.name() + " stopped")
}

// One field is shared by every server. Select the latest live token across languages when a
// token ends or a server retires, rather than losing another server's progress.
func (l *lsp) publishStatusLocked(fallback string) {
	var latest *progressEntry
	var language string
	for lang, generation := range l.statusActive {
		for _, entry := range l.progressTokens[generation] {
			if entry.Begun && (latest == nil || entry.Serial > latest.Serial) {
				latest, language = entry, lang
			}
		}
	}
	text := fallback
	if latest != nil {
		text = language + ": " + latest.Title
		if latest.Message != "" {
			text += " " + latest.Message
		}
		if latest.Percentage != nil {
			text += fmt.Sprintf(" (%d%%)", *latest.Percentage)
		}
	}
	l.statusLatest = statusText(text)
	select {
	case l.statusWake <- struct{}{}:
	default:
	}
}

func (s *server) status(text string, terminal bool) {
	s.l.statusMu.Lock()
	defer s.l.statusMu.Unlock()
	if s.l.statusActive[s.lang] != s.generation || (s.ended.Load() && !terminal) {
		return
	}
	if terminal {
		delete(s.l.progressTokens, s.generation)
	}
	s.l.publishStatusLocked(text)
}

func (l *lsp) startStatusWorker() {
	l.statusMu.Lock()
	l.initStatusLocked()
	l.statusWake = make(chan struct{}, 1)
	l.statusDone = make(chan struct{})
	l.statusMu.Unlock()
	go func() {
		for {
			select {
			case <-l.statusDone:
				return
			case <-l.statusWake:
				l.statusMu.Lock()
				text := l.statusLatest
				l.statusMu.Unlock()
				if err := l.ext.Status(text); err != nil {
					l.ext.Logf("status: %v", err)
				}
			}
		}
	}()
}

func (l *lsp) closeStatus() {
	l.statusOnce.Do(func() {
		if l.statusDone != nil {
			close(l.statusDone)
		}
	})
}

func (s *server) createProgress(params json.RawMessage) (any, int, string) {
	var p struct {
		Token json.RawMessage `json:"token"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, -32602, "invalid progress token"
	}
	key, token, ok := progressToken(p.Token)
	if !ok {
		return nil, -32602, "invalid progress token"
	}
	s.l.statusMu.Lock()
	defer s.l.statusMu.Unlock()
	if s.ended.Load() || s.l.statusActive[s.lang] != s.generation {
		return nil, -32600, "server is no longer active"
	}
	entries := s.l.progressTokens[s.generation]
	if entries[key] != nil || len(entries) >= 64 {
		return nil, -32602, "duplicate token or progress limit reached"
	}
	entries[key] = &progressEntry{Token: token}
	return nil, 0, ""
}

func (s *server) progress(params json.RawMessage) {
	var p struct {
		Token json.RawMessage `json:"token"`
		Value struct {
			Kind        string  `json:"kind"`
			Title       string  `json:"title"`
			Message     *string `json:"message"`
			Percentage  *int    `json:"percentage"`
			Cancellable *bool   `json:"cancellable"`
		} `json:"value"`
	}
	if json.Unmarshal(params, &p) != nil || (p.Value.Percentage != nil && (*p.Value.Percentage < 0 || *p.Value.Percentage > 100)) {
		return
	}
	key, _, ok := progressToken(p.Token)
	if !ok {
		return
	}
	s.l.statusMu.Lock()
	defer s.l.statusMu.Unlock()
	if s.ended.Load() || s.l.statusActive[s.lang] != s.generation {
		return
	}
	entries := s.l.progressTokens[s.generation]
	entry := entries[key]
	if entry == nil {
		return
	}
	switch p.Value.Kind {
	case "begin":
		if entry.Begun || p.Value.Title == "" {
			return
		}
		entry.Begun = true
		entry.Title = statusText(p.Value.Title)
	case "report":
		if !entry.Begun {
			return
		}
	case "end":
		delete(entries, key)
		s.l.publishStatusLocked(s.name())
		return
	default:
		return
	}
	if p.Value.Message != nil {
		entry.Message = statusText(*p.Value.Message)
	}
	if p.Value.Percentage != nil {
		entry.Percentage = p.Value.Percentage
	}
	if p.Value.Cancellable != nil {
		entry.Cancellable = *p.Value.Cancellable
	}
	s.l.statusSerial++
	entry.Serial = s.l.statusSerial
	s.l.publishStatusLocked(s.name())
}

func (l *lsp) cancelProgress() int {
	type cancellation struct {
		server *server
		token  json.RawMessage
	}
	var requests []cancellation
	l.mu.Lock()
	l.statusMu.Lock()
	for _, s := range l.servers {
		if s == nil || s.ended.Load() || l.statusActive[s.lang] != s.generation {
			continue
		}
		for _, p := range l.progressTokens[s.generation] {
			if p.Begun && p.Cancellable {
				requests = append(requests, cancellation{s, p.Token})
			}
		}
	}
	l.statusMu.Unlock()
	l.mu.Unlock()
	for _, request := range requests {
		request.server.notify("window/workDoneProgress/cancel", map[string]any{"token": request.token})
	}
	return len(requests)
}
