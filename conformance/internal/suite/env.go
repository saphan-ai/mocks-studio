package suite

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Exit codes of the runner; documented in spec/MOCK_ADAPTER_V1.md.
const (
	ExitPass        = 0 // every check passed
	ExitFail        = 1 // a check failed
	ExitRefused     = 2 // the mock was refused before checking
	ExitUnreachable = 3 // the mock could not be reached
)

// Defaults of the runner's timing settings; each has a flag.
const (
	DefaultRequestTimeout = 5 * time.Second
	DefaultEventWait      = 3 * time.Second
	DefaultListenHost     = "127.0.0.1"
	maxResponseBytes      = 1 << 20
)

// Options configures one run.
type Options struct {
	ManifestPath   string        // manifest file; its control.url is where the mock is reached
	ControlURL     string        // control endpoint; overrides the manifest's, or supplies the manifest
	ListenHost     string        // host the suite asks the mock to open listeners on
	RequestTimeout time.Duration // bound on every request the suite makes
	EventWait      time.Duration // how long the suite waits for an event it expects
	Out            io.Writer
}

// env is the state one run shares between checks.
type env struct {
	opts     Options
	ctx      context.Context
	manifest *Manifest
	control  string
	client   *http.Client
	probeCl  *http.Client
	events   *eventStore
	rep      *report
	nonce    string
	runs     int
}

func newEnv(ctx context.Context, o Options, rep *report) *env {
	return &env{
		opts: o, ctx: ctx, rep: rep, nonce: randHex(3),
		client:  &http.Client{Timeout: o.RequestTimeout},
		probeCl: &http.Client{Timeout: o.RequestTimeout, Transport: &http.Transport{DisableKeepAlives: true}},
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS entropy source failing is not recoverable
	}
	return hex.EncodeToString(b)
}

// reply is one control-surface answer.
type reply struct {
	status int
	body   []byte
	err    error // transport error: nothing was answered
}

func (r reply) json() (map[string]any, bool) {
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		return nil, false
	}
	return m, true
}

func (e *env) call(method, path string, body any) reply {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return reply{err: err}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, e.control+path, rdr)
	if err != nil {
		return reply{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer func() { _ = resp.Body.Close() }() // fully read below; nothing to flush
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return reply{status: resp.StatusCode, body: b, err: err}
}

// scopeHandle is one open scope the suite holds.
type scopeHandle struct {
	runID    string
	protocol string
	host     string
	port     int
}

func (h *scopeHandle) addr() string { return net.JoinHostPort(h.host, fmt.Sprint(h.port)) }

func (e *env) newRunID() string {
	e.runs++
	return fmt.Sprintf("conf-%s-%02d", e.nonce, e.runs)
}

func (e *env) freePort() (int, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(e.opts.ListenHost, "0"))
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }() // released so the mock can bind it
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func (e *env) openRequest(runID, protocol string, port int) map[string]any {
	return map[string]any{"run_id": runID, "listener": map[string]any{"protocol": protocol, "host": e.opts.ListenHost, "port": port}}
}

// open opens a scope on a fresh run id and a free port.
func (e *env) open(protocol string) (*scopeHandle, reply) {
	port, err := e.freePort()
	if err != nil {
		return nil, reply{err: err}
	}
	h := &scopeHandle{runID: e.newRunID(), protocol: protocol, host: e.opts.ListenHost, port: port}
	r := e.call(http.MethodPost, controlBase+"/scopes", e.openRequest(h.runID, protocol, port))
	if r.err != nil || r.status != http.StatusCreated {
		return nil, r
	}
	return h, r
}

// closeScope removes a scope; a failure is reported as a remark, never swallowed.
func (e *env) closeScope(h *scopeHandle) {
	if h == nil {
		return
	}
	r := e.call(http.MethodDelete, controlBase+"/scopes/"+h.runID, nil)
	if r.err != nil || (r.status != http.StatusNoContent && r.status != http.StatusNotFound) {
		e.rep.remark(fmt.Sprintf("cleanup of scope %s did not complete (status %d, error %v); the mock may still hold it", h.runID, r.status, r.err))
	}
}

// openScoped opens a scope and returns a cleanup. On failure the outcome says why.
func (e *env) openScoped(protocol string) (*scopeHandle, func(), *outcome) {
	h, r := e.open(protocol)
	if h == nil {
		o := fail("a scope could not be opened for the check: %s", describeReply(r))
		return nil, func() {}, &o
	}
	return h, func() { e.closeScope(h) }, nil
}

func describeReply(r reply) string {
	if r.err != nil {
		return "no answer (" + r.err.Error() + ")"
	}
	return fmt.Sprintf("status %d, body %s", r.status, truncate(string(r.body), 200))
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

const controlBase = "/_mock/v1"
