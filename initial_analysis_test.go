package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestKotlinStartupQueriesWaitForFirstDocumentAnalysis(t *testing.T) {
	l, s, d, pipe := syncHarness(t, synchronizationOptions{openClose: true, change: 1})
	s.lang, s.argv, d.lang = "Kotlin", []string{"/tools/kotlin-language-server"}, "Kotlin"
	if !l.ensureSynchronized(d, s) {
		t.Fatal("open failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := s.callContext(ctx, "textDocument/hover", nil); result <- err }()
	s.finishInitialAnalysis("file:///another.kt")
	select {
	case err := <-result:
		t.Fatal("query returned before the initial analysis", err)
	case <-time.After(25 * time.Millisecond):
	}
	if len(pipe.messages(t)) != 1 || pendingCount(s) != 0 {
		t.Fatal("query reached the server before analysis completed")
	}
	s.finishInitialAnalysis(fileURI(d.path))
	eventually(t, func() bool { return len(pipe.messages(t)) == 2 })
	// Duplicate diagnostics and later opens must not close twice or rearm the gate.
	s.finishInitialAnalysis(fileURI(d.path))
	s.beginInitialAnalysis("file:///second.kt")
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInitialAnalysisWaitCanBeCanceledStoppedOrExpire(t *testing.T) {
	for _, mode := range []string{"cancel", "stop", "expire"} {
		t.Run(mode, func(t *testing.T) {
			_, s, _ := testServer("Kotlin")
			s.argv = []string{"kotlin-language-server"}
			s.beginInitialAnalysis("file:///main.kt")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "cancel":
				cancel()
			case "stop":
				close(s.done)
			case "expire":
				s.initialAnalysisEnd = time.Now().Add(-time.Second)
			}
			start := time.Now()
			err := s.waitInitialAnalysis(ctx)
			if time.Since(start) > time.Second || (mode == "expire") != (err == nil) {
				t.Fatal(mode, err)
			}
		})
	}
	for _, argv := range [][]string{{"kotlin-lsp"}, {"other"}, nil} {
		_, s, _ := testServer("Kotlin")
		s.argv = argv
		s.beginInitialAnalysis("file:///main.kt")
		if s.initialAnalysis != nil {
			t.Fatal("applied the fwcd workaround to another server", argv)
		}
	}
}
