package kubemock

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxBodyBytes = 1 << 20

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
)

// scope is one run's listener, settings and cluster.
type scope struct {
	m        *Mock
	runID    string
	listener listenerSpec
	cl       *cluster

	mu       sync.Mutex
	settings map[string]any

	srv  *http.Server
	done chan struct{}
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, listener: l, cl: newCluster(m.cfg.RetainEvents), done: make(chan struct{})}
	s.resetSettings()
	return s
}

func (s *scope) addr() string { return net.JoinHostPort(s.listener.Host, fmt.Sprint(s.listener.Port)) }

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = map[string]any{}
	for _, d := range settingDefs {
		s.settings[d.name] = d.def
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

// intSetting reads an integer setting; values arrive as float64 from the control surface.
func (s *scope) intSetting(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch v := s.settings[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (s *scope) boolSetting(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.settings[name].(bool)
	return v
}

func (s *scope) start() error {
	ln, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: readHeaderLimit}
	go func() { _ = s.srv.Serve(ln) }() // returns when the server is closed; that error is the stop signal
	return nil
}

// stop ends every watch and closes the listener and its connections.
func (s *scope) stop() {
	select {
	case <-s.done:
		return
	default:
		close(s.done)
	}
	s.cl.close()
	if s.srv != nil {
		_ = s.srv.Close() // connections that outlive the close are cut; the error is not actionable
	}
}

// call is what one request carries through the events it causes.
type call struct {
	corr, source string
	start        time.Time
	fields       map[string]any
	body         []byte
}

func (s *scope) correlation(r *http.Request) (id, source string) {
	if m := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); m != nil && m[1] != allZeroTraceID {
		return m[1], "traceparent"
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

// emit publishes one event of this call.
func (s *scope) emit(c *call, ev event) {
	ev.RunID, ev.CorrelationID, ev.CorrelationSource = s.runID, c.corr, c.source
	s.m.publish(ev)
}

// payload is the event payload of a call: only safe fields, or a redacted digest.
func (c *call) payload() any {
	if len(c.fields) > 0 {
		return map[string]any{"redacted": false, "fields": c.fields}
	}
	return nil
}

func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	tooBig := err == nil && len(body) > maxBodyBytes
	corr, source := s.correlation(r)
	c := &call{corr: corr, source: source, start: time.Now(), body: body}
	if other := baggageRun(r); other != "" && other != s.runID {
		s.emit(c, event{Kind: "scope.mismatch", Direction: "internal",
			Summary: fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, other)})
	}
	rt := parseRoute(r.URL.Path)
	c.fields = rt.safeFields()
	s.m.logRequest(s.runID, r, body)
	in := event{Kind: "request.received", Direction: "inbound", Summary: r.Method + " " + r.URL.Path, Payload: c.payload()}
	if in.Payload == nil {
		in.Payload = digestPayload(body)
	}
	s.emit(c, in)

	fault := s.applyFaults(c, w, r)
	if fault.answered {
		return
	}
	if tooBig {
		s.reply(c, w, http.StatusRequestEntityTooLarge, (&apiError{code: http.StatusRequestEntityTooLarge, reason: "RequestEntityTooLarge", message: "the request body is too large for this mock"}).status(), fault.delayed)
		return
	}
	s.dispatch(c, w, r, rt, fault.delayed)
}

// faultResult says what the settings did to a call.
type faultResult struct {
	answered bool   // a setting answered the call; nothing more to do
	delayed  string // "latency_ms" when the call was delayed, else ""
}

// applyFaults applies latency_ms, error_status and throttle, in that order,
// emitting fault.injected for each that changes the answer.
func (s *scope) applyFaults(c *call, w http.ResponseWriter, r *http.Request) faultResult {
	var res faultResult
	if ms := s.intSetting(SettingLatencyMS); ms > 0 {
		s.emit(c, event{Kind: "fault.injected", Direction: "internal", Fault: SettingLatencyMS,
			Summary: fmt.Sprintf("answer delayed %d ms by the setting %s", ms, SettingLatencyMS)})
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
		case <-s.done:
		}
		res.delayed = SettingLatencyMS
	}
	if code := s.intSetting(SettingErrorStatus); code != 0 {
		s.emit(c, event{Kind: "fault.injected", Direction: "internal", Fault: SettingErrorStatus,
			Summary: fmt.Sprintf("answer forced to %d by the setting %s", code, SettingErrorStatus)})
		s.reply(c, w, code, errForced(code).status(), SettingErrorStatus)
		res.answered = true
		return res
	}
	if s.boolSetting(SettingThrottle) {
		s.emit(c, event{Kind: "fault.injected", Direction: "internal", Fault: SettingThrottle,
			Summary: "answer replaced by 429 by the setting " + SettingThrottle})
		w.Header().Set("Retry-After", fmt.Sprint(throttleRetrySecs))
		s.reply(c, w, http.StatusTooManyRequests, errThrottled().status(), SettingThrottle)
		res.answered = true
	}
	return res
}

// announce emits response.sent for a call about to be answered with status.
func (s *scope) announce(c *call, status int, fault string) {
	outcome := "ok"
	switch {
	case fault != "":
		outcome = "fault"
	case status >= http.StatusBadRequest:
		outcome = "refused"
	}
	ms := int(time.Since(c.start).Milliseconds())
	s.emit(c, event{Kind: "response.sent", Direction: "outbound", Summary: fmt.Sprintf("response %d", status),
		Outcome: outcome, Fault: fault, LatencyMS: &ms, Payload: c.payload()})
}

// reply emits response.sent and then writes the answer, so the event is visible
// by the time the client has the response.
func (s *scope) reply(c *call, w http.ResponseWriter, status int, v any, fault string) {
	s.announce(c, status, fault)
	writeJSON(w, status, v)
}
