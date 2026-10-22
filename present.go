package main

// Editor presentation (protocol 1.1, rhun 0.18) from the language server: folding ranges
// (textDocument/foldingRange), semantic tokens (textDocument/semanticTokens/full, /full/delta and
// /range: the server's legend registered with rhun, its UTF-16 tokens turned into byte columns and
// always sent whole), inlay hints (textDocument/inlayHint), code lens (textDocument/codeLens and
// codeLens/resolve for the lines asked about), document highlights (textDocument/documentHighlight)
// and colors (textDocument/documentColor, textDocument/colorPresentation for a swatch's new color).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// a little under what rhun waits for each (v1.1.md, Presentation providers)
const (
	waitFolding    = 25 * time.Second
	waitTokens     = 25 * time.Second
	waitHints      = 8 * time.Second
	waitLens       = 8 * time.Second
	waitHighlights = 4 * time.Second
	waitColors     = 8 * time.Second
	waitInline     = 4 * time.Second
)

// currentSoon is l.current, waiting while the document is still reaching its server (rhun asks
// about the active document as soon as it opens, before the server has started). The wait holds
// the SDK's event loop, so edits made meanwhile have not reached l.docs: while it waits the
// server's readiness does not open the document (awaitServer), and when the server is ready rhun
// is asked whether the document is still at version. If it is, this request opens it; if not, the
// edits held back open it with the newest text once the request has returned nothing (rhun asks
// again after them).
func (l *lsp) currentSoon(id rhunext.Doc, version int64, wait time.Duration) (*documentQuery, []byte, *server) {
	l.mu.Lock()
	doc := l.docs[id]
	if doc == nil || doc.version != version {
		l.mu.Unlock()
		return nil, nil, nil
	}
	if doc.server != nil && doc.syncedServer == doc.server && doc.syncedVersion == version {
		l.mu.Unlock()
		return l.current(id, version)
	}
	doc.presWaiting++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		doc.presWaiting--
		l.mu.Unlock()
	}()
	deadline := time.Now().Add(wait)
	for first := true; time.Now().Before(deadline); first = false {
		if !first {
			time.Sleep(150 * time.Millisecond)
		}
		l.mu.Lock()
		gone := l.docs[id] != doc || doc.version != version
		s := doc.server
		l.mu.Unlock()
		if gone || !l.stillAt(id, version) {
			return nil, nil, nil
		}
		if s != nil && ready(s) {
			return l.current(id, version)
		}
	}
	return nil, nil, nil
}

// stillAt says whether rhun's document is still at version (false when it cannot tell)
func (l *lsp) stillAt(id rhunext.Doc, version int64) bool {
	if l.ext == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, now, err := l.ext.TextContext(ctx, id)
	return err == nil && now == version
}

func init() {
	for method, info := range map[string]capabilityInfo{
		"textDocument/foldingRange": {"foldingRangeProvider", "textDocument.foldingRange", false}, "textDocument/semanticTokens": {"semanticTokensProvider", "textDocument.semanticTokens", false},
		"textDocument/inlayHint": {"inlayHintProvider", "textDocument.inlayHint", false}, "textDocument/codeLens": {"codeLensProvider", "textDocument.codeLens", true},
		"textDocument/documentHighlight": {"documentHighlightProvider", "textDocument.documentHighlight", false}, "textDocument/documentColor": {"colorProvider", "textDocument.colorProvider", false},
		"textDocument/inlineCompletion": {"inlineCompletionProvider", "textDocument.inlineCompletion", false},
	} {
		capabilityFields[method] = info
	}
	// a capability's own requests
	for method, base := range map[string]string{"textDocument/semanticTokens/full": "textDocument/semanticTokens",
		"textDocument/semanticTokens/full/delta": "textDocument/semanticTokens", "textDocument/semanticTokens/range": "textDocument/semanticTokens",
		"textDocument/colorPresentation": "textDocument/documentColor"} {
		dependent[method] = struct {
			base   string
			option bool
		}{base, false}
	}
	dependent["codeLens/resolve"] = struct {
		base   string
		option bool
	}{"textDocument/codeLens", true}
	providerOption["textDocument/codeLens"] = "resolveProvider"
}

// LSP's standard token types and modifiers, which the client says it understands
var tokenTypes = []string{"namespace", "type", "class", "enum", "interface", "struct", "typeParameter",
	"parameter", "variable", "property", "enumMember", "event", "function", "method", "macro", "keyword",
	"modifier", "comment", "string", "number", "regexp", "operator", "decorator"}
var tokenModifiers = []string{"declaration", "definition", "readonly", "static", "deprecated", "abstract", "async",
	"modification", "documentation", "defaultLibrary"}

// presentationCaps are the client capabilities of the presentation features rhun agreed
func (l *lsp) presentationCaps(text map[string]any) {
	if l.ext == nil {
		return
	}
	if l.ext.Has(rhunext.FeatureFoldingRanges) {
		text["foldingRange"] = map[string]any{"lineFoldingOnly": true,
			"foldingRangeKind": map[string]any{"valueSet": []string{"comment", "imports", "region"}},
			"foldingRange":     map[string]any{"collapsedText": true}}
	}
	if l.ext.Has(rhunext.FeatureSemanticTokens) {
		text["semanticTokens"] = map[string]any{"requests": map[string]any{"range": true, "full": map[string]any{"delta": true}},
			"tokenTypes": tokenTypes, "tokenModifiers": tokenModifiers, "formats": []string{"relative"},
			"overlappingTokenSupport": false, "multilineTokenSupport": false, "augmentsSyntaxTokens": true}
	}
	if l.ext.Has(rhunext.FeatureInlayHints) {
		text["inlayHint"] = map[string]any{}
	}
	if l.ext.Has(rhunext.FeatureCodeLens) {
		text["codeLens"] = map[string]any{}
	}
	if l.ext.Has(rhunext.FeatureHighlights) {
		text["documentHighlight"] = map[string]any{}
	}
	if l.ext.Has(rhunext.FeatureColors) {
		text["colorProvider"] = map[string]any{"dynamicRegistration": true}
	}
	if l.ext.Has(rhunext.FeatureInlineComplete) {
		text["inlineCompletion"] = map[string]any{}
	}
}

// ---- legends

// legendName is the server's legend's name in rhun: its program's name, as rhun takes it
func legendName(s *server) string {
	var b strings.Builder
	for _, r := range s.name() {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			b.WriteRune(r)
		}
		if b.Len() == 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "lsp"
	}
	return b.String()
}

// modifierEffects are the effects rhun gives a legend's modifiers beyond its own table, as the
// servers' VS Code extensions draw them
var modifierEffects = map[string]string{"mutable": "underline"}

// registerLegend tells rhun the server's semantic token legend (after initialize)
func (s *server) registerLegend(caps map[string]json.RawMessage) {
	ext := s.l.ext
	if ext == nil || !ext.Has(rhunext.FeatureSemanticTokens) {
		return
	}
	var opt struct {
		Legend struct {
			TokenTypes     []string `json:"tokenTypes"`
			TokenModifiers []string `json:"tokenModifiers"`
		} `json:"legend"`
	}
	if json.Unmarshal(caps["semanticTokensProvider"], &opt) != nil || len(opt.Legend.TokenTypes) == 0 {
		return
	}
	types := clean(opt.Legend.TokenTypes, 256)
	mods := clean(opt.Legend.TokenModifiers, 64)
	for i, m := range mods {
		if e := modifierEffects[m]; e != "" {
			mods[i] = m + "=" + e
		}
	}
	go func() {
		if err := ext.SemanticLegend(legendName(s), types, mods); err != nil {
			ext.Logf("semantic legend: %v", err)
		}
	}()
}

// clean keeps names rhun takes ([A-Za-z0-9_.-], 1 to 64 bytes): another becomes "unknown" so the
// indices still line up
func clean(names []string, max int) []string {
	if len(names) > max {
		names = names[:max]
	}
	out := make([]string, len(names))
	for i, n := range names {
		ok := n != "" && len(n) <= 64
		for _, r := range n {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
				ok = false
			}
		}
		if !ok {
			n = "unknown"
		}
		out[i] = n
	}
	return out
}

// ---- folding ranges

func (l *lsp) FoldingRanges(ext *rhunext.Ext, req rhunext.DocRequest) []rhunext.FoldingRange {
	d, _, s := l.currentSoon(req.Doc, req.Version, waitFolding/2)
	if d == nil || !s.supports("textDocument/foldingRange", selectorFor(d.origin)) {
		return nil
	}
	res, err := askFor(s, d, waitFolding, "textDocument/foldingRange",
		map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
	if err != nil {
		return nil
	}
	var ranges []struct {
		StartLine     int    `json:"startLine"`
		EndLine       int    `json:"endLine"`
		Kind          string `json:"kind"`
		CollapsedText string `json:"collapsedText"`
	}
	if json.Unmarshal(res, &ranges) != nil {
		return nil
	}
	out := make([]rhunext.FoldingRange, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, rhunext.FoldingRange{Start: r.StartLine + 1, End: r.EndLine + 1, Kind: r.Kind, Text: r.CollapsedText})
	}
	return out
}

// ---- semantic tokens

// tokenCache keeps each document's last result from its server: a delta is applied here, and
// rhun always gets the whole array (in byte columns)
type tokenCache struct {
	mu   sync.Mutex
	docs map[rhunext.Doc]tokenResult
}

type tokenResult struct {
	server *server
	result string
	data   []uint32 // the server's, UTF-16
}

var tokens = tokenCache{docs: map[rhunext.Doc]tokenResult{}}

func (l *lsp) SemanticTokens(ext *rhunext.Ext, req rhunext.SemanticTokensRequest) *rhunext.SemanticTokens {
	d, text, s := l.currentSoon(req.Doc, req.Version, waitTokens/2)
	if d == nil || !s.supports("textDocument/semanticTokens", selectorFor(d.origin)) {
		return nil
	}
	uri := map[string]any{"uri": fileURI(d.path)}
	if req.Range {
		r := lspRange{Start: lspPosition{Line: req.First - 1}, End: lspPosition{Line: req.Last}}
		res, err := askFor(s, d, waitTokens, "textDocument/semanticTokens/range", map[string]any{"textDocument": uri, "range": r})
		var got struct {
			Data []uint32 `json:"data"`
		}
		if err != nil || json.Unmarshal(res, &got) != nil {
			return nil
		}
		return &rhunext.SemanticTokens{Legend: legendName(s), Data: byteTokens(text, got.Data)}
	}
	tokens.mu.Lock()
	prev, have := tokens.docs[req.Doc]
	tokens.mu.Unlock()
	var data []uint32
	var result string
	if have && prev.server == s && prev.result != "" && req.Prev == prev.result {
		res, err := askFor(s, d, waitTokens, "textDocument/semanticTokens/full/delta",
			map[string]any{"textDocument": uri, "previousResultId": prev.result})
		if err != nil {
			return nil
		}
		var got struct {
			ResultID string    `json:"resultId"`
			Data     *[]uint32 `json:"data"`
			Edits    []struct {
				Start       int      `json:"start"`
				DeleteCount int      `json:"deleteCount"`
				Data        []uint32 `json:"data"`
			} `json:"edits"`
		}
		if json.Unmarshal(res, &got) != nil {
			return nil
		}
		result = got.ResultID
		if got.Data != nil {
			data = *got.Data
		} else {
			data = append([]uint32(nil), prev.data...)
			// the edits, from the last, on the previous array
			for i := len(got.Edits) - 1; i >= 0; i-- {
				e := got.Edits[i]
				if e.Start < 0 || e.DeleteCount < 0 || e.Start+e.DeleteCount > len(data) {
					return nil
				}
				data = append(data[:e.Start], append(append([]uint32(nil), e.Data...), data[e.Start+e.DeleteCount:]...)...)
			}
		}
	} else {
		res, err := askFor(s, d, waitTokens, "textDocument/semanticTokens/full", map[string]any{"textDocument": uri})
		var got struct {
			ResultID string   `json:"resultId"`
			Data     []uint32 `json:"data"`
		}
		if err != nil || json.Unmarshal(res, &got) != nil {
			return nil
		}
		result, data = got.ResultID, got.Data
	}
	tokens.mu.Lock()
	tokens.docs[req.Doc] = tokenResult{server: s, result: result, data: data}
	tokens.mu.Unlock()
	if !validResult(result) {
		result = ""
	}
	return &rhunext.SemanticTokens{Result: result, Legend: legendName(s), Data: byteTokens(text, data)}
}

func validResult(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			return false
		}
	}
	return true
}

// byteTokens is LSP's relative token array (UTF-16 columns and lengths) in byte columns, for text
func byteTokens(text []byte, data []uint32) []uint32 {
	out := make([]uint32, 0, len(data))
	lines := bytes.Split(text, []byte{'\n'})
	line, ch := 0, 0            // the server's absolute position
	prevLine, prevByte := -1, 0 // the last token's in bytes
	for i := 0; i+5 <= len(data); i += 5 {
		dl, dc := int(data[i]), int(data[i+1])
		if dl > 0 {
			line, ch = line+dl, dc
		} else {
			ch += dc
		}
		if line >= len(lines) {
			break
		}
		start, end := utf16Bytes(lines[line], ch), utf16Bytes(lines[line], ch+int(data[i+2]))
		if end <= start {
			continue
		}
		var bdl, bdc int
		if line != prevLine {
			bdl = line - max(prevLine, 0)
			if prevLine < 0 {
				bdl = line
			}
			bdc = start
		} else {
			bdc = start - prevByte
		}
		out = append(out, uint32(bdl), uint32(bdc), uint32(end-start), data[i+3], data[i+4])
		prevLine, prevByte = line, start
	}
	return out
}

// utf16Bytes is the byte offset of UTF-16 unit u in line (its end when past it)
func utf16Bytes(line []byte, u int) int {
	units, b := 0, 0
	for b < len(line) && units < u {
		r, n := utf8.DecodeRune(line[b:])
		units += utf16.RuneLen(r)
		b += n
	}
	return b
}

// ---- inlay hints

type lspHintPart struct {
	Value    string          `json:"value"`
	Tooltip  json.RawMessage `json:"tooltip"`
	Location *location       `json:"location"`
}

func (l *lsp) InlayHints(ext *rhunext.Ext, req rhunext.LinesRequest) []rhunext.InlayHint {
	d, text, s := l.currentSoon(req.Doc, req.Version, waitHints/2)
	if d == nil || !s.supports("textDocument/inlayHint", selectorFor(d.origin)) {
		return nil
	}
	r := lspRange{Start: lspPosition{Line: req.First - 1}, End: lspPosition{Line: req.Last}}
	res, err := askFor(s, d, waitHints, "textDocument/inlayHint",
		map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}, "range": r})
	if err != nil {
		return nil
	}
	var hints []struct {
		Position     lspPosition     `json:"position"`
		Label        json.RawMessage `json:"label"`
		Kind         int             `json:"kind"`
		Tooltip      json.RawMessage `json:"tooltip"`
		PaddingLeft  bool            `json:"paddingLeft"`
		PaddingRight bool            `json:"paddingRight"`
		TextEdits    []struct {
			Range   lspRange `json:"range"`
			NewText string   `json:"newText"`
		} `json:"textEdits"`
	}
	if json.Unmarshal(res, &hints) != nil {
		return nil
	}
	out := make([]rhunext.InlayHint, 0, len(hints))
	for _, h := range hints {
		var parts []rhunext.InlayHintPart
		var label string
		if json.Unmarshal(h.Label, &label) == nil {
			parts = []rhunext.InlayHintPart{{Label: label, Tip: markupText(h.Tooltip)}}
		} else {
			var lps []lspHintPart
			if json.Unmarshal(h.Label, &lps) != nil {
				continue
			}
			for _, p := range lps {
				part := rhunext.InlayHintPart{Label: p.Value, Tip: markupText(p.Tooltip)}
				if p.Location != nil {
					path := uriPath(p.Location.URI)
					target, _, err := l.textOf(path)
					if err == nil {
						if path == d.path {
							path = ""
						}
						part.Goto = &rhunext.InlayGoto{Path: path, At: rhunPos(target, p.Location.Range.Start)}
					}
				}
				parts = append(parts, part)
			}
			if len(parts) > 0 && parts[0].Tip == "" {
				parts[0].Tip = markupText(h.Tooltip)
			}
		}
		if len(parts) == 0 {
			continue
		}
		if h.PaddingLeft {
			parts[0].Label = " " + parts[0].Label
		}
		if h.PaddingRight {
			parts[len(parts)-1].Label += " "
		}
		hint := rhunext.InlayHint{At: rhunPos(text, h.Position), Parts: parts, Before: h.Kind == 2}
		for _, e := range h.TextEdits {
			hint.Edits = append(hint.Edits, rhunext.InlayEdit{Range: rhunRange(text, e.Range), Text: e.NewText})
		}
		out = append(out, hint)
	}
	return out
}

// markupText is a string or MarkupContent's value, or ""
func markupText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &m) == nil {
		if m.Kind == "markdown" {
			return unmark(m.Value)
		}
		return m.Value
	}
	return ""
}

// ---- code lens

type lspLens struct {
	Range   lspRange        `json:"range"`
	Command *lspCommand     `json:"command,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// lensRef is the command reference that runs a lens's (or an inline item's) server command
// (codeactions.go's exec, feature command-args), nil without one
func (l *lsp) lensRef(s *server, c *lspCommand) *rhunext.CommandRef {
	if c == nil || c.Command == "" {
		return nil
	}
	return l.execRef(s, *c)
}

func (l *lsp) CodeLenses(ext *rhunext.Ext, req rhunext.LinesRequest) []rhunext.CodeLens {
	d, text, s := l.currentSoon(req.Doc, req.Version, waitLens/2)
	if d == nil || !s.supports("textDocument/codeLens", selectorFor(d.origin)) {
		return nil
	}
	res, err := askFor(s, d, waitLens, "textDocument/codeLens", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
	if err != nil {
		return nil
	}
	var lenses []lspLens
	if json.Unmarshal(res, &lenses) != nil {
		return nil
	}
	out := make([]rhunext.CodeLens, 0, len(lenses))
	for _, ln := range lenses {
		line := ln.Range.Start.Line + 1
		if ln.Command == nil && line >= req.First && line <= req.Last {
			// resolved for the lines asked about
			if raw, err := askFor(s, d, waitLens, "codeLens/resolve", ln); err == nil {
				var got lspLens
				if json.Unmarshal(raw, &got) == nil {
					ln = got
				}
			}
		}
		lens := rhunext.CodeLens{Line: rhunPos(text, ln.Range.Start).Line}
		if ln.Command != nil {
			lens.Title = ln.Command.Title
			lens.Command = l.lensRef(s, ln.Command)
		}
		out = append(out, lens)
	}
	return out
}

// ---- document highlights

func (l *lsp) Highlights(ext *rhunext.Ext, req rhunext.PosRequest) []rhunext.Highlight {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !s.supports("textDocument/documentHighlight", selectorFor(d.origin)) {
		return nil
	}
	res, err := askFor(s, d, waitHighlights, "textDocument/documentHighlight", docParams(d, text, req.At))
	if err != nil {
		return nil
	}
	var hs []struct {
		Range lspRange `json:"range"`
		Kind  int      `json:"kind"`
	}
	if json.Unmarshal(res, &hs) != nil {
		return nil
	}
	out := make([]rhunext.Highlight, 0, len(hs))
	for _, h := range hs {
		out = append(out, rhunext.Highlight{Range: rhunRange(text, h.Range), Read: h.Kind == 2, Write: h.Kind == 3})
	}
	return out
}

// ---- colors

type lspColor struct {
	Red   float64 `json:"red"`
	Green float64 `json:"green"`
	Blue  float64 `json:"blue"`
	Alpha float64 `json:"alpha"`
}

func channel(v float64) uint8 { return uint8(math.Round(math.Max(0, math.Min(1, v)) * 255)) }

func (l *lsp) Colors(ext *rhunext.Ext, req rhunext.LinesRequest) []rhunext.DocumentColor {
	d, text, s := l.currentSoon(req.Doc, req.Version, waitColors/2)
	if d == nil || !s.supports("textDocument/documentColor", selectorFor(d.origin)) {
		return nil
	}
	res, err := askFor(s, d, waitColors, "textDocument/documentColor", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}})
	if err != nil {
		return nil
	}
	var cs []struct {
		Range lspRange `json:"range"`
		Color lspColor `json:"color"`
	}
	if json.Unmarshal(res, &cs) != nil {
		return nil
	}
	out := make([]rhunext.DocumentColor, 0, len(cs))
	for _, c := range cs {
		r := rhunRange(text, c.Range)
		if r.Start.Line < req.First || r.Start.Line > req.Last {
			continue
		}
		out = append(out, rhunext.DocumentColor{Range: r, R: channel(c.Color.Red), G: channel(c.Color.Green),
			B: channel(c.Color.Blue), A: channel(c.Color.Alpha)})
	}
	return out
}

func (l *lsp) ColorEdit(ext *rhunext.Ext, req rhunext.ColorEditRequest) (rhunext.WorkspaceEdit, error) {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil {
		return rhunext.WorkspaceEdit{}, fmt.Errorf("the document changed")
	}
	r := lspRange{Start: lspPos(text, byteOffset(text, req.Range.Start)), End: lspPos(text, byteOffset(text, req.Range.End))}
	color := lspColor{Red: float64(req.R) / 255, Green: float64(req.G) / 255, Blue: float64(req.B) / 255, Alpha: float64(req.A) / 255}
	res, err := askFor(s, d, waitColors, "textDocument/colorPresentation",
		map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}, "color": color, "range": r})
	if err != nil {
		return rhunext.WorkspaceEdit{}, err
	}
	var ps []struct {
		Label    string `json:"label"`
		TextEdit *struct {
			Range   lspRange `json:"range"`
			NewText string   `json:"newText"`
		} `json:"textEdit"`
	}
	if json.Unmarshal(res, &ps) != nil || len(ps) == 0 {
		return rhunext.WorkspaceEdit{}, fmt.Errorf("the language server offers no way to write that color")
	}
	edit := rhunext.TextEdit{Range: req.Range, Text: ps[0].Label}
	if ps[0].TextEdit != nil {
		edit = rhunext.TextEdit{Range: rhunRange(text, ps[0].TextEdit.Range), Text: ps[0].TextEdit.NewText}
	}
	return rhunext.WorkspaceEdit{Files: []rhunext.FileEdit{{Target: rhunext.Target{Doc: req.Doc}, Version: req.Version,
		Edits: []rhunext.TextEdit{edit}}}}, nil
}

// ---- inline completion

type lspInlineItem struct {
	InsertText json.RawMessage `json:"insertText"`
	Range      *lspRange       `json:"range,omitempty"`
	Command    *lspCommand     `json:"command,omitempty"`
}

// inlineText is an item's insertText: a string, or a StringValue whose snippet syntax is taken
// out ($1, ${1}, ${1:text} become text; \$ a dollar)
func inlineText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var sv struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &sv) != nil {
		return ""
	}
	if sv.Kind != "snippet" {
		return sv.Value
	}
	return snippetPlain(sv.Value)
}

func snippetPlain(v string) string {
	var b strings.Builder
	open := 0 // placeholders whose text is being copied
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' && i+1 < len(v) && strings.IndexByte("$}\\", v[i+1]) >= 0:
			i++
			b.WriteByte(v[i])
		case c == '$' && i+1 < len(v) && v[i+1] >= '0' && v[i+1] <= '9':
			for i+1 < len(v) && v[i+1] >= '0' && v[i+1] <= '9' {
				i++
			}
		case c == '$' && i+1 < len(v) && v[i+1] == '{':
			j := i + 2
			for j < len(v) && v[j] >= '0' && v[j] <= '9' {
				j++
			}
			if j == i+2 || j >= len(v) {
				b.WriteByte(c)
				continue
			}
			if v[j] == ':' {
				i = j // the placeholder's text follows; its closing brace is dropped below
				open++
				continue
			}
			if v[j] == '}' {
				i = j
				continue
			}
			b.WriteByte(c)
		case c == '}' && open > 0:
			open-- // a placeholder's end
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func (l *lsp) InlineComplete(ext *rhunext.Ext, req rhunext.InlineRequest) []rhunext.InlineItem {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !s.supports("textDocument/inlineCompletion", selectorFor(d.origin)) {
		return nil
	}
	params := docParams(d, text, req.At)
	kind := 2 // automatic
	if req.Kind == "invoked" {
		kind = 1
	}
	params["context"] = map[string]any{"triggerKind": kind}
	res, err := askFor(s, d, waitInline, "textDocument/inlineCompletion", params)
	if err != nil {
		return nil
	}
	var items []lspInlineItem
	if json.Unmarshal(res, &items) != nil {
		var list struct {
			Items []lspInlineItem `json:"items"`
		}
		if json.Unmarshal(res, &list) != nil {
			return nil
		}
		items = list.Items
	}
	out := make([]rhunext.InlineItem, 0, len(items))
	for _, it := range items {
		t := inlineText(it.InsertText)
		if t == "" {
			continue
		}
		r := rhunext.Range{Start: req.At, End: req.At}
		if it.Range != nil {
			r = rhunRange(text, *it.Range)
		}
		item := rhunext.InlineItem{Range: r, Text: t}
		item.Command = l.lensRef(s, it.Command)
		out = append(out, item)
		if len(out) == rhunext.MaxInlineItems {
			break
		}
	}
	return out
}
