package s3mock

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const testRegion = "us-east-1"

// syncBuffer is a log sink the server writes and the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// rig is a started mock with one open scope and a signing client.
type rig struct {
	t     *testing.T
	m     *Mock
	log   *syncBuffer
	sub   *subscriber
	runID string
	addr  string
	// credentials the client signs with; the scope verifies the defaults
	accessKey, secret string
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func newRig(t *testing.T) *rig {
	t.Helper()
	log := &syncBuffer{}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0", Log: log})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(m.Close)
	r := &rig{t: t, m: m, log: log, runID: "run-" + randomHex(4), accessKey: DefaultAccessKey, secret: DefaultSecretKey}
	r.sub = m.subscribe("")
	r.addr = freeAddr(t)
	host, port, _ := net.SplitHostPort(r.addr)
	var p int
	_, _ = fmt.Sscan(port, &p)
	body, _ := json.Marshal(openRequest{RunID: r.runID, Listener: listenerSpec{Protocol: "http", Host: host, Port: p}})
	resp, err := http.Post(m.ControlURL()+controlBase+"/scopes", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("open scope: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	return r
}

func (r *rig) settings(kv map[string]any) {
	r.t.Helper()
	body, _ := json.Marshal(kv)
	req, _ := http.NewRequest(http.MethodPut, r.m.ControlURL()+controlBase+"/scopes/"+r.runID+"/settings", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		r.t.Fatalf("settings %v: %v %v", kv, resp, err)
	}
	_ = resp.Body.Close()
}

// call is one signed request; the fields mutate exactly one thing each when a test needs a break.
type call struct {
	method, path string
	query        url.Values
	body         []byte
	headers      map[string]string
	secret       string // overrides the rig's signing secret
	accessKey    string // overrides the rig's signing access key
	unsigned     bool
	at           time.Time
	payloadHash  string // overrides the x-amz-content-sha256 value sent AND signed
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

// errCode reads the <Code> of an S3 error body.
func (p reply) errCode() string {
	var e s3Error
	if xml.Unmarshal(p.body, &e) != nil {
		return ""
	}
	return e.Code
}

func (r *rig) do(c call) reply {
	r.t.Helper()
	if c.query == nil {
		c.query = url.Values{}
	}
	if c.at.IsZero() {
		c.at = time.Now()
	}
	secret, ak := r.secret, r.accessKey
	if c.secret != "" {
		secret = c.secret
	}
	if c.accessKey != "" {
		ak = c.accessKey
	}
	hash := bodySHA(c.body)
	if c.payloadHash != "" {
		hash = c.payloadHash
	}
	u := "http://" + r.addr + c.path
	if len(c.query) > 0 {
		u += "?" + canonicalQuery(c.query, false)
	}
	req, err := http.NewRequest(c.method, u, bytes.NewReader(c.body))
	if err != nil {
		r.t.Fatal(err)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if !c.unsigned {
		date := c.at.UTC().Format(amzDateLayout)
		req.Header.Set("x-amz-date", date)
		req.Header.Set("x-amz-content-sha256", hash)
		hdrs := map[string]string{"x-amz-date": date, "x-amz-content-sha256": hash}
		signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
		sr := sigRequest{Method: c.method, Path: c.path, Query: c.query, Headers: hdrs, Host: r.addr, PayloadHash: hash}
		req.Header.Set("Authorization", SignHeaders(ak, secret, testRegion, c.at, sr, signed))
	}
	return r.send(req)
}

func (r *rig) send(req *http.Request) reply {
	r.t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return reply{resp.StatusCode, resp.Header, b}
}

func (r *rig) must(c call, status int) reply {
	r.t.Helper()
	p := r.do(c)
	if p.status != status {
		r.t.Fatalf("%s %s: status %d (%s), want %d", c.method, c.path, p.status, p.body, status)
	}
	return p
}

func (r *rig) wantError(c call, status int, code string) {
	r.t.Helper()
	p := r.do(c)
	if p.status != status || p.errCode() != code {
		r.t.Fatalf("%s %s: got %d %q (%s), want %d %s", c.method, c.path, p.status, p.errCode(), p.body, status, code)
	}
}

func (r *rig) makeBucket(name string) {
	r.t.Helper()
	r.must(call{method: "PUT", path: "/" + name}, 200)
}

func (r *rig) put(path string, data []byte) reply {
	r.t.Helper()
	return r.must(call{method: "PUT", path: path, body: data}, 200)
}

// events closes the scope and returns every event emitted so far, as raw JSON lines.
func (r *rig) events() []string {
	r.t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, r.m.ControlURL()+controlBase+"/scopes/"+r.runID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		r.t.Fatalf("close scope: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	var out []string
	for {
		select {
		case line := <-r.sub.ch:
			out = append(out, string(line))
		default:
			return out
		}
	}
}

func contentMD5(b []byte) string {
	s := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func repeat(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func containsAny(hay string, needles ...string) string {
	for _, n := range needles {
		if n != "" && strings.Contains(hay, n) {
			return n
		}
	}
	return ""
}
