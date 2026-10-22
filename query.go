package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

type documentQuery struct {
	origin     *document
	id         rhunext.Doc
	version    int64
	lspVersion int
	path, lang string
	text       []byte
	server     *server
}

func (l *lsp) captureQueryLocked(d *document) *documentQuery {
	if d == nil || l.stopping || l.docs[d.id] != d || d.text == nil || d.server == nil {
		return nil
	}
	return &documentQuery{origin: d, id: d.id, version: d.version, lspVersion: d.lspVer, path: d.path, lang: d.lang, text: d.text, server: d.server}
}
func (l *lsp) current(id rhunext.Doc, version int64) (*documentQuery, []byte, *server) {
	l.mu.Lock()
	d := l.docs[id]
	if d == nil || d.version != version || d.text == nil || d.server == nil {
		l.mu.Unlock()
		return nil, nil, nil
	}
	s := d.server
	l.mu.Unlock()
	if !ready(s) || !l.ensureSynchronized(d, s) {
		return nil, nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.docs[id] != d || d.server != s || d.version != version || d.syncedServer != s || d.syncedVersion != version {
		return nil, nil, nil
	}
	q := l.captureQueryLocked(d)
	if q == nil {
		return nil, nil, nil
	}
	return q, q.text, s
}

// latest is the document's current query, whatever its version (a completion item's resolve
// comes after the user typed on)
func (l *lsp) latest(id rhunext.Doc) (*documentQuery, []byte, *server) {
	l.mu.Lock()
	d := l.docs[id]
	if d == nil || d.text == nil || d.server == nil {
		l.mu.Unlock()
		return nil, nil, nil
	}
	s := d.server
	l.mu.Unlock()
	if !ready(s) || !l.ensureSynchronized(d, s) {
		return nil, nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.captureQueryLocked(d)
	if q == nil {
		return nil, nil, nil
	}
	return q, q.text, s
}

func (q *documentQuery) request(wait time.Duration, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return q.requestContext(ctx, method, params)
}

func (q *documentQuery) requestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := q.server
	l := s.l
	if !l.ensureSynchronized(q.origin, s) {
		return nil, errors.New("document is not synchronized")
	}
	guard := func() error {
		if !s.supports(method, selectorDocument{language: languageIDs[q.lang], scheme: "file", path: filepath.ToSlash(filepath.Clean(q.path))}) {
			return errCapabilityUnavailable
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		d := l.docs[q.id]
		if l.stopping || d != q.origin || d.server != s || d.version != q.version || d.syncedServer != s || d.syncedVersion != q.version || !s.ready.Load() {
			return errors.New("document changed before the request")
		}
		return nil
	}
	if err := guard(); err != nil {
		return nil, err
	}
	return s.callContextGuard(ctx, method, params, guard)
}
func (l *lsp) textForQuery(path string, q *documentQuery) ([]byte, int64, error) {
	if q != nil && filepath.Clean(q.path) == filepath.Clean(path) {
		return q.text, q.version, nil
	}
	return l.textOf(path)
}
func (l *lsp) hoverEvent(ev rhunext.Event) {
	id, at, ok := rhunext.ParseHover(ev)
	if !ok {
		return
	}
	l.mu.Lock()
	q := l.captureQueryLocked(l.docs[id])
	l.mu.Unlock()
	if q != nil && q.server.ready.Load() {
		go l.hover(q, at)
	}
}
func (l *lsp) hover(q *documentQuery, at rhunext.Pos) {
	res, err := q.request(15*time.Second, "textDocument/hover", map[string]any{"textDocument": map[string]any{"uri": fileURI(q.path)}, "position": lspPos(q.text, byteOffset(q.text, at))})
	if err != nil || res == nil {
		return
	}
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(res, &h) != nil {
		return
	}
	if text := hoverText(h.Contents); text != "" {
		l.ext.Hover(q.id, at, text)
	}
}
