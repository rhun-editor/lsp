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

var capabilityFields = map[string]string{
	"textDocument/hover": "hoverProvider", "textDocument/completion": "completionProvider",
	"textDocument/signatureHelp": "signatureHelpProvider", "textDocument/definition": "definitionProvider",
	"textDocument/references": "referencesProvider", "textDocument/documentSymbol": "documentSymbolProvider",
	"textDocument/codeAction": "codeActionProvider", "textDocument/rename": "renameProvider",
	"textDocument/formatting": "documentFormattingProvider",
}
var errCapabilityUnavailable = errors.New("the language server does not provide this service for this document")

type selectorDocument struct{ language, scheme, path string }

func selectorFor(d *document) selectorDocument {
	return selectorDocument{language: languageIDs[d.lang], scheme: "file", path: filepath.ToSlash(filepath.Clean(d.path))}
}

type documentFilter struct {
	language, scheme, base string
	pattern                *regexp.Regexp
}
type capabilityRegistration struct {
	method      string
	selector    []documentFilter
	cost        int
	resolve     bool
	syncKind    int
	includeText bool
	watchers    []watchedFileRule
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
	for method, field := range capabilityFields {
		value := caps[field]
		if len(value) == 0 || string(value) == "null" || string(value) == "false" {
			continue
		}
		options := method == "textDocument/completion" || method == "textDocument/signatureHelp"
		if _, err := jsonObject(value); err == nil || (!options && string(value) == "true") {
			static[method] = true
		} else if s.l.ext != nil {
			s.l.ext.Logf("ignoring invalid %s capability", field)
		}
	}
	s.capMu.Lock()
	s.staticCapabilities = static
	s.staticResolve = false
	if options, err := jsonObject(caps["codeActionProvider"]); err == nil {
		s.staticResolve = string(options["resolveProvider"]) == "true"
	}
	s.staticCompletionResolve = false
	if options, err := jsonObject(caps["completionProvider"]); err == nil {
		s.staticCompletionResolve = string(options["resolveProvider"]) == "true"
	}
	s.capMu.Unlock()
	return nil
}
func (s *server) supports(method string, d selectorDocument) bool {
	s.capMu.RLock()
	defer s.capMu.RUnlock()
	if s.ended.Load() {
		return false
	}
	resolve := method == "codeAction/resolve"
	if resolve {
		method = "textDocument/codeAction"
	}
	if method == "completionItem/resolve" {
		resolve = true
		method = "textDocument/completion"
		if s.staticCapabilities[method] && s.staticCompletionResolve {
			return true
		}
	} else if s.staticCapabilities[method] && (!resolve || s.staticResolve) {
		return true
	}
	for _, registration := range s.registrations {
		if registration.method == method && (!resolve || registration.resolve) && registration.matches(d) {
			return true
		}
	}
	return false
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
		if err != nil || (capabilityFields[method] == "" && !isSynchronizationMethod(method) && method != watchedFilesMethod) {
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
			if raw, exists := options["resolveProvider"]; exists && (method == "textDocument/codeAction" || method == "textDocument/completion") {
				if string(raw) != "true" && string(raw) != "false" {
					return nil, -32602, "invalid resolveProvider"
				}
				resolve = string(raw) == "true"
			}
		}
		if (method == "textDocument/didChange" || method == watchedFilesMethod) && (len(options) == 0 || string(options) == "null") {
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
