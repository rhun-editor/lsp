package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const configLimit = 1 << 20

type languageSettings struct {
	Settings              map[string]any  `json:"settings"`
	InitializationOptions json.RawMessage `json:"initializationOptions,omitempty"`
}
type settingsConfig struct {
	Defaults  languageSettings            `json:"defaults"`
	Languages map[string]languageSettings `json:"languages"`
}

func configBytes(path string) ([]byte, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, configLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > configLimit {
		return nil, fmt.Errorf("%s exceeds 1 MiB", filepath.Base(path))
	}
	return data, nil
}

func readSettings(path string) (*settingsConfig, error) {
	data, err := configBytes(path)
	if err != nil {
		return nil, err
	}
	cfg := &settingsConfig{}
	if data == nil {
		return cfg, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("settings.json: expected one JSON object")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil, fmt.Errorf("settings.json must be an object")
	}
	return cfg, nil
}

func mergeSettings(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		if object, ok := value.(map[string]any); ok {
			value = mergeSettings(object, nil)
		}
		out[key] = value
	}
	for key, value := range override {
		if object, ok := value.(map[string]any); ok {
			previous, _ := out[key].(map[string]any)
			value = mergeSettings(previous, object)
		}
		out[key] = value
	}
	return out
}

func (cfg *settingsConfig) forLanguage(language string) languageSettings {
	// These servers expect their editor defaults in workspace/configuration.
	// In particular, an empty JSON configuration disables validation.
	defaults := map[string]any{}
	switch language {
	case "JSON":
		defaults["json"] = map[string]any{"validate": map[string]any{"enable": true}}
	case "CSS":
		defaults["css"] = map[string]any{"validate": true}
	}
	if cfg == nil {
		return languageSettings{Settings: defaults}
	}
	local := cfg.Languages[language]
	options := cfg.Defaults.InitializationOptions
	if local.InitializationOptions != nil {
		options = local.InitializationOptions
	}
	return languageSettings{Settings: mergeSettings(mergeSettings(defaults, cfg.Defaults.Settings), local.Settings), InitializationOptions: options}
}

// Choosing Deno as the server opts this language into Deno analysis. It otherwise
// accepts requests but returns no features unless the client explicitly enables it.
func (cfg *settingsConfig) forServer(language, executable string) languageSettings {
	result := cfg.forLanguage(language)
	if name := filepath.Base(executable); name != "deno" && name != "deno.exe" {
		return result
	}
	result.Settings = mergeSettings(map[string]any{"deno": map[string]any{"enable": true}}, result.Settings)
	var custom map[string]any
	if len(result.InitializationOptions) == 0 || json.Unmarshal(result.InitializationOptions, &custom) == nil {
		result.InitializationOptions, _ = json.Marshal(mergeSettings(map[string]any{"enable": configurationSection(result.Settings, "deno.enable")}, custom))
	}
	return result
}

func readConfig(path string) (map[string][]string, error) {
	data, err := configBytes(path)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	section := ""
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fail := func(message string) (map[string][]string, error) {
			return nil, fmt.Errorf("lsp.ini line %d: %s", n+1, message)
		}
		if strings.HasPrefix(line, "[") {
			if line != "[servers]" {
				return fail("expected [servers]")
			}
			section = "servers"
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if section != "servers" || !found || key == "" {
			return fail("expected LANGUAGE = COMMAND in [servers]")
		}
		if value == "off" {
			out[key] = []string{}
			continue
		}
		var argv []string
		if strings.HasPrefix(value, "[") {
			var elements []json.RawMessage
			if err := json.Unmarshal([]byte(value), &elements); err != nil {
				return fail("command must be a JSON string array")
			}
			for _, element := range elements {
				element = bytes.TrimSpace(element)
				var argument string
				if len(element) == 0 || element[0] != '"' || json.Unmarshal(element, &argument) != nil {
					return fail("every command argument must be a string")
				}
				argv = append(argv, argument)
			}
		} else {
			argv = strings.Fields(value)
		}
		if len(argv) == 0 {
			return fail("command cannot be empty; use off to disable a server")
		}
		for i, arg := range argv {
			if (i == 0 && arg == "") || strings.ContainsAny(arg, "\x00\r\n") {
				return fail("invalid empty or multiline command argument")
			}
		}
		out[key] = argv
	}
	return out, nil
}

func configurationSection(settings map[string]any, section string) any {
	if section == "" {
		return settings
	}
	if exact, ok := settings[section]; ok {
		return exact
	}
	var value any = settings
	for _, part := range strings.Split(section, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[part]
	}
	return value
}

// Called with l.mu held. A resource is in scope when it is in a workspace folder (the project
// folder without workspace-folders) or an open document. Unknown resources never gain a scope.
func (l *lsp) configurationScope(scope string) bool {
	if scope == "" {
		return true
	}
	u, err := url.Parse(scope)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost" && runtime.GOOS != "windows") {
		return false
	}
	path := filepath.ToSlash(filepath.Clean(uriPath(scope)))
	if !filepath.IsAbs(path) {
		return false
	}
	if l.byPath[path] != nil {
		return true
	}
	roots := []string{l.project}
	if l.foldersKnown {
		roots = roots[:0]
		for _, f := range l.folders {
			roots = append(roots, f.path)
		}
	}
	for _, root := range roots {
		if root == "" || root == "-" {
			continue
		}
		relative, err := filepath.Rel(root, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}

// workspaceFolders are LSP's workspaceFolders: rhun's folders (feature workspace-folders), else the
// project folder; null without any. Called with l.mu held.
func (l *lsp) workspaceFolders() any {
	if l.foldersKnown {
		if len(l.folders) == 0 {
			return nil
		}
		return lspFolders(l.folders)
	}
	if l.project == "" || l.project == "-" {
		return nil
	}
	return []map[string]any{{"uri": fileURI(l.project), "name": filepath.Base(l.project)}}
}

// Serialize settings notifications per server. Every caller reads the newest published
// snapshot after acquiring configMu, so concurrent reloads cannot send an older snapshot last.
func (s *server) sendConfiguration() error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	s.l.mu.Lock()
	settings := s.l.settings.forServer(s.lang, s.name()).Settings
	s.l.mu.Unlock()
	return s.write(map[string]any{"jsonrpc": "2.0", "method": "workspace/didChangeConfiguration", "params": map[string]any{"settings": settings}})
}

func (s *server) publishReady() error {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	for {
		s.l.mu.Lock()
		generation := s.l.configGeneration
		settings := s.l.settings.forServer(s.lang, s.name()).Settings
		s.l.mu.Unlock()
		if err := s.write(map[string]any{"jsonrpc": "2.0", "method": "workspace/didChangeConfiguration", "params": map[string]any{"settings": settings}}); err != nil {
			return err
		}
		s.l.mu.Lock()
		if generation != s.l.configGeneration {
			s.l.mu.Unlock()
			continue
		}
		s.stateMu.Lock()
		if s.ended.Load() || s.processExited.Load() {
			s.stateMu.Unlock()
			s.l.mu.Unlock()
			return fmt.Errorf("server exited during initialization")
		}
		s.ready.Store(true)
		s.stateMu.Unlock()
		s.l.mu.Unlock()
		return nil
	}
}

func (l *lsp) reloadSettings() error {
	cfg, err := readSettings(filepath.Join(l.state, "settings.json"))
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.settings = cfg
	l.configGeneration++
	var ready []*server
	for _, s := range l.servers {
		if s != nil && s.ready.Load() {
			ready = append(ready, s)
		}
	}
	l.mu.Unlock()
	for _, s := range ready {
		if err := s.sendConfiguration(); err != nil {
			l.ext.Logf("configuration: %v", err)
		}
	}
	return nil
}

func (s *server) serverRequest(id json.RawMessage, method string, params json.RawMessage) {
	result, code, message := s.requestResult(method, params)
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if code != 0 {
		reply["error"] = map[string]any{"code": code, "message": message}
	} else {
		reply["result"] = result
	}
	if err := s.write(reply); err != nil {
		s.l.ext.Logf("server reply: %v", err)
	}
}

func (s *server) requestResult(method string, params json.RawMessage) (any, int, string) {
	switch method {
	case "client/registerCapability":
		return s.registerCapabilities(params)
	case "client/unregisterCapability":
		return s.unregisterCapabilities(params)
	case "workspace/diagnostic/refresh":
		go s.l.repull(s)
		return nil, 0, ""
	case "workspace/configuration":
		var p struct {
			Items *[]struct {
				ScopeURI string `json:"scopeUri"`
				Section  string `json:"section"`
			} `json:"items"`
		}
		if json.Unmarshal(params, &p) != nil || p.Items == nil || len(*p.Items) > 1024 {
			return nil, -32602, "invalid configuration items"
		}
		s.l.mu.Lock()
		settings := s.l.settings.forServer(s.lang, s.name()).Settings
		values := make([]any, len(*p.Items))
		for i, item := range *p.Items {
			if s.l.configurationScope(item.ScopeURI) {
				values[i] = configurationSection(settings, item.Section)
			}
		}
		s.l.mu.Unlock()
		// Encode each distinct section once, counting every repeated occurrence
		// before constructing the complete response. Settings snapshots are immutable.
		results := make([]json.RawMessage, len(values))
		cache := map[string]json.RawMessage{}
		encodedSize := 2
		for i, value := range values {
			raw := json.RawMessage("null")
			if value != nil {
				key := (*p.Items)[i].Section
				var ok bool
				raw, ok = cache[key]
				if !ok {
					var err error
					raw, err = json.Marshal(value)
					if err != nil {
						return nil, -32603, "configuration could not be encoded"
					}
					cache[key] = raw
				}
			}
			encodedSize += len(raw) + 1
			if encodedSize > maxRPCBody-(32<<10) {
				return nil, -32603, "configuration response exceeds 16 MiB"
			}
			results[i] = raw
		}
		return results, 0, ""
	case "workspace/workspaceFolders":
		s.l.mu.Lock()
		defer s.l.mu.Unlock()
		return s.l.workspaceFolders(), 0, ""
	case "window/workDoneProgress/create":
		return s.createProgress(params)
	default:
		return nil, -32601, "method not supported: " + method
	}
}
