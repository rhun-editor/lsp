package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

const maxSaveActions = 64

type saveBatch struct {
	l     *lsp
	ext   *rhunext.Ext
	req   rhunext.SaveActionsRequest
	query *documentQuery
}

func (l *lsp) refreshSaveQuery(ctx context.Context, id rhunext.Doc, expected int64) (*documentQuery, error) {
	text, version, err := l.ext.TextContext(ctx, id)
	if err != nil {
		return nil, err
	}
	if expected >= 0 && version != expected {
		return nil, errors.New("document changed during source actions")
	}
	l.mu.Lock()
	d := l.docs[id]
	l.mu.Unlock()
	if d == nil {
		return nil, errors.New("document closed")
	}
	d.syncMu.Lock()
	l.mu.Lock()
	if l.stopping || l.docs[id] != d || version < d.version {
		l.mu.Unlock()
		d.syncMu.Unlock()
		return nil, errors.New("document changed during source actions")
	}
	d.text = append([]byte{}, text...)
	d.version = version
	if d.server == nil {
		d.server = l.serverFor(d.lang)
	}
	s := d.server
	l.mu.Unlock()
	d.syncMu.Unlock()
	if s == nil {
		return nil, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.readyCh:
	}
	if !s.ready.Load() || !l.ensureSynchronized(d, s) {
		return nil, errors.New("language server unavailable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.captureQueryLocked(d)
	if q == nil || (expected >= 0 && q.version != expected) || q.server != s || d.syncedServer != s || d.syncedVersion != q.version {
		return nil, errors.New("document changed before source actions")
	}
	return q, nil
}
func (b *saveBatch) refresh(version int64) error {
	q, err := b.l.refreshSaveQuery(b.req.Context, b.req.Doc, version)
	if err != nil {
		return err
	}
	if q == nil {
		return errors.New("language server unavailable")
	}
	b.query = q
	return nil
}
func (b *saveBatch) apply(edit rhunext.WorkspaceEdit) error {
	version, err := b.ext.SaveApplyContext(b.req.Context, b.req.ID, b.req.Doc, b.query.version, edit)
	if err != nil {
		return err
	}
	return b.refresh(version)
}
func sourceKindMatches(requested, kind string) bool {
	return kind == requested || strings.HasPrefix(kind, requested+".")
}
func sourceIdentity(a lspAction) string { return a.Kind + "\x00" + a.Title }

func sourceCommand(a lspAction) (*lspCommand, error) {
	raw := a.Command
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var cmd lspCommand
	if raw[0] == '"' {
		if json.Unmarshal(raw, &cmd.Command) != nil {
			return nil, errors.New("invalid action command")
		}
		cmd.Title = a.Title
		if len(a.Args) > 0 && json.Unmarshal(a.Args, &cmd.Arguments) != nil {
			return nil, errors.New("invalid action command arguments")
		}
	} else if json.Unmarshal(raw, &cmd) != nil {
		return nil, errors.New("invalid action command")
	}
	if cmd.Command == "" {
		return nil, errors.New("empty action command")
	}
	if cmd.Title == "" {
		cmd.Title = a.Title
	}
	return &cmd, nil
}
func actionHasEdit(a lspAction) bool     { return len(a.Edit) > 0 && string(a.Edit) != "null" }
func actionHasCommand(a lspAction) bool  { return len(a.Command) > 0 && string(a.Command) != "null" }
func bareActionCommand(a lspAction) bool { return len(a.Command) > 0 && a.Command[0] == '"' }
func canResolveAction(q *documentQuery) bool {
	return q.server.supports("codeAction/resolve", selectorDocument{language: languageIDs[q.lang], scheme: "file", path: filepath.ToSlash(filepath.Clean(q.path))})
}
func resolveAction(ctx context.Context, q *documentQuery, a lspAction) (lspAction, error) {
	raw, err := q.requestContext(ctx, "codeAction/resolve", a)
	if err != nil {
		return a, err
	}
	var resolved lspAction
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &resolved) != nil {
		return a, errors.New("invalid resolved code action")
	}
	if len(resolved.Title) > 1000 || len(resolved.Kind) > 128 {
		return a, errors.New("resolved code action fields exceed limits")
	}
	if resolved.Kind == "" {
		resolved.Kind = a.Kind
	}
	if resolved.Title == "" {
		resolved.Title = a.Title
	}
	if !actionHasEdit(resolved) {
		resolved.Edit = a.Edit
	}
	if !actionHasCommand(resolved) {
		resolved.Command = a.Command
		resolved.Args = a.Args
	}
	return resolved, nil
}
func (b *saveBatch) resolve(a lspAction) (lspAction, error) {
	if !canResolveAction(b.query) || bareActionCommand(a) {
		if !actionHasEdit(a) && !actionHasCommand(a) {
			return a, errors.New("source action requires unsupported resolution")
		}
		return a, nil
	}
	return resolveAction(b.req.Context, b.query, a)
}

func (l *lsp) SaveActions(ext *rhunext.Ext, req rhunext.SaveActionsRequest) (rhunext.WorkspaceEdit, error) {
	var empty rhunext.WorkspaceEdit
	if req.Context == nil {
		req.Context = context.Background()
	}
	ctx, cancel := context.WithTimeout(req.Context, 30*time.Second)
	defer cancel()
	req.Context = ctx
	l.mu.Lock()
	d := l.docs[req.Doc]
	lang := ""
	if d != nil {
		lang = d.lang
	}
	l.mu.Unlock()
	if lang == "" {
		return empty, nil
	}
	gate := l.commandGate(lang)
	if err := acquireCommand(ctx, gate); err != nil {
		return empty, err
	}
	defer func() { <-gate }()
	l.mu.Lock()
	old := l.servers[lang]
	l.mu.Unlock()
	if old != nil && old.commandUsed.Load() {
		l.retireCommandServer(old)
	}
	q, err := l.refreshSaveQuery(ctx, req.Doc, req.Version)
	if err != nil || q == nil {
		return empty, err
	}
	b := &saveBatch{l: l, ext: ext, req: req, query: q}
	seen := map[string]bool{}
	for attempt := 0; attempt < maxSaveActions; attempt++ {
		q = b.query
		s := q.server
		res, err := q.requestContext(ctx, "textDocument/codeAction", map[string]any{
			"textDocument": map[string]any{"uri": fileURI(q.path)},
			"range":        lspRange{Start: lspPosition{}, End: lspPos(q.text, len(q.text))},
			"context":      map[string]any{"diagnostics": []any{}, "only": []string{req.Kind}, "triggerKind": 2},
		})
		if errors.Is(err, errCapabilityUnavailable) {
			return empty, nil
		}
		if err != nil {
			return empty, err
		}
		actions, err := parseSourceActions(res)
		if err != nil {
			return empty, err
		}
		identity := ""
		type candidate struct {
			action   lspAction
			resolved bool
		}
		var selected []candidate
		for _, a := range actions {
			if a.Title == "" || a.Disabled != nil || (a.Kind != "" && !sourceKindMatches(req.Kind, a.Kind)) || bareActionCommand(a) {
				continue
			}
			resolved := false
			if a.Kind == "" {
				if !canResolveAction(q) {
					continue
				}
				a, err = b.resolve(a)
				if err != nil {
					return empty, err
				}
				resolved = true
				if a.Disabled != nil || !sourceKindMatches(req.Kind, a.Kind) {
					continue
				}
			}
			key := sourceIdentity(a)
			if seen[key] {
				continue
			}
			if identity == "" {
				identity = key
			}
			if key == identity {
				selected = append(selected, candidate{action: a, resolved: resolved})
			}
		}
		if identity == "" {
			return empty, nil
		}
		seen[identity] = true
		var combined rhunext.WorkspaceEdit
		var command *lspCommand
		for _, candidate := range selected {
			a := candidate.action
			if !candidate.resolved {
				a, err = b.resolve(a)
				if err != nil {
					return empty, err
				}
			}
			if !sourceKindMatches(req.Kind, a.Kind) {
				return empty, errors.New("resolved action changed its source kind")
			}
			if a.Disabled != nil {
				continue
			}
			cmd, err := sourceCommand(a)
			if err != nil {
				return empty, err
			}
			if cmd != nil {
				if len(selected) > 1 {
					return empty, errors.New("ambiguous duplicate source command")
				}
				command = cmd
			}
			if len(a.Edit) > 0 && string(a.Edit) != "null" {
				edit, err := l.saveWorkspaceEdit(a.Edit, q)
				if err != nil {
					return empty, err
				}
				for _, file := range edit.Files {
					if len(combined.Files) == 0 {
						combined.Files = append(combined.Files, file)
					} else {
						combined.Files[0].Edits = append(combined.Files[0].Edits, file.Edits...)
					}
				}
			}
		}
		if len(combined.Files) > 0 {
			if err := b.apply(combined); err != nil {
				return empty, err
			}
		}
		if command != nil {
			s = b.query.server
			err = s.executeCaptured(ctx, *command, b)
			l.retireCommandServer(s)
			if err != nil {
				return empty, err
			}
			if err = b.refresh(b.query.version); err != nil {
				return empty, err
			}
		}
	}
	return empty, fmt.Errorf("source action limit (%d) reached", maxSaveActions)
}

func parseSourceActions(raw json.RawMessage) ([]lspAction, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	entries, err := boundedRawArray(raw, maxSaveActions)
	if err != nil {
		return nil, err
	}
	actions := make([]lspAction, 0, len(entries))
	for _, entry := range entries {
		var action lspAction
		if len(entry) == 0 || entry[0] != '{' || json.Unmarshal(entry, &action) != nil || len(action.Title) > 1000 || len(action.Kind) > 128 {
			return nil, errors.New("invalid or oversized source action")
		}
		actions = append(actions, action)
	}
	return actions, nil
}
