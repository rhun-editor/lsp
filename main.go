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
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	id      rhunext.Doc
	version int64 // rhun's
	lang    string
	path    string
	text    []byte
	lspVer  int // the version we told the server
	server  *server
}

// ---- the extension

type lsp struct {
	ext     *rhunext.Ext
	project string
	state   string
	mu      sync.Mutex
	docs    map[rhunext.Doc]*document
	byPath  map[string]*document
	servers map[string]*server // by language
	config  map[string][]string
	diags   map[string][]json.RawMessage // the server's diagnostics by path, for code actions
}

func (l *lsp) Ready(ext *rhunext.Ext, project string) {
	l.ext = ext
	l.project = project
	l.state = os.Getenv("RHUN_STATE")
	l.docs = map[rhunext.Doc]*document{}
	l.byPath = map[string]*document{}
	l.servers = map[string]*server{}
	l.diags = map[string][]json.RawMessage{}
	l.config = readConfig(filepath.Join(l.state, "lsp.ini"))
}

func (l *lsp) Event(ext *rhunext.Ext, ev rhunext.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch ev.Name {
	case "opened":
		o, ok := rhunext.ParseOpened(ev)
		if !ok || o.Path == "" {
			return
		}
		d := &document{id: o.Doc, version: o.Version, lang: o.Lang, path: o.Path}
		l.docs[d.id] = d
		l.byPath[d.path] = d
		go l.open(d)
	case "closed":
		var id int64
		fmt.Sscan(argOr(ev, 0), &id)
		if d := l.docs[rhunext.Doc(id)]; d != nil {
			delete(l.docs, d.id)
			delete(l.byPath, d.path)
			if d.server != nil {
				d.server.notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
			}
		}
	case "edits":
		var id, ver int64
		fmt.Sscan(argOr(ev, 0), &id)
		fmt.Sscan(argOr(ev, 1), &ver)
		d := l.docs[rhunext.Doc(id)]
		if d == nil || d.text == nil {
			return
		}
		edits, err := rhunext.ParseEdits(ev.Body)
		if err != nil {
			ext.Logf("edits: %v", err)
			go l.resync(d)
			return
		}
		changes := make([]map[string]any, 0, len(edits))
		for _, e := range edits {
			if e.Start < 0 || e.Start+e.Del > len(d.text) {
				go l.resync(d)
				return
			}
			rg := map[string]any{"start": lspPos(d.text, e.Start), "end": lspPos(d.text, e.Start+e.Del)}
			changes = append(changes, map[string]any{"range": rg, "text": string(e.Ins)})
			next := make([]byte, 0, len(d.text)-e.Del+len(e.Ins))
			next = append(next, d.text[:e.Start]...)
			next = append(next, e.Ins...)
			next = append(next, d.text[e.Start+e.Del:]...)
			d.text = next
		}
		d.version = ver
		if d.server != nil && d.server.ready {
			d.lspVer++
			d.server.notify("textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI(d.path), "version": d.lspVer},
				"contentChanges": changes,
			})
		}
	case "resync":
		var id int64
		fmt.Sscan(argOr(ev, 0), &id)
		if d := l.docs[rhunext.Doc(id)]; d != nil {
			go l.resync(d)
		}
	case "saved":
		var id int64
		fmt.Sscan(argOr(ev, 0), &id)
		if d := l.docs[rhunext.Doc(id)]; d != nil && d.server != nil && d.server.ready {
			d.server.notify("textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
		}
	case "hover":
		id, at, ok := rhunext.ParseHover(ev)
		if !ok {
			return
		}
		if d := l.docs[id]; d != nil && d.server != nil && d.server.ready && d.text != nil {
			go l.hover(d, at)
		}
	}
}

func (l *lsp) Run(ext *rhunext.Ext, command string, doc rhunext.Doc) {
	switch command {
	case "lsp.restart":
		l.mu.Lock()
		for lang, s := range l.servers {
			if s != nil {
				s.stop()
			}
			delete(l.servers, lang)
		}
		docs := make([]*document, 0, len(l.docs))
		for _, d := range l.docs {
			d.server = nil
			docs = append(docs, d)
		}
		l.mu.Unlock()
		for _, d := range docs {
			go l.open(d)
		}
		ext.Toast("Language servers restarted")
	}
}

// open fetches the text, starts the language's server if needed, and tells it about the document.
func (l *lsp) open(d *document) {
	text, version, err := l.ext.Text(d.id)
	if err != nil {
		return
	}
	l.mu.Lock()
	d.text, d.version = text, version
	s := l.serverFor(d.lang)
	d.server = s
	l.mu.Unlock()
	if s == nil {
		return
	}
	s.waitReady()
	if !s.ready {
		return
	}
	d.lspVer = 1
	s.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": fileURI(d.path), "languageId": languageIDs[d.lang], "version": d.lspVer, "text": string(text)}})
}

func (l *lsp) resync(d *document) {
	text, version, err := l.ext.Text(d.id)
	if err != nil {
		return
	}
	l.mu.Lock()
	d.text, d.version = text, version
	s := d.server
	l.mu.Unlock()
	if s != nil && s.ready {
		d.lspVer++
		s.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": fileURI(d.path), "version": d.lspVer},
			"contentChanges": []map[string]any{{"text": string(text)}},
		})
	}
}

// serverFor returns the running server for a language, starting it on first use; nil when
// there is none for the language or it cannot start. Called with l.mu held.
func (l *lsp) serverFor(lang string) *server {
	if s, ok := l.servers[lang]; ok {
		return s
	}
	argv := l.config[lang]
	if argv == nil {
		argv = servers[lang]
	}
	if argv == nil {
		l.servers[lang] = nil
		return nil
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		l.ext.Status(filepath.Base(argv[0]) + " not installed")
		l.servers[lang] = nil
		return nil
	}
	s := newServer(l, lang, argv)
	l.servers[lang] = s
	go s.start()
	return s
}

func (l *lsp) hover(d *document, at rhunext.Pos) {
	l.mu.Lock()
	off := byteOffset(d.text, at)
	pos := lspPos(d.text, off)
	l.mu.Unlock()
	res, err := d.server.call("textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)}, "position": pos})
	if err != nil || res == nil {
		return
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(res, &h) != nil {
		return
	}
	text := hoverText(h.Contents)
	if text != "" {
		l.ext.Hover(d.id, at, text)
	}
}

// ---- a language server

type server struct {
	l       *lsp
	lang    string
	argv    []string
	cmd     *exec.Cmd
	in      io.WriteCloser
	wmu     sync.Mutex
	nextID  int
	pmu     sync.Mutex
	pending map[int]chan json.RawMessage
	ready   bool
	readyCh chan struct{}
	caps    json.RawMessage
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
	return &server{l: l, lang: lang, argv: argv, pending: map[int]chan json.RawMessage{}, readyCh: make(chan struct{})}
}

func (s *server) start() {
	defer func() {
		if !s.ready {
			close(s.readyCh)
		}
	}()
	s.cmd = exec.Command(s.argv[0], s.argv[1:]...)
	s.cmd.Dir = s.l.project
	s.cmd.Stderr = os.Stderr
	in, err := s.cmd.StdinPipe()
	if err != nil {
		return
	}
	out, err := s.cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := s.cmd.Start(); err != nil {
		s.l.ext.Status(s.name() + " failed to start")
		return
	}
	s.in = in
	s.l.ext.Status(s.name() + ": starting")
	go s.read(bufio.NewReaderSize(out, 1<<20))
	root := fileURI(s.l.project)
	res, err := s.call("initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": root,
		"workspaceFolders": []map[string]any{{"uri": root, "name": filepath.Base(s.l.project)}},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"didSave": true},
				"publishDiagnostics": map[string]any{"versionSupport": true},
				"hover":              map[string]any{"contentFormat": []string{"plaintext", "markdown"}},
				"definition":         map[string]any{"linkSupport": true},
				"references":         map[string]any{},
				"documentSymbol":     map[string]any{"hierarchicalDocumentSymbolSupport": true},
				"codeAction": map[string]any{"isPreferredSupport": true, "disabledSupport": true,
					"codeActionLiteralSupport": map[string]any{"codeActionKind": map[string]any{"valueSet": []string{
						"", "quickfix", "refactor", "refactor.extract", "refactor.inline", "refactor.rewrite", "source",
						"source.organizeImports", "source.fixAll"}}}},
				"rename":     map[string]any{"prepareSupport": false},
				"formatting": map[string]any{},
				"completion": map[string]any{"contextSupport": true, "completionItem": map[string]any{
					"snippetSupport": false, "documentationFormat": []string{"plaintext", "markdown"}}},
				"signatureHelp": map[string]any{"contextSupport": true, "signatureInformation": map[string]any{
					"documentationFormat":  []string{"plaintext", "markdown"},
					"parameterInformation": map[string]any{"labelOffsetSupport": true}, "activeParameterSupport": true}},
			},
			"workspace": map[string]any{"applyEdit": true, "executeCommand": map[string]any{},
				"workspaceEdit": map[string]any{"documentChanges": true, "resourceOperations": []string{}}},
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
		},
	})
	if err != nil {
		s.l.ext.Status(s.name() + ": " + err.Error())
		return
	}
	s.caps = res
	s.notify("initialized", map[string]any{})
	s.ready = true
	close(s.readyCh)
	s.l.ext.Status(s.name())
}

func (s *server) waitReady() { <-s.readyCh }

func (s *server) name() string { return filepath.Base(s.argv[0]) }

func (s *server) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		s.notify("exit", nil)
		s.cmd.Process.Kill()
	}
}

func (s *server) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.in == nil {
		return fmt.Errorf("server not running")
	}
	if _, err := fmt.Fprintf(s.in, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err = s.in.Write(data)
	return err
}

func (s *server) notify(method string, params any) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *server) call(method string, params any) (json.RawMessage, error) {
	s.pmu.Lock()
	s.nextID++
	id := s.nextID
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.pmu.Unlock()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	res, ok := <-ch
	if !ok {
		return nil, fmt.Errorf("server went away")
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	if r.Error != nil {
		return nil, fmt.Errorf("%s", r.Error.Message)
	}
	return r.Result, nil
}

func (s *server) read(br *bufio.Reader) {
	defer func() {
		s.pmu.Lock()
		for id, ch := range s.pending {
			close(ch)
			delete(s.pending, id)
		}
		s.pmu.Unlock()
		s.l.ext.Status(s.name() + " exited")
	}()
	for {
		length := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				fmt.Sscan(v, &length)
			}
		}
		if length < 0 {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		switch {
		case msg.Method == "" && len(msg.ID) > 0:
			var id int
			json.Unmarshal(msg.ID, &id)
			s.pmu.Lock()
			ch := s.pending[id]
			delete(s.pending, id)
			s.pmu.Unlock()
			if ch != nil {
				ch <- body
			}
		case msg.Method == "textDocument/publishDiagnostics":
			s.publish(msg.Params)
		case msg.Method == "workspace/applyEdit" && len(msg.ID) > 0:
			go s.applyEdit(msg.ID, msg.Params)
		case len(msg.ID) > 0:
			// a request from the server: configuration, registrations, progress: answered with null
			s.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": nil})
		}
	}
}

func (s *server) publish(params json.RawMessage) {
	var p struct {
		URI         string `json:"uri"`
		Diagnostics []struct {
			Range    lspRange `json:"range"`
			Severity int      `json:"severity"`
			Message  string   `json:"message"`
			Source   string   `json:"source"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var raw struct {
		Diagnostics []json.RawMessage `json:"diagnostics"`
	}
	json.Unmarshal(params, &raw)
	s.l.mu.Lock()
	s.l.diags[uriPath(p.URI)] = raw.Diagnostics
	d := s.l.byPath[uriPath(p.URI)]
	var text []byte
	var version int64
	if d != nil {
		text, version = d.text, d.version
	}
	s.l.mu.Unlock()
	if d == nil || text == nil {
		return
	}
	ds := make([]rhunext.Diagnostic, 0, len(p.Diagnostics))
	for _, x := range p.Diagnostics {
		sev := rhunext.SevError
		switch x.Severity {
		case 2:
			sev = rhunext.SevWarning
		case 3:
			sev = rhunext.SevInfo
		case 4:
			sev = rhunext.SevHint
		}
		msg := x.Message
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if x.Source != "" {
			msg = x.Source + ": " + msg
		}
		ds = append(ds, rhunext.Diagnostic{Range: rhunext.Range{Start: rhunPos(text, x.Range.Start), End: rhunPos(text, x.Range.End)},
			Severity: sev, Message: msg})
	}
	if err := s.l.ext.Diagnostics(d.id, version, ds); err != nil {
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
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func uriPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	return filepath.FromSlash(u.Path)
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

func readConfig(path string) map[string][]string {
	out := map[string][]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section != "servers" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Fields(v)
	}
	return out
}

func argOr(ev rhunext.Event, i int) string {
	if i < len(ev.Args) {
		return ev.Args[i]
	}
	return ""
}

func main() {
	features := []string{rhunext.FeatureComplete, rhunext.FeatureSignature, rhunext.FeatureDefinition,
		rhunext.FeatureReferences, rhunext.FeatureSymbols, rhunext.FeatureActions, rhunext.FeatureRename, rhunext.FeatureFormat}
	if err := rhunext.RunWith(features, &lsp{}); err != nil {
		os.Exit(1)
	}
}
