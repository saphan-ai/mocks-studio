package dnsmock

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"strings"
	"testing"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

const (
	knownBearer = "bearer-secret-7f3a91c2e5d84b60"
	knownToken  = "query-secret-b81d40aa93c7e215"
)

// leaks returns every (haystack, secret) pair where the secret appears in the haystack.
func leaks(haystacks map[string]string, secrets ...string) []string {
	var out []string
	for name, hay := range haystacks {
		for _, s := range secrets {
			if strings.Contains(hay, s) {
				out = append(out, name+" contains "+s)
			}
		}
	}
	return out
}

// TestNoSecretInLogOrEvent sends a known bearer token and a known ?token= value in every kind of
// DoH request the mock sees (answered GET and POST, a refused path, a refused method, an injected
// HTTP failure) and finds neither in the captured log or in any event.
func TestNoSecretInLogOrEvent(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "http")
	auth := map[string]string{"Authorization": "Bearer " + knownBearer}
	q := query(t, 1, "www.example.test", dnsmsg.TypeA)
	requests := []struct {
		name   string
		method string
		url    string
		body   []byte
		hdr    map[string]string
	}{
		{"GET answered", "GET", dohGetURL(s.addr, q, "token="+knownToken), nil, auth},
		{"POST answered", "POST", "http://" + s.addr + dohPath + "?token=" + knownToken, q, map[string]string{"Authorization": "Bearer " + knownBearer, "Content-Type": "application/dns-message"}},
		{"refused path", "GET", "http://" + s.addr + "/other?token=" + knownToken, nil, auth},
		{"refused method", "PUT", dohGetURL(s.addr, q, "token="+knownToken), nil, auth},
		{"refused content type", "POST", "http://" + s.addr + dohPath + "?token=" + knownToken, q, auth},
	}
	for _, r := range requests {
		if _, _, err := doh(r.method, r.url, r.body, r.hdr); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
	}
	s.set(t, map[string]any{"doh_http_status": 503})
	if status, _, err := doh("GET", dohGetURL(s.addr, q, "token="+knownToken), nil, auth); err != nil || status != 503 {
		t.Fatalf("injected failure: status %d err %v", status, err)
	}
	evs := tm.events(t, func(evs []map[string]any) bool { return countKind(evs, "response.sent") >= len(requests)+1 })
	hay := map[string]string{"log": tm.log.String()}
	for i, ev := range evs {
		b, _ := json.Marshal(ev)
		hay["event "+string(rune('a'+i%26))+" "+ev["kind"].(string)] = string(b)
	}
	if found := leaks(hay, knownBearer, knownToken); len(found) > 0 {
		t.Fatalf("a secret reached the log or an event: %v\nlog:\n%s", found, tm.log.String())
	}
	// Positive evidence that the credentials were seen and named: without it a mock that never
	// looked at the request would pass.
	mustContain(t, "the log", tm.log.String(), "credentials=authorization,query-param")
}

func countKind(evs []map[string]any, kind string) int {
	n := 0
	for _, e := range evs {
		if e["kind"] == kind {
			n++
		}
	}
	return n
}

// TestSecretCheckerCatchesALoggerThatPrintsTheRawRequest is the negative control of the test
// above: a log line built the way a careless logger builds it (a dump of the request) must be
// found by the very check TestNoSecretInLogOrEvent relies on.
func TestSecretCheckerCatchesALoggerThatPrintsTheRawRequest(t *testing.T) {
	req, err := http.NewRequest("GET", "http://127.0.0.1/dns-query?dns=AAAA&token="+knownToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+knownBearer)
	dump, err := httputil.DumpRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"a raw request dump":       string(dump),
		"the URL alone":            req.URL.String(),
		"the Authorization header": req.Header.Get("Authorization"),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			found := leaks(map[string]string{"log": "dns-mock: query " + line}, knownBearer, knownToken)
			if len(found) == 0 {
				t.Fatalf("the secret check missed %s", name)
			}
		})
	}
	if found := leaks(map[string]string{"log": "dns-mock: query credentials=authorization,query-param"}, knownBearer, knownToken); len(found) != 0 {
		t.Fatalf("the secret check flags a line that carries only credential names: %v", found)
	}
}
