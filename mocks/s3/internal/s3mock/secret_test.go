package s3mock

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// secretsIn reports the first secret the haystack carries: the access secret, an
// Authorization header value, or a signature (header or presigned).
func secretsIn(hay string, secrets ...string) string {
	return containsAny(hay, secrets...)
}

// TestNoSecretInLogOrEvents signs requests with a known secret (header, presigned, and
// a wrong one) and finds neither the secret nor any signature in the log or an event.
func TestNoSecretInLogOrEvents(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/k"
	var signatures []string

	at := time.Now()
	r.put(path, []byte("body"))
	r.must(call{method: "GET", path: path, at: at}, 200)
	r.wantError(call{method: "GET", path: path, secret: "a-different-fictional-secret-key"}, 403, "SignatureDoesNotMatch")

	// The signature a header-signed request carries, recomputed the way the client did.
	date := at.UTC().Format(amzDateLayout)
	sr := sigRequest{Method: "GET", Path: path, Query: url.Values{}, Host: r.addr, PayloadHash: bodySHA(nil),
		Headers: map[string]string{"x-amz-date": date, "x-amz-content-sha256": bodySHA(nil)}}
	auth := SignHeaders(r.accessKey, r.secret, testRegion, at, sr, []string{"host", "x-amz-content-sha256", "x-amz-date"})
	signatures = append(signatures, auth[strings.LastIndex(auth, "Signature=")+len("Signature="):], auth)

	q := PresignQuery(r.accessKey, r.secret, testRegion, time.Now(), time.Minute, "GET", path, r.addr)
	pre := r.send(mustGet(t, "http://"+r.addr+path+"?"+q))
	if pre.status != 200 {
		t.Fatalf("presigned get: %d %s", pre.status, pre.body)
	}
	signatures = append(signatures, between(q+"&", "X-Amz-Signature=", "&"))
	r.settings(map[string]any{SettingSlowDown: true, SettingSecretKey: "a-new-fictional-secret-access-key"})
	r.wantError(call{method: "GET", path: "/"}, 503, "SlowDown")

	haystack := r.log.String() + "\n" + strings.Join(r.events(), "\n")
	if !strings.Contains(haystack, "op=GetObject") || !strings.Contains(haystack, `"kind":"response.sent"`) {
		t.Fatalf("the log or the events are empty; the proof reads nothing:\n%s", haystack)
	}
	if leaked := secretsIn(haystack, append([]string{r.secret, "a-new-fictional-secret-access-key", "a-different-fictional-secret-key"}, signatures...)...); leaked != "" {
		t.Fatalf("a secret or signature reached the log or an event: %q", leaked)
	}
}

// TestSecretCheckCatchesARawLogger is the negative control: a logger that prints the
// raw request carries the Authorization header, and the check above must see it.
func TestSecretCheckCatchesARawLogger(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest(http.MethodGet, "http://"+r.addr+"/", nil)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=x, SignedHeaders=host, Signature="+strings.Repeat("ab", 32))
	raw := new(strings.Builder)
	_ = req.Header.Write(raw)
	if secretsIn(raw.String(), strings.Repeat("ab", 32)) == "" {
		t.Fatal("the secret check did not see a signature in a raw header dump")
	}
}
