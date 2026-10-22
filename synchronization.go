package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

type synchronizationOptions struct {
	openClose         bool
	change            int
	save, includeText bool
}

func (s *server) loadSynchronization(raw json.RawMessage) error {
	result, err := jsonObject(raw)
	if err != nil {
		return fmt.Errorf("invalid initialize result")
	}
	caps, err := jsonObject(result["capabilities"])
	if err != nil {
		return fmt.Errorf("invalid initialize capabilities")
	}
	if encoding, ok := caps["positionEncoding"]; ok {
		value, err := jsonString(encoding)
		if err != nil || value != "utf-16" {
			return fmt.Errorf("server selected unsupported position encoding")
		}
	}
	options := synchronizationOptions{}
	value := caps["textDocumentSync"]
	if len(value) == 0 || string(value) == "null" {
		s.syncOptions = options
		return nil
	}
	if value[0] != '{' {
		if json.Unmarshal(value, &options.change) != nil || string(value) == "null" || options.change < 0 || options.change > 2 {
			return fmt.Errorf("invalid textDocumentSync kind")
		}
		options.openClose = options.change != 0
		options.save = options.openClose
	} else {
		obj, err := jsonObject(value)
		if err != nil {
			return err
		}
		for key, raw := range obj {
			switch key {
			case "openClose":
				if string(raw) != "true" && string(raw) != "false" {
					return fmt.Errorf("invalid openClose option")
				}
				options.openClose = string(raw) == "true"
			case "change":
				if json.Unmarshal(raw, &options.change) != nil || string(raw) == "null" || options.change < 0 || options.change > 2 {
					return fmt.Errorf("invalid synchronization change kind")
				}
			case "save":
				if string(raw) == "true" || string(raw) == "false" {
					options.save = string(raw) == "true"
					continue
				}
				fields, err := jsonObject(raw)
				if err != nil {
					return fmt.Errorf("invalid synchronization save option")
				}
				options.save = true
				if include, ok := fields["includeText"]; ok {
					if string(include) != "true" && string(include) != "false" {
						return fmt.Errorf("invalid includeText option")
					}
					options.includeText = string(include) == "true"
				}
			}
		}
	}
	s.syncOptions = options
	return nil
}

// syncMu orders document frames without blocking configuration callbacks on l.mu.
func (l *lsp) ensureSynchronized(d *document, s *server) bool {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	return l.synchronizeDeltaLocked(d, s, nil, -1)
}

// Callers without an explicit incremental base always send a full update.
func (l *lsp) synchronizeLocked(d *document, s *server, changes []map[string]any) bool {
	return l.synchronizeDeltaLocked(d, s, changes, -1)
}

func (l *lsp) synchronizeDeltaLocked(d *document, s *server, changes []map[string]any, baseVersion int64) bool {
	l.mu.Lock()
	if l.stopping || l.docs[d.id] != d || d.server != s || s == nil || !s.ready.Load() || d.text == nil {
		l.mu.Unlock()
		return false
	}
	options, _, generation := s.synchronizationFor(selectorFor(d))
	first := d.syncedServer != s
	version, text := d.version, d.text
	lspVersion, opened := d.lspVer, d.opened
	if first {
		lspVersion = 1
		opened = false
	}
	type syncFrame struct {
		method  string
		params  any
		version int
	}
	var frames []syncFrame
	sentOpen := !opened && options.openClose
	if sentOpen {
		if !first {
			lspVersion++
		}
		frames = append(frames, syncFrame{"textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path), "languageId": languageIDs[d.lang], "version": lspVersion, "text": string(text)}}, lspVersion})
		opened = true
	}
	if options.change != 0 && !sentOpen && (first || d.syncedVersion != version || d.syncChange == 0) {
		if !first {
			lspVersion++
		}
		if options.change == 1 || changes == nil || d.syncChange == 0 || d.syncedVersion != baseVersion {
			changes = []map[string]any{{"text": string(text)}}
		}
		frames = append(frames, syncFrame{"textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path), "version": lspVersion}, "contentChanges": changes}, lspVersion})
	}
	flushSave := d.pendingSave && d.saveVersion <= version
	if flushSave && options.save {
		params := map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}}
		if options.includeText {
			params["text"] = string(text)
		}
		frames = append(frames, syncFrame{"textDocument/didSave", params, lspVersion})
	}
	l.mu.Unlock()
	guard := func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		s.capMu.RLock()
		defer s.capMu.RUnlock()
		if l.stopping || l.docs[d.id] != d || d.server != s || s.ended.Load() || s.syncGeneration != generation {
			return errors.New("synchronization registration retired")
		}
		return nil
	}
	for _, frame := range frames {
		if err := s.writeContextGuard(context.Background(), map[string]any{"jsonrpc": "2.0", "method": frame.method, "params": frame.params}, guard); err != nil {
			return false
		}
		l.mu.Lock()
		if l.docs[d.id] != d || d.server != s {
			l.mu.Unlock()
			return false
		}
		// Retain successfully transmitted state even if a later frame is rejected
		// by a concurrent registration change. In particular, never open twice.
		d.syncedServer = s
		d.lspVer = frame.version
		if frame.method == "textDocument/didOpen" {
			d.opened = true
			d.syncedVersion = version
		}
		if frame.method == "textDocument/didChange" {
			d.syncedVersion = version
			d.syncChange = options.change
		}
		if frame.method == "textDocument/didSave" {
			d.pendingSave = false
		}
		l.mu.Unlock()
	}
	for _, frame := range frames {
		if frame.method != "textDocument/didSave" {
			// the text changed for the server: ask for its diagnostics when it gives them on request
			defer l.schedulePull(d, s)
			break
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	if l.stopping || l.docs[d.id] != d || d.server != s || !s.ready.Load() || s.syncGeneration != generation {
		return false
	}
	d.syncedServer = s
	d.syncedVersion = version
	d.lspVer = lspVersion
	d.opened = opened
	d.syncChange = options.change
	if flushSave {
		d.pendingSave = false
	}
	return true
}

func (l *lsp) awaitServer(d *document, s *server) {
	if s != nil && s.ready.Load() {
		l.ensureSynchronized(d, s)
		return
	}
	l.mu.Lock()
	if s == nil || l.stopping || l.docs[d.id] != d || d.server != s || d.awaiting == s {
		l.mu.Unlock()
		return
	}
	d.awaiting = s
	l.mu.Unlock()
	go func() {
		s.waitReady()
		l.mu.Lock()
		if d.awaiting == s {
			d.awaiting = nil
		}
		waiting := d.presWaiting > 0
		l.mu.Unlock()
		// (a waiting presentation request opens it, or the edits it holds back do; edits on
		// their way, when rhun is past the cached text, open it with the newest)
		if !waiting {
			l.openIfCurrent(d)
		}
	}()
}
func (l *lsp) fetchDocument(d *document) {
	l.mu.Lock()
	if l.stopping || l.docs[d.id] != d || d.fetching {
		l.mu.Unlock()
		return
	}
	d.fetching = true
	read := l.fetchText
	if read == nil {
		read = l.ext.Text
	}
	l.mu.Unlock()
	go func() {
		for {
			text, version, err := read(d.id)
			d.syncMu.Lock()
			l.mu.Lock()
			if l.stopping || l.docs[d.id] != d {
				d.fetching = false
				l.mu.Unlock()
				d.syncMu.Unlock()
				return
			}
			if err != nil {
				d.fetching = false
				l.mu.Unlock()
				d.syncMu.Unlock()
				if l.ext != nil {
					l.ext.Logf("document refresh: %v", err)
				}
				return
			}
			if version < d.version {
				if d.text == nil {
					l.mu.Unlock()
					d.syncMu.Unlock()
					time.Sleep(10 * time.Millisecond)
					continue
				}
			} else {
				d.text = append([]byte{}, text...)
				d.version = version
			}
			d.fetching = false
			if d.server == nil {
				d.server = l.serverFor(d.lang)
			}
			s := d.server
			l.mu.Unlock()
			if s != nil && s.ready.Load() {
				l.synchronizeDeltaLocked(d, s, nil, -1)
			}
			d.syncMu.Unlock()
			l.awaitServer(d, s)
			return
		}
	}()
}

func (l *lsp) documentEvent(ev rhunext.Event) {
	if ev.Name == "opened" {
		o, ok := rhunext.ParseOpened(ev)
		if !ok || o.Path == "" {
			return
		}
		l.mu.Lock()
		if l.stopping || l.docs[o.Doc] != nil {
			l.mu.Unlock()
			return
		}
		d := &document{id: o.Doc, version: o.Version, lang: o.Lang, path: o.Path}
		l.docs[d.id] = d
		l.byPath[d.path] = d
		l.mu.Unlock()
		l.fetchDocument(d)
		return
	}
	var id, version int64
	fmt.Sscan(argOr(ev, 0), &id)
	fmt.Sscan(argOr(ev, 1), &version)
	if ev.Name == "hover" {
		l.hoverEvent(ev)
		return
	}
	l.mu.Lock()
	d := l.docs[rhunext.Doc(id)]
	l.mu.Unlock()
	if d == nil {
		return
	}
	d.syncMu.Lock()
	l.mu.Lock()
	if l.stopping || l.docs[d.id] != d {
		l.mu.Unlock()
		d.syncMu.Unlock()
		return
	}
	needFetch := false
	baseVersion := d.version
	var changes []map[string]any
	s := d.server
	switch ev.Name {
	case "closed":
		delete(l.docs, d.id)
		delete(l.byPath, d.path)
		l.forgetPull(d)
		closeServer := d.syncedServer
		d.opened = false
		d.pendingSave = false
		l.mu.Unlock()
		if closeServer != nil && closeServer.ready.Load() {
			guard := func() error {
				_, interested, _ := closeServer.synchronizationFor(selectorFor(d))
				if !interested || closeServer.ended.Load() {
					return errors.New("close registration retired")
				}
				return nil
			}
			closeServer.writeContextGuard(context.Background(), map[string]any{"jsonrpc": "2.0", "method": "textDocument/didClose", "params": map[string]any{"textDocument": map[string]any{"uri": fileURI(d.path)}}}, guard)
		}
		d.syncMu.Unlock()
		return
	case "edits":
		if version <= d.version {
			l.mu.Unlock()
			d.syncMu.Unlock()
			return
		}
		edits, err := rhunext.ParseEdits(ev.Body)
		text := d.text
		if err == nil && text != nil {
			for _, edit := range edits {
				if edit.Start < 0 || edit.Del < 0 || edit.Start > len(text) || edit.Del > len(text)-edit.Start {
					err = fmt.Errorf("edit outside cached text")
					break
				}
				changes = append(changes, map[string]any{"range": map[string]any{"start": lspPos(text, edit.Start), "end": lspPos(text, edit.Start+edit.Del)}, "text": string(edit.Ins)})
				next := make([]byte, 0, len(text)-edit.Del+len(edit.Ins))
				next = append(next, text[:edit.Start]...)
				next = append(next, edit.Ins...)
				next = append(next, text[edit.Start+edit.Del:]...)
				text = next
			}
		}
		d.version = version
		if err != nil || d.text == nil {
			d.text = nil
			needFetch = true
		} else {
			d.text = text
		}
	case "resync":
		if version <= d.version && d.text != nil {
			l.mu.Unlock()
			d.syncMu.Unlock()
			return
		}
		if version > d.version {
			d.version = version
		}
		d.text = nil
		needFetch = true
	case "saved":
		d.pendingSave = true
		if version > d.saveVersion {
			d.saveVersion = version
		}
		if d.text == nil || d.version < version {
			needFetch = true
		}
	default:
		l.mu.Unlock()
		d.syncMu.Unlock()
		return
	}
	l.mu.Unlock()
	if !needFetch && s != nil {
		l.synchronizeDeltaLocked(d, s, changes, baseVersion)
	}
	d.syncMu.Unlock()
	if needFetch {
		l.fetchDocument(d)
	}
}
