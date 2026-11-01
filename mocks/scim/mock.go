// Package scimmock is a SCIM 2.0 service provider (RFC 7643, RFC 7644) that speaks the
// mocks.adapter/v1 contract: a control surface, one HTTP listener per run, scenario
// settings and mocks.event/v1 events. It is written from the two RFCs and the adapter
// specification, and uses only the standard library.
package scimmock

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed mocks.manifest.json
var manifestFile []byte

const (
	contractV1  = "mocks.adapter/v1"
	mockName    = "scim-mock"
	defaultCtl  = "http://127.0.0.1:9100"
	controlBase = "/_mock/v1"
	eventTime   = "2006-01-02T15:04:05.000Z"
	sseBuffer   = 256
	idBytes     = 16
)

// Config selects where the control endpoint listens and where the mock logs.
type Config struct {
	ControlAddr string      // host:port of the control endpoint; loopback only
	Logger      *log.Logger // nil discards; lines carry method, path, status and ids, never a header or body value
}

// Mock is a running SCIM mock.
type Mock struct {
	cfg        Config
	log        *log.Logger
	ln         net.Listener
	srv        *http.Server
	controlURL string
	version    string
	declared   []settingDef
	now        func() time.Time

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
	host, _, err := net.SplitHostPort(cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("the control address %q is not host:port and was refused; pass one such as 127.0.0.1:9100", cfg.ControlAddr)
	}
	if !isLoopback(host) {
		return nil, fmt.Errorf("the control address %q is not a loopback address and was refused; this mock serves loopback only, use 127.0.0.1", cfg.ControlAddr)
	}
	m := &Mock{cfg: cfg, log: cfg.Logger, now: time.Now, scopes: map[string]*scope{}, subs: map[*subscriber]struct{}{}}
	if m.log == nil {
		m.log = log.New(io.Discard, "", 0)
	}
	decl, version, err := parseManifest(manifestFile)
	if err != nil {
		return nil, err
	}
	m.declared, m.version = decl, version
	ln, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return nil, err
	}
	m.ln = ln
	m.controlURL = "http://" + ln.Addr().String()
	m.srv = &http.Server{Handler: m.controlMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = m.srv.Serve(ln) }() // Serve returns on Close; its error is the shutdown signal.
	return m, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ControlURL is the base URL of the control endpoint.
func (m *Mock) ControlURL() string { return m.controlURL }

// Manifest is the manifest this mock serves: the embedded file with its control URL
// replaced by the address actually bound.
func (m *Mock) Manifest() []byte {
	return []byte(strings.Replace(string(manifestFile), defaultCtl, m.controlURL, 1))
}

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

// publish stamps one event and fans it out to every subscriber whose filter matches.
func (m *Mock) publish(runID, kind, direction, summary string, extra map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	if len(summary) > 500 {
		summary = summary[:500]
	}
	ev := map[string]any{
		"schema":             "mocks.event/v1",
		"event_id":           fmt.Sprintf("evt-%06d-%s", m.seq, randomHex(4)),
		"at":                 m.now().UTC().Format(eventTime),
		"run_id":             runID,
		"correlation_id":     randomHex(idBytes),
		"correlation_source": "generated",
		"mock":               map[string]any{"name": mockName, "version": m.version, "contract": contractV1},
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

// refusal is the Refusal document of the adapter specification.
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
