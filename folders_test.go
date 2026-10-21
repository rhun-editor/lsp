package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// foldersEvent is "0 folders LEN" and its body for (id, location, flags, name, path) records
func foldersEvent(records ...[5]string) string {
	body := fmt.Sprintf("%d\n", len(records))
	for _, r := range records {
		body += fmt.Sprintf("%s %s %s %d %d\n%s%s", r[0], r[1], r[2], len(r[3]), len(r[4]), r[3], r[4])
	}
	return fmt.Sprintf("0 folders %d\n%s", len(body), body)
}

func TestWorkspaceFoldersAtInitializeAndWhenTheyChange(t *testing.T) {
	features := append(allFeatures, rhunext.FeatureWorkspaceFolders)
	h := newHarness(t, features, `{"workspace":{"workspaceFolders":{"supported":true,"changeNotifications":"folders-id"}}}`, sample, nil)
	h.host.write(foldersEvent([5]string{"1", "local", "3", "app", "/work/app"}, [5]string{"2", "local", "2", "lib", "/work/lib"},
		[5]string{"3", "remote", "2", "far", "/far"}))
	eventually(t, func() bool { h.l.mu.Lock(); defer h.l.mu.Unlock(); return h.l.foldersKnown })
	h.l.mu.Lock()
	initial, known, root := h.l.initializeFolders()
	folders, _ := json.Marshal(h.l.workspaceFolders())
	inApp, inLib, outside := h.l.configurationScope("file:///work/app/a.go"), h.l.configurationScope("file:///work/lib/b.go"), h.l.configurationScope("file:///elsewhere/c.go")
	h.l.mu.Unlock()
	if !known || len(initial) != 2 || root != "file:///work/app" {
		t.Fatal(initial, known, root)
	}
	if string(folders) != `[{"name":"app","uri":"file:///work/app"},{"name":"lib","uri":"file:///work/lib"}]` {
		t.Fatal(string(folders))
	}
	if !inApp || !inLib || outside {
		t.Fatal(inApp, inLib, outside)
	}
	if answer, code, _ := h.s.requestResult("workspace/workspaceFolders", nil); code != 0 || len(answer.([]map[string]any)) != 2 {
		t.Fatal(answer, code)
	}
	// the server knows the two folders (as at initialize); lib goes and docs comes
	h.s.folderMu.Lock()
	h.s.folders, h.s.foldersSent = initial, true
	h.s.folderMu.Unlock()
	h.host.write(foldersEvent([5]string{"1", "local", "3", "app", "/work/app"}, [5]string{"4", "local", "2", "docs", "/work/docs"}))
	change := h.srv.await(didChangeFoldersMethod)["params"].(map[string]any)["event"].(map[string]any)
	added, removed := change["added"].([]any), change["removed"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["uri"] != "file:///work/docs" || len(removed) != 1 || removed[0].(map[string]any)["name"] != "lib" {
		t.Fatal(change)
	}
	// the same list again: nothing to say
	h.host.write(foldersEvent([5]string{"1", "local", "3", "app", "/work/app"}, [5]string{"4", "local", "2", "docs", "/work/docs"}))
	h.host.ask("prepare-rename 1 1 1 1 invoked -", nil) // a round trip: the event was handled
	if n := len(h.srv.messages(didChangeFoldersMethod)); n != 1 {
		t.Fatal(n)
	}
}

func TestFolderChangesNeedTheServersInterest(t *testing.T) {
	features := append(allFeatures, rhunext.FeatureWorkspaceFolders)
	h := newHarness(t, features, `{}`, sample, nil)
	if h.s.wantsFolderChanges() {
		t.Fatal("no changeNotifications")
	}
	h.s.folderMu.Lock()
	h.s.folders, h.s.foldersSent = []workspaceFolder{{path: "/work/app", name: "app", primary: true}}, true
	h.s.folderMu.Unlock()
	h.host.write(foldersEvent([5]string{"1", "local", "3", "app", "/work/app"}, [5]string{"2", "local", "2", "lib", "/work/lib"}))
	h.host.ask("prepare-rename 1 1 1 1 invoked -", nil)
	if len(h.srv.messages(didChangeFoldersMethod)) != 0 {
		t.Fatal("told a server that did not ask")
	}
	// it registers for them: the folders that changed since it last heard
	if code := registerForTest(h.s, "f", didChangeFoldersMethod, nil); code != 0 || !h.s.wantsFolderChanges() {
		t.Fatal(code)
	}
	h.host.write(foldersEvent([5]string{"1", "local", "3", "app", "/work/app"}, [5]string{"2", "local", "2", "lib", "/work/lib"},
		[5]string{"5", "local", "2", "docs", "/work/docs"}))
	change := h.srv.await(didChangeFoldersMethod)["params"].(map[string]any)["event"].(map[string]any)
	if got := fmt.Sprint(change["added"]); !strings.Contains(got, "lib") || !strings.Contains(got, "docs") || len(change["removed"].([]any)) != 0 {
		t.Fatal(change)
	}
}

func TestWithoutTheFeatureTheProjectIsTheFolder(t *testing.T) {
	l := &lsp{project: "/work/app"}
	folders, _ := json.Marshal(l.workspaceFolders())
	_, known, root := l.initializeFolders()
	if string(folders) != `[{"name":"app","uri":"file:///work/app"}]` || known || root != "file:///work/app" {
		t.Fatal(string(folders), known, root)
	}
	l.foldersKnown = true
	if l.workspaceFolders() != nil {
		t.Fatal("a window without folders has none")
	}
}
