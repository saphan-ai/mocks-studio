package scimmock

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type jm = map[string]any

// syncBuf is a log sink safe for the mock's concurrent writers.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// env is one running mock with one open scope and a subscriber on its events.
type env struct {
	t      *testing.T
	m      *Mock
	logs   *syncBuf
	sub    *subscriber
	runID  string
	base   string // http://host:port of the scope's listener
	client *http.Client
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("no loopback socket: %v", err)
	}
	defer func() { _ = ln.Close() }() // only the port number is wanted
	return ln.Addr().(*net.TCPAddr).Port
}

func startMock(t *testing.T) (*Mock, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0", Logger: log.New(logs, "", 0)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Close)
	return m, logs
}

func newEnv(t *testing.T) *env {
	t.Helper()
	m, logs := startMock(t)
	e := &env{t: t, m: m, logs: logs, client: &http.Client{}}
	e.runID = "run-" + randomHex(3)
	e.open(e.runID)
	e.sub = m.subscribe(e.runID)
	return e
}

// open opens a scope for runID and returns its listener base URL, remembering it for the first scope.
func (e *env) open(runID string) string {
	e.t.Helper()
	port := freePort(e.t)
	st, _, body := e.control("POST", "/scopes", jm{"run_id": runID, "listener": jm{"protocol": "http", "host": "127.0.0.1", "port": port}})
	if st != http.StatusCreated {
		e.t.Fatalf("open scope %s: %d %v", runID, st, body)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if e.base == "" {
		e.base = base
	}
	return base
}

func (e *env) control(method, path string, body any) (int, http.Header, jm) {
	e.t.Helper()
	return e.send(e.m.ControlURL()+controlBase+path, method, body, nil)
}

// do sends a SCIM call to the first scope's listener.
func (e *env) do(method, path string, body any, headers ...string) (int, http.Header, jm) {
	e.t.Helper()
	return e.doAt(e.base, method, path, body, headers...)
}

func (e *env) doAt(base, method, path string, body any, headers ...string) (int, http.Header, jm) {
	e.t.Helper()
	h := map[string]string{}
	for i := 0; i+1 < len(headers); i += 2 {
		h[headers[i]] = headers[i+1]
	}
	return e.send(base+path, method, body, h)
}

func (e *env) send(url, method string, body any, headers map[string]string) (int, http.Header, jm) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	raw, _ := io.ReadAll(resp.Body)
	var out jm
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out) // a non-JSON body leaves out nil, which the assertions then notice
	}
	return resp.StatusCode, resp.Header, out
}

// events returns every event published since the last call, decoded.
func (e *env) events() []jm {
	var out []jm
	for {
		select {
		case line := <-e.sub.ch:
			var ev jm
			if err := json.Unmarshal(line, &ev); err != nil {
				e.t.Fatalf("event is not JSON: %s", line)
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (e *env) mustCreate(path string, body jm) jm {
	e.t.Helper()
	st, _, out := e.do("POST", path, body)
	if st != http.StatusCreated {
		e.t.Fatalf("POST %s %v: %d %v", path, body, st, out)
	}
	return out
}

func (e *env) createUser(userName string, extra jm) jm {
	body := jm{"schemas": []any{userURN}, "userName": userName}
	for k, v := range extra {
		body[k] = v
	}
	return e.mustCreate("/Users", body)
}

func str(m jm, key string) string {
	s, _ := m[key].(string)
	return s
}

// userNames lists the userName of every resource of a ListResponse, in order.
func userNames(list jm) []string {
	var out []string
	for _, r := range asArr(list["Resources"]) {
		out = append(out, str(r.(map[string]any), "userName"))
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
