package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func registerForTest(s *server, id, method string, selector any) int {
	options := map[string]any{}
	if selector != nil {
		options["documentSelector"] = selector
	}
	params, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": id, "method": method, "registerOptions": options}}})
	_, code, _ := s.registerCapabilities(params)
	return code
}
func unregisterForTest(s *server, id, method string) int {
	params, _ := json.Marshal(map[string]any{"unregisterations": []any{map[string]any{"id": id, "method": method}}})
	_, code, _ := s.unregisterCapabilities(params)
	return code
}
func TestStaticCapabilitiesAndSelectors(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	if err := s.loadCapabilities(json.RawMessage(`{"capabilities":{"hoverProvider":true,"renameProvider":false,"completionProvider":{},"signatureHelpProvider":true}}`)); err != nil {
		t.Fatal(err)
	}
	d := selectorDocument{language: "go", scheme: "file", path: "/project/main.go"}
	if !s.supports("textDocument/hover", d) || !s.supports("textDocument/completion", d) || s.supports("textDocument/rename", d) || s.supports("textDocument/signatureHelp", d) {
		t.Fatal("incorrect static providers")
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"capabilities":null}`, `{"capabilities":[]}`} {
		if err := s.loadCapabilities(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	selector := []any{map[string]any{"language": "go", "scheme": "file", "pattern": "**/*.{go,mod}"}}
	if code := registerForTest(s, "definition", "textDocument/definition", selector); code != 0 {
		t.Fatal(code)
	}
	if !s.supports("textDocument/definition", d) || s.supports("textDocument/definition", selectorDocument{language: "rust", scheme: "file", path: d.path}) {
		t.Fatal("incorrect selector")
	}
	if code := unregisterForTest(s, "definition", "textDocument/definition"); code != 0 || s.supports("textDocument/definition", d) {
		t.Fatal(code)
	}
	if code := registerForTest(s, "hover", "textDocument/hover", nil); code != 0 {
		t.Fatal(code)
	}
	unregisterForTest(s, "hover", "textDocument/hover")
	if !s.supports("textDocument/hover", d) {
		t.Fatal("lost static provider")
	}
}
func TestSelectorsEmptyAndRelative(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	d := selectorDocument{language: "go", scheme: "file", path: "/project/main.go"}
	if code := registerForTest(s, "empty", "textDocument/definition", []any{}); code != 0 || s.supports("textDocument/definition", d) {
		t.Fatal(code)
	}
	if code := registerForTest(s, "bad", "textDocument/definition", []any{map[string]any{}}); code != -32602 {
		t.Fatal(code)
	}
	// Use native paths for URI/base handling on each platform.
	base := t.TempDir()
	path := base + "/sub/main.go"
	selector := []any{map[string]any{"pattern": map[string]any{"baseUri": map[string]any{"uri": fileURI(base), "name": "project"}, "pattern": "**/*.go"}}}
	if code := registerForTest(s, "relative", "textDocument/definition", selector); code != 0 {
		t.Fatal(code)
	}
	if !s.supports("textDocument/definition", selectorDocument{path: strings.ReplaceAll(path, "\\", "/")}) || s.supports("textDocument/definition", d) {
		t.Fatal("relative base mismatch")
	}
}
func TestCapabilityBatchRollbackAndLimits(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	for _, params := range []string{
		`{"registrations":[{"id":"ok","method":"textDocument/hover"},{"id":"bad","method":"server/unsupported"}]}`,
		`{"registrations":[{"id":"ok","method":"textDocument/hover"},{"id":"bad","method":"textDocument/hover","registerOptions":{"documentSelector":[{}]}}]}`,
		`{"registrations":[{"id":"same","method":"textDocument/hover"},{"id":"same","method":"textDocument/hover"}]}`,
		`{"registrations":null}`,
	} {
		if _, code, _ := s.registerCapabilities(json.RawMessage(params)); code != -32602 || len(s.registrations) != 0 {
			t.Fatal(params, code, s.registrations)
		}
	}
	for i := 0; i < 256; i++ {
		if code := registerForTest(s, fmt.Sprint(i), "textDocument/hover", nil); code != 0 {
			t.Fatal(i, code)
		}
	}
	if code := registerForTest(s, "excess", "textDocument/hover", nil); code != -32602 {
		t.Fatal(code)
	}
	if code := unregisterForTest(s, "0", "textDocument/rename"); code != -32602 || len(s.registrations) != 256 {
		t.Fatal(code)
	}
	if code := unregisterForTest(s, "0", "textDocument/hover"); code != 0 {
		t.Fatal(code)
	}
	if code := registerForTest(s, "excess", "textDocument/hover", nil); code != 0 {
		t.Fatal(code)
	}
}
func TestSelectorValidation(t *testing.T) {
	for _, raw := range []string{`[{}]`, `[{"notebook":false}]`, `[{"language":null}]`, `[{"pattern":"["}]`, `[{"pattern":{"baseUri":"https://example.org","pattern":"**"}}]`, `[{"pattern":{"baseUri":"file:relative","pattern":"**"}}]`, `nullx`} {
		if _, err := parseSelector(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}
func TestQueuedQueryRechecksRemovedCapability(t *testing.T) {
	_, s, p := testServer("Go")
	defer s.stop()
	if code := registerForTest(s, "hover", "textDocument/hover", nil); code != 0 {
		t.Fatal(code)
	}
	s.writeGate <- struct{}{}
	answer := make(chan error, 1)
	go func() {
		_, err := s.requestDocument(time.Second, "textDocument/hover", nil, selectorDocument{})
		answer <- err
	}()
	eventually(t, func() bool { return pendingCount(s) == 1 })
	if code := unregisterForTest(s, "hover", "textDocument/hover"); code != 0 {
		t.Fatal(code)
	}
	<-s.writeGate
	if err := <-answer; !errors.Is(err, errCapabilityUnavailable) {
		t.Fatal(err)
	}
	if len(p.messages(t)) != 0 || pendingCount(s) != 0 || s.ended.Load() {
		t.Fatal("rejected query affected connection")
	}
}
func TestWrittenQueryMayFinishAfterUnregister(t *testing.T) {
	_, s, p := testServer("Go")
	defer s.stop()
	registerForTest(s, "hover", "textDocument/hover", nil)
	p.onBody = func([]byte) {
		unregisterForTest(s, "hover", "textDocument/hover")
		go s.read(bufio.NewReader(strings.NewReader(rpcFrame(`{"jsonrpc":"2.0","id":1,"result":"finished"}`))))
	}
	res, err := s.requestDocument(time.Second, "textDocument/hover", nil, selectorDocument{})
	if err != nil || string(res) != `"finished"` {
		t.Fatal(string(res), err)
	}
}
func TestRegistryConcurrentReadsAndUpdates(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				id := fmt.Sprint(worker)
				registerForTest(s, id, "textDocument/hover", nil)
				s.supports("textDocument/hover", selectorDocument{})
				unregisterForTest(s, id, "textDocument/hover")
			}
		}(worker)
	}
	wg.Wait()
}

func TestRegistrationOptionsBudgetAndSelectorBounds(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	params, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{
		"id": "oversized", "method": "textDocument/hover", "registerOptions": map[string]any{"padding": strings.Repeat("x", 1<<20)},
	}}})
	if _, code, _ := s.registerCapabilities(params); code != -32602 || len(s.registrations) != 0 {
		t.Fatal(code)
	}
	params, _ = json.Marshal(map[string]any{"registrations": []any{
		map[string]any{"id": "one", "method": "textDocument/hover", "registerOptions": map[string]any{"padding": strings.Repeat("x", 600000)}},
		map[string]any{"id": "two", "method": "textDocument/hover", "registerOptions": map[string]any{"padding": strings.Repeat("x", 600000)}},
	}})
	if _, code, _ := s.registerCapabilities(params); code != -32602 || len(s.registrations) != 0 {
		t.Fatal(code)
	}
	for _, selector := range []any{[]any{strings.Repeat("x", 257)}, []any{map[string]any{"scheme": strings.Repeat("x", 257)}}} {
		if code := registerForTest(s, "invalid", "textDocument/hover", selector); code != -32602 {
			t.Fatal(code)
		}
	}
}

func TestNotebookFiltersDoNotDisableFileRegistrations(t *testing.T) {
	filters, err := parseSelector(json.RawMessage(`[{"scheme":"file","language":"python"},{"notebook":{"scheme":"file"},"language":"python"}]`))
	if err != nil {
		t.Fatal(err)
	}
	r := capabilityRegistration{selector: filters}
	if !r.matches(selectorDocument{scheme: "file", language: "python"}) || r.matches(selectorDocument{scheme: "file", language: "go"}) {
		t.Fatal("mixed selector widened or lost file scope")
	}
	r.selector = filters[1:]
	if r.matches(selectorDocument{scheme: "file", language: "python"}) {
		t.Fatal("notebook filter matched a regular file")
	}
}

func TestDartRegistrationKeepsSupportedBatchServices(t *testing.T) {
	s := &server{registrations: map[string]capabilityRegistration{}, l: &lsp{}}
	raw := json.RawMessage(`{"registrations":[{"id":"symbols","method":"textDocument/documentSymbol","registerOptions":{"documentSelector":[{"language":"dart"}]}},{"id":"will-rename","method":"workspace/willRenameFiles","registerOptions":{"filters":[{"pattern":{"glob":"**/*.dart","matches":"file"}}]}}]}`)
	if _, code, message := s.registerCapabilities(raw); code != 0 {
		t.Fatal(code, message)
	}
	if !s.supports("textDocument/documentSymbol", selectorDocument{language: "dart", scheme: "file"}) {
		t.Fatal("Dart file features were rejected with willRenameFiles")
	}
	filters, err := parseSelector(json.RawMessage(`[{"language":"ocaml","scheme":null,"pattern":null}]`))
	if err != nil || !(capabilityRegistration{selector: filters}).matches(selectorDocument{language: "ocaml", scheme: "file"}) {
		t.Fatal("OCaml nullable selector", err)
	}
}
