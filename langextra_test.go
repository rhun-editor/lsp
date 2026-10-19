package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

var moreFeatures = append(append([]string{}, allFeatures...), rhunext.FeatureHoverQuery, rhunext.FeatureSelectionRanges,
	rhunext.FeatureHierarchy, rhunext.FeatureWorkspaceSymbols, rhunext.FeatureDocumentLinks, rhunext.FeatureFileDiagnostics)

func TestFileDiagnosticsCarryIDsCodesLinksTagsAndRelatedPlaces(t *testing.T) {
	h := newHarness(t, moreFeatures, `{}`, sample, nil)
	other := filepath.Join(t.TempDir(), "other.go")
	os.WriteFile(other, []byte("package é\nvar x = 1\n"), 0o600)
	h.srv.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": []any{
		map[string]any{"range": rng(2, 11, 2, 16), "severity": 2, "code": "unusedvar", "source": "vet",
			"codeDescription": map[string]any{"href": "https://example.org/unused"}, "tags": []int{1},
			"message":            "total declared and not used\nsecond line",
			"relatedInformation": []any{map[string]any{"location": lspLoc(fileURI(other), 1, 4, 1, 5), "message": "first\nuse"}}},
		map[string]any{"range": rng(0, 0, 0, 7), "code": 42, "message": "number code", "tags": []int{2}},
	}})
	r := h.host.request("file-diagnostics")
	if strings.Join(r.args, " ") != fmt.Sprintf("1 2 2 %d", len(r.body)) || !strings.HasPrefix(string(r.body), "#1") {
		t.Fatalf("%q %q", r.args, r.body)
	}
	h.l.mu.Lock()
	ids := h.l.diags[harnessPath].ids
	h.l.mu.Unlock()
	body := string(r.body)
	for _, want := range []string{
		"3 13 3 18 warning 39 ", "total declared and not used\nsecond line", "id " + fmt.Sprint(len(ids[0])) + "\n" + ids[0],
		"code 9\nunusedvar", "source 3\nvet", "href 26\nhttps://example.org/unused", "tag unnecessary 0\n",
		fmt.Sprintf("related 2 5 2 6 %d 5\n%sfirst", len(other), other),
		"1 1 1 8 error 11 ", "code 2\n42", "tag deprecated 0\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("no %q in %q", want, body)
		}
	}
	// a file that is not open: its path, version -, positions in the file on disk
	h.srv.notify("textDocument/publishDiagnostics", map[string]any{"uri": fileURI(other), "diagnostics": []any{
		map[string]any{"range": rng(0, 8, 0, 9), "message": "at é"}}})
	r = h.host.request("file-diagnostics")
	if r.args[0] != "-" || !strings.HasPrefix(string(r.body), other+"1 9 1 11 error 5 ") {
		t.Fatalf("%q %q", r.args, r.body)
	}
	// the server stops: its sets are emptied
	h.s.stop()
	cleared := map[string]bool{}
	for i := 0; i < 2; i++ {
		r = h.host.request("file-diagnostics")
		if r.args[1] != "0" {
			t.Fatalf("%q %q", r.args, r.body)
		}
		cleared[string(r.body)] = true
	}
	if !cleared[harnessPath] || !cleared[other] {
		t.Fatal(cleared)
	}
}

func TestV1DiagnosticsWithoutFileDiagnostics(t *testing.T) {
	h := newHarness(t, allFeatures, `{}`, sample, nil)
	h.srv.notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": []any{
		map[string]any{"range": rng(2, 11, 2, 16), "severity": 1, "source": "vet", "message": "unused\nmore"}}})
	r := h.host.request("diagnostics")
	if strings.Join(r.args[:3], " ") != "1 1 1" || !strings.Contains(string(r.body), "vet: unused") || strings.Contains(string(r.body), "more") {
		t.Fatal(r.args)
	}
}

func TestPullDiagnosticsAfterChanges(t *testing.T) {
	var pulls atomic.Int32
	h := newHarness(t, moreFeatures, `{"diagnosticProvider":{"interFileDependencies":false,"workspaceDiagnostics":false}}`, sample,
		func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
			if method != "textDocument/diagnostic" {
				return nil, true
			}
			if pulls.Add(1) == 1 {
				return map[string]any{"kind": "full", "resultId": "r1", "items": []any{map[string]any{"range": rng(0, 0, 0, 7), "message": "pulled"}}}, true
			}
			return map[string]any{"kind": "unchanged", "resultId": "r1"}, true
		})
	h.l.schedulePull(h.d, h.s)
	r := h.host.request("file-diagnostics")
	if !strings.Contains(string(r.body), "pulled") {
		t.Fatalf("%q", r.body)
	}
	h.l.schedulePull(h.d, h.s)
	second := h.srv.awaitN("textDocument/diagnostic", 2)
	if second["params"].(map[string]any)["previousResultId"] != "r1" {
		t.Fatal(second)
	}
	if refresh, code, _ := h.s.requestResult("workspace/diagnostic/refresh", nil); refresh != nil || code != 0 {
		t.Fatal(code)
	}
	h.srv.awaitN("textDocument/diagnostic", 3)
}

func TestHoverQuery(t *testing.T) {
	contents := map[string]any{"kind": "markdown", "value": "```go\nvar total int\n```\n**used** once"}
	handle := func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return map[string]any{"contents": contents, "range": rng(2, 11, 2, 16)}, true
	}
	h := newHarness(t, moreFeatures, `{"hoverProvider":true}`, sample, handle)
	a := h.host.ask("hover-query 1 1 3 14 invoked -", nil)
	if string(a.body) != "3 13 3 18\nvar total int\nused once" {
		t.Fatalf("%q", a.body)
	}
	md := newHarness(t, append(moreFeatures, featureMarkdown), `{"hoverProvider":true}`, sample, handle)
	a = md.host.ask("hover-query 1 1 3 14 pointer -", nil)
	if string(a.body) != "3 13 3 18\n```go\nvar total int\n```\n**used** once" {
		t.Fatalf("%q", a.body)
	}
	contents = map[string]any{"kind": "plaintext", "value": "a*b"}
	if a = md.host.ask("hover-query 1 1 3 14 invoked -", nil); string(a.body) != "3 13 3 18\na\\*b" {
		t.Fatalf("%q", a.body)
	}
	md.s.ready.Store(false)
	n := len(md.srv.messages("textDocument/hover"))
	if a = md.host.ask("hover-query 1 1 3 14 pointer -", nil); len(a.body) != 0 || len(md.srv.messages("textDocument/hover")) != n {
		t.Fatal("a pointer hover waited for a server that is not ready")
	}
}

func TestMarkdownOf(t *testing.T) {
	for raw, want := range map[string]string{
		`"plain *md*"`:                                         "plain *md*",
		`{"language":"go","value":"func f()\n"}`:               "```go\nfunc f()\n```",
		`[{"language":"c","value":"int x"},"doc"]`:             "```c\nint x\n```\n\n---\n\ndoc",
		`{"kind":"plaintext","value":"1. not a list\n[x](y)"}`: "1\\. not a list  \n\\[x\\]\\(y\\)",
		`{"kind":"markdown","value":"# Title"}`:                "# Title",
		`null`:                                                 "",
	} {
		if got := markdownOf(json.RawMessage(raw)); got != want {
			t.Fatalf("%s: %q", raw, got)
		}
	}
}

func TestSelectionRanges(t *testing.T) {
	h := newHarness(t, moreFeatures, `{"selectionRangeProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		word := map[string]any{"range": rng(2, 11, 2, 16), "parent": map[string]any{"range": rng(2, 9, 2, 23),
			"parent": map[string]any{"range": rng(2, 0, 2, 23)}}}
		return []any{word, map[string]any{"range": rng(0, 0, 0, 7)}}, true
	})
	a := h.host.ask("selection-ranges 1 1 2 9", []byte("3 14\n1 2\n"))
	if string(a.body) != "3\n3 13 3 18\n3 11 3 25\n3 1 3 25\n1\n1 1 1 8\n" {
		t.Fatalf("%q", a.body)
	}
	p := h.srv.messages("textDocument/selectionRange")[0]["params"].(map[string]any)["positions"].([]any)
	if len(p) != 2 || p[0].(map[string]any)["character"] != float64(12) {
		t.Fatal(p)
	}
}

func TestCallHierarchyWithItemData(t *testing.T) {
	item := map[string]any{"name": "é", "kind": 12, "detail": "func()", "uri": uri, "range": rng(2, 0, 2, 23), "selectionRange": rng(2, 5, 2, 6), "data": 7}
	caller := map[string]any{"name": "main", "kind": 12, "uri": uri, "range": rng(0, 0, 0, 12), "selectionRange": rng(0, 8, 0, 12)}
	h := newHarness(t, moreFeatures, `{"callHierarchyProvider":true,"typeHierarchyProvider":true}`, sample,
		func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
			switch method {
			case "textDocument/prepareCallHierarchy", "textDocument/prepareTypeHierarchy":
				return []any{item}, true
			case "callHierarchy/incomingCalls":
				return []any{map[string]any{"from": caller, "fromRanges": []any{rng(0, 0, 0, 4)}}}, true
			case "callHierarchy/outgoingCalls":
				return []any{map[string]any{"to": caller, "fromRanges": []any{rng(2, 11, 2, 16)}}}, true
			case "typeHierarchy/supertypes":
				return []any{caller}, true
			}
			return nil, true
		})
	a := h.host.ask("call-hierarchy 1 1 3 6 invoked -", nil)
	head, rest, _ := strings.Cut(string(a.body), "\n")
	f := strings.Fields(rest[:strings.IndexByte(rest, '\n')])
	if head != "1" || strings.Join(f[:9], " ") != "function 3 1 3 25 3 6 3 8" {
		t.Fatalf("%q", a.body)
	}
	dataLen := 0
	fmt.Sscan(f[12], &dataLen)
	data := rest[len(rest)-dataLen:]
	var hd hierarchyData
	if json.Unmarshal([]byte(data), &hd) != nil || hd.Lang != "Go" || hd.Generation != h.s.generation || !strings.Contains(string(hd.Item), `"data":7`) {
		t.Fatalf("%q", data)
	}
	in := h.host.ask(fmt.Sprintf("calls-in %d", len(data)), []byte(data))
	if !strings.HasPrefix(string(in.body), "1\nfunction 1 1 1 13 1 9 1 13 16 4 0 ") || !strings.HasSuffix(string(in.body), "}}1 1 1 5\n") {
		t.Fatalf("%q", in.body)
	}
	sent := h.srv.messages("callHierarchy/incomingCalls")[0]["params"].(map[string]any)["item"].(map[string]any)
	if sent["data"] != float64(7) || sent["name"] != "é" {
		t.Fatal(sent)
	}
	out := h.host.ask(fmt.Sprintf("calls-out %d", len(data)), []byte(data))
	if !strings.HasSuffix(string(out.body), "}}3 13 3 18\n") {
		t.Fatalf("%q", out.body)
	}
	a = h.host.ask("type-hierarchy 1 1 3 6 invoked -", nil)
	_, rest, _ = strings.Cut(string(a.body), "\n")
	f = strings.Fields(rest[:strings.IndexByte(rest, '\n')])
	fmt.Sscan(f[12], &dataLen)
	tdata := rest[len(rest)-dataLen:]
	if sup := h.host.ask(fmt.Sprintf("supertypes %d", len(tdata)), []byte(tdata)); !strings.HasPrefix(string(sup.body), "1\nfunction 1 1 1 13 ") {
		t.Fatalf("%q", sup.body)
	}
	// a restarted server's items are refused without asking anyone
	h.s.generation++
	n := len(h.srv.messages("callHierarchy/incomingCalls"))
	if stale := h.host.ask(fmt.Sprintf("calls-in %d", len(data)), []byte(data)); string(stale.body) != "0\n" || len(h.srv.messages("callHierarchy/incomingCalls")) != n {
		t.Fatalf("%q", stale.body)
	}
}

func TestWorkspaceSymbolsFromEveryServer(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.rs")
	os.WriteFile(lib, []byte("// é\nfn helper() {}\n"), 0o600)
	h := newHarness(t, moreFeatures, `{"workspaceSymbolProvider":true}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		return []any{map[string]any{"name": "total", "kind": 13, "containerName": "é", "location": lspLoc(uri, 2, 11, 2, 16)}}, true
	})
	addFakeServer(t, h.l, "Rust", `{"workspaceSymbolProvider":{"resolveProvider":true}}`, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		switch method {
		case "workspace/symbol":
			return []any{map[string]any{"name": "helper", "kind": 12, "location": map[string]any{"uri": fileURI(lib)}, "data": 1}}, true
		case "workspaceSymbol/resolve":
			return map[string]any{"name": "helper", "kind": 12, "location": lspLoc(fileURI(lib), 1, 3, 1, 9)}, true
		}
		return nil, true
	})
	a := h.host.ask("workspace-symbols 500 3", []byte("tot"))
	body := string(a.body)
	var parts []string
	for {
		select {
		case r := <-h.host.requests:
			if r.verb == "query-part" {
				parts = append(parts, string(r.body))
			}
			continue
		default:
		}
		break
	}
	all := body + strings.Join(parts, "")
	if !strings.Contains(all, "3 13 3 18 variable 16 5 2\n/project/main.gototalé") {
		t.Fatalf("%q", all)
	}
	if !strings.Contains(all, fmt.Sprintf("2 4 2 10 function %d 6 0\n%shelper", len(lib), lib)) {
		t.Fatalf("%q", all)
	}
}

func TestDocumentLinks(t *testing.T) {
	h := newHarness(t, moreFeatures, `{"documentLinkProvider":{"resolveProvider":true}}`, sample, func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool) {
		switch method {
		case "textDocument/documentLink":
			return []any{
				map[string]any{"range": rng(0, 0, 0, 7), "target": "https://example.org/x", "tooltip": "Open\nit"},
				map[string]any{"range": rng(2, 11, 2, 16), "target": "file:///usr/include/stdio.h#L12,3"},
				map[string]any{"range": rng(2, 5, 2, 6), "data": 1},
				map[string]any{"range": rng(2, 0, 2, 4), "target": "jdt://contents/x"},
			}, true
		case "documentLink/resolve":
			return map[string]any{"range": rng(2, 5, 2, 6), "target": uri, "tooltip": "resolved"}, true
		}
		return nil, true
	})
	a := h.host.ask("document-links 1 1", nil)
	want := "3\n1 1 1 8 21 4\nhttps://example.org/xOpen" +
		"3 13 3 18 25 0\n/usr/include/stdio.h:12:3" +
		"3 6 3 8 16 8\n/project/main.goresolved"
	if string(a.body) != want {
		t.Fatalf("%q", a.body)
	}
}

func TestLinkTargets(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.org/a?b#c": "https://example.org/a?b#c",
		"file:///a/b.go":            "/a/b.go",
		"file:///a/b.go#L5":         "/a/b.go:5",
		"file:///a/b.go#10:2":       "/a/b.go:10:2",
		"file:///a/b%20c.go":        "/a/b c.go",
		"mailto:x@example.org":      "",
		"relative/path":             "",
	} {
		if got := linkTarget(in); got != want {
			t.Fatalf("%s: %q", in, got)
		}
	}
}
