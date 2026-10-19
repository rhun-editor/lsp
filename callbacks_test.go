package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingPipe struct {
	mu     sync.Mutex
	data   bytes.Buffer
	onBody func([]byte)
}

func (p *recordingPipe) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.data.Write(data)
	hook := p.onBody
	p.mu.Unlock()
	if len(data) > 0 && data[0] == '{' && hook != nil {
		hook(data)
	}
	return len(data), nil
}
func (p *recordingPipe) Close() error { return nil }
func (p *recordingPipe) messages(t *testing.T) []map[string]any {
	t.Helper()
	p.mu.Lock()
	text := p.data.String()
	p.mu.Unlock()
	reader := bufio.NewReader(strings.NewReader(text))
	var messages []map[string]any
	for {
		header, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var size int
		if _, err := fmt.Sscanf(header, "Content-Length: %d", &size); err != nil {
			t.Fatal(err)
		}
		reader.ReadString('\n')
		body := make([]byte, size)
		if _, err := io.ReadFull(reader, body); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			} // writer may still be appending the current frame
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(body, &msg); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, msg)
	}
	return messages
}

func testServer(language string) (*lsp, *server, *recordingPipe) {
	l := &lsp{settings: &settingsConfig{}, servers: map[string]*server{}}
	s := newServer(l, language, []string{"fake"})
	l.servers[language] = s
	pipe := &recordingPipe{}
	s.in = pipe
	return l, s, pipe
}

func TestConfigurationCallbacksAndScopes(t *testing.T) {
	l, s, pipe := testServer("Go")
	l.project = t.TempDir()
	l.settings = &settingsConfig{Defaults: languageSettings{Settings: map[string]any{"gopls": map[string]any{"hints": true}}}}
	body, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"section": "gopls.hints"},
		map[string]any{"section": "gopls", "scopeUri": fileURI(filepath.Join(l.project, "a.go"))},
		map[string]any{"section": "gopls", "scopeUri": "https://example.org/a.go"},
		map[string]any{"section": "missing"},
	}})
	s.serverRequest(json.RawMessage(`"request-id"`), "workspace/configuration", body)
	messages := pipe.messages(t)
	result := messages[0]["result"].([]any)
	if result[0] != true || result[1].(map[string]any)["hints"] != true || result[2] != nil || result[3] != nil {
		t.Fatal(result)
	}
	if messages[0]["id"] != "request-id" {
		t.Fatal(messages)
	}
	if _, code, _ := s.requestResult("not/implemented", nil); code != -32601 {
		t.Fatal(code)
	}
	if _, code, _ := s.requestResult("workspace/configuration", json.RawMessage(`{"items":null}`)); code != -32602 {
		t.Fatal(code)
	}
	l.project = ""
	folders, code, _ := s.requestResult("workspace/workspaceFolders", nil)
	if code != 0 || folders != nil {
		t.Fatal(folders, code)
	}
}

func TestReadyHandoffSendsReloadedSettings(t *testing.T) {
	l, s, pipe := testServer("Go")
	l.state = t.TempDir()
	os.WriteFile(filepath.Join(l.state, "settings.json"), []byte(`{"defaults":{"settings":{"new":true}}}`), 0600)
	var once sync.Once
	pipe.onBody = func([]byte) {
		once.Do(func() {
			if err := l.reloadSettings(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := s.publishReady(); err != nil {
		t.Fatal(err)
	}
	messages := pipe.messages(t)
	if len(messages) != 2 || !s.ready.Load() {
		t.Fatal(messages)
	}
	params := messages[1]["params"].(map[string]any)
	if params["settings"].(map[string]any)["new"] != true {
		t.Fatal(params)
	}
}

func TestEndedServerCannotPublishReady(t *testing.T) {
	_, s, pipe := testServer("Go")
	pipe.onBody = func([]byte) { s.endServer("") }
	if err := s.publishReady(); err == nil {
		t.Fatal("ended server became ready")
	}
	if s.ready.Load() {
		t.Fatal("ready after EOF")
	}
}

func TestInvalidReloadPreservesPublishedSettings(t *testing.T) {
	l, _, _ := testServer("Go")
	previous := l.settings
	l.state = t.TempDir()
	os.WriteFile(filepath.Join(l.state, "settings.json"), []byte(`{"bad":true}`), 0600)
	if err := l.reloadSettings(); err == nil {
		t.Fatal("accepted malformed reload")
	}
	if l.settings != previous || l.configGeneration != 0 {
		t.Fatal("changed existing snapshot")
	}
}

func TestInitializeThenEOFReleasesReadyWaiters(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "lsp-initialize-eof" {
		reader := bufio.NewReader(os.Stdin)
		var size int
		header, _ := reader.ReadString('\n')
		fmt.Sscanf(header, "Content-Length: %d", &size)
		reader.ReadString('\n')
		io.ReadFull(reader, make([]byte, size))
		body := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`
		fmt.Printf("Content-Length: %d\r\n\r\n%s", len(body), body)
		os.Exit(0)
	}
	l := &lsp{settings: &settingsConfig{}, servers: map[string]*server{}}
	s := newServer(l, "Go", []string{os.Args[0], "-test.run=^TestInitializeThenEOFReleasesReadyWaiters$", "--", "lsp-initialize-eof"})
	l.servers["Go"] = s
	go s.start()
	select {
	case <-s.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("ready waiter leaked")
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("EOF did not end server")
	}
	if s.ready.Load() {
		t.Fatal("exited server became ready")
	}
	select {
	case <-s.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child was not reaped")
	}
}

func TestProgressAcrossLanguagesAndRetiredGenerations(t *testing.T) {
	l, goServer, _ := testServer("Go")
	rust := newServer(l, "Rust", []string{"rust-analyzer"})
	l.servers["Rust"] = rust
	for _, s := range []*server{goServer, rust} {
		if _, code, _ := s.createProgress(json.RawMessage(`{"token":1}`)); code != 0 {
			t.Fatal(code)
		}
	}
	goServer.progress(json.RawMessage(`{"token":1,"value":{"kind":"begin","title":"Go indexing","cancellable":true}}`))
	rust.progress(json.RawMessage(`{"token":1,"value":{"kind":"begin","title":"Rust indexing"}}`))
	if !strings.Contains(l.statusLatest, "Rust indexing") {
		t.Fatal(l.statusLatest)
	}
	rust.progress(json.RawMessage(`{"token":1,"value":{"kind":"end"}}`))
	if !strings.Contains(l.statusLatest, "Go indexing") {
		t.Fatal(l.statusLatest)
	}
	newer := newServer(l, "Go", []string{"gopls"})
	l.servers["Go"] = newer
	newer.status("new server", false)
	goServer.status("stale server", true)
	if l.statusLatest != "new server" {
		t.Fatal(l.statusLatest)
	}
}

func TestProgressTokenTypesLimitsAndCancellation(t *testing.T) {
	l, s, pipe := testServer("Go")
	for _, raw := range []string{`null`, `true`, `1.5`, `2147483648`, `{}`} {
		if _, _, ok := progressToken(json.RawMessage(raw)); ok {
			t.Fatal(raw)
		}
	}
	for _, params := range []string{`{"token":1}`, `{"token":"1"}`} {
		if _, code, _ := s.createProgress(json.RawMessage(params)); code != 0 {
			t.Fatal(code)
		}
	}
	if _, code, _ := s.createProgress(json.RawMessage(`{"token":1}`)); code != -32602 {
		t.Fatal("duplicate", code)
	}
	s.progress(json.RawMessage(`{"token":1,"value":{"kind":"begin","title":"Indexing","percentage":20,"cancellable":true}}`))
	if !strings.Contains(l.statusLatest, "20%") {
		t.Fatal(l.statusLatest)
	}
	if l.cancelProgress() != 1 {
		t.Fatal("did not cancel current work")
	}
	msg := pipe.messages(t)[0]
	if msg["method"] != "window/workDoneProgress/cancel" || msg["params"].(map[string]any)["token"] != float64(1) {
		t.Fatal(msg)
	}
	for i := 2; i < 64; i++ {
		s.createProgress(json.RawMessage(fmt.Sprintf(`{"token":%d}`, i)))
	}
	if _, code, _ := s.createProgress(json.RawMessage(`{"token":100}`)); code != -32602 {
		t.Fatal("unbounded tokens", code)
	}
	s.endServer("")
	if strings.Contains(l.statusLatest, "Indexing") {
		t.Fatal("ended progress stayed visible")
	}
}

func TestConfigurationResponseBudgetReturnsBoundedError(t *testing.T) {
	l, s, pipe := testServer("Go")
	defer s.stop()
	l.settings = &settingsConfig{Defaults: languageSettings{Settings: map[string]any{"large": strings.Repeat("x", 1<<20)}}}
	items := make([]map[string]any, 1024)
	for i := range items {
		items[i] = map[string]any{}
	}
	params, _ := json.Marshal(map[string]any{"items": items})
	s.serverRequest(json.RawMessage(`"settings-budget"`), "workspace/configuration", params)
	messages := pipe.messages(t)
	if len(messages) != 1 || messages[0]["id"] != "settings-budget" {
		t.Fatal(messages)
	}
	responseError, ok := messages[0]["error"].(map[string]any)
	if !ok || responseError["code"] != float64(-32603) {
		t.Fatal(messages)
	}
	pipe.mu.Lock()
	defer pipe.mu.Unlock()
	if pipe.data.Len() > 1024 {
		t.Fatal("oversized error", pipe.data.Len())
	}
}
