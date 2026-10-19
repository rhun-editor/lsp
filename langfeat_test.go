package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

var allFeatures = []string{rhunext.FeatureDefinition, rhunext.FeatureReferences, rhunext.FeatureActions,
	rhunext.FeatureRename, rhunext.FeatureFormat, rhunext.FeatureQueryStream, rhunext.FeatureProviderScope,
	rhunext.FeatureNavigation, rhunext.FeaturePrepareRename, rhunext.FeaturePartialFormat, rhunext.FeatureCodeActions}

const uri = "file:///project/main.go"

// "é" is two bytes and one UTF-16 unit: positions after it differ between rhun and LSP
const sample = "package main\n\nfunc é() { total := 1 }\n"

func TestClientCapabilitiesFollowTheAgreedFeatures(t *testing.T) {
	bare := (&lsp{}).clientCapabilities(false)
	text := bare["textDocument"].(map[string]any)
	for _, name := range []string{"declaration", "typeDefinition", "implementation", "rangeFormatting", "onTypeFormatting", "selectionRange", "documentLink", "diagnostic"} {
		if _, ok := text[name]; ok {
			t.Fatalf("%s offered without its feature", name)
		}
	}
	if text["rename"].(map[string]any)["prepareSupport"] != false || text["publishDiagnostics"].(map[string]any)["relatedInformation"] != nil {
		t.Fatal(text)
	}
	h := newHarness(t, append(allFeatures, rhunext.FeatureFileDiagnostics), `{}`, sample, nil)
	caps := h.l.clientCapabilities(true)
	text = caps["textDocument"].(map[string]any)
	for _, name := range []string{"declaration", "typeDefinition", "implementation"} {
		o := text[name].(map[string]any)
		if o["linkSupport"] != true || o["dynamicRegistration"] != true {
			t.Fatal(name, o)
		}
	}
	if r := text["rename"].(map[string]any); r["prepareSupport"] != true || r["prepareSupportDefaultBehavior"] != 1 {
		t.Fatal(r)
	}
	if text["rangeFormatting"] == nil || text["onTypeFormatting"] == nil || text["diagnostic"] == nil {
		t.Fatal(text)
	}
	diag := text["publishDiagnostics"].(map[string]any)
	if diag["relatedInformation"] != true || diag["codeDescriptionSupport"] != true || diag["dataSupport"] != true || diag["tagSupport"] == nil {
		t.Fatal(diag)
	}
	ca := text["codeAction"].(map[string]any)
	if ca["isPreferredSupport"] != true || ca["disabledSupport"] != true || ca["dataSupport"] != true || ca["resolveSupport"] == nil || ca["codeActionLiteralSupport"] == nil {
		t.Fatal(ca)
	}
	if enc := caps["general"].(map[string]any)["positionEncodings"].([]string); !slices.Equal(enc, []string{"utf-16"}) {
		t.Fatal(enc)
	}
	if caps["window"].(map[string]any)["workDoneProgress"] != true || caps["workspace"].(map[string]any)["didChangeWatchedFiles"] == nil {
		t.Fatal(caps)
	}
}

func TestDependentRequestsFollowTheirProviderOptions(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	doc := selectorDocument{language: "go", scheme: "file", path: "/project/main.go"}
	if err := s.loadCapabilities(json.RawMessage(`{"capabilities":{"renameProvider":true,"documentOnTypeFormattingProvider":{"firstTriggerCharacter":"\n","moreTriggerCharacter":["}",";"]},"declarationProvider":true,"callHierarchyProvider":{},"documentLinkProvider":true}}`)); err != nil {
		t.Fatal(err)
	}
	if !s.supports("textDocument/rename", doc) || s.supports("textDocument/prepareRename", doc) {
		t.Fatal("prepareRename without prepareProvider")
	}
	if !s.supports("textDocument/declaration", doc) || !s.supports("callHierarchy/incomingCalls", doc) || s.supports("textDocument/documentLink", doc) {
		t.Fatal("static providers")
	}
	if got := s.onTypeTriggers(doc); !slices.Equal(got, []string{"\n", "}", ";"}) {
		t.Fatal(got)
	}
	s.loadCapabilities(json.RawMessage(`{"capabilities":{"renameProvider":{"prepareProvider":true}}}`))
	if !s.supports("textDocument/prepareRename", doc) || len(s.onTypeTriggers(doc)) != 0 {
		t.Fatal("static prepareProvider")
	}
	raw := json.RawMessage(`{"registrations":[{"id":"r","method":"textDocument/rename","registerOptions":{"documentSelector":[{"language":"go"}],"prepareProvider":true}},` +
		`{"id":"t","method":"textDocument/onTypeFormatting","registerOptions":{"documentSelector":[{"language":"go"}],"firstTriggerCharacter":"=","moreTriggerCharacter":["."]}},` +
		`{"id":"w","method":"workspace/symbol","registerOptions":{"resolveProvider":true}}]}`)
	s.loadCapabilities(json.RawMessage(`{"capabilities":{}}`))
	if _, code, why := s.registerCapabilities(raw); code != 0 {
		t.Fatal(code, why)
	}
	if !s.supports("textDocument/prepareRename", doc) || !s.supports("workspaceSymbol/resolve", doc) {
		t.Fatal("registered options")
	}
	if got := s.onTypeTriggers(doc); !slices.Equal(got, []string{"=", "."}) {
		t.Fatal(got)
	}
	if got := s.onTypeTriggers(selectorDocument{language: "rust"}); len(got) != 0 {
		t.Fatal(got)
	}
	bad := json.RawMessage(`{"registrations":[{"id":"x","method":"textDocument/rename","registerOptions":{"prepareProvider":"yes"}}]}`)
	if _, code, _ := s.registerCapabilities(bad); code != -32602 {
		t.Fatal("accepted a bad prepareProvider")
	}
}

func TestManifestMatchesTheScopeTables(t *testing.T) {
	data, err := os.ReadFile("extension.ini")
	if err != nil {
		t.Fatal(err)
	}
	provides := map[string]string{}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if k, v, ok := strings.Cut(line, " = "); ok && section == "[provides]" {
			provides[k] = v
		}
	}
	want := strings.Join(manifestPatterns, " ")
	for _, k := range scopeKeys {
		if provides[k.key] != want {
			t.Fatalf("%s = %q", k.key, provides[k.key])
		}
	}
	if provides["type_format.triggers"] != strings.Join(manifestTypeTriggers, " ") {
		t.Fatal(provides["type_format.triggers"])
	}
	for _, p := range manifestPatterns {
		found := false
		for _, ps := range languagePatterns {
			found = found || slices.Contains(ps, p)
		}
		if !found {
			t.Fatal("no language for", p)
		}
	}
	for lang := range servers {
		if languagePatterns[lang] == nil {
			t.Fatal("no patterns for", lang)
		}
	}
}

func TestScopeFollowsTheServers(t *testing.T) {
	h := newHarness(t, allFeatures, `{"definitionProvider":true,"documentOnTypeFormattingProvider":{"firstTriggerCharacter":"\n","moreTriggerCharacter":["}","@"]}}`, sample, nil)
	l := h.l
	// every other language: turned off, except C (not started yet) and Rust (gone)
	for lang := range languagePatterns {
		l.config[lang] = []string{}
	}
	l.config["C"] = []string{"sh"}
	l.servers["Rust"] = nil
	v := l.scopeValues(l.scopeLanguages())
	if v["definition"] != "*.go *.c *.h" || v["references"] != "*.c *.h" || v["rename"] != "*.c *.h" {
		t.Fatal(v)
	}
	if v["type_format"] != "*.go *.c *.h" || v["type_format.triggers"] != "newline }" {
		t.Fatal(v["type_format"], v["type_format.triggers"])
	}
	if _, ok := v["hover"]; ok {
		t.Fatal("a key whose feature rhun did not agree")
	}
	l.config["C"] = []string{}
	v = l.scopeValues(l.scopeLanguages())
	if v["definition"] != "*.go" || v["references"] != "-" || v["type_format.triggers"] != "newline }" {
		t.Fatal(v)
	}
	if code := registerForTest(h.s, "refs", "textDocument/references", []any{map[string]any{"language": "go"}}); code != 0 {
		t.Fatal(code)
	}
	if v = l.scopeValues(l.scopeLanguages()); v["references"] != "*.go" {
		t.Fatal(v["references"])
	}
	// all languages possible again: the manifest's values
	for lang := range languagePatterns {
		l.config[lang] = []string{"sh"}
	}
	delete(l.servers, "Rust")
	l.configGeneration++
	h.s.loadCapabilities(json.RawMessage(`{"capabilities":{"definitionProvider":true}}`))
	delete(l.servers, "Go")
	if v = l.scopeValues(l.scopeLanguages()); v["definition"] != "*" || v["type_format"] != "*" || v["type_format.triggers"] != "-" {
		t.Fatal(v)
	}
}

func TestProvidesIsSentWhenTheScopeChanges(t *testing.T) {
	h := newHarness(t, allFeatures, `{"definitionProvider":true,"referencesProvider":true}`, sample, nil)
	for lang := range languagePatterns {
		h.l.config[lang] = []string{}
	}
	h.l.statusDone = make(chan struct{})
	defer close(h.l.statusDone)
	got := map[string]string{}
	h.host.replyTo = func(r hostRequest) string {
		if r.verb == "provides" {
			h.host.mu.Lock()
			got[r.args[0]] = strings.Join(r.args[1:], " ")
			h.host.mu.Unlock()
		}
		return ""
	}
	h.l.startScopeWorker()
	value := func(key string) string {
		h.host.mu.Lock()
		defer h.host.mu.Unlock()
		return got[key]
	}
	eventually(t, func() bool {
		return value("definition") == "*.go" && value("format") == "-" && value("type_format") == "-" && value("type_format.triggers") == "-"
	})
	if value("hover") != "" || value("complete") != "" {
		t.Fatal("a key whose feature rhun did not agree")
	}
	unregisterForTest(h.s, "none", "x")
	if code := registerForTest(h.s, "fmt", "textDocument/formatting", nil); code != 0 {
		t.Fatal(code)
	}
	eventually(t, func() bool { return value("format") == "*.go" })
}

func TestReferencesStreamPartsAndProgressBeforeTheAnswer(t *testing.T) {
	h := newHarness(t, allFeatures, `{"referencesProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		var p struct {
			Partial string `json:"partialResultToken"`
			Work    string `json:"workDoneToken"`
		}
		json.Unmarshal(params, &p)
		f.notify("$/progress", map[string]any{"token": p.Work, "value": map[string]any{"kind": "begin", "title": "Finding", "percentage": 10}})
		f.notify("$/progress", map[string]any{"token": p.Partial, "value": []any{lspLoc(uri, 2, 11, 2, 16)}})
		f.notify("$/progress", map[string]any{"token": p.Work, "value": map[string]any{"kind": "report", "message": "main.go", "percentage": 50}})
		f.notify("$/progress", map[string]any{"token": p.Work, "value": map[string]any{"kind": "end"}})
		return []any{}, true
	})
	id, answer := h.host.askAsync("references 1 1 3 14 invoked -")
	prog := h.host.request("query-progress")
	if strings.Join(prog.args, " ") != id+" 10 7" || string(prog.body) != "Finding" {
		t.Fatal(prog.args, string(prog.body))
	}
	part := h.host.request("query-part")
	// line 3, UTF-16 11..16 is bytes 12..17 (columns 13..18) after the two-byte é
	if part.args[0] != id || string(part.body) != "1\n"+locRecord("3 13 3 18") {
		t.Fatalf("%q %q", part.args, part.body)
	}
	prog = h.host.request("query-progress")
	if strings.Join(prog.args, " ") != id+" 50 15" || string(prog.body) != "Finding main.go" {
		t.Fatal(prog.args, string(prog.body))
	}
	a := <-answer
	if !a.ok || string(a.body) != "0\n" {
		t.Fatalf("%+v", a)
	}
	ref := h.srv.messages("textDocument/references")[0]["params"].(map[string]any)
	if ref["partialResultToken"] == nil || ref["workDoneToken"] == nil || ref["context"].(map[string]any)["includeDeclaration"] != true {
		t.Fatal(ref)
	}
}

func TestWithoutQueryStreamNoTokensAndPartsJoinTheAnswer(t *testing.T) {
	features := slices.DeleteFunc(slices.Clone(allFeatures), func(f string) bool { return f == rhunext.FeatureQueryStream })
	h := newHarness(t, features, `{"implementationProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return []any{map[string]any{"targetUri": uri, "targetRange": rng(2, 0, 2, 26), "targetSelectionRange": rng(2, 5, 2, 6)}}, true
	})
	a := h.host.ask("implementation 1 1 3 6 invoked -", nil)
	if string(a.body) != "1\n"+locRecord("3 6 3 8") {
		t.Fatalf("%q", a.body)
	}
	p := h.srv.messages("textDocument/implementation")[0]["params"].(map[string]any)
	if p["partialResultToken"] != nil || p["workDoneToken"] != nil {
		t.Fatal(p)
	}
}

func TestCancelledQuerySendsCancelRequest(t *testing.T) {
	h := newHarness(t, allFeatures, `{"declarationProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return nil, false // never answers
	})
	id, answer := h.host.askAsync("declaration 1 1 3 6 invoked -")
	req := h.srv.await("textDocument/declaration")
	h.host.write("0 cancel " + id + "\n")
	cancel := h.srv.await("$/cancelRequest")
	if cancel["params"].(map[string]any)["id"] != req["id"] {
		t.Fatal(cancel, req)
	}
	select {
	case a := <-answer:
		if string(a.body) != "0\n" {
			t.Fatalf("%q", a.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no answer after the cancel")
	}
}

func TestLongAnswerComesInParts(t *testing.T) {
	var locs []any
	for i := 0; i < 6001; i++ {
		locs = append(locs, lspLoc(uri, 0, 0, 0, 1))
	}
	h := newHarness(t, allFeatures, `{"referencesProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return locs, true
	})
	_, answer := h.host.askAsync("references 1 1 1 1 invoked -")
	part := h.host.request("query-part")
	if !strings.HasPrefix(string(part.body), "5000\n") {
		t.Fatalf("%.20q", part.body)
	}
	a := <-answer
	if !strings.HasPrefix(string(a.body), "1001\n") {
		t.Fatalf("%.20q", a.body)
	}
}

func TestCodeActionsOfProtocol11(t *testing.T) {
	diag := map[string]any{"range": rng(2, 11, 2, 16), "message": "unused total", "severity": 2, "source": "vet"}
	edit := map[string]any{"changes": map[string]any{uri: []any{map[string]any{"range": rng(2, 11, 2, 16), "newText": "_"}}}}
	h := newHarness(t, append(allFeatures, rhunext.FeatureFileDiagnostics), `{"codeActionProvider":{"resolveProvider":true},"executeCommandProvider":{"commands":["fix"]}}`, sample,
		func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
			switch method {
			case "textDocument/codeAction":
				return []any{
					map[string]any{"title": "Blank it", "kind": "quickfix", "isPreferred": true, "diagnostics": []any{diag}, "edit": edit},
					map[string]any{"title": "Extract", "kind": "refactor.extract", "data": map[string]any{"n": 1}},
					map[string]any{"title": "Both", "kind": "source.fixAll.vet", "edit": edit, "command": map[string]any{"title": "Both", "command": "fix", "arguments": []any{"a"}}},
					map[string]any{"title": "Nope", "kind": "refactor.inline", "disabled": map[string]any{"reason": "not here"}},
					map[string]any{"title": "Run it", "command": "fix", "arguments": []any{"b"}},
					map[string]any{"title": "Nothing", "kind": "quickfix"},
				}, true
			case "codeAction/resolve":
				return map[string]any{"title": "Extract", "kind": "refactor.extract", "edit": edit}, true
			}
			return nil, true
		})
	h.l.mu.Lock()
	raw, _ := json.Marshal(diag)
	other, _ := json.Marshal(map[string]any{"range": rng(0, 0, 0, 1), "message": "other"})
	h.l.diags[harnessPath] = keepDiagnostics([]json.RawMessage{other, raw})
	ids := h.l.diags[harnessPath].ids
	h.l.mu.Unlock()
	a := h.host.ask("code-actions 1 1 3 13 3 18 auto - "+ids[1], nil)
	acts := parseActions(t, a.body)
	var titles []string
	for _, r := range acts {
		titles = append(titles, r.kind+":"+r.title)
	}
	if strings.Join(titles, "|") != "quickfix:Blank it|refactor.extract:Extract|source.fixAll.vet:Both|refactor.inline:Nope|action:Run it" {
		t.Fatal(titles)
	}
	if acts[0].edit == "" || !strings.Contains(acts[0].edit, "3 13 3 18 1\n_") {
		t.Fatalf("%q", acts[0].edit)
	}
	if _, ok := extraRecord(acts[0].extra, "preferred"); !ok {
		t.Fatal(acts[0].extra)
	}
	if fixes, _ := extraRecord(acts[0].extra, "fixes"); fixes != ids[1] {
		t.Fatalf("fixes %q", fixes)
	}
	if reason, _ := extraRecord(acts[3].extra, "disabled"); reason != "not here" {
		t.Fatal(acts[3].extra)
	}
	for _, i := range []int{1, 2, 4} {
		if acts[i].edit != "" {
			t.Fatal("an edit the extension applies itself", acts[i])
		}
		if _, ok := extraRecord(acts[i].extra, "resolve"); !ok {
			t.Fatal(acts[i])
		}
	}
	ctx := h.srv.messages("textDocument/codeAction")[0]["params"].(map[string]any)["context"].(map[string]any)
	if ctx["triggerKind"] != float64(2) || ctx["only"] != nil || len(ctx["diagnostics"].([]any)) != 1 || ctx["diagnostics"].([]any)[0].(map[string]any)["message"] != "unused total" {
		t.Fatal(ctx)
	}
	// resolve: the server's edit for rhun
	data, _ := extraRecord(acts[1].extra, "resolve")
	r := h.host.ask("code-action-resolve 1 1 "+itoa(len(data)), []byte(data))
	if !r.ok || !strings.Contains(string(r.body), "\n2 1 1\n#13 13 3 18 1\n_") || !strings.HasSuffix(string(r.body), "_") {
		t.Fatalf("%+v %q", r, r.body)
	}
	if res := h.srv.messages("codeAction/resolve"); len(res) != 1 || res[0]["params"].(map[string]any)["data"] == nil {
		t.Fatal(res)
	}
	// a command: nothing for rhun, the extension runs it
	data, _ = extraRecord(acts[4].extra, "resolve")
	r = h.host.ask("code-action-resolve 1 1 "+itoa(len(data)), []byte(data))
	if !r.ok || string(r.body) != "0 0\n" {
		t.Fatalf("%q", r.body)
	}
	exec := h.srv.await("workspace/executeCommand")["params"].(map[string]any)
	if exec["command"] != "fix" || exec["arguments"].([]any)[0] != "b" {
		t.Fatal(exec)
	}
	// kinds: only, triggerKind invoked, the range's diagnostics without file-diagnostics ids
	a = h.host.ask("code-actions 1 1 3 13 3 13 invoked quickfix,source -", nil)
	if acts := parseActions(t, a.body); len(acts) != 2 || acts[0].title != "Blank it" || acts[1].title != "Both" {
		t.Fatal(acts)
	}
	ctx = h.srv.messages("textDocument/codeAction")[1]["params"].(map[string]any)["context"].(map[string]any)
	if ctx["triggerKind"] != float64(1) || len(ctx["only"].([]any)) != 2 || len(ctx["diagnostics"].([]any)) != 0 {
		t.Fatal(ctx)
	}
	if r := h.host.ask("code-action-resolve 1 1 4", []byte("9.99")); r.ok {
		t.Fatal("a forgotten action resolved")
	}
}

func TestCodeActionContextFromTheRangeWithoutFileDiagnostics(t *testing.T) {
	h := newHarness(t, allFeatures, `{"codeActionProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return []any{}, true
	})
	inside, _ := json.Marshal(map[string]any{"range": rng(2, 11, 2, 16), "message": "in"})
	outside, _ := json.Marshal(map[string]any{"range": rng(0, 0, 0, 1), "message": "out"})
	h.l.mu.Lock()
	h.l.diags[harnessPath] = keepDiagnostics([]json.RawMessage{outside, inside})
	h.l.mu.Unlock()
	h.host.ask("code-actions 1 1 3 14 3 14 invoked - -", nil)
	ctx := h.srv.messages("textDocument/codeAction")[0]["params"].(map[string]any)["context"].(map[string]any)
	if d := ctx["diagnostics"].([]any); len(d) != 1 || d[0].(map[string]any)["message"] != "in" {
		t.Fatal(ctx)
	}
}

func TestPrepareRename(t *testing.T) {
	var answer func(id json.RawMessage, f *fakeLSP) (any, bool)
	h := newHarness(t, allFeatures, `{"renameProvider":{"prepareProvider":true}}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return answer(id, f)
	})
	for _, tc := range []struct {
		result any
		want   string
	}{
		{rng(2, 11, 2, 16), "ok:3 13 3 18 5\ntotal"},
		{map[string]any{"range": rng(2, 5, 2, 6), "placeholder": "é"}, "ok:3 6 3 8 2\né"},
		{map[string]any{"defaultBehavior": true}, "ok:"},
		{nil, "error:refused Nothing to rename here"},
	} {
		answer = func(json.RawMessage, *fakeLSP) (any, bool) { return tc.result, true }
		a := h.host.ask("prepare-rename 1 1 3 14 invoked -", nil)
		got := "ok:" + string(a.body)
		if !a.ok {
			got = "error:" + strings.Join(a.args, " ")
		}
		if got != tc.want {
			t.Fatalf("%v: %q", tc.result, got)
		}
	}
	answer = func(id json.RawMessage, f *fakeLSP) (any, bool) {
		f.fail(id, -32602, "cannot rename a keyword\nmore")
		return nil, false
	}
	if a := h.host.ask("prepare-rename 1 1 1 2 invoked -", nil); a.ok || strings.Join(a.args, " ") != "refused cannot rename a keyword" {
		t.Fatal(a)
	}
	answer = func(id json.RawMessage, f *fakeLSP) (any, bool) {
		f.fail(id, codeContentModified, "modified")
		return nil, false
	}
	if a := h.host.ask("prepare-rename 1 1 1 2 invoked -", nil); !a.ok || len(a.body) != 0 {
		t.Fatal(a)
	}
	h.s.loadCapabilities(json.RawMessage(`{"capabilities":{"renameProvider":true}}`))
	n := len(h.srv.messages("textDocument/prepareRename"))
	if a := h.host.ask("prepare-rename 1 1 1 2 invoked -", nil); !a.ok || len(a.body) != 0 || len(h.srv.messages("textDocument/prepareRename")) != n {
		t.Fatal("asked a server without prepareProvider", a)
	}
}

func TestRangeAndTypeFormatting(t *testing.T) {
	h := newHarness(t, allFeatures, `{"documentRangeFormattingProvider":true,"documentOnTypeFormattingProvider":{"firstTriggerCharacter":"\n"}}`, sample,
		func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
			return []any{map[string]any{"range": rng(2, 9, 2, 11), "newText": " { "}}, true
		})
	a := h.host.ask("range-format 1 1 3 1 3 20 4 yes no yes", nil)
	if string(a.body) != "1\n2 1 1\n#13 11 3 13 3\n { " {
		t.Fatalf("%q", a.body)
	}
	p := h.srv.messages("textDocument/rangeFormatting")[0]["params"].(map[string]any)
	if o := p["options"].(map[string]any); o["tabSize"] != float64(4) || o["insertSpaces"] != true || o["trimTrailingWhitespace"] != false || o["insertFinalNewline"] != true {
		t.Fatal(o)
	}
	if r := p["range"].(map[string]any); r["end"].(map[string]any)["character"] != float64(18) {
		t.Fatal(r)
	}
	a = h.host.ask("type-format 1 1 3 1 newline 8 no yes no", nil)
	if string(a.body) != "1\n2 1 1\n#13 11 3 13 3\n { " {
		t.Fatalf("%q", a.body)
	}
	p = h.srv.messages("textDocument/onTypeFormatting")[0]["params"].(map[string]any)
	if p["ch"] != "\n" || p["position"].(map[string]any)["line"] != float64(2) || p["options"].(map[string]any)["trimTrailingWhitespace"] != true {
		t.Fatal(p)
	}
	h.s.ready.Store(false)
	if a := h.host.ask("type-format 1 1 3 1 ; 8 no yes no", nil); string(a.body) != "0\n" || len(h.srv.messages("textDocument/onTypeFormatting")) != 1 {
		t.Fatalf("asked a server that is not ready: %q", a.body)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// line3 is the record text of sample's third line
const line3 = "func é() { total := 1 }"

// locRecord is a locations record for main.go
func locRecord(rangeFields string) string {
	return fmt.Sprintf("%s 16 %d\n/project/main.go%s", rangeFields, len(line3), line3)
}
