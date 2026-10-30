package kubemock

import (
	"net/http"
	"net/http/httputil"
	"strings"
	"testing"
)

// The secrets this test sends. They are synthetic and appear nowhere else.
const (
	knownToken    = "acme-example-bearer-token-7f3a91c2"
	knownPassword = "acme-example-db-password-5d8e04b6"
	knownCookie   = "acme-example-session-cookie-1b6c77d0"
)

// leakedSecrets names every secret that appears verbatim in text.
func leakedSecrets(text string, secrets ...string) []string {
	var out []string
	for _, s := range secrets {
		if strings.Contains(text, s) {
			out = append(out, s)
		}
	}
	return out
}

// sendSecrets makes the calls that carry the known secrets: a create with a
// bearer token and a password in a configmap, a rejected create that carries
// the password too, and a watch that delivers the object back.
func sendSecrets(t *testing.T, s *scopeRig) {
	t.Helper()
	cm := configMap("acme-example-cm", map[string]string{"password": knownPassword, "color": "blue"})
	auth := []string{"Authorization", "Bearer " + knownToken, "Cookie", "session=" + knownCookie}
	if r := s.call(http.MethodPost, cmPath+"?access_token="+knownToken, cm, auth...); r.status != http.StatusCreated {
		t.Fatalf("create answered %d: %s", r.status, r.body)
	}
	bad := configMap("Acme-Example-Bad", map[string]string{"password": knownPassword})
	if r := s.call(http.MethodPost, cmPath, bad, auth...); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("the rejected create answered %d, want 422", r.status)
	}
	w := s.watch(cmPath + "?watch=true")
	w.mustNext()
	s.call(http.MethodGet, cmPath+"/acme-example-cm", nil, auth...)
}

func TestNoSecretReachesTheLogOrAnEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t, 0)
	s := r.open("no-secret-run")
	sendSecrets(t, s)

	log := r.log.String()
	// Positive evidence first: an empty log would pass the leak check for the wrong reason.
	for _, want := range []string{"POST " + cmPath, "Authorization=" + redactionMarker, "data.password=" + redactionMarker, "access_token=" + redactionMarker} {
		if !strings.Contains(log, want) {
			t.Fatalf("the log lacks %q, so a clean leak check would prove nothing:\n%s", want, log)
		}
	}
	if leaked := leakedSecrets(log, knownToken, knownPassword, knownCookie); len(leaked) > 0 {
		t.Fatalf("the log contains %v:\n%s", leaked, log)
	}
	var events strings.Builder
	for _, e := range s.drainEvents() {
		events.WriteString(toJSONString(e))
	}
	if events.Len() == 0 {
		t.Fatal("no events were captured, so the event check would prove nothing")
	}
	if leaked := leakedSecrets(events.String(), knownToken, knownPassword, knownCookie); len(leaked) > 0 {
		t.Fatalf("an event contains %v", leaked)
	}
}

// The negative control of the test above: the same detector, shown a logger
// that prints the raw request, must report the secrets.
func TestSecretCheckFailsAgainstALoggerThatPrintsTheRawRequest(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1"+cmPath+"?access_token="+knownToken,
		strings.NewReader(`{"data":{"password":"`+knownPassword+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+knownToken)
	req.Header.Set("Cookie", "session="+knownCookie)
	raw, err := httputil.DumpRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	if leaked := leakedSecrets(string(raw), knownToken, knownPassword, knownCookie); len(leaked) != 3 {
		t.Fatalf("the detector found %v in a raw request dump, want all three secrets", leaked)
	}
	// And the real request line, built from the very same request, is clean.
	line := requestLine("no-secret-run", req, []byte(`{"data":{"password":"`+knownPassword+`"}}`))
	if leaked := leakedSecrets(line, knownToken, knownPassword, knownCookie); len(leaked) > 0 {
		t.Fatalf("requestLine printed %v", leaked)
	}
}
