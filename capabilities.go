package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// capabilityInfo is one of the server's services: its provider field in the initialize result,
// the client capability (SECTION.NAME) that offers dynamic registration, and whether the provider
// must be an options object (a bare true is invalid)
type capabilityInfo struct {
	field, client string
	options       bool
}

// capabilityFields are the services by the method that asks for them (and that registers them)
var capabilityFields = map[string]capabilityInfo{
	"textDocument/hover":                {"hoverProvider", "textDocument.hover", false},
	"textDocument/completion":           {"completionProvider", "textDocument.completion", true},
	"textDocument/signatureHelp":        {"signatureHelpProvider", "textDocument.signatureHelp", true},
	"textDocument/definition":           {"definitionProvider", "textDocument.definition", false},
	"textDocument/references":           {"referencesProvider", "textDocument.references", false},
	"textDocument/documentSymbol":       {"documentSymbolProvider", "textDocument.documentSymbol", false},
	"textDocument/codeAction":           {"codeActionProvider", "textDocument.codeAction", false},
	"textDocument/rename":               {"renameProvider", "textDocument.rename", false},
	"textDocument/formatting":           {"documentFormattingProvider", "textDocument.formatting", false},
	"textDocument/declaration":          {"declarationProvider", "textDocument.declaration", false},
	"textDocument/typeDefinition":       {"typeDefinitionProvider", "textDocument.typeDefinition", false},
	"textDocument/implementation":       {"implementationProvider", "textDocument.implementation", false},
	"textDocument/selectionRange":       {"selectionRangeProvider", "textDocument.selectionRange", false},
	"textDocument/prepareCallHierarchy": {"callHierarchyProvider", "textDocument.callHierarchy", false},
	"textDocument/prepareTypeHierarchy": {"typeHierarchyProvider", "textDocument.typeHierarchy", false},
	"workspace/symbol":                  {"workspaceSymbolProvider", "workspace.symbol", false},
	"textDocument/rangeFormatting":      {"documentRangeFormattingProvider", "textDocument.rangeFormatting", false},
	"textDocument/onTypeFormatting":     {"documentOnTypeFormattingProvider", "textDocument.onTypeFormatting", true},
	"textDocument/documentLink":         {"documentLinkProvider", "textDocument.documentLink", true},
	"textDocument/diagnostic":           {"diagnosticProvider", "textDocument.diagnostic", true},
	"textDocument/linkedEditingRange":   {"linkedEditingRangeProvider", "textDocument.linkedEditingRange", false},
}

// providerOption is the option of a provider that enables the requests that depend on it
var providerOption = map[string]string{
	"textDocument/codeAction": "resolveProvider", "textDocument/completion": "resolveProvider",
	"workspace/symbol": "resolveProvider", "textDocument/documentLink": "resolveProvider",
	"textDocument/rename": "prepareProvider",
}

// dependent requests: the service they belong to, and whether they need its option
var dependent = map[string]struct {
	base   string
	option bool
}{
	"codeAction/resolve":          {"textDocument/codeAction", true},
	"completionItem/resolve":      {"textDocument/completion", true},
	"workspaceSymbol/resolve":     {"workspace/symbol", true},
	"documentLink/resolve":        {"textDocument/documentLink", true},
	"textDocument/prepareRename":  {"textDocument/rename", true},
	"callHierarchy/incomingCalls": {"textDocument/prepareCallHierarchy", false},
	"callHierarchy/outgoingCalls": {"textDocument/prepareCallHierarchy", false},
	"typeHierarchy/supertypes":    {"textDocument/prepareTypeHierarchy", false},
	"typeHierarchy/subtypes":      {"textDocument/prepareTypeHierarchy", false},
}

var errCapabilityUnavailable = errors.New("the language server does not provide this service for this document")

type selectorDocument struct{ language, scheme, path string }

func selectorFor(d *document) selectorDocument {
	return selectorDocument{language: languageIDs[d.lang], scheme: "file", path: filepath.ToSlash(filepath.Clean(d.path))}
}

type documentFilter struct {
	language, scheme, base string
	pattern                *regexp.Regexp
	notebook               bool
}
type capabilityRegistration struct {
	method      string
	selector    []documentFilter
	cost        int
	resolve     bool     // the provider's option (providerOption): resolve or prepare
	triggers    []string // onTypeFormatting's trigger characters
	syncKind    int
	includeText bool
	watchers    []watchedFileRule
	fileFilters []fileOpFilter // workspace/did{Create,Rename,Delete}Files
}

func jsonObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, fmt.Errorf("expected object")
	}
	return obj, nil
}
func jsonString(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("expected string")
	}
	return value, nil
}

func parseSelector(raw json.RawMessage) ([]documentFilter, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries []json.RawMessage
	if raw[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > 64 {
		return nil, fmt.Errorf("invalid document selector")
	}
	filters := make([]documentFilter, 0, len(entries))
	for _, entry := range entries {
		var filter documentFilter
		if len(entry) > 0 && entry[0] == '"' {
			filter.language, _ = jsonString(entry)
			if filter.language == "" || len(filter.language) > 256 {
				return nil, fmt.Errorf("empty selector language")
			}
		} else {
			obj, err := jsonObject(entry)
			if err != nil || len(obj) == 0 {
				return nil, fmt.Errorf("invalid document filter")
			}
			for key, value := range obj {
				if string(value) == "null" && (key == "scheme" || key == "pattern") {
					continue
				}
				switch key {
				case "language", "scheme":
					text, err := jsonString(value)
					if err != nil || text == "" || len(text) > 256 {
						return nil, fmt.Errorf("invalid selector %s", key)
					}
					if key == "language" {
						filter.language = text
					} else {
						filter.scheme = text
					}
				case "notebook":
					// Notebook cell filters cannot match rhun's file documents, but may
					// accompany ordinary Python filters in the same registration (Ruff).
					if name, err := jsonString(value); err == nil {
						if name == "" || len(name) > 256 {
							return nil, fmt.Errorf("invalid notebook filter")
						}
					} else {
						nb, err := jsonObject(value)
						if err != nil || len(nb) == 0 {
							return nil, fmt.Errorf("invalid notebook filter")
						}
						for key, value := range nb {
							if key != "notebookType" && key != "scheme" && key != "pattern" {
								return nil, fmt.Errorf("invalid notebook filter field")
							}
							if text, err := jsonString(value); err != nil || text == "" || len(text) > 4096 {
								return nil, fmt.Errorf("invalid notebook filter value")
							}
						}
					}
					filter.notebook = true
				case "pattern":
					pattern, err := jsonString(value)
					if err != nil {
						relative, err := jsonObject(value)
						if err != nil {
							return nil, fmt.Errorf("invalid relative pattern")
						}
						for key := range relative {
							if key != "baseUri" && key != "pattern" {
								return nil, fmt.Errorf("unknown relative pattern field")
							}
						}
						pattern, err = jsonString(relative["pattern"])
						if err != nil {
							return nil, err
						}
						base, err := jsonString(relative["baseUri"])
						if err != nil {
							folder, e := jsonObject(relative["baseUri"])
							if e != nil {
								return nil, e
							}
							base, err = jsonString(folder["uri"])
							if err != nil {
								return nil, err
							}
						}
						if len(base) > 4096 {
							return nil, fmt.Errorf("relative pattern base exceeds 4096 bytes")
						}
						u, err := url.Parse(base)
						if err != nil || u.Scheme != "file" || u.RawQuery != "" || u.Fragment != "" {
							return nil, fmt.Errorf("relative pattern base must be a file URI")
						}
						path := uriPath(base)
						if !filepath.IsAbs(path) {
							return nil, fmt.Errorf("relative pattern base must be absolute")
						}
						filter.base = filepath.ToSlash(filepath.Clean(path))
					}
					filter.pattern, err = compileGlob(pattern, runtime.GOOS == "windows")
					if err != nil {
						return nil, err
					}
				default:
					return nil, fmt.Errorf("unsupported document filter field %s", key)
				}
			}
		}
		filters = append(filters, filter)
	}
	return filters, nil
}
func (r capabilityRegistration) matches(d selectorDocument) bool {
	if r.selector == nil {
		return true
	}
	for _, f := range r.selector {
		if f.notebook {
			continue
		}
		if f.language != "" && f.language != "*" && f.language != d.language {
			continue
		}
		if f.scheme != "" && f.scheme != "*" && f.scheme != d.scheme {
			continue
		}
		path := d.path
		if f.base != "" {
			base, candidate := f.base, path
			if runtime.GOOS == "windows" {
				base, candidate = strings.ToLower(base), strings.ToLower(candidate)
			}
			if candidate == base {
				path = ""
			} else if strings.HasPrefix(candidate, strings.TrimSuffix(base, "/")+"/") {
				path = path[len(strings.TrimSuffix(f.base, "/"))+1:]
			} else {
				continue
			}
		}
		if f.pattern != nil && !f.pattern.MatchString(path) {
			continue
		}
		return true
	}
	return false
}

func (s *server) loadCapabilities(raw json.RawMessage) error {
	result, err := jsonObject(raw)
	if err != nil {
		return fmt.Errorf("invalid initialize result")
	}
	caps, err := jsonObject(result["capabilities"])
	if err != nil {
		return fmt.Errorf("invalid initialize capabilities")
	}
	static := map[string]bool{}
	for method, info := range capabilityFields {
		value := caps[info.field]
		if len(value) == 0 || string(value) == "null" || string(value) == "false" {
			continue
		}
		if _, err := jsonObject(value); err == nil || (!info.options && string(value) == "true") {
			static[method] = true
		} else if s.l.ext != nil {
			s.l.ext.Logf("ignoring invalid %s capability", info.field)
		}
	}
	options := map[string]bool{}
	for method, option := range providerOption {
		if o, err := jsonObject(caps[capabilityFields[method].field]); err == nil {
			options[method] = string(o[option]) == "true"
		}
	}
	var triggers []string
	if o, err := jsonObject(caps["documentOnTypeFormattingProvider"]); err == nil {
		triggers = typeTriggers(o)
	}
	s.capMu.Lock()
	s.staticCapabilities = static
	s.staticOptions = options
	s.staticTriggers = triggers
	s.capMu.Unlock()
	s.loadFileOpCapabilities(caps)
	s.loadFolderCapabilities(caps)
	s.registerLegend(caps) // its semantic token legend, for rhun (present.go)
	return nil
}

// typeTriggers are on-type formatting's trigger characters: firstTriggerCharacter and
// moreTriggerCharacter
func typeTriggers(o map[string]json.RawMessage) []string {
	var out []string
	if first, err := jsonString(o["firstTriggerCharacter"]); err == nil && first != "" {
		out = append(out, first)
	}
	var more []string
	if json.Unmarshal(o["moreTriggerCharacter"], &more) == nil {
		for _, c := range more {
			if c != "" && len(out) < 64 {
				out = append(out, c)
			}
		}
	}
	return out
}

func (s *server) supports(method string, d selectorDocument) bool {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	if s.ended.Load() {
		return false
	}
	option := false
	if dep, ok := dependent[method]; ok {
		method, option = dep.base, dep.option
	}
	if s.staticCapabilities[method] && (!option || s.staticOptions[method]) {
		return true
	}
	for _, registration := range s.registrations {
		if registration.method == method && (!option || registration.resolve) && registration.matches(d) {
			return true
		}
	}
	return false
}

// onTypeTriggers are the trigger characters of on-type formatting for the document: the static
// provider's and those of the registrations that match it
func (s *server) onTypeTriggers(d selectorDocument) []string {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	var out []string
	if s.staticCapabilities["textDocument/onTypeFormatting"] {
		out = append(out, s.staticTriggers...)
	}
	for _, registration := range s.registrations {
		if registration.method == "textDocument/onTypeFormatting" && registration.matches(d) {
			out = append(out, registration.triggers...)
		}
	}
	return out
}

func (s *server) registerCapabilities(params json.RawMessage) (any, int, string) {
	obj, err := jsonObject(params)
	if err != nil {
		return nil, -32602, "invalid registrations"
	}
	var entries []json.RawMessage
	if raw := obj["registrations"]; len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > 256 {
		return nil, -32602, "invalid registrations"
	}
	batch := map[string]capabilityRegistration{}
	batchBytes := 0
	hasWatch, hasSync := false, false
	for _, entry := range entries {
		item, err := jsonObject(entry)
		if err != nil {
			return nil, -32602, "invalid registration"
		}
		id, err := jsonString(item["id"])
		if err != nil || id == "" || len(id) > 256 {
			return nil, -32602, "invalid registration ID"
		}
		method, err := jsonString(item["method"])
		if _, known := capabilityFields[method]; err != nil || (!known && !isSynchronizationMethod(method) && method != watchedFilesMethod && !isFileOpMethod(method) && method != didChangeFoldersMethod && !passiveFileOpMethod(method)) {
			return nil, -32602, "unsupported registration method"
		}
		if _, duplicate := batch[id]; duplicate {
			return nil, -32602, "duplicate registration ID"
		}
		var selector []documentFilter
		resolve := false
		registration := capabilityRegistration{method: method}
		hasWatch = hasWatch || method == watchedFilesMethod
		hasSync = hasSync || isSynchronizationMethod(method)
		options := item["registerOptions"]
		batchBytes += len(id) + len(method) + len(options)
		if batchBytes > 1<<20 {
			return nil, -32602, "registration options exceed 1 MiB"
		}
		if len(options) > 0 && string(options) != "null" {
			options, err := jsonObject(options)
			if err != nil {
				return nil, -32602, "invalid registration options"
			}
			selector, err = parseSelector(options["documentSelector"])
			if err != nil {
				return nil, -32602, err.Error()
			}
			if isSynchronizationMethod(method) {
				if err := parseSynchronizationRegistration(&registration, options); err != nil {
					return nil, -32602, err.Error()
				}
			}
			if method == watchedFilesMethod {
				registration.watchers, err = s.parseWatchedFiles(options)
				if err != nil {
					return nil, -32602, err.Error()
				}
			}
			if isFileOpMethod(method) || passiveFileOpMethod(method) {
				registration.fileFilters, err = parseFileOpFilters(item["registerOptions"])
				if err != nil {
					return nil, -32602, err.Error()
				}
			}
			if option := providerOption[method]; option != "" {
				if raw, exists := options[option]; exists {
					if string(raw) != "true" && string(raw) != "false" {
						return nil, -32602, "invalid " + option
					}
					resolve = string(raw) == "true"
				}
			}
			if method == "textDocument/onTypeFormatting" {
				registration.triggers = typeTriggers(options)
			}
		}
		if (method == "textDocument/didChange" || method == watchedFilesMethod || isFileOpMethod(method)) && (len(options) == 0 || string(options) == "null") {
			return nil, -32602, "registration options required"
		}
		registration.selector, registration.resolve, registration.cost = selector, resolve, len(id)+len(method)+len(options)
		batch[id] = registration
	}
	commit := func() (any, int, string) {
		s.capMu.Lock()
		defer s.capMu.Unlock()
		if s.ended.Load() {
			return nil, -32600, "server is no longer active"
		}
		if len(s.registrations)+len(batch) > 256 {
			return nil, -32602, "registration limit reached"
		}
		totalBytes, totalWatchers := 0, 0
		for _, registration := range s.registrations {
			totalBytes += registration.cost
			totalWatchers += len(registration.watchers)
		}
		for _, registration := range batch {
			totalBytes += registration.cost
			totalWatchers += len(registration.watchers)
		}
		if totalWatchers > 1024 {
			return nil, -32602, "watcher rule limit reached"
		}
		if totalBytes > 1<<20 {
			return nil, -32602, "registration options exceed 1 MiB total"
		}
		for id := range batch {
			if _, exists := s.registrations[id]; exists {
				return nil, -32602, "duplicate registration ID"
			}
		}
		for id, r := range batch {
			s.registrations[id] = r
		}
		if hasWatch {
			s.watchGeneration++
		}
		if hasSync {
			s.syncGeneration++
		}
		return nil, 0, ""
	}
	var result any
	var code int
	var message string
	if hasWatch {
		result, code, message = s.commitWatchRegistry(true, commit)
	} else {
		result, code, message = commit()
	}
	if code == 0 && hasSync {
		s.refreshSynchronization()
	}
	if code == 0 {
		s.l.scopeChanged()
		for _, registration := range batch {
			if registration.method == "textDocument/diagnostic" {
				go s.l.repull(s)
				break
			}
		}
	}
	return result, code, message
}
func (s *server) unregisterCapabilities(params json.RawMessage) (any, int, string) {
	obj, err := jsonObject(params)
	if err != nil {
		return nil, -32602, "invalid unregisterations"
	}
	var entries []json.RawMessage
	if raw := obj["unregisterations"]; len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &entries) != nil || len(entries) > 256 {
		return nil, -32602, "invalid unregisterations"
	}
	batch := map[string]string{}
	for _, entry := range entries {
		item, err := jsonObject(entry)
		if err != nil {
			return nil, -32602, "invalid unregistration"
		}
		id, e1 := jsonString(item["id"])
		method, e2 := jsonString(item["method"])
		if e1 != nil || e2 != nil || id == "" || len(id) > 256 {
			return nil, -32602, "invalid unregistration ID or method"
		}
		if _, duplicate := batch[id]; duplicate {
			return nil, -32602, "duplicate unregistration ID"
		}
		batch[id] = method
	}
	hasWatch, hasSync := false, false
	for _, method := range batch {
		hasWatch = hasWatch || method == watchedFilesMethod
		hasSync = hasSync || isSynchronizationMethod(method)
	}
	commit := func() (any, int, string) {
		s.capMu.Lock()
		defer s.capMu.Unlock()
		if s.ended.Load() {
			return nil, -32600, "server is no longer active"
		}
		for id, method := range batch {
			r, exists := s.registrations[id]
			if !exists || r.method != method {
				return nil, -32602, "unknown registration ID or method"
			}
		}
		for id := range batch {
			delete(s.registrations, id)
		}
		if hasWatch {
			s.watchGeneration++
		}
		if hasSync {
			s.syncGeneration++
		}
		return nil, 0, ""
	}
	var result any
	var code int
	var message string
	if hasWatch {
		result, code, message = s.commitWatchRegistry(false, commit)
	} else {
		result, code, message = commit()
	}
	if code == 0 && hasSync {
		s.refreshSynchronization()
	}
	if code == 0 {
		s.l.scopeChanged()
	}
	return result, code, message
}

func (s *server) requestDocument(wait time.Duration, method string, params any, d selectorDocument) (json.RawMessage, error) {
	guard := func() error {
		if !s.supports(method, d) {
			return errCapabilityUnavailable
		}
		return nil
	}
	if err := guard(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return s.callContextGuard(ctx, method, params, guard)
}

// Some servers (Dart) include will* registrations in a batch despite the client only
// advertising did*. Retain their IDs without invoking them, so the supported services survive.
func passiveFileOpMethod(method string) bool {
	return method == "workspace/willCreateFiles" || method == "workspace/willRenameFiles" || method == "workspace/willDeleteFiles"
}
