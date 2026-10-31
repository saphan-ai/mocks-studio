// Package s3mock is a free S3-compatible object-store mock that speaks mocks.adapter/v1:
// buckets, objects, multipart upload, presigned URLs and AWS Signature Version 4,
// with slow reads, SlowDown throttling, forced errors and missing keys as settings.
package s3mock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	contractV1  = "mocks.adapter/v1"
	mockName    = "s3-mock"
	mockVersion = "0.1.0"
	controlBase = "/_mock/v1"
	eventTime   = "2006-01-02T15:04:05.000Z"
	sseBuffer   = 256
)

// Config selects where the control endpoint listens and where the access log goes.
type Config struct {
	ControlAddr string    // e.g. "127.0.0.1:9100"
	Log         io.Writer // one line per request; nil discards
	Now         func() time.Time
}

// Mock is a running S3 mock.
type Mock struct {
	cfg        Config
	ln         net.Listener
	srv        *http.Server
	controlURL string

	mu     sync.Mutex
	scopes map[string]*scope
	subs   map[*subscriber]struct{}
	seq    int
}

type subscriber struct {
	filter string
	ch     chan []byte
}

// Start starts the control endpoint and returns the running mock.
func Start(cfg Config) (*Mock, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	ln, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return nil, err
	}
	m := &Mock{cfg: cfg, ln: ln, scopes: map[string]*scope{}, subs: map[*subscriber]struct{}{}}
	m.controlURL = "http://" + ln.Addr().String()
	m.srv = &http.Server{Handler: m.controlMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.srv.Serve(ln) }() // Serve returns on Close; its error is the shutdown signal.
	return m, nil
}

// ControlURL is the base URL of the control endpoint.
func (m *Mock) ControlURL() string { return m.controlURL }

// Close stops the control endpoint and every open scope.
func (m *Mock) Close() {
	m.mu.Lock()
	scopes := make([]*scope, 0, len(m.scopes))
	for _, s := range m.scopes {
		scopes = append(scopes, s)
	}
	m.scopes = map[string]*scope{}
	m.mu.Unlock()
	for _, s := range scopes {
		s.stop()
	}
	_ = m.srv.Close() // best-effort shutdown
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// publish stamps and fans out one event to every subscriber whose filter matches.
func (m *Mock) publish(runID, kind, direction, summary string, extra map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev := map[string]any{
		"schema":             "mocks.event/v1",
		"event_id":           fmt.Sprintf("evt-%06d-%s", m.seq, randomHex(4)),
		"at":                 m.cfg.Now().UTC().Format(eventTime),
		"run_id":             runID,
		"correlation_id":     randomHex(16),
		"correlation_source": "generated",
		"mock":               map[string]any{"name": mockName, "version": mockVersion, "contract": contractV1},
		"kind":               kind,
		"direction":          direction,
		"protocol":           "http",
		"summary":            summary,
	}
	for k, v := range extra {
		ev[k] = v
	}
	line, err := json.Marshal(ev)
	if err != nil {
		panic(err) // plain values only
	}
	for sub := range m.subs {
		if sub.filter != "" && sub.filter != runID {
			continue
		}
		select {
		case sub.ch <- line:
		default: // a subscriber that stops reading loses events rather than stalling the mock
		}
	}
}

func (m *Mock) subscribe(filter string) *subscriber {
	sub := &subscriber{filter: filter, ch: make(chan []byte, sseBuffer)}
	m.mu.Lock()
	m.subs[sub] = struct{}{}
	m.mu.Unlock()
	return sub
}

func (m *Mock) unsubscribe(sub *subscriber) {
	m.mu.Lock()
	delete(m.subs, sub)
	m.mu.Unlock()
}

// refusal is the Refusal document of the specification.
type refusal struct {
	Message    string   `json:"message"`
	ReasonCode string   `json:"reason_code"`
	Allowed    []string `json:"allowed,omitempty"`
}

func refuse(w http.ResponseWriter, status int, code, message string, allowed []string) {
	writeJSON(w, status, refusal{Message: message, ReasonCode: code, Allowed: allowed})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client hanging up mid-write is not the mock's problem
}
