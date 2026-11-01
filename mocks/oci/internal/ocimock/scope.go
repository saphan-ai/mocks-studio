package ocimock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	shutdownGrace = 2 * time.Second
	rateWindow    = time.Minute
	maxFieldLen   = 256
)

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
)

// scope is one run's listener, settings, tokens and registry content.
type scope struct {
	m        *Mock
	runID    string
	listener listenerSpec
	store    *store

	mu       sync.Mutex
	settings map[string]any
	tokens   map[string]struct{}
	hits     []time.Time

	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, listener: l, store: newStore(), tokens: map[string]struct{}{}, done: make(chan struct{})}
	s.resetSettings()
	return s
}

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = map[string]any{}
	for _, d := range settingDefs {
		s.settings[d.name] = d.normalised()
	}
}

func (s *scope) apply(body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range body {
		s.settings[k] = v
	}
}

func (s *scope) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.settings))
	for k, v := range s.settings {
		out[k] = v
	}
	return out
}

func (s *scope) authRequired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := s.settings[settingAuthRequired].(bool)
	return b
}

func (s *scope) missingBlobs() map[string]bool {
	s.mu.Lock()
	raw, _ := s.settings[settingMissingBlob].(string)
	s.mu.Unlock()
	out := map[string]bool{}
	for _, d := range splitDigests(raw) {
		out[d] = true
	}
	return out
}

// countHit records one /v2/ request and returns the seconds to wait when the request
// exceeds the rate limit, or 0 when it is within it. Every request counts, so a limit
// set later applies to the traffic that came before it.
func (s *scope) countHit() int {
	now := s.m.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.hits[:0]
	for _, t := range s.hits {
		if now.Sub(t) < rateWindow {
			kept = append(kept, t)
		}
	}
	s.hits = append(kept, now)
	limit, _ := s.settings[settingRateLimit].(float64)
	if limit <= 0 || float64(len(s.hits)) <= limit {
		return 0
	}
	wait := int((rateWindow - now.Sub(s.hits[0])).Seconds()) + 1
	return max(wait, 1)
}

func (s *scope) addr() string {
	return net.JoinHostPort(s.listener.Host, fmt.Sprint(s.listener.Port))
}

func (s *scope) start() error {
	ln, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	s.ln = ln
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }() // returns on Shutdown; that error is the stop signal
	return nil
}

func (s *scope) stop() {
	select {
	case <-s.done:
		return
	default:
		close(s.done)
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // a connection that outlives the grace period is closed below
	_ = s.srv.Close()
}

// recorder remembers the status the handlers wrote and the setting that injected it.
type recorder struct {
	http.ResponseWriter
	status int
	fault  string
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// correlation resolves the correlation id of one call from its headers.
func correlation(r *http.Request) (id, source string) {
	if match := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); match != nil && match[1] != allZeroTraceID {
		return match[1], "traceparent"
	}
	return randomHex(16), "generated"
}

func baggageRun(r *http.Request) string {
	for _, member := range strings.Split(r.Header.Get("baggage"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if ok && k == "mocks.run" {
			return v
		}
	}
	return ""
}

func clip(v string) string {
	if len(v) > maxFieldLen {
		return v[:maxFieldLen]
	}
	return v
}

// payloadFields are the declared safe fields of one call and nothing else: never a
// header, a body, a credential or a token.
func payloadFields(rt route, r *http.Request) map[string]any {
	f := map[string]any{"operation": rt.op}
	if rt.name != "" && validName(rt.name) {
		f["repository"] = clip(rt.name)
	}
	switch rt.op {
	case opManifestGet, opManifestHead, opManifestPut, opManifestDelete:
		f["reference"] = clip(rt.ref)
		if _, ok := parseDigest(rt.ref); ok {
			f["digest"] = rt.ref
		}
	case opBlobGet, opBlobHead, opBlobDelete:
		f["digest"] = clip(rt.ref)
	case opUploadStart, opUploadPut:
		if d := r.URL.Query().Get("digest"); d != "" {
			f["digest"] = clip(d)
		}
	}
	return f
}

func mergeExtra(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.m.now()
	rt := parseRoute(r.Method, r.URL.Path)
	corr, source := correlation(r)
	common := map[string]any{"correlation_id": corr, "correlation_source": source}
	if other := baggageRun(r); other != "" && other != s.runID {
		s.m.publish(s.runID, "scope.mismatch", "internal", fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, other), common)
	}
	summary := rt.op
	if rt.name != "" && validName(rt.name) {
		summary += " " + clip(rt.name)
	}
	s.m.publish(s.runID, "request.received", "inbound", summary,
		mergeExtra(common, map[string]any{"payload": map[string]any{"redacted": false, "fields": payloadFields(rt, r)}}))
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	s.dispatch(rec, r, rt)
	out := mergeExtra(common, map[string]any{"latency_ms": int(s.m.now().Sub(start).Milliseconds()), "outcome": "ok"})
	if rec.fault != "" {
		s.m.publish(s.runID, "fault.injected", "internal", fmt.Sprintf("setting %s injected status %d", rec.fault, rec.status),
			mergeExtra(common, map[string]any{"outcome": "fault", "fault": rec.fault}))
		out["outcome"], out["fault"] = "fault", rec.fault
	} else if rec.status >= http.StatusBadRequest {
		out["outcome"] = "refused"
	}
	s.m.log.Printf("run=%s op=%s status=%d", s.runID, rt.op, rec.status)
	s.m.publish(s.runID, "response.sent", "outbound", fmt.Sprintf("response %d", rec.status), out)
}
