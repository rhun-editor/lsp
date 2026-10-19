package main

// The diagnostics the servers publish, kept per file as they came: code actions send the ones in
// their range (or the ones rhun names by id) back to the server as their context, and an action's
// diagnostics become the ids of those it fixes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
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

// ---- the diagnostics rhun shows

// lspDiagnostic is a diagnostic as a server publishes it
type lspDiagnostic struct {
	Range           lspRange        `json:"range"`
	Severity        int             `json:"severity"`
	Code            json.RawMessage `json:"code"`
	CodeDescription *struct {
		Href string `json:"href"`
	} `json:"codeDescription"`
	Source  string `json:"source"`
	Message string `json:"message"`
	Tags    []int  `json:"tags"`
	Related []struct {
		Location location `json:"location"`
		Message  string   `json:"message"`
	} `json:"relatedInformation"`
}

func severityOf(n int) rhunext.Severity {
	switch n {
	case 2:
		return rhunext.SevWarning
	case 3:
		return rhunext.SevInfo
	case 4:
		return rhunext.SevHint
	}
	return rhunext.SevError
}

// codeText is a diagnostic code, a string or a number, as text
func codeText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if s, err := jsonString(raw); err == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// richDiagnostics are a file's kept diagnostics for file-diagnostics, their positions in text:
// ids, codes, links, sources, tags and the related places (in their own files' texts)
func (l *lsp) richDiagnostics(k *keptDiagnostics, path string, text []byte) []rhunext.Diagnostic {
	texts := map[string][]byte{path: text}
	textFor := func(p string) []byte {
		t, ok := texts[p]
		if !ok {
			t, _, _ = l.textOf(p)
			texts[p] = t
		}
		return t
	}
	out := make([]rhunext.Diagnostic, 0, len(k.raw))
	for i, raw := range k.raw {
		var x lspDiagnostic
		if json.Unmarshal(raw, &x) != nil {
			continue
		}
		d := rhunext.Diagnostic{Range: rhunRange(text, x.Range), Severity: severityOf(x.Severity),
			Message: strings.TrimRight(x.Message, "\n"), ID: k.ids[i], Code: codeText(x.Code), Source: x.Source}
		if x.CodeDescription != nil {
			d.Href = x.CodeDescription.Href
		}
		for _, t := range x.Tags {
			switch t {
			case 1:
				d.Tags = append(d.Tags, rhunext.TagUnnecessary)
			case 2:
				d.Tags = append(d.Tags, rhunext.TagDeprecated)
			}
		}
		for _, r := range x.Related {
			p := uriPath(r.Location.URI)
			if !filepath.IsAbs(p) || len(d.Related) == rhunext.MaxRelated {
				continue
			}
			d.Related = append(d.Related, rhunext.RelatedInfo{Target: rhunext.Target{Path: p},
				Range: rhunRange(textFor(p), r.Location.Range), Message: oneLineText(r.Message)})
		}
		out = append(out, d)
	}
	return out
}

// plainDiagnostics are v1's: the first line of the message after the source
func plainDiagnostics(k *keptDiagnostics, text []byte) []rhunext.Diagnostic {
	out := make([]rhunext.Diagnostic, 0, len(k.raw))
	for _, raw := range k.raw {
		var x lspDiagnostic
		if json.Unmarshal(raw, &x) != nil {
			continue
		}
		msg := x.Message
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if x.Source != "" {
			msg = x.Source + ": " + msg
		}
		out = append(out, rhunext.Diagnostic{Range: rhunRange(text, x.Range), Severity: severityOf(x.Severity), Message: msg})
	}
	return out
}

// diagnosticsJob is a set to give rhun (file-diagnostics): for an open document at the version of
// its text, else for the file on disk; a clear job empties the set of a server that stopped
type diagnosticsJob struct {
	server  *server
	path    string
	kept    *keptDiagnostics
	doc     rhunext.Doc
	version int64
	text    []byte
	clear   bool
}

// keepFileDiagnostics keeps a server's diagnostics for a file and queues them for rhun (with
// file-diagnostics; v1 diagnostics otherwise go to the open document at once). It reports whether
// the file-diagnostics path took them.
func (l *lsp) keepFileDiagnostics(s *server, path string, raw []json.RawMessage) (*keptDiagnostics, bool) {
	kept := keepDiagnostics(raw)
	l.mu.Lock()
	l.diags[path] = kept
	if !l.has(rhunext.FeatureFileDiagnostics) {
		l.mu.Unlock()
		return kept, false
	}
	if l.diagOwner == nil {
		l.diagOwner = map[string]*server{}
	}
	l.diagOwner[path] = s
	job := diagnosticsJob{server: s, path: path, kept: kept}
	if d := l.byPath[path]; d != nil && d.text != nil {
		job.doc, job.version, job.text = d.id, d.version, d.text
	}
	l.mu.Unlock()
	l.queueDiagnostics(job)
	return kept, true
}

// dropDiagnostics empties the sets of a server that stopped, unless another published since
func (l *lsp) dropDiagnostics(s *server) {
	if !l.has(rhunext.FeatureFileDiagnostics) {
		return
	}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return
	}
	var paths []string
	for path, owner := range l.diagOwner {
		if owner == s {
			paths = append(paths, path)
		}
	}
	l.mu.Unlock()
	for _, path := range paths {
		l.queueDiagnostics(diagnosticsJob{server: s, path: path, clear: true})
	}
}

func (l *lsp) queueDiagnostics(job diagnosticsJob) {
	l.diagMu.Lock()
	l.diagJobs = append(l.diagJobs, job)
	start := !l.diagRunning
	l.diagRunning = true
	l.diagMu.Unlock()
	if start {
		go l.sendDiagnostics()
	}
}

// sendDiagnostics gives rhun the queued sets one at a time, in the order they came
func (l *lsp) sendDiagnostics() {
	for {
		l.diagMu.Lock()
		if len(l.diagJobs) == 0 {
			l.diagRunning = false
			l.diagMu.Unlock()
			return
		}
		job := l.diagJobs[0]
		l.diagJobs = l.diagJobs[1:]
		l.diagMu.Unlock()
		l.sendDiagnosticsJob(job)
	}
}

func (l *lsp) sendDiagnosticsJob(job diagnosticsJob) {
	if job.clear {
		l.mu.Lock()
		current := l.diagOwner[job.path] == job.server && !l.stopping
		if current {
			delete(l.diagOwner, job.path)
			delete(l.diags, job.path)
		}
		l.mu.Unlock()
		if current {
			if err := l.ext.FileDiagnostics(rhunext.Target{Path: job.path}, rhunext.OnDisk, nil); err != nil {
				l.ext.Logf("diagnostics %s: %v", job.path, err)
			}
		}
		return
	}
	l.mu.Lock()
	stale := l.diags[job.path] != job.kept
	l.mu.Unlock()
	if stale {
		return // a newer set for the file is queued
	}
	if job.doc != 0 {
		err := l.ext.FileDiagnostics(rhunext.Target{Doc: job.doc}, job.version, l.richDiagnostics(job.kept, job.path, job.text))
		var re *rhunext.Error
		if err == nil || !errors.As(err, &re) || re.Code != "nodoc" {
			if err != nil {
				// stale: the user typed meanwhile; the server publishes again after the change
				l.ext.Logf("diagnostics %s: %v", job.path, err)
			}
			return
		}
	}
	text, _ := os.ReadFile(job.path)
	if err := l.ext.FileDiagnostics(rhunext.Target{Path: job.path}, rhunext.OnDisk, l.richDiagnostics(job.kept, job.path, text)); err != nil {
		l.ext.Logf("diagnostics %s: %v", job.path, err)
	}
}

// ---- pull diagnostics (textDocument/diagnostic), for open documents after they change

type pullState struct {
	timer    *time.Timer
	server   *server
	resultID string
}

const pullDelay = 200 * time.Millisecond

// schedulePull asks the server for the document's diagnostics a moment after it last changed,
// when the server offers them on request (file-diagnostics only)
func (l *lsp) schedulePull(d *document, s *server) {
	if !l.has(rhunext.FeatureFileDiagnostics) || !s.supports("textDocument/diagnostic", selectorFor(d)) {
		return
	}
	l.pullMu.Lock()
	defer l.pullMu.Unlock()
	if l.pulls == nil {
		l.pulls = map[*document]*pullState{}
	}
	p := l.pulls[d]
	if p == nil {
		p = &pullState{}
		l.pulls[d] = p
	}
	if p.server != s {
		p.server, p.resultID = s, ""
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(pullDelay, func() { l.pull(d, s) })
}

// repull asks again for every open document of the server (workspace/diagnostic/refresh)
func (l *lsp) repull(s *server) {
	l.mu.Lock()
	var docs []*document
	for _, d := range l.docs {
		if d.server == s {
			docs = append(docs, d)
		}
	}
	l.mu.Unlock()
	for _, d := range docs {
		l.schedulePull(d, s)
	}
}

func (l *lsp) forgetPull(d *document) {
	l.pullMu.Lock()
	defer l.pullMu.Unlock()
	if p := l.pulls[d]; p != nil && p.timer != nil {
		p.timer.Stop()
	}
	delete(l.pulls, d)
}

// documentReport is a DocumentDiagnosticReport: full (items) or unchanged since resultId
type documentReport struct {
	Kind     string            `json:"kind"`
	ResultID string            `json:"resultId"`
	Items    []json.RawMessage `json:"items"`
	Related  map[string]struct {
		Kind  string            `json:"kind"`
		Items []json.RawMessage `json:"items"`
	} `json:"relatedDocuments"`
}

func (l *lsp) pull(d *document, s *server) {
	l.mu.Lock()
	q := l.captureQueryLocked(d)
	l.mu.Unlock()
	if q == nil || q.server != s {
		return
	}
	l.pullMu.Lock()
	p := l.pulls[d]
	previous := ""
	if p != nil && p.server == s {
		previous = p.resultID
	}
	l.pullMu.Unlock()
	params := map[string]any{"textDocument": map[string]any{"uri": fileURI(q.path)}}
	if previous != "" {
		params["previousResultId"] = previous
	}
	res, err := q.request(5*time.Second, "textDocument/diagnostic", params)
	var report documentReport
	if err != nil || json.Unmarshal(res, &report) != nil {
		return
	}
	l.pullMu.Lock()
	if p := l.pulls[d]; p != nil && p.server == s {
		p.resultID = report.ResultID
	}
	l.pullMu.Unlock()
	if report.Kind == "full" {
		kept := keepDiagnostics(report.Items)
		l.mu.Lock()
		l.diags[q.path] = kept
		if l.diagOwner == nil {
			l.diagOwner = map[string]*server{}
		}
		l.diagOwner[q.path] = s
		l.mu.Unlock()
		l.queueDiagnostics(diagnosticsJob{server: s, path: q.path, kept: kept, doc: q.id, version: q.version, text: q.text})
	}
	for uri, r := range report.Related {
		if path := uriPath(uri); r.Kind == "full" && filepath.IsAbs(path) {
			l.keepFileDiagnostics(s, path, r.Items)
		}
	}
}
