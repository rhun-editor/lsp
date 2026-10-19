package main

// Completion and signature help (sections 12b and 12c of the protocol) from the language server:
// textDocument/completion and textDocument/signatureHelp, their positions turned from UTF-16
// into rhun's byte columns, snippets into plain text, markdown into plain-ish text.

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// how long a request waits for the server: rhun drops a late answer anyway
const askTimeout = 3 * time.Second

// lspKinds maps LSP's CompletionItemKind (1-based) to rhun's
var lspKinds = []rhunext.CompletionKind{
	rhunext.KindText, rhunext.KindText, rhunext.KindMethod, rhunext.KindFunction, rhunext.KindFunction,
	rhunext.KindField, rhunext.KindVariable, rhunext.KindClass, rhunext.KindInterface, rhunext.KindModule,
	rhunext.KindProperty, rhunext.KindValue, rhunext.KindValue, rhunext.KindEnum, rhunext.KindKeyword,
	rhunext.KindSnippet, rhunext.KindValue, rhunext.KindFile, rhunext.KindVariable, rhunext.KindFolder,
	rhunext.KindConstant, rhunext.KindConstant, rhunext.KindStruct, rhunext.KindField, rhunext.KindKeyword,
	rhunext.KindClass,
}

type lspTextEdit struct {
	Range   *lspRange `json:"range"`
	Insert  *lspRange `json:"insert"` // InsertReplaceEdit
	Replace *lspRange `json:"replace"`
	NewText string    `json:"newText"`
}

type lspItem struct {
	Label        string `json:"label"`
	LabelDetails *struct {
		Detail      string `json:"detail"`
		Description string `json:"description"`
	} `json:"labelDetails"`
	Kind             int             `json:"kind"`
	Tags             []int           `json:"tags"`
	Detail           string          `json:"detail"`
	Documentation    json.RawMessage `json:"documentation"`
	Deprecated       bool            `json:"deprecated"`
	Preselect        bool            `json:"preselect"`
	SortText         string          `json:"sortText"`
	FilterText       string          `json:"filterText"`
	InsertText       string          `json:"insertText"`
	InsertTextFormat int             `json:"insertTextFormat"`
	InsertTextMode   int             `json:"insertTextMode"`
	TextEdit         *lspTextEdit    `json:"textEdit"`
	TextEditText     string          `json:"textEditText"`
	AdditionalEdits  []lspTextEdit   `json:"additionalTextEdits"`
	CommitCharacters []string        `json:"commitCharacters"`
	Command          *struct {
		Command string `json:"command"`
	} `json:"command"`
	Data json.RawMessage `json:"data"`
}

// lspItemDefaults are a list's values for items that leave them out (LSP 3.17 itemDefaults)
type lspItemDefaults struct {
	CommitCharacters []string        `json:"commitCharacters"`
	EditRange        json.RawMessage `json:"editRange"`
	InsertTextFormat int             `json:"insertTextFormat"`
	InsertTextMode   int             `json:"insertTextMode"`
	Data             json.RawMessage `json:"data"`
}

// ready waits a while for a server still starting
func ready(s *server) bool {
	if s.ready.Load() {
		return true
	}
	select {
	case <-s.readyCh:
		return s.ready.Load()
	case <-time.After(askTimeout):
		return false
	}
}

// ask is s.call with a deadline
func ask(s *server, d *documentQuery, method string, params any) (json.RawMessage, bool) {
	res, err := d.request(askTimeout, method, params)
	return res, err == nil && res != nil && string(res) != "null"
}

func (l *lsp) Complete(ext *rhunext.Ext, req rhunext.Request) rhunext.CompletionList {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return rhunext.CompletionList{}
	}
	ctx := map[string]any{"triggerKind": 1}
	switch req.Kind {
	case "trigger":
		ctx = map[string]any{"triggerKind": 2, "triggerCharacter": req.Char}
	case "incomplete":
		ctx["triggerKind"] = 3
	}
	res, ok := ask(s, d, "textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"position":     lspPos(text, byteOffset(text, req.At)), "context": ctx})
	if !ok {
		return rhunext.CompletionList{}
	}
	var items []lspItem
	var defaults lspItemDefaults
	incomplete := false
	if res[0] == '[' {
		json.Unmarshal(res, &items)
	} else {
		var list struct {
			IsIncomplete bool            `json:"isIncomplete"`
			ItemDefaults lspItemDefaults `json:"itemDefaults"`
			Items        []lspItem       `json:"items"`
		}
		json.Unmarshal(res, &list)
		items, incomplete, defaults = list.Items, list.IsIncomplete, list.ItemDefaults
	}
	// the server's order (sortText, else the label) is the order of equally good matches
	sort.SliceStable(items, func(i, j int) bool { return sortKey(items[i]) < sortKey(items[j]) })
	word := rhunext.Range{Start: rhunext.WordStart(text, req.At), End: req.At}
	resolves := s.supports("completionItem/resolve", selectorFor(d.origin)) && ext.Has(rhunext.FeatureCompleteItems)
	snip := newSnippetContext(ext, d, text, req.At)
	out := rhunext.CompletionList{Incomplete: incomplete}
	for _, it := range items {
		out.Items = append(out.Items, l.completionItem(ext, it, defaults, text, word, resolves, snip))
	}
	return out
}

// completionItem turns an LSP completion item into rhun's, with the complete-items records
// when rhun agreed them and native snippet fields when it agreed snippets
func (l *lsp) completionItem(ext *rhunext.Ext, it lspItem, defaults lspItemDefaults, text []byte,
	word rhunext.Range, resolves bool, snip *snippetContext) rhunext.CompletionItem {
	c := rhunext.CompletionItem{Range: word, Label: it.Label, Detail: it.Detail, Doc: docText(it.Documentation)}
	if it.Kind > 0 && it.Kind < len(lspKinds) {
		c.Kind = lspKinds[it.Kind]
	}
	conv := func(r *lspRange) rhunext.Range {
		return rhunext.Range{Start: rhunPos(text, r.Start), End: rhunPos(text, r.End)}
	}
	format, mode := it.InsertTextFormat, it.InsertTextMode
	if format == 0 {
		format = defaults.InsertTextFormat
	}
	if mode == 0 {
		mode = defaults.InsertTextMode
	}
	c.Insert = it.InsertText
	var replace *rhunext.Range
	edit := it.TextEdit
	if edit == nil && len(defaults.EditRange) > 0 {
		// itemDefaults.editRange: a Range, or insert and replace ranges
		var both lspTextEdit
		var one lspRange
		if json.Unmarshal(defaults.EditRange, &both) == nil && both.Insert != nil {
			edit = &lspTextEdit{Insert: both.Insert, Replace: both.Replace}
		} else if json.Unmarshal(defaults.EditRange, &one) == nil {
			edit = &lspTextEdit{Range: &one}
		}
		if edit != nil {
			edit.NewText = it.TextEditText
			if edit.NewText == "" {
				edit.NewText = it.InsertText
			}
			if edit.NewText == "" {
				edit.NewText = it.Label
			}
		}
	}
	if edit != nil {
		c.Insert = edit.NewText
		switch {
		case edit.Range != nil:
			c.Range = conv(edit.Range)
		case edit.Insert != nil:
			c.Range = conv(edit.Insert)
			if edit.Replace != nil {
				r := conv(edit.Replace)
				replace = &r
			}
		}
	}
	if c.Insert == "" {
		c.Insert = it.Label
	}
	if format == 2 {
		if p, ok := snip.prepare(c.Insert); ok {
			c = c.WithSnippetProgram(p)
		} else {
			var cursor int
			c.Insert, cursor = plainSnippet(c.Insert)
			if cursor >= 0 {
				c = c.WithCursor(cursor)
			}
		}
	}
	for _, e := range it.AdditionalEdits {
		if e.Range != nil {
			c.Edits = append(c.Edits, rhunext.TextEdit{Range: conv(e.Range), Text: e.NewText})
		}
	}
	if !ext.Has(rhunext.FeatureCompleteItems) {
		return c
	}
	if replace != nil && replace.Start == c.Range.Start && replace.End != c.Range.End {
		c.Replace = replace
	}
	commit := it.CommitCharacters
	if commit == nil {
		commit = defaults.CommitCharacters
	}
	c.Commit = strings.Join(commit, "")
	if it.FilterText != "" && it.FilterText != it.Label {
		c.Filter = it.FilterText
	}
	if it.LabelDetails != nil {
		c.Suffix = it.LabelDetails.Detail
		if it.LabelDetails.Description != "" {
			c.Detail = it.LabelDetails.Description
		}
	}
	c.Preselect = it.Preselect
	c.Deprecated = it.Deprecated || slices.Contains(it.Tags, 1)
	c.Adjust = mode == 2 && format != 2 && strings.Contains(c.Insert, "\n")
	if it.Command != nil {
		switch it.Command.Command {
		case "editor.action.triggerParameterHints":
			c.AfterSignature = true
		case "editor.action.triggerSuggest":
			c.AfterComplete = true
		}
	}
	if resolves {
		if it.Data == nil && defaults.Data != nil {
			it.Data = defaults.Data
		}
		if raw, err := json.Marshal(it); err == nil && len(raw) <= rhunext.MaxResolveData {
			c.Resolve = raw
			// imports may come with the resolution when the list had none for the item
			c.ResolveEdits = len(it.AdditionalEdits) == 0
		}
	}
	return c
}

// ResolveCompletion asks the server for the rest of an item (completionItem/resolve): its
// documentation, detail and the edits it makes besides its own (imports)
func (l *lsp) ResolveCompletion(ext *rhunext.Ext, req rhunext.ResolveRequest) rhunext.Resolution {
	d, text, s := l.latest(req.Doc)
	if d == nil || !ready(s) {
		return rhunext.Resolution{}
	}
	var it lspItem
	if json.Unmarshal(req.Data, &it) != nil {
		return rhunext.Resolution{}
	}
	ctx, cancel := context.WithTimeout(req.Context, askTimeout)
	defer cancel()
	res, err := d.requestContext(ctx, "completionItem/resolve", json.RawMessage(req.Data))
	if err != nil || res == nil || string(res) == "null" {
		return rhunext.Resolution{}
	}
	var got lspItem
	if json.Unmarshal(res, &got) != nil {
		return rhunext.Resolution{}
	}
	out := rhunext.Resolution{Doc: docText(got.Documentation)}
	if got.Detail != "" && got.Detail != it.Detail {
		out.Detail, out.HasDetail = got.Detail, true
	}
	if len(it.AdditionalEdits) == 0 {
		for _, e := range got.AdditionalEdits {
			if e.Range != nil {
				out.Edits = append(out.Edits, rhunext.TextEdit{
					Range: rhunext.Range{Start: rhunPos(text, e.Range.Start), End: rhunPos(text, e.Range.End)},
					Text:  e.NewText})
			}
		}
	}
	if got.Command != nil && it.Command == nil {
		out.AfterSignature = got.Command.Command == "editor.action.triggerParameterHints"
		out.AfterComplete = got.Command.Command == "editor.action.triggerSuggest"
	}
	return out
}

func sortKey(it lspItem) string {
	if it.SortText != "" {
		return it.SortText
	}
	return it.Label
}

// lspSignatureHelp is textDocument/signatureHelp's answer
type lspSignatureHelp struct {
	Signatures []struct {
		Label         string          `json:"label"`
		Documentation json.RawMessage `json:"documentation"`
		Parameters    []struct {
			Label json.RawMessage `json:"label"`
		} `json:"parameters"`
		ActiveParameter *int `json:"activeParameter"`
	} `json:"signatures"`
	ActiveSignature int  `json:"activeSignature"`
	ActiveParameter *int `json:"activeParameter"`
}

func (l *lsp) signatureHelp(req rhunext.Request) *lspSignatureHelp {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	ctx := map[string]any{"triggerKind": 1, "isRetrigger": req.Kind == "update"}
	if req.Kind == "trigger" {
		ctx["triggerKind"] = 2
		ctx["triggerCharacter"] = req.Char
	} else if req.Kind == "update" {
		ctx["triggerKind"] = 3
	}
	res, ok := ask(s, d, "textDocument/signatureHelp", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"position":     lspPos(text, byteOffset(text, req.At)), "context": ctx})
	if !ok {
		return nil
	}
	var help lspSignatureHelp
	if json.Unmarshal(res, &help) != nil || len(help.Signatures) == 0 {
		return nil
	}
	if help.ActiveSignature < 0 || help.ActiveSignature >= len(help.Signatures) {
		help.ActiveSignature = 0
	}
	return &help
}

// signatureInfo is overload n with its active parameter's bytes
func (h *lspSignatureHelp) signatureInfo(n int) rhunext.SignatureInfo {
	sig := h.Signatures[n]
	out := rhunext.SignatureInfo{Label: sig.Label, ActiveFrom: -1, ActiveTo: -1, Doc: docText(sig.Documentation)}
	active := h.ActiveParameter
	if sig.ActiveParameter != nil {
		active = sig.ActiveParameter
	}
	if active != nil && *active >= 0 && *active < len(sig.Parameters) {
		out.ActiveFrom, out.ActiveTo = paramBytes(sig.Label, sig.Parameters[*active].Label)
	}
	return out
}

func (l *lsp) Signature(ext *rhunext.Ext, req rhunext.Request) *rhunext.SignatureHelp {
	help := l.signatureHelp(req)
	if help == nil {
		return nil
	}
	info := help.signatureInfo(help.ActiveSignature)
	return &rhunext.SignatureHelp{Signature: info.Label, ActiveFrom: info.ActiveFrom, ActiveTo: info.ActiveTo, Doc: info.Doc}
}

// Signatures is every overload (feature signature-list), the server's active one first shown
func (l *lsp) Signatures(ext *rhunext.Ext, req rhunext.Request) *rhunext.SignatureList {
	help := l.signatureHelp(req)
	if help == nil {
		return nil
	}
	out := &rhunext.SignatureList{Active: help.ActiveSignature}
	for i := range help.Signatures {
		if i == 32 {
			break
		}
		out.Signatures = append(out.Signatures, help.signatureInfo(i))
	}
	if out.Active >= len(out.Signatures) {
		out.Active = 0
	}
	return out
}

// paramBytes finds a parameter in the signature: by its text (after the first "("), or by its
// [start, end] in UTF-16 units; -1 -1 when it is not there
func paramBytes(label string, raw json.RawMessage) (int, int) {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		from := strings.IndexByte(label, '(') + 1
		if i := strings.Index(label[from:], name); name != "" && i >= 0 {
			return from + i, from + i + len(name)
		}
		return -1, -1
	}
	var span [2]int
	if json.Unmarshal(raw, &span) != nil {
		return -1, -1
	}
	return utf16Byte(label, span[0]), utf16Byte(label, span[1])
}

// utf16Byte is the byte offset in s of a UTF-16 offset
func utf16Byte(s string, units int) int {
	n := 0
	for i, r := range s {
		if n >= units {
			return i
		}
		n += utf16.RuneLen(r)
	}
	return len(s)
}

// docText is a string or MarkupContent as plain-ish text
func docText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var mc struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &mc) == nil {
		if mc.Kind == "markdown" {
			return strings.TrimSpace(unmark(mc.Value))
		}
		return strings.TrimSpace(mc.Value)
	}
	return ""
}

// plainSnippet turns a snippet into its text: ${1:name} is name, a later $1 repeats it, $0 and an
// empty $N are nothing, \$ is $. The cursor goes where the first tab stop is ($1, else $0): its
// byte offset in the text, or -1 without one.
func plainSnippet(s string) (string, int) {
	stops := map[int]int{}
	text := snippetText(s, map[string]string{}, stops)
	first := -1
	for n := range stops {
		if n > 0 && (first < 0 || n < first) {
			first = n
		}
	}
	if first < 0 {
		first = 0
	}
	if off, ok := stops[first]; ok {
		return text, off
	}
	return text, -1
}

// snippetText is the text of a snippet; stops (when not nil) gets where each tab stop starts
func snippetText(s string, seen map[string]string, stops map[int]int) string {
	mark := func(num string, at int) {
		if stops == nil {
			return
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			return
		}
		if _, ok := stops[n]; !ok {
			stops[n] = at
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			b.WriteByte(s[i+1])
			i += 2
		case c == '$' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			mark(s[i+1:j], b.Len())
			b.WriteString(seen[s[i+1:j]])
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '{':
			// ${N:text} or ${N|a,b|}: the text, or the first choice
			j := i + 2
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			depth, k := 1, j
			for k < len(s) && depth > 0 {
				switch s[k] {
				case '{':
					depth++
				case '}':
					depth--
				}
				k++
			}
			inner := ""
			if j < k-1 {
				inner = s[j : k-1]
			}
			text := ""
			switch {
			case strings.HasPrefix(inner, ":"):
				text = snippetText(inner[1:], seen, nil)
			case strings.HasPrefix(inner, "|"):
				choices := strings.Trim(inner, "|")
				text, _, _ = strings.Cut(choices, ",")
			}
			seen[s[i+2:j]] = text
			mark(s[i+2:j], b.Len())
			b.WriteString(text)
			i = k
		default:
			_, n := utf8.DecodeRuneInString(s[i:])
			b.WriteString(s[i : i+n])
			i += n
		}
	}
	return b.String()
}

// snippetContext prepares LSP snippets as native fields (feature snippets) with variables from
// the extension's own copy of the document: the path, the insertion line and its indentation,
// the date from its clock; no selection and no clipboard
type snippetContext struct {
	ok, transforms, variants bool
	snapshot                 rhunext.SnippetSnapshot
	opts                     rhunext.SnippetVariableOptions
}

func newSnippetContext(ext *rhunext.Ext, d *documentQuery, text []byte, at rhunext.Pos) *snippetContext {
	if !ext.Has(rhunext.FeatureSnippets) {
		return &snippetContext{}
	}
	off := byteOffset(text, at)
	ls := bytes.LastIndexByte(text[:off], '\n') + 1
	le := bytes.IndexByte(text[off:], '\n')
	if le < 0 {
		le = len(text) - off
	}
	line := string(text[ls : off+le])
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	now := rhunext.SnippetTimeFromTime(time.Now())
	start := rhunext.WordStart(text, at)
	return &snippetContext{ok: true, transforms: ext.Has(rhunext.FeatureSnippetTransforms),
		variants: ext.Has(rhunext.FeatureSnippetVariants),
		snapshot: rhunext.SnippetSnapshot{Version: d.version, Position: start, TabWidth: 4,
			InsertSpaces: !strings.Contains(indent, "\t"), CurrentLine: line, Path: d.path,
			Workspace: d.server.l.project, Indent: indent},
		opts: rhunext.SnippetVariableOptions{Time: &now}}
}

// prepare is the snippet as native fields, or false when rhun cannot take it that way (it then
// goes in as plain text)
func (c *snippetContext) prepare(source string) (rhunext.SnippetProgram, bool) {
	if c == nil || !c.ok {
		return rhunext.SnippetProgram{}, false
	}
	p, err := rhunext.PrepareSnippetTemplateWithSnapshot(source, c.snapshot, c.opts)
	if err != nil || p.Validate() != nil || len(p.Transforms) > 0 && !c.transforms ||
		len(p.InitialVariants) > 0 && !c.variants {
		return rhunext.SnippetProgram{}, false
	}
	return p, true
}
