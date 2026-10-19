// Native bridge fixture: request dynamic sync and watched-file registrations,
// unregister after the first externally created hidden file is reported.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func send(message any) {
	data, _ := json.Marshal(message)
	fmt.Printf("Content-Length: %d\r\n\r\n%s", len(data), data)
}
func main() {
	log, err := os.Create(filepath.Join(os.Getenv("RHUN_STATE"), "callbacks.jsonl"))
	if err != nil {
		panic(err)
	}
	defer log.Close()
	reader := bufio.NewReader(os.Stdin)
	removed := false
	for {
		length := -1
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, "Content-Length") {
				length, _ = strconv.Atoi(strings.TrimSpace(value))
			}
		}
		if length < 0 || length > 16<<20 {
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		log.Write(append(body, '\n'))
		log.Sync()
		var message map[string]json.RawMessage
		if json.Unmarshal(body, &message) != nil {
			return
		}
		var method string
		json.Unmarshal(message["method"], &method)
		switch method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(message["id"]), "result": map[string]any{"capabilities": map[string]any{"textDocumentSync": 0}}})
		case "initialized":
			selector := []any{map[string]any{"language": "go", "scheme": "file"}}
			regs := []any{}
			for _, name := range []string{"didOpen", "didClose", "didChange", "didSave"} {
				options := map[string]any{"documentSelector": selector}
				if name == "didChange" {
					options["syncKind"] = 2
				}
				if name == "didSave" {
					options["includeText"] = true
				}
				regs = append(regs, map[string]any{"id": name, "method": "textDocument/" + name, "registerOptions": options})
			}
			regs = append(regs, map[string]any{"id": "files", "method": "workspace/didChangeWatchedFiles", "registerOptions": map[string]any{"watchers": []any{map[string]any{"globPattern": "**/*.watch", "kind": 7}}}})
			send(map[string]any{"jsonrpc": "2.0", "id": "watch-add", "method": "client/registerCapability", "params": map[string]any{"registrations": regs}})
		case "workspace/didChangeWatchedFiles":
			if !removed {
				removed = true
				send(map[string]any{"jsonrpc": "2.0", "id": "watch-remove", "method": "client/unregisterCapability", "params": map[string]any{"unregisterations": []any{map[string]any{"id": "files", "method": "workspace/didChangeWatchedFiles"}}}})
			}

		}
	}
}
