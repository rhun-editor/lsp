package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

const watchedFilesMethod = "workspace/didChangeWatchedFiles"
const maxWatchEntries = 32768
const maxWatchDirectories = 512
const maxWatchPathBytes = 8 << 20

type watchedFileRule struct {
	filter documentFilter
	kind   int
}
type watchedFileState struct{ info os.FileInfo }
type watchedSnapshot map[string]watchedFileState
type watchedChange struct {
	path string
	kind int
}

type workspaceWatcher struct {
	mu        sync.Mutex // serializes scan, publication and watched-file registry commits
	l         *lsp
	root      string
	handle    rhunext.WorkspaceWatch
	baseline  watchedSnapshot
	wake      chan struct{}
	done      chan struct{}
	once      sync.Once
	scan      func(string, <-chan struct{}) (watchedSnapshot, error)
	lastError time.Time
}

func scanWatchedWorkspace(path string, done <-chan struct{}) (watchedSnapshot, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	snapshot := watchedSnapshot{}
	dirs := []string{"."}
	pathBytes := 0
	for at := 0; at < len(dirs); at++ {
		select {
		case <-done:
			return nil, errors.New("watch runtime retired")
		default:
		}
		dir, err := root.Open(dirs[at])
		if err != nil {
			return nil, err
		}
		err = func() error {
			defer dir.Close()
			for {
				entries, err := dir.ReadDir(128)
				if err != nil && err != io.EOF {
					return err
				}
				for _, entry := range entries {
					select {
					case <-done:
						return errors.New("watch runtime retired")
					default:
					}
					relative := filepath.Join(dirs[at], entry.Name())
					absolute := filepath.Join(path, relative)
					pathBytes += len(absolute)
					if len(absolute) > 4095 || pathBytes > maxWatchPathBytes || len(snapshot) >= maxWatchEntries {
						return errors.New("workspace watch snapshot limit exceeded")
					}
					info, err := root.Lstat(relative)
					if err != nil {
						return err
					}
					snapshot[absolute] = watchedFileState{info: info}
					if info.IsDir() {
						if len(dirs) >= maxWatchDirectories {
							return errors.New("workspace watch directory limit exceeded")
						}
						dirs = append(dirs, relative)
					}
				}
				if err == io.EOF {
					return nil
				}
			}
		}()
		if err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func diffWatchedSnapshots(old, next watchedSnapshot) []watchedChange {
	changes := make([]watchedChange, 0)
	for path, now := range next {
		before, exists := old[path]
		kind := 1
		if exists {
			a, b := before.info, now.info
			if a.ModTime() == b.ModTime() && a.Size() == b.Size() && a.Mode() == b.Mode() && os.SameFile(a, b) {
				continue
			}
			kind = 2
		}
		changes = append(changes, watchedChange{path, kind})
	}
	for path := range old {
		if _, exists := next[path]; !exists {
			changes = append(changes, watchedChange{path, 3})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes
}

func (l *lsp) startWorkspaceWatcher() {
	if l.ext == nil || !l.ext.Has(rhunext.FeatureWorkspaceWatch) || l.project == "" || l.project == "-" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	handle, err := l.ext.WatchWorkspaceContext(ctx)
	cancel()
	if err != nil {
		l.ext.Logf("workspace watch: %v", err)
		return
	}
	w := &workspaceWatcher{l: l, root: filepath.Clean(l.project), handle: handle, wake: make(chan struct{}, 1), done: make(chan struct{}), scan: scanWatchedWorkspace}
	w.baseline, err = w.scan(w.root, w.done)
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		l.ext.UnwatchWorkspaceContext(ctx, handle)
		cancel()
		l.ext.Logf("workspace watch: %v", err)
		return
	}
	l.mu.Lock()
	l.watcher = w
	l.mu.Unlock()
	go w.run()
}
func (l *lsp) workspaceWatchAvailable() bool {
	l.mu.Lock()
	w := l.watcher
	l.mu.Unlock()
	return w != nil && !w.closed()
}
func (w *workspaceWatcher) closed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}
func (w *workspaceWatcher) run() {
	for {
		select {
		case <-w.done:
			return
		case <-w.wake:
			w.mu.Lock()
			err := w.reconcileLocked()
			if err != nil && !w.closed() && w.l.ext != nil && time.Since(w.lastError) >= 10*time.Second {
				w.l.ext.Logf("workspace watch: %v", err)
				w.lastError = time.Now()
			}
			w.mu.Unlock()
		}
	}
}
func (w *workspaceWatcher) reconcileLocked() error {
	if w.closed() {
		return errors.New("watch runtime retired")
	}
	next, err := w.scan(w.root, w.done)
	if err != nil {
		return err
	}
	w.l.mu.Lock()
	active := w.l.watcher == w && !w.l.stopping
	var servers []*server
	for _, s := range w.l.servers {
		if s != nil {
			servers = append(servers, s)
		}
	}
	w.l.mu.Unlock()
	if !active || w.closed() {
		return errors.New("watch runtime retired")
	}
	changes := diffWatchedSnapshots(w.baseline, next)
	// The mutex also excludes watched-file registration commits. A generation
	// checked at the write gate rejects queued frames retired by server shutdown.
	for _, s := range servers {
		if err := s.publishWatchedChanges(changes, w); err != nil {
			return err
		}
	}
	if w.closed() {
		return errors.New("watch runtime retired")
	}
	w.baseline = next
	return nil
}
func (l *lsp) workspaceWatchEvent(ev rhunext.Event) {
	if ev.Name == "project" || ev.Name == "bye" {
		l.stopWorkspaceWatcher()
		return
	}
	handle, _, ok := rhunext.ParseWorkspaceWatch(ev)
	if !ok {
		return
	}
	l.mu.Lock()
	w := l.watcher
	l.mu.Unlock()
	if w != nil && handle == w.handle && !w.closed() {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}
func (l *lsp) stopWorkspaceWatcher() {
	l.mu.Lock()
	w := l.watcher
	l.watcher = nil
	l.mu.Unlock()
	if w != nil {
		w.once.Do(func() { close(w.done) })
	}
	// Host retirement removes subscriptions. Avoid blocking a bye callback on a
	// request to a connection which has already begun stopping.
}

func pathInWatchRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
func (s *server) parseWatchedFiles(options map[string]json.RawMessage) ([]watchedFileRule, error) {
	if !s.l.workspaceWatchAvailable() {
		return nil, errors.New("workspace watching is unavailable")
	}
	var entries []json.RawMessage
	raw := options["watchers"]
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > 1024 {
		return nil, errors.New("invalid watchers")
	}
	rules := make([]watchedFileRule, 0, len(entries))
	for _, entry := range entries {
		obj, err := jsonObject(entry)
		if err != nil {
			return nil, err
		}
		for key := range obj {
			if key != "globPattern" && key != "kind" {
				return nil, errors.New("unknown watcher field")
			}
		}
		pattern := obj["globPattern"]
		filterRaw, _ := json.Marshal([]map[string]json.RawMessage{{"pattern": pattern}})
		filters, err := parseSelector(filterRaw)
		if err != nil || len(filters) != 1 {
			return nil, errors.New("invalid watcher globPattern")
		}
		filter := filters[0]
		if filter.base != "" {
			if !pathInWatchRoot(s.l.project, filepath.FromSlash(filter.base)) {
				return nil, errors.New("watcher base is outside workspace")
			}
		} else if text, err := jsonString(pattern); err == nil && !filepath.IsAbs(filepath.FromSlash(text)) {
			filter.base = filepath.ToSlash(filepath.Clean(s.l.project))
		}
		kind := 7
		if raw, exists := obj["kind"]; exists {
			if string(raw) == "null" || json.Unmarshal(raw, &kind) != nil || kind < 1 || kind > 7 {
				return nil, errors.New("invalid watcher kind")
			}
		}
		rules = append(rules, watchedFileRule{filter, kind})
	}
	return rules, nil
}
func (s *server) publishWatchedChanges(changes []watchedChange, runtime *workspaceWatcher) error {
	s.capMu.RLock()
	generation := s.watchGeneration
	var events []map[string]any
	for _, change := range changes {
		matched := false
		for _, registration := range s.registrations {
			if registration.method != watchedFilesMethod {
				continue
			}
			for _, rule := range registration.watchers {
				if rule.kind&(1<<(change.kind-1)) != 0 && (capabilityRegistration{selector: []documentFilter{rule.filter}}).matches(selectorDocument{scheme: "file", path: filepath.ToSlash(change.path)}) {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if matched {
			events = append(events, map[string]any{"uri": fileURI(change.path), "type": change.kind})
		}
	}
	s.capMu.RUnlock()
	if len(events) == 0 || s.ended.Load() {
		return nil
	}
	if !s.ready.Load() {
		return errors.New("language server is not ready for watched-file notifications")
	}
	guard := func() error {
		if runtime != nil {
			runtime.l.mu.Lock()
			active := runtime.l.watcher == runtime && !runtime.l.stopping
			runtime.l.mu.Unlock()
			if !active || runtime.closed() {
				return errors.New("watch runtime retired")
			}
		}
		s.capMu.RLock()
		defer s.capMu.RUnlock()
		if s.ended.Load() || s.watchGeneration != generation {
			return errors.New("watch registration retired")
		}
		return nil
	}
	// Keep each frame within the existing bounded transport. Large diffs split at
	// a fixed count, and every frame rechecks registration generation.
	for at := 0; at < len(events); at += 256 {
		end := at + 256
		if end > len(events) {
			end = len(events)
		}
		err := s.writeContextGuard(context.Background(), map[string]any{"jsonrpc": "2.0", "method": watchedFilesMethod, "params": map[string]any{"changes": events[at:end]}}, guard)
		if err != nil {
			if s.ended.Load() {
				return nil
			}
			return err
		}
	}
	return nil
}

// Watched-file additions establish a current baseline before their atomic
// registry commit. Old registrations receive outstanding changes first.
func (s *server) commitWatchRegistry(add bool, commit func() (any, int, string)) (any, int, string) {
	s.l.mu.Lock()
	w := s.l.watcher
	s.l.mu.Unlock()
	if w == nil || w.closed() {
		return nil, -32602, "workspace watching is unavailable"
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if add {
		if err := w.reconcileLocked(); err != nil {
			return nil, -32603, fmt.Sprintf("workspace watch baseline: %v", err)
		}
	}
	if w.closed() {
		return nil, -32600, "watch runtime retired"
	}
	return commit()
}
