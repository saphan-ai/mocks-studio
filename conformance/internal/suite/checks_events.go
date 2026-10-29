package suite

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/saphan-ai/mocks-studio/conformance/internal/schemas"
)

var vocabularyKinds = []string{
	"request.received", "response.sent", "message.delivered", "callout.sent", "fault.injected",
	"scenario.set", "scope.opened", "scope.reset", "scope.closed", "scope.mismatch",
}

var traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// eventProblem validates one event received from a mock for the scope it serves.
func (e *env) eventProblem(ev Event, runID string) string {
	if ev.Raw == nil {
		return "an event is not a JSON object: " + truncate(ev.Line, 120)
	}
	problem, err := schemas.Validate(schemas.EventID, ev.Raw)
	if err != nil {
		return err.Error()
	}
	if problem != "" {
		return fmt.Sprintf("event %q breaks mocks.event/v1: %s", ev.str("kind"), problem)
	}
	if got := ev.str("run_id"); got != runID {
		return fmt.Sprintf("event %q carries run_id %q, want %q", ev.str("kind"), got, runID)
	}
	mock, _ := ev.Raw["mock"].(map[string]any)
	if mock["name"] != e.manifest.Name || mock["version"] != e.manifest.Version || mock["contract"] != e.manifest.Contract {
		return fmt.Sprintf("event %q names mock %v, but the manifest says %s %s %s", ev.str("kind"), mock, e.manifest.Name, e.manifest.Version, e.manifest.Contract)
	}
	if kind := ev.str("kind"); strings.HasPrefix(kind, "x-") && !e.declaresKind(kind) {
		return fmt.Sprintf("event kind %q is a custom kind the manifest does not declare in event_kinds", kind)
	}
	return ""
}

func (e *env) declaresKind(kind string) bool {
	for _, k := range e.manifest.EventKinds {
		if k.Kind == kind {
			return true
		}
	}
	return false
}

func isKind(kind, runID string) func(Event) bool {
	return func(ev Event) bool {
		return ev.str("kind") == kind && (runID == "" || ev.str("run_id") == runID)
	}
}

func anyEvent(evs []Event, match func(Event) bool) (Event, bool) {
	for _, ev := range evs {
		if match(ev) {
			return ev, true
		}
	}
	return Event{}, false
}

// await waits for an event matching match among those arriving after mark.
func (e *env) await(mark int, match func(Event) bool) (Event, []Event, bool) {
	got, ok := e.events.waitFor(mark, e.opts.EventWait, func(evs []Event) bool {
		_, hit := anyEvent(evs, match)
		return hit
	})
	ev, _ := anyEvent(got, match)
	return ev, got, ok
}

func (e *env) checkEventValid(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	mark := e.events.mark()
	if p := e.probe(h, probeOpts{}); p.err != nil {
		return fail("the probe on %s got no answer: %v", h.addr(), p.err)
	}
	// The call's event is matched by kind alone: an event that lost its run_id must be
	// reported on its schema, not mistaken for no event at all.
	_, got, ok := e.await(mark, isKind("request.received", ""))
	if !ok {
		return fail("no request.received event arrived on /_mock/v1/events within %s of a call on the listener", e.opts.EventWait)
	}
	if problem := e.eventsProblem(got, h.runID); problem != "" {
		return fail("%s", problem)
	}
	return pass("%d event(s) after the call validate against mocks.event/v1 and carry run_id %s", len(got), h.runID)
}

// eventsProblem judges the events seen after a call on runID's listener. A late event of
// a scope this suite opened earlier (the previous scope's scope.closed) is held to its own
// run, not to this one; any other run_id, and every event's schema, is judged against runID.
func (e *env) eventsProblem(got []Event, runID string) string {
	for _, ev := range got {
		want := runID
		if r := ev.str("run_id"); r != runID && e.ownRun(r) {
			want = r
		}
		if problem := e.eventProblem(ev, want); problem != "" {
			return problem
		}
	}
	return ""
}

// ownRun reports whether runID names a scope this suite opened (newRunID's prefix).
func (e *env) ownRun(runID string) bool {
	return strings.HasPrefix(runID, "conf-"+e.nonce+"-")
}

func (e *env) checkLifecycleEvents(protocol string) outcome {
	mark := e.events.mark()
	h, r := e.open(protocol)
	if h == nil {
		return fail("a scope could not be opened for the check: %s", describeReply(r))
	}
	if _, _, ok := e.await(mark, isKind("scope.opened", h.runID)); !ok {
		e.closeScope(h)
		return fail("no scope.opened event for run %s arrived within %s of opening it", h.runID, e.opts.EventWait)
	}
	mark = e.events.mark()
	e.closeScope(h)
	ev, _, ok := e.await(mark, isKind("scope.closed", h.runID))
	if !ok {
		return fail("no scope.closed event for run %s arrived within %s of closing it", h.runID, e.opts.EventWait)
	}
	if problem := e.eventProblem(ev, h.runID); problem != "" {
		return fail("%s", problem)
	}
	return pass("scope.opened and scope.closed arrived for run %s", h.runID)
}

func (e *env) checkCorrelationTraceparent(protocol string) outcome {
	if protocol != "http" {
		return notApplicable("%s carries no headers; the endpoint is its only carrier", protocol)
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	traceID := randHex(16)
	if traceID == strings.Repeat("0", 32) {
		traceID = strings.Repeat("a", 32)
	}
	mark := e.events.mark()
	headers := map[string]string{"traceparent": fmt.Sprintf("00-%s-%s-01", traceID, randHex(8))}
	if p := e.probe(h, probeOpts{headers: headers}); p.err != nil {
		return fail("the probe on %s got no answer: %v", h.addr(), p.err)
	}
	ev, _, ok := e.await(mark, isKind("request.received", h.runID))
	if !ok {
		return fail("no request.received event for run %s arrived within %s", h.runID, e.opts.EventWait)
	}
	if ev.str("correlation_id") != traceID || ev.str("correlation_source") != "traceparent" {
		return fail("a call with traceparent trace-id %s produced correlation_id %q with correlation_source %q, want that trace-id and traceparent", traceID, ev.str("correlation_id"), ev.str("correlation_source"))
	}
	return pass("traceparent trace-id %s became the event's correlation_id with correlation_source traceparent", traceID)
}

func (e *env) checkCorrelationGenerated(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	var ids []string
	for i := 0; i < 2; i++ {
		mark := e.events.mark()
		if p := e.probe(h, probeOpts{}); p.err != nil {
			return fail("the probe on %s got no answer: %v", h.addr(), p.err)
		}
		ev, _, ok := e.await(mark, isKind("request.received", h.runID))
		if !ok {
			return fail("no request.received event for run %s arrived within %s", h.runID, e.opts.EventWait)
		}
		id := ev.str("correlation_id")
		if ev.str("correlation_source") != "generated" || !traceIDPattern.MatchString(id) || id == strings.Repeat("0", 32) {
			return fail("a call with no traceparent produced correlation_id %q with correlation_source %q, want a valid generated id", id, ev.str("correlation_source"))
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		return fail("two separate calls with no traceparent shared the generated correlation_id %s; each transaction needs its own", ids[0])
	}
	return pass("with no traceparent, each call got its own valid generated correlation_id")
}

func (e *env) checkScopeMismatch(protocol string) outcome {
	if protocol != "http" {
		return notApplicable("%s carries no baggage header", protocol)
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	other := "conf-" + e.nonce + "-other"
	mark := e.events.mark()
	p := e.probe(h, probeOpts{headers: map[string]string{"baggage": "mocks.run=" + other}})
	if p.err != nil {
		return fail("a call carrying baggage for another run was not served: %v; a conflict is recorded, the call is still served", p.err)
	}
	ev, _, ok := e.await(mark, isKind("scope.mismatch", h.runID))
	if !ok {
		return fail("a call on run %s carrying baggage mocks.run=%s produced no scope.mismatch event under run %s within %s", h.runID, other, h.runID, e.opts.EventWait)
	}
	if !strings.Contains(ev.Line, other) {
		return fail("the scope.mismatch event does not name the other run %s: %s", other, truncate(ev.Line, 200))
	}
	if _, _, ok := e.await(mark, isKind("request.received", h.runID)); !ok {
		return fail("the call with conflicting baggage was served but not recorded as request.received under run %s", h.runID)
	}
	return pass("conflicting baggage was recorded as scope.mismatch under run %s naming %s, and the call was served", h.runID, other)
}

func (e *env) checkScopesIsolated(protocol string) outcome {
	a, doneA, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer doneA()
	b, doneB, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer doneB()
	mark := e.events.mark()
	calls := []*scopeHandle{a, b, b}
	for _, h := range calls {
		if p := e.probe(h, probeOpts{}); p.err != nil {
			return fail("the probe on %s got no answer: %v", h.addr(), p.err)
		}
	}
	got, _ := e.events.waitFor(mark, e.opts.EventWait, func(evs []Event) bool { return countKind(evs, "request.received") >= len(calls) })
	tally := map[string]int{}
	for _, ev := range got {
		if ev.str("kind") == "request.received" {
			tally[ev.str("run_id")]++
		}
	}
	if tally[a.runID] != 1 || tally[b.runID] != 2 || len(tally) != 2 {
		return fail("one call on run %s and two on run %s were recorded as %v; each run's events must stay its own", a.runID, b.runID, tally)
	}
	return pass("run %s kept its 1 event and run %s its 2, with no leak between them", a.runID, b.runID)
}

func countKind(evs []Event, kind string) int {
	n := 0
	for _, ev := range evs {
		if ev.str("kind") == kind {
			n++
		}
	}
	return n
}

func (e *env) checkEventsFilter(protocol string) outcome {
	a, doneA, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer doneA()
	b, doneB, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer doneB()
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	filtered, _, err := openStream(ctx, e.control+controlBase+"/events?run_id="+a.runID, e.opts.RequestTimeout)
	if err != nil {
		return fail("GET /_mock/v1/events?run_id=%s could not be opened: %v", a.runID, err)
	}
	// A1, B1, A2: the second call on A is a barrier. An event for B that the filter let
	// through would be on the stream before it.
	for _, h := range []*scopeHandle{a, b, a} {
		if p := e.probe(h, probeOpts{}); p.err != nil {
			return fail("the probe on %s got no answer: %v", h.addr(), p.err)
		}
	}
	got, ok := filtered.waitFor(0, e.opts.EventWait, func(evs []Event) bool { return countKind(evs, "request.received") >= 2 })
	for _, ev := range got {
		if id := ev.str("run_id"); id != a.runID {
			return fail("the stream filtered to run_id=%s delivered an event of run %q", a.runID, id)
		}
	}
	if !ok {
		return fail("the stream filtered to run_id=%s delivered %d of the 2 request.received events for it within %s", a.runID, countKind(got, "request.received"), e.opts.EventWait)
	}
	return pass("the stream filtered to run %s delivered its events and none of run %s", a.runID, b.runID)
}

// payloadProblem checks one event's payload against the manifest's safe fields.
func (e *env) payloadProblem(ev Event) string {
	payload, ok := ev.Raw["payload"].(map[string]any)
	if !ok || payload["redacted"] != false {
		return ""
	}
	fields, _ := payload["fields"].(map[string]any)
	for name := range fields {
		if !contains(e.manifest.SafeFields, name) {
			return fmt.Sprintf("event %q exports field %q in clear, but safe_fields is [%s]", ev.str("kind"), name, joinList(e.manifest.SafeFields))
		}
	}
	return ""
}

func (e *env) checkPayloadSafeFields(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	marker := "conformance-marker-" + randHex(8)
	opts := probeOpts{marker: marker}
	if protocol == "http" {
		opts.headers = map[string]string{"X-Conformance-Marker": marker}
	}
	mark := e.events.mark()
	if p := e.probe(h, opts); p.err != nil {
		return fail("the probe on %s got no answer: %v", h.addr(), p.err)
	}
	_, got, ok := e.await(mark, isKind("response.sent", ""))
	if !ok {
		return fail("no response.sent event arrived within %s of the probe", e.opts.EventWait)
	}
	for _, ev := range got {
		if problem := e.payloadProblem(ev); problem != "" {
			return fail("%s", problem)
		}
		if strings.Contains(outsidePayload(ev), marker) {
			return fail("event %q repeats the probe's unsafe content (marker %s) outside payload.fields: %s", ev.str("kind"), marker, truncate(ev.Line, 200))
		}
	}
	return pass("%d event(s) export only declared safe fields and leak nothing from the probe", len(got))
}

// outsidePayload is the event with its payload removed, as JSON text.
func outsidePayload(ev Event) string {
	rest := map[string]any{}
	for k, v := range ev.Raw {
		if k != "payload" {
			rest[k] = v
		}
	}
	b, err := json.Marshal(rest)
	if err != nil {
		return ev.Line
	}
	return string(b)
}

func (e *env) checkNoCredentialInEvent(protocol string) outcome {
	if protocol != "http" {
		return notApplicable("%s has no header the suite can carry a credential in", protocol)
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	token := "conformance-credential-" + randHex(8)
	mark := e.events.mark()
	headers := map[string]string{"Authorization": "Bearer " + token}
	if p := e.probe(h, probeOpts{headers: headers}); p.err != nil {
		return fail("the probe on %s got no answer: %v", h.addr(), p.err)
	}
	_, got, ok := e.await(mark, isKind("response.sent", ""))
	if !ok {
		return fail("no response.sent event arrived within %s of the probe", e.opts.EventWait)
	}
	for _, ev := range got {
		if strings.Contains(ev.Line, token) {
			return fail("event %q contains the credential the probe sent in its Authorization header: %s", ev.str("kind"), truncate(ev.Line, 200))
		}
	}
	return pass("a probe's Authorization credential appears in none of the %d event(s)", len(got))
}
