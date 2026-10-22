package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestSaveWorkspaceEditUsesSynchronizedLSPVersionAndOneTarget(t *testing.T) {
	posixFixtures(t)
	l := &lsp{}
	q := &documentQuery{id: 7, version: 99, lspVersion: 3, path: "/project/main.go", text: []byte("a😀b\n")}
	edit := `{"range":{"start":{"line":0,"character":1},"end":{"line":0,"character":3}},"newText":"x"}`
	changes := func(uri, edits string) string { return fmt.Sprintf(`{"changes":{%q:[%s]}}`, uri, edits) }
	versioned := func(version, edits string) string {
		return fmt.Sprintf(`{"documentChanges":[{"textDocument":{"uri":"file:///project/main.go","version":%s},"edits":[%s]}]}`, version, edits)
	}
	for _, raw := range []string{`{}`, changes("file:///project/main.go", edit), changes("file:///project/./main.go", edit), versioned("3", edit), versioned("null", edit)} {
		result, err := l.saveWorkspaceEdit(json.RawMessage(raw), q)
		if err != nil {
			t.Fatal(raw, err)
		}
		if len(result.Files) > 0 && (result.Files[0].Target.Doc != 7 || result.Files[0].Target.Path != "" || result.Files[0].Version != 99 || result.Files[0].Edits[0].Range.End.Col != 6) {
			t.Fatalf("%+v", result)
		}
	}
	for _, raw := range []string{
		changes("file:///project/other.go", edit), versioned("99", edit), versioned("2", edit), versioned(`"3"`, edit),
		`{"changes":{},"documentChanges":[]}`, `{"documentChanges":[{"kind":"create","uri":"file:///project/main.go"}]}`,
		`{"documentChanges":[{"textDocument":{"uri":"file:///project/main.go"},"edits":[]}]}`,
		`{"changes":{"file:///project/main.go":[],"file:///project/./main.go":[]}}`,
		changes("file:///project/main.go", strings.Replace(edit, `"character":3`, `"character":2`, 1)),
		changes("file:///project/main.go", strings.Replace(edit, `"line":0`, `"line":50`, 1)),
		changes("file:///project/main.go", strings.Replace(edit, `"character":1`, `"character":50`, 1)),
		changes("file:///project/main.go", `{"range":{},"newText":"x"}`),
	} {
		if _, err := l.saveWorkspaceEdit(json.RawMessage(raw), q); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestSourceActionDecodeLimitsBeforeBuildingActionSlice(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `[{"title":"Fix","kind":"source.fixAll"}]`} {
		if _, err := parseSourceActions(json.RawMessage(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{
		`[` + strings.Repeat(`{},`, 64) + `{}]`,
		`[{"title":"` + strings.Repeat("x", 1001) + `","kind":"source.fixAll"}]`,
		`[{"title":"Fix","kind":"` + strings.Repeat("x", 129) + `"}]`,
		`[null]`, `{}[]`,
	} {
		if _, err := parseSourceActions(json.RawMessage(raw)); err == nil {
			t.Fatal("accepted", raw[:min(len(raw), 100)])
		}
	}
}

func TestCommandWorkspaceEditVersionsAndMultipleFileSnapshots(t *testing.T) {
	posixFixtures(t)
	l, s, d, _ := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	if !l.ensureSynchronized(d, s) {
		t.Fatal("sync failed")
	}
	d2 := &document{id: 2, version: 23, lspVer: 8, lang: "Go", path: "/project/other.go", text: []byte("second"), server: s, syncedServer: s, syncedVersion: 23}
	l.mu.Lock()
	l.docs[2] = d2
	l.byPath[d2.path] = d2
	l.mu.Unlock()
	group := func(uri string, version int) string {
		return fmt.Sprintf(`{"textDocument":{"uri":%q,"version":%d},"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"x"}]}`, uri, version)
	}
	raw := `{"documentChanges":[` + group(fileURI(d.path), 1) + `,` + group(fileURI(d2.path), 8) + `]}`
	converted, err := l.commandWorkspaceEdit(json.RawMessage(raw), s)
	if err != nil || len(converted.Files) != 2 || converted.Files[0].Version != 1 || converted.Files[1].Version != 23 {
		t.Fatal(converted, err)
	}
	for _, raw := range []string{
		`{"documentChanges":[` + group(fileURI(d2.path), 23) + `]}`,
		`{"documentChanges":[` + group(fileURI(d.path), 1) + `,` + group(fileURI(d.path), 1) + `]}`,
	} {
		if _, err := l.commandWorkspaceEdit(json.RawMessage(raw), s); err == nil {
			t.Fatal("accepted", raw)
		}
	}
}

func TestDeferredCommandEditKeepsPreResolveForeignSnapshot(t *testing.T) {
	posixFixtures(t)
	l, s, d, _ := syncHarness(t, synchronizationOptions{openClose: true, change: 2})
	if !l.ensureSynchronized(d, s) {
		t.Fatal("sync failed")
	}
	origin, _, _ := l.current(d.id, d.version)
	foreign := &document{id: 2, path: "/project/other.go", text: []byte("other\n"), version: 23}
	l.mu.Lock()
	l.docs[2] = foreign
	l.byPath[foreign.path] = foreign
	l.mu.Unlock()
	queries := l.commandQuerySnapshots(s, origin)
	l.mu.Lock()
	foreign.text = []byte("zother\n")
	foreign.version = 24
	l.mu.Unlock()
	raw := json.RawMessage(`{"changes":{"file:///project/./other.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}},"newText":"[R]"}]}}`)
	edit, err := l.commandWorkspaceEdit(raw, s, queries...)
	if err != nil || len(edit.Files) != 1 || edit.Files[0].Version != 23 || edit.Files[0].Edits[0].Range.End.Col != 6 {
		t.Fatal(edit, err)
	}
	unknown := json.RawMessage(`{"changes":{"file:///project/unopened.go":[]}}`)
	if _, err := l.commandWorkspaceEdit(unknown, s, queries...); err == nil {
		t.Fatal("unsnapshotted target accepted")
	}
}
