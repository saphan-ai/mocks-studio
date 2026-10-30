package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The credentials one call carries, each in a different place.
const (
	secretPassword = "Pw!-7hunter2-body"
	secretPIN      = "PIN-482913-query"
	secretBearer   = "tok-9f3a-bearer-header"
	secretAPIKey   = "key-5c1d-apikey-header"
	secretCookie   = "sess-77aa-cookie"
)

var allSecrets = []string{secretPassword, secretPIN, secretBearer, secretAPIKey, secretCookie}

// leaked returns the secrets that appear in the output.
func leaked(output string, secrets ...string) []string {
	var found []string
	for _, s := range secrets {
		if strings.Contains(output, s) {
			found = append(found, s)
		}
	}
	return found
}

// loginCall sends one request carrying every secret and returns everything the engine logged and every event it emitted.
func loginCall(t *testing.T, h *harness, run string) (logged, events string) {
	t.Helper()
	base := h.open(run)
	f := h.feed(run)
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + secretBearer,
		"X-Api-Key":     secretAPIKey,
		"Cookie":        "session=" + secretCookie,
	}
	r := send(t, http.MethodPost, base+"/login?pin="+secretPIN, `{"user":"ann","password":"`+secretPassword+`"}`, headers)
	if r.status != 200 {
		t.Fatalf("POST /login = %d %s", r.status, r.body)
	}
	f.next("response.sent")
	var evs strings.Builder
	for _, ev := range f.drain() {
		b, _ := json.Marshal(ev)
		evs.Write(b)
	}
	return h.log.String(), evs.String()
}

// The engine never writes a credential to its log: it writes the field's name and a redaction marker.
func TestNoSecretReachesTheLog(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		name := "default log"
		if verbose {
			name = "verbose log"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, apiDocument, func(o *Options) { o.Verbose = verbose })
			logged, events := loginCall(t, h, "run-secrets")
			if got := leaked(logged, allSecrets...); len(got) > 0 {
				t.Fatalf("the log carries secrets %q:\n%s", got, logged)
			}
			if got := leaked(events, allSecrets...); len(got) > 0 {
				t.Fatalf("the events carry secrets %q:\n%s", got, events)
			}
			if !strings.Contains(logged, "call run=run-secrets") {
				t.Fatalf("the engine logged no call line, so the check above measured nothing:\n%s", logged)
			}
			if verbose {
				for _, want := range []string{`"header.Authorization"="[redacted]"`, `"query.pin"="[redacted]"`, `"body./password"="[redacted]"`} {
					if !strings.Contains(logged, want) {
						t.Fatalf("the verbose log does not name the field with a marker (%s):\n%s", want, logged)
					}
				}
			}
		})
	}
}

// Negative control: the same check against a logger that prints the raw request must fail it. Without this, a check that
// reads an empty log would pass for every logger.
func TestSecretCheckCatchesALoggerThatPrintsRawValues(t *testing.T) {
	h := newHarness(t, apiDocument, func(o *Options) {
		o.Verbose = true
		o.Redact = func(_, value string) string { return value }
	})
	logged, _ := loginCall(t, h, "run-raw")
	got := leaked(logged, allSecrets...)
	if len(got) != len(allSecrets) {
		t.Fatalf("a raw logger leaked only %q of %q; the check cannot see what it is meant to see:\n%s", got, allSecrets, logged)
	}
}

func TestRedactorKeepsOnlyOperatorSafeFieldsThatAreNotCredentials(t *testing.T) {
	redact := NewRedactor([]string{"sku", "password"})
	cases := []struct{ name, value, want string }{
		{"sku", "abc", "abc"},
		{"password", "hunter2", RedactedMarker}, // named safe, but a credential name wins
		{"quantity", "3", RedactedMarker},
		{"Authorization", "Bearer x", RedactedMarker},
	}
	for _, c := range cases {
		if got := redact(c.name, c.value); got != c.want {
			t.Errorf("redact(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSecretNames(t *testing.T) {
	for _, n := range []string{"password", "Password", "user_pin", "PIN", "client_secret", "access_token", "api-key", "apiKey", "Authorization", "Cookie", "Set-Cookie", "session_id"} {
		if !IsSecretName(n) {
			t.Errorf("%q is not recognised as a credential name", n)
		}
	}
	for _, n := range []string{"sku", "quantity", "name", "channel"} {
		if IsSecretName(n) {
			t.Errorf("%q is recognised as a credential name", n)
		}
	}
}
