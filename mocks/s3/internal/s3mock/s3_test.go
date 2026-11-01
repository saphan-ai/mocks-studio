package s3mock

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const bkt = "acme-example-data"

func TestObjectRoundTripSingle(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	data := []byte("hello object \x00\x01\xff binary")
	put := r.put("/"+bkt+"/dir/a b+c.bin", data)
	got := r.must(call{method: "GET", path: "/" + bkt + "/dir/a b+c.bin"}, 200)
	if !bytes.Equal(got.body, data) {
		t.Fatalf("read back %q, want %q", got.body, data)
	}
	if put.header.Get("ETag") != got.header.Get("ETag") || got.header.Get("ETag") != `"`+md5Hex(data)+`"` {
		t.Fatalf("etag put=%q get=%q want md5 %q", put.header.Get("ETag"), got.header.Get("ETag"), md5Hex(data))
	}
	head := r.must(call{method: "HEAD", path: "/" + bkt + "/dir/a b+c.bin"}, 200)
	if head.header.Get("Content-Length") != fmt.Sprint(len(data)) || len(head.body) != 0 {
		t.Fatalf("head length %q body %d bytes", head.header.Get("Content-Length"), len(head.body))
	}
}

func TestObjectRoundTripMultipart(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/big.bin"
	init := r.must(call{method: "POST", path: path, query: url.Values{"uploads": {""}}}, 200)
	var ir struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(init.body, &ir); err != nil || ir.UploadID == "" {
		t.Fatalf("initiate: %v %s", err, init.body)
	}
	p1, p2, p3 := repeat('a', 5<<20), repeat('b', 5<<20+7), []byte("tail")
	var parts strings.Builder
	for i, p := range [][]byte{p1, p2, p3} {
		up := r.must(call{method: "PUT", path: path, body: p, query: url.Values{"uploadId": {ir.UploadID}, "partNumber": {fmt.Sprint(i + 1)}}}, 200)
		fmt.Fprintf(&parts, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, up.header.Get("ETag"))
	}
	done := r.must(call{method: "POST", path: path, query: url.Values{"uploadId": {ir.UploadID}},
		body: []byte("<CompleteMultipartUpload>" + parts.String() + "</CompleteMultipartUpload>")}, 200)
	if !strings.Contains(string(done.body), "-3&#34;") && !strings.Contains(string(done.body), `-3"`) {
		t.Fatalf("complete etag should end -3: %s", done.body)
	}
	want := append(append(append([]byte{}, p1...), p2...), p3...)
	got := r.must(call{method: "GET", path: path}, 200)
	if !bytes.Equal(got.body, want) {
		t.Fatalf("multipart read back differs: got %d bytes, want %d", len(got.body), len(want))
	}
	r.wantError(call{method: "PUT", path: path, body: []byte("x"), query: url.Values{"uploadId": {ir.UploadID}, "partNumber": {"1"}}}, 404, "NoSuchUpload")
}

func TestMultipartAbort(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/k"
	init := r.must(call{method: "POST", path: path, query: url.Values{"uploads": {""}}}, 200)
	id := between(string(init.body), "<UploadId>", "</UploadId>")
	r.must(call{method: "DELETE", path: path, query: url.Values{"uploadId": {id}}}, 204)
	r.wantError(call{method: "DELETE", path: path, query: url.Values{"uploadId": {id}}}, 404, "NoSuchUpload")
}

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}

func TestRangeRead(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.put("/"+bkt+"/r", []byte("0123456789"))
	for _, c := range []struct{ rng, want, cr string }{
		{"bytes=2-4", "234", "bytes 2-4/10"},
		{"bytes=7-", "789", "bytes 7-9/10"},
		{"bytes=-3", "789", "bytes 7-9/10"},
	} {
		p := r.must(call{method: "GET", path: "/" + bkt + "/r", headers: map[string]string{"Range": c.rng}}, 206)
		if string(p.body) != c.want || p.header.Get("Content-Range") != c.cr {
			t.Fatalf("%s: body %q range %q, want %q %q", c.rng, p.body, p.header.Get("Content-Range"), c.want, c.cr)
		}
	}
	r.wantError(call{method: "GET", path: "/" + bkt + "/r", headers: map[string]string{"Range": "bytes=50-60"}}, 416, "InvalidRange")
}

func TestListObjectsV2(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	for _, k := range []string{"logs/2026/a", "logs/2026/b", "logs/2027/c", "readme", "z"} {
		r.put("/"+bkt+"/"+k, []byte(k))
	}
	type listed struct {
		IsTruncated bool
		NextToken   string `xml:"NextContinuationToken"`
		KeyCount    int
		Keys        []string `xml:"Contents>Key"`
		Prefixes    []string `xml:"CommonPrefixes>Prefix"`
	}
	list := func(q url.Values) listed {
		q.Set("list-type", "2")
		var l listed
		p := r.must(call{method: "GET", path: "/" + bkt, query: q}, 200)
		if err := xml.Unmarshal(p.body, &l); err != nil {
			t.Fatalf("list xml: %v %s", err, p.body)
		}
		return l
	}
	if l := list(url.Values{"prefix": {"logs/"}, "delimiter": {"/"}}); len(l.Keys) != 0 || strings.Join(l.Prefixes, ",") != "logs/2026/,logs/2027/" {
		t.Fatalf("delimiter listing: %+v", l)
	}
	if l := list(url.Values{"prefix": {"logs/2026/"}}); strings.Join(l.Keys, ",") != "logs/2026/a,logs/2026/b" || l.KeyCount != 2 {
		t.Fatalf("prefix listing: %+v", l)
	}
	var all []string
	token := ""
	for pages := 0; pages < 10; pages++ {
		q := url.Values{"max-keys": {"2"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		l := list(q)
		all = append(all, l.Keys...)
		if !l.IsTruncated {
			break
		}
		token = l.NextToken
	}
	if strings.Join(all, ",") != "logs/2026/a,logs/2026/b,logs/2027/c,readme,z" {
		t.Fatalf("paged listing: %v", all)
	}
}

func TestBucketLifecycle(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	if p := r.must(call{method: "GET", path: "/"}, 200); !strings.Contains(string(p.body), "<Name>"+bkt+"</Name>") {
		t.Fatalf("list buckets: %s", p.body)
	}
	r.put("/"+bkt+"/k", []byte("v"))
	r.wantError(call{method: "DELETE", path: "/" + bkt}, 409, "BucketNotEmpty")
	r.must(call{method: "DELETE", path: "/" + bkt + "/k"}, 204)
	r.must(call{method: "DELETE", path: "/" + bkt}, 204)
	r.wantError(call{method: "GET", path: "/" + bkt, query: url.Values{"list-type": {"2"}}}, 404, "NoSuchBucket")
}

// One test per error; each breaks exactly one thing (law #27).

func TestErrorNoSuchKey(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.wantError(call{method: "GET", path: "/" + bkt + "/absent"}, 404, "NoSuchKey")
}

func TestErrorNoSuchBucket(t *testing.T) {
	r := newRig(t)
	r.wantError(call{method: "PUT", path: "/acme-example-nowhere/k", body: []byte("v")}, 404, "NoSuchBucket")
}

func TestErrorInvalidBucketName(t *testing.T) {
	r := newRig(t)
	r.wantError(call{method: "PUT", path: "/Bad_Name"}, 400, "InvalidBucketName")
}

func TestErrorSignatureWrongSecret(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.wantError(call{method: "GET", path: "/" + bkt + "/k", secret: "a-different-fictional-secret-key"}, 403, "SignatureDoesNotMatch")
}

func TestErrorSignatureWrongSecretPresigned(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	q := PresignQuery(r.accessKey, "a-different-fictional-secret-key", testRegion, time.Now(), time.Minute, "GET", "/"+bkt+"/k", r.addr)
	p := r.send(mustGet(t, "http://"+r.addr+"/"+bkt+"/k?"+q))
	if p.status != 403 || p.errCode() != "SignatureDoesNotMatch" {
		t.Fatalf("got %d %s", p.status, p.body)
	}
}

func TestErrorUnknownAccessKey(t *testing.T) {
	r := newRig(t)
	r.wantError(call{method: "GET", path: "/", accessKey: "AKIAEXAMPLEOTHER0001"}, 403, "InvalidAccessKeyId")
}

func TestErrorUnsigned(t *testing.T) {
	r := newRig(t)
	r.wantError(call{method: "GET", path: "/", unsigned: true}, 403, "AccessDenied")
}

func TestErrorSignedBodyChanged(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	// The signature covers the declared payload hash; the body sent differs from it.
	r.wantError(call{method: "PUT", path: "/" + bkt + "/k", body: []byte("sent"), payloadHash: bodySHA([]byte("signed"))}, 400, "XAmzContentSHA256Mismatch")
}

func TestErrorRequestTimeSkewed(t *testing.T) {
	r := newRig(t)
	r.wantError(call{method: "GET", path: "/", at: time.Now().Add(-time.Hour)}, 403, "RequestTimeTooSkewed")
}

func TestErrorBadDigest(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.wantError(call{method: "PUT", path: "/" + bkt + "/k", body: []byte("body"), headers: map[string]string{"Content-MD5": contentMD5([]byte("other"))}}, 400, "BadDigest")
}

func TestContentMD5Accepted(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.must(call{method: "PUT", path: "/" + bkt + "/k", body: []byte("body"), headers: map[string]string{"Content-MD5": contentMD5([]byte("body"))}}, 200)
}

func TestErrorInvalidDigest(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.wantError(call{method: "PUT", path: "/" + bkt + "/k", body: []byte("body"), headers: map[string]string{"Content-MD5": "not-base64!"}}, 400, "InvalidDigest")
}

func TestErrorInvalidPart(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/k"
	init := r.must(call{method: "POST", path: path, query: url.Values{"uploads": {""}}}, 200)
	id := between(string(init.body), "<UploadId>", "</UploadId>")
	r.must(call{method: "PUT", path: path, body: []byte("p"), query: url.Values{"uploadId": {id}, "partNumber": {"1"}}}, 200)
	body := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"` + strings.Repeat("0", 32) + `"</ETag></Part></CompleteMultipartUpload>`
	r.wantError(call{method: "POST", path: path, query: url.Values{"uploadId": {id}}, body: []byte(body)}, 400, "InvalidPart")
}

func TestErrorEntityTooSmall(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/k"
	init := r.must(call{method: "POST", path: path, query: url.Values{"uploads": {""}}}, 200)
	id := between(string(init.body), "<UploadId>", "</UploadId>")
	var parts strings.Builder
	for i := 1; i <= 2; i++ {
		p := r.must(call{method: "PUT", path: path, body: []byte("small"), query: url.Values{"uploadId": {id}, "partNumber": {fmt.Sprint(i)}}}, 200)
		fmt.Fprintf(&parts, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i, p.header.Get("ETag"))
	}
	r.wantError(call{method: "POST", path: path, query: url.Values{"uploadId": {id}}, body: []byte("<CompleteMultipartUpload>" + parts.String() + "</CompleteMultipartUpload>")}, 400, "EntityTooSmall")
	r.settings(map[string]any{SettingMinPartBytes: 0})
	r.must(call{method: "POST", path: path, query: url.Values{"uploadId": {id}}, body: []byte("<CompleteMultipartUpload>" + parts.String() + "</CompleteMultipartUpload>")}, 200)
}

func TestErrorMalformedCompleteBody(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/k"
	init := r.must(call{method: "POST", path: path, query: url.Values{"uploads": {""}}}, 200)
	id := between(string(init.body), "<UploadId>", "</UploadId>")
	r.wantError(call{method: "POST", path: path, query: url.Values{"uploadId": {id}}, body: []byte("<not-closed")}, 400, "MalformedXML")
}

func mustGet(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestPresignedGetAndPut(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	path := "/" + bkt + "/presigned.txt"
	putQ := PresignQuery(r.accessKey, r.secret, testRegion, time.Now(), time.Minute, "PUT", path, r.addr)
	req, _ := http.NewRequest(http.MethodPut, "http://"+r.addr+path+"?"+putQ, strings.NewReader("via presigned put"))
	if p := r.send(req); p.status != 200 {
		t.Fatalf("presigned put: %d %s", p.status, p.body)
	}
	getQ := PresignQuery(r.accessKey, r.secret, testRegion, time.Now(), time.Minute, "GET", path, r.addr)
	p := r.send(mustGet(t, "http://"+r.addr+path+"?"+getQ))
	if p.status != 200 || string(p.body) != "via presigned put" {
		t.Fatalf("presigned get: %d %q", p.status, p.body)
	}
}

func TestErrorPresignedExpired(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	q := PresignQuery(r.accessKey, r.secret, testRegion, time.Now().Add(-2*time.Hour), time.Minute, "GET", "/"+bkt+"/k", r.addr)
	p := r.send(mustGet(t, "http://"+r.addr+"/"+bkt+"/k?"+q))
	if p.status != 403 || p.errCode() != "AccessDenied" || !strings.Contains(string(p.body), "expired") {
		t.Fatalf("got %d %s", p.status, p.body)
	}
}

func TestErrorPresignedWrongMethod(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	q := PresignQuery(r.accessKey, r.secret, testRegion, time.Now(), time.Minute, "GET", "/"+bkt+"/k", r.addr)
	req, _ := http.NewRequest(http.MethodDelete, "http://"+r.addr+"/"+bkt+"/k?"+q, nil)
	if p := r.send(req); p.status != 403 || p.errCode() != "SignatureDoesNotMatch" {
		t.Fatalf("got %d %s", p.status, p.body)
	}
}

func TestSlowDownIsS3XML(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.settings(map[string]any{SettingSlowDown: true})
	p := r.do(call{method: "GET", path: "/"})
	if p.status != 503 || p.errCode() != "SlowDown" || !strings.HasPrefix(p.header.Get("Content-Type"), "application/xml") {
		t.Fatalf("got %d %s (%s)", p.status, p.body, p.header.Get("Content-Type"))
	}
	if !strings.Contains(string(p.body), "<Error><Code>SlowDown</Code><Message>Please reduce your request rate.</Message>") {
		t.Fatalf("not S3's error shape: %s", p.body)
	}
	r.settings(map[string]any{SettingSlowDown: false})
	r.must(call{method: "GET", path: "/"}, 200)
}

func TestFaultInjectedEvent(t *testing.T) {
	r := newRig(t)
	r.settings(map[string]any{SettingSlowDown: true})
	r.wantError(call{method: "GET", path: "/"}, 503, "SlowDown")
	var fault, resp string
	for _, line := range r.events() {
		switch {
		case strings.Contains(line, `"kind":"fault.injected"`):
			fault = line
		case strings.Contains(line, `"kind":"response.sent"`):
			resp = line
		}
	}
	if !strings.Contains(fault, `"fault":"slow_down"`) || !strings.Contains(fault, `"outcome":"fault"`) {
		t.Fatalf("fault.injected event: %q", fault)
	}
	if !strings.Contains(resp, `"fault":"slow_down"`) || !strings.Contains(resp, `"outcome":"fault"`) {
		t.Fatalf("response.sent should carry the fault: %q", resp)
	}
}

func TestForcedErrorStatus(t *testing.T) {
	r := newRig(t)
	r.settings(map[string]any{SettingErrorStatus: 500})
	r.wantError(call{method: "GET", path: "/"}, 500, "InternalError")
}

func TestMissingKeyPrefix(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.put("/"+bkt+"/lost/a", []byte("here"))
	r.put("/"+bkt+"/kept/a", []byte("here"))
	r.settings(map[string]any{SettingMissingPrefix: "lost/"})
	r.wantError(call{method: "GET", path: "/" + bkt + "/lost/a"}, 404, "NoSuchKey")
	r.must(call{method: "GET", path: "/" + bkt + "/kept/a"}, 200)
}

func TestSlowRead(t *testing.T) {
	r := newRig(t)
	r.makeBucket(bkt)
	r.put("/"+bkt+"/k", []byte("v"))
	r.settings(map[string]any{SettingGetLatency: 300})
	start := time.Now()
	r.must(call{method: "GET", path: "/" + bkt + "/k"}, 200)
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("read took %v, want at least 300ms", d)
	}
	start = time.Now()
	r.must(call{method: "PUT", path: "/" + bkt + "/k2", body: []byte("v")}, 200)
	if d := time.Since(start); d >= 300*time.Millisecond {
		t.Fatalf("a write took %v; the latency applies to reads only", d)
	}
}

func TestScopesKeepSeparateBuckets(t *testing.T) {
	a, b := newRig(t), newRig(t)
	a.makeBucket(bkt)
	b.wantError(call{method: "GET", path: "/" + bkt, query: url.Values{"list-type": {"2"}}}, 404, "NoSuchBucket")
}

func TestSettingsRefusals(t *testing.T) {
	r := newRig(t)
	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, r.m.ControlURL()+controlBase+"/scopes/"+r.runID+"/settings", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct{ name, body string }{
		{"unknown", `{"nope":1}`},
		{"wrong type", `{"slow_down":"yes"}`},
		{"out of range", `{"get_latency_ms":-1}`},
		{"not allowed", `{"error_status":418}`},
	} {
		if got := put(c.body); got != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", c.name, got)
		}
	}
}

func TestManifestMatchesFileAndDeclaresSettings(t *testing.T) {
	file := readFile(t, "../../mocks.manifest.json")
	if string(file) != string(ManifestBytes(DefaultControlURL)) {
		t.Fatal("mocks.manifest.json differs from what `s3-mock manifest` prints; regenerate it")
	}
}
