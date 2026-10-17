package main

import (
	"encoding/json"
	"testing"
)

func TestPlainSnippet(t *testing.T) {
	for in, want := range map[string]string{
		"for ${1:i} := 0; $1 < ${2:n}; $1++ {\n\t$0\n}": "for i := 0; i < n; i++ {\n\t\n}",
		"println!(\"${1}\")":                            "println!(\"\")",
		"${1|one,two|}":                                 "one",
		"a \\$ b":                                       "a $ b",
		"${1:outer ${2:inner}}":                         "outer inner",
		"héllo$0":                                       "héllo",
	} {
		if got := plainSnippet(in); got != want {
			t.Errorf("plainSnippet(%q) = %q, want %q", in, got, want)
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
