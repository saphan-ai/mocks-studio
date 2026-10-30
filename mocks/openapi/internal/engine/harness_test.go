package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

const repoSpec = "../../../../spec/"

// apiDocument is the document most engine tests serve: one operation per kind of answer a mock gives.
const apiDocument = `
openapi: 3.1.0
info: {title: Test API, version: "3"}
paths:
  /orders:
    get:
      operationId: listOrders
      responses:
        "200":
          description: ok
          content: {application/json: {example: [{id: 1}]}}
        "503":
          description: down
          content: {application/json: {example: {error: down}}}
    post:
      operationId: createOrder
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [sku, quantity]
              additionalProperties: false
              properties:
                sku: {type: string, minLength: 3}
                quantity: {type: integer, minimum: 1}
      responses:
        "201":
          description: created
          content: {application/json: {schema: {type: object, properties: {id: {type: integer}, sku: {type: string}}}}}
  /orders/{orderId}:
    get:
      operationId: getOrder
      parameters:
        - {name: orderId, in: path, required: true, schema: {type: integer, minimum: 1}}
        - {name: X-Tenant, in: header, required: true, schema: {type: string, pattern: "^[a-z]+$"}}
        - {name: limit, in: query, schema: {type: integer, maximum: 100}}
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                required: [id, name, tags]
                properties:
                  id: {type: integer, minimum: 1}
                  name: {type: string}
                  tags: {type: array, items: {type: string}}
  /login:
    post:
      operationId: login
      parameters:
        - {name: pin, in: query, schema: {type: string}}
      requestBody:
        content:
          application/json:
            schema: {type: object, properties: {user: {type: string}, password: {type: string}}}
      responses:
        "200":
          description: ok
          content: {application/json: {example: {ok: true}}}
`

// syncBuffer is a log sink two goroutines may write.
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

type harness struct {
	t   *testing.T
	eng *Engine
	ctl *httptest.Server
	log *syncBuffer
}

func newHarness(t *testing.T, document string, tweak func(*Options)) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := oas.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	h := &harness{t: t, log: &syncBuffer{}}
	opts := Options{Config: Config{Name: "test-mock", ControlURL: "http://127.0.0.1:9100"}, Log: h.log, KeepAlive: time.Hour}
	if tweak != nil {
		tweak(&opts)
	}
	h.eng, err = New(doc, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.ctl = httptest.NewServer(h.eng)
	t.Cleanup(func() { h.ctl.Close(); h.eng.Close() })
	return h
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %q", r.body)
	}
	return m
}

func send(t *testing.T, method, url, body string, headers map[string]string) reply {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }() // drained below
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{resp.StatusCode, resp.Header, b}
}

func (h *harness) control(method, path, body string) reply {
	h.t.Helper()
	headers := map[string]string{}
	if body != "" {
		headers["Content-Type"] = "application/json"
	}
	return send(h.t, method, h.ctl.URL+controlBase+path, body, headers)
}

// open opens a scope on an ephemeral port and returns the base URL of its listener.
func (h *harness) open(run string) string {
	h.t.Helper()
	r := h.control(http.MethodPost, "/scopes", `{"run_id":"`+run+`","listener":{"protocol":"http","host":"127.0.0.1","port":0}}`)
	if r.status != http.StatusCreated {
		h.t.Fatalf("open %s: %d %s", run, r.status, r.body)
	}
	l, _ := r.json(h.t)["listener"].(map[string]any)
	return "http://127.0.0.1:" + strings.TrimSuffix(strings.TrimSpace(jsonNumber(l["port"])), ".0")
}

func jsonNumber(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (h *harness) set(run, body string) reply {
	return h.control(http.MethodPut, "/scopes/"+run+"/settings", body)
}

// feed collects the events of one run.
type feed struct {
	t   *testing.T
	sub *subscriber
	h   *harness
}

func (h *harness) feed(run string) *feed { return &feed{t: h.t, sub: h.eng.hub.subscribe(run), h: h} }

// next waits for the next event of the kind.
func (f *feed) next(kind string) map[string]any {
	f.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case line := <-f.sub.ch:
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err != nil {
				f.t.Fatalf("event is not JSON: %q", line)
			}
			if ev["kind"] == kind {
				return ev
			}
		case <-deadline:
			f.t.Fatalf("no %s event arrived", kind)
		}
	}
}

// drain returns every event queued now.
func (f *feed) drain() []map[string]any {
	var out []map[string]any
	for {
		select {
		case line := <-f.sub.ch:
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err == nil {
				out = append(out, ev)
			}
		default:
			return out
		}
	}
}

func compile(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	s, err := c.Compile(repoSpec + file)
	if err != nil {
		t.Fatalf("compile %s: %v", file, err)
	}
	return s
}
