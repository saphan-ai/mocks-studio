package ocimock

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"
)

// secretsOf are the two assets (law #56): the registry password a caller sends and the
// bearer token the mock issues, plus the Basic header that carries the password.
func secretsOf(token string) []string {
	return []string{Password, token, base64.StdEncoding.EncodeToString([]byte(Username + ":" + Password))}
}

// loginAndPush logs in with the known password and pushes an image with the bearer token,
// returning the token.
func loginAndPush(t *testing.T, e *env) string {
	t.Helper()
	e.set(map[string]any{settingAuthRequired: true})
	if res := e.do(http.MethodGet, "/v2/", nil); res.status != http.StatusUnauthorized {
		t.Fatalf("anonymous call with auth_required: %d, want 401", res.status)
	}
	tok, res := e.login(Password)
	if res.status != http.StatusOK || tok == "" {
		t.Fatalf("login: %d %s", res.status, res.body)
	}
	bearer := []string{"Authorization", "Bearer " + tok}
	im := buildImage("layer bytes that must never reach a log or an event: BLOB-CONTENT-MARKER")
	if r := e.pushBlobMonolithic(testRepo, im.config, bearer...); r.status != http.StatusCreated {
		t.Fatalf("push config: %d %s", r.status, r.body)
	}
	if r := e.pushBlobChunked(testRepo, im.layer, 9, bearer...); r.status != http.StatusCreated {
		t.Fatalf("push layer: %d %s", r.status, r.body)
	}
	if r := e.putManifest(testRepo, "v1", im.manifest, mediaImageManifest, bearer...); r.status != http.StatusCreated {
		t.Fatalf("push manifest: %d %s", r.status, r.body)
	}
	if r := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/v1", nil, bearer...); r.status != http.StatusOK {
		t.Fatalf("pull manifest: %d", r.status)
	}
	return tok
}

// The asset is the registry password and the bearer token; the proof is that neither, nor
// the Basic header, nor a blob's bytes, appears in the captured log or in any event.
func TestNoSecretInLogOrEvents(t *testing.T) {
	e := newEnv(t)
	tok := loginAndPush(t, e)
	haystack := map[string]string{"log": e.logs.String(), "events": e.rawEvents()}
	for where, text := range haystack {
		if hit := contains(text, append(secretsOf(tok), "BLOB-CONTENT-MARKER")...); len(hit) != 0 {
			t.Errorf("%s carries a secret or blob bytes: %q", where, hit)
		}
		if text == "" {
			t.Errorf("the %s is empty, so finding no secret in it proves nothing", where)
		}
	}
	if n := len(e.allEvents()); n < 10 {
		t.Errorf("only %d events were captured for a login and a push; the check has nothing to read", n)
	}
}

// Negative control: a logger that prints the raw request is caught by the same finder, so
// a green TestNoSecretInLogOrEvents is not a finder that cannot see.
func TestNegativeControlRawRequestLoggerIsCaught(t *testing.T) {
	e := newEnv(t)
	s := e.m.scopes[e.runID]
	var raw lockedBuffer
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dump, err := httputil.DumpRequest(r, false)
		if err != nil {
			t.Errorf("dump: %v", err)
		}
		_, _ = raw.Write(dump)
		s.serveHTTP(w, r)
	}))
	defer spy.Close()
	e.base = spy.URL
	tok := loginAndPush(t, e)
	if hit := contains(raw.String(), secretsOf(tok)...); len(hit) < 2 {
		t.Fatalf("a logger printing raw requests leaked only %d of the secrets; the finder is blind", len(hit))
	}
}

// The mock's own redaction is what keeps the token out of an event: this proves the
// events are built from safe fields only, whatever the headers hold.
func TestEventsExportOnlyDeclaredSafeFields(t *testing.T) {
	e := newEnv(t)
	loginAndPush(t, e)
	var manifest struct {
		SafeFields []string `json:"safe_fields"`
	}
	mustJSON(t, ManifestBytes("http://127.0.0.1:9100"), &manifest)
	safe := map[string]bool{}
	for _, f := range manifest.SafeFields {
		safe[f] = true
	}
	for _, ev := range e.allEvents() {
		p, ok := ev["payload"].(map[string]any)
		if !ok {
			continue
		}
		if p["redacted"] != false {
			t.Errorf("payload of %v is not the clear-fields form: %v", ev["kind"], p)
			continue
		}
		for k := range p["fields"].(map[string]any) {
			if !safe[k] {
				t.Errorf("event %v exports %q, which the manifest does not declare safe", ev["kind"], k)
			}
		}
	}
	if !strings.Contains(e.rawEvents(), `"operation":"manifest.put"`) {
		t.Error("the push left no manifest.put operation in the events")
	}
}
