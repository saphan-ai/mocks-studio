package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestManifestIsPublishedSchemaValidAndServedUnchanged(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	schema := compile(t, "schemas/mocks-manifest-v1.schema.json")
	var doc any
	if err := json.Unmarshal(h.eng.ManifestJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(doc); err != nil {
		t.Fatalf("the manifest breaks its schema: %v", err)
	}
	served := h.control(http.MethodGet, "/manifest", "")
	if served.status != http.StatusOK || !bytes.Equal(served.body, h.eng.ManifestJSON()) {
		t.Fatalf("the served manifest differs from the manifest file bytes")
	}
	again := newHarness(t, apiDocument, nil)
	if !bytes.Equal(again.eng.ManifestJSON(), h.eng.ManifestJSON()) {
		t.Fatal("two engines built from one document produced different manifests")
	}
}

func TestEveryDeclaredSettingStaysInsideTheManifestFieldNamePattern(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	names := map[string]bool{}
	for _, s := range h.eng.Manifest().Settings {
		if !fieldNamePattern.MatchString(s.Name) {
			t.Errorf("setting %q does not match %s", s.Name, fieldNamePattern)
		}
		if names[s.Name] {
			t.Errorf("setting %q is declared twice", s.Name)
		}
		names[s.Name] = true
	}
	for _, want := range []string{"status_code", "delay_ms", "fail_rate", "operation_status.list_orders", "operation_status.create_order", "operation_status.get_order", "operation_status.login"} {
		if !names[want] {
			t.Errorf("setting %q is not declared", want)
		}
	}
}

func TestSettingNamesAreSanitisedAndStayUnique(t *testing.T) {
	taken := map[string]bool{}
	cases := map[string]string{
		"listOrders":   "operation_status.list_orders",
		"Get-User.v2":  "operation_status.get-user.v2",
		"éé":           "operation_status.operation",
		"listOrders ":  "operation_status.list_orders_2",
		"LIST_ORDERS":  "operation_status.list_orders_3",
		"a/b c":        "operation_status.a_b_c",
		"HTTPServerOK": "operation_status.httpserver_ok",
	}
	for _, id := range []string{"listOrders", "Get-User.v2", "éé", "listOrders ", "LIST_ORDERS", "a/b c", "HTTPServerOK"} {
		got := settingName(id, taken)
		if got != cases[id] {
			t.Errorf("settingName(%q) = %q, want %q", id, got, cases[id])
		}
		if !fieldNamePattern.MatchString(got) {
			t.Errorf("settingName(%q) = %q breaks the field name pattern", id, got)
		}
	}
	if got := settingName(strings.Repeat("a", 100), map[string]bool{}); len(got) > maxFieldName {
		t.Errorf("a long operationId gave a %d character setting name", len(got))
	}
}

func TestConfigRefusals(t *testing.T) {
	good := Config{Name: "ok-mock", ControlURL: "http://127.0.0.1:9100"}
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"name", func(c *Config) { c.Name = "Bad Name" }},
		{"control url path", func(c *Config) { c.ControlURL = "http://127.0.0.1:9100/x" }},
		{"control url scheme", func(c *Config) { c.ControlURL = "ftp://127.0.0.1:9100" }},
		{"safe field pattern", func(c *Config) { c.SafeFields = []string{"Bad"} }},
		{"safe field credential", func(c *Config) { c.SafeFields = []string{"password"} }},
		{"safe field credential pin", func(c *Config) { c.SafeFields = []string{"user_pin"} }},
		{"safe field twice", func(c *Config) { c.SafeFields = []string{"sku", "sku"} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := good
			c.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", cfg)
			}
		})
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("the good configuration was refused: %v", err)
	}
}

func TestListenerAnswersFromTheDocument(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-answers")
	r := send(t, http.MethodGet, base+"/orders", "", nil)
	if r.status != 200 || strings.TrimSpace(string(r.body)) != `[{"id":1}]` || r.header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET /orders = %d %s (%s)", r.status, r.body, r.header.Get("Content-Type"))
	}
	r = send(t, http.MethodPost, base+"/orders", `{"sku":"abc","quantity":2}`, map[string]string{"Content-Type": "application/json"})
	if r.status != 201 {
		t.Fatalf("POST /orders = %d %s", r.status, r.body)
	}
}

func TestSchemaGeneratedAnswersAreByteIdentical(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	headers := map[string]string{"X-Tenant": "acme"}
	first := send(t, http.MethodGet, h.open("run-det-one")+"/orders/5", "", headers)
	second := send(t, http.MethodGet, h.open("run-det-two")+"/orders/5", "", headers)
	third := send(t, http.MethodGet, h.open("run-det-one-b")+"/orders/5", "", headers)
	if first.status != 200 || len(first.body) == 0 {
		t.Fatalf("answer = %d %q", first.status, first.body)
	}
	if !bytes.Equal(first.body, second.body) || !bytes.Equal(first.body, third.body) {
		t.Fatalf("the same request answered differently: %q / %q / %q", first.body, second.body, third.body)
	}
}

// each case breaks exactly one field and must come back 400 naming that field's pointer in the body and in the event.
func TestBrokenRequestIsAnswered400NamingTheField(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-validate")
	f := h.feed("run-validate")
	okHeaders := func() map[string]string { return map[string]string{"X-Tenant": "acme"} }
	jsonType := map[string]string{"Content-Type": "application/json"}
	cases := []struct {
		name, method, path, body string
		headers                  map[string]string
		pointer, rule            string
	}{
		{"path", "GET", "/orders/seven", "", okHeaders(), "/path/orderId", "type"},
		{"path minimum", "GET", "/orders/0", "", okHeaders(), "/path/orderId", "minimum"},
		{"query", "GET", "/orders/5?limit=1000", "", okHeaders(), "/query/limit", "maximum"},
		{"query type", "GET", "/orders/5?limit=many", "", okHeaders(), "/query/limit", "type"},
		{"header missing", "GET", "/orders/5", "", nil, "/header/X-Tenant", "required"},
		{"header pattern", "GET", "/orders/5", "", map[string]string{"X-Tenant": "ACME-1"}, "/header/X-Tenant", "pattern"},
		{"body type", "POST", "/orders", `{"sku":"abc","quantity":"two"}`, jsonType, "/body/quantity", "type"},
		{"body minLength", "POST", "/orders", `{"sku":"a","quantity":2}`, jsonType, "/body/sku", "minLength"},
		{"body minimum", "POST", "/orders", `{"sku":"abc","quantity":0}`, jsonType, "/body/quantity", "minimum"},
		{"body required", "POST", "/orders", `{"sku":"abc"}`, jsonType, "/body/quantity", "required"},
		{"body extra field", "POST", "/orders", `{"sku":"abc","quantity":1,"colour":"red"}`, jsonType, "/body/colour", "additionalProperties"},
		{"body not JSON", "POST", "/orders", `{"sku":`, jsonType, "/body", "json"},
		{"body absent", "POST", "/orders", ``, jsonType, "/body", "required"},
		{"content type", "POST", "/orders", `{"sku":"abc","quantity":1}`, map[string]string{"Content-Type": "text/plain"}, "/header/Content-Type", "content-type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := send(t, c.method, base+c.path, c.body, c.headers)
			if r.status != http.StatusBadRequest {
				t.Fatalf("status = %d (%s), want 400", r.status, r.body)
			}
			doc := r.json(t)
			if doc["pointer"] != c.pointer || doc["rule"] != c.rule || doc["reason_code"] != "request-invalid" {
				t.Fatalf("body = %s, want pointer %s and rule %s", r.body, c.pointer, c.rule)
			}
			if msg, _ := doc["message"].(string); !strings.Contains(msg, c.pointer) {
				t.Fatalf("the message does not name the field %s: %q", c.pointer, msg)
			}
			ev := f.next("request.received")
			if summary, _ := ev["summary"].(string); !strings.Contains(summary, c.pointer) {
				t.Fatalf("the request.received summary %q does not name %s", summary, c.pointer)
			}
			if out := f.next("response.sent"); out["outcome"] != "refused" {
				t.Fatalf("response.sent outcome = %v, want refused", out["outcome"])
			}
		})
	}
	t.Run("the same requests, unbroken, are served", func(t *testing.T) {
		if r := send(t, "GET", base+"/orders/5?limit=10", "", okHeaders()); r.status != 200 {
			t.Fatalf("GET = %d %s", r.status, r.body)
		}
		if r := send(t, "POST", base+"/orders", `{"sku":"abc","quantity":1}`, jsonType); r.status != 201 {
			t.Fatalf("POST = %d %s", r.status, r.body)
		}
	})
}

func TestBrokenRequestNeverEchoesItsValues(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-echo")
	f := h.feed("run-echo")
	r := send(t, http.MethodPost, base+"/orders", `{"sku":"abc","quantity":"leaky-value-77"}`, map[string]string{"Content-Type": "application/json"})
	all := string(r.body)
	for _, ev := range f.drain() {
		b, _ := json.Marshal(ev)
		all += string(b)
	}
	if strings.Contains(all, "leaky-value-77") {
		t.Fatalf("a refused value was echoed in an answer or an event: %s", all)
	}
}

func TestUnknownPathIs404AndUndeclaredMethodIs405(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-routing")
	r := send(t, http.MethodGet, base+"/nothing-here", "", nil)
	if doc := r.json(t); r.status != 404 || doc["reason_code"] != "route-unknown" {
		t.Fatalf("unknown path = %d %s", r.status, r.body)
	}
	r = send(t, http.MethodDelete, base+"/orders", "", nil)
	doc := r.json(t)
	if r.status != 405 || doc["reason_code"] != "method-not-allowed" || r.header.Get("Allow") != "GET, POST" {
		t.Fatalf("undeclared method = %d %s Allow=%q", r.status, r.body, r.header.Get("Allow"))
	}
	if allowed, _ := doc["allowed"].([]any); len(allowed) != 2 {
		t.Fatalf("allowed = %v", doc["allowed"])
	}
}

func TestRefusalsAreValidRefusalDocuments(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	schema := compile(t, "schemas/mocks-refusal-v1.schema.json")
	h.open("run-refusals")
	cases := []struct {
		name   string
		r      reply
		status int
		code   string
	}{
		{"invalid run id", h.control("POST", "/scopes", `{"run_id":"A1","listener":{"protocol":"http","host":"127.0.0.1","port":0}}`), 422, "run-id-invalid"},
		{"duplicate", h.control("POST", "/scopes", `{"run_id":"run-refusals","listener":{"protocol":"http","host":"127.0.0.1","port":0}}`), 409, "run-already-open"},
		{"protocol", h.control("POST", "/scopes", `{"run_id":"run-tcp","listener":{"protocol":"tcp","host":"127.0.0.1","port":0}}`), 422, "listener-protocol-unsupported"},
		{"unknown run close", h.control("DELETE", "/scopes/run-none", ""), 404, "run-unknown"},
		{"unknown run reset", h.control("POST", "/scopes/run-none/reset", ""), 404, "run-unknown"},
		{"unknown run settings", h.set("run-none", `{"status_code":1}`), 404, "run-unknown"},
		{"setting unknown", h.set("run-refusals", `{"nope":1}`), 422, "setting-unknown"},
		{"setting type", h.set("run-refusals", `{"status_code":"x"}`), 422, "setting-type-invalid"},
		{"setting fractional integer", h.set("run-refusals", `{"status_code":503.5}`), 422, "setting-type-invalid"},
		{"setting range high", h.set("run-refusals", `{"status_code":600}`), 422, "setting-out-of-range"},
		{"setting range low", h.set("run-refusals", `{"delay_ms":-1}`), 422, "setting-out-of-range"},
		{"fail rate range", h.set("run-refusals", `{"fail_rate":1.5}`), 422, "setting-out-of-range"},
		{"body not object", h.set("run-refusals", `[1]`), 422, "body-invalid"},
		{"wrong control method", h.control("GET", "/scopes", ""), 405, "method-not-allowed"},
		{"unknown control path", h.control("GET", "/nothing", ""), 404, "control-path-unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.r.status != c.status {
				t.Fatalf("status = %d (%s), want %d", c.r.status, c.r.body, c.status)
			}
			doc := c.r.json(t)
			if err := schema.Validate(doc); err != nil {
				t.Fatalf("not a Refusal document: %v (%s)", err, c.r.body)
			}
			if doc["reason_code"] != c.code {
				t.Fatalf("reason_code = %v, want %s", doc["reason_code"], c.code)
			}
		})
	}
	t.Run("unknown setting lists the declared names", func(t *testing.T) {
		doc := h.set("run-refusals", `{"nope":1}`).json(t)
		if allowed, _ := doc["allowed"].([]any); len(allowed) != len(h.eng.Manifest().Settings) {
			t.Fatalf("allowed = %v", doc["allowed"])
		}
	})
}

func TestSettingsAreAllOrNothing(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-atomic")
	if r := h.set("run-atomic", `{"status_code":503,"delay_ms":-5}`); r.status != 422 {
		t.Fatalf("status = %d", r.status)
	}
	if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
		t.Fatalf("a refused request still applied status_code: GET /orders = %d", r.status)
	}
}

func TestFaultSettingsChangeTheAnswerAndAreRecorded(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-faults")
	f := h.feed("run-faults")
	headers := map[string]string{"X-Tenant": "acme"}

	t.Run("global status with a declared response answers it", func(t *testing.T) {
		h.set("run-faults", `{"status_code":503}`)
		r := send(t, http.MethodGet, base+"/orders", "", nil)
		if r.status != 503 || strings.TrimSpace(string(r.body)) != `{"error":"down"}` {
			t.Fatalf("GET /orders = %d %s", r.status, r.body)
		}
		f.next("request.received")
		if ev := f.next("fault.injected"); ev["fault"] != "status_code" {
			t.Fatalf("fault.injected = %v", ev)
		}
		if ev := f.next("response.sent"); ev["outcome"] != "fault" || ev["fault"] != "status_code" {
			t.Fatalf("response.sent = %v", ev)
		}
	})
	t.Run("global status without a declared response answers a refusal document", func(t *testing.T) {
		r := send(t, http.MethodGet, base+"/orders/5", "", headers)
		if doc := r.json(t); r.status != 503 || doc["reason_code"] != "fault-injected" {
			t.Fatalf("GET /orders/5 = %d %s", r.status, r.body)
		}
	})
	t.Run("a per-operation status wins over the global one", func(t *testing.T) {
		h.set("run-faults", `{"status_code":503,"operation_status.list_orders":418}`)
		if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 418 {
			t.Fatalf("GET /orders = %d, want 418", r.status)
		}
		if r := send(t, http.MethodGet, base+"/orders/5", "", headers); r.status != 503 {
			t.Fatalf("GET /orders/5 = %d, want the global 503", r.status)
		}
	})
	t.Run("reset restores the document's answers", func(t *testing.T) {
		if r := h.control("POST", "/scopes/run-faults/reset", ""); r.status != 204 {
			t.Fatalf("reset = %d", r.status)
		}
		if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
			t.Fatalf("GET /orders after reset = %d", r.status)
		}
	})
	t.Run("a status below 200 is not forced", func(t *testing.T) {
		h.set("run-faults", `{"status_code":100}`)
		if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
			t.Fatalf("GET /orders = %d, want the document's 200", r.status)
		}
	})
}

func TestFailRateAnswers503AndEmitsFaultInjected(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-rate")
	f := h.feed("run-rate")
	h.set("run-rate", `{"fail_rate":1}`)
	if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 503 {
		t.Fatalf("fail_rate 1 answered %d", r.status)
	}
	f.next("scenario.set")
	f.next("request.received")
	if ev := f.next("fault.injected"); ev["fault"] != "fail_rate" {
		t.Fatalf("fault.injected = %v", ev)
	}
	h.set("run-rate", `{"fail_rate":0}`)
	if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
		t.Fatalf("fail_rate 0 answered %d", r.status)
	}
}

func TestFailRateIsAShareOfCalls(t *testing.T) {
	rolls := []float64{0.1, 0.9, 0.49, 0.5}
	next := 0
	h := newHarness(t, apiDocument, func(o *Options) {
		o.Rand = func() float64 { v := rolls[next%len(rolls)]; next++; return v }
	})
	base := h.open("run-share")
	h.set("run-share", `{"fail_rate":0.5}`)
	var got []int
	for range rolls {
		got = append(got, send(t, http.MethodGet, base+"/orders", "", nil).status)
	}
	want := []int{503, 200, 503, 200}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	}
}

func TestDelayWaitsAndGivesUpWhenTheScopeCloses(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-delay")
	h.set("run-delay", `{"delay_ms":150}`)
	start := time.Now()
	if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
		t.Fatalf("GET /orders = %d", r.status)
	}
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Fatalf("answered in %s with delay_ms 150", took)
	}
	h.set("run-delay", `{"delay_ms":60000}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cl := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		if resp, err := cl.Get(base + "/orders"); err == nil {
			_ = resp.Body.Close() // the answer is irrelevant: the scope was closed under it
		}
	}()
	time.Sleep(100 * time.Millisecond)
	closing := time.Now()
	if r := h.control("DELETE", "/scopes/run-delay", ""); r.status != 204 {
		t.Fatalf("DELETE = %d", r.status)
	}
	if took := time.Since(closing); took > 5*time.Second {
		t.Fatalf("closing a scope with a delayed call took %s", took)
	}
	<-done
}

func TestCorrelationAndMismatch(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-corr")
	f := h.feed("run-corr")
	trace := "4bf92f3577b34da6a3ce929d0e0e4736"
	send(t, http.MethodGet, base+"/orders", "", map[string]string{"traceparent": "00-" + trace + "-00f067aa0ba902b7-01"})
	ev := f.next("request.received")
	if ev["correlation_id"] != trace || ev["correlation_source"] != "traceparent" {
		t.Fatalf("request.received = %v", ev)
	}
	if out := f.next("response.sent"); out["correlation_id"] != trace {
		t.Fatalf("response.sent correlation = %v", out["correlation_id"])
	}
	for _, bad := range []string{"garbage", "00-" + strings.Repeat("0", 32) + "-00f067aa0ba902b7-01", "00-" + trace + "-" + strings.Repeat("0", 16) + "-01"} {
		send(t, http.MethodGet, base+"/orders", "", map[string]string{"traceparent": bad})
		ev := f.next("request.received")
		if ev["correlation_source"] != "generated" || ev["correlation_id"] == strings.Repeat("0", 32) {
			t.Fatalf("traceparent %q gave %v", bad, ev)
		}
		f.next("response.sent")
	}
	send(t, http.MethodGet, base+"/orders", "", map[string]string{"baggage": "mocks.run=run-other,mocks.step=s1"})
	mm := f.next("scope.mismatch")
	if s, _ := mm["summary"].(string); !strings.Contains(s, "run-corr") || !strings.Contains(s, "run-other") || mm["run_id"] != "run-corr" {
		t.Fatalf("scope.mismatch = %v", mm)
	}
}

func TestEventsValidateAgainstThePublishedEnvelope(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	schema := compile(t, "ecosystem-v1/mocks-event-v1.schema.json")
	f := h.feed("run-envelope") // subscribed before the scope opens, so scope.opened is seen
	base := h.open("run-envelope")
	headers := map[string]string{"X-Tenant": "acme", "Authorization": "Bearer tok-envelope-1"}
	send(t, http.MethodGet, base+"/orders/5", "", headers)
	send(t, http.MethodGet, base+"/orders/zero", "", headers)
	send(t, http.MethodGet, base+"/missing", "", nil)
	send(t, http.MethodPatch, base+"/orders", "", nil)
	h.set("run-envelope", `{"status_code":503}`)
	send(t, http.MethodGet, base+"/orders", "", nil)
	h.control("POST", "/scopes/run-envelope/reset", "")
	h.control("DELETE", "/scopes/run-envelope", "")
	kinds := map[string]bool{}
	events := f.drain()
	for _, ev := range events {
		if err := schema.Validate(ev); err != nil {
			t.Fatalf("event breaks mocks.event/v1: %v\n%v", err, ev)
		}
		kinds[ev["kind"].(string)] = true
	}
	for _, k := range []string{"scope.opened", "request.received", "response.sent", "fault.injected", "scenario.set", "scope.reset", "scope.closed"} {
		if !kinds[k] {
			t.Errorf("no %s event among %d", k, len(events))
		}
	}
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		if strings.Contains(string(b), "tok-envelope-1") {
			t.Fatalf("the Authorization credential is in an event: %s", b)
		}
	}
}

func TestPayloadIsRedactedUnlessTheOperatorNamedSafeFields(t *testing.T) {
	body := `{"sku":"abc","quantity":3}`
	post := func(h *harness, run string) map[string]any {
		base := h.open(run)
		f := h.feed(run)
		send(t, http.MethodPost, base+"/orders?channel=web", body, map[string]string{"Content-Type": "application/json"})
		return f.next("request.received")["payload"].(map[string]any)
	}
	t.Run("by default nothing of the content is exported", func(t *testing.T) {
		p := post(newHarness(t, apiDocument, nil), "run-payload-a")
		if p["redacted"] != true || !strings.HasPrefix(p["digest"].(string), "sha256:") || int(p["size"].(float64)) != len(body) {
			t.Fatalf("payload = %v", p)
		}
		if _, has := p["fields"]; has {
			t.Fatalf("payload carries fields: %v", p)
		}
	})
	t.Run("a named safe field is exported and no other", func(t *testing.T) {
		h := newHarness(t, apiDocument, func(o *Options) { o.SafeFields = []string{"sku", "channel"} })
		p := post(h, "run-payload-b")
		fields, _ := p["fields"].(map[string]any)
		if p["redacted"] != false || len(fields) != 2 || fields["sku"] != "abc" || fields["channel"] != "web" {
			t.Fatalf("payload = %v", p)
		}
		if got := h.eng.Manifest().SafeFields; len(got) != 2 {
			t.Fatalf("manifest safe_fields = %v", got)
		}
	})
}

func TestScopeLifecycle(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-life")
	t.Run("a second open of the run is refused and the first keeps answering", func(t *testing.T) {
		if r := h.control("POST", "/scopes", `{"run_id":"run-life","listener":{"protocol":"http","host":"127.0.0.1","port":0}}`); r.status != 409 {
			t.Fatalf("status = %d", r.status)
		}
		if r := send(t, http.MethodGet, base+"/orders", "", nil); r.status != 200 {
			t.Fatalf("GET = %d", r.status)
		}
	})
	t.Run("a taken port is refused", func(t *testing.T) {
		u, _ := url.Parse(base)
		r := h.control("POST", "/scopes", `{"run_id":"run-taken","listener":{"protocol":"http","host":"127.0.0.1","port":`+u.Port()+`}}`)
		if doc := r.json(t); r.status != 409 || doc["reason_code"] != "listener-unavailable" {
			t.Fatalf("status = %d %s", r.status, r.body)
		}
	})
	t.Run("two runs keep their events apart", func(t *testing.T) {
		other := h.open("run-life-other")
		fa, fb := h.feed("run-life"), h.feed("run-life-other")
		send(t, http.MethodGet, other+"/orders", "", nil)
		fb.next("response.sent")
		for _, ev := range fa.drain() {
			if ev["run_id"] != "run-life" {
				t.Fatalf("run-life saw %v", ev)
			}
			if ev["kind"] == "request.received" {
				t.Fatalf("run-life saw the call made on the other run's listener")
			}
		}
	})
	t.Run("a closed scope no longer answers", func(t *testing.T) {
		if r := h.control("DELETE", "/scopes/run-life", ""); r.status != 204 {
			t.Fatalf("DELETE = %d", r.status)
		}
		cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		if resp, err := cl.Get(base + "/orders"); err == nil {
			_ = resp.Body.Close() // the call must not have been answered
			t.Fatalf("the closed listener answered %d", resp.StatusCode)
		}
		if r := h.control("DELETE", "/scopes/run-life", ""); r.status != 404 {
			t.Fatalf("second DELETE = %d", r.status)
		}
	})
}

func TestEventStreamDeliversLiveEventsOfTheRun(t *testing.T) {
	h := newHarness(t, apiDocument, nil)
	base := h.open("run-stream")
	req, err := http.NewRequest(http.MethodGet, h.ctl.URL+controlBase+"/events?run_id=run-stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // the stream ends with the test server
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	send(t, http.MethodGet, base+"/orders", "", nil)
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(string(buf), `"kind":"response.sent"`) {
		n, err := resp.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
	}
	if !strings.Contains(string(buf), "data: {") || !strings.Contains(string(buf), `"kind":"response.sent"`) {
		t.Fatalf("stream carried %q", buf)
	}
}
