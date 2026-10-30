package kubemock

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestErrorStatusForcesEveryCallAndEmitsFaultInjected(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("setting-error")
	if r := s.call(http.MethodGet, podPath, nil); r.status != http.StatusOK {
		t.Fatalf("control: with error_status off the list answered %d, want 200", r.status)
	}
	s.drainEvents()
	s.put(map[string]any{SettingErrorStatus: 500})
	r := s.call(http.MethodGet, podPath, nil)
	if r.status != http.StatusInternalServerError || r.json(t)["reason"] != "InternalError" {
		t.Fatalf("error_status=500 answered %d %s, want a 500 Status", r.status, r.body)
	}
	if _, ok := s.event(func(e map[string]any) bool { return e["kind"] == "fault.injected" && e["fault"] == SettingErrorStatus }); !ok {
		t.Fatal("no fault.injected event named error_status")
	}
	if ev, ok := s.event(func(e map[string]any) bool { return e["kind"] == "response.sent" }); !ok || ev["outcome"] != "fault" || ev["fault"] != SettingErrorStatus {
		t.Fatalf("response.sent does not carry outcome fault / fault error_status: %v", ev)
	}
}

func TestThrottleAnswers429WithRetryAfter(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("setting-throttle")
	if r := s.call(http.MethodGet, podPath, nil); r.status != http.StatusOK || r.header.Get("Retry-After") != "" {
		t.Fatalf("control: with throttle off the list answered %d with Retry-After %q", r.status, r.header.Get("Retry-After"))
	}
	s.put(map[string]any{SettingThrottle: true})
	r := s.call(http.MethodGet, podPath, nil)
	if r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" || r.json(t)["reason"] != "TooManyRequests" {
		t.Fatalf("throttle=true answered %d (Retry-After %q) %s, want 429 with Retry-After", r.status, r.header.Get("Retry-After"), r.body)
	}
	if _, ok := s.event(func(e map[string]any) bool { return e["kind"] == "fault.injected" && e["fault"] == SettingThrottle }); !ok {
		t.Fatal("no fault.injected event named throttle")
	}
}

func TestErrorStatusWinsOverThrottle(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("setting-precedence")
	s.put(map[string]any{SettingThrottle: true, SettingErrorStatus: 503})
	if r := s.call(http.MethodGet, podPath, nil); r.status != http.StatusServiceUnavailable {
		t.Fatalf("with both set the call answered %d, want 503 from error_status", r.status)
	}
}

func TestLatencyDelaysTheAnswerAndEmitsFaultInjected(t *testing.T) {
	t.Parallel()
	const delayMS = 200
	s := newRig(t, 0).open("setting-latency")
	start := time.Now()
	s.call(http.MethodGet, podPath, nil)
	if base := time.Since(start); base >= delayMS*time.Millisecond {
		t.Fatalf("control: with latency_ms off the call already took %s", base)
	}
	s.put(map[string]any{SettingLatencyMS: delayMS})
	start = time.Now()
	if r := s.call(http.MethodGet, podPath, nil); r.status != http.StatusOK {
		t.Fatalf("a delayed call answered %d, want 200", r.status)
	}
	if took := time.Since(start); took < delayMS*time.Millisecond {
		t.Fatalf("latency_ms=%d: the call took %s", delayMS, took)
	}
	if _, ok := s.event(func(e map[string]any) bool { return e["kind"] == "fault.injected" && e["fault"] == SettingLatencyMS }); !ok {
		t.Fatal("no fault.injected event named latency_ms")
	}
}

func TestResetReturnsSettingsToTheirDefaults(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	s := r.open("setting-reset")
	s.put(map[string]any{SettingErrorStatus: 500, SettingDropWatchAfterEvent: 2})
	resp, err := http.Post(r.m.ControlURL()+controlBase+"/scopes/"+s.runID+"/reset", "application/json", strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // status only
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset answered %d, want 204", resp.StatusCode)
	}
	if got := s.call(http.MethodGet, podPath, nil); got.status != http.StatusOK {
		t.Fatalf("after reset the call answered %d, want 200", got.status)
	}
}

func TestSettingRefusalsKeepReasonCodesAndAllowedNames(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	s := r.open("setting-refusals")
	put := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPut, r.m.ControlURL()+controlBase+"/scopes/"+s.runID+"/settings", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }() // decoded below
		var doc map[string]any
		_ = decodeJSONReader(resp.Body, &doc) // a non-JSON body leaves doc empty, which fails the assertion below
		return resp.StatusCode, doc
	}
	for body, want := range map[string]string{
		`{"nope":1}`:             "setting-unknown",
		`{"error_status":"x"}`:   "setting-type-invalid",
		`{"error_status":600}`:   "setting-out-of-range",
		`{"error_status":200}`:   "setting-out-of-range",
		`{"latency_ms":-1}`:      "setting-out-of-range",
		`{"throttle":"yes"}`:     "setting-type-invalid",
		`{"error_status":503.5}`: "setting-type-invalid",
	} {
		if status, doc := put(body); status != http.StatusUnprocessableEntity || doc["reason_code"] != want {
			t.Errorf("PUT %s answered %d %v, want 422 %s", body, status, doc, want)
		}
	}
	if status, _ := put(`{"error_status":503,"nope":1}`); status != http.StatusUnprocessableEntity {
		t.Fatalf("a request with one bad entry answered %d, want 422", status)
	}
	if got := s.call(http.MethodGet, podPath, nil); got.status != http.StatusOK {
		t.Fatalf("a refused request applied part of itself: the next call answered %d, want 200", got.status)
	}
}

func TestEventsOfACallShareOneCorrelationAndFollowTheContract(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("events-contract")
	const trace = "0af7651916cd43dd8448eb211c80319c"
	s.call(http.MethodPost, cmPath, configMap("acme-example-cm", map[string]string{"k": "v"}),
		"traceparent", "00-"+trace+"-b7ad6b7169203331-01")
	var got []map[string]any
	for _, e := range s.drainEvents() {
		if e["kind"] == "request.received" || e["kind"] == "response.sent" {
			got = append(got, e)
		}
	}
	if len(got) != 2 || got[0]["kind"] != "request.received" || got[1]["kind"] != "response.sent" {
		t.Fatalf("one call produced %d request/response events, want request.received then response.sent: %v", len(got), got)
	}
	for _, e := range got {
		if e["correlation_id"] != trace || e["correlation_source"] != "traceparent" || e["run_id"] != s.runID {
			t.Errorf("event %v does not carry the traceparent correlation and the run", e)
		}
	}
	fields, _ := path(got[1], "payload", "fields").(map[string]any)
	if fields["kind"] != "ConfigMap" || fields["namespace"] != "default" || fields["name"] != "acme-example-cm" {
		t.Errorf("response.sent safe fields are %v, want kind, namespace and name", fields)
	}
	if got[1]["outcome"] != "ok" {
		t.Errorf("a created object's response.sent has outcome %v, want ok", got[1]["outcome"])
	}
}

func TestWatchDeliveriesAreMessageDeliveredEvents(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("events-delivered")
	w := s.watch(cmPath + "?watch=true&resourceVersion=" + s.listRV(cmPath))
	s.call(http.MethodPost, cmPath, configMap("acme-example-cm", nil))
	w.mustNext()
	ev, ok := s.event(kindIs("message.delivered"))
	if !ok {
		t.Fatal("no message.delivered event for a delivered watch change")
	}
	fields, _ := path(ev, "payload", "fields").(map[string]any)
	if ev["direction"] != "outbound" || fields["name"] != "acme-example-cm" || fields["kind"] != "ConfigMap" {
		t.Fatalf("message.delivered is %v, want outbound with kind ConfigMap and name acme-example-cm", ev)
	}
}

func TestCallWithBaggageForAnotherRunIsServedAndRecordedAsMismatch(t *testing.T) {
	t.Parallel()
	s := newRig(t, 0).open("events-mismatch")
	if r := s.call(http.MethodGet, podPath, nil, "baggage", "mocks.run=other-run"); r.status != http.StatusOK {
		t.Fatalf("a call with foreign baggage answered %d, want 200", r.status)
	}
	if ev, ok := s.event(kindIs("scope.mismatch")); !ok || !strings.Contains(ev["summary"].(string), "other-run") {
		t.Fatalf("no scope.mismatch naming the other run: %v", ev)
	}
}

func TestPublishedManifestFileIsTheServedManifest(t *testing.T) {
	t.Parallel()
	want := readFileForTest(t, "../mocks.manifest.json")
	if got := ManifestBytes("http://127.0.0.1:9100"); string(got) != string(want) {
		t.Fatalf("mocks.manifest.json is out of date; regenerate it with `kubernetes-mock manifest`")
	}
}
