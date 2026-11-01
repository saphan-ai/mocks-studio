package scimmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
	"time"
)

func kinds(evs []jm) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = str(ev, "kind")
	}
	return out
}

func find(evs []jm, kind string) (jm, bool) {
	for _, ev := range evs {
		if ev["kind"] == kind {
			return ev, true
		}
	}
	return nil, false
}

func (e *env) set(settings jm) {
	e.t.Helper()
	if st, _, out := e.control("PUT", "/scopes/"+e.runID+"/settings", settings); st != 200 {
		e.t.Fatalf("PUT settings %v: %d %v", settings, st, out)
	}
}

func TestErrorStatusInjectsFaultAndEmitsEvent(t *testing.T) {
	e := newEnv(t)
	if st, _, _ := e.do("GET", "/Users", nil); st != 200 { // twin: the setting is off
		t.Fatalf("baseline GET: %d", st)
	}
	e.events()
	e.set(jm{"error_status": 503})
	st, _, out := e.do("GET", "/Users", nil)
	assertScimError(t, st, out, 503, "")
	evs := e.events()
	fi, ok := find(evs, "fault.injected")
	if !ok || fi["fault"] != "error_status" || fi["outcome"] != "fault" {
		t.Fatalf("no fault.injected for error_status in %v", kinds(evs))
	}
	rs, _ := find(evs, "response.sent")
	if rs["outcome"] != "fault" || rs["fault"] != "error_status" {
		t.Fatalf("response.sent = %v", rs)
	}
	e.set(jm{"error_status": 0})
	if st, _, _ := e.do("GET", "/Users", nil); st != 200 {
		t.Fatalf("error_status=0 must turn the fault off: %d", st)
	}
}

func TestErrorStatusBetween1And99IsRefused(t *testing.T) {
	e := newEnv(t)
	e.set(jm{"error_status": 100}) // the twin: the lowest real status is accepted
	st, _, out := e.control("PUT", "/scopes/"+e.runID+"/settings", jm{"error_status": 99})
	if st != 422 || out["reason_code"] != "setting-out-of-range" {
		t.Fatalf("error_status=99: %d %v", st, out)
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t)
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e.m.now = func() time.Time { return clock }
	e.set(jm{"rate_limit": 2})
	for i := 1; i <= 2; i++ {
		if st, _, _ := e.do("GET", "/Users", nil); st != 200 {
			t.Fatalf("request %d within the limit: %d", i, st)
		}
	}
	e.events()
	st, h, out := e.do("GET", "/Users", nil)
	assertScimError(t, st, out, 429, "")
	if h.Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q", h.Get("Retry-After"))
	}
	if fi, ok := find(e.events(), "fault.injected"); !ok || fi["fault"] != "rate_limit" {
		t.Fatal("no fault.injected for rate_limit")
	}
	clock = clock.Add(61 * time.Second) // the window passes
	if st, _, _ := e.do("GET", "/Users", nil); st != 200 {
		t.Fatalf("after the window: %d", st)
	}
	e.control("POST", "/scopes/"+e.runID+"/reset", nil)
	for i := 0; i < 5; i++ {
		if st, _, _ := e.do("GET", "/Users", nil); st != 200 {
			t.Fatalf("reset must turn rate_limit off: %d", st)
		}
	}
}

func TestLatency(t *testing.T) {
	e := newEnv(t)
	e.set(jm{"latency_ms": 120})
	start := time.Now()
	st, _, _ := e.do("GET", "/Users", nil)
	if took := time.Since(start); st != 200 || took < 100*time.Millisecond {
		t.Fatalf("latency_ms=120 answered %d after %s", st, took)
	}
	evs := e.events()
	if fi, ok := find(evs, "fault.injected"); !ok || fi["fault"] != "latency_ms" {
		t.Fatalf("no fault.injected for latency_ms in %v", kinds(evs))
	}
	rs, _ := find(evs, "response.sent")
	if rs["latency_ms"].(float64) < 100 || rs["fault"] != "latency_ms" {
		t.Fatalf("response.sent = %v", rs)
	}
}

func TestSettingsRefusals(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name     string
		settings jm
		code     string
	}{
		{"unknown-name", jm{"nope": 1}, "setting-unknown"},
		{"wrong-type", jm{"error_status": "503"}, "setting-type-invalid"},
		{"fractional-integer", jm{"latency_ms": 1.5}, "setting-type-invalid"},
		{"above-maximum", jm{"latency_ms": 10001}, "setting-out-of-range"},
		{"below-minimum", jm{"rate_limit": -1}, "setting-out-of-range"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, out := e.control("PUT", "/scopes/"+e.runID+"/settings", c.settings)
			if st != 422 || out["reason_code"] != c.code {
				t.Fatalf("%d %v, want 422 %s", st, out, c.code)
			}
		})
	}
	t.Run("all-or-nothing", func(t *testing.T) {
		st, _, _ := e.control("PUT", "/scopes/"+e.runID+"/settings", jm{"error_status": 503, "nope": 1})
		if st != 422 {
			t.Fatalf("status %d", st)
		}
		if st, _, _ := e.do("GET", "/Users", nil); st != 200 {
			t.Fatal("the valid half of a refused request was applied")
		}
	})
}

func TestEventsCarryEnvelopeAndSafeFieldsOnly(t *testing.T) {
	e := newEnv(t)
	e.events()
	tp := "0af7651916cd43dd8448eb211c80319c"
	e.do("POST", "/Users", jm{"userName": "ev@acme-example.test", "displayName": "Ev", "active": true, "title": "Secret Title", "password": "p-ev"},
		"traceparent", "00-"+tp+"-b7ad6b7169203331-01")
	evs := e.events()
	if got := kinds(evs); len(got) != 2 || got[0] != "request.received" || got[1] != "response.sent" {
		t.Fatalf("events = %v", got)
	}
	req := evs[0]
	if req["correlation_id"] != tp || req["correlation_source"] != "traceparent" || req["run_id"] != e.runID || req["direction"] != "inbound" {
		t.Fatalf("request.received = %v", req)
	}
	fields := req["payload"].(map[string]any)["fields"].(map[string]any)
	want := jm{"method": "POST", "path": "/Users", "resource_type": "Users", "user_name": "ev@acme-example.test", "display_name": "Ev", "active": true}
	if len(fields) != len(want) {
		t.Fatalf("payload fields = %v, want exactly %v", fields, want)
	}
	for k, v := range want {
		if fields[k] != v {
			t.Fatalf("payload field %s = %v, want %v", k, fields[k], v)
		}
	}
	if strings.Contains(fmt.Sprint(evs), "Secret Title") {
		t.Fatal("a non-safe attribute reached an event")
	}
	if evs[1]["outcome"] != "ok" || evs[1]["summary"] != "response 201" {
		t.Fatalf("response.sent = %v", evs[1])
	}
}

func TestPatchPayloadExportsSafeOperationsOnly(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("pp@acme-example.test", nil)
	e.events()
	e.do("PATCH", "/Users/"+str(u, "id"), jm{"Operations": []any{
		jm{"op": "replace", "path": "active", "value": false},
		jm{"op": "replace", "path": "title", "value": "Hidden"},
		jm{"op": "replace", "value": jm{"displayName": "Shown", "nickName": "Hidden2"}},
	}})
	evs := e.events()
	fields := evs[0]["payload"].(map[string]any)["fields"].(map[string]any)
	if fields["active"] != false || fields["display_name"] != "Shown" || fields["method"] != "PATCH" {
		t.Fatalf("fields = %v", fields)
	}
	if s := fmt.Sprint(evs); strings.Contains(s, "Hidden") {
		t.Fatalf("a non-safe operation value reached an event: %s", s)
	}
}

func TestMismatchedBaggageIsRecordedUnderTheListenersRun(t *testing.T) {
	e := newEnv(t)
	e.events()
	st, _, _ := e.do("GET", "/Users", nil, "baggage", "mocks.run=run-elsewhere")
	if st != 200 {
		t.Fatalf("a call with foreign baggage must still be served: %d", st)
	}
	mm, ok := find(e.events(), "scope.mismatch")
	if !ok || mm["run_id"] != e.runID || !strings.Contains(str(mm, "summary"), "run-elsewhere") {
		t.Fatalf("scope.mismatch = %v", mm)
	}
}

func TestLifecycleEvents(t *testing.T) {
	e := newEnv(t)
	e.events()
	e.control("PUT", "/scopes/"+e.runID+"/settings", jm{"latency_ms": 0})
	e.control("POST", "/scopes/"+e.runID+"/reset", nil)
	e.control("DELETE", "/scopes/"+e.runID, nil)
	got := kinds(e.events())
	want := []string{"scenario.set", "scope.reset", "scope.closed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestControlSurfaceRefusals(t *testing.T) {
	e := newEnv(t)
	open := func(run, proto, host string) (int, jm) {
		st, _, out := e.control("POST", "/scopes", jm{"run_id": run, "listener": jm{"protocol": proto, "host": host, "port": freePort(t)}})
		return st, out
	}
	if st, _ := open("run-fine", "http", "127.0.0.1"); st != 201 { // twin
		t.Fatalf("valid open refused: %d", st)
	}
	for name, c := range map[string]struct {
		run, proto, host, code string
		status                 int
	}{
		"run-id-invalid":                {"A1", "http", "127.0.0.1", "run-id-invalid", 422},
		"run-already-open":              {"run-fine", "http", "127.0.0.1", "run-already-open", 409},
		"listener-protocol-unsupported": {"run-tcp", "tcp", "127.0.0.1", "listener-protocol-unsupported", 422},
		"listener-not-loopback":         {"run-wide", "http", "0.0.0.0", "listener-unavailable", 409},
	} {
		t.Run(name, func(t *testing.T) {
			st, out := open(c.run, c.proto, c.host)
			if st != c.status || out["reason_code"] != c.code || str(out, "message") == "" {
				t.Fatalf("%d %v, want %d %s", st, out, c.status, c.code)
			}
		})
	}
	if st, _, out := e.control("DELETE", "/scopes/run-never-opened", nil); st != 404 || out["reason_code"] != "run-unknown" {
		t.Fatalf("close unknown: %d %v", st, out)
	}
}

func TestListenerPortTakenIsRefused(t *testing.T) {
	e := newEnv(t)
	port := freePort(t)
	body := func(run string) jm {
		return jm{"run_id": run, "listener": jm{"protocol": "http", "host": "127.0.0.1", "port": port}}
	}
	if st, _, _ := e.control("POST", "/scopes", body("run-first")); st != 201 {
		t.Fatalf("first open: %d", st)
	}
	st, _, out := e.control("POST", "/scopes", body("run-second"))
	if st != 409 || out["reason_code"] != "listener-unavailable" {
		t.Fatalf("second open on a taken port: %d %v", st, out)
	}
}

func TestStartRefusesNonLoopbackControl(t *testing.T) {
	if m, err := Start(Config{ControlAddr: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	} else {
		m.Close()
	}
	if m, err := Start(Config{ControlAddr: "0.0.0.0:0"}); err == nil {
		m.Close()
		t.Fatal("0.0.0.0 must be refused")
	}
	if m, err := Start(Config{ControlAddr: "no-port"}); err == nil {
		m.Close()
		t.Fatal("an address without a port must be refused")
	}
}

func TestServedManifestIsTheEmbeddedFile(t *testing.T) {
	e := newEnv(t)
	resp, err := http.Get(e.m.ControlURL() + controlBase + "/manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	var served, file map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestFile, &file); err != nil {
		t.Fatal(err)
	}
	served["control"], file["control"] = nil, nil // the bound address differs from the file's default
	a, _ := json.Marshal(served)
	b, _ := json.Marshal(file)
	if !bytes.Equal(a, b) {
		t.Fatalf("served manifest differs from mocks.manifest.json:\n%s\n%s", a, b)
	}
	for _, d := range e.m.declared {
		if code, msg, _ := d.check(d.Default); code != "" {
			t.Fatalf("default of %s breaks its own declaration: %s", d.Name, msg)
		}
	}
}

// ---- no secret in a log or an event (law #56) ----

const (
	knownPassword  = "pw-Zx9!known-secret-value"
	knownPassword2 = "pw-Qr7?second-known-secret"
	knownBearer    = "bearer-Tk4-known-token-value"
)

// leaks names every secret that appears in any text.
func leaks(secrets []string, texts ...string) []string {
	var found []string
	for _, s := range secrets {
		for _, text := range texts {
			if strings.Contains(text, s) {
				found = append(found, s)
				break
			}
		}
	}
	return found
}

func TestNoSecretInLogOrEvent(t *testing.T) {
	e := newEnv(t)
	auth := []string{"Authorization", "Bearer " + knownBearer}
	st, _, u := e.do("POST", "/Users", jm{"userName": "secret@acme-example.test", "displayName": "Sec", "password": knownPassword}, auth...)
	if st != 201 {
		t.Fatalf("create: %d %v", st, u)
	}
	id := str(u, "id")
	st, _, patched := e.do("PATCH", "/Users/"+id, jm{"Operations": []any{
		jm{"op": "replace", "path": "password", "value": knownPassword2},
		jm{"op": "replace", "value": jm{"password": knownPassword2, "displayName": "Sec Two"}},
	}}, auth...)
	if st != 200 {
		t.Fatalf("patch: %d %v", st, patched)
	}
	e.do("PUT", "/Users/"+id, jm{"userName": "secret@acme-example.test", "password": knownPassword}, auth...)
	e.do("GET", "/Users/"+id, nil, auth...)
	e.set(jm{"error_status": 500})
	e.do("POST", "/Users", jm{"userName": "faulty@acme-example.test", "password": knownPassword}, auth...)

	secrets := []string{knownPassword, knownPassword2, knownBearer}
	var events []string
	for _, ev := range e.events() {
		raw, _ := json.Marshal(ev)
		events = append(events, string(raw))
	}
	if len(events) < 8 {
		t.Fatalf("only %d events captured; the scan would prove nothing", len(events))
	}
	if got := leaks(secrets, events...); len(got) > 0 {
		t.Fatalf("secrets reached an event: %v", got)
	}
	if e.logs.String() == "" {
		t.Fatal("the mock logged nothing; the scan would prove nothing")
	}
	if got := leaks(secrets, e.logs.String()); len(got) > 0 {
		t.Fatalf("secrets reached the log: %v\n%s", got, e.logs.String())
	}
	body, _ := json.Marshal(patched)
	if got := leaks(secrets, string(body)); len(got) > 0 {
		t.Fatalf("a secret came back in a response: %v", got)
	}
	// the log names the header only by whether it was present
	if !strings.Contains(e.logs.String(), "auth=true") {
		t.Fatalf("log lacks the auth=true marker:\n%s", e.logs.String())
	}
}

// Negative control: a logger that prints the raw request leaks the same secrets, and the scan sees it.
func TestNegativeControlRawRequestLoggerIsCaught(t *testing.T) {
	var raw syncBuf
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dump, _ := httputil.DumpRequest(r, true)
		_, _ = raw.Write(dump)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/Users", strings.NewReader(`{"userName":"x","password":"`+knownPassword+`"}`))
	req.Header.Set("Authorization", "Bearer "+knownBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // nothing to read
	got := leaks([]string{knownPassword, knownBearer}, raw.String())
	if len(got) != 2 {
		t.Fatalf("the scan found %v in a raw request dump; it must find both secrets", got)
	}
}
