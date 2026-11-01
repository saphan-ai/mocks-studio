package dnsmock

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

// validRecords is one record set the mock accepts; each refusal case below changes exactly one
// field of it, so a refusal is the answer to that field alone.
func validRecords() []map[string]any {
	return []map[string]any{
		{"name": "www.example.test", "type": "A", "value": "192.0.2.10", "ttl": 30},
		{"name": "www.example.test", "type": "AAAA", "value": "2001:db8::10"},
		{"name": "alias.example.test", "type": "CNAME", "value": "www.example.test"},
		{"name": "example.test", "type": "MX", "value": "10 mail.example.test"},
		{"name": "example.test", "type": "TXT", "value": "hello"},
	}
}

func mutated(f func(r []map[string]any)) []map[string]any {
	r := validRecords()
	f(r)
	return r
}

func TestValidRecordSetIsAccepted(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, validRecords()...)
}

func TestSettingRefusals(t *testing.T) {
	huge := make([]map[string]any, maxDeclared+1)
	for i := range huge {
		huge[i] = rec("www.example.test", "TXT", "x")
	}
	cases := []struct {
		field   string
		setting map[string]any
		code    string
		allowed bool // the refusal must list what is allowed
	}{
		{"records: not JSON", map[string]any{"records": "{nope"}, "setting-type-invalid", false},
		{"records: an object instead of an array", map[string]any{"records": `{"name":"a.example.test"}`}, "setting-type-invalid", false},
		{"records: unknown member", map[string]any{"records": `[{"name":"a.example.test","type":"A","value":"192.0.2.1","extra":1}]`}, "setting-type-invalid", false},
		{"record name: outside the zones", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["name"] = "www.example.com" }))}, "setting-out-of-range", true},
		{"record name: empty label", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["name"] = "a..example.test" }))}, "setting-type-invalid", false},
		{"record type: not answered", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["type"] = "SRV" }))}, "setting-out-of-range", true},
		{"A value: outside the reserved ranges", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["value"] = "8.8.8.8" }))}, "setting-out-of-range", true},
		{"A value: an IPv6 address", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["value"] = "2001:db8::1" }))}, "setting-type-invalid", false},
		{"AAAA value: outside 2001:db8::/32", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[1]["value"] = "2606:4700::1" }))}, "setting-out-of-range", true},
		{"AAAA value: an IPv4 address", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[1]["value"] = "192.0.2.1" }))}, "setting-type-invalid", false},
		{"CNAME target: outside the zones", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[2]["value"] = "www.example.com" }))}, "setting-out-of-range", true},
		{"MX value: no preference", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[3]["value"] = "mail.example.test" }))}, "setting-type-invalid", false},
		{"MX exchange: outside the zones", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[3]["value"] = "10 mail.example.com" }))}, "setting-out-of-range", true},
		{"TXT value: empty", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[4]["value"] = "" }))}, "setting-out-of-range", false},
		{"ttl: above the limit", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[0]["ttl"] = maxTTL + 1 }))}, "setting-out-of-range", false},
		{"CNAME beside another record at its name", map[string]any{"records": jsonStr(mutated(func(r []map[string]any) { r[2]["name"] = "www.example.test" }))}, "setting-type-invalid", false},
		{"records: more than the limit", map[string]any{"records": jsonStr(huge)}, "setting-out-of-range", false},
		{"blocked: not JSON", map[string]any{"blocked": "ads.example.test"}, "setting-type-invalid", false},
		{"blocked name: outside the zones", map[string]any{"blocked": `["ads.example.com"]`}, "setting-out-of-range", true},
		{"block_answer: not allowed", map[string]any{"block_answer": "drop"}, "setting-out-of-range", true},
		{"sinkhole_ip: not an address", map[string]any{"sinkhole_ip": "sink"}, "setting-type-invalid", false},
		{"sinkhole_ip: outside the reserved ranges", map[string]any{"sinkhole_ip": "10.0.0.1"}, "setting-out-of-range", true},
		{"servfail: a string", map[string]any{"servfail": "yes"}, "setting-type-invalid", false},
		{"drop_udp: a number", map[string]any{"drop_udp": 1}, "setting-type-invalid", false},
		{"latency_ms: above the maximum", map[string]any{"latency_ms": 10001}, "setting-out-of-range", false},
		{"latency_ms: below the minimum", map[string]any{"latency_ms": -1}, "setting-out-of-range", false},
		{"latency_ms: not an integer", map[string]any{"latency_ms": 1.5}, "setting-type-invalid", false},
		{"doh_http_status: below 200", map[string]any{"doh_http_status": 199}, "setting-out-of-range", false},
		{"doh_http_status: above 599", map[string]any{"doh_http_status": 600}, "setting-out-of-range", false},
		{"unknown setting", map[string]any{"no_such_setting": 1}, "setting-unknown", true},
	}
	tm := startMock(t)
	s := tm.open(t, "dns")
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			status, doc := tm.control(t, "PUT", "/scopes/"+s.runID+"/settings", c.setting)
			if status != http.StatusUnprocessableEntity || doc["reason_code"] != c.code {
				t.Fatalf("status %d body %v, want 422 %s", status, doc, c.code)
			}
			msg, _ := doc["message"].(string)
			if !strings.Contains(msg, "refused") {
				t.Fatalf("message %q does not say what was refused", msg)
			}
			if _, has := doc["allowed"]; has != c.allowed {
				t.Fatalf("allowed listed = %v, want %v (body %v)", has, c.allowed, doc)
			}
		})
	}
}

func TestARefusedSettingAppliesNothing(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	good := jsonString(t, []map[string]any{rec("www.example.test", "A", "192.0.2.10")})
	status, _ := tm.control(t, "PUT", "/scopes/"+s.runID+"/settings", map[string]any{"records": good, "latency_ms": 99999})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", status)
	}
	if m := answer(t, transports[0], s, query(t, 1, "www.example.test", dnsmsg.TypeA)); m.Header.Rcode != dnsmsg.RcodeNXDomain {
		t.Fatalf("rcode %s: the valid half of a refused request was applied", dnsmsg.RcodeName(m.Header.Rcode))
	}
}

func TestSettingsOfAnUnknownRunAreRefused(t *testing.T) {
	tm := startMock(t)
	status, doc := tm.control(t, "PUT", "/scopes/no-such-run/settings", map[string]any{"servfail": true})
	if status != http.StatusNotFound || doc["reason_code"] != "run-unknown" {
		t.Fatalf("status %d body %v, want 404 run-unknown", status, doc)
	}
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain values only
	}
	return string(b)
}

func TestServedManifestIsTheFileExceptForTheControlURL(t *testing.T) {
	tm := startMock(t)
	file, err := os.ReadFile("mocks.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", tm.ControlURL()+controlBase+"/manifest", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // decoded below
	var served, onDisk map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&served); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(file, &onDisk); err != nil {
		t.Fatal(err)
	}
	if got := served["control"].(map[string]any)["url"]; got != tm.ControlURL() {
		t.Fatalf("served control.url = %v, want the live %s", got, tm.ControlURL())
	}
	delete(served, "control")
	delete(onDisk, "control")
	if jsonStr(served) != jsonStr(onDisk) {
		t.Fatalf("served manifest differs from the file beyond control.url")
	}
}

func TestEveryDefaultSatisfiesItsOwnDeclaration(t *testing.T) {
	tm := startMock(t)
	for _, name := range tm.defOrder {
		d := tm.defs[name]
		if p := d.check(d.Default); p != nil {
			t.Fatalf("default of %s is refused by its own declaration: %s", name, p.msg)
		}
	}
}

func TestManifestProbeIsAValidQuery(t *testing.T) {
	var doc struct {
		Conformance struct {
			Probe struct {
				HTTP struct{ Method, Path string }
			}
		}
	}
	raw, err := os.ReadFile("mocks.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	tm := startMock(t)
	s := tm.open(t, "http")
	p := doc.Conformance.Probe.HTTP
	status, body, err := doh(p.Method, "http://"+s.addr+p.Path, nil, nil)
	if err != nil || status != 200 {
		t.Fatalf("probe status %d err %v", status, err)
	}
	m, err := dnsmsg.Unpack(body)
	if err != nil || m.Questions[0].Name != "probe.example.test" {
		t.Fatalf("probe answer %+v err %v, want a response to probe.example.test", m, err)
	}
}
