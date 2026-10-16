// fakeserver: a deterministic language server for the lsp extension's tests. It speaks just
// enough LSP: initialize, didOpen/didChange (full or incremental), hover, definition, and it
// publishes one warning per line containing TODO and one error per line containing ERR, with the
// range covering the word. Hover answers "fake: <word under the pointer>". Definition points at
// line 1, character 0 of the same file. It keeps the text in step incrementally, so a wrong range
// from the extension shows up as wrong diagnostics.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
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
			reply(msg.ID, map[string]any{"capabilities": map[string]any{"textDocumentSync": 2, "hoverProvider": true, "definitionProvider": true}})
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
		case "textDocument/definition":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			json.Unmarshal(msg.Params, &p)
			reply(msg.ID, []map[string]any{{"uri": p.TextDocument.URI, "range": rng{position{0, 0}, position{0, 0}}}})
		default:
			if len(msg.ID) > 0 {
				reply(msg.ID, nil)
			}
		}
	}
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
		return ""
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
			diags = append(diags, map[string]any{
				"range":    rng{position{line, start}, position{line, start + len(word.w)}},
				"severity": word.sev, "source": "fake", "message": word.w + " here",
			})
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
