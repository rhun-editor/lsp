package main

// Provider scope (feature provider-scope): the manifest's [provides] keys name every language
// the extension knows; as servers start, initialize, register or remove services and stop, the
// extension tells rhun which file patterns each kind is really served for ("provides KEY
// VALUES"), so rhun asks it only where a server can answer. A language whose server has not
// started yet keeps its patterns (it starts when such a document opens); one whose server is
// missing, turned off or gone loses them; a running server keeps the kinds it supports. The
// on-type formatting triggers are those the running servers declared (only a running server is
// asked to format as you type).

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// featureMarkdown is the presentation feature under which rhun renders Markdown documentation
const featureMarkdown = "markdown"

// manifestPatterns are the file patterns of every [provides] kind in extension.ini, in its order
var manifestPatterns = []string{"*.go", "*.rs", "*.ts", "*.tsx", "*.js", "*.jsx", "*.mjs", "*.cjs", "*.py",
	"*.c", "*.h", "*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hh", "*.zig", "*.lua", "*.rb", "*.dart", "*.kt", "*.kts",
	"*.swift", "*.ml", "*.mli", "*.hs", "*.ex", "*.exs", "*.nix", "*.tf", "*.yaml", "*.yml", "*.json", "*.html",
	"*.htm", "*.css"}

// manifestTypeTriggers are type_format.triggers in extension.ini: the characters servers format on
var manifestTypeTriggers = []string{"newline", ".", "=", "<", ">", "{", "(", "|", "+", "}", ";"}

// languagePatterns are the manifest's patterns of each language (a header serves C and C++)
var languagePatterns = map[string][]string{
	"Go": {"*.go"}, "Rust": {"*.rs"}, "TypeScript": {"*.ts", "*.tsx"}, "JavaScript": {"*.js", "*.jsx", "*.mjs", "*.cjs"},
	"Python": {"*.py"}, "C": {"*.c", "*.h"}, "C++": {"*.cc", "*.cpp", "*.cxx", "*.hpp", "*.hh", "*.h"},
	"Zig": {"*.zig"}, "Lua": {"*.lua"}, "Ruby": {"*.rb"}, "Dart": {"*.dart"}, "Kotlin": {"*.kt", "*.kts"},
	"Swift": {"*.swift"}, "OCaml": {"*.ml", "*.mli"}, "Haskell": {"*.hs"}, "Elixir": {"*.ex", "*.exs"},
	"Nix": {"*.nix"}, "Terraform": {"*.tf"}, "YAML": {"*.yaml", "*.yml"}, "JSON": {"*.json"},
	"HTML": {"*.html", "*.htm"}, "CSS": {"*.css"},
}

// scopeKey is a [provides] key, the server method that serves it and the feature rhun must have
// agreed for the key to mean anything (any of them)
type scopeKey struct {
	key, method string
	features    []string
}

var scopeKeys = []scopeKey{
	{"complete", "textDocument/completion", []string{rhunext.FeatureComplete}},
	{"signature", "textDocument/signatureHelp", []string{rhunext.FeatureSignature, rhunext.FeatureSignatureList}},
	{"definition", "textDocument/definition", []string{rhunext.FeatureDefinition}},
	{"references", "textDocument/references", []string{rhunext.FeatureReferences}},
	{"symbols", "textDocument/documentSymbol", []string{rhunext.FeatureSymbols}},
	{"actions", "textDocument/codeAction", []string{rhunext.FeatureActions, rhunext.FeatureCodeActions}},
	{"rename", "textDocument/rename", []string{rhunext.FeatureRename}},
	{"format", "textDocument/formatting", []string{rhunext.FeatureFormat}},
	{"save_actions", "textDocument/codeAction", []string{rhunext.FeatureSaveActions}},
	{"hover", "textDocument/hover", []string{rhunext.FeatureHoverQuery}},
	{"declaration", "textDocument/declaration", []string{rhunext.FeatureNavigation}},
	{"type_definition", "textDocument/typeDefinition", []string{rhunext.FeatureNavigation}},
	{"implementation", "textDocument/implementation", []string{rhunext.FeatureNavigation}},
	{"selection_ranges", "textDocument/selectionRange", []string{rhunext.FeatureSelectionRanges}},
	{"call_hierarchy", "textDocument/prepareCallHierarchy", []string{rhunext.FeatureHierarchy}},
	{"type_hierarchy", "textDocument/prepareTypeHierarchy", []string{rhunext.FeatureHierarchy}},
	{"workspace_symbols", "workspace/symbol", []string{rhunext.FeatureWorkspaceSymbols}},
	{"range_format", "textDocument/rangeFormatting", []string{rhunext.FeaturePartialFormat}},
	{"type_format", "textDocument/onTypeFormatting", []string{rhunext.FeaturePartialFormat}},
	{"document_links", "textDocument/documentLink", []string{rhunext.FeatureDocumentLinks}},
}

// languageState is what the extension knows of a language's server for the scope
type languageState int

const (
	langUnstarted languageState = iota // no document of it opened yet: it may serve
	langStarting                       // started, its capabilities unknown: it may serve
	langRunning                        // initialized: it serves what it declared
	langNone                           // missing, turned off or gone
)

// scopeServer is a language's state for the scope, with its server when it runs
type scopeServer struct {
	state  languageState
	server *server
}

// scopeValues is the value of each key the features rhun agreed name: "*" for the manifest's,
// "-" for none, else the patterns (or triggers) in the manifest's order
func (l *lsp) scopeValues(langs map[string]scopeServer) map[string]string {
	out := map[string]string{}
	agreed := func(k scopeKey) bool {
		for _, f := range k.features {
			if l.has(f) {
				return true
			}
		}
		return false
	}
	probe := func(lang, pattern string) selectorDocument {
		dir := l.project
		if dir == "" || dir == "-" {
			dir = string(filepath.Separator)
		}
		path := filepath.Join(dir, "probe"+strings.TrimPrefix(pattern, "*"))
		return selectorDocument{language: languageIDs[lang], scheme: "file", path: filepath.ToSlash(filepath.Clean(path))}
	}
	for _, k := range scopeKeys {
		if !agreed(k) {
			continue
		}
		var keep []string
		triggers := map[string]bool{}
		for _, pattern := range manifestPatterns {
			served := false
			for lang, patterns := range languagePatterns {
				if !slices.Contains(patterns, pattern) {
					continue
				}
				st := langs[lang]
				switch st.state {
				case langUnstarted, langStarting:
					served = true
				case langRunning:
					doc := probe(lang, pattern)
					if st.server.supports(k.method, doc) {
						served = true
						if k.key == "type_format" {
							for _, c := range st.server.onTypeTriggers(doc) {
								if c == "\n" {
									c = "newline"
								}
								triggers[c] = true
							}
						}
					}
				}
			}
			if served {
				keep = append(keep, pattern)
			}
		}
		out[k.key] = scopeValue(keep, manifestPatterns)
		if k.key == "type_format" {
			// only running servers ask for on-type formatting: theirs are the triggers
			var chars []string
			for _, c := range manifestTypeTriggers {
				if triggers[c] {
					chars = append(chars, c)
				}
			}
			out["type_format.triggers"] = scopeValue(chars, manifestTypeTriggers)
		}
	}
	return out
}

func scopeValue(keep, all []string) string {
	switch len(keep) {
	case 0:
		return "-"
	case len(all):
		return "*"
	}
	return strings.Join(keep, " ")
}

// scopeLanguages is every language's server state now
func (l *lsp) scopeLanguages() map[string]scopeServer {
	l.mu.Lock()
	defer l.mu.Unlock()
	langs := map[string]scopeServer{}
	for lang := range languagePatterns {
		s, started := l.servers[lang]
		switch {
		case !started:
			argv := l.config[lang]
			if argv == nil {
				argv = servers[lang]
			}
			if len(argv) == 0 || !l.installed(argv[0]) {
				langs[lang] = scopeServer{state: langNone}
			} else {
				langs[lang] = scopeServer{state: langUnstarted}
			}
		case s == nil || s.ended.Load():
			langs[lang] = scopeServer{state: langNone}
		case s.ready.Load():
			langs[lang] = scopeServer{state: langRunning, server: s}
		default:
			langs[lang] = scopeServer{state: langStarting}
		}
	}
	return langs
}

// installed is whether a server's program is on PATH, remembered until the configuration changes.
// Called with l.mu held.
func (l *lsp) installed(program string) bool {
	if l.found == nil || l.foundGeneration != l.configGeneration {
		l.found = map[string]bool{}
		l.foundGeneration = l.configGeneration
	}
	ok, seen := l.found[program]
	if !seen {
		_, err := exec.LookPath(program)
		ok = err == nil
		l.found[program] = ok
	}
	return ok
}

// scopeChanged asks the scope worker to tell rhun the values in force now
func (l *lsp) scopeChanged() {
	if l.scopeWake == nil {
		return
	}
	select {
	case l.scopeWake <- struct{}{}:
	default:
	}
}

// startScopeWorker sends provides requests one at a time, only for keys whose value changed
func (l *lsp) startScopeWorker() {
	if !l.has(rhunext.FeatureProviderScope) {
		return
	}
	l.scopeWake = make(chan struct{}, 1)
	sent := map[string]string{}
	done := l.statusDone
	go func() {
		for {
			select {
			case <-done:
				return
			case <-l.scopeWake:
			}
			want := l.scopeValues(l.scopeLanguages())
			for _, k := range append(scopeKeyNames(), "type_format.triggers") {
				v, ok := want[k]
				if !ok || sent[k] == v || (sent[k] == "" && v == "*") {
					continue
				}
				var err error
				switch v {
				case "*":
					err = l.ext.ProvidesManifest(k)
				case "-":
					err = l.ext.Withdraw(k)
				default:
					err = l.ext.Provides(k, strings.Fields(v)...)
				}
				if err != nil {
					l.ext.Logf("provides %s %s: %v", k, v, err)
					continue
				}
				sent[k] = v
			}
		}
	}()
	l.scopeChanged()
}

func scopeKeyNames() []string {
	names := make([]string, len(scopeKeys))
	for i, k := range scopeKeys {
		names[i] = k.key
	}
	return names
}
