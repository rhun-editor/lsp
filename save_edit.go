package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// A save edit is resolved entirely against one synchronized query snapshot. LSP and native
// versions are separate counters. Resource operations and a second target are never admitted.
func (l *lsp) saveWorkspaceEdit(raw json.RawMessage, q *documentQuery) (rhunext.WorkspaceEdit, error) {
	var out rhunext.WorkspaceEdit
	obj, err := jsonObject(raw)
	if err != nil || q == nil {
		return out, errors.New("invalid save workspace edit")
	}
	changes, hasChanges := obj["changes"]
	documentChanges, hasDocuments := obj["documentChanges"]
	if hasChanges && hasDocuments {
		return out, errors.New("workspace edit mixes changes and documentChanges")
	}
	type group struct {
		uri       string
		edits     json.RawMessage
		version   json.RawMessage
		versioned bool
	}
	var groups []group
	if hasDocuments {
		entries, arrayErr := boundedRawArray(documentChanges, 1)
		if arrayErr != nil {
			return out, errors.New("invalid documentChanges")
		}
		for _, raw := range entries {
			entry, err := jsonObject(raw)
			if err != nil || entry["kind"] != nil {
				return out, errors.New("save edit contains a resource operation")
			}
			doc, err := jsonObject(entry["textDocument"])
			if err != nil {
				return out, err
			}
			uri, err := jsonString(doc["uri"])
			version, ok := doc["version"]
			if err != nil || !ok {
				return out, errors.New("invalid versioned document edit")
			}
			groups = append(groups, group{uri, entry["edits"], version, true})
		}
	} else if hasChanges {
		changes, err := jsonObject(changes)
		if err != nil || len(changes) > 128 {
			return out, errors.New("invalid changes")
		}
		for uri, edits := range changes {
			groups = append(groups, group{uri: uri, edits: edits})
		}
	}
	if len(groups) > 1 {
		return out, errors.New("save edit must contain at most one document group")
	}
	for _, g := range groups {
		path := uriPath(g.uri)
		if !filepath.IsAbs(path) || filepath.Clean(path) != filepath.Clean(q.path) {
			return out, errors.New("save edit targets another document")
		}
		if g.versioned && string(g.version) != "null" {
			var version int
			if json.Unmarshal(g.version, &version) != nil || version < 0 || version != q.lspVersion {
				return out, errors.New("stale LSP workspace edit version")
			}
		}
		edits, arrayErr := boundedRawArray(g.edits, rhunext.MaxEdits)
		if arrayErr != nil {
			return out, errors.New("invalid text edits")
		}
		for _, raw := range edits {
			edit, err := jsonObject(raw)
			if err != nil {
				return out, err
			}
			if _, err = jsonString(edit["newText"]); err != nil {
				return out, err
			}
			span, err := jsonObject(edit["range"])
			if err != nil {
				return out, err
			}
			var ends [2]lspPosition
			for i, key := range []string{"start", "end"} {
				p, err := jsonObject(span[key])
				if err != nil {
					return out, err
				}
				if len(p["line"]) == 0 || len(p["character"]) == 0 || string(p["line"]) == "null" || string(p["character"]) == "null" ||
					json.Unmarshal(p["line"], &ends[i].Line) != nil || json.Unmarshal(p["character"], &ends[i].Character) != nil || ends[i].Line < 0 || ends[i].Character < 0 {
					return out, errors.New("invalid LSP edit position")
				}
				converted := rhunPos(q.text, ends[i])
				if lspPos(q.text, byteOffset(q.text, converted)) != ends[i] {
					return out, fmt.Errorf("LSP edit position is outside the document or inside a character")
				}
			}
			if before(ends[1], ends[0]) {
				return out, errors.New("reversed LSP edit range")
			}
		}
	}
	converted, err := l.workspaceEdit(raw, q)
	if err == nil && q.id != 0 {
		for i := range converted.Files {
			converted.Files[i].Target = rhunext.Target{Doc: q.id}
		}
	}
	return converted, err
}

// Deferred resolution must retain the versions and text that existed before the server
// calculated its edits. Text buffers are immutable; later notifications replace them.
func (l *lsp) commandQuerySnapshots(s *server, origin *documentQuery) []*documentQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	queries := []*documentQuery{origin}
	for _, d := range l.docs {
		if origin != nil && d.id == origin.id {
			continue
		}
		q := &documentQuery{id: d.id, origin: d, path: d.path, lang: d.lang,
			text: d.text, version: d.version, lspVersion: -1}
		if d.syncedServer == s && d.syncedVersion == d.version {
			q.lspVersion = d.lspVer
		}
		queries = append(queries, q)
	}
	return queries
}

// commandWorkspaceEditFull is commandWorkspaceEdit with what a transaction needs: with
// workspace-edits agreed the edit goes through the SDK's normaliser (file operations, files that
// are not open hashed on disk, repeated groups of a file composed), named by label, with the
// documentChanges index behind each entry and its file operations.
func (l *lsp) commandWorkspaceEditFull(raw json.RawMessage, s *server, label string, queries ...*documentQuery) (convertedEdit, error) {
	if caps := l.editCaps(); caps.ws {
		return l.convertEdit(raw, convertOptions{caps: caps, queries: queries, strict: true, server: s, label: label})
	}
	edit, err := l.commandWorkspaceEdit(raw, s, queries...)
	return convertedEdit{edit: edit}, err
}

// A deferred answer cannot safely use text first observed after the server started work.
func (l *lsp) deferredWorkspaceEditFull(raw json.RawMessage, s *server, label string, queries ...*documentQuery) (convertedEdit, error) {
	if caps := l.editCaps(); caps.ws {
		return l.convertEdit(raw, convertOptions{caps: caps, queries: queries, strict: true, snapshotsOnly: true, server: s, label: label})
	}
	edit, err := l.commandWorkspaceEdit(raw, s, queries...)
	return convertedEdit{edit: edit}, err
}

// Command edits may touch several files. Each group is converted against its own immutable
// snapshot. A numeric LSP version is meaningful only for a document synchronized to this server.
func (l *lsp) commandWorkspaceEdit(raw json.RawMessage, s *server, queries ...*documentQuery) (rhunext.WorkspaceEdit, error) {
	if caps := l.editCaps(); caps.ws {
		conv, err := l.convertEdit(raw, convertOptions{caps: caps, queries: queries, strict: true, server: s})
		return conv.edit, err
	}
	var out rhunext.WorkspaceEdit
	obj, err := jsonObject(raw)
	if err != nil {
		return out, err
	}
	changes, hasChanges := obj["changes"]
	docs, hasDocs := obj["documentChanges"]
	if hasChanges && hasDocs {
		return out, errors.New("workspace edit mixes changes and documentChanges")
	}
	type group struct {
		uri       string
		raw       json.RawMessage
		versioned bool
		version   json.RawMessage
	}
	var groups []group
	if hasDocs {
		entries, arrayErr := boundedRawArray(docs, rhunext.MaxEditFiles)
		if arrayErr != nil {
			return out, errors.New("invalid documentChanges")
		}
		for _, entry := range entries {
			x, err := jsonObject(entry)
			if err != nil || x["kind"] != nil {
				return out, errors.New("command edit contains a resource operation")
			}
			doc, err := jsonObject(x["textDocument"])
			if err != nil {
				return out, err
			}
			uri, err := jsonString(doc["uri"])
			if err != nil {
				return out, err
			}
			body, _ := json.Marshal(map[string]any{"documentChanges": []json.RawMessage{entry}})
			groups = append(groups, group{uri: uri, raw: body, versioned: true, version: doc["version"]})
		}
	} else if hasChanges {
		entries, err := jsonObject(changes)
		if err != nil || len(entries) > rhunext.MaxEditFiles {
			return out, errors.New("invalid changes")
		}
		for uri, edits := range entries {
			body, _ := json.Marshal(map[string]any{"changes": map[string]json.RawMessage{uri: edits}})
			groups = append(groups, group{uri: uri, raw: body})
		}
	}
	seen := map[string]bool{}
	for _, g := range groups {
		path := filepath.ToSlash(filepath.Clean(uriPath(g.uri)))
		if !filepath.IsAbs(path) || seen[path] {
			return out, errors.New("invalid or duplicate command edit target")
		}
		seen[path] = true
		var q *documentQuery
		if len(queries) != 0 {
			for _, query := range queries {
				if query != nil && filepath.ToSlash(filepath.Clean(query.path)) == path {
					q = query
					break
				}
			}
			if q == nil || q.text == nil {
				return out, errors.New("code action target has no snapshot from before resolution")
			}
		} else {
			l.mu.Lock()
			d := l.byPath[path]
			if d != nil && d.text != nil {
				q = &documentQuery{id: d.id, origin: d, path: path, lang: d.lang, text: d.text, version: d.version, lspVersion: -1}
				if d.syncedServer == s && d.syncedVersion == d.version {
					q.lspVersion = d.lspVer
				}
			}
			l.mu.Unlock()
		}
		if q == nil {
			text, version, err := l.textOf(path)
			if err != nil {
				return out, err
			}
			q = &documentQuery{path: path, text: text, version: version, lspVersion: -1}
		}
		converted, err := l.saveWorkspaceEdit(g.raw, q)
		if err != nil {
			return out, err
		}
		out.Files = append(out.Files, converted.Files...)
	}
	return out, nil
}

// Decode incrementally so a short RPC array of millions of tiny objects cannot allocate an
// unbounded slice before the logical request limit is checked.
func boundedRawArray(raw json.RawMessage, limit int) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("expected array")
	}
	var items []json.RawMessage
	for decoder.More() {
		if len(items) >= limit {
			return nil, errors.New("array item limit exceeded")
		}
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if _, err = decoder.Token(); err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, errors.New("trailing array data")
	}
	return items, nil
}

// A snapshot of an open document names that incarnation, even if another tab later
// reopens its old path with the same per-document version counter.
func queryMatchesTarget(q *documentQuery, target rhunext.Target) bool {
	if q == nil {
		return false
	}
	if target.Doc != 0 {
		return q.id == target.Doc
	}
	return filepath.Clean(q.path) == filepath.Clean(target.Path)
}
