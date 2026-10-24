package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func TestNavigationLocationsNormalizeAliasesAcrossPartsAndFinal(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "main file.go"))
	uri := fileURI(path)
	alias := strings.Replace(uri, "/main%20file.go", "//main%20file.go", 1)
	l := &lsp{}
	query := &documentQuery{path: path, text: []byte("😀total\nother\n")}
	span := lspRange{Start: lspPosition{Line: 0, Character: 2}, End: lspPosition{Line: 0, Character: 7}}
	link := map[string]any{"targetUri": alias, "targetRange": span, "targetSelectionRange": span}
	loc := map[string]any{"uri": uri, "range": span}
	other := map[string]any{"uri": uri, "range": lspRange{Start: lspPosition{Line: 1}, End: lspPosition{Line: 1, Character: 5}}}
	raw := func(items ...any) json.RawMessage {
		data, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	convert := l.locationConverter(query, true)
	first := convert(raw(link))
	if len(first) != 1 || first[0].Target.Path != path || first[0].Range.Start != (rhunext.Pos{Line: 1, Col: 5}) || first[0].Range.End.Col != 10 {
		t.Fatal(first)
	}
	final := convert(raw(loc, link, other))
	if len(final) != 1 || final[0].Range.Start.Line != 2 {
		t.Fatal(final)
	}
	if got := l.locationConverter(query, true)(raw(link, loc)); len(got) != 1 {
		t.Fatal("new navigation request did not retain its own target", got)
	}
	if got := l.locationConverter(query, false)(raw(link, loc)); len(got) != 2 {
		t.Fatal("reference occurrences changed", got)
	}
}
