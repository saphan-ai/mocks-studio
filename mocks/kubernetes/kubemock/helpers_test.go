package kubemock

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testWait   = 5 * time.Second
	testNoMore = 300 * time.Millisecond
)

// syncBuffer is a log sink the tests can read while the mock writes.
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

// rig is a running mock with its log captured.
type rig struct {
	t   *testing.T
	m   *Mock
	log *syncBuffer
}

func newRig(t *testing.T, retain int) *rig {
	t.Helper()
	log := &syncBuffer{}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0", RetainEvents: retain, Log: log})
	if err != nil {
		t.Fatalf("the mock did not start on loopback: %v", err)
	}
	t.Cleanup(m.Close)
	return &rig{t: t, m: m, log: log}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no free loopback port: %v", err)
	}
	defer func() { _ = ln.Close() }() // only the number is wanted
	return ln.Addr().(*net.TCPAddr).Port
}

// scopeRig is one open scope.
type scopeRig struct {
	*rig
	runID string
	base  string
	sub   *subscriber
}

func (r *rig) open(runID string) *scopeRig {
	r.t.Helper()
	port := freePort(r.t)
	body := fmt.Sprintf(`{"run_id":%q,"listener":{"protocol":"http","host":"127.0.0.1","port":%d}}`, runID, port)
	resp, err := http.Post(r.m.ControlURL()+controlBase+"/scopes", "application/json", strings.NewReader(body))
	if err != nil {
		r.t.Fatalf("opening scope %s: %v", runID, err)
	}
	defer func() { _ = resp.Body.Close() }() // drained below
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		r.t.Fatalf("opening scope %s answered %d: %s", runID, resp.StatusCode, b)
	}
	sr := &scopeRig{rig: r, runID: runID, base: fmt.Sprintf("http://127.0.0.1:%d", port), sub: r.m.subscribe(runID)}
	r.t.Cleanup(func() { r.m.unsubscribe(sr.sub) })
	return sr
}

// reply is one answer.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// json decodes the body into a generic object.
func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("the answer is not a JSON object (%v): %s", err, r.body)
	}
	return out
}

// call sends one request with a JSON body (or none) and returns the answer.
func (s *scopeRig) call(method, path string, body any, headers ...string) reply {
	s.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			s.t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, s.base+path, rd)
	if err != nil {
		s.t.Fatalf("request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := (&http.Client{Timeout: testWait}).Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }() // drained below
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, header: resp.Header, body: b}
}

func (s *scopeRig) put(settings map[string]any) {
	s.t.Helper()
	raw, _ := json.Marshal(settings)
	req, _ := http.NewRequest(http.MethodPut, s.m.ControlURL()+controlBase+"/scopes/"+s.runID+"/settings", bytes.NewReader(raw))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("settings: %v", err)
	}
	defer func() { _ = resp.Body.Close() }() // status only
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("settings %v answered %d: %s", settings, resp.StatusCode, b)
	}
}

// stream is an open watch.
type stream struct {
	t    *testing.T
	resp *http.Response
	sc   *bufio.Scanner
}

// watch opens a watch on a path of the scope and returns once the response headers are in.
func (s *scopeRig) watch(path string) *stream { return s.watchURL(s.base + path) }

// watchURL opens a watch on a full URL.
func (s *scopeRig) watchURL(url string) *stream {
	s.t.Helper()
	resp, err := (&http.Client{}).Get(url)
	if err != nil {
		s.t.Fatalf("watch %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // already failing
		s.t.Fatalf("watch %s answered %d: %s", url, resp.StatusCode, b)
	}
	st := &stream{t: s.t, resp: resp, sc: bufio.NewScanner(resp.Body)}
	s.t.Cleanup(func() { _ = resp.Body.Close() }) // a second close is harmless
	return st
}

// watchEvent is one line of a watch.
type watchEvent struct {
	Type   string         `json:"type"`
	Object map[string]any `json:"object"`
}

func (w watchEvent) name() string { return str(path(w.Object, "metadata", "name")) }

// next reads one event; ok is false when the stream ended or nothing came in time.
func (st *stream) next(within time.Duration) (ev watchEvent, ok bool, ended bool) {
	type result struct {
		line string
		ok   bool
	}
	got := make(chan result, 1)
	go func() {
		if st.sc.Scan() {
			got <- result{st.sc.Text(), true}
			return
		}
		got <- result{}
	}()
	select {
	case r := <-got:
		if !r.ok {
			return watchEvent{}, false, true
		}
		if err := json.Unmarshal([]byte(r.line), &ev); err != nil {
			st.t.Fatalf("watch line is not an event: %v: %s", err, r.line)
		}
		return ev, true, false
	case <-time.After(within):
		return watchEvent{}, false, false
	}
}

// mustNext reads one event or fails.
func (st *stream) mustNext() watchEvent {
	st.t.Helper()
	ev, ok, ended := st.next(testWait)
	if !ok {
		st.t.Fatalf("no watch event arrived (stream ended: %v)", ended)
	}
	return ev
}

// event waits for a published event of the scope matching pred.
func (s *scopeRig) event(pred func(map[string]any) bool) (map[string]any, bool) {
	deadline := time.After(testWait)
	for {
		select {
		case line := <-s.sub.ch:
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err != nil {
				s.t.Fatalf("event is not JSON: %v", err)
			}
			if pred(ev) {
				return ev, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

func kindIs(kind string) func(map[string]any) bool {
	return func(ev map[string]any) bool { return ev["kind"] == kind }
}

// drainEvents returns what has been published so far without waiting.
func (s *scopeRig) drainEvents() []map[string]any {
	var out []map[string]any
	for {
		select {
		case line := <-s.sub.ch:
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err == nil {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func configMap(name string, data map[string]string) map[string]any {
	d := map[string]any{}
	for k, v := range data {
		d[k] = v
	}
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name}, "data": d}
}

func pod(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": "registry.invalid/acme-example-app:1"}}},
	}
}

func deployment(name string, replicas int) map[string]any {
	labels := map[string]any{"app": "acme-example-web"}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": name, "labels": labels},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": labels},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "web", "image": "registry.invalid/acme-example-web:1"}}},
			},
		},
	}
}

const (
	nsPath   = "/api/v1/namespaces/default"
	cmPath   = nsPath + "/configmaps"
	podPath  = nsPath + "/pods"
	depPath  = "/apis/apps/v1/namespaces/default/deployments"
	listPath = "/api/v1/namespaces"
)

// listRV returns the resourceVersion a list of collection is consistent with.
func (s *scopeRig) listRV(collection string) string {
	s.t.Helper()
	r := s.call(http.MethodGet, collection, nil)
	return str(path(r.json(s.t), "metadata", "resourceVersion"))
}

func decodeJSONReader(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }

func toJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func readFileForTest(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return b
}
