package main

import (
	"encoding/json"
	"testing"
)

func TestPlainSnippet(t *testing.T) {
	for _, c := range []struct {
		in, want string
		cursor   int
	}{
		{"for ${1:i} := 0; $1 < ${2:n}; $1++ {\n\t$0\n}", "for i := 0; i < n; i++ {\n\t\n}", 4},
		{"println!(\"${1}\")", "println!(\"\")", 10},
		{"${1|one,two|}", "one", 0},
		{"a \\$ b", "a $ b", -1},
		{"${1:outer ${2:inner}}", "outer inner", 0},
		{"héllo$0", "héllo", 6},
		{"Println($0)", "Println()", 8},
	} {
		got, cursor := plainSnippet(c.in)
		if got != c.want || cursor != c.cursor {
			t.Errorf("plainSnippet(%q) = %q %d, want %q %d", c.in, got, cursor, c.want, c.cursor)
		}
	}
}

func TestParamBytes(t *testing.T) {
	label := "func Println(a ...any) (n int, err error)"
	if a, b := paramBytes(label, json.RawMessage(`"a ...any"`)); label[a:b] != "a ...any" {
		t.Fatalf("by name: %d %d", a, b)
	}
	if a, b := paramBytes(label, json.RawMessage(`[13, 21]`)); label[a:b] != "a ...any" {
		t.Fatalf("by span: %d %d", a, b)
	}
	if a, b := paramBytes("f(é, x)", json.RawMessage(`[5, 6]`)); a != 6 || b != 7 {
		t.Fatalf("utf-16 span: %d %d", a, b)
	}
}

func TestDocText(t *testing.T) {
	if got := docText(json.RawMessage(`{"kind":"markdown","value":"**Bold** text"}`)); got == "" {
		t.Fatal("markdown")
	}
	if got := docText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Fatal(got)
	}
}
