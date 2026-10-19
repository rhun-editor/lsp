package main

// Streaming answers (feature query-stream): a request that may run long (references,
// implementations, the definition-likes, workspace symbols, hierarchy calls) goes to the server
// with a partialResultToken and a workDoneToken of the extension's own. The server's $/progress
// notifications for them become parts of rhun's request and progress reports on it, in the order
// they came and before the final answer; each one extends the time the extension waits, as it
// extends rhun's. When rhun withdraws its request (0 cancel, or cancelled or toobig in reply to a
// part) the request's context ends and the server gets $/cancelRequest.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// queryLimits is how long the extension waits for a kind: base, extended by base after each part
// or progress report, up to ceiling (rhun's limits of the kind, a little less)
type queryLimits struct{ base, ceiling time.Duration }

// feedItem is a $/progress value for one of the extension's tokens
type feedItem struct {
	partial bool // a partial result; else a work-done report
	value   json.RawMessage
}

// queryFeed collects the $/progress values of one request's tokens, in the order they came
type queryFeed struct {
	mu    sync.Mutex
	items []feedItem
	wake  chan struct{}
}

func (f *queryFeed) push(it feedItem) {
	f.mu.Lock()
	f.items = append(f.items, it)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *queryFeed) take() []feedItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	items := f.items
	f.items = nil
	return items
}

var tokenSerial atomic.Uint64

// openFeed makes a partial-result token and a work-done token for one request
func (s *server) openFeed() (string, string, *queryFeed) {
	n := strconv.FormatUint(tokenSerial.Add(1), 10)
	partial, work := "rhun-partial-"+n, "rhun-work-"+n
	f := &queryFeed{wake: make(chan struct{}, 1)}
	s.tokenMu.Lock()
	if s.feeds == nil {
		s.feeds = map[string]*queryFeed{}
	}
	s.feeds["s:"+partial] = f
	s.feeds["s:"+work] = f
	s.tokenMu.Unlock()
	return partial, work, f
}

func (s *server) closeFeed(partial, work string) {
	s.tokenMu.Lock()
	delete(s.feeds, "s:"+partial)
	delete(s.feeds, "s:"+work)
	s.tokenMu.Unlock()
}

// feedProgress takes a $/progress notification for one of the extension's request tokens (and
// reports whether it was one); others are the server's own work-done progress
func (s *server) feedProgress(params json.RawMessage) bool {
	var p struct {
		Token json.RawMessage `json:"token"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	key, _, ok := progressToken(p.Token)
	if !ok {
		return false
	}
	s.tokenMu.Lock()
	f := s.feeds[key]
	s.tokenMu.Unlock()
	if f == nil {
		return false
	}
	text, _ := jsonString(p.Token)
	f.push(feedItem{partial: len(text) > 13 && text[:13] == "rhun-partial-", value: p.Value})
	return true
}

// workDone reads a work-done report: its percentage (-1 for none) and its text
type workReport struct {
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	Percentage *int   `json:"percentage"`
}

// streamQuery sends a request of q's server and forwards what the server streams for it: partial
// results through convert and send as parts of out (when rhun agreed query-stream and takes
// them; otherwise they are carried into the answer), work-done reports as out's progress. It
// returns the final result and the carried items.
func streamQuery[T any](l *lsp, q *documentQuery, out *rhunext.Stream, lim queryLimits, method string, params map[string]any,
	convert func(json.RawMessage) []T, send func(*rhunext.Stream, []T) error) (json.RawMessage, []T, error) {
	parent := context.Background()
	if out != nil && out.Context != nil {
		parent = out.Context
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	start := time.Now()
	timer := time.AfterFunc(lim.base, cancel)
	defer timer.Stop()
	extend := func() {
		if left := lim.ceiling - time.Since(start); left > 0 {
			timer.Reset(min(lim.base, left))
		}
	}
	streaming := out != nil && l.has(rhunext.FeatureQueryStream)
	var feed *queryFeed
	var wake chan struct{}
	if streaming {
		partial, work, f := q.server.openFeed()
		defer q.server.closeFeed(partial, work)
		params["partialResultToken"] = partial
		params["workDoneToken"] = work
		feed, wake = f, f.wake
	}
	type answer struct {
		res json.RawMessage
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := q.requestContext(ctx, method, params)
		done <- answer{res, err}
	}()
	var carried []T
	parts := streaming
	title := ""
	forward := func() {
		for _, it := range feed.take() {
			if !it.partial {
				var w workReport
				if json.Unmarshal(it.value, &w) != nil || w.Kind == "end" {
					continue
				}
				if w.Title != "" {
					title = w.Title
				}
				percent := -1
				if w.Percentage != nil {
					percent = *w.Percentage
				}
				text := title
				if w.Message != "" {
					if text != "" {
						text += " "
					}
					text += w.Message
				}
				if ctx.Err() == nil && out.Progress(percent, statusText(text)) == nil {
					extend()
				}
				continue
			}
			items := convert(it.value)
			if len(items) == 0 {
				continue
			}
			if !parts || ctx.Err() != nil {
				carried = append(carried, items...)
				continue
			}
			err := send(out, items)
			var re *rhunext.Error
			switch {
			case err == nil:
				extend()
			case errors.As(err, &re) && re.Code == "syntax":
				// rhun dropped this part (counted); the query goes on
			case errors.Is(err, rhunext.ErrNoStream) || errors.As(err, &re) && re.Code == "refused":
				parts = false
				carried = append(carried, items...)
			default:
				// cancelled, toobig or gone: rhun keeps what it has
				cancel()
			}
		}
	}
	for {
		select {
		case a := <-done:
			if feed != nil {
				forward()
			}
			return a.res, carried, a.err
		case <-wake:
			forward()
		}
	}
}

// partChunks splits items into parts rhun takes whole: at most 5000 records and about 900 KB each
func partChunks[T any](items []T, size func(T) int) [][]T {
	var out [][]T
	from, bytes := 0, 0
	for i, it := range items {
		n := size(it)
		if i > from && (i-from >= 5000 || bytes+n > 900<<10) {
			out = append(out, items[from:i])
			from, bytes = i, 0
		}
		bytes += n
	}
	if from < len(items) {
		out = append(out, items[from:])
	}
	return out
}

// streamRest sends all but the last chunk of a long answer as parts (when rhun takes parts) and
// returns what is left for the answer itself
func streamRest[T any](l *lsp, out *rhunext.Stream, items []T, size func(T) int, send func(*rhunext.Stream, []T) error) []T {
	if out == nil || !l.has(rhunext.FeatureQueryStream) {
		return items
	}
	chunks := partChunks(items, size)
	for len(chunks) > 1 {
		if err := send(out, chunks[0]); err != nil {
			var re *rhunext.Error
			if errors.As(err, &re) && re.Code == "syntax" {
				chunks = chunks[1:]
				continue
			}
			if errors.Is(err, rhunext.ErrNoStream) || errors.As(err, &re) && re.Code == "refused" {
				break
			}
			return nil
		}
		chunks = chunks[1:]
	}
	var rest []T
	for _, c := range chunks {
		rest = append(rest, c...)
	}
	return rest
}
