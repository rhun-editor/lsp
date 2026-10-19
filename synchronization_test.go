package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func syncHarness(t *testing.T, options synchronizationOptions) (*lsp, *server, *document, *recordingPipe) {
	t.Helper()
	l, s, p := testServer("Go")
	d := &document{id: 1, version: 1, lang: "Go", path: "/project/main.go", text: []byte("a"), server: s}
	l.docs = map[rhunext.Doc]*document{1: d}
	l.byPath = map[string]*document{d.path: d}
	s.syncOptions = options
	s.staticCapabilities = map[string]bool{"textDocument/hover": true}
	s.ready.Store(true)
	t.Cleanup(s.stop)
	return l, s, d, p
}
func docEvent(name string, version int64, body string) rhunext.Event {
	return rhunext.Event{Name: name, Args: []string{"1", fmt.Sprint(version)}, Body: []byte(body)}
}
func currentVersion(l *lsp, d *document) int64 { l.mu.Lock(); defer l.mu.Unlock(); return d.version }

func TestStaticSynchronizationOptions(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want synchronizationOptions
	}{
		{`0`, synchronizationOptions{}}, {`1`, synchronizationOptions{openClose: true, change: 1, save: true}},
		{`2`, synchronizationOptions{openClose: true, change: 2, save: true}},
		{`{}`, synchronizationOptions{}}, {`null`, synchronizationOptions{}},
		{`{"openClose":true,"change":2,"save":{"includeText":true}}`, synchronizationOptions{openClose: true, change: 2, save: true, includeText: true}},
		{`{"change":1,"save":false}`, synchronizationOptions{change: 1}},
	} {
		_, s, _ := testServer("Go")
		if err := s.loadSynchronization(json.RawMessage(`{"capabilities":{"textDocumentSync":` + tc.raw + `}}`)); err != nil || s.syncOptions != tc.want {
			t.Fatal(tc.raw, s.syncOptions, err)
		}
	}
	for _, raw := range []string{`3`, `true`, `[]`, `{"openClose":null}`, `{"change":null}`, `{"save":null}`, `{"save":{"includeText":null}}`} {
		_, s, _ := testServer("Go")
		if err := s.loadSynchronization(json.RawMessage(`{"capabilities":{"textDocumentSync":` + raw + `}}`)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	_, s, _ := testServer("Go")
	if err := s.loadSynchronization(json.RawMessage(`{"capabilities":{"positionEncoding":"utf-8"}}`)); err == nil {
		t.Fatal("unsupported position encoding")
	}
}
func TestSynchronizationModesAndSaveText(t *testing.T) {
	for _, mode := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: mode, save: true, includeText: true})
			if !l.ensureSynchronized(d, s) {
				t.Fatal("initial synchronization failed")
			}
			l.documentEvent(docEvent("edits", 2, "1 0 2\nβ"))
			l.documentEvent(docEvent("saved", 2, ""))
			messages := p.messages(t)
			if messages[0]["method"] != "textDocument/didOpen" {
				t.Fatal(messages)
			}
			if mode != 0 {
				change := messages[1]["params"].(map[string]any)["contentChanges"].([]any)[0].(map[string]any)
				if mode == 1 {
					if change["text"] != "aβ" || change["range"] != nil {
						t.Fatal(change)
					}
				} else if change["text"] != "β" || change["range"] == nil {
					t.Fatal(change)
				}
			}
			save := messages[len(messages)-1]
			if save["method"] != "textDocument/didSave" || save["params"].(map[string]any)["text"] != "aβ" {
				t.Fatal(messages)
			}
			l.documentEvent(docEvent("closed", 0, ""))
			messages = p.messages(t)
			if messages[len(messages)-1]["method"] != "textDocument/didClose" {
				t.Fatal(messages)
			}
		})
	}
}
func TestNoOpenCloseOrSaveNotificationsWhenDisabled(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{})
	l.ensureSynchronized(d, s)
	l.documentEvent(docEvent("edits", 2, "1 0 1\nx"))
	l.documentEvent(docEvent("saved", 2, ""))
	l.documentEvent(docEvent("closed", 0, ""))
	if len(p.messages(t)) != 0 {
		t.Fatal(p.messages(t))
	}
}
func TestDelayedInitializationOpensLatestTextOnce(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	s.ready.Store(false)
	l.awaitServer(d, s)
	l.documentEvent(docEvent("edits", 2, "1 0 1\nx"))
	if len(p.messages(t)) != 0 {
		t.Fatal("sent before ready")
	}
	s.ready.Store(true)
	close(s.readyCh)
	eventually(t, func() bool { return len(p.messages(t)) == 1 })
	l.ensureSynchronized(d, s)
	messages := p.messages(t)
	if len(messages) != 1 || messages[0]["params"].(map[string]any)["textDocument"].(map[string]any)["text"] != "ax" {
		t.Fatal(messages)
	}
}
func TestCloseBeforeInitializeCannotOpenLater(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	s.ready.Store(false)
	l.awaitServer(d, s)
	l.documentEvent(docEvent("closed", 0, ""))
	s.ready.Store(true)
	close(s.readyCh)
	eventually(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return d.awaiting == nil })
	if len(p.messages(t)) != 0 {
		t.Fatal(p.messages(t))
	}
}
func TestRefreshSaveWaitsForLatestSynchronizedText(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2, save: true, includeText: true})
	l.ensureSynchronized(d, s)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	l.fetchText = func(rhunext.Doc) ([]byte, int64, error) {
		entered <- struct{}{}
		<-release
		return []byte("latest"), 3, nil
	}
	l.documentEvent(docEvent("resync", 2, ""))
	<-entered
	l.documentEvent(docEvent("saved", 2, ""))
	l.documentEvent(docEvent("edits", 3, "0 0 1\nx"))
	if len(p.messages(t)) != 1 {
		t.Fatal("save overtook refresh")
	}
	close(release)
	eventually(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return !d.pendingSave && d.syncedVersion == 3 })
	messages := p.messages(t)
	if messages[1]["method"] != "textDocument/didChange" || messages[2]["method"] != "textDocument/didSave" {
		t.Fatal(messages)
	}
	change := messages[1]["params"].(map[string]any)["contentChanges"].([]any)[0].(map[string]any)
	if change["text"] != "latest" || messages[2]["params"].(map[string]any)["text"] != "latest" {
		t.Fatal(messages)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if d.pendingSave {
		t.Fatal("save stayed pending")
	}
}
func TestStaleRefreshRetriesWithoutOverwritingNewerVersion(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	l.ensureSynchronized(d, s)
	var calls atomic.Int32
	l.fetchText = func(rhunext.Doc) ([]byte, int64, error) {
		if calls.Add(1) == 1 {
			return []byte("old"), 1, nil
		}
		return []byte("fresh"), 2, nil
	}
	l.documentEvent(docEvent("resync", 2, ""))
	eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return !d.fetching && string(d.text) == "fresh" && d.syncedVersion == 2
	})
	if calls.Load() != 2 || len(p.messages(t)) != 2 {
		t.Fatal(calls.Load(), p.messages(t))
	}
}
func TestLateFetchCannotReopenClosedDocument(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	l.ensureSynchronized(d, s)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	l.fetchText = func(rhunext.Doc) ([]byte, int64, error) {
		entered <- struct{}{}
		<-release
		return []byte("late"), 2, nil
	}
	l.documentEvent(docEvent("resync", 2, ""))
	<-entered
	l.documentEvent(docEvent("closed", 0, ""))
	close(release)
	eventually(t, func() bool { l.mu.Lock(); defer l.mu.Unlock(); return !d.fetching })
	messages := p.messages(t)
	if len(messages) != 2 || messages[1]["method"] != "textDocument/didClose" {
		t.Fatal(messages)
	}
}
func TestQueuedQueryRejectsInterveningDocumentChange(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	l.ensureSynchronized(d, s)
	q, _, _ := l.current(1, 1)
	if q == nil {
		t.Fatal("missing query snapshot")
	}
	s.writeGate <- struct{}{}
	answer := make(chan error, 1)
	go func() {
		_, err := q.request(time.Second, "textDocument/hover", map[string]any{"position": lspPos(q.text, 1)})
		answer <- err
	}()
	eventually(t, func() bool { return pendingCount(s) == 1 })
	done := make(chan struct{})
	go func() { l.documentEvent(docEvent("edits", 2, "1 0 1\nx")); close(done) }()
	eventually(t, func() bool { return currentVersion(l, d) == 2 })
	<-s.writeGate
	if err := <-answer; err == nil || !strings.Contains(err.Error(), "document changed") {
		t.Fatal(err)
	}
	<-done
	for _, message := range p.messages(t) {
		if message["method"] == "textDocument/hover" {
			t.Fatal("stale query transmitted")
		}
	}
	if string(q.text) != "a" {
		t.Fatal("query snapshot mutated")
	}
}
func TestRetiredDocumentSendCannotSynchronizeReplacement(t *testing.T) {
	l, old, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	replacement := newServer(l, "Go", []string{"replacement"})
	replacement.in = &recordingPipe{}
	replacement.syncOptions = old.syncOptions
	replacement.ready.Store(true)
	t.Cleanup(replacement.stop)
	p.onBody = func([]byte) { l.mu.Lock(); d.server = replacement; l.mu.Unlock() }
	if l.ensureSynchronized(d, old) {
		t.Fatal("retired association became synchronized")
	}
	if !l.ensureSynchronized(d, replacement) {
		t.Fatal("replacement failed")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if d.syncedServer != replacement {
		t.Fatal("old send replaced new state")
	}
}
