package main

// The diagnostics the servers publish, kept per file as they came: code actions send the ones in
// their range (or the ones rhun names by id) back to the server as their context, and an action's
// diagnostics become the ids of those it fixes.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"sync/atomic"
)

// keptDiagnostics are a file's diagnostics as the server published them, each with its id: unique
// in the set and never reused, so an id rhun names from an older set is simply not found
type keptDiagnostics struct {
	raw []json.RawMessage
	ids []string
}

var diagnosticSerial atomic.Uint64

func keepDiagnostics(raw []json.RawMessage) *keptDiagnostics {
	set := strconv.FormatUint(diagnosticSerial.Add(1), 10)
	k := &keptDiagnostics{raw: raw, ids: make([]string, len(raw))}
	for i := range raw {
		k.ids[i] = "d" + set + "." + strconv.Itoa(i)
	}
	return k
}

// diagnosticKey is what identifies a diagnostic a server sends back in an action
type diagnosticKey struct {
	Range    lspRange        `json:"range"`
	Message  string          `json:"message"`
	Source   string          `json:"source"`
	Code     json.RawMessage `json:"code"`
	Severity int             `json:"severity"`
}

func (a diagnosticKey) same(b diagnosticKey) bool {
	return a.Range == b.Range && a.Message == b.Message && a.Source == b.Source &&
		bytes.Equal(bytes.TrimSpace(a.Code), bytes.TrimSpace(b.Code)) && (a.Severity == 0 || b.Severity == 0 || a.Severity == b.Severity)
}

// rangeDiagnostics are the file's diagnostics that touch the range
func (l *lsp) rangeDiagnostics(path string, start, end lspPosition) []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []json.RawMessage{}
	if k := l.diags[path]; k != nil {
		for _, raw := range k.raw {
			var x struct {
				Range lspRange `json:"range"`
			}
			if json.Unmarshal(raw, &x) == nil && !before(x.Range.End, start) && !before(end, x.Range.Start) {
				out = append(out, raw)
			}
		}
	}
	return out
}

// diagnosticsByID are the file's diagnostics rhun names (those still in the current set)
func (l *lsp) diagnosticsByID(path string, ids []string) []json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []json.RawMessage{}
	k := l.diags[path]
	if k == nil {
		return out
	}
	for _, id := range ids {
		for i, have := range k.ids {
			if have == id {
				out = append(out, k.raw[i])
				break
			}
		}
	}
	return out
}

// diagnosticIDs are the ids of the kept diagnostics an action names (it echoes them back)
func (l *lsp) diagnosticIDs(path string, given []json.RawMessage) []string {
	if len(given) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.diags[path]
	if k == nil {
		return nil
	}
	keys := make([]diagnosticKey, len(k.raw))
	for i, raw := range k.raw {
		json.Unmarshal(raw, &keys[i])
	}
	var out []string
	for _, raw := range given {
		var g diagnosticKey
		if json.Unmarshal(raw, &g) != nil {
			continue
		}
		for i, have := range keys {
			if have.same(g) {
				out = append(out, k.ids[i])
				break
			}
		}
	}
	return out
}
