package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	readHeaderTimeout = 5 * time.Second
	maxHeaderBytes    = 64 << 10
	maxRequestBody    = 1 << 20
)

// scope is what the mock keeps for one run: a listener, settings and a way to stop answering.
type scope struct {
	e        *Engine
	runID    string
	listener listenerSpec

	srv  *http.Server
	done chan struct{} // closed when the scope stops, so a delayed call gives up
	wg   sync.WaitGroup
	serv sync.WaitGroup

	mu       sync.Mutex
	settings map[string]any
}

func (e *Engine) openScope(runID string, l listenerSpec) (*scope, *refusal) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, open := e.scopes[runID]; open {
		return nil, &refusal{
			Message:    "A scope for this run is already open and the request was refused; close it first or use another run id. The open scope keeps answering.",
			ReasonCode: "run-already-open",
		}
	}
	ln, err := net.Listen("tcp", l.addr())
	if err != nil {
		return nil, &refusal{
			Message:    "The listener could not be opened (the address is taken or not available here) and the scope was not opened; ask for another port.",
			ReasonCode: "listener-unavailable",
		}
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		l.Port = tcp.Port // a requested port 0 is answered with the port that was bound
	}
	s := &scope{e: e, runID: runID, listener: l, done: make(chan struct{}), settings: e.defaultSettings()}
	s.srv = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: readHeaderTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	s.serv.Add(1)
	go func() {
		defer s.serv.Done()
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.log.printf("listener of run=%s stopped: %v", runID, err)
		}
	}()
	e.scopes[runID] = s
	e.publish(Event{RunID: runID, CorrelationID: newTraceID(), CorrelationSource: sourceGenerated, Kind: "scope.opened", Direction: "internal", Summary: "scope opened on " + l.addr()})
	e.log.printf("scope opened run=%s listener=%s", runID, l.addr())
	return s, nil
}

// stop closes the listener and every connection, and waits for calls in flight to end.
func (s *scope) stop() {
	close(s.done)
	_ = s.srv.Close() // closes the listener and the connections; the error is the listener's close error
	s.serv.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()
	waited := make(chan struct{})
	go func() { s.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-ctx.Done():
	}
}

func (e *Engine) defaultSettings() map[string]any {
	out := make(map[string]any, len(e.man.Settings))
	for _, d := range e.man.Settings {
		out[d.Name] = d.Default
	}
	return out
}

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = s.e.defaultSettings()
}

// applySettings sets the entries (already checked) and returns every setting with its current value.
func (s *scope) applySettings(body map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range body {
		s.settings[k] = v
	}
	out := make(map[string]any, len(s.settings))
	for k, v := range s.settings {
		out[k] = v
	}
	return out
}

func (s *scope) snapshot() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64, len(s.settings))
	for k, v := range s.settings {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}
