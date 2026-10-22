package main

// The hover card, Expand Selection, call and type hierarchies, Go to Symbol in Project and
// document links of protocol 1.1 (hover-query, selection-ranges, hierarchy, workspace-symbols,
// document-links): textDocument/hover, selectionRange, prepareCallHierarchy and its calls,
// prepareTypeHierarchy and its super- and subtypes, workspace/symbol from every running server and
// documentLink, their positions made byte columns in the text of each file as rhun has it.

import (
	"context"
	"encoding/json"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// a little under rhun's limits of these kinds
const (
	waitHover           = 2800 * time.Millisecond
	waitSelectionRanges = 900 * time.Millisecond
	waitHierarchy       = 4500 * time.Millisecond
	waitDocumentLinks   = 1800 * time.Millisecond
	maxResolved         = 200 // workspace symbols and links resolved for what they lack
	maxSymbolFiles      = 200 // files read to place workspace symbols
)

var (
	hierarchyLimits = queryLimits{waitHierarchy, 29 * time.Second}
	symbolLimits    = queryLimits{waitHierarchy, 29 * time.Second}
)

// withContext is ctx (or a fresh one) limited to wait
func withContext(ctx context.Context, wait time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, wait)
}

// fileTexts are the texts of the files an answer names, read once each: the open document's,
// else the file on disk (at most limit files read; nil for the others)
type fileTexts struct {
	l     *lsp
	query *documentQuery
	texts map[string][]byte
	limit int
}

func (l *lsp) newFileTexts(q *documentQuery, limit int) *fileTexts {
	return &fileTexts{l: l, query: q, texts: map[string][]byte{}, limit: limit}
}

func (f *fileTexts) of(path string) []byte {
	if t, ok := f.texts[path]; ok {
		return t
	}
	var t []byte
	if f.query != nil && filepath.Clean(f.query.path) == filepath.Clean(path) {
		t = f.query.text
	} else if f.limit != 0 {
		f.limit--
		t, _, _ = f.l.textOf(path)
	}
	f.texts[path] = t
	return t
}

// placeRange is r in text, or LSP's positions taken as columns when the text is not at hand
func placeRange(text []byte, r lspRange) rhunext.Range {
	if text == nil {
		return rhunext.Range{Start: rhunext.Pos{Line: r.Start.Line + 1, Col: r.Start.Character + 1},
			End: rhunext.Pos{Line: r.End.Line + 1, Col: r.End.Character + 1}}
	}
	return rhunRange(text, r)
}

// ---- hover-query

// Hover answers hover-query from textDocument/hover: the range the server gives and its text,
// Markdown passing through when rhun agreed markdown, else plain text. A pointer's hover does not
// wait for a server still starting.
func (l *lsp) Hover(ext *rhunext.Ext, req rhunext.HoverRequest) *rhunext.Hover {
	if req.Kind == "pointer" && !l.serverReady(req.Doc) {
		return nil
	}
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	ctx, cancel := withContext(req.Context, waitHover)
	defer cancel()
	res, err := d.requestContext(ctx, "textDocument/hover", docParams(d, text, req.At))
	if err != nil || len(res) == 0 || string(res) == "null" {
		return nil
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
		Range    *lspRange       `json:"range"`
	}
	if json.Unmarshal(res, &h) != nil {
		return nil
	}
	out := &rhunext.Hover{}
	if l.has(featureMarkdown) {
		out.Text = strings.TrimSpace(markdownOf(h.Contents))
	} else {
		out.Text = hoverText(h.Contents)
	}
	if out.Text == "" {
		return nil
	}
	if h.Range != nil {
		out.Range = rhunRange(text, *h.Range)
	}
	return out
}

// serverReady is whether the document's server runs (without waiting for one starting)
func (l *lsp) serverReady(doc rhunext.Doc) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.docs[doc]
	return d != nil && d.server != nil && d.server.ready.Load()
}

// markdownOf is hover contents as Markdown: MarkupContent's markdown as it is, its plaintext
// escaped, MarkedStrings (Markdown, or code in a language) and arrays of them joined by rules
func markdownOf(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if s, err := jsonString(raw); err == nil {
		return s
	}
	if raw[0] == '[' {
		var arr []json.RawMessage
		json.Unmarshal(raw, &arr)
		var parts []string
		for _, a := range arr {
			if t := strings.TrimSpace(markdownOf(a)); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n---\n\n")
	}
	var x struct {
		Kind     string `json:"kind"`
		Language string `json:"language"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(raw, &x) != nil {
		return ""
	}
	switch {
	case x.Kind == "markdown":
		return x.Value
	case x.Kind != "":
		return escapeMarkdown(x.Value)
	case x.Language != "":
		return "```" + x.Language + "\n" + strings.TrimRight(x.Value, "\n") + "\n```"
	}
	return x.Value
}

// escapeMarkdown makes plain text show as it is in Markdown: punctuation escaped and each line
// break kept
func escapeMarkdown(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("  \n")
		case r < 0x80 && strings.ContainsRune("\\`*_{}[]()#+-.!|<>~", r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// documentation is a completion item's or signature's documentation as rhun shows it: Markdown
// with markdown agreed, else plain text
func (l *lsp) documentation(raw json.RawMessage) string {
	if !l.has(featureMarkdown) {
		return docText(raw)
	}
	return strings.TrimSpace(markdownOf(raw))
}

// ---- selection-ranges

// SelectionRanges answers selection-ranges from textDocument/selectionRange: each position's
// chain of parents, innermost first
func (l *lsp) SelectionRanges(ext *rhunext.Ext, req rhunext.SelectionRangesRequest) [][]rhunext.Range {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	positions := make([]lspPosition, len(req.Positions))
	for i, p := range req.Positions {
		positions[i] = lspPos(text, byteOffset(text, p))
	}
	ctx, cancel := withContext(req.Context, waitSelectionRanges)
	defer cancel()
	res, err := d.requestContext(ctx, "textDocument/selectionRange", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)}, "positions": positions})
	if err != nil {
		return nil
	}
	return selectionGroups(res, text)
}

// selectionGroups reads SelectionRange[]: each one's range and its parents'
func selectionGroups(res json.RawMessage, text []byte) [][]rhunext.Range {
	type selection struct {
		Range  lspRange        `json:"range"`
		Parent json.RawMessage `json:"parent"`
	}
	var list []json.RawMessage
	if json.Unmarshal(res, &list) != nil {
		return nil
	}
	out := make([][]rhunext.Range, 0, len(list))
	for _, raw := range list {
		var group []rhunext.Range
		for len(raw) > 0 && string(raw) != "null" && len(group) < rhunext.MaxSelectionRanges {
			var sel selection
			if json.Unmarshal(raw, &sel) != nil {
				break
			}
			group = append(group, rhunRange(text, sel.Range))
			raw = sel.Parent
		}
		out = append(out, group)
	}
	return out
}

// ---- hierarchy

// hierarchyData is an item's DATA: the server that made it (its language and generation, so an
// item of a server since restarted is refused) and the item as the server gave it
type hierarchyData struct {
	Lang       string          `json:"l"`
	Generation uint64          `json:"g"`
	Item       json.RawMessage `json:"i"`
}

type lspHierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}

// hierarchyItem is a server's CallHierarchyItem or TypeHierarchyItem as rhun's item, false when
// it names no file or its data is too long
func hierarchyItem(s *server, raw json.RawMessage, texts *fileTexts) (rhunext.HierarchyItem, bool) {
	var it lspHierarchyItem
	if json.Unmarshal(raw, &it) != nil || it.Name == "" {
		return rhunext.HierarchyItem{}, false
	}
	path := uriPath(it.URI)
	if !filepath.IsAbs(path) {
		return rhunext.HierarchyItem{}, false
	}
	data, _ := json.Marshal(hierarchyData{Lang: s.lang, Generation: s.generation, Item: raw})
	if len(data) > rhunext.MaxHierarchyData {
		return rhunext.HierarchyItem{}, false
	}
	text := texts.of(path)
	return rhunext.HierarchyItem{Kind: symbolKind(it.Kind), Name: it.Name, Detail: it.Detail, Target: rhunext.Target{Path: path},
		Range: placeRange(text, it.Range), Selection: placeRange(text, it.SelectionRange), Data: data}, true
}

// hierarchyRoots asks prepareCallHierarchy or prepareTypeHierarchy at the cursor
func (l *lsp) hierarchyRoots(req rhunext.HierarchyRequest, method string) []rhunext.HierarchyItem {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	ctx, cancel := withContext(req.Context, waitHierarchy)
	defer cancel()
	res, err := d.requestContext(ctx, method, docParams(d, text, req.At))
	if err != nil {
		return nil
	}
	var raws []json.RawMessage
	if json.Unmarshal(res, &raws) != nil {
		return nil
	}
	texts := l.newFileTexts(d, maxSymbolFiles)
	var out []rhunext.HierarchyItem
	for _, raw := range raws {
		if it, ok := hierarchyItem(s, raw, texts); ok {
			out = append(out, it)
		}
	}
	return out
}

// hierarchyServer is the running server an item's DATA names, and the item; nil when that server
// stopped or restarted since
func (l *lsp) hierarchyServer(data []byte) (*server, json.RawMessage) {
	var h hierarchyData
	if json.Unmarshal(data, &h) != nil || len(h.Item) == 0 {
		return nil, nil
	}
	l.mu.Lock()
	s := l.servers[h.Lang]
	l.mu.Unlock()
	if s == nil || !s.ready.Load() || s.generation != h.Generation {
		return nil, nil
	}
	return s, h.Item
}

// hierarchyCall sends a request about an item to its server, streaming its partial results
func hierarchyCall[T any](l *lsp, data []byte, out *rhunext.Stream, method string,
	convert func(s *server, item json.RawMessage, texts *fileTexts, raw json.RawMessage) []T,
	send func(*rhunext.Stream, []T) error) []T {
	s, item := l.hierarchyServer(data)
	if s == nil {
		return nil
	}
	var it lspHierarchyItem
	json.Unmarshal(item, &it)
	if !s.supports(method, selectorDocument{language: languageIDs[s.lang], scheme: "file", path: filepath.ToSlash(uriPath(it.URI))}) {
		return nil
	}
	texts := l.newFileTexts(nil, maxSymbolFiles)
	var mu sync.Mutex // texts is used by the partial results and by the answer
	conv := func(raw json.RawMessage) []T {
		mu.Lock()
		defer mu.Unlock()
		return convert(s, item, texts, raw)
	}
	params := map[string]any{"item": item}
	call := func(ctx context.Context) (json.RawMessage, error) { return s.callContext(ctx, method, params) }
	res, carried, err := streamCall(l, s, out, hierarchyLimits, params, call, conv, send)
	if err != nil && len(carried) == 0 {
		return nil
	}
	return append(carried, conv(res)...)
}

// CallHierarchy answers call-hierarchy: the roots at the cursor
func (l *lsp) CallHierarchy(ext *rhunext.Ext, req rhunext.HierarchyRequest) []rhunext.HierarchyItem {
	return l.hierarchyRoots(req, "textDocument/prepareCallHierarchy")
}

// calls reads CallHierarchyIncomingCall[] (the callers, their call sites in their own files) or
// CallHierarchyOutgoingCall[] (the callees, called at sites in the file of the item asked about)
func calls(s *server, asked json.RawMessage, texts *fileTexts, raw json.RawMessage, incoming bool) []rhunext.Call {
	var list []struct {
		From       json.RawMessage `json:"from"`
		To         json.RawMessage `json:"to"`
		FromRanges []lspRange      `json:"fromRanges"`
	}
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var askedItem lspHierarchyItem
	json.Unmarshal(asked, &askedItem)
	var out []rhunext.Call
	for _, c := range list {
		other, siteFile := c.To, uriPath(askedItem.URI)
		if incoming {
			var from lspHierarchyItem
			json.Unmarshal(c.From, &from)
			other, siteFile = c.From, uriPath(from.URI)
		}
		it, ok := hierarchyItem(s, other, texts)
		if !ok {
			continue
		}
		call := rhunext.Call{Item: it}
		text := texts.of(siteFile)
		for _, r := range c.FromRanges {
			call.Sites = append(call.Sites, placeRange(text, r))
		}
		if len(call.Sites) == 0 {
			call.Sites = []rhunext.Range{it.Selection}
		}
		out = append(out, call)
	}
	return out
}

func sendCalls(out *rhunext.Stream, c []rhunext.Call) error { return out.PartCalls(c) }

// CallsIn answers calls-in: callHierarchy/incomingCalls
func (l *lsp) CallsIn(ext *rhunext.Ext, data []byte, out *rhunext.Stream) []rhunext.Call {
	return hierarchyCall(l, data, out, "callHierarchy/incomingCalls",
		func(s *server, item json.RawMessage, texts *fileTexts, raw json.RawMessage) []rhunext.Call {
			return calls(s, item, texts, raw, true)
		}, sendCalls)
}

// CallsOut answers calls-out: callHierarchy/outgoingCalls
func (l *lsp) CallsOut(ext *rhunext.Ext, data []byte, out *rhunext.Stream) []rhunext.Call {
	return hierarchyCall(l, data, out, "callHierarchy/outgoingCalls",
		func(s *server, item json.RawMessage, texts *fileTexts, raw json.RawMessage) []rhunext.Call {
			return calls(s, item, texts, raw, false)
		}, sendCalls)
}

// TypeHierarchy answers type-hierarchy: the roots at the cursor
func (l *lsp) TypeHierarchy(ext *rhunext.Ext, req rhunext.HierarchyRequest) []rhunext.HierarchyItem {
	return l.hierarchyRoots(req, "textDocument/prepareTypeHierarchy")
}

func typeItems(s *server, _ json.RawMessage, texts *fileTexts, raw json.RawMessage) []rhunext.HierarchyItem {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []rhunext.HierarchyItem
	for _, r := range list {
		if it, ok := hierarchyItem(s, r, texts); ok {
			out = append(out, it)
		}
	}
	return out
}

func sendItems(out *rhunext.Stream, items []rhunext.HierarchyItem) error {
	return out.PartHierarchyItems(items)
}

// Supertypes answers supertypes: typeHierarchy/supertypes
func (l *lsp) Supertypes(ext *rhunext.Ext, data []byte, out *rhunext.Stream) []rhunext.HierarchyItem {
	return hierarchyCall(l, data, out, "typeHierarchy/supertypes", typeItems, sendItems)
}

// Subtypes answers subtypes: typeHierarchy/subtypes
func (l *lsp) Subtypes(ext *rhunext.Ext, data []byte, out *rhunext.Stream) []rhunext.HierarchyItem {
	return hierarchyCall(l, data, out, "typeHierarchy/subtypes", typeItems, sendItems)
}

// ---- workspace-symbols

// lspWorkspaceSymbol is a SymbolInformation or a WorkspaceSymbol (whose location may lack a range
// until resolved)
type lspWorkspaceSymbol struct {
	Name      string          `json:"name"`
	Kind      int             `json:"kind"`
	Container string          `json:"containerName"`
	Location  json.RawMessage `json:"location"`
}

// WorkspaceSymbols answers workspace-symbols from every running server that offers
// workspace/symbol, each server's symbols a part as they come (query-stream); symbols without a
// range are resolved (workspaceSymbol/resolve) for the first maxResolved
func (l *lsp) WorkspaceSymbols(ext *rhunext.Ext, req rhunext.WorkspaceSymbolsRequest, out *rhunext.Stream) []rhunext.WorkspaceSymbol {
	l.mu.Lock()
	var running []*server
	for _, s := range l.servers {
		if s != nil && s.ready.Load() && s.supports("workspace/symbol", selectorDocument{language: languageIDs[s.lang], scheme: "file"}) {
			running = append(running, s)
		}
	}
	l.mu.Unlock()
	var sendMu sync.Mutex
	send := func(o *rhunext.Stream, syms []rhunext.WorkspaceSymbol) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return o.PartWorkspaceSymbols(syms)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var all []rhunext.WorkspaceSymbol
	resolveBudget := maxResolved
	for _, s := range running {
		wg.Add(1)
		go func(s *server) {
			defer wg.Done()
			texts := l.newFileTexts(nil, maxSymbolFiles)
			var tmu sync.Mutex
			resolves := s.supports("workspaceSymbol/resolve", selectorDocument{language: languageIDs[s.lang], scheme: "file"})
			conv := func(raw json.RawMessage) []rhunext.WorkspaceSymbol {
				var list []json.RawMessage
				if json.Unmarshal(raw, &list) != nil {
					return nil
				}
				tmu.Lock()
				defer tmu.Unlock()
				var syms []rhunext.WorkspaceSymbol
				for _, item := range list {
					var ws lspWorkspaceSymbol
					if json.Unmarshal(item, &ws) != nil || ws.Name == "" {
						continue
					}
					uri, r, ok := symbolLocation(ws.Location)
					if !ok && resolves {
						mu.Lock()
						budget := resolveBudget > 0
						resolveBudget--
						mu.Unlock()
						if budget {
							uri, r, _ = resolveWorkspaceSymbol(s, out, item)
						}
					}
					if uri == "" {
						continue
					}
					path := uriPath(uri)
					if !filepath.IsAbs(path) {
						continue
					}
					var rg rhunext.Range
					if r != nil {
						rg = placeRange(texts.of(path), *r)
					} else {
						rg = rhunext.Range{Start: rhunext.Pos{Line: 1, Col: 1}, End: rhunext.Pos{Line: 1, Col: 1}}
					}
					syms = append(syms, rhunext.WorkspaceSymbol{Target: rhunext.Target{Path: path}, Range: rg,
						Kind: symbolKind(ws.Kind), Name: ws.Name, Detail: ws.Container})
				}
				return syms
			}
			params := map[string]any{"query": req.Query}
			call := func(ctx context.Context) (json.RawMessage, error) {
				return s.callContext(ctx, "workspace/symbol", params)
			}
			res, carried, err := streamCall(l, s, out, symbolLimits, params, call, conv, send)
			if err != nil && len(carried) == 0 {
				return
			}
			syms := append(carried, conv(res)...)
			syms = streamRest(l, out, syms, symbolSize, send)
			mu.Lock()
			all = append(all, syms...)
			mu.Unlock()
		}(s)
	}
	wg.Wait()
	return all
}

func symbolSize(s rhunext.WorkspaceSymbol) int {
	return len(s.Target.String()) + len(s.Name) + len(s.Detail) + 48
}

// symbolLocation is a workspace symbol's file and range; false without a range (resolve it)
func symbolLocation(raw json.RawMessage) (string, *lspRange, bool) {
	var loc struct {
		URI   string    `json:"uri"`
		Range *lspRange `json:"range"`
	}
	if json.Unmarshal(raw, &loc) != nil || loc.URI == "" {
		return "", nil, false
	}
	return loc.URI, loc.Range, loc.Range != nil
}

func resolveWorkspaceSymbol(s *server, out *rhunext.Stream, raw json.RawMessage) (string, *lspRange, bool) {
	parent := context.Background()
	if out != nil && out.Context != nil {
		parent = out.Context
	}
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	res, err := s.callContext(ctx, "workspaceSymbol/resolve", raw)
	if err != nil {
		var ws lspWorkspaceSymbol
		json.Unmarshal(raw, &ws)
		uri, _, _ := symbolLocation(ws.Location)
		return uri, nil, uri != ""
	}
	var ws lspWorkspaceSymbol
	if json.Unmarshal(res, &ws) != nil {
		return "", nil, false
	}
	uri, r, ok := symbolLocation(ws.Location)
	return uri, r, ok || uri != ""
}

// ---- document-links

type lspDocumentLink struct {
	Range   lspRange        `json:"range"`
	Target  string          `json:"target"`
	Tooltip string          `json:"tooltip"`
	Data    json.RawMessage `json:"data"`
}

// DocumentLinks answers document-links from textDocument/documentLink, resolving the targets the
// first maxResolved links lack (documentLink/resolve)
func (l *lsp) DocumentLinks(ext *rhunext.Ext, req rhunext.DocRequest) []rhunext.DocumentLink {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	ctx, cancel := withContext(req.Context, waitDocumentLinks)
	defer cancel()
	res, err := d.requestContext(ctx, "textDocument/documentLink", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
	if err != nil {
		return nil
	}
	raws, err := boundedRawArray(res, rhunext.MaxRecords)
	if err != nil {
		return nil
	}
	resolves := s.supports("documentLink/resolve", selectorFor(d.origin))
	resolved := 0
	var out []rhunext.DocumentLink
	for _, raw := range raws {
		var link lspDocumentLink
		if json.Unmarshal(raw, &link) != nil {
			continue
		}
		if link.Target == "" && resolves && resolved < maxResolved && ctx.Err() == nil {
			resolved++
			if r, err := d.requestContext(ctx, "documentLink/resolve", raw); err == nil {
				var got lspDocumentLink
				if json.Unmarshal(r, &got) == nil {
					link.Target = got.Target
					if got.Tooltip != "" {
						link.Tooltip = got.Tooltip
					}
				}
			}
		}
		target := linkTarget(link.Target)
		if target == "" {
			continue
		}
		out = append(out, rhunext.DocumentLink{Range: rhunRange(text, link.Range), Target: target, Tip: oneLineText(link.Tooltip)})
	}
	return out
}

// lineFragment is a file URI's #L10, #10, #L10,5 or #10:5 (one-based)
var lineFragment = regexp.MustCompile(`^L?(\d+)(?:[,:]C?(\d+))?$`)

// linkTarget is a link's target as rhun takes it: an http(s) URL as it is, a file URI as its path
// (with a line and column from #L10,5); "" for anything else
func linkTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if rhunext.ValidLinkTarget(target) {
			return target
		}
	case "file":
		fragment := u.Fragment
		u.Fragment = ""
		path := uriPath(u.String())
		if !filepath.IsAbs(path) {
			return ""
		}
		var at rhunext.Pos
		if m := lineFragment.FindStringSubmatch(fragment); m != nil {
			at.Line, _ = strconv.Atoi(m[1])
			if m[2] != "" {
				at.Col, _ = strconv.Atoi(m[2])
			}
		}
		if t := rhunext.LinkPath(path, at); rhunext.ValidLinkTarget(t) {
			return t
		}
	}
	return ""
}
