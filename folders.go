package main

// Workspace folders (feature workspace-folders): rhun names the window's folders ("0 folders",
// before ready and again when they change for a program that keeps running). They are the
// servers' workspaceFolders at initialize (rootUri the primary folder's), the answer to
// workspace/workspaceFolders and the resources workspace/configuration may name; a running
// server that asked for changes (changeNotifications, or a registration) gets
// workspace/didChangeWorkspaceFolders with the folders added and removed. Without the feature
// the project folder is the one folder.

import (
	"encoding/json"
	"path/filepath"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

const didChangeFoldersMethod = "workspace/didChangeWorkspaceFolders"

// workspaceFolder is a folder rhun named, as the servers see it
type workspaceFolder struct {
	path, name string
	primary    bool
}

// lspFolders are folders as LSP's WorkspaceFolder[]
func lspFolders(folders []workspaceFolder) []map[string]any {
	out := make([]map[string]any, 0, len(folders))
	for _, f := range folders {
		out = append(out, map[string]any{"uri": fileURI(f.path), "name": f.name})
	}
	return out
}

// folderChanges are the folders of next that old lacks and those of old that next lacks
func folderChanges(old, next []workspaceFolder) (added, removed []workspaceFolder) {
	has := func(list []workspaceFolder, path string) bool {
		for _, f := range list {
			if filepath.Clean(f.path) == filepath.Clean(path) {
				return true
			}
		}
		return false
	}
	for _, f := range next {
		if !has(old, f.path) {
			added = append(added, f)
		}
	}
	for _, f := range old {
		if !has(next, f.path) {
			removed = append(removed, f)
		}
	}
	return added, removed
}

// folderEvent takes "0 folders" (and reports whether the event was one): the folders a server
// can reach (local, absolute); servers already running hear of the change
func (l *lsp) folderEvent(ev rhunext.Event) bool {
	if ev.Name != "folders" {
		return false
	}
	list, ok := rhunext.ParseFolders(ev)
	if !ok {
		return true
	}
	next := []workspaceFolder{}
	for _, f := range list {
		if f.Location == "local" && filepath.IsAbs(f.Path) {
			next = append(next, workspaceFolder{path: f.Path, name: f.Name, primary: f.Flags&1 != 0})
		}
	}
	l.mu.Lock()
	l.folders, l.foldersKnown = next, true
	var running []*server
	for _, s := range l.servers {
		if s != nil {
			running = append(running, s)
		}
	}
	l.mu.Unlock()
	for _, s := range running {
		s.syncFolders()
	}
	return true
}

// syncFolders tells a running server the folders that changed since it last heard of them (at
// initialize, or by an earlier notification), when it asked for changes
func (s *server) syncFolders() {
	if !s.ready.Load() || !s.wantsFolderChanges() {
		return
	}
	s.l.mu.Lock()
	next := append([]workspaceFolder(nil), s.l.folders...)
	known := s.l.foldersKnown
	s.l.mu.Unlock()
	s.folderMu.Lock()
	defer s.folderMu.Unlock()
	if !known || !s.foldersSent {
		return
	}
	added, removed := folderChanges(s.folders, next)
	if len(added)+len(removed) == 0 {
		return
	}
	s.folders = next
	s.notify(didChangeFoldersMethod, map[string]any{"event": map[string]any{"added": lspFolders(added), "removed": lspFolders(removed)}})
}

// wantsFolderChanges is whether the server asked for workspace/didChangeWorkspaceFolders: its
// workspace.workspaceFolders.changeNotifications (true or a registration id), or a registration
func (s *server) wantsFolderChanges() bool {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	if s.staticFolderChanges {
		return true
	}
	for _, r := range s.registrations {
		if r.method == didChangeFoldersMethod {
			return true
		}
	}
	return false
}

// loadFolderCapabilities reads workspace.workspaceFolders.changeNotifications
func (s *server) loadFolderCapabilities(caps map[string]json.RawMessage) {
	changes := false
	if workspace, err := jsonObject(caps["workspace"]); err == nil {
		if folders, err := jsonObject(workspace["workspaceFolders"]); err == nil {
			raw := folders["changeNotifications"]
			id, err := jsonString(raw)
			changes = string(raw) == "true" || err == nil && id != ""
		}
	}
	s.capMu.Lock()
	s.staticFolderChanges = changes
	s.capMu.Unlock()
}

// initializeFolders are a starting server's workspaceFolders and rootUri: rhun's folders (the
// root the primary one), else the project folder. Called with l.mu held.
func (l *lsp) initializeFolders() (folders []workspaceFolder, known bool, root any) {
	if l.foldersKnown {
		folders = append([]workspaceFolder(nil), l.folders...)
		for _, f := range folders {
			if f.primary {
				return folders, true, fileURI(f.path)
			}
		}
		if len(folders) > 0 {
			return folders, true, fileURI(folders[0].path)
		}
		return folders, true, nil
	}
	if l.project != "" && l.project != "-" {
		root = fileURI(l.project)
	}
	return nil, false, root
}
