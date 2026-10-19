package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// The gate belongs to the language rather than a server generation, so a retired server
// cannot let a waiting explicit command overtake a source-action batch.
func (l *lsp) commandGate(lang string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.commandGates == nil {
		l.commandGates = map[string]chan struct{}{}
	}
	gate := l.commandGates[lang]
	if gate == nil {
		gate = make(chan struct{}, 1)
		l.commandGates[lang] = gate
	}
	return gate
}

func acquireCommand(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case gate <- struct{}{}:
		return nil
	}
}

type commandCapture struct {
	ctx     context.Context
	cancel  context.CancelFunc
	save    *saveBatch
	queries []*documentQuery
	active  bool
	rpc     int
	pending int
	work    sync.WaitGroup
	tail    chan struct{}
	failure error
}

type commandEdit struct {
	capture      *commandCapture
	before, done chan struct{}
}

func (s *server) admitCommandEdit() *commandEdit {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	c := s.commandCapture
	if c == nil || !c.active || c.ctx.Err() != nil || c.pending >= 128 || s.ended.Load() {
		return nil
	}
	if c.tail == nil {
		c.tail = make(chan struct{})
		close(c.tail)
	}
	work := &commandEdit{capture: c, before: c.tail, done: make(chan struct{})}
	c.tail = work.done
	c.pending++
	c.work.Add(1)
	return work
}
func (s *server) closeCommandReply(id int) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if c := s.commandCapture; c != nil && c.rpc == id {
		c.active = false
	}
}
func (s *server) cancelCommandCapture() {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if c := s.commandCapture; c != nil {
		c.active = false
		c.cancel()
	}
}
func (s *server) rejectCommandEdit(id json.RawMessage, why string) {
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"applied": false, "failureReason": why}})
}

// Caller owns the per-language command gate. Admission closes in the reader before the
// executeCommand reply wakes this goroutine. Already admitted handlers drain before completion.
func (s *server) executeCaptured(parent context.Context, command lspCommand, batch *saveBatch, queries ...*documentQuery) error {
	ctx, cancel := context.WithCancel(parent)
	if batch == nil && len(queries) == 0 {
		queries = s.l.commandQuerySnapshots(s, nil)
	}
	c := &commandCapture{ctx: ctx, cancel: cancel, save: batch, active: true, queries: queries}
	s.commandMu.Lock()
	if s.commandCapture != nil || s.ended.Load() {
		s.commandMu.Unlock()
		cancel()
		return errors.New("server command unavailable")
	}
	s.commandCapture = c
	s.commandUsed.Store(true)
	s.commandMu.Unlock()
	args := command.Arguments
	if args == nil {
		args = []json.RawMessage{}
	}
	_, err := s.callContextRegistered(ctx, "workspace/executeCommand", map[string]any{"command": command.Command, "arguments": args}, nil, func(id int) {
		s.commandMu.Lock()
		c.rpc = id
		s.commandMu.Unlock()
	})
	s.commandMu.Lock()
	c.active = false
	s.commandMu.Unlock()
	if err != nil {
		cancel()
	}
	c.work.Wait()
	s.commandMu.Lock()
	if s.commandCapture == c {
		s.commandCapture = nil
	}
	s.commandMu.Unlock()
	cancel()
	if err == nil {
		err = c.failure
	}
	return err
}
func (s *server) applyCommandEdit(work *commandEdit, id json.RawMessage, params json.RawMessage) {
	c := work.capture
	defer func() { close(work.done); s.commandMu.Lock(); c.pending--; s.commandMu.Unlock(); c.work.Done() }()
	<-work.before
	var p struct {
		Edit json.RawMessage `json:"edit"`
	}
	err := json.Unmarshal(params, &p)
	if err == nil {
		err = c.ctx.Err()
	}
	if err == nil {
		err = c.failure
	}
	if err == nil {
		if c.save != nil {
			var edit rhunext.WorkspaceEdit
			edit, err = s.l.saveWorkspaceEdit(p.Edit, c.save.query)
			if err == nil {
				err = c.save.apply(edit)
			}
		} else {
			var edit rhunext.WorkspaceEdit
			edit, err = s.l.commandWorkspaceEdit(p.Edit, s, c.queries...)
			if err == nil {
				c.queries, err = s.l.applyExplicitEdit(c.ctx, edit, c.queries, s)
			}
		}
	}
	result := map[string]any{"applied": err == nil}
	if err != nil {
		result["failureReason"] = err.Error()
		if c.failure == nil {
			c.failure = err
		}
	}
	if writeErr := s.writeContext(c.ctx, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); writeErr != nil && c.failure == nil {
		c.failure = writeErr
	}
}

// A command may send delayed edits without a causal token. Source batches retire any generation
// that has executed commands, and retire each source command's generation after its handlers drain.
func (l *lsp) retireCommandServer(s *server) {
	l.mu.Lock()
	var docs []*document
	if l.servers[s.lang] == s {
		delete(l.servers, s.lang)
		l.retireStatus(s)
		for _, d := range l.docs {
			if d.server == s {
				// Publish a replacement before replying to the source request so the
				// next formatting request can wait for it rather than see no server.
				d.server = l.serverFor(d.lang)
				d.syncedServer = nil
				d.opened = false
				docs = append(docs, d)
			}
		}
	}
	l.mu.Unlock()
	s.stop()
	for _, d := range docs {
		go l.open(d)
	}
}

func (l *lsp) runExplicitCommand(a actionData) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	gate := l.commandGate(a.Lang)
	if err := acquireCommand(ctx, gate); err != nil {
		return
	}
	defer func() { <-gate }()
	l.mu.Lock()
	s := l.servers[a.Lang]
	l.mu.Unlock()
	if s == nil || !s.ready.Load() || (a.Generation != 0 && a.Generation != s.generation) {
		l.ext.Toast("The language server is not running")
		return
	}
	queries := l.commandQuerySnapshots(s, nil)
	command := a.Command
	selected := a.Resolve
	if selected == nil {
		selected = a.Prepared
	}
	if selected != nil {
		q, _, current := l.current(a.Doc, a.Version)
		if q == nil || current != s {
			l.ext.Toast("The document changed before resolving the code action")
			return
		}
		queries = l.commandQuerySnapshots(s, q)
		if err := retainActionTargetVersions(queries, a.Targets); err != nil {
			l.ext.Toast(selected.Title + ": " + err.Error())
			return
		}
		resolved := *selected
		if a.Resolve != nil {
			var err error
			resolved, err = resolveAction(ctx, q, *selected)
			if err != nil {
				l.ext.Toast(selected.Title + ": " + err.Error())
				return
			}
		}
		if resolved.Disabled != nil {
			return
		}
		if actionHasEdit(resolved) {
			edit, err := l.commandWorkspaceEdit(resolved.Edit, s, queries...)
			if err == nil {
				// Even an action editing only another file remains conditional on its origin.
				found := false
				for _, file := range edit.Files {
					if queryMatchesTarget(q, file.Target) {
						found = true
						break
					}
				}
				if !found {
					edit.Files = append(edit.Files, rhunext.FileEdit{Target: rhunext.Target{Doc: q.id}, Version: q.version})
				}
				queries, err = l.applyExplicitEdit(ctx, edit, queries, s)
			}
			if err != nil {
				l.ext.Toast(resolved.Title + ": " + err.Error())
				return
			}
		}
		cmd, err := sourceCommand(resolved)
		if err != nil {
			l.ext.Toast(resolved.Title + ": " + err.Error())
			return
		}
		if cmd == nil {
			return
		}
		if !actionHasEdit(resolved) {
			_, version, err := l.ext.TextContext(ctx, q.id)
			if err != nil || version != q.version {
				l.ext.Toast("The document changed before running the code action")
				return
			}
		}
		command = *cmd
	}
	if err := s.executeCaptured(ctx, command, nil, queries...); err != nil {
		l.ext.Toast(command.Title + ": " + err.Error())
	}
}

// Predict only the changes accepted by the host. Each nonempty deletion and insertion
// increments rhun's native document version once (doc.s raw_delete/raw_insert). Requiring
// both this version and the resulting text prevents a refresh from crediting intervening input.
func commandEditSnapshots(edit rhunext.WorkspaceEdit, queries []*documentQuery) ([]*documentQuery, error) {
	next := append([]*documentQuery(nil), queries...)
	for _, file := range edit.Files {
		index := -1
		for i, q := range queries {
			if queryMatchesTarget(q, file.Target) {
				index = i
				break
			}
		}
		if index < 0 || queries[index].text == nil || queries[index].version != file.Version {
			return nil, errors.New("command target snapshot unavailable")
		}
		q := *queries[index]
		type change struct {
			start, end int
			text       string
		}
		changes := make([]change, 0, len(file.Edits))
		for _, e := range file.Edits {
			start, end := byteOffset(q.text, e.Range.Start), byteOffset(q.text, e.Range.End)
			if start < 0 || start > end || end > len(q.text) {
				return nil, errors.New("invalid command range")
			}
			changes = append(changes, change{start, end, e.Text})
		}
		sort.SliceStable(changes, func(i, j int) bool {
			if changes[i].start != changes[j].start {
				return changes[i].start < changes[j].start
			}
			return changes[i].end < changes[j].end
		})
		var text bytes.Buffer
		at := 0
		for _, e := range changes {
			if e.start < at {
				return nil, errors.New("overlapping command edits")
			}
			text.Write(q.text[at:e.start])
			text.WriteString(e.text)
			at = e.end
			if e.end > e.start {
				q.version++
			}
			if len(e.text) > 0 {
				q.version++
			}
		}
		text.Write(q.text[at:])
		q.text = text.Bytes()
		next[index] = &q
	}
	return next, nil
}

func (l *lsp) applyExplicitEdit(ctx context.Context, edit rhunext.WorkspaceEdit, queries []*documentQuery, commandServer *server) ([]*documentQuery, error) {
	expected, err := commandEditSnapshots(edit, queries)
	if err != nil {
		return nil, err
	}
	body, err := edit.Encode()
	if err != nil {
		return nil, err
	}
	if _, err = l.ext.RequestContext(ctx, "apply", body, strconv.Itoa(len(body))); err != nil {
		return nil, err
	}
	next := append([]*documentQuery(nil), queries...)
	for i, q := range expected {
		if q == queries[i] {
			continue
		}
		current, err := l.refreshCommandSnapshot(ctx, q, commandServer)
		if err != nil {
			return nil, err
		}
		if current == nil || !bytes.Equal(current.text, q.text) {
			return nil, errors.New("document changed during command edits")
		}
		next[i] = current
	}
	return next, nil
}

// Command targets can be plain text or belong to another language. Their native edit credit
// does not require a server, and numeric LSP versions belong only to the command's own session.
func (l *lsp) refreshCommandSnapshot(ctx context.Context, expected *documentQuery, commandServer *server) (*documentQuery, error) {
	text, version, err := l.ext.TextContext(ctx, expected.id)
	if err != nil {
		return nil, err
	}
	if version != expected.version || !bytes.Equal(text, expected.text) {
		return nil, errors.New("document changed during command edits")
	}
	l.mu.Lock()
	d := l.docs[expected.id]
	l.mu.Unlock()
	if d == nil {
		return nil, errors.New("command target closed")
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	l.mu.Lock()
	if l.stopping || l.docs[d.id] != d || d != expected.origin || filepath.Clean(d.path) != filepath.Clean(expected.path) || d.version > version {
		l.mu.Unlock()
		return nil, errors.New("command target changed")
	}
	d.text = append([]byte{}, text...)
	d.version = version
	targetServer := d.server
	l.mu.Unlock()
	if targetServer != nil && targetServer.ready.Load() && !l.synchronizeLocked(d, targetServer, nil) {
		return nil, errors.New("command target synchronization failed")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopping || l.docs[d.id] != d || d.version != version {
		return nil, errors.New("command target changed")
	}
	q := &documentQuery{id: d.id, origin: d, path: d.path, lang: d.lang, text: d.text, version: d.version, lspVersion: -1, server: d.server}
	if d.syncedServer == commandServer && d.syncedVersion == d.version {
		q.lspVersion = d.lspVer
	}
	return q, nil
}

// Existing edit ranges were calculated during the original codeAction request, before the
// picker opened. Retain those versions across selection as well as the resolve round trip.
func retainActionTargetVersions(queries []*documentQuery, targets []actionTarget) error {
	for _, target := range targets {
		found := false
		for _, q := range queries {
			if q != nil && q.id == target.Doc && filepath.Clean(q.path) == filepath.Clean(target.Path) {
				if q.version != target.Version {
					return errors.New("code action target changed before selection")
				}
				q.lspVersion = target.LSPVersion
				found = true
				break
			}
		}
		if !found {
			return errors.New("code action target closed before selection")
		}
	}
	return nil
}
