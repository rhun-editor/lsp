package main

import (
	"context"
	"errors"
	"time"
)

// fwcd starts compilation on a background executor before initialize completes.
// Queries on its separate request executor can corrupt that compiler's PSI state.
// Its first published diagnostics acknowledge completion of the startup analysis.
func (s *server) beginInitialAnalysis(uri string) {
	if s.lang != "Kotlin" || len(s.argv) == 0 || serverProgram(s.argv[0]) != "kotlin-language-server" {
		return
	}
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	if s.initialAnalysis == nil {
		s.initialAnalysis = make(chan struct{})
		s.initialAnalysisURI = uri
		s.initialAnalysisEnd = time.Now().Add(30 * time.Second)
	}
}

func (s *server) finishInitialAnalysis(uri string) {
	s.analysisMu.Lock()
	defer s.analysisMu.Unlock()
	if s.initialAnalysis != nil && s.initialAnalysisURI == uri {
		close(s.initialAnalysis)
		s.initialAnalysisURI = ""
	}
}

func (s *server) waitInitialAnalysis(ctx context.Context) error {
	s.analysisMu.Lock()
	ready, deadline := s.initialAnalysis, s.initialAnalysisEnd
	s.analysisMu.Unlock()
	if ready == nil {
		return nil
	}
	// Preserve compatibility with servers configured not to publish diagnostics.
	// Cancellation and process exit always interrupt this startup-only wait.
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ready:
		return nil
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("server went away during initial analysis")
	}
}
