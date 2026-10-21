// fakeserver: a deterministic language server for the lsp extension's tests. It speaks just
// enough LSP: initialize, didOpen/didChange (full or incremental), hover, definition, and it
// publishes one warning per line containing TODO and one error per line containing ERR, with the
// range covering the word. Hover answers "fake: <word under the pointer>". Definition points at
// line 1, character 0 of the same file. It keeps the text in step incrementally, so a wrong range
// from the extension shows up as wrong diagnostics. References are every occurrence of the word
// in the open documents and in other.go beside the file; symbols are the funcs with their
// variables as children; the code actions at an ERR are a quick fix (ERR to Err) and a command
// that asks the client to apply an edit (a comment on line 1); rename edits every occurrence of
// the word in the open documents and in other.go; formatting turns four leading spaces into a tab
// (the whole document, a range, or the line before a newline typed). Protocol 1.1: references come
// with a partial result and work-done progress when the client passes tokens, prepareRename
// answers the word's range and placeholder (null on a keyword), declaration is the word's first
// occurrence and the implementations are the func lines (as LocationLinks), the call hierarchy
// of a func is its callers and callees in the same file; ERR diagnostics carry a code, a link and
// (after the first) the first one as related information;
// linked editing ranges are the occurrences of the word under the cursor in an .html document
// when there are two of them (a tag's name and its closing tag's).
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
)

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type rng struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

var docs = map[string][]rune{} // uri -> text as runes (positions are UTF-16 units; the test texts are BMP)

func main() {
	br := bufio.NewReader(os.Stdin)
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
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &msg)
		switch msg.Method {
		case "initialize":
			reply(msg.ID, map[string]any{"capabilities": map[string]any{"textDocumentSync": 2, "hoverProvider": true, "definitionProvider": true,
				"referencesProvider": true, "documentSymbolProvider": true, "codeActionProvider": true,
				"renameProvider": map[string]any{"prepareProvider": true}, "declarationProvider": true, "implementationProvider": true,
				"documentFormattingProvider": true, "documentRangeFormattingProvider": true, "linkedEditingRangeProvider": true,
				"documentOnTypeFormattingProvider": map[string]any{"firstTriggerCharacter": "\n", "moreTriggerCharacter": []string{"}", "@"}},
				"executeCommandProvider":           map[string]any{"commands": []string{"fake.comment"}}, "callHierarchyProvider": true,
				"completionProvider": map[string]any{"triggerCharacters": []string{"."}}, "signatureHelpProvider": map[string]any{"triggerCharacters": []string{"(", ","}}}})
		case "shutdown":
			reply(msg.ID, nil)
		case "exit":
			return
		case "textDocument/didOpen":
			var p struct {
				TextDocument struct {
					URI  string `json:"uri"`
					Text string `json:"text"`
				} `json:"textDocument"`
			}
			json.Unmarshal(msg.Params, &p)
			docs[p.TextDocument.URI] = []rune(p.TextDocument.Text)
			publish(p.TextDocument.URI)
		case "textDocument/didChange":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				ContentChanges []struct {
					Range *rng   `json:"range"`
					Text  string `json:"text"`
				} `json:"contentChanges"`
			}
			json.Unmarshal(msg.Params, &p)
			text := docs[p.TextDocument.URI]
			for _, c := range p.ContentChanges {
				if c.Range == nil {
					text = []rune(c.Text)
					continue
				}
				a, b := offset(text, c.Range.Start), offset(text, c.Range.End)
				next := append([]rune{}, text[:a]...)
				next = append(next, []rune(c.Text)...)
				next = append(next, text[b:]...)
				text = next
			}
			docs[p.TextDocument.URI] = text
			publish(p.TextDocument.URI)
		case "textDocument/hover":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			w := wordAt(docs[p.TextDocument.URI], p.Position)
			if w == "" {
				reply(msg.ID, nil)
			} else {
				reply(msg.ID, map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nfake: " + w + "\n```"}})
			}
		case "textDocument/completion":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			// the word before the cursor is what the items replace
			text := docs[p.TextDocument.URI]
			off := offset(text, p.Position)
			start := off
			for start > 0 && (unicode.IsLetter(text[start-1]) || unicode.IsDigit(text[start-1]) || text[start-1] == '_') {
				start--
			}
			word := rng{Start: position{p.Position.Line, p.Position.Character - (off - start)}, End: p.Position}
			reply(msg.ID, map[string]any{"isIncomplete": false, "items": []map[string]any{
				{"label": "Sprint", "kind": 3, "sortText": "3", "additionalTextEdits": []map[string]any{
					{"range": rng{position{0, 0}, position{0, 0}}, "newText": "// imported\n"}}},
				{"label": "Println", "kind": 3, "sortText": "1", "detail": "func(a ...any) (n int, err error)",
					"documentation": map[string]any{"kind": "markdown", "value": "Println formats using the **default** formats."},
					"textEdit":      map[string]any{"range": word, "newText": "Println"}},
				{"label": "Printf", "kind": 3, "sortText": "2", "insertText": "Printf"},
				{"label": "for", "kind": 15, "sortText": "4", "insertTextFormat": 2, "insertText": "for ${1:i} := range ${2:n} {\n\t$0\n}"},
			}})
		case "textDocument/signatureHelp":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			text := docs[p.TextDocument.URI]
			off := offset(text, p.Position)
			ls := off
			for ls > 0 && text[ls-1] != '\n' {
				ls--
			}
			before := string(text[ls:off])
			if strings.Count(before, "(") <= strings.Count(before, ")") {
				reply(msg.ID, nil)
				break
			}
			reply(msg.ID, map[string]any{"activeSignature": 0, "activeParameter": strings.Count(before[strings.LastIndex(before, "("):], ","),
				"signatures": []map[string]any{{"label": "func Println(a ...any, sep string)", "documentation": "Println prints.",
					"parameters": []map[string]any{{"label": []int{13, 21}}, {"label": "sep string"}}}}})
		case "textDocument/definition":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			json.Unmarshal(msg.Params, &p)
			reply(msg.ID, []map[string]any{{"uri": p.TextDocument.URI, "range": rng{position{0, 0}, position{0, 0}}}})
		case "textDocument/references", "textDocument/rename":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
				NewName  string   `json:"newName"`
			}
			json.Unmarshal(msg.Params, &p)
			w := wordAt(docs[p.TextDocument.URI], p.Position)
			if w == "" {
				reply(msg.ID, nil)
				break
			}
			// the open documents, and other.go beside the file as it is on disk
			texts := map[string][]rune{}
			for uri, t := range docs {
				texts[uri] = t
			}
			other := p.TextDocument.URI[:strings.LastIndex(p.TextDocument.URI, "/")+1] + "other.go"
			if _, ok := texts[other]; !ok {
				if data, err := os.ReadFile(strings.TrimPrefix(other, "file://")); err == nil {
					texts[other] = []rune(string(data))
				}
			}
			uris := make([]string, 0, len(texts))
			for uri := range texts {
				uris = append(uris, uri)
			}
			sort.Strings(uris)
			var locs []map[string]any
			changes := map[string][]map[string]any{}
			for _, uri := range uris {
				for _, r := range occurrences(texts[uri], w) {
					locs = append(locs, map[string]any{"uri": uri, "range": r})
					changes[uri] = append(changes[uri], map[string]any{"range": r, "newText": p.NewName})
				}
			}
			if msg.Method == "textDocument/references" {
				var tokens struct {
					Partial json.RawMessage `json:"partialResultToken"`
					Work    json.RawMessage `json:"workDoneToken"`
				}
				json.Unmarshal(msg.Params, &tokens)
				if len(tokens.Work) > 0 {
					notify("$/progress", map[string]any{"token": tokens.Work, "value": map[string]any{"kind": "begin", "title": "Finding references", "percentage": 0}})
					notify("$/progress", map[string]any{"token": tokens.Work, "value": map[string]any{"kind": "end"}})
				}
				if len(tokens.Partial) > 0 && len(locs) > 1 {
					// the first location as a partial result, the rest in the answer
					notify("$/progress", map[string]any{"token": tokens.Partial, "value": locs[:1]})
					locs = locs[1:]
				}
				reply(msg.ID, locs)
			} else if p.NewName == "bad" {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32602, "message": "bad is not a good name"}})
			} else {
				reply(msg.ID, map[string]any{"changes": changes})
			}
		case "textDocument/prepareRename":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			text := docs[p.TextDocument.URI]
			if w := wordAt(text, p.Position); w == "" || w == "package" || w == "func" {
				reply(msg.ID, nil)
			} else {
				for _, r := range occurrences(text, w) {
					if r.Start.Line == p.Position.Line && r.Start.Character <= p.Position.Character && p.Position.Character <= r.End.Character {
						reply(msg.ID, map[string]any{"range": r, "placeholder": w})
						break
					}
				}
			}
		case "textDocument/declaration", "textDocument/implementation":
			// the declaration: the word's first occurrence; implementations: every func line
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			text := docs[p.TextDocument.URI]
			var locs []map[string]any
			if msg.Method == "textDocument/declaration" {
				if all := occurrences(text, wordAt(text, p.Position)); len(all) > 0 {
					locs = append(locs, map[string]any{"uri": p.TextDocument.URI, "range": all[0]})
				}
			} else {
				for i, line := range strings.Split(string(text), "\n") {
					if strings.HasPrefix(line, "func ") {
						locs = append(locs, map[string]any{"targetUri": p.TextDocument.URI, "targetRange": rng{position{i, 0}, position{i, len(line)}},
							"targetSelectionRange": rng{position{i, 5}, position{i, 5 + strings.IndexAny(line[5:]+"(", "( ")}}})
					}
				}
			}
			reply(msg.ID, locs)
		case "textDocument/rangeFormatting", "textDocument/onTypeFormatting":
			// four leading spaces become a tab: on the lines of the range, or on the line before the
			// cursor after a newline
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Range    rng      `json:"range"`
				Position position `json:"position"`
				Ch       string   `json:"ch"`
			}
			json.Unmarshal(msg.Params, &p)
			from, to := p.Range.Start.Line, p.Range.End.Line
			if msg.Method == "textDocument/onTypeFormatting" {
				from, to = p.Position.Line-1, p.Position.Line-1
			}
			edits := []map[string]any{}
			for i, line := range strings.Split(string(docs[p.TextDocument.URI]), "\n") {
				if i >= from && i <= to && strings.HasPrefix(line, "    ") {
					edits = append(edits, map[string]any{"range": rng{position{i, 0}, position{i, 4}}, "newText": "\t"})
				}
			}
			reply(msg.ID, edits)
		case "textDocument/prepareCallHierarchy":
			// the func whose name is under the cursor
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			w := wordAt(docs[p.TextDocument.URI], p.Position)
			if item := funcItem(p.TextDocument.URI, w); item != nil {
				reply(msg.ID, []any{item})
			} else {
				reply(msg.ID, nil)
			}
		case "callHierarchy/incomingCalls", "callHierarchy/outgoingCalls":
			// callers: the funcs whose bodies call the item, with each call's range; callees: the
			// funcs the item's body calls
			var p struct {
				Item struct {
					Name string `json:"name"`
					URI  string `json:"uri"`
				} `json:"item"`
			}
			json.Unmarshal(msg.Params, &p)
			calls := []map[string]any{}
			for _, f := range funcs(p.Item.URI) {
				if msg.Method == "callHierarchy/incomingCalls" {
					if sites := f.calls[p.Item.Name]; len(sites) > 0 {
						calls = append(calls, map[string]any{"from": funcItem(p.Item.URI, f.name), "fromRanges": sites})
					}
				} else if f.name == p.Item.Name {
					for _, callee := range funcs(p.Item.URI) {
						if sites := f.calls[callee.name]; len(sites) > 0 {
							calls = append(calls, map[string]any{"to": funcItem(p.Item.URI, callee.name), "fromRanges": sites})
						}
					}
				}
			}
			reply(msg.ID, calls)
		case "textDocument/linkedEditingRange":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Position position `json:"position"`
			}
			json.Unmarshal(msg.Params, &p)
			w := wordAt(docs[p.TextDocument.URI], p.Position)
			found := occurrences(docs[p.TextDocument.URI], w)
			if w == "" || !strings.HasSuffix(p.TextDocument.URI, ".html") || len(found) != 2 {
				reply(msg.ID, nil)
				break
			}
			reply(msg.ID, map[string]any{"ranges": found, "wordPattern": "[-_\\w.:]+"})
		case "textDocument/documentSymbol":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			json.Unmarshal(msg.Params, &p)
			reply(msg.ID, symbols(docs[p.TextDocument.URI]))
		case "textDocument/codeAction":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
				Context struct {
					Diagnostics []struct {
						Range   rng    `json:"range"`
						Message string `json:"message"`
					} `json:"diagnostics"`
				} `json:"context"`
			}
			json.Unmarshal(msg.Params, &p)
			var actions []map[string]any
			for _, d := range p.Context.Diagnostics {
				if d.Message != "ERR here" {
					continue
				}
				actions = append(actions, map[string]any{"title": "Rename ERR to Err", "kind": "quickfix", "isPreferred": true,
					"edit": map[string]any{"documentChanges": []map[string]any{{"textDocument": map[string]any{"uri": p.TextDocument.URI, "version": 1},
						"edits": []map[string]any{{"range": d.Range, "newText": "Err"}}}}}})
			}
			actions = append(actions, map[string]any{"title": "Add a comment", "kind": "source",
				"command": map[string]any{"title": "Add a comment", "command": "fake.comment", "arguments": []string{p.TextDocument.URI}}})
			actions = append(actions, map[string]any{"title": "Not now", "kind": "refactor", "disabled": map[string]any{"reason": "no"}})
			reply(msg.ID, actions)
		case "workspace/executeCommand":
			var p struct {
				Command   string   `json:"command"`
				Arguments []string `json:"arguments"`
			}
			json.Unmarshal(msg.Params, &p)
			if p.Command == "fake.comment" && len(p.Arguments) == 1 {
				send(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "workspace/applyEdit", "params": map[string]any{
					"edit": map[string]any{"changes": map[string]any{p.Arguments[0]: []map[string]any{
						{"range": rng{position{0, 0}, position{0, 0}}, "newText": "// added by a command\n"}}}}}})
			}
			reply(msg.ID, nil)
		case "textDocument/formatting":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			json.Unmarshal(msg.Params, &p)
			var edits []map[string]any
			for i, line := range strings.Split(string(docs[p.TextDocument.URI]), "\n") {
				if strings.HasPrefix(line, "    ") {
					edits = append(edits, map[string]any{"range": rng{position{i, 0}, position{i, 4}}, "newText": "\t"})
				}
			}
			if edits == nil {
				edits = []map[string]any{}
			}
			reply(msg.ID, edits)
		default:
			if len(msg.ID) > 0 && msg.Method != "" {
				reply(msg.ID, nil)
			}
		}
	}
}

// occurrences are the ranges of word as a whole word in text
func occurrences(text []rune, word string) []rng {
	var out []rng
	lines := strings.Split(string(text), "\n")
	isWord := func(r byte) bool {
		return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	}
	for i, line := range lines {
		for from := 0; ; {
			j := strings.Index(line[from:], word)
			if j < 0 {
				break
			}
			a, b := from+j, from+j+len(word)
			if (a == 0 || !isWord(line[a-1])) && (b == len(line) || !isWord(line[b])) {
				out = append(out, rng{position{i, utf16units(line[:a])}, position{i, utf16units(line[:b])}})
			}
			from = b
		}
	}
	return out
}

// symbols: each "func NAME" line is a function whose children are the "NAME :=" lines up to the
// next func
func symbols(text []rune) []map[string]any {
	lines := strings.Split(string(text), "\n")
	out := []map[string]any{}
	var cur map[string]any
	for i, line := range lines {
		if rest, ok := strings.CutPrefix(line, "func "); ok {
			name := rest[:strings.IndexAny(rest+"(", "( ")]
			r := rng{position{i, 0}, position{i, len(line)}}
			cur = map[string]any{"name": name, "kind": 12, "detail": "func()", "range": r,
				"selectionRange": rng{position{i, 5}, position{i, 5 + len(name)}}, "children": []map[string]any{}}
			out = append(out, cur)
			continue
		}
		if j := strings.Index(line, " := "); j > 0 && cur != nil {
			k := j
			for k > 0 && line[k-1] != ' ' && line[k-1] != '\t' {
				k--
			}
			name := line[k:j]
			r := rng{position{i, k}, position{i, j}}
			cur["children"] = append(cur["children"].([]map[string]any), map[string]any{"name": name, "kind": 13, "range": r, "selectionRange": r})
		}
	}
	return out
}

// fakeFunc is a "func NAME" of a document: its line and the calls "OTHER(" in its body
type fakeFunc struct {
	name  string
	line  int
	calls map[string][]rng
}

func funcs(uri string) []fakeFunc {
	var out []fakeFunc
	for i, line := range strings.Split(string(docs[uri]), "\n") {
		if rest, ok := strings.CutPrefix(line, "func "); ok {
			out = append(out, fakeFunc{name: rest[:strings.IndexAny(rest+"(", "( ")], line: i, calls: map[string][]rng{}})
			continue
		}
		if len(out) == 0 {
			continue
		}
		f := out[len(out)-1]
		for from := 0; ; {
			j := strings.Index(line[from:], "(")
			if j < 0 {
				break
			}
			end := from + j
			start := end
			for start > 0 && (unicode.IsLetter(rune(line[start-1])) || unicode.IsDigit(rune(line[start-1])) || line[start-1] == '_') {
				start--
			}
			if start < end {
				f.calls[line[start:end]] = append(f.calls[line[start:end]], rng{position{i, utf16units(line[:start])}, position{i, utf16units(line[:end])}})
			}
			from = end + 1
		}
	}
	return out
}

// funcItem is the CallHierarchyItem of a func of the document, nil when there is none
func funcItem(uri, name string) map[string]any {
	lines := strings.Split(string(docs[uri]), "\n")
	for _, f := range funcs(uri) {
		if f.name == name {
			return map[string]any{"name": name, "kind": 12, "detail": "func()", "uri": uri,
				"range":          rng{position{f.line, 0}, position{f.line, utf16units(lines[f.line])}},
				"selectionRange": rng{position{f.line, 5}, position{f.line, 5 + len(name)}}, "data": map[string]any{"fake": name}}
		}
	}
	return nil
}

func offset(text []rune, p position) int {
	off, line := 0, 0
	for off < len(text) && line < p.Line {
		if text[off] == '\n' {
			line++
		}
		off++
	}
	units := 0
	for off < len(text) && text[off] != '\n' && units < p.Character {
		units += utf16.RuneLen(text[off])
		off++
	}
	return off
}

func wordAt(text []rune, p position) string {
	off := offset(text, p)
	if off >= len(text) {
		return ""
	}
	isWord := func(r rune) bool {
		return r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r > 127
	}
	if !isWord(text[off]) {
		// a position just after a word is in it, as for real servers
		if off == 0 || !isWord(text[off-1]) {
			return ""
		}
		off--
	}
	a, b := off, off
	for a > 0 && isWord(text[a-1]) {
		a--
	}
	for b < len(text) && isWord(text[b]) {
		b++
	}
	return string(text[a:b])
}

func publish(uri string) {
	text := docs[uri]
	var diags []map[string]any
	var first any
	line, col := 0, 0
	lines := strings.Split(string(text), "\n")
	for line = range lines {
		for _, word := range []struct {
			w   string
			sev int
		}{{"TODO", 2}, {"ERR", 1}} {
			col = strings.Index(lines[line], word.w)
			if col < 0 {
				continue
			}
			start := utf16units(lines[line][:col])
			d := map[string]any{
				"range":    rng{position{line, start}, position{line, start + len(word.w)}},
				"severity": word.sev, "source": "fake", "message": word.w + " here",
			}
			if word.w == "ERR" {
				d["code"] = "E1"
				d["codeDescription"] = map[string]any{"href": "https://example.org/E1"}
				if first != nil {
					d["relatedInformation"] = []any{map[string]any{"location": map[string]any{"uri": uri, "range": first}, "message": "first ERR"}}
				} else {
					first = d["range"]
				}
			}
			diags = append(diags, d)
		}
	}
	if diags == nil {
		diags = []map[string]any{}
	}
	notify("textDocument/publishDiagnostics", map[string]any{"uri": uri, "diagnostics": diags})
}

func utf16units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

func reply(id json.RawMessage, result any) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func notify(method string, params any) {
	send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func send(msg any) {
	data, _ := json.Marshal(msg)
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data)
}
