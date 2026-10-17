package main

// Completion and signature help (sections 12b and 12c of the protocol) from the language server:
// textDocument/completion and textDocument/signatureHelp, their positions turned from UTF-16
// into rhun's byte columns, snippets into plain text, markdown into plain-ish text.

import (
	"encoding/json"
	"sort"
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
	NewText string    `json:"newText"`
}

type lspItem struct {
	Label            string          `json:"label"`
	Kind             int             `json:"kind"`
	Detail           string          `json:"detail"`
	Documentation    json.RawMessage `json:"documentation"`
	SortText         string          `json:"sortText"`
	InsertText       string          `json:"insertText"`
	InsertTextFormat int             `json:"insertTextFormat"`
	TextEdit         *lspTextEdit    `json:"textEdit"`
	AdditionalEdits  []lspTextEdit   `json:"additionalTextEdits"`
}

// snapshot is a document's text and server at the time of a request
func (l *lsp) snapshot(doc rhunext.Doc) (*document, []byte, *server) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.docs[doc]
	if d == nil || d.server == nil || d.text == nil {
		return nil, nil, nil
	}
	return d, d.text, d.server
}

// ready waits a while for a server still starting
func ready(s *server) bool {
	select {
	case <-s.readyCh:
		return s.ready
	case <-time.After(askTimeout):
		return false
	}
}

// ask is s.call with a deadline
func ask(s *server, method string, params any) (json.RawMessage, bool) {
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
		return a.res, a.err == nil && a.res != nil && string(a.res) != "null"
	case <-time.After(askTimeout):
		return nil, false
	}
}

func (l *lsp) Complete(ext *rhunext.Ext, req rhunext.Request) rhunext.CompletionList {
	d, text, s := l.snapshot(req.Doc)
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
	res, ok := ask(s, "textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"position":     lspPos(text, byteOffset(text, req.At)), "context": ctx})
	if !ok {
		return rhunext.CompletionList{}
	}
	var items []lspItem
	incomplete := false
	if res[0] == '[' {
		json.Unmarshal(res, &items)
	} else {
		var list struct {
			IsIncomplete bool      `json:"isIncomplete"`
			Items        []lspItem `json:"items"`
		}
		json.Unmarshal(res, &list)
		items, incomplete = list.Items, list.IsIncomplete
	}
	// the server's order (sortText, else the label) is the order of equally good matches
	sort.SliceStable(items, func(i, j int) bool { return sortKey(items[i]) < sortKey(items[j]) })
	word := rhunext.Range{Start: rhunext.WordStart(text, req.At), End: req.At}
	out := rhunext.CompletionList{Incomplete: incomplete}
	for _, it := range items {
		c := rhunext.CompletionItem{Range: word, Label: it.Label, Detail: it.Detail, Doc: docText(it.Documentation)}
		if it.Kind > 0 && it.Kind < len(lspKinds) {
			c.Kind = lspKinds[it.Kind]
		}
		c.Insert = it.InsertText
		if it.TextEdit != nil {
			c.Insert = it.TextEdit.NewText
			rg := it.TextEdit.Range
			if rg == nil {
				rg = it.TextEdit.Insert
			}
			if rg != nil {
				c.Range = rhunext.Range{Start: rhunPos(text, rg.Start), End: rhunPos(text, rg.End)}
			}
		}
		if c.Insert == "" {
			c.Insert = it.Label
		}
		if it.InsertTextFormat == 2 {
			c.Insert = plainSnippet(c.Insert)
		}
		for _, e := range it.AdditionalEdits {
			if e.Range != nil {
				c.Edits = append(c.Edits, rhunext.TextEdit{
					Range: rhunext.Range{Start: rhunPos(text, e.Range.Start), End: rhunPos(text, e.Range.End)},
					Text:  e.NewText})
			}
		}
		out.Items = append(out.Items, c)
	}
	return out
}

func sortKey(it lspItem) string {
	if it.SortText != "" {
		return it.SortText
	}
	return it.Label
}

func (l *lsp) Signature(ext *rhunext.Ext, req rhunext.Request) *rhunext.SignatureHelp {
	d, text, s := l.snapshot(req.Doc)
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
	res, ok := ask(s, "textDocument/signatureHelp", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"position":     lspPos(text, byteOffset(text, req.At)), "context": ctx})
	if !ok {
		return nil
	}
	var help struct {
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
	if json.Unmarshal(res, &help) != nil || len(help.Signatures) == 0 {
		return nil
	}
	n := help.ActiveSignature
	if n < 0 || n >= len(help.Signatures) {
		n = 0
	}
	sig := help.Signatures[n]
	out := &rhunext.SignatureHelp{Signature: sig.Label, ActiveFrom: -1, ActiveTo: -1, Doc: docText(sig.Documentation)}
	active := help.ActiveParameter
	if sig.ActiveParameter != nil {
		active = sig.ActiveParameter
	}
	if active != nil && *active >= 0 && *active < len(sig.Parameters) {
		out.ActiveFrom, out.ActiveTo = paramBytes(sig.Label, sig.Parameters[*active].Label)
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
// empty $N are nothing, \$ is $
func plainSnippet(s string) string {
	return snippetText(s, map[string]string{})
}

func snippetText(s string, seen map[string]string) string {
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
				text = snippetText(inner[1:], seen)
			case strings.HasPrefix(inner, "|"):
				choices := strings.Trim(inner, "|")
				text, _, _ = strings.Cut(choices, ",")
			}
			seen[s[i+2:j]] = text
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
