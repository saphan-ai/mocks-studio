// Package dnsmock is a DNS and DNS-over-HTTPS filtering resolver mock that speaks
// mocks.adapter/v1 (spec/MOCK_ADAPTER_V1.md). A run's scope is one loopback port that answers
// DNS over UDP, DNS over TCP and DoH (RFC 8484, cleartext HTTP) with that run's declared records
// and blocked names; every query is an event.
package dnsmock

import (
	"crypto/rand"
	_ "embed" // the manifest file is embedded so the served manifest is that file
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed mocks.manifest.json
var manifestFile []byte

const (
	contractV1  = "mocks.adapter/v1"
	mockName    = "dns-mock"
	sseBuffer   = 256
	idBytes     = 16
	eventTime   = "2006-01-02T15:04:05.000Z"
	controlBase = "/_mock/v1"
)

// Config selects where the mock listens.
type Config struct {
	ControlAddr string    // listen address of the control endpoint, a loopback host:port
	Log         io.Writer // where the mock logs; nil discards. Nothing a caller sends as a credential is written to it.
}

// Mock is a running mock.
type Mock struct {
	ln         net.Listener
	srv        *http.Server
	controlURL string
	log        *log.Logger
	defs       map[string]settingDef
	defOrder   []string
	version    string
	manifest   map[string]any

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
	host, _, err := net.SplitHostPort(cfg.ControlAddr)
	if err != nil {
		return nil, fmt.Errorf("control address %q is not host:port: %w", cfg.ControlAddr, err)
	}
	if !isLoopback(host) {
		return nil, fmt.Errorf("control address %q is not on loopback; this mock binds loopback only", cfg.ControlAddr)
	}
	var doc map[string]any
	if err := json.Unmarshal(manifestFile, &doc); err != nil {
		return nil, fmt.Errorf("embedded manifest does not decode: %w", err)
	}
	var withSettings struct {
		Version  string       `json:"version"`
		Settings []settingDef `json:"settings"`
	}
	if err := json.Unmarshal(manifestFile, &withSettings); err != nil {
		return nil, fmt.Errorf("embedded manifest settings do not decode: %w", err)
	}
	m := &Mock{scopes: map[string]*scope{}, subs: map[*subscriber]struct{}{}, defs: map[string]settingDef{}, version: withSettings.Version, manifest: doc}
	for _, d := range withSettings.Settings {
		m.defs[d.Name] = d
		m.defOrder = append(m.defOrder, d.Name)
	}
	w := cfg.Log
	if w == nil {
		w = io.Discard
	}
	m.log = log.New(w, "dns-mock: ", log.LstdFlags|log.Lmicroseconds)
	ln, err := net.Listen("tcp", cfg.ControlAddr)
	if err != nil {
		return nil, err
	}
	m.ln = ln
	m.controlURL = "http://" + ln.Addr().String()
	m.srv = &http.Server{Handler: m.controlMux(), ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
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

// manifestBytes is the manifest file; control.url is rewritten only when the control endpoint is
// not where the file says, so that the served manifest never points the core at a dead address.
func (m *Mock) manifestBytes() []byte {
	doc := map[string]any{}
	for k, v := range m.manifest {
		doc[k] = v
	}
	control, _ := doc["control"].(map[string]any)
	if control["url"] == m.controlURL {
		return manifestFile
	}
	doc["control"] = map[string]any{"url": m.controlURL}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // plain values decoded from JSON always marshal
	}
	return append(b, '\n')
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// publish stamps and fans out one event to every subscriber whose filter matches.
func (m *Mock) publish(runID, kind, direction, protocol, summary string, extra map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev := map[string]any{
		"schema":             "mocks.event/v1",
		"event_id":           fmt.Sprintf("evt-%06d-%s", m.seq, randomHex(4)),
		"at":                 time.Now().UTC().Format(eventTime),
		"run_id":             runID,
		"correlation_id":     randomHex(idBytes),
		"correlation_source": "generated",
		"mock":               map[string]any{"name": mockName, "version": m.version, "contract": contractV1},
		"kind":               kind,
		"direction":          direction,
		"protocol":           protocol,
		"summary":            truncateSummary(summary),
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

func truncateSummary(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit-3] + "..."
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client hanging up is not the mock's problem
}

// refuse writes a mocks refusal document.
func (m *Mock) refuse(w http.ResponseWriter, status int, code, message string, allowed []string) {
	doc := map[string]any{"message": message, "reason_code": code}
	if len(allowed) > 0 {
		doc["allowed"] = allowed
	}
	writeJSON(w, status, doc)
}
