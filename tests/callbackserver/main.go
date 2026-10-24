package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func send(value any) {
	data, _ := json.Marshal(value)
	fmt.Printf("Content-Length: %d\r\n\r\n%s", len(data), data)
}
func main() {
	state := os.Getenv("RHUN_STATE")
	trace, err := os.OpenFile(filepath.Join(state, "callbacks.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		panic(err)
	}
	defer trace.Close()
	reader := bufio.NewReader(os.Stdin)
	pushes := 0
	dynamic := false
	definitionAliases := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var length int
		fmt.Sscanf(line, "Content-Length: %d", &length)
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(reader, data); err != nil {
			return
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		trace.Write(append(data, '\n'))
		method, _ := message["method"].(string)
		switch method {
		case "initialize":
			params, _ := message["params"].(map[string]any)
			options, _ := params["initializationOptions"].(map[string]any)
			dynamic, _ = options["dynamicCapabilities"].(bool)
			definitionAliases, _ = options["definitionAliases"].(bool)
			var syncOptions any = 2
			if kind, ok := options["syncKind"].(float64); ok {
				openClose, _ := options["syncOpenClose"].(bool)
				save, _ := options["syncSave"].(bool)
				syncOptions = map[string]any{"openClose": openClose, "change": kind, "save": save}
				if save {
					syncOptions.(map[string]any)["save"] = map[string]any{"includeText": true}
				}
			}
			if delay, ok := options["initializeDelayMs"].(float64); ok {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
			caps := map[string]any{"textDocumentSync": syncOptions}
			if definitionAliases {
				caps["definitionProvider"] = true
			}
			send(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{"capabilities": caps}})
		case "initialized":
			if dynamic {
				send(map[string]any{"jsonrpc": "2.0", "id": "dynamic-register", "method": "client/registerCapability", "params": map[string]any{"registrations": []any{
					map[string]any{"id": "go-definition", "method": "textDocument/definition", "registerOptions": map[string]any{"documentSelector": []any{map[string]any{"language": "go", "pattern": "**/*.go"}}}},
					map[string]any{"id": "rust-definition", "method": "textDocument/definition", "registerOptions": map[string]any{"documentSelector": []any{map[string]any{"language": "rust"}}}},
				}}})
			}
			for _, request := range []struct {
				id, method string
				params     any
			}{
				{"config", "workspace/configuration", map[string]any{"items": []any{map[string]any{"section": "gopls"}, map[string]any{"section": "missing"}}}},
				{"folders", "workspace/workspaceFolders", nil},
				{"progress", "window/workDoneProgress/create", map[string]any{"token": "index"}},
				{"unknown", "server/unsupported", nil},
				{"registration", "client/futureCapability", map[string]any{"registrations": []any{}}},
			} {
				send(map[string]any{"jsonrpc": "2.0", "id": request.id, "method": request.method, "params": request.params})
			}
		case "workspace/didChangeConfiguration":
			pushes++
			if pushes == 1 {
				params, _ := message["params"].(map[string]any)
				settings, _ := params["settings"].(map[string]any)
				if settings["invalidRestart"] == true {
					os.WriteFile(filepath.Join(state, "lsp.ini"), []byte("[servers]\nGo =\n"), 0600)
				} else {
					os.WriteFile(filepath.Join(state, "settings.json"), []byte(`{"defaults":{"settings":{"gopls":{"mode":"new"}},"initializationOptions":{"from":"reloaded"}}}`), 0600)
				}
			}
		case "window/workDoneProgress/cancel":
			send(map[string]any{"jsonrpc": "2.0", "method": "$/progress", "params": map[string]any{"token": "index", "value": map[string]any{"kind": "end"}}})
		case "textDocument/definition":
			if definitionAliases {
				params := message["params"].(map[string]any)
				uri := params["textDocument"].(map[string]any)["uri"].(string)
				alias := strings.Replace(uri, "/main.go", "//main.go", 1)
				rg := map[string]any{"start": map[string]any{"line": 0, "character": 0}, "end": map[string]any{"line": 0, "character": 7}}
				link := func(uri string) any {
					return map[string]any{"targetUri": uri, "targetRange": rg, "targetSelectionRange": rg}
				}
				if token, ok := params["partialResultToken"]; ok {
					send(map[string]any{"jsonrpc": "2.0", "method": "$/progress", "params": map[string]any{"token": token, "value": []any{link(alias)}}})
				}
				send(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": []any{link(uri), link(alias)}})
				continue
			}
			send(map[string]any{"jsonrpc": "2.0", "id": "dynamic-unregister", "method": "client/unregisterCapability", "params": map[string]any{"unregisterations": []any{map[string]any{"id": "go-definition", "method": "textDocument/definition"}}}})
			send(map[string]any{"jsonrpc": "2.0", "id": message["id"], "result": []any{}})
		case "exit":
			return
		case "":
			if message["id"] == "progress" {
				send(map[string]any{"jsonrpc": "2.0", "method": "$/progress", "params": map[string]any{"token": "index", "value": map[string]any{"kind": "begin", "title": "Indexing", "message": "files", "percentage": 40, "cancellable": true}}})
			}
		}
	}
}
