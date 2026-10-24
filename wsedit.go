package main

// Workspace edits as transactions (protocol 1.1, features workspace-edits and file-operations).
// LSP's WorkspaceEdit (changes, or documentChanges: text edits and create, rename and delete
// operations in order) is turned into rhun's through the SDK's NormalizeOrdered: each text edit
// converted from UTF-16 against the text of its file at that point of the sequence (an open
// document's snapshot at its version; a file that is not open read from disk, the bytes it was
// converted against named by their fnv1a64 hash), annotations that need confirmation asking rhun
// to confirm, and the LSP change behind each entry kept for workspace/applyEdit's failedChange.
// Once an edit with file operations is made, the servers that asked for them get
// workspace/didCreateFiles, didRenameFiles and didDeleteFiles.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// editCaps is what rhun agreed of protocol 1.1's workspace edits
type editCaps struct{ ws, ops bool }

func (l *lsp) editCaps() editCaps {
	if l.ext == nil {
		return editCaps{}
	}
	ws := l.ext.Has(rhunext.FeatureWorkspaceEdits)
	return editCaps{ws: ws, ops: ws && l.ext.Has(rhunext.FeatureFileOperations)}
}

// fileOp is a file operation of an edit, for the did… notifications
type fileOp struct {
	Kind   string `json:"kind"` // create, rename, delete
	Path   string `json:"path"`
	To     string `json:"to,omitempty"`
	Folder bool   `json:"folder,omitempty"`
}

// convertedEdit is an LSP WorkspaceEdit as rhun's: the edit, for each entry of its body the
// documentChanges index it came from (nil for changes), and its file operations
type convertedEdit struct {
	edit    rhunext.WorkspaceEdit
	origins []int
	ops     []fileOp
}

// editError is a WorkspaceEdit that cannot be made, and the documentChanges index at fault (-1)
type editError struct {
	change int
	err    error
}

func (e *editError) Error() string { return e.err.Error() }

// convertOptions say how to convert: rhun's caps; the snapshots of open documents to convert
// against (a document not among them is read as it is now); strict refuses positions outside
// the text (edits of commands and code actions); server is the one whose LSP versions the
// snapshots carry; label names the change
type convertOptions struct {
	caps    editCaps
	queries []*documentQuery
	strict  bool
	// Deferred results must use only snapshots captured before asking the server.
	snapshotsOnly bool
	server        *server
	label         string
}

// snapshot is the open document a path names: a query's snapshot, else the document as it is
func (l *lsp) snapshot(path string, o convertOptions) *documentQuery {
	for _, q := range o.queries {
		if q != nil && q.text != nil && filepath.Clean(q.path) == filepath.Clean(path) {
			return q
		}
	}
	if o.snapshotsOnly {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	d := l.byPath[path]
	if d == nil || d.text == nil {
		return nil
	}
	q := &documentQuery{id: d.id, origin: d, path: d.path, lang: d.lang, text: d.text, version: d.version, lspVersion: -1}
	if o.server != nil && d.syncedServer == o.server && d.syncedVersion == d.version {
		q.lspVersion = d.lspVer
	}
	return q
}

// lspAnnotatedEdit is a TextEdit or an AnnotatedTextEdit
type lspAnnotatedEdit struct {
	Range        *lspRange `json:"range"`
	NewText      *string   `json:"newText"`
	AnnotationID string    `json:"annotationId"`
}

// applyRhunEdits makes edits (rhun's positions, against text) as rhun does: sorted stably by
// start then end, apart
func applyRhunEdits(text []byte, edits []rhunext.TextEdit) ([]byte, error) {
	type change struct {
		start, end int
		text       string
	}
	cs := make([]change, 0, len(edits))
	for _, e := range edits {
		a, z := byteOffset(text, e.Range.Start), byteOffset(text, e.Range.End)
		if a > z {
			return nil, errors.New("a reversed range")
		}
		cs = append(cs, change{a, z, e.Text})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].start != cs[j].start {
			return cs[i].start < cs[j].start
		}
		return cs[i].end < cs[j].end
	})
	var b bytes.Buffer
	at := 0
	for _, c := range cs {
		if c.start < at {
			return nil, errors.New("two edits of a file overlap")
		}
		b.Write(text[at:c.start])
		b.WriteString(c.text)
		at = c.end
	}
	b.Write(text[at:])
	return b.Bytes(), nil
}

// convertEdit turns LSP's WorkspaceEdit into rhun's (with workspace-edits agreed)
func (l *lsp) convertEdit(raw json.RawMessage, o convertOptions) (convertedEdit, error) {
	var out convertedEdit
	obj, err := jsonObject(raw)
	if err != nil {
		return out, errors.New("invalid workspace edit")
	}
	needsConfirm := map[string]bool{}
	if annotations, err := jsonObject(obj["changeAnnotations"]); err == nil {
		for id, a := range annotations {
			var x struct {
				NeedsConfirmation bool `json:"needsConfirmation"`
			}
			if json.Unmarshal(a, &x) == nil && x.NeedsConfirmation {
				needsConfirm[id] = true
			}
		}
	}
	confirm := false
	var steps []rhunext.Change
	evolving := map[string][]byte{} // a file's text after its groups so far
	group := func(index int, uri string, version json.RawMessage, rawEdits json.RawMessage) error {
		path := uriPath(uri)
		if !filepath.IsAbs(path) {
			return fmt.Errorf("an edit for %s", uri)
		}
		at, err := rhunext.Locate(steps, path)
		if err != nil {
			return err
		}
		step := rhunext.Change{Kind: rhunext.ChangeEdit, Path: path, Version: rhunext.OnDisk}
		text, seen := evolving[at.Path]
		switch {
		case seen:
		case at.Created:
			text = []byte{}
		default:
			q := l.snapshot(at.Path, o)
			if o.snapshotsOnly && q == nil {
				return errors.New("code action target has no snapshot from before resolution")
			}
			// a numeric LSP version is meaningful only for a document synchronized to this
			// server; a command's edit naming another is refused
			if len(version) > 0 && string(version) != "null" {
				var v int
				if json.Unmarshal(version, &v) != nil || q != nil && q.lspVersion >= 0 && v != q.lspVersion ||
					o.strict && (q == nil || q.lspVersion < 0) {
					return errors.New("stale LSP workspace edit version")
				}
			}
			if q != nil {
				text, step.Version = q.text, q.version
			} else {
				data, err := os.ReadFile(at.Path)
				if err != nil {
					return errors.New("cannot read " + filepath.Base(at.Path))
				}
				text = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
				step.Hashed, step.Hash = true, rhunext.Fnv1a64(data)
			}
		}
		entries, err := boundedRawArray(rawEdits, rhunext.MaxEdits)
		if err != nil {
			return errors.New("invalid text edits")
		}
		for _, e := range entries {
			var x lspAnnotatedEdit
			if json.Unmarshal(e, &x) != nil || x.Range == nil || x.NewText == nil {
				return errors.New("an edit that cannot be read")
			}
			if needsConfirm[x.AnnotationID] {
				confirm = true
			}
			if before(x.Range.End, x.Range.Start) {
				return errors.New("reversed LSP edit range")
			}
			if o.strict {
				for _, p := range []lspPosition{x.Range.Start, x.Range.End} {
					if p.Line < 0 || p.Character < 0 || lspPos(text, byteOffset(text, rhunPos(text, p))) != p {
						return errors.New("LSP edit position is outside the document or inside a character")
					}
				}
			}
			step.Edits = append(step.Edits, rhunext.TextEdit{Range: rhunRange(text, *x.Range), Text: *x.NewText})
		}
		next, err := applyRhunEdits(text, step.Edits)
		if err != nil {
			return err
		}
		evolving[at.Path] = next
		steps = append(steps, step)
		out.origins = append(out.origins, index)
		return nil
	}
	docs, hasDocs := obj["documentChanges"]
	if hasDocs && string(docs) != "null" {
		entries, err := boundedRawArray(docs, rhunext.MaxEditFiles)
		if err != nil {
			return out, errors.New("invalid documentChanges")
		}
		for i, e := range entries {
			x, err := jsonObject(e)
			if err != nil {
				return out, &editError{i, errors.New("an edit that cannot be read")}
			}
			kind := ""
			if raw, ok := x["kind"]; ok {
				if kind, err = jsonString(raw); err != nil {
					return out, &editError{i, errors.New("an edit that cannot be read")}
				}
			}
			if kind == "" {
				doc, err := jsonObject(x["textDocument"])
				if err != nil {
					return out, &editError{i, err}
				}
				uri, err := jsonString(doc["uri"])
				if err != nil {
					return out, &editError{i, err}
				}
				if err := group(i, uri, doc["version"], x["edits"]); err != nil {
					return out, &editError{i, err}
				}
				continue
			}
			if !o.caps.ops {
				return out, &editError{i, errors.New("the edit creates, renames or deletes files")}
			}
			var op struct {
				URI, OldURI, NewURI string
				Options             struct {
					Overwrite, IgnoreIfExists, Recursive, IgnoreIfNotExists bool
				}
				AnnotationID string
			}
			if json.Unmarshal(e, &op) != nil {
				return out, &editError{i, errors.New("an edit that cannot be read")}
			}
			if needsConfirm[op.AnnotationID] {
				confirm = true
			}
			step := rhunext.Change{}
			fo := fileOp{Kind: kind}
			switch kind {
			case "create":
				step = rhunext.Change{Kind: rhunext.ChangeCreate, Path: uriPath(op.URI),
					Create: rhunext.CreateOptions{Overwrite: op.Options.Overwrite, IgnoreExisting: op.Options.IgnoreIfExists}}
			case "rename":
				step = rhunext.Change{Kind: rhunext.ChangeRename, Path: uriPath(op.OldURI), To: uriPath(op.NewURI),
					Rename: rhunext.RenameOptions{Overwrite: op.Options.Overwrite, IgnoreExisting: op.Options.IgnoreIfExists}}
				fo.To = step.To
			case "delete":
				step = rhunext.Change{Kind: rhunext.ChangeDelete, Path: uriPath(op.URI),
					Delete: rhunext.DeleteOptions{Recursive: op.Options.Recursive, IgnoreMissing: op.Options.IgnoreIfNotExists}}
			default:
				return out, &editError{i, fmt.Errorf("an unknown resource operation %q", kind)}
			}
			for _, p := range []string{step.Path, step.To} {
				if p != "" && !filepath.IsAbs(p) {
					return out, &editError{i, fmt.Errorf("a file operation on %s", p)}
				}
			}
			fo.Path = step.Path
			if st, err := os.Stat(step.Path); err == nil {
				fo.Folder = st.IsDir()
			}
			steps = append(steps, step)
			out.origins = append(out.origins, i)
			out.ops = append(out.ops, fo)
		}
	} else if changes, ok := obj["changes"]; ok && string(changes) != "null" {
		groups, err := jsonObject(changes)
		if err != nil || len(groups) > rhunext.MaxEditFiles {
			return out, errors.New("invalid changes")
		}
		uris := make([]string, 0, len(groups))
		for uri := range groups {
			uris = append(uris, uri)
		}
		sort.Strings(uris)
		for _, uri := range uris {
			if err := group(-1, uri, nil, groups[uri]); err != nil {
				return out, err
			}
		}
	}
	w, origins, err := rhunext.NormalizeOrderedOrigins(steps)
	if err != nil {
		var oe *rhunext.OrderError
		if errors.As(err, &oe) && oe.Change >= 0 && oe.Change < len(out.origins) {
			return convertedEdit{}, &editError{out.origins[oe.Change], err}
		}
		return convertedEdit{}, err
	}
	for k, n := range origins {
		origins[k] = out.origins[n]
	}
	// an open document of a snapshot is named by its id: that incarnation of it
	for i, f := range w.Files {
		for _, q := range o.queries {
			if q != nil && q.id != 0 && f.Version >= 0 && filepath.Clean(q.path) == filepath.Clean(f.Target.Path) && q.version == f.Version {
				w.Files[i].Target = rhunext.Target{Doc: q.id}
			}
		}
	}
	w.Label, w.Confirm = o.label, confirm
	out.edit, out.origins = w, origins
	if !hasDocs || string(docs) == "null" {
		out.origins = nil
	}
	return out, nil
}

// failedChange is the documentChanges index behind a refused apply: the entry rhun named, or
// the change the conversion refused; -1 when there is none
func failedChange(err error, origins []int) int {
	var ae *rhunext.ApplyError
	if errors.As(err, &ae) && ae.Entry >= 1 && ae.Entry <= len(origins) {
		return origins[ae.Entry-1]
	}
	var ee *editError
	if errors.As(err, &ee) {
		return ee.change
	}
	return -1
}

// ---- workspace/did{Create,Rename,Delete}Files

var fileOpMethods = map[string]string{"create": "workspace/didCreateFiles", "rename": "workspace/didRenameFiles", "delete": "workspace/didDeleteFiles"}

func isFileOpMethod(method string) bool {
	for _, m := range fileOpMethods {
		if m == method {
			return true
		}
	}
	return false
}

// fileOpFilter is one of a server's FileOperationFilters
type fileOpFilter struct {
	scheme  string
	pattern documentFilter
	matches string // file, folder or ""
}

// parseFileOpFilters reads FileOperationRegistrationOptions' filters
func parseFileOpFilters(raw json.RawMessage) ([]fileOpFilter, error) {
	obj, err := jsonObject(raw)
	if err != nil {
		return nil, errors.New("invalid file operation options")
	}
	var entries []json.RawMessage
	if f := obj["filters"]; len(f) == 0 || f[0] != '[' || json.Unmarshal(f, &entries) != nil || len(entries) > 256 {
		return nil, errors.New("invalid file operation filters")
	}
	out := make([]fileOpFilter, 0, len(entries))
	for _, e := range entries {
		var x struct {
			Scheme  string `json:"scheme"`
			Pattern struct {
				Glob    string `json:"glob"`
				Matches string `json:"matches"`
				Options struct {
					IgnoreCase bool `json:"ignoreCase"`
				} `json:"options"`
			} `json:"pattern"`
		}
		if json.Unmarshal(e, &x) != nil || x.Pattern.Glob == "" {
			return nil, errors.New("invalid file operation filter")
		}
		re, err := compileGlob(x.Pattern.Glob, x.Pattern.Options.IgnoreCase)
		if err != nil {
			return nil, err
		}
		out = append(out, fileOpFilter{scheme: x.Scheme, pattern: documentFilter{pattern: re}, matches: x.Pattern.Matches})
	}
	return out, nil
}

func (f fileOpFilter) match(path string, folder bool) bool {
	if f.scheme != "" && f.scheme != "file" {
		return false
	}
	if f.matches == "file" && folder || f.matches == "folder" && !folder {
		return false
	}
	return f.pattern.pattern.MatchString(filepath.ToSlash(path))
}

// wantsFileOp says whether the server asked to hear of an operation of this kind on path
func (s *server) wantsFileOp(kind, path string, folder bool) bool {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	if s.ended.Load() {
		return false
	}
	for _, f := range s.fileOpFilters[kind] {
		if f.match(path, folder) {
			return true
		}
	}
	for _, r := range s.registrations {
		if r.method == fileOpMethods[kind] {
			for _, f := range r.fileFilters {
				if f.match(path, folder) {
					return true
				}
			}
		}
	}
	return false
}

// loadFileOpCapabilities reads capabilities.workspace.fileOperations' didCreate, didRename and
// didDelete
func (s *server) loadFileOpCapabilities(caps map[string]json.RawMessage) {
	filters := map[string][]fileOpFilter{}
	if ws, err := jsonObject(caps["workspace"]); err == nil {
		if ops, err := jsonObject(ws["fileOperations"]); err == nil {
			for kind, key := range map[string]string{"create": "didCreate", "rename": "didRename", "delete": "didDelete"} {
				if raw, ok := ops[key]; ok && string(raw) != "null" {
					if f, err := parseFileOpFilters(raw); err == nil {
						filters[kind] = f
					} else if s.l.ext != nil {
						s.l.ext.Logf("ignoring invalid fileOperations.%s: %v", key, err)
					}
				}
			}
		}
	}
	s.capMu.Lock()
	s.fileOpFilters = filters
	s.capMu.Unlock()
}

// notifyFileOps tells every running server that asked of the file operations an edit made
func (l *lsp) notifyFileOps(ops []fileOp) {
	if len(ops) == 0 {
		return
	}
	l.mu.Lock()
	var servers []*server
	for _, s := range l.servers {
		if s != nil && s.ready.Load() {
			servers = append(servers, s)
		}
	}
	l.mu.Unlock()
	for _, s := range servers {
		files := map[string][]map[string]any{}
		for _, o := range ops {
			folder := o.Folder
			if o.Kind != "delete" {
				target := o.Path
				if o.Kind == "rename" {
					target = o.To
				}
				if st, err := os.Stat(target); err == nil {
					folder = st.IsDir()
				}
			}
			if !s.wantsFileOp(o.Kind, o.Path, folder) && (o.Kind != "rename" || !s.wantsFileOp(o.Kind, o.To, folder)) {
				continue
			}
			if o.Kind == "rename" {
				files[o.Kind] = append(files[o.Kind], map[string]any{"oldUri": fileURI(o.Path), "newUri": fileURI(o.To)})
			} else {
				files[o.Kind] = append(files[o.Kind], map[string]any{"uri": fileURI(o.Path)})
			}
		}
		for _, kind := range []string{"create", "rename", "delete"} {
			if len(files[kind]) > 0 {
				s.notify(fileOpMethods[kind], map[string]any{"files": files[kind]})
			}
		}
	}
}

// pendingFileOps are the file operations of edits answered to rhun (a rename's): rhun makes
// them when the user applies them, and says nothing back, so they are told to the servers once
// the disk shows them made (looked at on the next document and workspace events), or forgotten
type pendingFileOps struct {
	mu    sync.Mutex
	items []pendingOps
}

type pendingOps struct {
	ops      []fileOp
	deadline time.Time
}

func (l *lsp) expectFileOps(ops []fileOp) {
	if len(ops) == 0 {
		return
	}
	l.pending.mu.Lock()
	if len(l.pending.items) >= 16 {
		l.pending.items = l.pending.items[1:]
	}
	l.pending.items = append(l.pending.items, pendingOps{ops: ops, deadline: time.Now().Add(2 * time.Minute)})
	l.pending.mu.Unlock()
}

// made says whether the disk shows an operation made
func (o fileOp) made() bool {
	_, err := os.Stat(o.Path)
	switch o.Kind {
	case "create":
		return err == nil
	case "delete":
		return errors.Is(err, os.ErrNotExist)
	}
	_, err2 := os.Stat(o.To)
	return errors.Is(err, os.ErrNotExist) && err2 == nil
}

// checkFileOps tells the servers of the pending operations the disk shows made
func (l *lsp) checkFileOps() {
	l.pending.mu.Lock()
	var done [][]fileOp
	keep := l.pending.items[:0]
	now := time.Now()
	for _, p := range l.pending.items {
		all := true
		for _, o := range p.ops {
			all = all && o.made()
		}
		switch {
		case all:
			done = append(done, p.ops)
		case now.Before(p.deadline):
			keep = append(keep, p)
		}
	}
	l.pending.items = keep
	l.pending.mu.Unlock()
	for _, ops := range done {
		l.notifyFileOps(ops)
	}
}

// fileOpEvent looks at the pending operations on the events that follow a change on disk
func (l *lsp) fileOpEvent(ev rhunext.Event) {
	switch ev.Name {
	case "opened", "closed", "watch":
		l.pending.mu.Lock()
		waiting := len(l.pending.items) > 0
		l.pending.mu.Unlock()
		if waiting {
			go l.checkFileOps()
		}
	}
}

// ---- window/showDocument

// showDocument opens a file of the server's window/showDocument in rhun with the selection, and
// reveals it; external documents and other schemes are refused (success false). rhun's open
// always activates the tab, so takeFocus false is not honoured.
func (s *server) showDocument(id json.RawMessage, params json.RawMessage) {
	ok := s.showDocumentResult(params)
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"success": ok}})
}

func (s *server) showDocumentResult(params json.RawMessage) bool {
	var p struct {
		URI       string    `json:"uri"`
		External  bool      `json:"external"`
		Selection *lspRange `json:"selection"`
	}
	ext := s.l.ext
	if ext == nil || json.Unmarshal(params, &p) != nil || p.External || !strings.HasPrefix(p.URI, "file:") {
		return false
	}
	path := uriPath(p.URI)
	if !filepath.IsAbs(path) {
		return false
	}
	text, _, err := s.l.textOf(path)
	if err != nil {
		return false
	}
	var at rhunext.Pos
	if p.Selection != nil {
		at = rhunPos(text, p.Selection.Start)
	}
	doc, err := ext.Open(path, at)
	if err != nil {
		return false
	}
	if p.Selection != nil {
		if r := rhunRange(text, *p.Selection); r.Start != r.End {
			ext.Select(doc, r)
		}
		if ext.Has(rhunext.FeatureReveal) {
			ext.Reveal(doc, at, rhunext.RevealCenterIfOutside)
		}
	}
	return true
}

// workspaceEditCapabilities is the client's workspace.workspaceEdit for what rhun agreed: with
// workspace-edits an edit is a transaction, with file-operations it creates, renames and deletes
func workspaceEditCapabilities(caps editCaps) map[string]any {
	if !caps.ws {
		return map[string]any{"documentChanges": true, "resourceOperations": []string{}}
	}
	ops := []string{}
	if caps.ops {
		ops = []string{"create", "rename", "delete"}
	}
	return map[string]any{"documentChanges": true, "resourceOperations": ops, "failureHandling": "transactional",
		"normalizesLineEndings": true, "changeAnnotationSupport": map[string]any{"groupsOnLabel": false}}
}

// movedByOps says whether a document's path is renamed or deleted by file operations (itself or
// a folder holding it)
func movedByOps(path string, ops []rhunext.FileOp) bool {
	for _, o := range ops {
		if o.Kind == rhunext.OpCreate {
			continue
		}
		p := filepath.Clean(o.Path)
		if filepath.Clean(path) == p || strings.HasPrefix(filepath.Clean(path), p+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
