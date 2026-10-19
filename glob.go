package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Translate without expanding brace alternatives. RE2 keeps matching bounded even
// for patterns with many alternatives or stars.
func compileGlob(pattern string, insensitive bool) (*regexp.Regexp, error) {
	if len(pattern) > 4096 {
		return nil, fmt.Errorf("glob exceeds 4096 bytes")
	}
	p := globParser{text: []rune(pattern)}
	body, err := p.part(0)
	if err != nil {
		return nil, err
	}
	if p.at != len(p.text) {
		return nil, fmt.Errorf("unexpected glob delimiter")
	}
	if len(body) > 64<<10 {
		return nil, fmt.Errorf("compiled glob exceeds 64 KiB")
	}
	prefix := "^"
	if insensitive {
		prefix = "(?i)^"
	}
	return regexp.Compile(prefix + body + "$")
}

type globParser struct {
	text []rune
	at   int
}

func (p *globParser) part(depth int) (string, error) {
	if depth > 16 {
		return "", fmt.Errorf("glob braces exceed depth 16")
	}
	var b strings.Builder
	for p.at < len(p.text) {
		c := p.text[p.at]
		if c == ',' || c == '}' {
			if depth > 0 {
				break
			}
			if c == '}' {
				return "", fmt.Errorf("unmatched glob brace")
			}
		}
		p.at++
		switch c {
		case '*':
			if p.at < len(p.text) && p.text[p.at] == '*' {
				p.at++
				if p.at < len(p.text) && p.text[p.at] == '/' {
					p.at++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '\\':
			if p.at == len(p.text) {
				return "", fmt.Errorf("unfinished glob escape")
			}
			b.WriteString(regexp.QuoteMeta(string(p.text[p.at])))
			p.at++
		case '[':
			start := p.at
			for p.at < len(p.text) && p.text[p.at] != ']' {
				p.at++
			}
			if p.at == len(p.text) || p.at == start {
				return "", fmt.Errorf("unfinished or empty glob class")
			}
			class := string(p.text[start:p.at])
			p.at++
			if class == "!" || strings.ContainsAny(class, "/\\[") {
				return "", fmt.Errorf("invalid glob class")
			}
			if strings.HasPrefix(class, "!") {
				b.WriteString("[^/" + strings.TrimPrefix(class, "!") + "]")
			} else {
				// A caret is a literal unless the class uses glob's ! negation.
				if strings.HasPrefix(class, "^") {
					class = "\\" + class
				}
				b.WriteString("[" + class + "]")
			}
		case '{':
			b.WriteString("(?:")
			for {
				part, err := p.part(depth + 1)
				if err != nil {
					return "", err
				}
				b.WriteString(part)
				if p.at == len(p.text) {
					return "", fmt.Errorf("unfinished glob brace")
				}
				delim := p.text[p.at]
				p.at++
				if delim == '}' {
					break
				}
				b.WriteByte('|')
			}
			b.WriteByte(')')
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String(), nil
}
