package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func rpcFrame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestRPCFramingLimits(t *testing.T) {
	for _, text := range []string{
		"Content-Length: -1\r\n\r\n", "Content-Length: 16777217\r\n\r\n",
		"Content-Length: 1\r\ncontent-length: 1\r\n\r\n{}", "Unknown: 1\r\n\r\n",
		"Content-Length: +1\n\n", strings.Repeat("x", maxRPCHeader+1) + "\n",
		rpcFrame(`[]`), rpcFrame(`null`), rpcFrame(`{"jsonrpc":"1.0"}`), rpcFrame("{\xff}"),
	} {
		if _, err := readRPC(bufio.NewReaderSize(strings.NewReader(text), maxRPCHeader)); err == nil {
			t.Fatalf("accepted %q", text[:min(len(text), 80)])
		}
	}
	body := `{"jsonrpc":"2.0","method":"example","params":"日本語"}`
	r := bufio.NewReaderSize(&oneByteReader{r: strings.NewReader(rpcFrame(body) + rpcFrame(body))}, maxRPCHeader)
	for i := 0; i < 2; i++ {
		got, err := readRPC(r)
		if err != nil || string(got) != body {
			t.Fatal(string(got), err)
		}
	}
	got, err := readRPC(bufio.NewReader(strings.NewReader("content-length: " + fmt.Sprint(len(body)) + "\nContent-Type: application/vscode-jsonrpc; charset=utf-8\n\n" + body)))
	if err != nil || string(got) != body {
		t.Fatal(string(got), err)
	}
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(p []byte) (int, error) { return r.r.Read(p[:min(len(p), 1)]) }

func pendingCount(s *server) int { s.pmu.Lock(); defer s.pmu.Unlock(); return len(s.pending) }
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	limit := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(limit) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRequestTimeoutCancelsAndRemovesPending(t *testing.T) {
	_, s, p := testServer("Go")
	defer s.stop()
	_, err := s.callFor(30*time.Millisecond, "textDocument/hover", nil)
	if !errors.Is(err, context.DeadlineExceeded) || pendingCount(s) != 0 {
		t.Fatal(err, pendingCount(s))
	}
	eventually(t, func() bool { return len(p.messages(t)) == 2 })
	msgs := p.messages(t)
	if msgs[1]["method"] != "$/cancelRequest" || msgs[1]["params"].(map[string]any)["id"] != msgs[0]["id"] {
		t.Fatal(msgs)
	}
	// A late response has no waiter and must not block the reader.
	go s.read(bufio.NewReader(strings.NewReader(rpcFrame(`{"jsonrpc":"2.0","id":1,"result":true}`))))
	select {
	case <-s.done:
	case <-time.After(time.Second):
		t.Fatal("late response blocked reader")
	}
}

type failedPipe struct{}

func (failedPipe) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (failedPipe) Close() error              { return nil }
func TestWriteFailureClearsPending(t *testing.T) {
	_, s, _ := testServer("Go")
	s.in = failedPipe{}
	if _, err := s.call("test", nil); err == nil || pendingCount(s) != 0 {
		t.Fatal(err, pendingCount(s))
	}
}
func TestPendingRequestLimit(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	for i := 0; i < maxPending; i++ {
		s.pending[-i] = make(chan json.RawMessage, 1)
	}
	if _, err := s.call("excess", nil); err == nil || pendingCount(s) != maxPending {
		t.Fatal(err, pendingCount(s))
	}
}

type blockedPipe struct {
	done    chan struct{}
	once    sync.Once
	started chan struct{}
}

func (p *blockedPipe) Write([]byte) (int, error) {
	p.once.Do(func() { close(p.started) })
	<-p.done
	return 0, io.ErrClosedPipe
}
func (p *blockedPipe) Close() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}
func TestBlockedWriterDeadlineClosesTransport(t *testing.T) {
	_, s, _ := testServer("Go")
	p := &blockedPipe{done: make(chan struct{}), started: make(chan struct{})}
	s.in = p
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := s.writeContext(ctx, map[string]any{"test": true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !s.ended.Load() {
		t.Fatal("blocked transport survived")
	}
	eventually(t, func() bool { return len(s.writeGate) == 0 })
}
func TestCanceledGateWaitDoesNotKillTransport(t *testing.T) {
	_, s, _ := testServer("Go")
	defer s.stop()
	s.writeGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.writeContext(ctx, map[string]any{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s.ended.Load() {
		t.Fatal("canceled gate waiter killed server")
	}
	<-s.writeGate
}

func TestTransportHelper(t *testing.T) {
	if len(os.Args) < 2 {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "final-response":
		body, err := readRPC(bufio.NewReader(os.Stdin))
		if err != nil {
			os.Exit(2)
		}
		var req struct {
			ID int `json:"id"`
		}
		json.Unmarshal(body, &req)
		fmt.Print(rpcFrame(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":"final"}`, req.ID)))
		os.Exit(0)
	case "no-read":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "exit-now":
		os.Exit(0)
	}
}
func subprocessServer(mode string) *server {
	l := &lsp{settings: &settingsConfig{}, servers: map[string]*server{}}
	return newServer(l, "Go", []string{os.Args[0], "-test.run=^TestTransportHelper$", "--", mode})
}
func TestNaturalExitDrainsFinalResponse(t *testing.T) {
	s := subprocessServer("final-response")
	defer s.stop()
	out, err := s.launch()
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan string, 1)
	go func() { res, err := s.callFor(3*time.Second, "last", nil); result <- fmt.Sprint(string(res), err) }()
	// Deliberately wait for reaping before starting the reader. The response must
	// survive Wait and reach the caller even if EOF closes done immediately after.
	select {
	case <-s.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("child not reaped")
	}
	go s.read(bufio.NewReader(out))
	select {
	case got := <-result:
		if got != `"final"<nil>` {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("final response lost")
	}
}
func TestNaturalExitDrainIsBounded(t *testing.T) {
	s := subprocessServer("exit-now")
	defer s.stop()
	if _, err := s.launch(); err != nil {
		t.Fatal(err)
	}
	// No reader: simulates stdout held by a descendant. Wait must still reap and
	// the drain deadline must end the transport without waiting for EOF.
	select {
	case <-s.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("not reaped")
	}
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("drain never ended")
	}
}
func TestStopBeforeStartAndBlockedChild(t *testing.T) {
	s := subprocessServer("exit-now")
	s.stop()
	if _, err := s.launch(); err == nil || s.cmd != nil {
		t.Fatal("launched stopped server")
	}
	s = subprocessServer("no-read")
	defer s.stop()
	if _, err := s.launch(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.writeContext(ctx, map[string]any{"payload": strings.Repeat("x", 1<<20)}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-s.waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked child not reaped")
	}
}

func TestWriteAllHandlesPartialWrites(t *testing.T) {
	var out bytes.Buffer
	if err := writeAll(shortWriter{&out}, []byte("abcdef")); err != nil || out.String() != "abcdef" {
		t.Fatal(out.String(), err)
	}
}

type shortWriter struct{ w io.Writer }

func (w shortWriter) Write(p []byte) (int, error) { return w.w.Write(p[:min(2, len(p))]) }

func TestBufferedResponseSurvivesConcurrentWriteTeardown(t *testing.T) {
	_, s, pipe := testServer("Go")
	pipe.onBody = func([]byte) {
		s.pmu.Lock()
		ch := s.pending[1]
		delete(s.pending, 1)
		s.pmu.Unlock()
		ch <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":"received"}`)
		s.endServer("")
	}
	result, err := s.call("last", nil)
	if err != nil || string(result) != `"received"` {
		t.Fatal(string(result), err)
	}
}

func TestRPCEnvelopeValidation(t *testing.T) {
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"result":true,"error":{"code":1,"message":"bad"}}`,
		`{"jsonrpc":"2.0","id":[],"result":true}`,
		`{"jsonrpc":"2.0","id":1.5,"result":true}`,
		`{"jsonrpc":"2.0","id":1,"error":null}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":null,"message":"bad"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":null}}`,
		`{"jsonrpc":"2.0","method":null}`, `{"jsonrpc":"2.0","method":""}`,
		`{"jsonrpc":"2.0","method":"example","id":null}`,
		`{"jsonrpc":"2.0","method":"example","id":1,"result":null}`,
	} {
		if _, err := readRPC(bufio.NewReader(strings.NewReader(rpcFrame(body)))); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":"string-id","result":null}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`,
		`{"jsonrpc":"2.0","method":"notification"}`,
		`{"jsonrpc":"2.0","method":"request","id":"id"}`,
	} {
		if _, err := readRPC(bufio.NewReader(strings.NewReader(rpcFrame(body)))); err != nil {
			t.Fatal(body, err)
		}
	}
}

func TestMalformedResponseCannotSucceed(t *testing.T) {
	_, s, pipe := testServer("Go")
	pipe.onBody = func([]byte) { go s.read(bufio.NewReader(strings.NewReader(rpcFrame(`{"jsonrpc":"2.0","id":1}`)))) }
	if _, err := s.call("initialize", nil); err == nil {
		t.Fatal("malformed response reported success")
	}
	if !s.ended.Load() || s.ready.Load() || pendingCount(s) != 0 {
		t.Fatal("invalid peer survived")
	}
}

func TestShutdownKillsAndReapsChildren(t *testing.T) {
	s := subprocessServer("no-read")
	l := s.l
	l.servers["Go"] = s
	if _, err := s.launch(); err != nil {
		t.Fatal(err)
	}
	l.shutdown()
	select {
	case <-s.waitDone:
	default:
		t.Fatal("child survived shutdown")
	}
	if !s.ended.Load() {
		t.Fatal("connection survived shutdown")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if next := l.serverFor("Go"); next != nil {
		t.Fatal("server could activate after shutdown")
	}
}

func TestRPCRequestIDLimit(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"request","id":"` + strings.Repeat("x", 4096) + `"}`
	if _, err := readRPC(bufio.NewReader(strings.NewReader(rpcFrame(body)))); err == nil {
		t.Fatal("oversized request ID accepted")
	}
}

func TestRPCExactKeysForDispatchAndReply(t *testing.T) {
	for _, tc := range []struct{ frame, result, errorText string }{
		{`{"jsonrpc":"2.0","id":1,"ID":99,"result":"real","Result":"override"}`, `"real"`, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"real error","Message":"override"},"Error":null}`, "", "real error"},
		{`{"jsonrpc":"2.0","id":1,"method":"workspace/workspaceFolders","Method":"","Result":"fake"}`, "", "server went away"},
	} {
		t.Run(tc.frame, func(t *testing.T) {
			_, s, pipe := testServer("Go")
			defer s.stop()
			var once sync.Once
			pipe.onBody = func([]byte) { once.Do(func() { go s.read(bufio.NewReader(strings.NewReader(rpcFrame(tc.frame)))) }) }
			result, err := s.call("initialize", nil)
			if string(result) != tc.result || (tc.errorText == "" && err != nil) || (tc.errorText != "" && (err == nil || err.Error() != tc.errorText)) {
				t.Fatal(string(result), err)
			}
		})
	}
}
