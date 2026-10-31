package dnsmock

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	routeWait   = 5 * time.Second
	idleTimeout = 30 * time.Second
	maxUDPWire  = 512 // RFC 1035 section 2.3.4; the mock negotiates no EDNS buffer size
	maxMessage  = 65535
)

// scope is one run's listener, settings and in-flight work.
type scope struct {
	m        *Mock
	runID    string
	label    string // the protocol the core asked for; it labels the scope's lifecycle events
	listener listenerSpec

	mu       sync.Mutex
	settings map[string]any
	parsed   runSettings
	closed   bool
	conns    map[net.Conn]struct{}

	ln    net.Listener
	pc    net.PacketConn
	srv   *http.Server
	httpC chan net.Conn
	done  chan struct{}
	wg    sync.WaitGroup
}

func newScope(m *Mock, runID string, l listenerSpec) *scope {
	s := &scope{m: m, runID: runID, label: l.Protocol, listener: l, conns: map[net.Conn]struct{}{}, httpC: make(chan net.Conn), done: make(chan struct{})}
	s.settings = s.defaults()
	s.parsed = parseSettings(s.settings)
	return s
}

func (s *scope) defaults() map[string]any {
	out := make(map[string]any, len(s.m.defs))
	for name, d := range s.m.defs {
		out[name] = d.Default
	}
	return out
}

func (s *scope) addr() string { return net.JoinHostPort(s.listener.Host, fmt.Sprint(s.listener.Port)) }

func (s *scope) resetSettings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings = s.defaults()
	s.parsed = parseSettings(s.settings)
}

// apply merges validated settings and returns every setting of the run with its current value.
func (s *scope) apply(body map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range body {
		s.settings[k] = v
	}
	s.parsed = parseSettings(s.settings)
	out := make(map[string]any, len(s.settings))
	for k, v := range s.settings {
		out[k] = v
	}
	return out
}

func (s *scope) current() runSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parsed
}

// start binds the TCP listener and the UDP socket of the same port; either failing refuses the open.
func (s *scope) start() error {
	ln, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp", s.addr())
	if err != nil {
		_ = ln.Close() // the open failed; nothing is left listening
		return err
	}
	s.ln, s.pc = ln, pc
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serveDoH), ReadHeaderTimeout: 5 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	s.goRun(func() { _ = s.srv.Serve(chanListener{s}) }) // returns on Close; that error is the stop signal
	s.goRun(s.acceptLoop)
	s.goRun(s.udpLoop)
	return nil
}

func (s *scope) goRun(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// stop closes every socket and waits for in-flight queries, so that once it returns the scope
// answers nothing and emits nothing more.
func (s *scope) stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	_ = s.ln.Close()  // the accept loop ends on this error
	_ = s.pc.Close()  // the UDP loop ends on this error
	_ = s.srv.Close() // closes the connections the HTTP server holds
	for _, c := range conns {
		_ = c.Close() // the peer may be gone already
	}
	s.wg.Wait()
}

// enter registers one in-flight query; it is refused once the scope is closing.
func (s *scope) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

func (s *scope) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *scope) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// chanListener hands the HTTP server the connections the router recognised as HTTP.
type chanListener struct{ s *scope }

func (l chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.s.httpC:
		return c, nil
	case <-l.s.done:
		return nil, net.ErrClosed
	}
}
func (l chanListener) Close() error   { return nil }
func (l chanListener) Addr() net.Addr { return l.s.ln.Addr() }

// bufConn is a connection whose first bytes were already read by the router.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (s *scope) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		if !s.track(c) {
			_ = c.Close() // the scope is closing
			continue
		}
		s.goRun(func() { s.route(c) })
	}
}

// route tells DoH from DNS over TCP by the first bytes: an HTTP request starts with a method
// and a space, a DNS message with a two-byte length that no real query makes look like one.
func (s *scope) route(c net.Conn) {
	br := bufio.NewReaderSize(c, 4096)
	_ = c.SetReadDeadline(time.Now().Add(routeWait)) // a peer that sends nothing is dropped below
	first, err := br.Peek(1)
	if err == nil && mayBeHTTP(first[0]) {
		first, err = br.Peek(4) // only a byte that could start a method is worth waiting for more bytes
	}
	_ = c.SetReadDeadline(time.Time{}) // cleared: the protocol handlers set their own
	if err != nil {
		s.untrack(c)
		_ = c.Close() // nothing was said, or the peer left
		return
	}
	if looksLikeHTTP(first) {
		select {
		case s.httpC <- &bufConn{Conn: c, r: br}:
		case <-s.done:
			_ = c.Close() // the scope closed while the connection was routed
		}
		return
	}
	s.serveTCP(c, br)
}

// mayBeHTTP is true for the first byte of a method name (D, G, H, O, P); as the high byte of a
// DNS length it would announce a message of 17,408 bytes or more, which no query is.
func mayBeHTTP(b byte) bool {
	switch b {
	case 'D', 'G', 'H', 'O', 'P':
		return true
	}
	return false
}

func looksLikeHTTP(head []byte) bool {
	switch string(head) {
	case "GET ", "POST", "PUT ", "HEAD", "DELE", "OPTI", "PATC":
		return true
	}
	return false
}

// serveTCP answers DNS over TCP (RFC 1035 section 4.2.2): a two-byte length, then the message.
func (s *scope) serveTCP(c net.Conn, br *bufio.Reader) {
	defer func() {
		s.untrack(c)
		_ = c.Close() // the peer may be gone already
	}()
	for {
		if err := c.SetReadDeadline(time.Now().Add(idleTimeout)); err != nil {
			return
		}
		var lenBuf [2]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lenBuf[:]))
		msg := make([]byte, n)
		if _, err := io.ReadFull(br, msg); err != nil {
			return
		}
		resp := s.process(queryReq{raw: msg, transport: "tcp"})
		if resp.drop {
			return
		}
		out := make([]byte, 2, 2+len(resp.wire))
		binary.BigEndian.PutUint16(out, uint16(len(resp.wire)))
		out = append(out, resp.wire...)
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

func (s *scope) udpLoop() {
	buf := make([]byte, maxMessage)
	for {
		n, peer, err := s.pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // a transient read error on a datagram socket is not the end of the scope
		}
		msg := append([]byte(nil), buf[:n]...)
		if !s.enter() {
			return
		}
		go func() {
			defer s.wg.Done()
			resp := s.processEntered(queryReq{raw: msg, transport: "udp"})
			if resp.drop {
				return
			}
			_, _ = s.pc.WriteTo(resp.wire, peer) // a peer that left does not need its answer
		}()
	}
}
