package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func watchHarness(t *testing.T) (*lsp, *server, *recordingPipe, *workspaceWatcher) {
	t.Helper()
	l, s, p := testServer("Go")
	l.project = t.TempDir()
	l.servers = map[string]*server{"Go": s}
	w := &workspaceWatcher{l: l, root: l.project, wake: make(chan struct{}, 1), done: make(chan struct{}), scan: scanWatchedWorkspace}
	var err error
	w.baseline, err = w.scan(w.root, w.done)
	if err != nil {
		t.Fatal(err)
	}
	l.watcher = w
	s.ready.Store(true)
	t.Cleanup(func() { l.stopWorkspaceWatcher(); s.stop() })
	return l, s, p, w
}
func watchRegister(s *server, id string, options any) (int, string) {
	raw, _ := json.Marshal(map[string]any{"registrations": []any{map[string]any{"id": id, "method": watchedFilesMethod, "registerOptions": options}}})
	_, code, message := s.registerCapabilities(raw)
	return code, message
}
func watcherRules(pattern any, kind int) any {
	return map[string]any{"watchers": []any{map[string]any{"globPattern": pattern, "kind": kind}}}
}
func reconcileWatch(t *testing.T, w *workspaceWatcher) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.reconcileLocked(); err != nil {
		t.Fatal(err)
	}
}
func writeWatchFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}
func watchChanges(t *testing.T, p *recordingPipe) []map[string]any {
	t.Helper()
	var changes []map[string]any
	for _, m := range p.messages(t) {
		if m["method"] == watchedFilesMethod {
			for _, c := range m["params"].(map[string]any)["changes"].([]any) {
				changes = append(changes, c.(map[string]any))
			}
		}
	}
	return changes
}
func TestWatchedFilesHiddenOverlapAndLifecycle(t *testing.T) {
	l, s, p, w := watchHarness(t)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	if code, msg := watchRegister(s, "overlap", watcherRules("**/*", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	hidden := filepath.Join(l.project, ".hidden.go")
	writeWatchFile(t, hidden, "one")
	reconcileWatch(t, w)
	writeWatchFile(t, hidden, "longer")
	reconcileWatch(t, w)
	if err := os.Remove(hidden); err != nil {
		t.Fatal(err)
	}
	reconcileWatch(t, w)
	changes := watchChanges(t, p)
	if len(changes) != 3 {
		t.Fatal(changes)
	}
	for i, c := range changes {
		if c["type"] != float64(i+1) || c["uri"] != fileURI(hidden) {
			t.Fatal(changes)
		}
	}
	if code := unregisterForTest(s, "go", watchedFilesMethod); code != 0 {
		t.Fatal(code)
	}
	if code := unregisterForTest(s, "overlap", watchedFilesMethod); code != 0 {
		t.Fatal(code)
	}
	writeWatchFile(t, hidden, "later")
	reconcileWatch(t, w)
	if len(watchChanges(t, p)) != 3 {
		t.Fatal("delivered after unregister")
	}
}
func TestWatchedFilesRegistrationBaselineIsCurrent(t *testing.T) {
	l, s, p, w := watchHarness(t)
	writeWatchFile(t, filepath.Join(l.project, "before.go"), "existing")
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	reconcileWatch(t, w)
	if len(watchChanges(t, p)) != 0 {
		t.Fatal("existing file reported as new")
	}
	writeWatchFile(t, filepath.Join(l.project, "after.go"), "new")
	reconcileWatch(t, w)
	if len(watchChanges(t, p)) != 1 {
		t.Fatal(watchChanges(t, p))
	}
}
func TestWatchedFilesRelativeBaseAndKinds(t *testing.T) {
	l, s, p, w := watchHarness(t)
	sub := filepath.Join(l.project, "sub")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	pattern := map[string]any{"baseUri": fileURI(sub), "pattern": "*.go"}
	if code, msg := watchRegister(s, "sub", watcherRules(pattern, 1)); code != 0 {
		t.Fatal(code, msg)
	}
	writeWatchFile(t, filepath.Join(l.project, "root.go"), "root")
	target := filepath.Join(sub, "child.go")
	writeWatchFile(t, target, "first")
	reconcileWatch(t, w)
	writeWatchFile(t, target, "changed")
	reconcileWatch(t, w)
	changes := watchChanges(t, p)
	if len(changes) != 1 || changes[0]["uri"] != fileURI(target) {
		t.Fatal(changes)
	}
	for _, bad := range []any{watcherRules(map[string]any{"baseUri": fileURI(t.TempDir()), "pattern": "**"}, 7), watcherRules("**", 0), watcherRules("**", 8), watcherRules("[", 7), map[string]any{"watchers": nil}} {
		if code, _ := watchRegister(s, "bad", bad); code != -32602 {
			t.Fatal(code, bad)
		}
	}
}
func TestWatchedSnapshotDoesNotFollowLinksAndPreservesOnFailure(t *testing.T) {
	l, s, p, w := watchHarness(t)
	outside := t.TempDir()
	writeWatchFile(t, filepath.Join(outside, "secret.go"), "secret")
	if err := os.Symlink(outside, filepath.Join(l.project, "outside")); err == nil {
		snapshot, err := scanWatchedWorkspace(l.project, w.done)
		if err != nil {
			t.Fatal(err)
		}
		for path := range snapshot {
			if strings.Contains(path, "secret.go") {
				t.Fatal("followed directory link", path)
			}
		}
	}
	if code, msg := watchRegister(s, "all", watcherRules("**", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	baseline := w.baseline
	w.scan = func(string, <-chan struct{}) (watchedSnapshot, error) { return nil, os.ErrPermission }
	w.mu.Lock()
	err := w.reconcileLocked()
	w.mu.Unlock()
	if err == nil || len(w.baseline) != len(baseline) || len(watchChanges(t, p)) != 0 {
		t.Fatal(err, w.baseline, watchChanges(t, p))
	}
	if code, _ := watchRegister(s, "rejected", watcherRules("**", 7)); code != -32603 {
		t.Fatal(code)
	}
	s.capMu.RLock()
	_, exists := s.registrations["rejected"]
	s.capMu.RUnlock()
	if exists {
		t.Fatal("failed baseline committed")
	}
}
func TestWatchedRegistrationWaitsForInFlightScan(t *testing.T) {
	l, s, p, w := watchHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	original := w.scan
	var once sync.Once
	w.scan = func(path string, done <-chan struct{}) (watchedSnapshot, error) {
		once.Do(func() { close(entered); <-release })
		return original(path, done)
	}
	scanDone := make(chan struct{})
	go func() { w.mu.Lock(); w.reconcileLocked(); w.mu.Unlock(); close(scanDone) }()
	<-entered
	writeWatchFile(t, filepath.Join(l.project, "before.go"), "before")
	registered := make(chan int, 1)
	go func() { code, _ := watchRegister(s, "go", watcherRules("**/*.go", 7)); registered <- code }()
	select {
	case code := <-registered:
		t.Fatal("registration overtook scan", code)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-scanDone
	if code := <-registered; code != 0 {
		t.Fatal(code)
	}
	reconcileWatch(t, w)
	if len(watchChanges(t, p)) != 0 {
		t.Fatal("pre-registration event leaked")
	}
}
func TestRetiredWatchScanCannotPublish(t *testing.T) {
	l, s, p, w := watchHarness(t)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	original := w.scan
	entered, release := make(chan struct{}), make(chan struct{})
	w.scan = func(path string, done <-chan struct{}) (watchedSnapshot, error) {
		close(entered)
		<-release
		return original(path, make(chan struct{}))
	}
	writeWatchFile(t, filepath.Join(l.project, "new.go"), "new")
	finished := make(chan error, 1)
	go func() { w.mu.Lock(); defer w.mu.Unlock(); finished <- w.reconcileLocked() }()
	<-entered
	l.workspaceWatchEvent(rhunext.Event{Name: "project", Args: []string{t.TempDir()}})
	close(release)
	if err := <-finished; err == nil {
		t.Fatal("retired scan succeeded")
	}
	if len(watchChanges(t, p)) != 0 || len(w.baseline) != 0 {
		t.Fatal("retired scan published")
	}
}
func TestWatchedQueuedFrameRejectsRetiredRuntime(t *testing.T) {
	l, s, p, w := watchHarness(t)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	s.writeGate <- struct{}{}
	finished := make(chan struct{})
	go func() {
		s.publishWatchedChanges([]watchedChange{{filepath.Join(l.project, "new.go"), 1}}, w)
		close(finished)
	}()
	l.stopWorkspaceWatcher()
	<-s.writeGate
	<-finished
	if len(watchChanges(t, p)) != 0 {
		t.Fatal("retired runtime frame sent")
	}
}
func TestWatchSnapshotEntryLimit(t *testing.T) {
	root := t.TempDir()
	// Exercise the independent directory limit with a bounded deep tree.
	current := root
	for i := 0; i < maxWatchDirectories; i++ {
		current = filepath.Join(current, "d")
		if err := os.Mkdir(current, 0700); err != nil {
			t.Skip(err)
		}
	}
	if _, err := scanWatchedWorkspace(root, make(chan struct{})); err == nil {
		t.Fatal("accepted directory overflow")
	}
}

func TestWatchedUnregisterOrdersAfterInFlightScan(t *testing.T) {
	l, s, p, w := watchHarness(t)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	original := w.scan
	var once sync.Once
	w.scan = func(path string, done <-chan struct{}) (watchedSnapshot, error) {
		once.Do(func() { close(entered); <-release })
		return original(path, done)
	}
	writeWatchFile(t, filepath.Join(l.project, "before.go"), "before")
	scanDone := make(chan struct{})
	go func() { w.mu.Lock(); w.reconcileLocked(); w.mu.Unlock(); close(scanDone) }()
	<-entered
	unregistered := make(chan int, 1)
	go func() { unregistered <- unregisterForTest(s, "go", watchedFilesMethod) }()
	select {
	case code := <-unregistered:
		t.Fatal("unregistration overtook scan", code)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-scanDone
	if code := <-unregistered; code != 0 {
		t.Fatal(code)
	}
	if len(watchChanges(t, p)) != 1 {
		t.Fatal(watchChanges(t, p))
	}
	writeWatchFile(t, filepath.Join(l.project, "after.go"), "after")
	reconcileWatch(t, w)
	if len(watchChanges(t, p)) != 1 {
		t.Fatal("event followed unregistration acknowledgement")
	}
}

func TestWatchedFilesRetryAnUnsentDiff(t *testing.T) {
	l, s, p, w := watchHarness(t)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	writeWatchFile(t, filepath.Join(l.project, "lost.go"), "new")
	s.writeGate <- struct{}{}
	w.mu.Lock()
	err := w.reconcileLocked()
	w.mu.Unlock()
	<-s.writeGate
	if err == nil || s.ended.Load() || len(w.baseline) != 0 {
		t.Fatal("unsent diff advanced baseline", err, s.ended.Load(), w.baseline)
	}
	reconcileWatch(t, w)
	changes := watchChanges(t, p)
	if len(changes) != 1 || changes[0]["uri"] != fileURI(filepath.Join(l.project, "lost.go")) {
		t.Fatal("unsent diff was lost", changes)
	}
}

func TestWatchedFilesRetainDiffDuringDynamicRegistrationStartup(t *testing.T) {
	l, s, p, w := watchHarness(t)
	// A server can register after initialized, while publishReady is still
	// sending configuration and the bridge's internal ready flag remains false.
	s.ready.Store(false)
	if code, msg := watchRegister(s, "go", watcherRules("**/*.go", 7)); code != 0 {
		t.Fatal(code, msg)
	}
	writeWatchFile(t, filepath.Join(l.project, "during-startup.go"), "new")
	w.mu.Lock()
	err := w.reconcileLocked()
	w.mu.Unlock()
	if err == nil || len(w.baseline) != 0 {
		t.Fatal("startup discarded an unpublished diff", err, w.baseline)
	}
	s.ready.Store(true)
	reconcileWatch(t, w)
	changes := watchChanges(t, p)
	if len(changes) != 1 || changes[0]["uri"] != fileURI(filepath.Join(l.project, "during-startup.go")) {
		t.Fatal(changes)
	}
}
