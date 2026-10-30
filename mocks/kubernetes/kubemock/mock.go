// Package kubemock is a mock of a slice of the Kubernetes API (namespaces, pods,
// configmaps, deployments) that speaks mocks.adapter/v1: a control surface, one
// HTTP listener per scope, one in-memory cluster per scope, and events for
// everything it receives, sends and changes.
//
// It is written from the public behaviour of the Kubernetes API (API concepts:
// resource versions, list and watch, Status objects) and from
// spec/MOCK_ADAPTER_V1.md. It shares no code with any Kubernetes project.
package kubemock

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
	mockName    = "kubernetes-mock"
	mockVersion = "0.1.0"
	controlBase = "/_mock/v1"

	// DefaultRetainEvents is how many changes a cluster remembers for watch replay.
	DefaultRetainEvents = 256

	sseBuffer        = 1024
	idBytes          = 16
	eventTimeFormat  = "2006-01-02T15:04:05.000Z"
	readHeaderLimit  = 5 * time.Second
	controlBodyLimit = 1 << 16
)

// Config selects how a mock runs.
type Config struct {
	// ControlAddr is the listen address of the control endpoint, e.g. "127.0.0.1:9100".
	ControlAddr string
	// RetainEvents is the size of the watch replay window; 0 means DefaultRetainEvents.
	RetainEvents int
	// Log receives the mock's request log. It never receives a credential or a
	// value (see redact.go). Nil discards the log.
	Log io.Writer
}

// Mock is a running mock.
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

// Start binds the control endpoint and returns the running mock.
func Start(cfg Config) (*Mock, error) {
	if cfg.RetainEvents <= 0 {
		cfg.RetainEvents = DefaultRetainEvents
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
	m.srv = &http.Server{Handler: m.controlMux(), ReadHeaderTimeout: readHeaderLimit}
	go func() { _ = m.srv.Serve(ln) }() // Serve returns when Close is called; that error is the shutdown signal.
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
	_ = m.srv.Close() // best-effort shutdown; nothing to report to
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

type mockRef struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Contract string `json:"contract"`
}

// event is one mocks.event/v1 object.
type event struct {
	Schema            string  `json:"schema"`
	EventID           string  `json:"event_id"`
	At                string  `json:"at"`
	RunID             string  `json:"run_id"`
	CorrelationID     string  `json:"correlation_id"`
	CorrelationSource string  `json:"correlation_source"`
	Mock              mockRef `json:"mock"`
	Kind              string  `json:"kind"`
	Direction         string  `json:"direction"`
	Protocol          string  `json:"protocol"`
	Summary           string  `json:"summary"`
	Outcome           string  `json:"outcome,omitempty"`
	Fault             string  `json:"fault,omitempty"`
	LatencyMS         *int    `json:"latency_ms,omitempty"`
	Payload           any     `json:"payload,omitempty"`
}

// publish stamps one event and fans it out to every subscriber whose filter matches.
func (m *Mock) publish(ev event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev.Schema = "mocks.event/v1"
	ev.EventID = fmt.Sprintf("evt-%06d-%s", m.seq, randomHex(4))
	ev.At = time.Now().UTC().Format(eventTimeFormat)
	ev.Mock = mockRef{Name: mockName, Version: mockVersion, Contract: contractV1}
	ev.Protocol = "http"
	if ev.CorrelationID == "" {
		ev.CorrelationID, ev.CorrelationSource = randomHex(idBytes), "generated"
	}
	line, err := json.Marshal(ev)
	if err != nil {
		panic(err) // plain values only
	}
	for sub := range m.subs {
		if sub.filter != "" && sub.filter != ev.RunID {
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
