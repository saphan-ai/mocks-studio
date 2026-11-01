package ocimock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRepo = "acme-example/app"

// lockedBuffer is a log sink the race detector accepts.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// result is one HTTP answer, fully read.
type result struct {
	status int
	header http.Header
	body   []byte
}

// errCode is the code of the first entry of a registry error document, or "".
func (r result) errCode() string {
	var doc struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if json.Unmarshal(r.body, &doc) != nil || len(doc.Errors) == 0 {
		return ""
	}
	return doc.Errors[0].Code
}

// env is one mock with one open scope, its log and its event feed.
type env struct {
	t      *testing.T
	m      *Mock
	runID  string
	base   string // http://host:port of the scope's listener
	logs   *lockedBuffer
	feed   *subscriber
	events [][]byte
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback networking is required for these tests: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith starts a mock whose clock is now (nil: the real one) and opens one scope.
func newEnvWith(t *testing.T, now func() time.Time) *env {
	t.Helper()
	logs := &lockedBuffer{}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0", Logger: log.New(logs, "", 0), Now: now})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(m.Close)
	e := &env{t: t, m: m, logs: logs, feed: m.subscribe("")}
	e.runID = "run-" + randomHex(4)
	port := freePort(t)
	res := e.control(http.MethodPost, "/scopes", map[string]any{"run_id": e.runID, "listener": map[string]any{"protocol": "http", "host": "127.0.0.1", "port": port}})
	if res.status != http.StatusCreated {
		t.Fatalf("open scope: %d %s", res.status, res.body)
	}
	e.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	return e
}

func (e *env) control(method, path string, body any) result {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.m.ControlURL()+controlBase+path, rd)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return e.send(req)
}

// do sends one registry call to the scope's listener; hdr is name, value pairs.
func (e *env) do(method, path string, body []byte, hdr ...string) result {
	e.t.Helper()
	req, err := http.NewRequest(method, e.base+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return e.send(req)
}

func (e *env) send(req *http.Request) result {
	e.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: b}
}

func (e *env) set(settings map[string]any) {
	e.t.Helper()
	if res := e.control(http.MethodPut, "/scopes/"+e.runID+"/settings", settings); res.status != http.StatusOK {
		e.t.Fatalf("set %v: %d %s", settings, res.status, res.body)
	}
}

// login fetches a bearer token with the given password.
func (e *env) login(password string) (string, result) {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.base+"/token?service="+serviceName, nil)
	if err != nil {
		e.t.Fatalf("request: %v", err)
	}
	req.SetBasicAuth(Username, password)
	res := e.send(req)
	var doc struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(res.body, &doc) // a refusal has no token; the caller reads the status
	return doc.Token, res
}

// allEvents drains the feed and returns every event seen so far, decoded.
func (e *env) allEvents() []map[string]any {
	e.t.Helper()
	for {
		select {
		case line := <-e.feed.ch:
			e.events = append(e.events, line)
		default:
			out := make([]map[string]any, 0, len(e.events))
			for _, l := range e.events {
				var ev map[string]any
				if err := json.Unmarshal(l, &ev); err != nil {
					e.t.Fatalf("event is not JSON: %v", err)
				}
				out = append(out, ev)
			}
			return out
		}
	}
}

func (e *env) rawEvents() string {
	e.allEvents()
	return string(bytes.Join(e.events, []byte("\n")))
}

// image is one pushed image: a config blob, a layer blob and the manifest over them.
type image struct {
	config, layer, manifest     []byte
	configDigest, layerDigest   string
	manifestDigest, manifestRef string
}

func buildImage(layer string) image {
	im := image{config: []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`), layer: []byte(layer)}
	im.configDigest, im.layerDigest = digestOf(im.config), digestOf(im.layer)
	doc := map[string]any{
		"schemaVersion": 2, "mediaType": mediaImageManifest,
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": im.configDigest, "size": len(im.config)},
		"layers": []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": im.layerDigest, "size": len(im.layer)}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err) // plain values only
	}
	im.manifest = b
	im.manifestDigest = digestOf(b)
	return im
}

// pushBlobMonolithic pushes a blob in one POST.
func (e *env) pushBlobMonolithic(repo string, data []byte, hdr ...string) result {
	e.t.Helper()
	return e.do(http.MethodPost, "/v2/"+repo+"/blobs/uploads/?digest="+url.QueryEscape(digestOf(data)), data, hdr...)
}

// pushBlobChunked pushes a blob as a session: POST, PATCH of every chunk, PUT with the digest.
func (e *env) pushBlobChunked(repo string, data []byte, chunk int, hdr ...string) result {
	e.t.Helper()
	start := e.do(http.MethodPost, "/v2/"+repo+"/blobs/uploads/", nil, hdr...)
	if start.status != http.StatusAccepted {
		e.t.Fatalf("start upload: %d %s", start.status, start.body)
	}
	loc := start.header.Get("Location")
	off := 0
	for off+chunk < len(data) {
		part := data[off : off+chunk]
		res := e.do(http.MethodPatch, loc, part, append([]string{"Content-Type", "application/octet-stream", "Content-Range", fmt.Sprintf("%d-%d", off, off+len(part)-1)}, hdr...)...)
		if res.status != http.StatusAccepted {
			e.t.Fatalf("patch at %d: %d %s", off, res.status, res.body)
		}
		off += len(part)
	}
	return e.do(http.MethodPut, loc+"?digest="+url.QueryEscape(digestOf(data)), data[off:], hdr...)
}

func (e *env) putManifest(repo, ref string, body []byte, ctype string, hdr ...string) result {
	e.t.Helper()
	return e.do(http.MethodPut, "/v2/"+repo+"/manifests/"+ref, body, append([]string{"Content-Type", ctype}, hdr...)...)
}

// pushImage pushes the config monolithically, the layer in chunks and the manifest by tag.
func (e *env) pushImage(repo, tag string, im image) {
	e.t.Helper()
	if res := e.pushBlobMonolithic(repo, im.config); res.status != http.StatusCreated {
		e.t.Fatalf("push config: %d %s", res.status, res.body)
	}
	if res := e.pushBlobChunked(repo, im.layer, 7); res.status != http.StatusCreated {
		e.t.Fatalf("push layer: %d %s", res.status, res.body)
	}
	if res := e.putManifest(repo, tag, im.manifest, mediaImageManifest); res.status != http.StatusCreated {
		e.t.Fatalf("push manifest: %d %s", res.status, res.body)
	}
}

func contains(haystack string, needles ...string) []string {
	var hit []string
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			hit = append(hit, n)
		}
	}
	return hit
}
