package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

var allCaps = editCaps{ws: true, ops: true}

// a project with main.go open (#1, rhun version 7, LSP version 3) and on disk only: crlf.go
// (CRLF line ends, a character outside the BMP), a.go, gone.go
func convertHarness(t *testing.T) (*lsp, *server, *documentQuery, string) {
	dir := t.TempDir()
	l, s, _ := testServer("Go")
	t.Cleanup(s.stop)
	write := func(name, text string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.go", "package main\n")
	write("crlf.go", "a😀b\r\nline two\r\n")
	write("a.go", "package a\n\nfunc F() {}\n")
	write("gone.go", "package gone\n")
	d := &document{id: 1, version: 7, lspVer: 3, lang: "Go", path: filepath.ToSlash(filepath.Join(dir, "main.go")), text: []byte("package main\n"), server: s, syncedServer: s, syncedVersion: 7}
	l.docs = map[rhunext.Doc]*document{1: d}
	l.byPath = map[string]*document{d.path: d}
	q := &documentQuery{origin: d, id: 1, version: 7, lspVersion: 3, path: d.path, lang: "Go", text: d.text, server: s}
	return l, s, q, dir
}

func lspEditJSON(l1, c1, l2, c2 int, text string) string {
	return fmt.Sprintf(`{"range":{"start":{"line":%d,"character":%d},"end":{"line":%d,"character":%d}},"newText":%q}`, l1, c1, l2, c2, text)
}

func hashOf(t *testing.T, path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("fnv1a64:%016x", rhunext.Fnv1a64(data))
}

func TestConvertEditTransactions(t *testing.T) {
	l, s, q, dir := convertHarness(t)
	p := func(name string) string { return filepath.ToSlash(filepath.Join(dir, name)) } // as rhun writes paths
	u := func(name string) string { return fileURI(p(name)) }
	raw := `{"documentChanges":[` +
		`{"textDocument":{"uri":"` + u("main.go") + `","version":3},"edits":[` + lspEditJSON(0, 8, 0, 12, "MAIN") + `]},` +
		`{"textDocument":{"uri":"` + u("crlf.go") + `","version":null},"edits":[` + lspEditJSON(0, 3, 0, 4, "B") + `,` + lspEditJSON(1, 0, 1, 8, "2") + `]},` +
		`{"textDocument":{"uri":"` + u("a.go") + `","version":null},"edits":[` + lspEditJSON(0, 8, 0, 9, "b") + `]},` +
		`{"kind":"rename","oldUri":"` + u("a.go") + `","newUri":"` + u("b.go") + `"},` +
		`{"textDocument":{"uri":"` + u("b.go") + `","version":null},"edits":[` + lspEditJSON(2, 5, 2, 6, "G") + `]},` +
		`{"kind":"create","uri":"` + u("new.go") + `","options":{"ignoreIfExists":true}},` +
		`{"textDocument":{"uri":"` + u("new.go") + `","version":null},"edits":[` + lspEditJSON(0, 0, 0, 0, "package b\n") + `]},` +
		`{"kind":"delete","uri":"` + u("gone.go") + `","annotationId":"careful"}` +
		`],"changeAnnotations":{"careful":{"label":"Delete","needsConfirmation":true}}}`
	conv, err := l.convertEdit(json.RawMessage(raw), convertOptions{caps: allCaps, queries: []*documentQuery{q}, strict: true, server: s, label: "Move F"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := conv.edit.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("7\ncreate %d ignore-existing\n%s", len(p("new.go")), p("new.go")) +
		"2 7 1\n#11 9 1 13 4\nMAIN" +
		fmt.Sprintf("%d %s 2\n%s1 6 1 7 1\nB2 1 2 9 1\n2", len(p("crlf.go")), hashOf(t, p("crlf.go")), p("crlf.go")) +
		fmt.Sprintf("%d %s 2\n%s1 9 1 10 1\nb3 6 3 7 1\nG", len(p("a.go")), hashOf(t, p("a.go")), p("a.go")) +
		fmt.Sprintf("%d - 1\n%s1 1 1 1 10\npackage b\n", len(p("new.go")), p("new.go")) +
		fmt.Sprintf("rename %d %d -\n%s%s", len(p("a.go")), len(p("b.go")), p("a.go"), p("b.go")) +
		fmt.Sprintf("delete %d -\n%s", len(p("gone.go")), p("gone.go")) +
		"label 6\nMove Fconfirm 0\n"
	if string(body) != want {
		t.Fatalf("got\n%q\nwant\n%q", body, want)
	}
	// each entry's documentChanges index: create 5, the groups 0 1 2 6, rename 3, delete 7
	if fmt.Sprint(conv.origins) != "[5 0 1 2 6 3 7]" {
		t.Fatalf("origins %v", conv.origins)
	}
	if len(conv.ops) != 3 || conv.ops[0] != (fileOp{Kind: "rename", Path: p("a.go"), To: p("b.go")}) || conv.ops[2].Kind != "delete" {
		t.Fatalf("ops %+v", conv.ops)
	}
	if n := failedChange(&rhunext.ApplyError{Code: "stale", Entry: 4, Text: "x"}, conv.origins); n != 2 {
		t.Fatalf("failedChange %d", n)
	}
}

func TestDeferredWorkspaceEditUsesOnlyPreRequestSnapshots(t *testing.T) {
	l, s, q, dir := convertHarness(t)
	path := filepath.ToSlash(filepath.Join(dir, "a.go"))
	raw := json.RawMessage(fmt.Sprintf(`{"changes":{%q:[%s]}}`, fileURI(path), lspEditJSON(0, 0, 0, 9, "[R]")))
	o := convertOptions{caps: allCaps, queries: []*documentQuery{q}, strict: true, snapshotsOnly: true, server: s}
	if _, err := l.convertEdit(raw, o); err == nil {
		t.Fatal("accepted a closed target without a pre-request snapshot")
	}
	foreign := &document{id: 2, version: 8, path: path, text: []byte("package a\n")}
	l.docs[2], l.byPath[path] = foreign, foreign
	if _, err := l.convertEdit(raw, o); err == nil {
		t.Fatal("accepted a target opened after the request")
	}
	o.queries = append(o.queries, &documentQuery{id: 2, version: 8, path: path, text: foreign.text, lspVersion: -1})
	foreign.text, foreign.version = []byte("zpackage a\n"), 9
	conv, err := l.convertEdit(raw, o)
	if err != nil || len(conv.edit.Files) != 1 || conv.edit.Files[0].Target.Doc != 2 || conv.edit.Files[0].Version != 8 {
		t.Fatal(conv, err)
	}
}

func TestConvertEditRefusals(t *testing.T) {
	l, s, q, dir := convertHarness(t)
	u := func(name string) string { return fileURI(filepath.Join(dir, name)) }
	o := convertOptions{caps: allCaps, queries: []*documentQuery{q}, strict: true, server: s}
	cases := []struct {
		raw    string
		caps   editCaps
		change int
		part   string
	}{
		{`{"documentChanges":[{"kind":"create","uri":"` + u("x.go") + `"}]}`, editCaps{ws: true}, 0, "creates, renames or deletes"},
		{`{"documentChanges":[{"kind":"delete","uri":"` + u("gone.go") + `"},{"textDocument":{"uri":"` + u("gone.go") + `","version":null},"edits":[]}]}`, allCaps, 1, "was deleted"},
		{`{"documentChanges":[{"textDocument":{"uri":"` + u("main.go") + `","version":2},"edits":[]}]}`, allCaps, 0, "stale LSP"},
		{`{"documentChanges":[{"textDocument":{"uri":"` + u("main.go") + `","version":3},"edits":[` + lspEditJSON(5, 0, 5, 1, "x") + `]}]}`, allCaps, 0, "outside the document"},
		{`{"documentChanges":[{"textDocument":{"uri":"` + u("a.go") + `","version":null},"edits":[` + lspEditJSON(0, 0, 0, 0, "XY") + `]},` +
			`{"textDocument":{"uri":"` + u("a.go") + `","version":null},"edits":[` + lspEditJSON(0, 1, 0, 1, "!") + `]}]}`, allCaps, 1, "earlier edit"},
		{`{"documentChanges":[{"kind":"move","uri":"` + u("a.go") + `"}]}`, allCaps, 0, "unknown resource operation"},
		{`{"documentChanges":[{"textDocument":{"uri":"` + u("none.go") + `","version":null},"edits":[]}]}`, allCaps, 0, "cannot read"},
	}
	for _, c := range cases {
		o.caps = c.caps
		_, err := l.convertEdit(json.RawMessage(c.raw), o)
		var ee *editError
		if !errors.As(err, &ee) || ee.change != c.change || !strings.Contains(err.Error(), c.part) {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if n := failedChange(err, nil); n != c.change {
			t.Fatalf("failedChange %d", n)
		}
	}
	// changes (no order, no failedChange): a file that is not open is hashed
	o.caps = allCaps
	conv, err := l.convertEdit(json.RawMessage(`{"changes":{"`+u("a.go")+`":[`+lspEditJSON(0, 0, 0, 7, "PACKAGE")+`]}}`), o)
	if err != nil || conv.origins != nil || len(conv.edit.Files) != 1 || !conv.edit.Files[0].Hashed || conv.edit.Files[0].Version != rhunext.OnDisk {
		t.Fatalf("%+v %v", conv, err)
	}
}

func TestWordPatternsBecomeTheWordRecord(t *testing.T) {
	cases := map[string]string{
		`[-_\w.:]+`:     "-_.:",
		`[a-zA-Z0-9-]*`: "-",
		`[\w\-\.]+`:     "_-.",
		`^[!-/\w]+$`:    "!\"#$%&'()*+,-./_",
		`[\s\w]+`:       " _",
	}
	for in, want := range cases {
		if got, ok := wordChars(in); !ok || got != want {
			t.Errorf("%s: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{`[^<>]+`, `(-?\d*\.\d\w*)|([^\s]+)`, `\w+`, `[\p{L}]+`, `[a-z][0-9]`, ``} {
		if got, ok := wordChars(bad); ok {
			t.Errorf("%s: %q", bad, got)
		}
	}
}

func TestLinkedEditingFromTheServer(t *testing.T) {
	l, s, d, p := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	d.text = []byte("<div>\n</div>\n")
	s.staticCapabilities["textDocument/linkedEditingRange"] = true
	p.onBody = func(b []byte) {
		var m struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal(b, &m) == nil && m.Method == "textDocument/linkedEditingRange" {
			reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"ranges":[`+
				`{"start":{"line":0,"character":1},"end":{"line":0,"character":4}},`+
				`{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}],"wordPattern":"[-_\\w.:]+"}}`, m.ID)
			go s.read(bufio.NewReader(strings.NewReader(rpcFrame(reply))))
		}
	}
	got := l.Linked(nil, rhunext.LinkedRequest{Doc: 1, Version: 1, At: rhunext.Pos{Line: 1, Col: 3}})
	if len(got.Ranges) != 2 || got.Ranges[1] != (rhunext.Range{Start: rhunext.Pos{Line: 2, Col: 3}, End: rhunext.Pos{Line: 2, Col: 6}}) ||
		got.Word == nil || *got.Word != "-_.:" {
		t.Fatalf("%+v", got)
	}
	// another version: nothing asked
	if got := l.Linked(nil, rhunext.LinkedRequest{Doc: 1, Version: 9, At: rhunext.Pos{Line: 1, Col: 3}}); len(got.Ranges) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestFileOperationNotifications(t *testing.T) {
	l, s, p := testServer("Go")
	defer s.stop()
	s.ready.Store(true)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.go"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "pkg"), 0o755)
	caps := `{"capabilities":{"workspace":{"fileOperations":{"didRename":{"filters":[{"scheme":"file","pattern":{"glob":"**/*.go","matches":"file"}}]},` +
		`"didDelete":{"filters":[{"pattern":{"glob":"**/*.GO","options":{"ignoreCase":true}}}]}}}}}`
	if err := s.loadCapabilities(json.RawMessage(caps)); err != nil {
		t.Fatal(err)
	}
	l.notifyFileOps([]fileOp{
		{Kind: "rename", Path: filepath.Join(dir, "a.go"), To: filepath.Join(dir, "b.go")},
		{Kind: "rename", Path: filepath.Join(dir, "old"), To: filepath.Join(dir, "pkg")}, // a folder: not asked for
		{Kind: "create", Path: filepath.Join(dir, "c.go")},                               // creates: not asked for
		{Kind: "delete", Path: filepath.Join(dir, "gone.go")},
	})
	msgs := p.messages(t)
	if len(msgs) != 2 || msgs[0]["method"] != "workspace/didRenameFiles" || msgs[1]["method"] != "workspace/didDeleteFiles" {
		t.Fatalf("%v", msgs)
	}
	files := msgs[0]["params"].(map[string]any)["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["newUri"] != fileURI(filepath.Join(dir, "b.go")) {
		t.Fatalf("%v", files)
	}
	// a registration adds creates
	params, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": "c", "method": "workspace/didCreateFiles",
		"registerOptions": map[string]any{"filters": []any{map[string]any{"pattern": map[string]any{"glob": "**/c.go"}}}}}}})
	if _, code, msg := s.registerCapabilities(params); code != 0 {
		t.Fatal(code, msg)
	}
	l.notifyFileOps([]fileOp{{Kind: "create", Path: filepath.Join(dir, "c.go")}})
	if msgs := p.messages(t); len(msgs) != 3 || msgs[2]["method"] != "workspace/didCreateFiles" {
		t.Fatalf("%v", msgs)
	}
	bad, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": "d", "method": "workspace/didDeleteFiles", "registerOptions": map[string]any{}}}})
	if _, code, _ := s.registerCapabilities(bad); code == 0 {
		t.Fatal("filters missing")
	}
	// an answered edit's operations are told once the disk shows them
	l.expectFileOps([]fileOp{{Kind: "rename", Path: filepath.Join(dir, "x.go"), To: filepath.Join(dir, "y.go")}})
	l.checkFileOps()
	if msgs := p.messages(t); len(msgs) != 3 {
		t.Fatal("told before it was made")
	}
	os.WriteFile(filepath.Join(dir, "y.go"), nil, 0o644)
	l.checkFileOps()
	if msgs := p.messages(t); len(msgs) != 4 || msgs[3]["method"] != "workspace/didRenameFiles" {
		t.Fatalf("%v", msgs)
	}
	l.checkFileOps()
	if msgs := p.messages(t); len(msgs) != 4 {
		t.Fatal("told twice")
	}
}

func TestCommandEditSnapshotsAndOperations(t *testing.T) {
	q := &documentQuery{id: 1, version: 4, path: "/p/main.go", text: []byte("ab\n")}
	var w rhunext.WorkspaceEdit
	w.AddDoc(1, 4, rhunext.TextEdit{Range: rhunext.Range{Start: rhunext.Pos{Line: 1, Col: 1}, End: rhunext.Pos{Line: 1, Col: 2}}, Text: "X"})
	w.AddFileHashed("/p/closed.go", []byte("x"))
	w.Rename("/p/main.go", "/p/m2.go", rhunext.RenameOptions{})
	next, err := commandEditSnapshots(w, []*documentQuery{q})
	if err != nil || string(next[0].text) != "Xb\n" || next[0].version != 6 {
		t.Fatalf("%v %v", next, err)
	}
	if !movedByOps("/p/main.go", w.Ops) || movedByOps("/p/other.go", w.Ops) || !movedByOps("/p/dir/x.go", []rhunext.FileOp{{Kind: rhunext.OpDelete, Path: "/p/dir"}}) {
		t.Fatal("movedByOps")
	}
}

func TestWorkspaceEditCapabilitiesFollowTheAgreedFeatures(t *testing.T) {
	if c := workspaceEditCapabilities(editCaps{}); fmt.Sprint(c["resourceOperations"]) != "[]" || c["failureHandling"] != nil {
		t.Fatal(c)
	}
	c := workspaceEditCapabilities(allCaps)
	if fmt.Sprint(c["resourceOperations"]) != "[create rename delete]" || c["failureHandling"] != "transactional" || c["normalizesLineEndings"] != true {
		t.Fatal(c)
	}
}

// fakeRhun runs an Ext against scripted replies: what the extension sends comes on lines
type fakeRhun struct {
	t     *testing.T
	ext   chan *rhunext.Ext
	lines *bufio.Reader
	w     *io.PipeWriter
}

type extCatcher struct{ got chan *rhunext.Ext }

func (c extCatcher) Ready(ext *rhunext.Ext, project string)              { c.got <- ext }
func (c extCatcher) Run(ext *rhunext.Ext, command string, d rhunext.Doc) {}
func (c extCatcher) Event(ext *rhunext.Ext, ev rhunext.Event)            {}

func startFakeRhun(t *testing.T, features string) (*rhunext.Ext, *fakeRhun) {
	toExt, hostW := io.Pipe()
	hostR, fromExt := io.Pipe()
	f := &fakeRhun{t: t, ext: make(chan *rhunext.Ext, 1), lines: bufio.NewReader(hostR), w: hostW}
	go rhunext.RunOnWith(toExt, fromExt, io.Discard, strings.Fields(features), extCatcher{f.ext})
	f.line() // hello
	io.WriteString(hostW, "0 features "+features+"\n0 ready -\n")
	select {
	case ext := <-f.ext:
		t.Cleanup(func() { io.WriteString(hostW, "0 bye\n") })
		return ext, f
	case <-time.After(3 * time.Second):
		t.Fatal("no ready")
		return nil, nil
	}
}

func (f *fakeRhun) line() string {
	f.t.Helper()
	got := make(chan string, 1)
	go func() { l, _ := f.lines.ReadString('\n'); got <- strings.TrimSuffix(l, "\n") }()
	select {
	case l := <-got:
		return l
	case <-time.After(3 * time.Second):
		f.t.Fatal("nothing sent")
		return ""
	}
}

func TestShowDocumentOpensSelectsAndReveals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.ToSlash(filepath.Join(dir, "a.go"))
	os.WriteFile(path, []byte("package a\n\nfunc F() {}\n"), 0o644)
	ext, rhun := startFakeRhun(t, "reveal")
	l, s, _ := testServer("Go")
	defer s.stop()
	l.ext = ext
	l.byPath = map[string]*document{}
	done := make(chan bool, 1)
	go func() {
		done <- s.showDocumentResult(json.RawMessage(`{"uri":"` + fileURI(path) + `","takeFocus":true,` +
			`"selection":{"start":{"line":2,"character":5},"end":{"line":2,"character":6}}}`))
	}()
	if l := rhun.line(); l != "1 open 3 6 "+path {
		t.Fatal(l)
	}
	io.WriteString(rhun.w, "ok 1 4\n")
	if l := rhun.line(); l != "2 select 4 3 6 3 7" {
		t.Fatal(l)
	}
	io.WriteString(rhun.w, "ok 2\n")
	if l := rhun.line(); l != "3 reveal 4 3 6 center-if-outside" {
		t.Fatal(l)
	}
	io.WriteString(rhun.w, "ok 3\n")
	if !<-done {
		t.Fatal("not shown")
	}
	for _, refused := range []string{`{"uri":"https://example.org","external":true}`, `{"uri":"untitled:1"}`, `{"uri":"` + fileURI(path) + `","external":true}`} {
		if s.showDocumentResult(json.RawMessage(refused)) {
			t.Fatal(refused)
		}
	}
}

func TestNumericVersionsOfAnotherServerAreRefusedInCommandEdits(t *testing.T) {
	l, s, q, dir := convertHarness(t)
	raw := json.RawMessage(`{"documentChanges":[{"textDocument":{"uri":"` + fileURI(filepath.Join(dir, "a.go")) + `","version":4},"edits":[]}]}`)
	if _, err := l.convertEdit(raw, convertOptions{caps: allCaps, queries: []*documentQuery{q}, strict: true, server: s}); err == nil || !strings.Contains(err.Error(), "stale LSP") {
		t.Fatal(err)
	}
	q.lspVersion = -1
	raw = json.RawMessage(`{"documentChanges":[{"textDocument":{"uri":"` + fileURI(q.path) + `","version":3},"edits":[]}]}`)
	if _, err := l.convertEdit(raw, convertOptions{caps: allCaps, queries: []*documentQuery{q}, strict: true, server: s}); err == nil {
		t.Fatal("a version of another server's session accepted")
	}
	// a rename's answer is not strict: the version of a file that is not open is not looked at
	if _, err := l.convertEdit(raw, convertOptions{caps: allCaps, queries: []*documentQuery{q}, server: s}); err != nil {
		t.Fatal(err)
	}
}
