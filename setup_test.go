package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func TestSetupCoversEveryServer(t *testing.T) {
	programs := readmeAlternatives(t)
	for lang, argv := range servers {
		programs = append(programs, argv[0])
		for _, platform := range []string{"darwin", "linux", "windows"} {
			guide := setupMarkdown(lang, argv, platform, false)
			if !strings.Contains(guide, "Official installation instructions") || !strings.Contains(guide, argv[0]) {
				t.Errorf("%s on %s has no installation instructions", lang, platform)
			}
		}
	}
	for _, program := range programs {
		g, ok := installGuides[program]
		if !ok || g.prerequisites == "" {
			t.Errorf("%s has no installation guide", program)
		}
		u, err := url.Parse(g.url)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s has no official HTTPS link: %q", program, g.url)
		}
	}
}

// Presentation requests share the SDK's event loop with notification clicks and commands.
// Once a lookup fails, they must stop waiting for a server that cannot become ready.
func TestMissingServerDoesNotHoldSetupBehindPresentation(t *testing.T) {
	for _, delay := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			d := &document{id: 1, version: 1, lang: "Go"}
			l := &lsp{docs: map[rhunext.Doc]*document{1: d}, servers: map[string]*server{}}
			if delay == 0 {
				l.servers["Go"] = nil
			} else {
				go func() {
					time.Sleep(delay)
					l.mu.Lock()
					l.servers["Go"] = nil
					l.mu.Unlock()
				}()
			}
			start := time.Now()
			q, _, s := l.currentSoon(1, 1, time.Second)
			if q != nil || s != nil || time.Since(start) > 600*time.Millisecond {
				t.Fatalf("missing server kept the event loop waiting for %s", time.Since(start))
			}
		})
	}
}

func TestSetupRespectsPlatformAndConfiguredServer(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		guide := setupMarkdown("Lua", []string{"lua-language-server"}, platform, false)
		if strings.Contains(guide, "brew install") {
			t.Errorf("offered Homebrew on %s", platform)
		}
	}
	if guide := setupMarkdown("Lua", []string{"lua-language-server"}, "darwin", false); !strings.Contains(guide, "brew install lua-language-server") {
		t.Fatal(guide)
	}
	guide := setupMarkdown("Python", []string{"ruff", "server"}, "linux", true)
	if !strings.Contains(guide, "uv tool install ruff") || strings.Contains(guide, "install -g pyright") || strings.Contains(guide, "Executable missing") {
		t.Fatal(guide)
	}
	guide = setupMarkdown("Python", []string{"/custom/my`server", "--stdio"}, "linux", false)
	if !strings.Contains(guide, "custom server command") || strings.Contains(guide, "install -g pyright") || !strings.Contains(guide, "`` /custom/my`server --stdio ``") {
		t.Fatal(guide)
	}
	guide = setupMarkdown("Python", []string{}, "linux", false)
	if !strings.Contains(guide, "disabled") || strings.Contains(guide, "Run in your terminal") {
		t.Fatal(guide)
	}
	guide = setupMarkdown("Nix", []string{"nil"}, "windows", false)
	if strings.Contains(guide, "nix profile install") || !strings.Contains(guide, "Linux environment") {
		t.Fatal(guide)
	}
	if got := serverProgram("pyright-langserver.CMD"); got != "pyright-langserver" {
		t.Fatal(got)
	}
}

func TestSetupDoesNotEnableDisabledServers(t *testing.T) {
	l := &lsp{servers: map[string]*server{}, config: map[string][]string{"Go": {}}}
	if s := l.serverFor("Go"); s != nil {
		t.Fatal("started a disabled server")
	}
	if s := l.serverFor("unsupported"); s != nil {
		t.Fatal("started an unsupported server")
	}
}

func TestSetupCreatesSettingsWithoutOverwriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lsp.ini")
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfig(path); err != nil {
		t.Fatal(err)
	}
	const custom = "[servers]\nPython = ruff server\n"
	if err := os.WriteFile(path, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != custom {
		t.Fatal("overwrote existing server settings")
	}
}
