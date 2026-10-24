package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// helpers are programs the servers run themselves (README, Needs)
var helpers = []string{"@build", "getconf", "nix", "git", "xcode-select", "xcrun", "tr", "echo", "zsh", "clang", "cc", "ld", "ar", "bash", "uname", "dirname", "basename", "readlink", "which", "sed", "grep", "cut", "ls", "xargs", "java", "kotlinc", "dune", "ghc", "ghc-pkg", "cabal", "stack", "elixir", "erl", "mix", "zig", "swift", "swiftc", "node", "go", "cargo", "rustc"}

// manifestRuns is extension.ini's [needs] run list
func manifestRuns(t *testing.T) map[string]bool {
	data, err := os.ReadFile("extension.ini")
	if err != nil {
		t.Fatal(err)
	}
	section := ""
	runs := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if section == "[needs]" && ok && strings.TrimSpace(key) == "run" {
			for _, name := range strings.Fields(value) {
				runs[name] = true
			}
		}
	}
	if len(runs) == 0 {
		t.Fatal("no run list in extension.ini")
	}
	return runs
}

// readmeAlternatives are the programs of the README table's alternatives column
func readmeAlternatives(t *testing.T) []string {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var programs []string
	code := regexp.MustCompile("`([^`]+)`")
	table := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "| Language | Server | Alternatives") {
			table = true
			continue
		}
		if table && !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(line, "|")
		if !table || len(cells) < 4 {
			continue
		}
		for _, m := range code.FindAllStringSubmatch(cells[3], -1) {
			programs = append(programs, strings.Fields(m[1])[0])
		}
	}
	if len(programs) == 0 {
		t.Fatal("no alternatives in the README's server table")
	}
	return programs
}

// The sandbox starts only what the run list names: every default server and every alternative the
// README offers for lsp.ini must be on it, and it names nothing else.
func TestRunListNamesEveryServer(t *testing.T) {
	runs := manifestRuns(t)
	known := map[string]bool{}
	for lang, argv := range servers {
		known[argv[0]] = true
		if !runs[argv[0]] {
			t.Errorf("%s's default server %s is not in the run list", lang, argv[0])
		}
	}
	for _, program := range readmeAlternatives(t) {
		known[program] = true
		if !runs[program] {
			t.Errorf("the README's alternative %s is not in the run list", program)
		}
	}
	for _, program := range helpers {
		known[program] = true
		if !runs[program] {
			t.Errorf("required helper %s is not in the run list", program)
		}
	}
	for name := range runs {
		if !known[name] {
			t.Errorf("the run list names %s, which is no server, alternative or helper", name)
		}
	}
}
