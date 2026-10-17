package main

// Go to Definition, Find All References, Go to Symbol, code actions, rename and format
// (sections 12d to 12h of the protocol) from the language server: textDocument/definition,
// references, documentSymbol, codeAction, rename and formatting, their UTF-16 positions turned
// into rhun's byte columns and their workspace edits into rhun's, made against the text of each
// document at the version rhun knows. A code action that runs a server command comes back as
// the action event and becomes workspace/executeCommand; the edits the server then asks for
// (workspace/applyEdit) go to rhun as an apply request.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// a little under what rhun waits for each answer (section 12d to 12h)
const (
	waitDefinition = 4500 * time.Millisecond
	waitReferences = 9 * time.Second
	waitSymbols    = 4500 * time.Millisecond
	waitActions    = 2500 * time.Millisecond
	waitRename     = 14 * time.Second
	waitFormat     = 9 * time.Second
)

// askFor is s.call with a deadline; the server's error, if it gave one
func askFor(s *server, wait time.Duration, method string, params any) (json.RawMessage, error) {
	type answer struct {
		res json.RawMessage
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		res, err := s.call(method, params)
		ch <- answer{res, err}
	}()
	select {
	case a := <-ch:
		return a.res, a.err
	case <-time.After(wait):
		return nil, errors.New("the language server did not answer in time")
	}
}

// current is the document a request names, if the extension has it at the request's version
// (every event up to that version was delivered before the request, so a mismatch means the
// document is not one the server knows yet)
func (l *lsp) current(doc rhunext.Doc, version int64) (*document, []byte, *server) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.docs[doc]
	if d == nil || d.server == nil || d.text == nil || d.version != version {
		return nil, nil, nil
	}
	return d, d.text, d.server
}

func docParams(d *document, text []byte, at rhunext.Pos) map[string]any {
	return map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}, "position": lspPos(text, byteOffset(text, at))}
}

// textOf is a file's text as rhun has it: the open document's, else the file on disk; and the
// version to name in an edit (rhunext.OnDisk for a file that is not open)
func (l *lsp) textOf(path string) ([]byte, int64, error) {
	l.mu.Lock()
	d := l.byPath[path]
	var text []byte
	var version int64
	if d != nil && d.text != nil {
		text, version = d.text, d.version
	}
	l.mu.Unlock()
	if text != nil {
		return text, version, nil
	}
	data, err := os.ReadFile(path)
	return data, rhunext.OnDisk, err
}

func rhunRange(text []byte, r lspRange) rhunext.Range {
	return rhunext.Range{Start: rhunPos(text, r.Start), End: rhunPos(text, r.End)}
}

// lineText is line n (1-based) of text, without its line break
func lineText(text []byte, n int) string {
	for i := 1; i < n; i++ {
		j := bytes.IndexByte(text, '\n')
		if j < 0 {
			return ""
		}
		text = text[j+1:]
	}
	if j := bytes.IndexByte(text, '\n'); j >= 0 {
		text = text[:j]
	}
	return strings.TrimRight(string(text), "\r")
}

// ---- definition, references (12d)

func (l *lsp) Definition(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Location {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	res, err := askFor(s, waitDefinition, "textDocument/definition", docParams(d, text, req.At))
	if err != nil {
		return nil
	}
	return l.locations(res)
}

func (l *lsp) References(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Location {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	p := docParams(d, text, req.At)
	p["context"] = map[string]any{"includeDeclaration": true}
	res, err := askFor(s, waitReferences, "textDocument/references", p)
	if err != nil {
		return nil
	}
	return l.locations(res)
}

// locations reads Location, Location[] or LocationLink[]
func (l *lsp) locations(res json.RawMessage) []rhunext.Location {
	if len(res) == 0 || string(res) == "null" {
		return nil
	}
	var locs []location
	if res[0] == '[' {
		json.Unmarshal(res, &locs)
	} else {
		var one location
		json.Unmarshal(res, &one)
		locs = []location{one}
	}
	texts := map[string][]byte{}
	var out []rhunext.Location
	for _, loc := range locs {
		uri, rg := loc.URI, loc.Range
		if uri == "" {
			uri = loc.TargetURI
			if loc.TargetSelectionRange != nil {
				rg = *loc.TargetSelectionRange
			}
		}
		path := uriPath(uri)
		if !filepath.IsAbs(path) {
			continue
		}
		text, ok := texts[path]
		if !ok {
			text, _, _ = l.textOf(path)
			texts[path] = text
		}
		r := rhunRange(text, rg)
		out = append(out, rhunext.Location{Target: rhunext.Target{Path: path}, Range: r, Text: lineText(text, r.Start.Line)})
	}
	return out
}

// ---- symbols (12e)

// LSP's SymbolKind, 1-based, is the order of the protocol's kind words
var symbolKinds = []rhunext.SymbolKind{
	rhunext.SymbolFile, rhunext.SymbolModule, rhunext.SymbolNamespace, rhunext.SymbolPackage, rhunext.SymbolClass,
	rhunext.SymbolMethod, rhunext.SymbolProperty, rhunext.SymbolField, rhunext.SymbolConstructor, rhunext.SymbolEnum,
	rhunext.SymbolInterface, rhunext.SymbolFunction, rhunext.SymbolVariable, rhunext.SymbolConstant,
	rhunext.SymbolString, rhunext.SymbolNumber, rhunext.SymbolBoolean, rhunext.SymbolArray, rhunext.SymbolObject,
	rhunext.SymbolKey, rhunext.SymbolNull, rhunext.SymbolEnumMember, rhunext.SymbolStruct, rhunext.SymbolEvent,
	rhunext.SymbolOperator, rhunext.SymbolTypeParameter,
}

func symbolKind(k int) rhunext.SymbolKind {
	if k >= 1 && k <= len(symbolKinds) {
		return symbolKinds[k-1]
	}
	return rhunext.SymbolVariable
}

type lspSymbol struct {
	Name           string      `json:"name"`
	Detail         string      `json:"detail"`
	Kind           int         `json:"kind"`
	Range          lspRange    `json:"range"`
	SelectionRange *lspRange   `json:"selectionRange"`
	Children       []lspSymbol `json:"children"`
	Location       *location   `json:"location"`      // SymbolInformation
	Container      string      `json:"containerName"` // SymbolInformation
}

func (l *lsp) Symbols(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Symbol {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	res, err := askFor(s, waitSymbols, "textDocument/documentSymbol",
		map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
	if err != nil || len(res) == 0 || string(res) == "null" {
		return nil
	}
	var syms []lspSymbol
	if json.Unmarshal(res, &syms) != nil {
		return nil
	}
	var out []rhunext.Symbol
	var walk func(list []lspSymbol, depth int)
	walk = func(list []lspSymbol, depth int) {
		sort.SliceStable(list, func(i, j int) bool { return before(list[i].Range.Start, list[j].Range.Start) })
		for _, sym := range list {
			rg := sym.Range
			if sym.SelectionRange != nil {
				rg = *sym.SelectionRange // the name: where the cursor goes
			}
			if sym.Location != nil {
				rg = sym.Location.Range
			}
			detail := sym.Detail
			if detail == "" && sym.Container != "" {
				detail = sym.Container
			}
			out = append(out, rhunext.Symbol{Range: rhunRange(text, rg), Kind: symbolKind(sym.Kind), Depth: depth,
				Name: sym.Name, Detail: detail})
			walk(sym.Children, depth+1)
		}
	}
	// SymbolInformation carries its range in location
	for i := range syms {
		if syms[i].Location != nil {
			syms[i].Range = syms[i].Location.Range
		}
	}
	walk(syms, 0)
	return out
}

func before(a, b lspPosition) bool {
	return a.Line < b.Line || a.Line == b.Line && a.Character < b.Character
}

// ---- code actions (12f)

type lspCommand struct {
	Title     string            `json:"title"`
	Command   string            `json:"command"`
	Arguments []json.RawMessage `json:"arguments,omitempty"`
}

type lspAction struct {
	Title       string            `json:"title"`
	Kind        string            `json:"kind"`
	Diagnostics []json.RawMessage `json:"diagnostics"`
	IsPreferred bool              `json:"isPreferred"`
	Disabled    *struct {
		Reason string `json:"reason"`
	} `json:"disabled"`
	Edit    json.RawMessage `json:"edit"`
	Command json.RawMessage `json:"command"` // a Command in a CodeAction, a string in a bare Command
	Args    json.RawMessage `json:"arguments"`
}

// actionData is what comes back with the action event: the command to run on which server
type actionData struct {
	Lang    string     `json:"lang"`
	Command lspCommand `json:"command"`
}

func (l *lsp) Actions(ext *rhunext.Ext, req rhunext.ActionsRequest) []rhunext.CodeAction {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	start, end := lspPos(text, byteOffset(text, req.Range.Start)), lspPos(text, byteOffset(text, req.Range.End))
	l.mu.Lock()
	var diags []json.RawMessage
	for _, raw := range l.diags[d.path] {
		var x struct {
			Range lspRange `json:"range"`
		}
		if json.Unmarshal(raw, &x) == nil && !before(x.Range.End, start) && !before(end, x.Range.Start) {
			diags = append(diags, raw)
		}
	}
	l.mu.Unlock()
	if diags == nil {
		diags = []json.RawMessage{}
	}
	res, err := askFor(s, waitActions, "textDocument/codeAction", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"range":        map[string]any{"start": start, "end": end},
		"context":      map[string]any{"diagnostics": diags, "triggerKind": 1}})
	if err != nil || len(res) == 0 || string(res) == "null" {
		return nil
	}
	var raws []lspAction
	if json.Unmarshal(res, &raws) != nil {
		return nil
	}
	var out []rhunext.CodeAction
	for _, a := range raws {
		if a.Disabled != nil || a.Title == "" {
			continue
		}
		act := rhunext.CodeAction{Kind: actionKind(a), Title: a.Title}
		var cmd *lspCommand
		if len(a.Command) > 0 && a.Command[0] == '"' {
			// a bare Command: {title, command, arguments}
			var c lspCommand
			json.Unmarshal(a.Command, &c.Command)
			c.Title = a.Title
			if len(a.Args) > 0 {
				json.Unmarshal(a.Args, &c.Arguments)
			}
			cmd = &c
		} else if len(a.Command) > 0 && string(a.Command) != "null" {
			var c lspCommand
			if json.Unmarshal(a.Command, &c) == nil && c.Command != "" {
				cmd = &c
			}
		}
		if len(a.Edit) > 0 && string(a.Edit) != "null" {
			edit, err := l.workspaceEdit(a.Edit)
			if err != nil {
				ext.Logf("action %q: %v", a.Title, err)
				continue
			}
			act.Edit = &edit
		}
		if cmd != nil {
			data, _ := json.Marshal(actionData{Lang: d.lang, Command: *cmd})
			act.Data = data
		}
		if act.Edit == nil && act.Data == nil {
			continue
		}
		out = append(out, act)
	}
	// the server's preferred fix first among the quick fixes
	sort.SliceStable(out, func(i, j int) bool { return preferred(raws, out[i].Title) && !preferred(raws, out[j].Title) })
	return out
}

func preferred(raws []lspAction, title string) bool {
	for _, a := range raws {
		if a.Title == title {
			return a.IsPreferred
		}
	}
	return false
}

func actionKind(a lspAction) rhunext.ActionKind {
	kind, _, _ := strings.Cut(a.Kind, ".")
	switch kind {
	case "quickfix":
		return rhunext.ActionQuickFix
	case "refactor":
		return rhunext.ActionRefactor
	case "source":
		return rhunext.ActionSource
	case "":
		if len(a.Diagnostics) > 0 {
			return rhunext.ActionQuickFix
		}
		return "action"
	}
	return rhunext.ActionKind(kind)
}

// Action runs a chosen action's server command; its edits come back as workspace/applyEdit
func (l *lsp) Action(ext *rhunext.Ext, data []byte) {
	var a actionData
	if json.Unmarshal(data, &a) != nil || a.Command.Command == "" {
		return
	}
	l.mu.Lock()
	s := l.servers[a.Lang]
	l.mu.Unlock()
	if s == nil || !s.ready {
		ext.Toast("The language server is not running")
		return
	}
	go func() {
		args := a.Command.Arguments
		if args == nil {
			args = []json.RawMessage{}
		}
		if _, err := s.call("workspace/executeCommand", map[string]any{"command": a.Command.Command, "arguments": args}); err != nil {
			l.ext.Toast(a.Command.Title + ": " + err.Error())
		}
	}()
}

// applyEdit answers the server's workspace/applyEdit: the edit goes to rhun, which checks it
// whole and changes nothing when anything is off
func (s *server) applyEdit(id json.RawMessage, params json.RawMessage) {
	var p struct {
		Edit json.RawMessage `json:"edit"`
	}
	json.Unmarshal(params, &p)
	result := map[string]any{"applied": false}
	edit, err := s.l.workspaceEdit(p.Edit)
	if err == nil {
		err = s.l.ext.Apply(edit)
	}
	if err != nil {
		result["failureReason"] = err.Error()
	} else {
		result["applied"] = true
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// ---- workspace edits (12g)

type lspEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

// workspaceEdit turns LSP's WorkspaceEdit (changes, or documentChanges of TextDocumentEdits)
// into rhun's: each file's edits made against the text rhun has (the open document at its
// version, else the file on disk). Creating, renaming or deleting files is not supported.
func (l *lsp) workspaceEdit(raw json.RawMessage) (rhunext.WorkspaceEdit, error) {
	var w struct {
		Changes         map[string][]lspEdit `json:"changes"`
		DocumentChanges []json.RawMessage    `json:"documentChanges"`
	}
	var out rhunext.WorkspaceEdit
	if err := json.Unmarshal(raw, &w); err != nil {
		return out, err
	}
	type group struct {
		uri   string
		edits []lspEdit
	}
	var groups []group
	if w.DocumentChanges != nil {
		for _, dc := range w.DocumentChanges {
			var x struct {
				Kind         string `json:"kind"`
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Edits []lspEdit `json:"edits"`
			}
			if json.Unmarshal(dc, &x) != nil {
				return out, errors.New("an edit that cannot be read")
			}
			if x.Kind != "" {
				return out, errors.New("the edit creates, renames or deletes files")
			}
			groups = append(groups, group{x.TextDocument.URI, x.Edits})
		}
	} else {
		uris := make([]string, 0, len(w.Changes))
		for uri := range w.Changes {
			uris = append(uris, uri)
		}
		sort.Strings(uris)
		for _, uri := range uris {
			groups = append(groups, group{uri, w.Changes[uri]})
		}
	}
	// one group per file: LSP may list a file twice in documentChanges
	merged := map[string]int{}
	for _, g := range groups {
		path := uriPath(g.uri)
		if !filepath.IsAbs(path) {
			return out, errors.New("an edit for " + g.uri)
		}
		text, version, err := l.textOf(path)
		if err != nil {
			return out, errors.New("cannot read " + filepath.Base(path))
		}
		edits := make([]rhunext.TextEdit, 0, len(g.edits))
		for _, e := range g.edits {
			edits = append(edits, rhunext.TextEdit{Range: rhunRange(text, e.Range), Text: e.NewText})
		}
		if i, ok := merged[path]; ok {
			out.Files[i].Edits = append(out.Files[i].Edits, edits...)
			continue
		}
		merged[path] = len(out.Files)
		out.AddFile(path, version, edits...)
	}
	return out, nil
}

// ---- rename (12g), format (12h)

func (l *lsp) Rename(ext *rhunext.Ext, req rhunext.RenameRequest) (rhunext.WorkspaceEdit, error) {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return rhunext.WorkspaceEdit{}, errors.New("no language server for this file")
	}
	p := docParams(d, text, req.At)
	p["newName"] = req.Name
	res, err := askFor(s, waitRename, "textDocument/rename", p)
	if err != nil {
		return rhunext.WorkspaceEdit{}, err
	}
	if len(res) == 0 || string(res) == "null" {
		return rhunext.WorkspaceEdit{}, errors.New("nothing to rename here")
	}
	return l.workspaceEdit(res)
}

func (l *lsp) Format(ext *rhunext.Ext, req rhunext.FormatRequest) rhunext.WorkspaceEdit {
	var out rhunext.WorkspaceEdit
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return out
	}
	res, err := askFor(s, waitFormat, "textDocument/formatting", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"options":      map[string]any{"tabSize": req.TabWidth, "insertSpaces": req.Spaces}})
	if err != nil {
		ext.Toast("Format: " + err.Error())
		return out
	}
	var edits []lspEdit
	if json.Unmarshal(res, &edits) != nil || len(edits) == 0 {
		return out
	}
	re := make([]rhunext.TextEdit, 0, len(edits))
	for _, e := range edits {
		re = append(re, rhunext.TextEdit{Range: rhunRange(text, e.Range), Text: e.NewText})
	}
	out.AddDoc(req.Doc, req.Version, re...)
	return out
}
