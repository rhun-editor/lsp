package main

// An in-process harness for the protocol 1.1 language features: a fake rhun drives the
// extension through the real SDK over pipes, and a fake language server answers the extension's
// requests through the real transport.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// ---- the fake language server

type fakeLSP struct {
	t      *testing.T
	out    *io.PipeWriter
	wmu    sync.Mutex
	mu     sync.Mutex
	got    []map[string]any
	gotRaw []json.RawMessage
	handle func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool)
	seen   chan string
}

func (f *fakeLSP) send(msg any) {
	data, _ := json.Marshal(msg)
	f.wmu.Lock()
	defer f.wmu.Unlock()
	fmt.Fprintf(f.out, "Content-Length: %d\r\n\r\n%s", len(data), data)
}
func (f *fakeLSP) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}
func (f *fakeLSP) fail(id json.RawMessage, code int, message string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}
func (f *fakeLSP) notify(method string, params any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// messages are the extension's messages of a method, as they came
func (f *fakeLSP) messages(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.got {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

// await waits until the extension sent a message of the method
func (f *fakeLSP) await(method string) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := f.messages(method); len(m) > 0 {
			return m[len(m)-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("no %s", method)
	return nil
}

// awaitN waits until the extension sent n messages of the method and returns the nth
func (f *fakeLSP) awaitN(method string, n int) map[string]any {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m := f.messages(method); len(m) >= n {
			return m[n-1]
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("fewer than %d %s", n, method)
	return nil
}

func (f *fakeLSP) serve(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		length := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
				length, _ = strconv.Atoi(v)
			}
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(br, body); err != nil {
			return
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &msg)
		f.mu.Lock()
		f.got = append(f.got, m)
		f.gotRaw = append(f.gotRaw, body)
		f.mu.Unlock()
		if msg.Method == "" || len(msg.ID) == 0 {
			continue
		}
		if f.handle != nil {
			go func() {
				if result, ok := f.handle(f, msg.Method, msg.ID, msg.Params); ok {
					f.reply(msg.ID, result)
				}
			}()
		} else {
			f.reply(msg.ID, nil)
		}
	}
}

// ---- the fake rhun

type hostRequest struct {
	id   string
	verb string
	args []string
	body []byte
}

type hostAnswer struct {
	ok   bool
	args []string
	body []byte
}

type fakeHost struct {
	t        *testing.T
	in       *io.PipeWriter
	wmu      sync.Mutex
	mu       sync.Mutex
	answers  map[string]chan hostAnswer
	requests chan hostRequest
	replyTo  func(r hostRequest) string // the reply line to an extension request ("" for ok ID)
	next     int
}

// the extension's requests that carry a body: LEN is their last field
var bodiedVerbs = map[string]bool{"toast": true, "status": true, "query-part": true, "query-progress": true,
	"apply": true, "file-diagnostics": true, "diagnostics": true, "hover": true, "replace": true, "insert": true, "choose": true}

func (h *fakeHost) write(s string) {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	io.WriteString(h.in, s)
}

func (h *fakeHost) read(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "hello" {
			continue
		}
		if f[0] == "ok" || f[0] == "error" {
			a := hostAnswer{ok: f[0] == "ok", args: f[2:]}
			if a.ok && len(f) == 3 {
				n, _ := strconv.Atoi(f[2])
				a.body = make([]byte, n)
				io.ReadFull(br, a.body)
			}
			h.mu.Lock()
			ch := h.answers[f[1]]
			h.mu.Unlock()
			if ch != nil {
				ch <- a
			}
			continue
		}
		req := hostRequest{id: f[0], verb: f[1], args: f[2:]}
		if bodiedVerbs[req.verb] && len(f) > 2 {
			n, _ := strconv.Atoi(f[len(f)-1])
			req.body = make([]byte, n)
			io.ReadFull(br, req.body)
		}
		reply := ""
		if h.replyTo != nil {
			reply = h.replyTo(req)
		}
		if reply == "" {
			reply = "ok " + req.id
		}
		h.write(reply + "\n")
		select {
		case h.requests <- req:
		default:
		}
	}
}

// ask sends one of rhun's requests ("VERB ARGS", and a body) and waits for its answer
func (h *fakeHost) ask(line string, body []byte) hostAnswer {
	h.t.Helper()
	h.mu.Lock()
	h.next++
	id := strconv.Itoa(100 + h.next)
	ch := make(chan hostAnswer, 1)
	h.answers[id] = ch
	h.mu.Unlock()
	h.write(id + " " + line + "\n" + string(body))
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Second):
		h.t.Fatalf("no answer to %q", line)
	}
	return hostAnswer{}
}

// askAsync sends a request and returns its id and the channel its answer comes on
func (h *fakeHost) askAsync(line string) (string, chan hostAnswer) {
	h.mu.Lock()
	h.next++
	id := strconv.Itoa(100 + h.next)
	ch := make(chan hostAnswer, 1)
	h.answers[id] = ch
	h.mu.Unlock()
	h.write(id + " " + line + "\n")
	return id, ch
}

// request waits for the extension's next request of a verb
func (h *fakeHost) request(verb string) hostRequest {
	h.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-h.requests:
			if r.verb == verb {
				return r
			}
		case <-deadline:
			h.t.Fatalf("no %s request", verb)
			return hostRequest{}
		}
	}
}

// ---- the harness

type capture struct {
	*lsp
	got chan *rhunext.Ext
}

func (c *capture) Event(ext *rhunext.Ext, ev rhunext.Event) {
	if ev.Name == "features" {
		c.got <- ext
		return
	}
	c.lsp.Event(ext, ev)
}

type harness struct {
	l    *lsp
	s    *server
	d    *document
	host *fakeHost
	srv  *fakeLSP
}

const harnessPath = "/project/main.go"

// newHarness starts the extension with the features agreed, one Go document with text at
// version 1 synchronized to a server with the capabilities (an initialize result's
// "capabilities" object) answering through handle
func newHarness(t *testing.T, features []string, caps, text string,
	handle func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool)) *harness {
	t.Helper()
	hostR, hostW := io.Pipe()
	extR, extW := io.Pipe()
	l := &lsp{settings: &settingsConfig{}, servers: map[string]*server{}, project: "/project", config: map[string][]string{}}
	c := &capture{lsp: l, got: make(chan *rhunext.Ext, 1)}
	go rhunext.RunOnWith(hostR, extW, io.Discard, features, c)
	h := &fakeHost{t: t, in: hostW, answers: map[string]chan hostAnswer{}, requests: make(chan hostRequest, 256)}
	go h.read(extR)
	h.write("0 features " + strings.Join(features, " ") + "\n")
	select {
	case l.ext = <-c.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no features event")
	}
	s, srv := addFakeServer(t, l, "Go", caps, handle)
	d := &document{id: 1, version: 1, lang: "Go", path: harnessPath, text: []byte(text), server: s,
		syncedServer: s, syncedVersion: 1, opened: true, lspVer: 1}
	l.docs = map[rhunext.Doc]*document{1: d}
	l.byPath = map[string]*document{d.path: d}
	l.diags = map[string]*keptDiagnostics{}
	t.Cleanup(func() {
		hostW.Close()
		extW.Close()
	})
	return &harness{l: l, s: s, d: d, host: h, srv: srv}
}

// addFakeServer runs a fake server for a language, initialized with the capabilities
func addFakeServer(t *testing.T, l *lsp, lang, caps string,
	handle func(f *fakeLSP, method string, id, params json.RawMessage) (any, bool)) (*server, *fakeLSP) {
	t.Helper()
	s := newServer(l, lang, []string{"fake"})
	srvInR, srvInW := io.Pipe()
	srvOutR, srvOutW := io.Pipe()
	s.in = srvInW
	srv := &fakeLSP{t: t, out: srvOutW, handle: handle}
	go srv.serve(srvInR)
	go s.read(bufio.NewReaderSize(srvOutR, maxRPCHeader))
	if err := s.loadCapabilities(json.RawMessage(`{"capabilities":` + caps + `}`)); err != nil {
		t.Fatal(err)
	}
	s.ready.Store(true)
	l.mu.Lock()
	l.servers[lang] = s
	l.mu.Unlock()
	t.Cleanup(func() {
		s.stop()
		srvOutW.Close()
	})
	return s, srv
}

// ---- reading answers

type actionRecord struct {
	kind, title string
	edit, extra string
}

// parseActions reads a code-actions answer
func parseActions(t *testing.T, body []byte) []actionRecord {
	t.Helper()
	s := string(body)
	head, rest, _ := strings.Cut(s, "\n")
	n, err := strconv.Atoi(head)
	if err != nil {
		t.Fatalf("answer %q", s)
	}
	var out []actionRecord
	for i := 0; i < n; i++ {
		line, tail, _ := strings.Cut(rest, "\n")
		f := strings.Fields(line)
		if len(f) != 4 {
			t.Fatalf("record %q in %q", line, s)
		}
		tl, _ := strconv.Atoi(f[1])
		el, _ := strconv.Atoi(f[2])
		xl, _ := strconv.Atoi(f[3])
		out = append(out, actionRecord{kind: f[0], title: tail[:tl], edit: tail[tl : tl+el], extra: tail[tl+el : tl+el+xl]})
		rest = tail[tl+el+xl:]
	}
	return out
}

// extraRecord is the bytes of an EXTRA record of a name, and whether it is there
func extraRecord(extra, name string) (string, bool) {
	for extra != "" {
		line, rest, _ := strings.Cut(extra, "\n")
		f := strings.Fields(line)
		if len(f) == 0 {
			return "", false
		}
		n, _ := strconv.Atoi(f[len(f)-1])
		if f[0] == name {
			return rest[:n], true
		}
		extra = rest[n:]
	}
	return "", false
}

// lspLoc is an LSP Location
func lspLoc(uri string, l1, c1, l2, c2 int) map[string]any {
	return map[string]any{"uri": uri, "range": rng(l1, c1, l2, c2)}
}

func rng(l1, c1, l2, c2 int) map[string]any {
	return map[string]any{"start": map[string]any{"line": l1, "character": c1}, "end": map[string]any{"line": l2, "character": c2}}
}
