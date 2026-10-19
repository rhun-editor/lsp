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
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxRPCHeader     = 8 << 10
	maxRPCBody       = 16 << 20
	maxPending       = 256
	writeTimeout     = 2 * time.Second
	exitDrainTimeout = 500 * time.Millisecond
)

// Own both pipe pairs: Cmd.Wait must reap immediately without closing stdout before
// the reader has delivered responses written immediately before the child exited.
func (s *server) launch() (io.ReadCloser, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.ended.Load() {
		return nil, errors.New("server stopped before launch")
	}
	input, in, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	out, output, err := os.Pipe()
	if err != nil {
		input.Close()
		in.Close()
		return nil, err
	}
	cmd := exec.Command(s.argv[0], s.argv[1:]...)
	cmd.Dir, cmd.Stderr = s.l.project, os.Stderr
	cmd.Stdin, cmd.Stdout = input, output
	err = cmd.Start()
	input.Close()
	output.Close()
	if err != nil {
		in.Close()
		out.Close()
		return nil, err
	}
	s.cmd, s.in, s.out = cmd, in, out
	go func() {
		cmd.Wait()
		s.stateMu.Lock()
		s.processExited.Store(true)
		s.ready.Store(false)
		s.stateMu.Unlock()
		close(s.waitDone)
		timer := time.NewTimer(exitDrainTimeout)
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
			s.endServer(s.name() + " exited (stdout drain timed out)")
		}
	}()
	return out, nil
}

func (s *server) endServer(reason string) {
	s.stateMu.Lock()
	if s.ended.Load() {
		s.stateMu.Unlock()
		return
	}
	s.ended.Store(true)
	s.ready.Store(false)
	close(s.done)
	in, out, cmd := s.in, s.out, s.cmd
	s.stateMu.Unlock()
	s.cancelCommandCapture()
	// Kill before closing the pipes so a child blocked on I/O cannot hold cleanup.
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
	if in != nil {
		in.Close()
	}
	if out != nil {
		out.Close()
	}
	s.pmu.Lock()
	for id, ch := range s.pending {
		delete(s.pending, id)
		close(ch)
	}
	s.pmu.Unlock()
	if reason == "" {
		reason = s.name() + " exited"
	}
	s.status(reason, true)
	s.l.scopeChanged()
}

func readRPC(br *bufio.Reader) ([]byte, error) {
	length, total := -1, 0
	for {
		line, err := br.ReadSlice('\n')
		total += len(line)
		if total > maxRPCHeader || err == bufio.ErrBufferFull {
			return nil, errors.New("LSP header exceeds 8 KiB")
		}
		if err != nil {
			return nil, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			break
		}
		key, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return nil, errors.New("malformed LSP header")
		}
		if strings.EqualFold(key, "Content-Length") {
			value = strings.TrimSpace(value)
			if length >= 0 || value == "" {
				return nil, errors.New("duplicate or empty Content-Length")
			}
			for _, c := range value {
				if c < '0' || c > '9' {
					return nil, errors.New("invalid Content-Length")
				}
			}
			n, err := strconv.ParseUint(value, 10, 32)
			if err != nil || n > maxRPCBody {
				return nil, errors.New("LSP message exceeds 16 MiB")
			}
			length = int(n)
		}
	}
	if length < 0 {
		return nil, errors.New("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	var envelope map[string]json.RawMessage
	var version string
	if !utf8.Valid(body) || json.Unmarshal(body, &envelope) != nil || envelope == nil || json.Unmarshal(envelope["jsonrpc"], &version) != nil || version != "2.0" {
		return nil, errors.New("invalid JSON-RPC 2.0 message")
	}
	if err := validateEnvelope(envelope); err != nil {
		return nil, err
	}
	return body, nil
}

func validRPCID(raw json.RawMessage, allowNull bool) bool {
	if bytes.Equal(raw, []byte("null")) {
		return allowNull
	}
	if len(raw) == 0 || len(raw) > 4096 {
		return false
	}
	if raw[0] == '"' {
		var id string
		return json.Unmarshal(raw, &id) == nil
	}
	var id int32
	return json.Unmarshal(raw, &id) == nil
}

func validateEnvelope(msg map[string]json.RawMessage) error {
	invalid := errors.New("invalid JSON-RPC request or response envelope")
	_, hasID := msg["id"]
	_, hasResult := msg["result"]
	_, hasError := msg["error"]
	if method, hasMethod := msg["method"]; hasMethod {
		var name string
		if json.Unmarshal(method, &name) != nil || name == "" || len(name) > 4096 || hasResult || hasError || (hasID && !validRPCID(msg["id"], false)) {
			return invalid
		}
		return nil
	}
	if !hasID || hasResult == hasError || !validRPCID(msg["id"], hasError) {
		return invalid
	}
	if hasError {
		var errObject map[string]json.RawMessage
		var code int32
		var message string
		if json.Unmarshal(msg["error"], &errObject) != nil || errObject == nil ||
			len(errObject["code"]) == 0 || bytes.Equal(errObject["code"], []byte("null")) || json.Unmarshal(errObject["code"], &code) != nil ||
			len(errObject["message"]) == 0 || bytes.Equal(errObject["message"], []byte("null")) || json.Unmarshal(errObject["message"], &message) != nil {
			return invalid
		}
	}
	return nil
}

// serverError is the error a server answered a request with
type serverError struct {
	Code    int
	Message string
}

func (e *serverError) Error() string { return e.Message }

// LSP's error codes for a request the server dropped because it was cancelled or its document
// changed meanwhile
const (
	codeRequestCancelled = -32800
	codeContentModified  = -32801
)

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (s *server) write(msg any) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return s.writeContext(ctx, msg)
}

func (s *server) writeContext(parent context.Context, msg any) error {
	return s.writeContextGuard(parent, msg, nil)
}

func (s *server) writeContextGuard(parent context.Context, msg any, guard func() error) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(data) > maxRPCBody {
		return errors.New("outbound LSP message exceeds 16 MiB")
	}
	ctx, cancel := context.WithTimeout(parent, writeTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("server went away")
	case s.writeGate <- struct{}{}:
	}
	s.stateMu.Lock()
	in, ended := s.in, s.ended.Load()
	s.stateMu.Unlock()
	if in == nil || ended || ctx.Err() != nil {
		<-s.writeGate
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("server not running")
	}
	if guard != nil {
		if err := guard(); err != nil {
			<-s.writeGate
			return err
		}
	}
	ack := make(chan error, 1)
	go func() {
		err := writeAll(in, []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))))
		if err == nil {
			err = writeAll(in, data)
		}
		ack <- err
		<-s.writeGate
	}()
	select {
	case err := <-ack:
		if err != nil {
			s.endServer(s.name() + ": write failed: " + err.Error())
		}
		return err
	case <-ctx.Done():
		// If the writer finished concurrently, no teardown is necessary.
		select {
		case err := <-ack:
			return err
		default:
		}
		s.endServer(s.name() + ": write timed out")
		return ctx.Err()
	case <-s.done:
		select {
		case err := <-ack:
			return err
		default:
		}
		return errors.New("server went away")
	}
}

func (s *server) notify(method string, params any) {
	if err := s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil && s.l.ext != nil {
		s.l.ext.Logf("%s: %v", method, err)
	}
}

func (s *server) call(method string, params any) (json.RawMessage, error) {
	return s.callFor(15*time.Second, method, params)
}

func (s *server) callFor(wait time.Duration, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return s.callContext(ctx, method, params)
}

func (s *server) queueCancel(id int) {
	s.cancelOnce.Do(func() {
		go func() {
			for {
				select {
				case <-s.done:
					return
				case id := <-s.cancels:
					ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
					s.writeContext(ctx, map[string]any{"jsonrpc": "2.0", "method": "$/cancelRequest", "params": map[string]any{"id": id}})
					cancel()
				}
			}
		}()
	})
	select {
	case s.cancels <- id:
	default:
	}
}

func (s *server) callContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return s.callContextGuard(ctx, method, params, nil)
}

func (s *server) callContextGuard(ctx context.Context, method string, params any, guard func() error) (json.RawMessage, error) {
	return s.callContextRegistered(ctx, method, params, guard, nil)
}

func (s *server) callContextRegistered(ctx context.Context, method string, params any, guard func() error, registered func(int)) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.pmu.Lock()
	if s.ended.Load() || len(s.pending) >= maxPending {
		s.pmu.Unlock()
		return nil, errors.New("server unavailable or request limit reached")
	}
	s.nextID++
	id := s.nextID
	ch := make(chan json.RawMessage, 1)
	s.pending[id] = ch
	s.pmu.Unlock()
	if registered != nil {
		registered(id)
	}
	defer func() { s.pmu.Lock(); delete(s.pending, id); s.pmu.Unlock() }()
	var res json.RawMessage
	if err := s.writeContextGuard(ctx, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}, guard); err != nil {
		// The peer can reply and close before the physical writer reports completion.
		select {
		case res = <-ch:
		default:
		}
		if len(res) == 0 {
			return nil, err
		}
	}
	if res == nil {
		select {
		case res = <-ch:
		case <-ctx.Done():
			select {
			case res = <-ch:
			default:
				s.queueCancel(id)
				return nil, ctx.Err()
			}
		case <-s.done:
			// Reader removes and buffers a response before signaling EOF. Preserve it.
			select {
			case res = <-ch:
			default:
			}
		}
	}
	if len(res) == 0 {
		return nil, errors.New("server went away")
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(res, &reply); err != nil {
		return nil, err
	}
	if raw, ok := reply["error"]; ok {
		var detail map[string]json.RawMessage
		var message string
		if json.Unmarshal(raw, &detail) != nil || json.Unmarshal(detail["message"], &message) != nil {
			return nil, errors.New("invalid JSON-RPC error")
		}
		var code int
		json.Unmarshal(detail["code"], &code)
		return nil, &serverError{Code: code, Message: message}
	}
	return reply["result"], nil
}
