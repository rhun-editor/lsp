package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func TestCommandAdmissionClosesAtExecuteReplyAndWaitsForAdmittedWork(t *testing.T) {
	_, s, p := testServer("Go")
	defer s.stop()
	admitted := make(chan *commandEdit, 1)
	p.onBody = func(body []byte) {
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(body, &msg)
		if msg.Method != "workspace/executeCommand" {
			return
		}
		admitted <- s.admitCommandEdit()
		s.closeCommandReply(msg.ID)
		if s.admitCommandEdit() != nil {
			t.Error("admission remained open after execute reply")
		}
		s.pmu.Lock()
		ch := s.pending[msg.ID]
		s.pmu.Unlock()
		ch <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":null}`)
	}
	done := make(chan error, 1)
	go func() { done <- s.executeCaptured(context.Background(), lspCommand{Command: "test"}, nil) }()
	work := <-admitted
	if work == nil {
		t.Fatal("edit not admitted")
	}
	select {
	case err := <-done:
		t.Fatal("completed before admitted work", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(work.done)
	s.commandMu.Lock()
	work.capture.pending--
	s.commandMu.Unlock()
	work.capture.work.Done()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.admitCommandEdit() != nil {
		t.Fatal("retired capture accepted an edit")
	}
}

func TestCommandEditsPreserveReaderOrderWhenGoroutinesStartInReverse(t *testing.T) {
	_, s, p := testServer("Go")
	defer s.stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.commandCapture = &commandCapture{ctx: ctx, cancel: cancel, active: true}
	first, second := s.admitCommandEdit(), s.admitCommandEdit()
	go s.applyCommandEdit(second, json.RawMessage(`"second"`), json.RawMessage(`bad`))
	go func() {
		time.Sleep(30 * time.Millisecond)
		s.applyCommandEdit(first, json.RawMessage(`"first"`), json.RawMessage(`bad`))
	}()
	s.commandCapture.work.Wait()
	msgs := p.messages(t)
	if len(msgs) != 2 || msgs[0]["id"] != "first" || msgs[1]["id"] != "second" {
		t.Fatal(msgs)
	}
	for _, msg := range msgs {
		if msg["result"].(map[string]any)["applied"] != false {
			t.Fatal(msg)
		}
	}
}

func TestCommandGateSurvivesGenerationAndHonorsCancellation(t *testing.T) {
	l, s, _ := testServer("Go")
	defer s.stop()
	gate := l.commandGate("Go")
	if err := acquireCommand(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := acquireCommand(ctx, gate); err == nil {
		t.Fatal("cancelled waiter admitted")
	}
	l.mu.Lock()
	delete(l.servers, "Go")
	l.mu.Unlock()
	if l.commandGate("Go") != gate {
		t.Fatal("gate replaced with generation")
	}
	<-gate
}

func TestSourceResolveCapabilityHonorsStaticAndDynamicOptions(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	doc := selectorDocument{language: "go", scheme: "file", path: "/project/main.go"}
	for _, enabled := range []string{"false", "true"} {
		raw := json.RawMessage(`{"capabilities":{"codeActionProvider":{"resolveProvider":` + enabled + `}}}`)
		if err := s.loadCapabilities(raw); err != nil {
			t.Fatal(err)
		}
		if s.supports("codeAction/resolve", doc) != (enabled == "true") {
			t.Fatal(enabled)
		}
	}
	s.capMu.Lock()
	s.staticResolve = false
	s.capMu.Unlock()
	raw := json.RawMessage(`{"registrations":[{"id":"source","method":"textDocument/codeAction","registerOptions":{"documentSelector":[{"language":"go"}],"resolveProvider":true}}]}`)
	if _, code, why := s.registerCapabilities(raw); code != 0 {
		t.Fatal(code, why)
	}
	if !s.supports("codeAction/resolve", doc) || s.supports("codeAction/resolve", selectorDocument{language: "rust"}) {
		t.Fatal("resolve selector ignored")
	}
	if code := unregisterForTest(s, "source", "textDocument/codeAction"); code != 0 {
		t.Fatal(code)
	}
	if s.supports("codeAction/resolve", doc) {
		t.Fatal("resolve survived unregister")
	}
	bad := strings.Replace(string(raw), `"resolveProvider":true`, `"resolveProvider":"true"`, 1)
	if _, code, _ := s.registerCapabilities(json.RawMessage(bad)); code != -32602 {
		t.Fatal("accepted invalid resolveProvider", code)
	}
}

func TestCommandSnapshotsPredictStableEditsWithoutCreditingOtherDocuments(t *testing.T) {
	q := &documentQuery{id: 1, path: "/project/main.go", version: 10, text: []byte("a😀b\n")}
	other := &documentQuery{id: 2, path: "/project/other.go", version: 20, text: []byte("other")}
	edit := rhunext.WorkspaceEdit{Files: []rhunext.FileEdit{{Target: rhunext.Target{Path: q.path}, Version: 10, Edits: []rhunext.TextEdit{
		{Range: rhunext.Range{Start: rhunext.Pos{Line: 1, Col: 2}, End: rhunext.Pos{Line: 1, Col: 6}}, Text: "R"},
		{Range: rhunext.Range{Start: rhunext.Pos{Line: 1, Col: 2}, End: rhunext.Pos{Line: 1, Col: 2}}, Text: "I"},
		{Range: rhunext.Range{Start: rhunext.Pos{Line: 1, Col: 2}, End: rhunext.Pos{Line: 1, Col: 2}}, Text: "J"},
	}}}}
	next, err := commandEditSnapshots(edit, []*documentQuery{q, other})
	if err != nil || string(next[0].text) != "aIJRb\n" || next[0].version != 14 || next[1] != other || q.version != 10 || string(q.text) != "a😀b\n" {
		t.Fatal(next, err)
	}
	edit.Files[0].Version = 11
	if _, err := commandEditSnapshots(edit, []*documentQuery{q, other}); err == nil {
		t.Fatal("changed version accepted")
	}
	edit.Files[0].Version = 10
	edit.Files[0].Edits = append(edit.Files[0].Edits, edit.Files[0].Edits[0])
	if _, err := commandEditSnapshots(edit, []*documentQuery{q, other}); err == nil {
		t.Fatal("overlap accepted")
	}
}

func TestOriginalActionTargetVersionsSurviveSelection(t *testing.T) {
	targets := []actionTarget{{Doc: 2, Path: "/project/./other.go", Version: 10, LSPVersion: 3}}
	q := &documentQuery{id: 2, path: "/project/other.go", version: 10, lspVersion: 9}
	if err := retainActionTargetVersions([]*documentQuery{q}, targets); err != nil || q.lspVersion != 3 {
		t.Fatal(q, err)
	}
	q.version = 11
	if err := retainActionTargetVersions([]*documentQuery{q}, targets); err == nil {
		t.Fatal("changed target accepted")
	}
	q.version = 10
	q.id = 3
	if err := retainActionTargetVersions([]*documentQuery{q}, targets); err == nil {
		t.Fatal("reopened target accepted")
	}
}
