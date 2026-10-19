package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSettingsMergeAndOptions(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	data := `{"defaults":{"settings":{"x":{"a":1,"b":2},"keep":true},"initializationOptions":{"default":true}},"languages":{"Go":{"settings":{"x":{"b":null,"c":3}},"initializationOptions":{"go":true}}}}`
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := readSettings(file)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.forLanguage("Go")
	encoded, _ := json.Marshal(got.Settings)
	if string(encoded) != `{"keep":true,"x":{"a":1,"b":null,"c":3}}` {
		t.Fatal(string(encoded))
	}
	if string(got.InitializationOptions) != `{"go":true}` {
		t.Fatal(string(got.InitializationOptions))
	}
	base, _ := json.Marshal(cfg.forLanguage("Rust").Settings)
	if string(base) != `{"keep":true,"x":{"a":1,"b":2}}` {
		t.Fatal(string(base))
	}
}

func TestSettingsRejectMalformedAndUnknownFields(t *testing.T) {
	for _, body := range []string{``, `null`, `{`, `{} {}`, `{"languages":{"Go":{"settings":[]}}}`, `{"mispelled":true}`} {
		file := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(file, []byte(body), 0600)
		if _, err := readSettings(file); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if _, err := readSettings(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatal(err)
	}
}

func TestServerCommandValidation(t *testing.T) {
	for _, body := range []string{"[servers]\nGo =\n", "[servers]\nGo = []\n", "[servers]\nGo = [\"gopls\",null]\n", "Go = gopls\n", "[servers]\nmissing equals\n", "[unknown]\nx = y\n"} {
		path := filepath.Join(t.TempDir(), "lsp.ini")
		os.WriteFile(path, []byte(body), 0600)
		if _, err := readConfig(path); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	path := filepath.Join(t.TempDir(), "lsp.ini")
	os.WriteFile(path, []byte("[servers]\nGo = gopls -remote=auto\nRust = off\nC = [\"path with spaces\",\"--stdio\"]\n"), 0600)
	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"Go": {"gopls", "-remote=auto"}, "Rust": {}, "C": {"path with spaces", "--stdio"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%#v", got)
	}
}

func TestDottedConfigurationLookup(t *testing.T) {
	cfg := map[string]any{"gopls": map[string]any{"hints": map[string]any{"x": true}}, "a.b": "exact"}
	if value := configurationSection(cfg, "gopls.hints.x"); value != true {
		t.Fatal(value)
	}
	if value := configurationSection(cfg, "a.b"); value != "exact" {
		t.Fatal(value)
	}
	if value := configurationSection(cfg, "missing.x"); value != nil {
		t.Fatal(value)
	}
}

func TestFileURIRoundTrip(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "a b.go"), filepath.Join(t.TempDir(), "héllo.go")} {
		if got := uriPath(fileURI(path)); got != path {
			t.Fatalf("%q => %q", path, got)
		}
	}
	if got := fileURI("C:/Users/name/a b.go"); got != "file:///C:/Users/name/a%20b.go" {
		t.Fatal(got)
	}
	if got := uriPath("file:///C:/Users/name/a%20b.go"); filepath.ToSlash(got) != "C:/Users/name/a b.go" {
		t.Fatal(got)
	}
}
