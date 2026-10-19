package main

// Linked editing (protocol 1.1, feature linked-editing): rhun asks for the ranges that change
// together with the word at the cursor (an HTML tag's name and its closing tag's) and links them
// itself while the user types; the server's textDocument/linkedEditingRange answers. Its
// wordPattern becomes the word record when it is a plain character class, else rhun's default.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// a little under the second rhun waits
const waitLinked = 900 * time.Millisecond

func (l *lsp) Linked(ext *rhunext.Ext, req rhunext.LinkedRequest) rhunext.Linked {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return rhunext.Linked{}
	}
	res, err := askFor(s, d, waitLinked, "textDocument/linkedEditingRange", docParams(d, text, req.At))
	if err != nil || len(res) == 0 || string(res) == "null" {
		return rhunext.Linked{}
	}
	var r struct {
		Ranges      []lspRange `json:"ranges"`
		WordPattern *string    `json:"wordPattern"`
	}
	if json.Unmarshal(res, &r) != nil || len(r.Ranges) < 2 || len(r.Ranges) > rhunext.MaxLinked {
		return rhunext.Linked{}
	}
	out := rhunext.Linked{}
	for _, rg := range r.Ranges {
		out.Ranges = append(out.Ranges, rhunRange(text, rg))
	}
	if r.WordPattern != nil {
		if chars, ok := wordChars(*r.WordPattern); ok {
			out.Word = &chars
		}
	}
	return out
}

// wordChars is the ASCII characters besides letters and digits a word pattern that is a plain
// character class allows ("[-_\\w.:]+" is "-_.:"); false for another pattern (a negated class,
// alternatives, anything but one class and a repetition)
func wordChars(pattern string) (string, bool) {
	p := strings.TrimPrefix(pattern, "^")
	p = strings.TrimSuffix(p, "$")
	if strings.HasSuffix(p, "+") || strings.HasSuffix(p, "*") {
		p = p[:len(p)-1]
	}
	if len(p) < 2 || p[0] != '[' || p[len(p)-1] != ']' || p[1] == '^' {
		return "", false
	}
	body := p[1 : len(p)-1]
	var chars []byte
	add := func(c byte) {
		isAlnum := c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z'
		if c >= ' ' && c <= '~' && !isAlnum && strings.IndexByte(string(chars), c) < 0 {
			chars = append(chars, c)
		}
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '\\':
			if i+1 >= len(body) {
				return "", false
			}
			i++
			switch e := body[i]; e {
			case 'w':
				add('_')
			case 'd':
			case 's':
				add(' ')
			case 'W', 'D', 'S', 'b', 'B', 'p', 'P', 'u', 'x':
				return "", false
			default:
				add(e)
			}
		case c == '[' || c == ']':
			return "", false
		case c >= 0x80:
			// non-ASCII is allowed anyway
		case i+2 < len(body) && body[i+1] == '-' && body[i+2] != '\\':
			for x := int(c); x <= int(body[i+2]); x++ {
				add(byte(x))
			}
			i += 2
		default:
			add(c)
		}
	}
	return string(chars), true
}
