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

type pos struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type span struct {
	Start pos `json:"start"`
	End   pos `json:"end"`
}
type doc struct {
	text    string
	version int
}

func position(text string, off int) pos {
	before := text[:off]
	line := strings.Count(before, "\n")
	start := strings.LastIndex(before, "\n") + 1
	return pos{line, off - start}
}
func offset(text string, p pos) int {
	start := 0
	for n := 0; n < p.Line; n++ {
		i := strings.IndexByte(text[start:], '\n')
		if i < 0 {
			return len(text)
		}
		start += i + 1
	}
	if start+p.Character > len(text) {
		return len(text)
	}
	return start + p.Character
}
func edit(at pos, text string) map[string]any {
	return map[string]any{"range": span{at, at}, "newText": text}
}
func changes(uri string, edits ...map[string]any) map[string]any {
	return map[string]any{"changes": map[string]any{uri: edits}}
}
func send(value any) {
	body, _ := json.Marshal(value)
	fmt.Printf("Content-Length: %d\r\n\r\n%s", len(body), body)
}
func reply(id any, value any) { send(map[string]any{"jsonrpc": "2.0", "id": id, "result": value}) }
func main() {
	trace, _ := os.OpenFile(filepath.Join(os.Getenv("RHUN_STATE"), "callbacks.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	defer trace.Close()
	reader := bufio.NewReader(os.Stdin)
	docs := map[string]*doc{}
	mode := ""
	uri := ""
	var commandID any
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var size int
		fmt.Sscanf(line, "Content-Length: %d", &size)
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "" {
				break
			}
		}
		body := make([]byte, size)
		if _, err = io.ReadFull(reader, body); err != nil {
			return
		}
		var msg map[string]any
		if json.Unmarshal(body, &msg) != nil {
			continue
		}
		record := map[string]any{}
		for k, v := range msg {
			record[k] = v
		}
		record["serverPID"] = os.Getpid()
		encoded, _ := json.Marshal(record)
		trace.Write(append(encoded, '\n'))
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		switch method {
		case "initialize":
			opts, _ := params["initializationOptions"].(map[string]any)
			mode, _ = opts["sourceMode"].(string)
			reply(msg["id"], map[string]any{"capabilities": map[string]any{"textDocumentSync": 2, "codeActionProvider": map[string]any{"resolveProvider": mode != "unsupported-resolve" && mode != "inline-foreign-delayed" && mode != "inline-edit-command"}, "documentFormattingProvider": true}})
		case "textDocument/didOpen":
			td := params["textDocument"].(map[string]any)
			uri = td["uri"].(string)
			docs[uri] = &doc{text: td["text"].(string), version: int(td["version"].(float64))}
		case "textDocument/didChange":
			td := params["textDocument"].(map[string]any)
			uri = td["uri"].(string)
			d := docs[uri]
			if d == nil {
				d = &doc{}
				docs[uri] = d
			}
			for _, raw := range params["contentChanges"].([]any) {
				change := raw.(map[string]any)
				text := change["text"].(string)
				if r, ok := change["range"]; ok {
					raw, _ := json.Marshal(r)
					var span span
					json.Unmarshal(raw, &span)
					a, z := offset(d.text, span.Start), offset(d.text, span.End)
					d.text = d.text[:a] + text + d.text[z:]
				} else {
					d.text = text
				}
			}
			d.version = int(td["version"].(float64))
		case "textDocument/codeAction":
			uri = params["textDocument"].(map[string]any)["uri"].(string)
			d := docs[uri]
			only := "source.fixAll"
			kinds, asked := params["context"].(map[string]any)["only"].([]any)
			if asked && len(kinds) > 0 {
				only = kinds[0].(string)
			}
			var actions []any
			makeAction := func(title, kind string, value any) map[string]any {
				return map[string]any{"title": title, "kind": kind, "edit": value}
			}
			if only == "source.organizeImports" {
				if !strings.Contains(d.text, "[O]") {
					actions = append(actions, makeAction("Organize", "source.organizeImports", changes(uri, edit(pos{}, "[O]"))))
				}
			} else {
				switch mode {
				case "resolve-initial-foreign-delayed", "inline-foreign-delayed":
					foreign := strings.Replace(uri, "main.go", "other.go", 1)
					action := makeAction("Resolve", "source.fixAll", changes(foreign, map[string]any{"range": map[string]any{"start": pos{}, "end": pos{0, 5}}, "newText": "[R]"}))
					actions = append(actions, action)
					time.Sleep(600 * time.Millisecond)
				case "resolve", "resolve-no-data", "resolve-null-data", "resolve-title-only", "resolve-delayed", "resolve-foreign-delayed", "resolve-foreign-replace-delayed", "unsupported-resolve":
					if !strings.Contains(d.text, "[R]") {
						action := map[string]any{"title": "Resolve", "kind": "source.fixAll", "data": map[string]any{"uri": uri}}
						if mode == "resolve-no-data" || mode == "resolve-title-only" {
							delete(action, "data")
						}
						if mode == "resolve-null-data" {
							action["data"] = nil
						}
						if mode == "resolve-title-only" {
							delete(action, "kind")
						}
						actions = append(actions, action)
					}
				case "inline-edit-command", "command", "command-two", "command-foreign-then-valid", "late-command", "cancel-command", "explicit-stale-command", "command-no-server-two", "command-foreign-server-two", "duplicate-command":
					if !strings.Contains(d.text, "[C]") && !strings.Contains(d.text, "[1]") {
						a := map[string]any{"title": "Command", "kind": "source.fixAll", "command": map[string]any{"command": "source.command", "arguments": []any{uri}}}
						if mode == "inline-edit-command" {
							a["edit"] = changes(uri, edit(pos{}, "[I]"))
						}
						actions = append(actions, a)
						if mode == "duplicate-command" {
							actions = append(actions, a)
						}
					}
				case "stale-version":
					actions = append(actions, makeAction("Stale", "source.fixAll", map[string]any{"documentChanges": []any{map[string]any{"textDocument": map[string]any{"uri": uri, "version": d.version + 1}, "edits": []any{edit(pos{}, "[S]")}}}}))
				case "foreign":
					foreign := strings.Replace(uri, "main.go", "other.go", 1)
					actions = append(actions, makeAction("Foreign", "source.fixAll", map[string]any{"changes": map[string]any{uri: []any{edit(pos{}, "[S]")}, foreign: []any{edit(pos{}, "[X]")}}}))
				default:
					if mode == "delayed" {
						time.Sleep(600 * time.Millisecond)
					}
					if !strings.Contains(d.text, "[A]") {
						actions = append(actions, makeAction("First", "source.fixAll", changes(uri, edit(pos{}, "[A]"))))
					}
					if !strings.Contains(d.text, "[B]") {
						title := "Second"
						if mode == "duplicate-edit" {
							title = "First"
						}
						actions = append(actions, makeAction(title, "source.fixAll", changes(uri, edit(position(d.text, len(d.text)), "[B]"))))
					}
				}
			}
			actions = append(actions, map[string]any{"title": "Disabled", "kind": "source.fixAll", "disabled": map[string]any{"reason": "no"}})
			if asked {
				// a kind the save did not ask for (rhun lists refactors before source actions, so
				// the interactive list leaves it out)
				actions = append(actions, makeAction("Wrong kind", "refactor", changes(uri, edit(pos{}, "[BAD]"))))
			}
			reply(msg["id"], actions)
		case "codeAction/resolve":
			if mode == "resolve-initial-foreign-delayed" {
				reply(msg["id"], params)
			} else if strings.HasPrefix(mode, "resolve") {
				if strings.Contains(mode, "delayed") {
					time.Sleep(600 * time.Millisecond)
				}
				target := uri
				if mode == "resolve-foreign-delayed" || mode == "resolve-foreign-replace-delayed" {
					target = strings.Replace(uri, "main.go", "other.go", 1)
				}
				action := map[string]any{"title": "Resolve", "edit": changes(target, edit(pos{}, "[R]"))}
				if mode == "resolve-foreign-replace-delayed" {
					action["edit"] = changes(target, map[string]any{"range": map[string]any{"start": pos{}, "end": pos{Line: 0, Character: 5}}, "newText": "[R]"})
				}
				if mode == "resolve-title-only" {
					action["kind"] = "source.fixAll"
				}
				reply(msg["id"], action)
			} else {
				reply(msg["id"], params)
			}
		case "workspace/executeCommand":
			commandID = msg["id"]
			uri = params["arguments"].([]any)[0].(string)
			if mode == "cancel-command" || mode == "explicit-stale-command" {
				time.Sleep(700 * time.Millisecond)
			}
			if mode == "late-command" {
				reply(commandID, nil)
			}
			marker := "[C]"
			if mode == "command-two" || mode == "command-foreign-then-valid" || (mode == "command-no-server-two" || mode == "command-foreign-server-two") {
				marker = "[1]"
			}
			target := uri
			if mode == "command-no-server-two" || mode == "command-foreign-server-two" {
				name := "go.mod"
				if mode == "command-foreign-server-two" {
					name = "other.rs"
				}
				uri = strings.Replace(uri, "main.go", name, 1)
				target = uri
			}
			if mode == "command-foreign-then-valid" {
				target = strings.Replace(uri, "main.go", "other.go", 1)
			}
			first := edit(pos{}, marker)
			if mode == "explicit-stale-command" {
				first = map[string]any{"range": map[string]any{"start": pos{}, "end": pos{0, 5}}, "newText": marker}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": "apply1", "method": "workspace/applyEdit", "params": map[string]any{"edit": changes(target, first)}})
			if mode != "command-two" && mode != "command-foreign-then-valid" && mode != "command-no-server-two" && mode != "command-foreign-server-two" && mode != "late-command" {
				reply(commandID, nil)
			}
		case "textDocument/formatting":
			reply(msg["id"], []any{edit(pos{}, "F")})
		case "":
			if (mode == "command-two" || mode == "command-foreign-then-valid" || (mode == "command-no-server-two" || mode == "command-foreign-server-two")) && msg["id"] == "apply1" {
				at := pos{}
				if mode != "command-no-server-two" && mode != "command-foreign-server-two" {
					at = position(docs[uri].text, len(docs[uri].text))
				}
				body := changes(uri, edit(at, "[2]"))
				if mode == "command-foreign-server-two" {
					body = map[string]any{"documentChanges": []any{map[string]any{"textDocument": map[string]any{"uri": uri, "version": 2}, "edits": []any{edit(pos{}, "[2]")}}}}
				}
				send(map[string]any{"jsonrpc": "2.0", "id": "apply2", "method": "workspace/applyEdit", "params": map[string]any{"edit": body}})
			} else if (mode == "command-two" || mode == "command-foreign-then-valid" || (mode == "command-no-server-two" || mode == "command-foreign-server-two")) && msg["id"] == "apply2" {
				reply(commandID, nil)
			}
		case "exit":
			return
		}
	}
}
