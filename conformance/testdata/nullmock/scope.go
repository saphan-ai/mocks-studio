package nullmock

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	shutdownGrace = 2 * time.Second
	maxLine       = 4096
	maxBody       = 1 << 16
	maxConnIdle   = 30 * time.Second
)

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
)

// scope is one run's listener and settings.
type scope struct {
	m        *Mock
	runID    string
	protocol string
	listener listenerSpec

	mu       sync.Mutex
	settings map[string]any

	ln    net.Listener
	srv   *http.Server
	conns map[net.Conn]struct{}
	done  chan struct{}
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, protocol: m.cfg.Mode, listener: l, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
	s.settings = map[string]any{}
	for name, d := range s.declared() {
		s.settings[name] = d.def
	}
	return s
}

// declared is the settings the manifest declares, decoded from the manifest itself so
// the fixture cannot declare one thing and enforce another.
func (s *scope) declared() map[string]setting {
	out := map[string]setting{}
	doc := baseManifest(s.m.cfg.Mode, s.m.controlURL)
	for _, raw := range doc["settings"].([]any) {
		d := raw.(map[string]any)
		st := setting{typ: d["type"].(string), def: toJSONValue(d["default"])}
		if a, ok := d["allowed"].([]string); ok {
			for _, v := range a {
				st.allowed = append(st.allowed, v)
			}
		}
		if v, ok := d["minimum"].(int); ok {
			f := float64(v)
			st.min = &f
		}
		if v, ok := d["maximum"].(int); ok {
			f := float64(v)
			st.max = &f
		}
		out[d["name"].(string)] = st
	}
	return out
}

// toJSONValue normalises Go ints to float64, the type a decoded JSON number has.
func toJSONValue(v any) any {
	if i, ok := v.(int); ok {
		return float64(i)
	}
	return v
}

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, d := range s.declared() {
		s.settings[name] = d.def
	}
}

func (s *scope) apply(body map[string]any, ignored bool) {
	if ignored {
		return
	}
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

func (s *scope) get(name string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings[name]
}

func (s *scope) addr() string {
	return net.JoinHostPort(s.listener.Host, fmt.Sprint(s.listener.Port))
}

func (s *scope) start() error {
	if s.m.isMutant(MutantListenerSilent) {
		return nil // the scope opens and answers 201, but nothing listens
	}
	ln, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	s.ln = ln
	if s.protocol == ModeHTTP {
		s.srv = &http.Server{Handler: http.HandlerFunc(s.serveHTTP), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = s.srv.Serve(ln) }() // returns on Shutdown; that error is the stop signal
		return nil
	}
	go s.acceptTCP()
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
	if s.ln != nil {
		_ = s.ln.Close() // already closed by Shutdown for http; the error is expected
	}
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close() // the peer may be gone already
	}
	s.mu.Unlock()
}

// correlation resolves the correlation id of one http call from its headers.
func (s *scope) correlation(r *http.Request) (id, source string) {
	if !s.m.isMutant(MutantTraceparentIgnored) {
		if match := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); match != nil && match[1] != allZeroTraceID {
			return match[1], "traceparent"
		}
	}
	source = "generated"
	if s.m.isMutant(MutantGeneratedLabelledHeader) {
		source = "traceparent"
	}
	return randomHex(idBytes), source
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

func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxBody)) // an unreadable body is recorded as empty
	corr, source := s.correlation(r)
	common := map[string]any{"correlation_id": corr, "correlation_source": source}
	if other := baggageRun(r); other != "" && other != s.runID && !s.m.isMutant(MutantMismatchNotRecorded) {
		s.m.publish(s.runID, "scope.mismatch", "internal", "http",
			fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, other), common)
	}
	summary := fmt.Sprintf("%s %s", r.Method, r.URL.Path)
	if s.m.isMutant(MutantCredentialInSummary) {
		summary += " auth=" + r.Header.Get("Authorization")
	}
	in := mergeExtra(common, map[string]any{"payload": s.httpPayload(r, body)})
	s.m.publish(s.runID, "request.received", "inbound", "http", summary, in)
	status := http.StatusOK
	if v, ok := s.get("status_code").(float64); ok {
		status = int(v)
	}
	out := mergeExtra(common, map[string]any{"latency_ms": int(time.Since(start).Milliseconds()), "outcome": "ok"})
	if status != http.StatusOK {
		out["outcome"], out["fault"] = "fault", "status_code"
	}
	direction := "outbound"
	if s.m.isMutant(MutantResponseSentInvalid) {
		direction = "sideways"
	}
	s.m.publish(s.runID, "response.sent", direction, "http", fmt.Sprintf("response %d", status), out)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, "ok\n") // the client hanging up is not the fixture's problem
}

func (s *scope) httpPayload(r *http.Request, body []byte) map[string]any {
	if s.m.isMutant(MutantNonSafeFieldInClear) {
		return map[string]any{"redacted": false, "fields": map[string]any{"method": r.Method, "path": r.URL.Path, "body": string(body)}}
	}
	return map[string]any{"redacted": false, "fields": map[string]any{"method": r.Method, "path": r.URL.Path}}
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

func (s *scope) acceptTCP() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serveTCP(c)
	}
}

func (s *scope) serveTCP(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close() // the peer may be gone already
	}()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 0, maxLine), maxLine)
	for {
		if err := c.SetReadDeadline(time.Now().Add(maxConnIdle)); err != nil || !sc.Scan() {
			return
		}
		line := sc.Text()
		common := map[string]any{"correlation_id": randomHex(idBytes), "correlation_source": "generated"}
		s.m.publish(s.runID, "request.received", "inbound", "tcp", fmt.Sprintf("line received (%d bytes)", len(line)), mergeExtra(common, map[string]any{
			"payload": map[string]any{"redacted": false, "fields": map[string]any{"length": len(line)}},
		}))
		reply := line
		if s.get("reply_mode") == "upper" {
			reply = strings.ToUpper(line)
		}
		s.m.publish(s.runID, "response.sent", "outbound", "tcp", "line answered", mergeExtra(common, map[string]any{"outcome": "ok"}))
		if _, err := fmt.Fprintf(c, "%s\n", reply); err != nil {
			return
		}
	}
}
