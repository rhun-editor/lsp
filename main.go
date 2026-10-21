// lsp: language servers for rhun. One server per language per project (gopls, rust-analyzer,
// typescript-language-server, pyright, clangd, …), started when a document of the language opens;
// diagnostics in the gutter, hover text when the pointer rests, completion lists as you type and
// signature help inside a call (complete.go), and definitions, references, symbols, code actions,
// rename and format (lang.go).
// Documents are kept in step with rhun through the edits stream (section 6a of the protocol) and
// sent to the server incrementally. Settings: $RHUN_STATE/lsp.ini.
// Protocol: https://rhun.app/extensions/v1/
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// servers by grammar name (spaces as underscores), overridable in lsp.ini's [servers]
var servers = map[string][]string{
	"Go":         {"gopls"},
	"Rust":       {"rust-analyzer"},
	"TypeScript": {"typescript-language-server", "--stdio"},
	"JavaScript": {"typescript-language-server", "--stdio"},
	"Python":     {"pyright-langserver", "--stdio"},
	"C":          {"clangd"},
	"C++":        {"clangd"},
	"Zig":        {"zls"},
	"Lua":        {"lua-language-server"},
	"Ruby":       {"solargraph", "stdio"},
	"Dart":       {"dart", "language-server"},
	"Kotlin":     {"kotlin-language-server"},
	"Swift":      {"sourcekit-lsp"},
	"OCaml":      {"ocamllsp"},
	"Haskell":    {"haskell-language-server-wrapper", "--lsp"},
	"Elixir":     {"elixir-ls"},
	"Nix":        {"nil"},
	"Terraform":  {"terraform-ls", "serve"},
	"YAML":       {"yaml-language-server", "--stdio"},
	"JSON":       {"vscode-json-language-server", "--stdio"},
	"HTML":       {"vscode-html-language-server", "--stdio"},
	"CSS":        {"vscode-css-language-server", "--stdio"},
}

var languageIDs = map[string]string{
	"Go": "go", "Rust": "rust", "TypeScript": "typescript", "JavaScript": "javascript", "Python": "python",
	"C": "c", "C++": "cpp", "Zig": "zig", "Lua": "lua", "Ruby": "ruby", "Dart": "dart", "Kotlin": "kotlin",
	"Swift": "swift", "OCaml": "ocaml", "Haskell": "haskell", "Elixir": "elixir", "Nix": "nix",
	"Terraform": "terraform", "YAML": "yaml", "JSON": "json", "HTML": "html", "CSS": "css",
}

// ---- documents

type document struct {
	id            rhunext.Doc
	version       int64
	lang, path    string
	text          []byte
	server        *server
	syncMu        sync.Mutex
	lspVer        int
	syncedServer  *server
	syncedVersion int64
	opened        bool
	syncChange    int
	fetching      bool
	awaiting      *server
	pendingSave   bool
	saveVersion   int64
}

// ---- the extension

type lsp struct {
	ext                            *rhunext.Ext
	fetchText                      func(rhunext.Doc) ([]byte, int64, error)
	project                        string
	watcher                        *workspaceWatcher
	state                          string
	mu                             sync.Mutex
	docs                           map[rhunext.Doc]*document
	byPath                         map[string]*document
	servers                        map[string]*server // by language
	commandGates                   map[string]chan struct{}
	settings                       *settingsConfig
	configGeneration               uint64
	statusMu                       sync.Mutex
	statusActive                   map[string]uint64
	progressTokens                 map[uint64]map[string]*progressEntry
	statusGeneration, statusSerial uint64
	statusLatest                   string
	statusWake                     chan struct{}
	statusDone                     chan struct{}
	statusOnce                     sync.Once
	stopping                       bool
	config                         map[string][]string
	diags                          map[string]*keptDiagnostics // the server's diagnostics by path (diagnostics.go)
	scopeWake                      chan struct{}               // provider scope (scope.go)
	found                          map[string]bool
	foundGeneration                uint64
	actionsMu                      sync.Mutex // code actions waiting to be chosen (codeactions.go)
	actionSerial                   uint64
	actionSets                     map[uint64][]actionData
	diagOwner                      map[string]*server // which server's set rhun has for a file (diagnostics.go)
	diagMu                         sync.Mutex
	diagJobs                       []diagnosticsJob
	diagRunning                    bool
	pullMu                         sync.Mutex
	pulls                          map[*document]*pullState
	execReady                      atomic.Bool    // the exec command is registered (codeactions.go)
	pending                        pendingFileOps // file operations of edits answered to rhun
}

func (l *lsp) Ready(ext *rhunext.Ext, project string) {
	l.ext = ext
	l.project = project
	l.state = os.Getenv("RHUN_STATE")
	l.docs = map[rhunext.Doc]*document{}
	l.byPath = map[string]*document{}
	l.servers = map[string]*server{}
	l.diags = map[string]*keptDiagnostics{}
	l.startStatusWorker()
	l.startScopeWorker()
	var err error
	l.config, err = readConfig(filepath.Join(l.state, "lsp.ini"))
	if err != nil {
		l.config = map[string][]string{}
		l.configError(err)
	}
	l.startWorkspaceWatcher()
	l.settings, err = readSettings(filepath.Join(l.state, "settings.json"))
	if err != nil {
		l.settings = &settingsConfig{}
		l.configError(err)
	}
	l.registerExec()
}

func (l *lsp) Event(ext *rhunext.Ext, ev rhunext.Event) {
	l.workspaceWatchEvent(ev)
	l.documentEvent(ev)
	l.fileOpEvent(ev)
}

func (l *lsp) Run(ext *rhunext.Ext, command string, doc rhunext.Doc) {
	switch command {
	case "lsp.reload_configuration":
		if err := l.reloadSettings(); err != nil {
			l.configError(err)
		} else {
			ext.Toast("Language server settings reloaded. Restart to apply initialization options.")
		}
	case "lsp.cancel_progress":
		if l.cancelProgress() == 0 {
			ext.Toast("No cancellable language server work")
		}
	case "lsp.restart":
		commands, err := readConfig(filepath.Join(l.state, "lsp.ini"))
		if err != nil {
			l.configError(err)
			return
		}
		settings, err := readSettings(filepath.Join(l.state, "settings.json"))
		if err != nil {
			l.configError(err)
			return
		}
		l.mu.Lock()
		l.config, l.settings = commands, settings
		l.configGeneration++
		var oldServers []*server
		for lang, s := range l.servers {
			if s != nil {
				l.retireStatus(s)
				oldServers = append(oldServers, s)
			}
			delete(l.servers, lang)
		}
		docs := make([]*document, 0, len(l.docs))
		for _, d := range l.docs {
			d.server = nil
			d.syncedServer = nil
			d.opened = false
			docs = append(docs, d)
		}
		l.mu.Unlock()
		for _, s := range oldServers {
			s.stop()
		}
		for _, d := range docs {
			go l.open(d)
		}
		l.scopeChanged()
		ext.Toast("Language servers restarted")
	}
}

func (l *lsp) open(d *document)   { l.fetchDocument(d) }
func (l *lsp) resync(d *document) { l.fetchDocument(d) }

// serverFor returns the running server for a language, starting it on first use; nil when
// there is none for the language or it cannot start. Called with l.mu held.
func (l *lsp) serverFor(lang string) *server {
	if l.stopping {
		return nil
	}
	if s, ok := l.servers[lang]; ok {
		return s
	}
	argv := l.config[lang]
	if argv == nil {
		argv = servers[lang]
	}
	defer l.scopeChanged()
	if len(argv) == 0 {
		l.servers[lang] = nil
		return nil
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		l.statusMu.Lock()
		l.publishStatusLocked(filepath.Base(argv[0]) + " not installed")
		l.statusMu.Unlock()
		l.servers[lang] = nil
		return nil
	}
	s := newServer(l, lang, argv)
	l.servers[lang] = s
	go s.start()
	return s
}

// ---- a language server

type server struct {
	l                  *lsp
	lang               string
	argv               []string
	cmd                *exec.Cmd
	in                 io.WriteCloser
	out                io.ReadCloser
	writeGate          chan struct{}
	cancelOnce         sync.Once
	cancels            chan int
	waitDone           chan struct{}
	processExited      atomic.Bool
	nextID             int
	pmu                sync.Mutex
	pending            map[int]chan json.RawMessage
	ready              atomic.Bool
	ended              atomic.Bool
	stateMu            sync.Mutex
	configMu           sync.Mutex
	done               chan struct{}
	generation         uint64
	readyCh            chan struct{}
	capMu              sync.RWMutex
	staticCapabilities map[string]bool
	staticOptions      map[string]bool // providerOption of each static provider
	staticTriggers     []string        // on-type formatting's
	registrations      map[string]capabilityRegistration
	syncOptions        synchronizationOptions
	syncGeneration     uint64
	watchGeneration    uint64
	commandMu          sync.Mutex
	commandCapture     *commandCapture
	commandUsed        atomic.Bool
	tokenMu            sync.Mutex
	feeds              map[string]*queryFeed     // the extension's request tokens (stream.go)
	fileOpFilters      map[string][]fileOpFilter // workspace.fileOperations: create, rename, delete
}

type location struct {
	URI                  string    `json:"uri"`
	Range                lspRange  `json:"range"`
	TargetURI            string    `json:"targetUri"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}
type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

func newServer(l *lsp, lang string, argv []string) *server {
	s := &server{l: l, lang: lang, argv: argv, pending: map[int]chan json.RawMessage{}, readyCh: make(chan struct{}), done: make(chan struct{}), writeGate: make(chan struct{}, 1), cancels: make(chan int, 128), waitDone: make(chan struct{}), registrations: map[string]capabilityRegistration{}}
	l.activateStatus(s)
	return s
}

func (s *server) start() {
	defer func() {
		if !s.ready.Load() {
			s.endServer("")
		}
		close(s.readyCh)
	}()
	out, err := s.launch()
	if err != nil {
		s.endServer(s.name() + ": " + err.Error())
		return
	}
	s.status(s.name()+": starting", false)
	go s.read(bufio.NewReaderSize(out, maxRPCHeader))
	s.l.mu.Lock()
	options := s.l.settings.forLanguage(s.lang).InitializationOptions
	folders := s.l.workspaceFolders()
	var root any
	if s.l.project != "" && s.l.project != "-" {
		root = fileURI(s.l.project)
	}
	s.l.mu.Unlock()
	initializeParams := map[string]any{
		"processId": os.Getpid(), "rootUri": root,
		"workspaceFolders":      folders,
		"initializationOptions": options,
		"capabilities":          s.l.clientCapabilities(s.l.workspaceWatchAvailable()),
	}
	res, err := s.call("initialize", initializeParams)
	if err != nil {
		s.endServer(s.name() + ": " + err.Error())
		return
	}
	if err := s.loadSynchronization(res); err != nil {
		s.endServer(s.name() + ": " + err.Error())
		return
	}
	if err := s.loadCapabilities(res); err != nil {
		s.endServer(s.name() + ": " + err.Error())
		return
	}
	s.notify("initialized", map[string]any{})
	if err := s.publishReady(); err != nil {
		s.endServer(s.name() + ": " + err.Error())
		return
	}
	s.status(s.name(), false)
	s.l.scopeChanged()
}

func (s *server) waitReady() { <-s.readyCh }

func (s *server) name() string {
	if len(s.argv) == 0 {
		return s.lang
	}
	return filepath.Base(s.argv[0])
}

func (s *server) stop() { s.endServer("") }

func (s *server) read(br *bufio.Reader) {
	defer s.endServer("")
	for {
		body, err := readRPC(br)
		if err != nil {
			if err != io.EOF && !s.ended.Load() {
				s.endServer(s.name() + ": " + err.Error())
			}
			return
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil {
			s.endServer(s.name() + ": invalid JSON-RPC message")
			return
		}
		msg := struct {
			ID, Params json.RawMessage
			Method     string
		}{ID: envelope["id"], Params: envelope["params"]}
		if raw, ok := envelope["method"]; ok {
			json.Unmarshal(raw, &msg.Method)
		}
		switch {
		case msg.Method == "" && len(msg.ID) > 0:
			var id int
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			s.closeCommandReply(id)
			s.pmu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.pmu.Unlock()
			if ch != nil {
				ch <- body
			}
		case msg.Method == "$/progress":
			if !s.feedProgress(msg.Params) {
				s.progress(msg.Params)
			}
		case msg.Method == "textDocument/publishDiagnostics":
			s.publish(msg.Params)
		case msg.Method == "window/showDocument" && len(msg.ID) > 0:
			go s.showDocument(msg.ID, msg.Params)
		case msg.Method == "workspace/applyEdit" && len(msg.ID) > 0:
			if capture := s.admitCommandEdit(); capture != nil {
				go s.applyCommandEdit(capture, msg.ID, msg.Params)
			} else {
				s.rejectCommandEdit(msg.ID, "no active command accepts this edit")
			}
		case len(msg.ID) > 0:
			s.serverRequest(msg.ID, msg.Method, msg.Params)
		}
	}
}

// publish takes textDocument/publishDiagnostics: kept for code actions, given to rhun as the file's
// set (file-diagnostics) or as the open document's v1 diagnostics
func (s *server) publish(params json.RawMessage) {
	var p struct {
		URI         string            `json:"uri"`
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	path := uriPath(p.URI)
	kept, rich := s.l.keepFileDiagnostics(s, path, p.Diagnostics)
	if rich {
		return
	}
	s.l.mu.Lock()
	d := s.l.byPath[path]
	var text []byte
	var version int64
	if d != nil {
		text, version = d.text, d.version
	}
	s.l.mu.Unlock()
	if d == nil || text == nil {
		return
	}
	if err := s.l.ext.Diagnostics(d.id, version, plainDiagnostics(kept, text)); err != nil {
		// stale: the user typed meanwhile; the server will publish again after the didChange
		s.l.ext.Logf("diagnostics: %v", err)
	}
}

// ---- positions: rhun has 1-based lines and byte columns; LSP has 0-based lines and UTF-16 units

func lspPos(text []byte, off int) lspPosition {
	if off > len(text) {
		off = len(text)
	}
	line := bytes.Count(text[:off], []byte{'\n'})
	ls := bytes.LastIndexByte(text[:off], '\n') + 1
	units := 0
	for _, r := range string(text[ls:off]) {
		units += utf16.RuneLen(r)
		if units < 0 {
			units = 0
		}
	}
	return lspPosition{Line: line, Character: units}
}

func byteOffset(text []byte, p rhunext.Pos) int {
	off := 0
	for n := 1; n < p.Line; n++ {
		i := bytes.IndexByte(text[off:], '\n')
		if i < 0 {
			return len(text)
		}
		off += i + 1
	}
	end := bytes.IndexByte(text[off:], '\n')
	if end < 0 {
		end = len(text) - off
	}
	c := p.Col - 1
	if c > end {
		c = end
	}
	return off + c
}

func rhunPos(text []byte, p lspPosition) rhunext.Pos {
	off := 0
	for n := 0; n < p.Line; n++ {
		i := bytes.IndexByte(text[off:], '\n')
		if i < 0 {
			break
		}
		off += i + 1
	}
	end := bytes.IndexByte(text[off:], '\n')
	if end < 0 {
		end = len(text) - off
	}
	line := text[off : off+end]
	units, col := 0, 0
	for col < len(line) && units < p.Character {
		r, n := utf8.DecodeRune(line[col:])
		units += utf16.RuneLen(r)
		col += n
	}
	return rhunext.Pos{Line: p.Line + 1, Col: col + 1}
}

func fileURI(path string) string {
	path = filepath.ToSlash(path)
	if len(path) >= 2 && path[1] == ':' {
		path = "/" + path
	}
	if strings.HasPrefix(path, "//") {
		host, tail, _ := strings.Cut(path[2:], "/")
		return (&url.URL{Scheme: "file", Host: host, Path: "/" + tail}).String()
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	path := u.Path
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	if u.Host != "" && u.Host != "localhost" {
		path = "//" + u.Host + path
	}
	return filepath.FromSlash(path)
}

// hoverText turns MarkupContent, MarkedString or their arrays into plain-ish text.
func hoverText(raw json.RawMessage) string {
	var mc struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &mc) == nil && mc.Value != "" {
		return unmark(mc.Value)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return unmark(s)
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		var parts []string
		for _, a := range arr {
			if t := hoverText(a); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	var ms struct {
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(raw, &ms) == nil {
		return ms.Value
	}
	return ""
}

// unmark drops the markdown that hover texts commonly carry: code fences and emphasis
func unmark(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.NewReplacer("**", "", "__", "", "`", "").Replace(line)))
	}
	text := strings.TrimSpace(strings.Join(out, "\n"))
	if len(text) > 3000 {
		text = text[:3000]
	}
	return text
}

// has is whether rhun agreed a feature (false before the session, as in the tests)
func (l *lsp) has(feature string) bool { return l.ext != nil && l.ext.Has(feature) }

// completionCapabilities is the client's completion capabilities: snippets only when rhun
// agreed native snippet fields, and the item properties complete-items carries
func (l *lsp) completionCapabilities() map[string]any {
	formats := []string{"plaintext", "markdown"}
	if l.has(featureMarkdown) {
		formats = []string{"markdown", "plaintext"}
	}
	item := map[string]any{"snippetSupport": l.has(rhunext.FeatureSnippets), "documentationFormat": formats}
	caps := map[string]any{"contextSupport": true, "completionItem": item}
	if l.has(rhunext.FeatureCompleteItems) {
		item["insertReplaceSupport"] = true
		item["commitCharactersSupport"] = true
		item["labelDetailsSupport"] = true
		item["deprecatedSupport"] = true
		item["preselectSupport"] = true
		item["tagSupport"] = map[string]any{"valueSet": []int{1}}
		item["insertTextModeSupport"] = map[string]any{"valueSet": []int{1, 2}}
		item["resolveSupport"] = map[string]any{"properties": []string{"documentation", "detail", "additionalTextEdits"}}
		caps["completionList"] = map[string]any{"itemDefaults": []string{"commitCharacters", "editRange", "insertTextFormat", "insertTextMode", "data"}}
	}
	return caps
}

// clientCapabilities are what the client offers the server: the services of the features rhun
// agreed, each with dynamic registration. Positions stay UTF-16: the extension converts columns
// in that encoding everywhere, so it offers no other.
func (l *lsp) clientCapabilities(watch bool) map[string]any {
	formats := []string{"plaintext", "markdown"}
	if l.has(featureMarkdown) {
		formats = []string{"markdown", "plaintext"}
	}
	diagnostics := map[string]any{"versionSupport": true, "dataSupport": true}
	if l.has(rhunext.FeatureFileDiagnostics) {
		diagnostics["relatedInformation"] = true
		diagnostics["tagSupport"] = map[string]any{"valueSet": []int{1, 2}}
		diagnostics["codeDescriptionSupport"] = true
	}
	rename := map[string]any{"prepareSupport": false}
	if l.has(rhunext.FeaturePrepareRename) {
		rename = map[string]any{"prepareSupport": true, "prepareSupportDefaultBehavior": 1}
	}
	text := map[string]any{
		"synchronization":    map[string]any{"didSave": true},
		"publishDiagnostics": diagnostics,
		"hover":              map[string]any{"contentFormat": formats},
		"definition":         map[string]any{"linkSupport": true},
		"references":         map[string]any{},
		"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
		"codeAction": map[string]any{"isPreferredSupport": true, "disabledSupport": true,
			"dataSupport": true, "resolveSupport": map[string]any{"properties": []string{"edit", "command"}},
			"codeActionLiteralSupport": map[string]any{"codeActionKind": map[string]any{"valueSet": []string{
				"", "quickfix", "refactor", "refactor.extract", "refactor.inline", "refactor.move", "refactor.rewrite",
				"source", "source.organizeImports", "source.fixAll"}}}},
		"rename":     rename,
		"formatting": map[string]any{},
		"completion": l.completionCapabilities(),
		"signatureHelp": map[string]any{"contextSupport": true, "signatureInformation": map[string]any{
			"documentationFormat":  formats,
			"parameterInformation": map[string]any{"labelOffsetSupport": true}, "activeParameterSupport": true}},
	}
	if l.has(rhunext.FeatureNavigation) {
		for _, name := range []string{"declaration", "typeDefinition", "implementation"} {
			text[name] = map[string]any{"linkSupport": true}
		}
	}
	if l.has(rhunext.FeatureSelectionRanges) {
		text["selectionRange"] = map[string]any{}
	}
	if l.has(rhunext.FeatureHierarchy) {
		text["callHierarchy"] = map[string]any{}
		text["typeHierarchy"] = map[string]any{}
	}
	if l.has(rhunext.FeaturePartialFormat) {
		text["rangeFormatting"] = map[string]any{}
		text["onTypeFormatting"] = map[string]any{}
	}
	if l.has(rhunext.FeatureDocumentLinks) {
		text["documentLink"] = map[string]any{"tooltipSupport": true}
	}
	if l.has(rhunext.FeatureFileDiagnostics) {
		text["diagnostic"] = map[string]any{"relatedDocumentSupport": false}
	}
	workspace := map[string]any{"configuration": true, "workspaceFolders": true, "applyEdit": true, "executeCommand": map[string]any{},
		"workspaceEdit": workspaceEditCapabilities(l.editCaps())}
	if l.editCaps().ops {
		workspace["fileOperations"] = map[string]any{"dynamicRegistration": true, "didCreate": true, "didRename": true, "didDelete": true}
	}
	if l.has(rhunext.FeatureLinkedEditing) {
		text["linkedEditingRange"] = map[string]any{}
	}
	if l.has(rhunext.FeatureWorkspaceSymbols) {
		kinds := make([]int, len(symbolKinds))
		for i := range kinds {
			kinds[i] = i + 1
		}
		workspace["symbol"] = map[string]any{"symbolKind": map[string]any{"valueSet": kinds},
			"tagSupport": map[string]any{"valueSet": []int{1}}, "resolveSupport": map[string]any{"properties": []string{"location.range"}}}
	}
	if l.has(rhunext.FeatureFileDiagnostics) {
		workspace["diagnostics"] = map[string]any{"refreshSupport": true}
	}
	if watch {
		workspace["didChangeWatchedFiles"] = map[string]any{"dynamicRegistration": true, "relativePatternSupport": true}
	}
	caps := map[string]any{"textDocument": text, "workspace": workspace,
		"window":  map[string]any{"workDoneProgress": true, "showDocument": map[string]any{"support": true}},
		"general": map[string]any{"positionEncodings": []string{"utf-16"}}}
	text["synchronization"].(map[string]any)["dynamicRegistration"] = true
	for _, info := range capabilityFields {
		section, name, _ := strings.Cut(info.client, ".")
		if group, ok := caps[section].(map[string]any); ok {
			if options, ok := group[name].(map[string]any); ok {
				options["dynamicRegistration"] = true
			}
		}
	}
	return caps
}

func (l *lsp) configError(err error) {
	l.ext.Logf("configuration: %v", err)
	go l.ext.Toast("Language server configuration: " + err.Error())
}

func argOr(ev rhunext.Event, i int) string {
	if i < len(ev.Args) {
		return ev.Args[i]
	}
	return ""
}

func main() {
	features := []string{rhunext.FeatureComplete, rhunext.FeatureSignature, rhunext.FeatureDefinition,
		rhunext.FeatureReferences, rhunext.FeatureSymbols, rhunext.FeatureActions, rhunext.FeatureRename, rhunext.FeatureFormat, rhunext.FeatureSaveActions, rhunext.FeatureWorkspaceWatch,
		// protocol 1.1: native snippet fields, completion items, overloads, streaming and narrowing,
		// declaration/type definition/implementation, prepare-rename, range and on-type formatting,
		// code actions with kinds and resolve,
		rhunext.FeatureSnippets, rhunext.FeatureSnippetVariants, rhunext.FeatureSnippetTransforms,
		rhunext.FeatureCompleteItems, rhunext.FeatureSignatureList, rhunext.FeatureQueryStream, rhunext.FeatureProviderScope,
		rhunext.FeatureNavigation, rhunext.FeaturePrepareRename, rhunext.FeaturePartialFormat, rhunext.FeatureCodeActions,
		// the hover card, Expand Selection, hierarchies, Go to Symbol in Project, links, the
		// Problems store, and Markdown documentation where rhun renders it
		rhunext.FeatureHoverQuery, rhunext.FeatureSelectionRanges, rhunext.FeatureHierarchy, rhunext.FeatureWorkspaceSymbols,
		rhunext.FeatureDocumentLinks, rhunext.FeatureFileDiagnostics, featureMarkdown,
		// commands invoked with a body: a code action's server command is a command reference
		rhunext.FeatureCommandArgs,
		// workspace edits as transactions, file operations, showDocument, linked editing
		rhunext.FeatureWorkspaceEdits, rhunext.FeatureFileOperations, rhunext.FeatureReveal, rhunext.FeatureLinkedEditing}
	l := &lsp{}
	err := rhunext.RunWith(features, l)
	l.shutdown()
	if err != nil {
		os.Exit(1)
	}
}

func (l *lsp) shutdown() {
	l.stopWorkspaceWatcher()
	l.closeStatus()
	l.mu.Lock()
	l.stopping = true
	var old []*server
	for _, s := range l.servers {
		if s != nil {
			old = append(old, s)
		}
	}
	l.mu.Unlock()
	for _, s := range old {
		s.stop()
	}
	// All children have already received Kill and their pipes are closed. Bound
	// the aggregate reap wait so shutdown remains responsive on every platform.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, s := range old {
		s.stateMu.Lock()
		launched := s.cmd != nil
		s.stateMu.Unlock()
		if launched {
			select {
			case <-s.waitDone:
			case <-ctx.Done():
				return
			}
		}
	}
}
