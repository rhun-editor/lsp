package main

import (
	"encoding/json"
	"fmt"
)

func isSynchronizationMethod(method string) bool {
	switch method {
	case "textDocument/didOpen", "textDocument/didClose", "textDocument/didChange", "textDocument/didSave":
		return true
	}
	return false
}
func parseSynchronizationRegistration(r *capabilityRegistration, options map[string]json.RawMessage) error {
	if r.method == "textDocument/didChange" {
		raw := options["syncKind"]
		if string(raw) == "null" || json.Unmarshal(raw, &r.syncKind) != nil || r.syncKind < 1 || r.syncKind > 2 {
			return fmt.Errorf("invalid synchronization syncKind")
		}
	}
	if r.method == "textDocument/didSave" {
		if raw, ok := options["includeText"]; ok {
			if string(raw) != "true" && string(raw) != "false" {
				return fmt.Errorf("invalid synchronization includeText")
			}
			r.includeText = string(raw) == "true"
		}
	}
	return nil
}
func (s *server) synchronizationFor(d selectorDocument) (synchronizationOptions, bool, uint64) {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	options := s.syncOptions
	closeInterest := options.openClose
	for _, r := range s.registrations {
		if !r.matches(d) {
			continue
		}
		switch r.method {
		case "textDocument/didOpen":
			options.openClose = true
		case "textDocument/didClose":
			closeInterest = true
		case "textDocument/didChange":
			if options.change == 0 || r.syncKind < options.change {
				options.change = r.syncKind
			}
		case "textDocument/didSave":
			options.save = true
			options.includeText = options.includeText || r.includeText
		}
	}
	return options, closeInterest, s.syncGeneration
}
func (s *server) refreshSynchronization() {
	s.l.mu.Lock()
	var docs []*document
	for _, d := range s.l.docs {
		if d.server == s {
			docs = append(docs, d)
		}
	}
	s.l.mu.Unlock()
	for _, d := range docs {
		s.l.ensureSynchronized(d, s)
	}
}
