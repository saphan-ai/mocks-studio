package dnsmock

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
)

const dohPath = "/dns-query"

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-[0-9a-f]{16}-[0-9a-f]{2}$`)
	allZeroTraceID     = strings.Repeat("0", 32)
)

func baggageRun(r *http.Request) string {
	for _, member := range strings.Split(r.Header.Get("baggage"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if ok && k == "mocks.run" {
			return v
		}
	}
	return ""
}

// credentialNames lists WHICH credentials a DoH request carried, never what they were: the
// Authorization header, and any query parameter other than the DNS message (a token in the URL).
// This is the only place a request's credentials are looked at, and it returns names only.
func credentialNames(r *http.Request) []string {
	var names []string
	if _, ok := r.Header["Authorization"]; ok {
		names = append(names, "authorization")
	}
	for k := range r.URL.Query() {
		if k != "dns" {
			names = append(names, "query-param")
			break
		}
	}
	return names
}

// serveDoH answers RFC 8484: GET with ?dns=<base64url>, or POST of application/dns-message.
// A DNS message that is malformed is answered FORMERR inside a 200, as DNS does; an HTTP request
// that is not a DoH request is refused with its HTTP status and recorded.
func (s *scope) serveDoH(w http.ResponseWriter, r *http.Request) {
	q := queryReq{transport: "doh", mismatchRun: baggageRun(r), creds: credentialNames(r)}
	q.corrSource = "generated"
	if match := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); match != nil && match[1] != allZeroTraceID {
		q.corrID, q.corrSource = match[1], "traceparent"
	}
	if r.URL.Path != dohPath {
		s.refuseHTTP(w, q, http.StatusNotFound, "the path is not "+dohPath)
		return
	}
	switch r.Method {
	case http.MethodGet:
		vals := r.URL.Query()["dns"]
		if len(vals) != 1 {
			s.refuseHTTP(w, q, http.StatusBadRequest, "a GET carries exactly one dns parameter")
			return
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(vals[0], "="))
		if err != nil {
			s.refuseHTTP(w, q, http.StatusBadRequest, "the dns parameter is not base64url")
			return
		}
		q.raw = raw
	case http.MethodPost:
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/dns-message" {
			s.refuseHTTP(w, q, http.StatusUnsupportedMediaType, "a POST is application/dns-message")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxMessage+1))
		if err != nil || len(body) > maxMessage {
			s.refuseHTTP(w, q, http.StatusRequestEntityTooLarge, "the body is not a readable DNS message of at most 65535 bytes")
			return
		}
		q.raw = body
	default:
		w.Header().Set("Allow", "GET, POST")
		s.refuseHTTP(w, q, http.StatusMethodNotAllowed, "DoH is GET or POST")
		return
	}
	resp := s.process(q)
	switch {
	case resp.drop: // the scope closed under the request
		http.Error(w, "scope closed", http.StatusServiceUnavailable)
	case resp.httpStatus != 0:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(resp.httpStatus)
		_, _ = io.WriteString(w, "injected failure (doh_http_status)\n") // the client hanging up is not the mock's problem
	default:
		w.Header().Set("Content-Type", "application/dns-message")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(resp.wire) // the client hanging up is not the mock's problem
	}
}

// refuseHTTP answers a request that is not a DoH query and records it as a refused call.
func (s *scope) refuseHTTP(w http.ResponseWriter, q queryReq, status int, why string) {
	if s.enter() {
		defer s.wg.Done()
		common := map[string]any{"correlation_id": q.corrID, "correlation_source": q.corrSource}
		if q.corrID == "" {
			common["correlation_id"], common["correlation_source"] = randomHex(idBytes), "generated"
		}
		s.m.publish(s.runID, "request.received", "inbound", "http", "DoH request refused: "+why,
			mergeExtra(common, map[string]any{"payload": digest(nil)}))
		s.m.publish(s.runID, "response.sent", "outbound", "http", fmt.Sprintf("HTTP %d: %s", status, why),
			mergeExtra(common, map[string]any{"outcome": "refused", "payload": fields(map[string]any{"transport": "doh"})}))
		s.m.log.Printf("query run=%s transport=doh refused http=%d credentials=%s", s.runID, status, credList(q.creds))
	}
	http.Error(w, why, status)
}
