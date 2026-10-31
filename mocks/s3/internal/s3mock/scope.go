package s3mock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	maxBody       = 256 << 20
)

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
	hexDigest          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// scope is one run's listener, settings and buckets.
type scope struct {
	m        *Mock
	runID    string
	listener listenerSpec
	store    *store

	mu       sync.Mutex
	settings map[string]any

	ln  net.Listener
	srv *http.Server
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, listener: l, store: newStore(), settings: map[string]any{}}
	s.resetSettings()
	return s
}

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range settingDefs {
		s.settings[d.name] = toJSONValue(d.def)
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

func (s *scope) str(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.settings[name].(string)
	return v
}

func (s *scope) num(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.settings[name].(float64)
	return int(v)
}

func (s *scope) flag(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, _ := s.settings[name].(bool)
	return v
}

func (s *scope) addr() string { return net.JoinHostPort(s.listener.Host, fmt.Sprint(s.listener.Port)) }

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
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = s.srv.Shutdown(ctx) // a connection that outlives the grace period is closed below
	_ = s.srv.Close()
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

func (s *scope) correlation(r *http.Request) (id, source string) {
	if match := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); match != nil && match[1] != allZeroTraceID {
		return match[1], "traceparent"
	}
	return randomHex(16), "generated"
}

// request is one S3 call after parsing, as the operations see it.
type request struct {
	r         *http.Request
	body      []byte
	bucket    string
	key       string
	query     map[string][]string
	operation string
}

// parseTarget splits a path-style target into bucket and key.
func parseTarget(path string) (bucket, key string) {
	p := strings.TrimPrefix(path, "/")
	bucket, key, _ = strings.Cut(p, "/")
	return bucket, key
}

func lowerHeaders(r *http.Request) map[string]string {
	out := make(map[string]string, len(r.Header))
	for k, vs := range r.Header {
		out[strings.ToLower(k)] = strings.Join(vs, ",")
	}
	return out
}

func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	corr, source := s.correlation(r)
	common := map[string]any{"correlation_id": corr, "correlation_source": source}
	if other := baggageRun(r); other != "" && other != s.runID {
		s.m.publish(s.runID, "scope.mismatch", "internal",
			fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, other), common)
	}
	q := r.URL.Query()
	req := &request{r: r, body: body, query: q}
	req.bucket, req.key = parseTarget(r.URL.Path)
	req.operation = operationName(r.Method, req.bucket, req.key, q)

	fields := map[string]any{"operation": req.operation}
	if req.bucket != "" {
		fields["bucket"] = req.bucket
	}
	if req.key != "" {
		fields["key"] = req.key
	}
	s.m.publish(s.runID, "request.received", "inbound", fmt.Sprintf("%s %s", r.Method, r.URL.Path),
		mergeExtra(common, map[string]any{"payload": map[string]any{"redacted": false, "fields": fields}}))

	rw := &recorder{ResponseWriter: w, status: http.StatusOK}
	var fault string
	switch {
	case err != nil || len(body) > maxBody:
		writeS3Error(rw, req, 400, "IncompleteBody", "The request body could not be read in full or is larger than this mock accepts.", "")
	default:
		fault = s.dispatch(rw, req)
	}
	s.m.logRequest(s.runID, r, req.operation, rw.status)

	out := mergeExtra(common, map[string]any{"latency_ms": int(time.Since(start).Milliseconds()), "outcome": "ok"})
	switch {
	case fault != "":
		out["outcome"], out["fault"] = "fault", fault
	case rw.status >= 400:
		out["outcome"] = "refused"
	}
	s.m.publish(s.runID, "response.sent", "outbound", fmt.Sprintf("%s %s answered %d", r.Method, req.operation, rw.status), out)
}

// recorder remembers the status a handler wrote.
type recorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *recorder) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *recorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
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

func operationName(method, bucket, key string, q map[string][]string) string {
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case bucket == "":
		return "ListBuckets"
	case key == "":
		switch method {
		case http.MethodPut:
			return "CreateBucket"
		case http.MethodDelete:
			return "DeleteBucket"
		case http.MethodHead:
			return "HeadBucket"
		case http.MethodGet:
			if has("list-type") {
				return "ListObjectsV2"
			}
			return "ListObjects"
		}
	default:
		switch {
		case method == http.MethodPut && has("uploadId") && has("partNumber"):
			return "UploadPart"
		case method == http.MethodPut:
			return "PutObject"
		case method == http.MethodGet:
			return "GetObject"
		case method == http.MethodHead:
			return "HeadObject"
		case method == http.MethodDelete && has("uploadId"):
			return "AbortMultipartUpload"
		case method == http.MethodDelete:
			return "DeleteObject"
		case method == http.MethodPost && has("uploads"):
			return "CreateMultipartUpload"
		case method == http.MethodPost && has("uploadId"):
			return "CompleteMultipartUpload"
		}
	}
	return "Unsupported"
}

// injectFault applies the scenario settings that fail or delay a request before it is
// authenticated, so they show on any call. It returns the setting that fired.
func (s *scope) injectFault(w http.ResponseWriter, req *request) string {
	if s.flag(SettingSlowDown) {
		s.announce(req, SettingSlowDown, "SlowDown answered 503")
		writeS3Error(w, req, 503, "SlowDown", "Please reduce your request rate.", "")
		return SettingSlowDown
	}
	if st := s.num(SettingErrorStatus); st != 0 {
		s.announce(req, SettingErrorStatus, fmt.Sprintf("forced status %d", st))
		code, msg := errorForStatus(st)
		writeS3Error(w, req, st, code, msg, "")
		return SettingErrorStatus
	}
	return ""
}

func (s *scope) announce(req *request, setting, what string) {
	s.m.publish(s.runID, "fault.injected", "internal", fmt.Sprintf("%s: %s on %s", setting, what, req.operation),
		map[string]any{"outcome": "fault", "fault": setting})
}

func errorForStatus(st int) (code, msg string) {
	switch st {
	case 400:
		return "BadRequest", "The request was refused by the injected scenario."
	case 403:
		return "AccessDenied", "Access Denied"
	case 404:
		return "NoSuchKey", "The specified key does not exist."
	case 409:
		return "OperationAborted", "A conflicting conditional operation is currently in progress against this resource. Please try again."
	case 501:
		return "NotImplemented", "A header you provided implies functionality that is not implemented."
	case 503:
		return "ServiceUnavailable", "Service is unable to handle request."
	default:
		return "InternalError", "We encountered an internal error. Please try again."
	}
}

func bodySHA(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
