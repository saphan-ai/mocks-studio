package dnsmock

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

const wait = 3 * time.Second

// syncBuf is a log sink the mock writes and the test reads.
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

type testMock struct {
	*Mock
	log *syncBuf
	sub *subscriber
}

func startMock(t *testing.T) *testMock {
	t.Helper()
	buf := &syncBuf{}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0", Log: buf})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(m.Close)
	return &testMock{Mock: m, log: buf, sub: m.subscribe("")}
}

// control sends one control request and returns the status and the decoded JSON body.
func (tm *testMock) control(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, tm.ControlURL()+controlBase+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc) // a 204 has no body; the status is what the caller reads then
	return resp.StatusCode, doc
}

type scopeT struct {
	tm    *testMock
	runID string
	addr  string
}

var runSeq int

// testHost is the loopback address the scopes listen on: 127.0.0.1, unless this host refuses to
// SEND a UDP datagram to it (measured on the fleet seat: EPERM on IPv4 loopback, while IPv6
// loopback works), in which case ::1, so that the UDP tests still cross a real kernel socket.
var testHost = pickHost()

func pickHost() string {
	srv, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "::1"
	}
	defer func() { _ = srv.Close() }() // the probe socket
	c, err := net.Dial("udp", srv.LocalAddr().String())
	if err != nil {
		return "::1"
	}
	defer func() { _ = c.Close() }() // the probe socket
	if _, err := c.Write([]byte{0}); err != nil {
		return "::1"
	}
	_ = srv.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := srv.ReadFrom(make([]byte, 1)); err != nil {
		return "::1"
	}
	return "127.0.0.1"
}

// open opens a scope on a free loopback port, retrying a port taken between choice and bind.
func (tm *testMock) open(t *testing.T, protocol string) *scopeT {
	t.Helper()
	runSeq++
	runID := fmt.Sprintf("test-run-%03d", runSeq)
	for attempt := 0; attempt < 5; attempt++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(testHost, "0"))
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close() // released so the mock can bind it
		status, doc := tm.control(t, "POST", "/scopes", map[string]any{"run_id": runID, "listener": map[string]any{"protocol": protocol, "host": testHost, "port": port}})
		if status == http.StatusCreated {
			s := &scopeT{tm: tm, runID: runID, addr: net.JoinHostPort(testHost, fmt.Sprint(port))}
			t.Cleanup(func() { tm.control(t, "DELETE", "/scopes/"+runID, nil) })
			return s
		}
		if doc["reason_code"] != "listener-unavailable" {
			t.Fatalf("open scope: status %d body %v", status, doc)
		}
	}
	t.Fatal("no free port after 5 attempts")
	return nil
}

func (s *scopeT) set(t *testing.T, settings map[string]any) {
	t.Helper()
	if status, doc := s.tm.control(t, "PUT", "/scopes/"+s.runID+"/settings", settings); status != http.StatusOK {
		t.Fatalf("set %v: status %d body %v", settings, status, doc)
	}
}

func rec(name, typ, value string) map[string]any {
	return map[string]any{"name": name, "type": typ, "value": value}
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (s *scopeT) records(t *testing.T, recs ...map[string]any) {
	t.Helper()
	s.set(t, map[string]any{"records": jsonString(t, recs)})
}

func (s *scopeT) blocked(t *testing.T, answer string, names ...string) {
	t.Helper()
	s.set(t, map[string]any{"blocked": jsonString(t, names), "block_answer": answer})
}

func query(t *testing.T, id uint16, name string, typ uint16) []byte {
	t.Helper()
	b, err := (&dnsmsg.Message{Header: dnsmsg.Header{ID: id, RecursionDesired: true}, Questions: []dnsmsg.Question{{Name: name, Type: typ, Class: dnsmsg.ClassIN}}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// transport asks one raw DNS message over one transport and returns the raw answer.
type transport struct {
	name string
	ask  func(t *testing.T, addr string, wire []byte) ([]byte, error)
}

var transports = []transport{
	{"udp", askUDP}, {"tcp", askTCP}, {"doh-get", askDoHGet}, {"doh-post", askDoHPost},
}

func askUDP(t *testing.T, addr string, wire []byte) ([]byte, error) {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }() // one datagram each way
	_ = c.SetDeadline(time.Now().Add(wait))
	if _, err := c.Write(wire); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	return buf[:n], err
}

func askTCP(t *testing.T, addr string, wire []byte) ([]byte, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, wait)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }() // one exchange
	return exchangeTCP(c, wire)
}

func exchangeTCP(c net.Conn, wire []byte) ([]byte, error) {
	_ = c.SetDeadline(time.Now().Add(wait))
	out := binary.BigEndian.AppendUint16(nil, uint16(len(wire)))
	if _, err := c.Write(append(out, wire...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(c, buf)
	return buf, err
}

func dohGetURL(addr string, wire []byte, extraQuery string) string {
	u := "http://" + addr + dohPath + "?dns=" + base64.RawURLEncoding.EncodeToString(wire)
	if extraQuery != "" {
		u += "&" + extraQuery
	}
	return u
}

func askDoHGet(t *testing.T, addr string, wire []byte) ([]byte, error) {
	t.Helper()
	status, body, err := doh(http.MethodGet, dohGetURL(addr, wire, ""), nil, nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("DoH GET answered HTTP %d", status)
	}
	return body, err
}

func askDoHPost(t *testing.T, addr string, wire []byte) ([]byte, error) {
	t.Helper()
	status, body, err := doh(http.MethodPost, "http://"+addr+dohPath, wire, map[string]string{"Content-Type": "application/dns-message"})
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("DoH POST answered HTTP %d", status)
	}
	return body, err
}

func doh(method, url string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	cl := &http.Client{Timeout: wait, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// answer asks and decodes; a transport failure or an undecodable answer fails the test.
func answer(t *testing.T, tr transport, s *scopeT, wire []byte) *dnsmsg.Message {
	t.Helper()
	raw, err := tr.ask(t, s.addr, wire)
	if err != nil {
		t.Fatalf("%s: no answer: %v", tr.name, err)
	}
	m, err := dnsmsg.Unpack(raw)
	if err != nil {
		t.Fatalf("%s: undecodable answer: %v", tr.name, err)
	}
	return m
}

// events returns the events received since the last call, waiting until done(events) holds.
func (tm *testMock) events(t *testing.T, done func([]map[string]any) bool) []map[string]any {
	t.Helper()
	var got []map[string]any
	deadline := time.After(wait)
	for {
		if done(got) {
			return got
		}
		select {
		case line := <-tm.sub.ch:
			var ev map[string]any
			if err := json.Unmarshal(line, &ev); err != nil {
				t.Fatalf("an event is not JSON: %s", line)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("events did not arrive within %s; got %d: %v", wait, len(got), got)
		}
	}
}

func hasKind(evs []map[string]any, kind string) bool {
	for _, e := range evs {
		if e["kind"] == kind {
			return true
		}
	}
	return false
}

func kindOf(evs []map[string]any, kind string) map[string]any {
	for _, e := range evs {
		if e["kind"] == kind {
			return e
		}
	}
	return nil
}

func payloadFields(ev map[string]any) map[string]any {
	p, _ := ev["payload"].(map[string]any)
	f, _ := p["fields"].(map[string]any)
	return f
}

func mustContain(t *testing.T, what, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Fatalf("%s does not contain %q: %s", what, needle, hay)
	}
}
