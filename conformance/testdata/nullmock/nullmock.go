// Package nullmock is the conformance suite's own fixture: a mock that speaks
// mocks.adapter/v1 and does nothing else. In its correct form it passes every check;
// each Mutant breaks exactly one rule so a test can prove the suite notices.
//
// It is written from the specification alone (spec/MOCK_ADAPTER_V1.md) and shares no
// code with the runner. Modes: "http" (one declared setting, a forced status code)
// and "tcp" (a line-echo protocol with no headers, which proves the run comes from
// the listener and from nothing else).
package nullmock

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Modes the fixture runs in.
const (
	ModeHTTP = "http"
	ModeTCP  = "tcp"
)

// Mutant names. The empty mutant is the correct mock.
const (
	MutantNone                     = ""
	MutantManifestMissingName      = "manifest-missing-name"
	MutantContractV2               = "contract-v2"
	MutantHealthFailing            = "health-failing"
	MutantServedManifestDiffers    = "served-manifest-differs"
	MutantEventsWrongContentType   = "events-wrong-content-type"
	MutantScopeOpenRefused         = "scope-open-refused"
	MutantListenerSilent           = "listener-silent"
	MutantAcceptsInvalidRunID      = "accepts-invalid-run-id"
	MutantMergesDuplicateScope     = "merges-duplicate-scope"
	MutantAcceptsUnsupportedProto  = "accepts-unsupported-protocol"
	MutantEventMissingRunID        = "event-missing-run-id"
	MutantNoScopeClosedEvent       = "no-scope-closed-event"
	MutantTraceparentIgnored       = "traceparent-ignored"
	MutantGeneratedLabelledHeader  = "generated-labelled-traceparent"
	MutantMismatchNotRecorded      = "mismatch-not-recorded"
	MutantScopesLeakEvents         = "scopes-leak-events"
	MutantFilterIgnored            = "filter-ignored"
	MutantUnknownSettingAccepted   = "unknown-setting-accepted"
	MutantRefusalWithoutReasonCode = "refusal-without-reason-code"
	MutantUnknownSettingNoAllowed  = "unknown-setting-no-allowed"
	MutantOutOfRangeAccepted       = "out-of-range-accepted"
	MutantWrongTypeAccepted        = "wrong-type-accepted"
	MutantSettingNoEffect          = "setting-no-effect"
	MutantNoScenarioSetEvent       = "no-scenario-set-event"
	MutantResetKeepsSettings       = "reset-keeps-settings"
	MutantNoScopeResetEvent        = "no-scope-reset-event"
	MutantNonSafeFieldInClear      = "non-safe-field-in-clear"
	MutantCredentialInSummary      = "credential-in-summary"
	MutantListenerStaysOpen        = "listener-stays-open"
	MutantResponseSentInvalid      = "response-sent-invalid"
)

const (
	contractV1  = "mocks.adapter/v1"
	mockName    = "null-mock"
	mockVersion = "0.1.0"
	sseBuffer   = 256
	idBytes     = 16
	eventTime   = "2006-01-02T15:04:05.000Z"
	controlBase = "/_mock/v1"
)

// Config selects what the fixture serves.
type Config struct {
	Mode        string // ModeHTTP or ModeTCP
	Mutant      string // one of the Mutant constants
	ControlAddr string // listen address of the control endpoint, e.g. "127.0.0.1:0"
}

// Mock is a running fixture.
type Mock struct {
	cfg        Config
	ln         net.Listener
	srv        *http.Server
	controlURL string

	mu     sync.Mutex
	scopes map[string]*scope
	order  []string // run ids in the order they were opened
	subs   map[*subscriber]struct{}
	seq    int
}

type subscriber struct {
	filter string
	ch     chan []byte
}

// Start starts the control endpoint and returns the running mock.
func Start(cfg Config) (*Mock, error) {
	if cfg.Mode != ModeHTTP && cfg.Mode != ModeTCP {
		return nil, fmt.Errorf("mode must be %q or %q, got %q", ModeHTTP, ModeTCP, cfg.Mode)
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
	_ = m.srv.Close() // best-effort shutdown of a test fixture
}

// Manifest returns the manifest this mock publishes, with any manifest mutant applied.
func (m *Mock) Manifest() []byte { return m.manifestBytes(m.cfg.Mutant) }

// FileManifest returns the manifest an operator would have written to disk. It is the
// served manifest except for the one mutant that makes the two disagree.
func (m *Mock) FileManifest() []byte {
	if m.cfg.Mutant == MutantServedManifestDiffers {
		return m.manifestBytes(MutantNone)
	}
	return m.manifestBytes(m.cfg.Mutant)
}

func (m *Mock) manifestBytes(mutant string) []byte {
	return ManifestBytes(m.cfg.Mode, mutant, m.controlURL)
}

// ManifestBytes renders the fixture's manifest for a control URL without starting
// anything, with a manifest mutant applied when one is named.
func ManifestBytes(mode, mutant, controlURL string) []byte {
	doc := baseManifest(mode, controlURL)
	switch mutant {
	case MutantManifestMissingName:
		delete(doc, "name")
	case MutantContractV2:
		doc["contract"] = "mocks.adapter/v2"
	case MutantServedManifestDiffers:
		doc["version"] = "9.9.9"
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // a map of plain values always marshals; a failure here is a fixture bug
	}
	return append(b, '\n')
}

func baseManifest(mode, controlURL string) map[string]any {
	doc := map[string]any{
		"contract":    contractV1,
		"name":        mockName,
		"version":     mockVersion,
		"description": "The conformance suite's null mock: it answers, records and does nothing else.",
		"control":     map[string]any{"url": controlURL},
	}
	switch mode {
	case ModeHTTP:
		doc["protocols"] = []string{"http"}
		doc["safe_fields"] = []string{"method", "path"}
		doc["settings"] = []any{map[string]any{
			"name": "status_code", "type": "integer", "default": 200, "minimum": 100, "maximum": 599, "scope": "run",
			"description": "HTTP status the listener answers with.",
			"effect":      map[string]any{"protocol": "http", "value": 503, "expect": map[string]any{"http_status": 503}},
		}}
		doc["conformance"] = map[string]any{"probe": map[string]any{"http": map[string]any{"method": "POST", "path": "/probe", "body": "hello"}}}
	default:
		doc["protocols"] = []string{"tcp"}
		doc["safe_fields"] = []string{"length"}
		doc["settings"] = []any{map[string]any{
			"name": "reply_mode", "type": "string", "default": "echo", "allowed": []string{"echo", "upper"}, "scope": "run",
			"description": "Whether the line is echoed as received or in upper case.",
			"effect":      map[string]any{"protocol": "tcp", "value": "upper", "expect": map[string]any{"tcp_reply": "PING"}},
		}}
		doc["conformance"] = map[string]any{"probe": map[string]any{"tcp": map[string]any{"send": "ping"}}}
	}
	return doc
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable for a fixture
	}
	return hex.EncodeToString(b)
}

// isMutant reports whether the named single-rule break is active.
func (m *Mock) isMutant(name string) bool { return m.cfg.Mutant == name }

// publish stamps and fans out one event to every subscriber whose filter matches.
func (m *Mock) publish(runID, kind, direction, protocol, summary string, extra map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	stamped := runID
	if m.isMutant(MutantScopesLeakEvents) && len(m.order) > 1 {
		stamped = m.order[0]
	}
	ev := map[string]any{
		"schema":             "mocks.event/v1",
		"event_id":           fmt.Sprintf("evt-%06d-%s", m.seq, randomHex(4)),
		"at":                 time.Now().UTC().Format(eventTime),
		"run_id":             stamped,
		"correlation_id":     randomHex(idBytes),
		"correlation_source": "generated",
		"mock":               map[string]any{"name": mockName, "version": mockVersion, "contract": contractV1},
		"kind":               kind,
		"direction":          direction,
		"protocol":           protocol,
		"summary":            summary,
	}
	for k, v := range extra {
		ev[k] = v
	}
	if m.isMutant(MutantEventMissingRunID) {
		delete(ev, "run_id")
	}
	line, err := json.Marshal(ev)
	if err != nil {
		panic(err) // plain values only
	}
	for sub := range m.subs {
		if sub.filter != "" && sub.filter != runID && !m.isMutant(MutantFilterIgnored) {
			continue
		}
		select {
		case sub.ch <- line:
		default: // a subscriber that stops reading loses events rather than stalling the mock
		}
	}
}

// refusal is the Refusal document of the specification.
type refusal struct {
	Message    string   `json:"message"`
	ReasonCode string   `json:"reason_code,omitempty"`
	Allowed    []string `json:"allowed,omitempty"`
}

func (m *Mock) refuse(w http.ResponseWriter, status int, code, message string, allowed []string) {
	r := refusal{Message: message, ReasonCode: code, Allowed: allowed}
	if m.isMutant(MutantRefusalWithoutReasonCode) && code == "setting-unknown" {
		r.ReasonCode = ""
	}
	if m.isMutant(MutantUnknownSettingNoAllowed) && code == "setting-unknown" {
		r.Allowed = nil
	}
	writeJSON(w, status, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client hanging up mid-write is not the fixture's problem
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

func quoteJoin(items []string) string { return strings.Join(items, ", ") }
