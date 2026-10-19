package main

import (
	"encoding/json"
	"testing"
)

func registerSync(t *testing.T, s *server, id, method string, options any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": id, "method": method, "registerOptions": options}}})
	if _, code, msg := s.registerCapabilities(raw); code != 0 {
		t.Fatal(code, msg)
	}
}
func TestDynamicSynchronizationEnablesExistingDocumentAndReenablesFullText(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{})
	l.ensureSynchronized(d, s)
	registerSync(t, s, "open", "textDocument/didOpen", map[string]any{"documentSelector": []any{map[string]any{"language": "go"}}})
	registerSync(t, s, "change", "textDocument/didChange", map[string]any{"syncKind": 2})
	l.documentEvent(docEvent("edits", 2, "1 0 1\nx"))
	if code := unregisterForTest(s, "change", "textDocument/didChange"); code != 0 {
		t.Fatal(code)
	}
	l.documentEvent(docEvent("edits", 3, "2 0 1\ny"))
	registerSync(t, s, "change2", "textDocument/didChange", map[string]any{"syncKind": 2})
	messages := p.messages(t)
	opens := 0
	for _, m := range messages {
		if m["method"] == "textDocument/didOpen" {
			opens++
		}
	}
	if opens != 1 {
		t.Fatal(messages)
	}
	last := messages[len(messages)-1]
	change := last["params"].(map[string]any)["contentChanges"].([]any)[0].(map[string]any)
	if last["method"] != "textDocument/didChange" || change["text"] != "axy" || change["range"] != nil {
		t.Fatal(messages)
	}
}
func TestDynamicSaveSelectorAndIndependentClose(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{})
	l.ensureSynchronized(d, s)
	registerSync(t, s, "wrong", "textDocument/didOpen", map[string]any{"documentSelector": []any{map[string]any{"language": "rust"}}})
	registerSync(t, s, "save", "textDocument/didSave", map[string]any{"includeText": true})
	registerSync(t, s, "close", "textDocument/didClose", nil)
	l.documentEvent(docEvent("saved", 1, ""))
	l.documentEvent(docEvent("closed", 0, ""))
	messages := p.messages(t)
	if len(messages) != 2 || messages[0]["method"] != "textDocument/didSave" || messages[0]["params"].(map[string]any)["text"] != "a" || messages[1]["method"] != "textDocument/didClose" {
		t.Fatal(messages)
	}
}
func TestDynamicSynchronizationValidationAndStaticUnion(t *testing.T) {
	_, s, _, _ := syncHarness(t, synchronizationOptions{openClose: true, change: 1, save: true})
	for _, options := range []any{nil, map[string]any{}, map[string]any{"syncKind": 0}, map[string]any{"syncKind": 3}, map[string]any{"syncKind": nil}} {
		raw, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": "bad", "method": "textDocument/didChange", "registerOptions": options}}})
		if _, code, _ := s.registerCapabilities(raw); code != -32602 {
			t.Fatal(options, code)
		}
	}
	registerSync(t, s, "change", "textDocument/didChange", map[string]any{"syncKind": 2})
	options, close, _ := s.synchronizationFor(selectorDocument{})
	if options.change != 1 || !options.openClose || !close || !options.save {
		t.Fatal(options, close)
	}
	if code := unregisterForTest(s, "change", "textDocument/didChange"); code != 0 {
		t.Fatal(code)
	}
	options, _, _ = s.synchronizationFor(selectorDocument{})
	if options.change != 1 {
		t.Fatal(options)
	}
}

func TestSynchronizationRecoversAnUnsentIncrementalBase(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	if !l.ensureSynchronized(d, s) {
		t.Fatal("initial sync")
	}
	s.writeGate <- struct{}{}
	l.documentEvent(docEvent("edits", 2, "1 0 1\nx"))
	<-s.writeGate
	if s.ended.Load() {
		t.Fatal("pre-write timeout retired server")
	}
	l.documentEvent(docEvent("edits", 3, "2 0 1\ny"))
	messages := p.messages(t)
	if len(messages) != 2 {
		t.Fatal(messages)
	}
	change := messages[1]["params"].(map[string]any)["contentChanges"].([]any)[0].(map[string]any)
	if change["text"] != "axy" || change["range"] != nil {
		t.Fatal("missed incremental base was not replaced", messages)
	}
}
func TestSynchronizationUsesActualBaseAcrossCoalescedVersions(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	l.ensureSynchronized(d, s)
	l.documentEvent(docEvent("edits", 3, "1 0 1\nx2 0 1\ny"))
	messages := p.messages(t)
	changes := messages[1]["params"].(map[string]any)["contentChanges"].([]any)
	if len(changes) != 2 || changes[0].(map[string]any)["range"] == nil || changes[1].(map[string]any)["range"] == nil {
		t.Fatal(messages)
	}
}
