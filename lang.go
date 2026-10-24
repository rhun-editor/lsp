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
func askFor(s *server, d *documentQuery, wait time.Duration, method string, params any) (json.RawMessage, error) {
	return d.request(wait, method, params)
}

func docParams(d *documentQuery, text []byte, at rhunext.Pos) map[string]any {
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
	// rhun's text of a file: CR before LF dropped
	data, err := os.ReadFile(path)
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), rhunext.OnDisk, err
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

// ---- definition, references (12d); declaration, type definition, implementation (navigation)

// locationKinds are rhun's location requests: the server's method and how long to wait
var locationKinds = map[string]struct {
	method string
	limits queryLimits
}{
	"definition":      {"textDocument/definition", queryLimits{waitDefinition, 29 * time.Second}},
	"declaration":     {"textDocument/declaration", queryLimits{waitDefinition, 29 * time.Second}},
	"type-definition": {"textDocument/typeDefinition", queryLimits{waitDefinition, 29 * time.Second}},
	"implementation":  {"textDocument/implementation", queryLimits{waitReferences, 59 * time.Second}},
	"references":      {"textDocument/references", queryLimits{waitReferences, 119 * time.Second}},
}

func (l *lsp) Definition(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Location {
	return l.LocationsStream(ext, "definition", req, nil)
}

func (l *lsp) References(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Location {
	return l.LocationsStream(ext, "references", req, nil)
}

// LocationsStream answers definition, references, declaration, type-definition and
// implementation; with query-stream agreed the server's partial results come as parts, its
// progress as progress, and a long answer is sent in parts rhun takes whole
func (l *lsp) LocationsStream(ext *rhunext.Ext, kind string, req rhunext.PosRequest, out *rhunext.Stream) []rhunext.Location {
	k, ok := locationKinds[kind]
	if !ok {
		return nil
	}
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	p := docParams(d, text, req.At)
	if kind == "references" {
		p["context"] = map[string]any{"includeDeclaration": true}
	}
	convert := func(raw json.RawMessage) []rhunext.Location { return l.locations(raw, d) }
	res, carried, err := streamQuery(l, d, out, k.limits, k.method, p, convert, sendLocations)
	if err != nil && len(carried) == 0 {
		return nil
	}
	locs := append(carried, l.locations(res, d)...)
	return streamRest(l, out, locs, locationSize, sendLocations)
}

func sendLocations(out *rhunext.Stream, locs []rhunext.Location) error {
	return out.PartLocations(locs)
}

func locationSize(loc rhunext.Location) int { return len(loc.Target.String()) + len(loc.Text) + 48 }

// locations reads Location, Location[] or LocationLink[]
func (l *lsp) locations(res json.RawMessage, query *documentQuery) []rhunext.Location {
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
			text, _, _ = l.textForQuery(path, query)
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
	res, err := askFor(s, d, waitSymbols, "textDocument/documentSymbol",
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
	Data    json.RawMessage `json:"data"`
	Edit    json.RawMessage `json:"edit"`
	Command json.RawMessage `json:"command"` // a Command in a CodeAction, a string in a bare Command
	Args    json.RawMessage `json:"arguments"`
}

// actionData is what comes back with the action event: the command to run on which server
type actionTarget struct {
	Doc        rhunext.Doc `json:"doc"`
	Path       string      `json:"path"`
	Version    int64       `json:"version"`
	LSPVersion int         `json:"lspVersion"`
}

type actionData struct {
	Generation uint64         `json:"generation,omitempty"`
	Lang       string         `json:"lang"`
	Command    lspCommand     `json:"command"`
	Resolve    *lspAction     `json:"resolve,omitempty"`
	Prepared   *lspAction     `json:"prepared,omitempty"`
	Targets    []actionTarget `json:"targets,omitempty"`
	Doc        rhunext.Doc    `json:"doc,omitempty"`
	Version    int64          `json:"version"`
	FileOps    []fileOp       `json:"fileOps,omitempty"` // the action's edit made them
	// protocol 1.1 (kept in memory, never sent): an edit with file operations that goes with the
	// resolve answer, and the command reference that follows it
	Edit    *rhunext.WorkspaceEdit `json:"-"`
	Ref     *rhunext.CommandRef    `json:"-"`
	Queries []*documentQuery       `json:"-"`
}

func (l *lsp) Actions(ext *rhunext.Ext, req rhunext.ActionsRequest) []rhunext.CodeAction {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	start, end := lspPos(text, byteOffset(text, req.Range.Start)), lspPos(text, byteOffset(text, req.Range.End))
	diags := l.rangeDiagnostics(d.path, start, end)
	queries := l.commandQuerySnapshots(s, d)
	res, err := askFor(s, d, waitActions, "textDocument/codeAction", map[string]any{
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
		if !bareActionCommand(a) && canResolveAction(d) {
			action := a
			data := actionData{Lang: d.lang, Generation: s.generation, Resolve: &action, Doc: d.id, Version: d.version}
			if actionHasEdit(a) {
				edit, err := l.commandWorkspaceEdit(a.Edit, s, queries...)
				if err != nil {
					ext.Logf("action %q: %v", a.Title, err)
					continue
				}
				data.Targets = actionTargets(edit, queries)
			}
			act.Data, _ = json.Marshal(data)
			out = append(out, act)
			continue
		}
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
		var fileOps []fileOp
		if len(a.Edit) > 0 && string(a.Edit) != "null" {
			conv, err := l.commandWorkspaceEditFull(a.Edit, s, a.Title, queries...)
			if err != nil {
				ext.Logf("action %q: %v", a.Title, err)
				continue
			}
			edit := conv.edit
			act.Edit, fileOps = &edit, conv.ops
		}
		if cmd != nil {
			data := actionData{Lang: d.lang, Generation: s.generation, Command: *cmd}
			if act.Edit != nil {
				action := a
				data.Prepared = &action
				data.Doc = d.id
				data.Version = d.version
				data.Targets = actionTargets(*act.Edit, queries)
				// Apply and synchronize the edit before executing its command.
				act.Edit = nil
			}
			act.Data, _ = json.Marshal(data)
		}
		if act.Data == nil && len(fileOps) > 0 {
			// the action event says the edit was made: the servers hear of its file operations
			act.Data, _ = json.Marshal(actionData{Lang: d.lang, Generation: s.generation, FileOps: fileOps})
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
	if json.Unmarshal(data, &a) != nil {
		return
	}
	if len(a.FileOps) > 0 && a.Command.Command == "" && a.Resolve == nil && a.Prepared == nil {
		go l.notifyFileOps(a.FileOps)
		return
	}
	if a.Command.Command == "" && a.Resolve == nil && a.Prepared == nil {
		return
	}
	go l.runExplicitCommand(a)
}

// ---- workspace edits (12g)

type lspEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

// workspaceEdit turns LSP's WorkspaceEdit (changes, or documentChanges of TextDocumentEdits)
// into rhun's: each file's edits made against the text rhun has (the open document at its
// version, else the file on disk). Creating, renaming or deleting files is not supported.
func (l *lsp) workspaceEdit(raw json.RawMessage, queries ...*documentQuery) (rhunext.WorkspaceEdit, error) {
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
		var query *documentQuery
		if len(queries) > 0 {
			query = queries[0]
		}
		text, version, err := l.textForQuery(path, query)
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
	res, err := askFor(s, d, waitRename, "textDocument/rename", p)
	if err != nil {
		return rhunext.WorkspaceEdit{}, err
	}
	if len(res) == 0 || string(res) == "null" {
		return rhunext.WorkspaceEdit{}, errors.New("nothing to rename here")
	}
	if caps := l.editCaps(); caps.ws {
		// a transaction: files that are not open edited on disk, files renamed (a module's)
		conv, err := l.convertEdit(res, convertOptions{caps: caps, queries: []*documentQuery{d}, server: s, label: "Rename to " + req.Name})
		if err != nil {
			return rhunext.WorkspaceEdit{}, err
		}
		l.expectFileOps(conv.ops)
		return conv.edit, nil
	}
	return l.workspaceEdit(res, d)
}

func (l *lsp) Format(ext *rhunext.Ext, req rhunext.FormatRequest) rhunext.WorkspaceEdit {
	var out rhunext.WorkspaceEdit
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return out
	}
	res, err := askFor(s, d, waitFormat, "textDocument/formatting", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"options":      map[string]any{"tabSize": req.TabWidth, "insertSpaces": req.Spaces}})
	if err != nil {
		ext.Toast("Format: " + err.Error())
		return out
	}
	return docEdit(res, req.Doc, req.Version, text)
}

// ---- prepare-rename, partial-format (protocol 1.1)

// a little under rhun's limits of prepare-rename, range-format and type-format
const (
	waitPrepareRename = 1800 * time.Millisecond
	waitTypeFormat    = 450 * time.Millisecond
)

// PrepareRename asks textDocument/prepareRename what F2 renames: a range (its text is the
// placeholder), a range and a placeholder, or the default behaviour (the word at the cursor, an
// empty answer); null or the server's error refuses. A server without prepareRename, or no answer
// in time, leaves the word at the cursor.
func (l *lsp) PrepareRename(ext *rhunext.Ext, req rhunext.PosRequest) (*rhunext.PreparedRename, error) {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil, nil
	}
	if !s.supports("textDocument/prepareRename", selectorFor(d.origin)) {
		return nil, nil
	}
	res, err := d.request(waitPrepareRename, "textDocument/prepareRename", docParams(d, text, req.At))
	var refused *serverError
	switch {
	case errors.As(err, &refused) && refused.Code != codeRequestCancelled && refused.Code != codeContentModified:
		if strings.TrimSpace(refused.Message) == "" {
			return nil, rhunext.ErrNothingToRename
		}
		return nil, errors.New(oneLineText(refused.Message))
	case err != nil:
		return nil, nil // no answer: the word at the cursor
	}
	return preparedRename(res, text)
}

// preparedRename reads prepareRename's answer: Range | {range, placeholder} | {defaultBehavior}
// | null
func preparedRename(res json.RawMessage, text []byte) (*rhunext.PreparedRename, error) {
	if len(res) == 0 || string(res) == "null" {
		return nil, rhunext.ErrNothingToRename
	}
	var x struct {
		Range           *lspRange    `json:"range"`
		Placeholder     *string      `json:"placeholder"`
		DefaultBehavior bool         `json:"defaultBehavior"`
		Start           *lspPosition `json:"start"`
		End             *lspPosition `json:"end"`
	}
	if json.Unmarshal(res, &x) != nil {
		return nil, nil
	}
	switch {
	case x.Range != nil:
		r := rhunRange(text, *x.Range)
		p := &rhunext.PreparedRename{Range: r}
		if x.Placeholder != nil {
			p.Placeholder = *x.Placeholder
		} else {
			p.Placeholder = string(text[byteOffset(text, r.Start):byteOffset(text, r.End)])
		}
		return p, nil
	case x.Start != nil && x.End != nil:
		r := rhunRange(text, lspRange{Start: *x.Start, End: *x.End})
		return &rhunext.PreparedRename{Range: r, Placeholder: string(text[byteOffset(text, r.Start):byteOffset(text, r.End)])}, nil
	case x.DefaultBehavior:
		return nil, nil
	}
	return nil, rhunext.ErrNothingToRename
}

// oneLineText is the first line of a server's message
func oneLineText(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// formattingOptions are LSP's FormattingOptions from rhun's settings in force
func formattingOptions(tab int, spaces, trim, final bool) map[string]any {
	return map[string]any{"tabSize": tab, "insertSpaces": spaces, "trimTrailingWhitespace": trim, "insertFinalNewline": final}
}

// docEdit is a server's TextEdit[] for the document as rhun's edit at the query's version
func docEdit(res json.RawMessage, doc rhunext.Doc, version int64, text []byte) rhunext.WorkspaceEdit {
	var out rhunext.WorkspaceEdit
	var edits []lspEdit
	if json.Unmarshal(res, &edits) != nil || len(edits) == 0 {
		return out
	}
	re := make([]rhunext.TextEdit, 0, len(edits))
	for _, e := range edits {
		re = append(re, rhunext.TextEdit{Range: rhunRange(text, e.Range), Text: e.NewText})
	}
	out.AddDoc(doc, version, re...)
	return out
}

// RangeFormat asks textDocument/rangeFormatting for the selection (feature partial-format)
func (l *lsp) RangeFormat(ext *rhunext.Ext, req rhunext.RangeFormatRequest) rhunext.WorkspaceEdit {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return rhunext.WorkspaceEdit{}
	}
	res, err := d.request(waitFormat, "textDocument/rangeFormatting", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"range": map[string]any{"start": lspPos(text, byteOffset(text, req.Range.Start)),
			"end": lspPos(text, byteOffset(text, req.Range.End))},
		"options": formattingOptions(req.TabWidth, req.Spaces, req.Trim, req.Final)})
	if err != nil {
		ext.Toast("Format Selection: " + err.Error())
		return rhunext.WorkspaceEdit{}
	}
	return docEdit(res, req.Doc, req.Version, text)
}

// TypeFormat asks textDocument/onTypeFormatting after a trigger character (feature
// partial-format); a server still starting is not waited for
func (l *lsp) TypeFormat(ext *rhunext.Ext, req rhunext.TypeFormatRequest) rhunext.WorkspaceEdit {
	l.mu.Lock()
	var s *server
	if d := l.docs[req.Doc]; d != nil {
		s = d.server
	}
	l.mu.Unlock()
	if s == nil || !s.ready.Load() {
		return rhunext.WorkspaceEdit{}
	}
	d, text, _ := l.current(req.Doc, req.Version)
	if d == nil {
		return rhunext.WorkspaceEdit{}
	}
	ch := req.Char
	if ch == "newline" {
		ch = "\n"
	}
	p := docParams(d, text, req.At)
	p["ch"] = ch
	p["options"] = formattingOptions(req.TabWidth, req.Spaces, req.Trim, req.Final)
	res, err := d.request(waitTypeFormat, "textDocument/onTypeFormatting", p)
	if err != nil {
		return rhunext.WorkspaceEdit{}
	}
	return docEdit(res, req.Doc, req.Version, text)
}

func actionTargets(edit rhunext.WorkspaceEdit, queries []*documentQuery) []actionTarget {
	var targets []actionTarget
	for _, file := range edit.Files {
		for _, q := range queries {
			if queryMatchesTarget(q, file.Target) {
				targets = append(targets, actionTarget{Doc: q.id, Path: q.path, Version: q.version, LSPVersion: q.lspVersion})
				break
			}
		}
	}
	return targets
}
