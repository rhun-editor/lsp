package main

// Code actions of protocol 1.1 (feature code-actions): rhun's kinds become the request's only,
// its diagnostic ids (or the diagnostics in the range) the context's diagnostics, its trigger the
// trigger kind. Hierarchical kinds, isPreferred, disabled reasons and the diagnostics an action
// fixes go back as they are. An action's edit is applied by rhun; one that needs the server's
// codeAction/resolve carries resolve data, a key to the action the extension keeps, and resolving
// asks the server. A server command becomes a command reference (feature command-args) to the
// extension's runtime command exec, whose ARGS name the server and the command with its
// arguments: rhun runs it after the edit and the extension sends workspace/executeCommand, whose
// workspace/applyEdit requests become apply requests. Without command-args an action with a
// command carries resolve data and the extension runs it (after applying the edit itself) when
// it is resolved.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// a little under rhun's limits of code-actions (invoked, auto) and code-action-resolve
const (
	waitActionsInvoked = 2800 * time.Millisecond
	waitActionsAuto    = 900 * time.Millisecond
	waitActionResolve  = 4500 * time.Millisecond
	keptActionSets     = 64
)

// actionKind11 is an action's hierarchical kind: the server's, else quickfix for one that names
// diagnostics, else "action"
func actionKind11(a lspAction) string {
	switch {
	case a.Kind != "":
		return a.Kind
	case len(a.Diagnostics) > 0:
		return string(rhunext.ActionQuickFix)
	}
	return "action"
}

// keepActions remembers a list's actions that need resolving until keptActionSets lists later
func (l *lsp) keepActions(set []actionData) uint64 {
	l.actionsMu.Lock()
	defer l.actionsMu.Unlock()
	l.actionSerial++
	if l.actionSets == nil {
		l.actionSets = map[uint64][]actionData{}
	}
	if len(set) > 0 {
		l.actionSets[l.actionSerial] = set
	}
	delete(l.actionSets, l.actionSerial-keptActionSets)
	return l.actionSerial
}

func actionKey(serial uint64, i int) []byte {
	return []byte(strconv.FormatUint(serial, 10) + "." + strconv.Itoa(i))
}

func (l *lsp) keptAction(key []byte) (actionData, bool) {
	a, b, ok := strings.Cut(string(key), ".")
	serial, e1 := strconv.ParseUint(a, 10, 64)
	i, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || i < 0 {
		return actionData{}, false
	}
	l.actionsMu.Lock()
	defer l.actionsMu.Unlock()
	set := l.actionSets[serial]
	if i >= len(set) {
		return actionData{}, false
	}
	return set[i], true
}

// actionContext is the codeAction request's context
func (l *lsp) actionContext(req rhunext.CodeActionsRequest, path string, start, end lspPosition) map[string]any {
	var diags []json.RawMessage
	if l.has(rhunext.FeatureFileDiagnostics) {
		diags = l.diagnosticsByID(path, req.Diagnostics)
	} else {
		diags = l.rangeDiagnostics(path, start, end)
	}
	ctx := map[string]any{"diagnostics": diags, "triggerKind": 1}
	if req.Trigger == "auto" {
		ctx["triggerKind"] = 2
	}
	if len(req.Kinds) > 0 {
		ctx["only"] = req.Kinds
	}
	return ctx
}

// CodeActions answers code-actions (feature code-actions)
func (l *lsp) CodeActions(ext *rhunext.Ext, req rhunext.CodeActionsRequest) []rhunext.Action {
	d, text, s := l.current(req.Doc, req.Version)
	if d == nil || !ready(s) {
		return nil
	}
	start, end := lspPos(text, byteOffset(text, req.Range.Start)), lspPos(text, byteOffset(text, req.Range.End))
	wait := waitActionsInvoked
	if req.Trigger == "auto" {
		wait = waitActionsAuto
	}
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, wait)
	defer cancel()
	queries := l.commandQuerySnapshots(s, d)
	res, err := d.requestContext(ctx, "textDocument/codeAction", map[string]any{
		"textDocument": map[string]any{"uri": fileURI(d.path)},
		"range":        map[string]any{"start": start, "end": end},
		"context":      l.actionContext(req, d.path, start, end)})
	if err != nil || len(res) == 0 || string(res) == "null" {
		return nil
	}
	raws, err := boundedRawArray(res, rhunext.MaxActions)
	if err != nil {
		return nil
	}
	var pending []actionData
	var out []rhunext.Action
	serial := l.keepActions(nil)
	later := func(a actionData) []byte {
		pending = append(pending, a)
		return actionKey(serial, len(pending)-1)
	}
	resolves := canResolveAction(d)
	for _, raw := range raws {
		var a lspAction
		if json.Unmarshal(raw, &a) != nil || a.Title == "" {
			continue
		}
		act := rhunext.Action{Kind: actionKind11(a), Title: a.Title, Preferred: a.IsPreferred}
		if l.has(rhunext.FeatureFileDiagnostics) {
			act.Fixes = l.diagnosticIDs(d.path, a.Diagnostics)
		}
		if a.Disabled != nil {
			act.Disabled = a.Disabled.Reason
			if act.Disabled == "" {
				act.Disabled = "Not available"
			}
			out = append(out, act)
			continue
		}
		base := actionData{Lang: d.lang, Generation: s.generation, Doc: d.id, Version: d.version}
		edit, command := actionHasEdit(a), actionHasCommand(a)
		switch {
		case bareActionCommand(a) || !edit && command && !(resolves && hasData(a)):
			cmd, err := sourceCommand(a)
			if err != nil {
				continue
			}
			if act.Command = l.execRef(s, *cmd); act.Command != nil {
				break
			}
			base.Command = *cmd
			act.Resolve = later(base)
		case edit:
			// with workspace-edits a transaction named by the action's title
			conv, err := l.commandWorkspaceEditFull(a.Edit, s, a.Title, queries...)
			if err != nil {
				ext.Logf("action %q: %v", a.Title, err)
				continue
			}
			converted := conv.edit
			var ref *rhunext.CommandRef
			if command {
				if cmd, err := sourceCommand(a); err == nil {
					ref = l.execRef(s, *cmd)
				}
				if ref == nil {
					// the extension applies the edit and then runs the command
					action := a
					base.Prepared = &action
					base.Targets = actionTargets(converted, queries)
					act.Resolve = later(base)
					break
				}
			}
			if len(conv.ops) > 0 {
				// an edit that creates, renames or deletes files goes with the resolve answer, so
				// the servers hear of its operations only once it is chosen
				base.Edit, base.Ref, base.FileOps = &converted, ref, conv.ops
				act.Resolve = later(base)
				break
			}
			// rhun applies the edit, then runs the command's reference
			act.Edit, act.Command = &converted, ref
		case resolves && hasData(a):
			action := a
			base.Resolve = &action
			act.Resolve = later(base)
		default:
			continue
		}
		out = append(out, act)
	}
	if len(pending) > 0 {
		l.actionsMu.Lock()
		if serial+keptActionSets > l.actionSerial {
			l.actionSets[serial] = pending
		}
		l.actionsMu.Unlock()
	}
	return out
}

func hasData(a lspAction) bool { return len(a.Data) > 0 && string(a.Data) != "null" }

// ResolveCodeAction answers code-action-resolve: the server's resolved edit for rhun to apply, or,
// for an action with a server command, nothing for rhun to do while the extension runs it
func (l *lsp) ResolveCodeAction(ext *rhunext.Ext, req rhunext.CodeActionResolveRequest) (rhunext.ResolvedAction, error) {
	a, ok := l.keptAction(req.Data)
	if !ok || a.Doc != req.Doc {
		return rhunext.ResolvedAction{}, errors.New("This code action is no longer available")
	}
	if a.Resolve == nil {
		if a.Edit != nil {
			// an edit with file operations, made by rhun now
			l.expectFileOps(a.FileOps)
			return rhunext.ResolvedAction{Edit: a.Edit, Command: a.Ref}, nil
		}
		go l.runExplicitCommand(a)
		return rhunext.ResolvedAction{}, nil
	}
	d, _, s := l.current(req.Doc, req.Version)
	if d == nil || s == nil {
		return rhunext.ResolvedAction{}, errors.New("The document changed before resolving the code action")
	}
	if a.Generation != 0 && a.Generation != s.generation {
		return rhunext.ResolvedAction{}, errors.New("The language server restarted")
	}
	parent := req.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, waitActionResolve)
	defer cancel()
	queries := l.commandQuerySnapshots(s, d)
	resolved, err := resolveAction(ctx, d, *a.Resolve)
	if err != nil {
		return rhunext.ResolvedAction{}, err
	}
	if resolved.Disabled != nil {
		return rhunext.ResolvedAction{}, errors.New(resolved.Disabled.Reason)
	}
	var out rhunext.ResolvedAction
	if actionHasCommand(resolved) {
		if cmd, err := sourceCommand(resolved); err == nil {
			out.Command = l.execRef(s, *cmd)
		}
		if out.Command == nil {
			next := actionData{Lang: a.Lang, Generation: a.Generation, Prepared: &resolved, Doc: req.Doc, Version: req.Version, Queries: queries}
			go l.runExplicitCommand(next)
			return rhunext.ResolvedAction{}, nil
		}
	}
	if actionHasEdit(resolved) {
		conv, err := l.deferredWorkspaceEditFull(resolved.Edit, s, resolved.Title, queries...)
		if err != nil {
			return rhunext.ResolvedAction{}, err
		}
		// Even a foreign-file edit remains conditional on the unchanged origin.
		found := false
		for _, file := range conv.edit.Files {
			if queryMatchesTarget(d, file.Target) {
				found = true
				break
			}
		}
		if !found {
			conv.edit.Files = append(conv.edit.Files, rhunext.FileEdit{Target: rhunext.Target{Doc: d.id}, Version: d.version})
		}
		out.Edit = &conv.edit
		l.expectFileOps(conv.ops)
	}
	return out, nil
}

// ---- server commands as command references (feature command-args)

// execArgs are the ARGS of a reference to the exec command: the server (its language and
// generation, so a restarted server's command is refused) and the command with its arguments
type execArgs struct {
	Lang       string     `json:"l"`
	Generation uint64     `json:"g"`
	Command    lspCommand `json:"c"`
}

// execCommand is the runtime command that runs a server command from a code action
const execCommand = "exec"

// registerExec registers exec when rhun agreed command-args; until then (and on rhun builds
// without the feature) server commands go through resolve data
func (l *lsp) registerExec() {
	if !l.has(rhunext.FeatureCommandArgs) {
		return
	}
	if err := l.ext.Register(execCommand, "", "Run Language Server Command"); err != nil {
		l.ext.Logf("register %s: %v", execCommand, err)
		return
	}
	l.execReady.Store(true)
}

// execRef is the command reference that runs a server command of s, nil without command-args
// or when its ARGS would be over rhun's limit
func (l *lsp) execRef(s *server, cmd lspCommand) *rhunext.CommandRef {
	if !l.execReady.Load() || !l.has(rhunext.FeatureCommandArgs) {
		return nil
	}
	args, err := json.Marshal(execArgs{Lang: s.lang, Generation: s.generation, Command: cmd})
	if err != nil || len(args) > rhunext.MaxRefArgs {
		return nil
	}
	return &rhunext.CommandRef{Name: execCommand, Args: args}
}

// Invoke receives the extension's commands with how they were invoked (feature command-args):
// exec from a code action's reference runs its server command, the others are Run's
func (l *lsp) Invoke(ext *rhunext.Ext, inv rhunext.Invocation) {
	if inv.Name() != execCommand {
		l.Run(ext, inv.Command, inv.Doc)
		return
	}
	var a execArgs
	if inv.From != rhunext.FromRef || json.Unmarshal(inv.Body, &a) != nil || a.Command.Command == "" {
		go ext.Toast("Language server commands run from code actions (ctrl+.)")
		return
	}
	go func() {
		l.catchUp(inv.Doc)
		l.runExplicitCommand(actionData{Lang: a.Lang, Generation: a.Generation, Command: a.Command})
	}()
}

// catchUp brings the document and its server to the text rhun has now: rhun may run a reference's
// command before the edits its action just made reach the extension
func (l *lsp) catchUp(doc rhunext.Doc) {
	if doc == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	text, version, err := l.ext.TextContext(ctx, doc)
	if err != nil {
		return
	}
	l.mu.Lock()
	d := l.docs[doc]
	l.mu.Unlock()
	if d == nil {
		return
	}
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	l.mu.Lock()
	if l.stopping || l.docs[doc] != d {
		l.mu.Unlock()
		return
	}
	if version > d.version {
		d.text, d.version = append([]byte{}, text...), version
	}
	s := d.server
	l.mu.Unlock()
	if s != nil && s.ready.Load() {
		l.synchronizeLocked(d, s, nil)
	}
}
