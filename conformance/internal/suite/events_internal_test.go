package suite

import (
	"strings"
	"testing"
)

// event builds a valid mocks.event/v1 object for the test manifest, of kind on runID.
func testEvent(kind, runID string) Event {
	return Event{Raw: map[string]any{
		"schema":             "mocks.event/v1",
		"event_id":           "evt-" + runID + "-" + kind,
		"at":                 "2026-10-29T08:00:00.000Z",
		"run_id":             runID,
		"correlation_id":     "4bf92f3577b34da6a3ce929d0e0e4736",
		"correlation_source": "generated",
		"mock":               map[string]any{"name": "nullmock", "version": "0.1.0", "contract": "mocks.adapter/v1"},
		"kind":               kind,
		"direction":          "internal",
		"protocol":           "http",
		"summary":            kind,
	}}
}

func testEnv() *env {
	return &env{manifest: &Manifest{Contract: "mocks.adapter/v1", Name: "nullmock", Version: "0.1.0"}, nonce: "ab12cd"}
}

// Measured 2026-10-29 on a fresh checkout: the previous scope's scope.closed arrived after
// the next scope's call and event-valid failed a conformant mock on it. A late event of a
// scope the suite opened is judged on its own run; a foreign run_id still fails.
func TestEventsAfterACallAreJudgedOnTheirOwnRun(t *testing.T) {
	e := testEnv()
	this, previous := "conf-ab12cd-04", "conf-ab12cd-03"
	cases := []struct {
		name string
		got  []Event
		want string // substring of the problem; "" means no problem
	}{
		{"late scope.closed of the previous scope", []Event{testEvent("scope.closed", previous), testEvent("request.received", this)}, ""},
		{"call event on this scope", []Event{testEvent("request.received", this)}, ""},
		{"call event on a run the suite never opened", []Event{testEvent("request.received", "someone-elses-run")}, `carries run_id "someone-elses-run", want "conf-ab12cd-04"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := e.eventsProblem(c.got, this)
			if c.want == "" && got != "" {
				t.Fatalf("problem %q, want none", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("problem %q, want it to contain %q", got, c.want)
			}
		})
	}
}

// An event that lost its run_id is still refused, on the schema, wherever it sits.
func TestEventWithoutRunIDIsRefusedOnTheSchema(t *testing.T) {
	ev := testEvent("request.received", "conf-ab12cd-04")
	delete(ev.Raw, "run_id")
	if got := testEnv().eventsProblem([]Event{ev}, "conf-ab12cd-04"); !strings.Contains(got, "breaks mocks.event/v1") {
		t.Fatalf("problem %q, want a schema refusal", got)
	}
}
