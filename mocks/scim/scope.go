package scimmock

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	shutdownGrace = 2 * time.Second
	maxBody       = 1 << 20
	rateWindow    = 60 * time.Second
)

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
)

// scope is one run's listener, settings and SCIM data. Nothing is shared between scopes.
type scope struct {
	m        *Mock
	runID    string
	listener listenerSpec

	mu          sync.Mutex // settings and the rate window
	settings    map[string]any
	windowStart time.Time
	windowCount int

	smu   sync.Mutex // the SCIM store: one request at a time reads or writes it
	store *store

	ln   net.Listener
	srv  *http.Server
	done chan struct{}
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, listener: l, store: newStore(), done: make(chan struct{})}
	s.resetSettings()
	return s
}

func (s *scope) addr() string {
	return net.JoinHostPort(s.listener.Host, strconv.Itoa(s.listener.Port))
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
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = s.srv.Shutdown(ctx) // a connection that outlives the grace period is closed below
		_ = s.srv.Close()
	}
}

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = map[string]any{}
	for _, d := range s.m.declared {
		s.settings[d.Name] = d.Default
	}
	s.windowStart, s.windowCount = time.Time{}, 0
}

func (s *scope) apply(body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range body {
		s.settings[k] = v
	}
	s.windowStart, s.windowCount = time.Time{}, 0
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

func (s *scope) intSetting(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, _ := s.settings[name].(float64)
	return int(f)
}

// overLimit counts one request against the rate window and reports whether it exceeds rate_limit.
func (s *scope) overLimit(limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.m.now()
	if s.windowStart.IsZero() || now.Sub(s.windowStart) >= rateWindow {
		s.windowStart, s.windowCount = now, 0
	}
	s.windowCount++
	return s.windowCount > limit
}

// correlation resolves the correlation id of one call from its traceparent header.
func correlation(r *http.Request) (id, source string) {
	if match := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); match != nil && match[1] != allZeroTraceID {
		return match[1], "traceparent"
	}
	return randomHex(idBytes), "generated"
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

func merge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// serveHTTP is every call on the scope's listener: record it, apply the scenario settings,
// answer it, record the answer. The run is the listener's; nothing in the call can change it.
func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.m.now()
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody)) // an unreadable body is treated as empty
	corr, source := correlation(r)
	common := map[string]any{"correlation_id": corr, "correlation_source": source}
	if other := baggageRun(r); other != "" && other != s.runID {
		s.m.publish(s.runID, "scope.mismatch", "internal",
			fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, other), common)
	}
	summary := r.Method + " " + r.URL.Path
	s.m.publish(s.runID, "request.received", "inbound", summary, merge(common, map[string]any{"payload": requestPayload(r, body)}))

	rep, fault := s.fault(r, common)
	if rep == nil {
		s.smu.Lock()
		rep = s.route(r, body)
		s.smu.Unlock()
	}
	outcome := "ok"
	switch {
	case fault != "":
		outcome = "fault"
	case rep.status >= 400:
		outcome = "refused"
	}
	out := merge(common, map[string]any{"latency_ms": int(s.m.now().Sub(start).Milliseconds()), "outcome": outcome})
	if fault != "" {
		out["fault"] = fault
	}
	s.m.publish(s.runID, "response.sent", "outbound", fmt.Sprintf("response %d", rep.status), out)
	s.m.log.Printf("scim run=%s %s %s -> %d corr=%s auth=%t", s.runID, r.Method, r.URL.Path, rep.status, corr, r.Header.Get("Authorization") != "")
	rep.write(w)
}

// fault applies the scenario settings in the order latency_ms, rate_limit, error_status. It
// returns the injected reply, if any, and the name of the setting that caused the outcome.
func (s *scope) fault(r *http.Request, common map[string]any) (*reply, string) {
	injected := func(setting, summary string) {
		s.m.publish(s.runID, "fault.injected", "internal", summary, merge(common, map[string]any{"outcome": "fault", "fault": setting}))
	}
	name := ""
	if ms := s.intSetting("latency_ms"); ms > 0 {
		name = "latency_ms" // a delayed answer is a faulted one even when it then succeeds
		injected("latency_ms", fmt.Sprintf("delaying the answer by %d ms", ms))
		t := time.NewTimer(time.Duration(ms) * time.Millisecond)
		select {
		case <-t.C:
		case <-r.Context().Done():
		case <-s.done:
		}
		t.Stop()
	}
	if limit := s.intSetting("rate_limit"); limit > 0 && s.overLimit(limit) {
		injected("rate_limit", fmt.Sprintf("answering 429: more than %d requests in the current window", limit))
		rep := errReply(newErr(http.StatusTooManyRequests, "", "More than %d requests arrived in the current 60-second window and this one was refused; wait for the Retry-After seconds or raise rate_limit.", limit))
		rep.header.Set("Retry-After", strconv.Itoa(int(rateWindow.Seconds())))
		return rep, "rate_limit"
	}
	if st := s.intSetting("error_status"); st > 0 {
		injected("error_status", fmt.Sprintf("answering %d as scenario setting error_status says", st))
		return errReply(newErr(st, "", "The scenario setting error_status makes this mock answer %d to every SCIM request and this one was refused; set error_status to 0 to turn it off.", st)), "error_status"
	}
	return nil, name
}
