package ocimock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPushThenPullByTagAndDigest(t *testing.T) {
	e := newEnv(t)
	im := buildImage("layer bytes of the acme-example image, long enough to need several chunks")
	e.pushImage(testRepo, "v1", im)

	for _, ref := range []string{"v1", im.manifestDigest} {
		res := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/"+ref, nil)
		if res.status != http.StatusOK || !bytes.Equal(res.body, im.manifest) {
			t.Fatalf("pull manifest by %s: %d, byte-identical=%v", ref, res.status, bytes.Equal(res.body, im.manifest))
		}
		if got := res.header.Get("Docker-Content-Digest"); got != im.manifestDigest {
			t.Errorf("manifest by %s: Docker-Content-Digest %s, want %s", ref, got, im.manifestDigest)
		}
		if got := res.header.Get("Content-Type"); got != mediaImageManifest {
			t.Errorf("manifest by %s: Content-Type %s, want %s", ref, got, mediaImageManifest)
		}
		head := e.do(http.MethodHead, "/v2/"+testRepo+"/manifests/"+ref, nil)
		if head.status != http.StatusOK || len(head.body) != 0 || head.header.Get("Content-Length") != strconv.Itoa(len(im.manifest)) {
			t.Errorf("HEAD manifest by %s: status %d, body %d bytes, Content-Length %q", ref, head.status, len(head.body), head.header.Get("Content-Length"))
		}
	}
	for name, want := range map[string][]byte{im.configDigest: im.config, im.layerDigest: im.layer} {
		res := e.do(http.MethodGet, "/v2/"+testRepo+"/blobs/"+name, nil)
		if res.status != http.StatusOK || !bytes.Equal(res.body, want) {
			t.Errorf("pull blob %s: %d, byte-identical=%v", name, res.status, bytes.Equal(res.body, want))
		}
		head := e.do(http.MethodHead, "/v2/"+testRepo+"/blobs/"+name, nil)
		if head.status != http.StatusOK || head.header.Get("Content-Length") != strconv.Itoa(len(want)) {
			t.Errorf("HEAD blob %s: %d, Content-Length %q", name, head.status, head.header.Get("Content-Length"))
		}
	}
}

func TestUploadSessionRangeAndStatus(t *testing.T) {
	e := newEnv(t)
	start := e.do(http.MethodPost, "/v2/"+testRepo+"/blobs/uploads/", nil)
	loc := start.header.Get("Location")
	if start.status != http.StatusAccepted || start.header.Get("Docker-Upload-UUID") == "" {
		t.Fatalf("start: %d %v", start.status, start.header)
	}
	if res := e.do(http.MethodPatch, loc, []byte("0123456789"), "Content-Range", "0-9"); res.status != http.StatusAccepted || res.header.Get("Range") != "0-9" {
		t.Fatalf("first chunk: %d Range %q", res.status, res.header.Get("Range"))
	}
	if res := e.do(http.MethodGet, loc, nil); res.status != http.StatusNoContent || res.header.Get("Range") != "0-9" {
		t.Errorf("status: %d Range %q, want 204 and 0-9", res.status, res.header.Get("Range"))
	}
	if res := e.do(http.MethodPatch, loc, []byte("x"), "Content-Range", "3-3"); res.status != http.StatusRequestedRangeNotSatisfiable || res.errCode() != "BLOB_UPLOAD_INVALID" {
		t.Errorf("chunk at the wrong offset: %d %s, want 416 BLOB_UPLOAD_INVALID", res.status, res.errCode())
	}
	if res := e.do(http.MethodDelete, loc, nil); res.status != http.StatusNoContent {
		t.Errorf("cancel: %d, want 204", res.status)
	}
	if res := e.do(http.MethodGet, loc, nil); res.status != http.StatusNotFound {
		t.Errorf("status after cancel: %d, want 404", res.status)
	}
}

func TestTagListPagination(t *testing.T) {
	e := newEnv(t)
	im := buildImage("layer")
	e.pushImage(testRepo, "a", im)
	for _, tag := range []string{"b", "c", "d"} {
		if res := e.putManifest(testRepo, tag, im.manifest, mediaImageManifest); res.status != http.StatusCreated {
			t.Fatalf("tag %s: %d", tag, res.status)
		}
	}
	var got []string
	path := "/v2/" + testRepo + "/tags/list?n=2"
	for pages := 0; path != ""; pages++ {
		if pages > 5 {
			t.Fatal("pagination does not end")
		}
		res := e.do(http.MethodGet, path, nil)
		var doc struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if res.status != http.StatusOK || json.Unmarshal(res.body, &doc) != nil || doc.Name != testRepo {
			t.Fatalf("page %d: %d %s", pages, res.status, res.body)
		}
		got = append(got, doc.Tags...)
		path = ""
		if link := res.header.Get("Link"); link != "" {
			target, _, ok := strings.Cut(strings.TrimPrefix(link, "<"), ">")
			if !ok {
				t.Fatalf("Link %q is not <target>; rel=\"next\"", link)
			}
			path = target
		}
	}
	if fmt.Sprint(got) != "[a b c d]" {
		t.Errorf("tags over pages: %v, want [a b c d]", got)
	}
}

func TestImageIndexAndDeletes(t *testing.T) {
	e := newEnv(t)
	im := buildImage("layer")
	e.pushImage(testRepo, "v1", im)
	index, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": mediaImageIndex,
		"manifests": []any{map[string]any{"mediaType": mediaImageManifest, "digest": im.manifestDigest, "size": len(im.manifest)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := e.putManifest(testRepo, "multi", index, mediaImageIndex); res.status != http.StatusCreated {
		t.Fatalf("push index: %d %s", res.status, res.body)
	}
	if res := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/multi", nil); res.status != http.StatusOK || res.header.Get("Content-Type") != mediaImageIndex || !bytes.Equal(res.body, index) {
		t.Errorf("pull index: %d %s", res.status, res.header.Get("Content-Type"))
	}
	if res := e.do(http.MethodDelete, "/v2/"+testRepo+"/manifests/v1", nil); res.status != http.StatusAccepted {
		t.Errorf("delete tag: %d, want 202", res.status)
	}
	if res := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/v1", nil); res.errCode() != "MANIFEST_UNKNOWN" {
		t.Errorf("pull deleted tag: %d %s", res.status, res.errCode())
	}
	if res := e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/"+im.manifestDigest, nil); res.status != http.StatusOK {
		t.Errorf("manifest by digest after tag delete: %d, want 200", res.status)
	}
	if res := e.do(http.MethodDelete, "/v2/"+testRepo+"/manifests/"+im.manifestDigest, nil); res.status != http.StatusAccepted {
		t.Errorf("delete by digest: %d, want 202", res.status)
	}
	if res := e.do(http.MethodDelete, "/v2/"+testRepo+"/blobs/"+im.layerDigest, nil); res.status != http.StatusAccepted {
		t.Errorf("delete blob: %d, want 202", res.status)
	}
	if res := e.do(http.MethodGet, "/v2/"+testRepo+"/blobs/"+im.layerDigest, nil); res.errCode() != "BLOB_UNKNOWN" {
		t.Errorf("pull deleted blob: %d %s", res.status, res.errCode())
	}
}

func TestCrossMountAndCatalog(t *testing.T) {
	e := newEnv(t)
	im := buildImage("layer")
	e.pushImage(testRepo, "v1", im)
	res := e.do(http.MethodPost, "/v2/acme-example/other/blobs/uploads/?mount="+im.layerDigest+"&from="+testRepo, nil)
	if res.status != http.StatusCreated {
		t.Fatalf("mount: %d %s", res.status, res.body)
	}
	if got := e.do(http.MethodGet, "/v2/acme-example/other/blobs/"+im.layerDigest, nil); got.status != http.StatusOK || !bytes.Equal(got.body, im.layer) {
		t.Errorf("pull mounted blob: %d", got.status)
	}
	if res := e.do(http.MethodPost, "/v2/acme-example/third/blobs/uploads/?mount="+digestOf([]byte("nothing"))+"&from="+testRepo, nil); res.status != http.StatusAccepted {
		t.Errorf("mount of an unknown blob must fall back to a session: %d", res.status)
	}
	cat := e.do(http.MethodGet, "/v2/_catalog", nil)
	var doc struct {
		Repositories []string `json:"repositories"`
	}
	if json.Unmarshal(cat.body, &doc) != nil || fmt.Sprint(doc.Repositories) != "[acme-example/app acme-example/other acme-example/third]" {
		t.Errorf("catalog: %s", cat.body)
	}
}

// errorCase is one registry error, proved by a request that succeeds and the same
// request with exactly one field changed (law #27): run is called twice on fresh
// scopes, once with broken=false and once with broken=true, and the field it changes
// is named in field.
type errorCase struct {
	name   string
	field  string
	status int
	code   string
	run    func(e *env, broken bool) result
}

var otherDigest = digestOf([]byte("a well-formed digest of something never pushed"))

func pick(broken bool, ok, bad string) string {
	if broken {
		return bad
	}
	return ok
}

func TestEachErrorBreaksExactlyOneField(t *testing.T) {
	cases := []errorCase{
		{"BLOB_UNKNOWN", "the blob digest in the URL", 404, "BLOB_UNKNOWN", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+testRepo+"/blobs/"+pick(broken, im.layerDigest, otherDigest), nil)
		}},
		{"MANIFEST_UNKNOWN", "the tag in the URL", 404, "MANIFEST_UNKNOWN", func(e *env, broken bool) result {
			pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/"+pick(broken, "v1", "v2"), nil)
		}},
		{"DIGEST_INVALID upload", "the digest query of a blob upload", 400, "DIGEST_INVALID", func(e *env, broken bool) result {
			data := []byte("blob bytes")
			return e.do(http.MethodPost, "/v2/"+testRepo+"/blobs/uploads/?digest="+pick(broken, digestOf(data), otherDigest), data)
		}},
		{"DIGEST_INVALID session", "the digest query of the closing PUT", 400, "DIGEST_INVALID", func(e *env, broken bool) result {
			data := []byte("blob bytes")
			start := e.do(http.MethodPost, "/v2/"+testRepo+"/blobs/uploads/", nil)
			return e.do(http.MethodPut, start.header.Get("Location")+"?digest="+pick(broken, digestOf(data), otherDigest), data)
		}},
		{"DIGEST_INVALID manifest", "the digest reference in the URL", 400, "DIGEST_INVALID", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			return e.putManifest(testRepo, pick(broken, im.manifestDigest, otherDigest), im.manifest, mediaImageManifest)
		}},
		{"DIGEST_INVALID form", "the digest reference form", 400, "DIGEST_INVALID", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+testRepo+"/manifests/"+pick(broken, im.manifestDigest, "sha256:abc"), nil)
		}},
		{"NAME_INVALID", "the repository name", 400, "NAME_INVALID", func(e *env, broken bool) result {
			pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+pick(broken, testRepo, "Acme-Example/App")+"/tags/list", nil)
		}},
		{"NAME_UNKNOWN", "the repository name", 404, "NAME_UNKNOWN", func(e *env, broken bool) result {
			pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+pick(broken, testRepo, "acme-example/unknown")+"/tags/list", nil)
		}},
		{"BLOB_UPLOAD_UNKNOWN", "the session id in the URL", 404, "BLOB_UPLOAD_UNKNOWN", func(e *env, broken bool) result {
			start := e.do(http.MethodPost, "/v2/"+testRepo+"/blobs/uploads/", nil)
			id := start.header.Get("Docker-Upload-UUID")
			return e.do(http.MethodPatch, "/v2/"+testRepo+"/blobs/uploads/"+pick(broken, id, "00000000000000000000000000000000"), []byte("x"))
		}},
		{"MANIFEST_BLOB_UNKNOWN", "the config digest inside the manifest", 400, "MANIFEST_BLOB_UNKNOWN", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			doc := map[string]any{"schemaVersion": 2, "mediaType": mediaImageManifest,
				"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": pick(broken, im.configDigest, otherDigest), "size": len(im.config)},
				"layers": []any{}}
			b, _ := json.Marshal(doc)
			return e.putManifest(testRepo, "v2", b, mediaImageManifest)
		}},
		{"MANIFEST_INVALID content type", "the Content-Type header", 400, "MANIFEST_INVALID", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			return e.putManifest(testRepo, "v2", im.manifest, pick(broken, mediaImageManifest, "text/plain"))
		}},
		{"MANIFEST_INVALID schema", "schemaVersion inside the manifest", 400, "MANIFEST_INVALID", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			var doc map[string]any
			_ = json.Unmarshal(im.manifest, &doc)
			doc["schemaVersion"] = map[bool]int{false: 2, true: 1}[broken]
			b, _ := json.Marshal(doc)
			return e.putManifest(testRepo, "v2", b, mediaImageManifest)
		}},
		{"TAG_INVALID", "the tag in the URL", 400, "TAG_INVALID", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			return e.putManifest(testRepo, pick(broken, "v2", "-v2"), im.manifest, mediaImageManifest)
		}},
		{"PAGINATION_NUMBER_INVALID", "the n query", 400, "PAGINATION_NUMBER_INVALID", func(e *env, broken bool) result {
			pushedEnv2(e)
			return e.do(http.MethodGet, "/v2/"+testRepo+"/tags/list?n="+pick(broken, "1", "many"), nil)
		}},
		{"UNSUPPORTED path", "the path prefix", 404, "UNSUPPORTED", func(e *env, broken bool) result {
			return e.do(http.MethodGet, pick(broken, "/v2/", "/v3/"), nil)
		}},
		{"UNSUPPORTED method", "the HTTP method", 405, "UNSUPPORTED", func(e *env, broken bool) result {
			return e.do(pick(broken, http.MethodGet, http.MethodDelete), "/v2/", nil)
		}},
		{"UNAUTHORIZED registry", "the Authorization header", 401, "UNAUTHORIZED", func(e *env, broken bool) result {
			e.set(map[string]any{settingAuthRequired: true})
			tok, _ := e.login(Password)
			if broken {
				return e.do(http.MethodGet, "/v2/", nil)
			}
			return e.do(http.MethodGet, "/v2/", nil, "Authorization", "Bearer "+tok)
		}},
		{"UNAUTHORIZED login", "the password", 401, "UNAUTHORIZED", func(e *env, broken bool) result {
			_, res := e.login(pick(broken, Password, "not-the-password"))
			return res
		}},
		{"TOOMANYREQUESTS", "the rate_limit setting", 429, "TOOMANYREQUESTS", func(e *env, broken bool) result {
			e.set(map[string]any{settingRateLimit: map[bool]int{false: 100, true: 1}[broken]})
			e.do(http.MethodGet, "/v2/", nil)
			return e.do(http.MethodGet, "/v2/", nil)
		}},
		{"missing_blob", "the missing_blob setting", 404, "BLOB_UNKNOWN", func(e *env, broken bool) result {
			_, im := pushedEnv2(e)
			e.set(map[string]any{settingMissingBlob: pick(broken, "", im.layerDigest)})
			return e.do(http.MethodGet, "/v2/"+testRepo+"/blobs/"+im.layerDigest, nil)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok := c.run(newEnv(t), false)
			if ok.status >= 400 {
				t.Fatalf("with %s intact the request failed: %d %s", c.field, ok.status, ok.body)
			}
			bad := c.run(newEnv(t), true)
			if bad.status != c.status || bad.errCode() != c.code {
				t.Fatalf("with %s broken: %d %q, want %d %q (body %s)", c.field, bad.status, bad.errCode(), c.status, c.code, bad.body)
			}
		})
	}
}

// pushedEnv2 pushes the standard image into an env the case already holds.
func pushedEnv2(e *env) (*env, image) {
	im := buildImage("layer of the acme-example image")
	e.pushImage(testRepo, "v1", im)
	return e, im
}

func TestRateLimitWindowSlides(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	e := newEnvWith(t, func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})
	e.set(map[string]any{settingRateLimit: 2})
	for i := 0; i < 2; i++ {
		if res := e.do(http.MethodGet, "/v2/", nil); res.status != http.StatusOK {
			t.Fatalf("request %d within the limit: %d", i, res.status)
		}
	}
	res := e.do(http.MethodGet, "/v2/", nil)
	if res.status != http.StatusTooManyRequests || res.header.Get("Retry-After") == "" {
		t.Fatalf("request over the limit: %d, Retry-After %q", res.status, res.header.Get("Retry-After"))
	}
	mu.Lock()
	now = now.Add(rateWindow + time.Second)
	mu.Unlock()
	if res := e.do(http.MethodGet, "/v2/", nil); res.status != http.StatusOK {
		t.Errorf("request after the window: %d, want 200", res.status)
	}
}
