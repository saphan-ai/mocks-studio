package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

var (
	traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)
	zeroTraceID        = strings.Repeat("0", 32)
	zeroSpanID         = strings.Repeat("0", 16)
)

// unmatchedPath stands in an event or a log line where the path of a request that matched no route would be.
const unmatchedPath = "<unmatched path>"

// Rules a violation can carry that the document's schemas do not name.
const ruleBodySize = "maxBytes"

// outcomes of response.sent (spec event schema).
const (
	outcomeOK      = "ok"
	outcomeFault   = "fault"
	outcomeRefused = "refused"
)

// call is one request on a scope's listener, from arrival to its answer.
type call struct {
	r      *http.Request
	start  time.Time
	corr   string
	source string
	body   []byte
}

// answer is what a call is answered with, and how it is recorded.
type answer struct {
	status      int
	contentType string
	body        []byte
	allow       string
	outcome     string
	fault       string // the setting that changed the answer
	faultNote   string
}

// correlation takes the id from a valid traceparent, else generates one (spec §3.1).
func correlation(r *http.Request) (id, source string) {
	if m := traceparentPattern.FindStringSubmatch(r.Header.Get("traceparent")); m != nil && m[1] != zeroTraceID && m[2] != zeroSpanID {
		return m[1], sourceTraceparent
	}
	return newTraceID(), sourceGenerated
}

// baggageRun is the run a call's baggage names, when it names one that is a run id.
func baggageRun(r *http.Request) string {
	for _, member := range strings.Split(r.Header.Get("baggage"), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(member), "=")
		if ok && k == "mocks.run" && runIDPattern.MatchString(v) {
			return v
		}
	}
	return ""
}

func (s *scope) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.wg.Add(1)
	defer s.wg.Done()
	c := &call{r: r, start: time.Now()}
	c.corr, c.source = correlation(r)
	settings := s.snapshot()
	if !s.wait(r, settings[SettingDelayMS]) {
		return
	}
	c.body, _ = io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody)) // an unreadable or oversized body is judged below
	if other := baggageRun(r); other != "" && other != s.runID {
		s.event(c, Event{Kind: "scope.mismatch", Direction: "internal", Summary: fmt.Sprintf("call on run %s listener carried baggage for run %s; served under run %s", s.runID, other, s.runID)})
	}
	summary, ans, viol, op := s.decide(c, settings)
	s.event(c, Event{Kind: "request.received", Direction: "inbound", Summary: summary, Payload: s.requestPayload(c, op)})
	if ans.fault != "" {
		s.event(c, Event{Kind: "fault.injected", Direction: "internal", Outcome: outcomeFault, Fault: ans.fault, Summary: ans.faultNote})
	}
	latency := int(time.Since(c.start).Milliseconds())
	s.event(c, Event{
		Kind: "response.sent", Direction: "outbound", Outcome: ans.outcome, Fault: ans.fault, LatencyMS: &latency,
		Summary: fmt.Sprintf("answered %d", ans.status), Payload: redactedPayload(ans.body),
	})
	s.logCall(c, summary, ans, viol)
	write(w, ans)
}

// wait delays a call by delay_ms; it returns false when the call or the scope ended first.
func (s *scope) wait(r *http.Request, ms float64) bool {
	if ms <= 0 {
		return true
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	case <-s.done:
		return false
	}
}

func write(w http.ResponseWriter, a answer) {
	if a.allow != "" {
		w.Header().Set("Allow", a.allow)
	}
	if a.contentType != "" && len(a.body) > 0 {
		w.Header().Set("Content-Type", a.contentType)
	}
	w.WriteHeader(a.status)
	if len(a.body) > 0 {
		_, _ = w.Write(a.body) // the client hanging up is not the engine's problem
	}
}

func (s *scope) event(c *call, ev Event) {
	ev.RunID, ev.CorrelationID, ev.CorrelationSource = s.runID, c.corr, c.source
	s.e.publish(ev)
}

// decide routes the call, judges it against the document and chooses the answer.
func (s *scope) decide(c *call, settings map[string]float64) (string, answer, []oas.Violation, *oas.Operation) {
	doc := s.e.doc
	route, params, found := doc.Match(c.r.URL.EscapedPath())
	if !found {
		// The path is not recorded: it can carry what a caller meant to keep private, such as a reset token.
		return fmt.Sprintf("%s %s — no path template of the document matches", c.r.Method, unmatchedPath),
			refusalAnswer(http.StatusNotFound, refusal{
				Message:    "No path template of the document matches this request path and the request was refused; use one of the document's paths.",
				ReasonCode: "route-unknown",
			}, ""), nil, nil
	}
	op, declared := route.Methods[c.r.Method]
	if !declared {
		allowed := route.Allowed()
		return fmt.Sprintf("%s %s — the method is not declared", c.r.Method, route.Template),
			refusalAnswer(http.StatusMethodNotAllowed, refusal{
				Message:    fmt.Sprintf("The path %s does not declare the method %s and the request was refused; use one of the allowed methods.", route.Template, c.r.Method),
				ReasonCode: "method-not-allowed", Allowed: allowed,
			}, strings.Join(allowed, ", ")), nil, nil
	}
	viol := s.judge(c, op, params)
	summary := c.r.Method + " " + route.Template + violationSummary(viol)
	if name, status, forced := s.fault(op, settings); forced {
		ans := faultAnswer(op, name, status)
		return summary, ans, viol, op
	}
	if len(viol) > 0 {
		return summary, violationAnswer(viol), viol, op
	}
	a := op.SuccessAnswer()
	return summary, answer{status: a.Status, contentType: a.ContentType, body: a.Body, outcome: outcomeOK}, viol, op
}

func (s *scope) judge(c *call, op *oas.Operation, params map[string]string) []oas.Violation {
	if len(c.body) >= maxRequestBody {
		return []oas.Violation{{Pointer: "/body", Rule: ruleBodySize}}
	}
	return op.Validate(oas.Request{
		PathParams: params, Query: c.r.URL.Query(), Header: c.r.Header,
		ContentType: c.r.Header.Get("Content-Type"), Body: c.body,
	})
}

func violationSummary(v []oas.Violation) string {
	if len(v) == 0 {
		return ""
	}
	s := fmt.Sprintf(" — invalid %s (rule %s)", v[0].Pointer, v[0].Rule)
	if len(v) > 1 {
		s += fmt.Sprintf(" and %d more", len(v)-1)
	}
	return s
}

// fault finds the setting that changes this call's answer: an operation's status, the global status, then fail_rate.
func (s *scope) fault(op *oas.Operation, settings map[string]float64) (string, int, bool) {
	if name := s.e.opSettings[op.ID]; settings[name] >= MinForceStatus {
		return name, int(settings[name]), true
	}
	if settings[SettingStatusCode] >= MinForceStatus {
		return SettingStatusCode, int(settings[SettingStatusCode]), true
	}
	if rate := settings[SettingFailRate]; rate > 0 && s.e.opts.Rand() < rate {
		return SettingFailRate, FaultStatus, true
	}
	return "", 0, false
}

func faultAnswer(op *oas.Operation, name string, status int) answer {
	a := answer{status: status, outcome: outcomeFault, fault: name,
		faultNote: fmt.Sprintf("setting %s forced %d on %s", name, status, op.ID)}
	if decl, ok := op.AnswerFor(status); ok {
		a.contentType, a.body = decl.ContentType, decl.Body
		return a
	}
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return a
	}
	a.contentType = "application/json"
	a.body = marshal(refusal{
		Message:    fmt.Sprintf("The mock answered %d because the setting %q forces it; set it back to 0 to see the document's answer.", status, name),
		ReasonCode: "fault-injected",
	})
	return a
}

// violationAnswer is the 400 for a request that breaks the document: it names the field and the rule, never the value.
func violationAnswer(v []oas.Violation) answer {
	type item struct {
		Pointer string `json:"pointer"`
		Rule    string `json:"rule"`
	}
	items := make([]item, len(v))
	for i, x := range v {
		items[i] = item{x.Pointer, x.Rule}
	}
	message := fmt.Sprintf("The request field %s breaks the rule %q of the OpenAPI document and the request was refused; correct that field and send the request again.", v[0].Pointer, v[0].Rule)
	if v[0].Rule == oas.RuleMaxDepth {
		message = fmt.Sprintf("The request body nests objects and arrays deeper than the limit of %d levels and the request was refused; send a body that nests no deeper.", oas.MaxBodyDepth)
	}
	body := struct {
		refusal
		Pointer    string `json:"pointer"`
		Rule       string `json:"rule"`
		Violations []item `json:"violations"`
	}{
		refusal: refusal{
			Message:    message,
			ReasonCode: "request-invalid",
		},
		Pointer: v[0].Pointer, Rule: v[0].Rule, Violations: items,
	}
	return answer{status: http.StatusBadRequest, contentType: "application/json", body: marshal(body), outcome: outcomeRefused}
}

func refusalAnswer(status int, r refusal, allow string) answer {
	return answer{status: status, contentType: "application/json", body: marshal(r), outcome: outcomeRefused, allow: allow}
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"message":"The mock could not encode its own answer.","reason_code":"internal-error"}`)
	}
	return b
}
