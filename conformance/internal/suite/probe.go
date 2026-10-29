package suite

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// probeOpts are the extras one probe carries on top of the manifest's probe.
type probeOpts struct {
	headers map[string]string // http only
	marker  string            // appended to the body (http) or the line (tcp)
}

// probeResult is what a listener answered.
type probeResult struct {
	httpStatus int
	tcpReply   string
	err        error
}

func (e *env) probe(h *scopeHandle, o probeOpts) probeResult {
	if h.protocol == "tcp" {
		return e.probeTCP(h, o)
	}
	return e.probeHTTP(h, o)
}

func (e *env) probeHTTP(h *scopeHandle, o probeOpts) probeResult {
	p := e.manifest.Conformance.Probe.HTTP
	body := p.Body
	if o.marker != "" {
		body += "\nmarker=" + o.marker
	}
	req, err := http.NewRequestWithContext(e.ctx, p.Method, "http://"+h.addr()+p.Path, strings.NewReader(body))
	if err != nil {
		return probeResult{err: err}
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.probeCl.Do(req)
	if err != nil {
		return probeResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()                                // drained below; nothing to flush
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes)) // the status line is the answer; a short body read changes nothing
	return probeResult{httpStatus: resp.StatusCode}
}

func (e *env) probeTCP(h *scopeHandle, o probeOpts) probeResult {
	d := net.Dialer{Timeout: e.opts.RequestTimeout}
	conn, err := d.DialContext(e.ctx, "tcp", h.addr())
	if err != nil {
		return probeResult{err: err}
	}
	defer func() { _ = conn.Close() }() // one line each way; nothing to flush
	if err := conn.SetDeadline(deadlineAfter(e.opts.RequestTimeout)); err != nil {
		return probeResult{err: err}
	}
	line := e.manifest.Conformance.Probe.TCP.Send
	if o.marker != "" {
		line += " " + o.marker
	}
	if _, err := fmt.Fprintf(conn, "%s\n", line); err != nil {
		return probeResult{err: err}
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return probeResult{err: err}
	}
	return probeResult{tcpReply: strings.TrimRight(reply, "\r\n")}
}

// satisfies reports whether the answer matches an effect's expectation, and says what it saw.
func (r probeResult) satisfies(x *Effect) (bool, string) {
	if r.err != nil {
		return false, "no answer (" + r.err.Error() + ")"
	}
	if x.Protocol == "tcp" {
		return r.tcpReply == x.Expect.TCPReply, fmt.Sprintf("tcp reply %q", r.tcpReply)
	}
	return r.httpStatus == x.Expect.HTTPStatus, fmt.Sprintf("http status %d", r.httpStatus)
}
