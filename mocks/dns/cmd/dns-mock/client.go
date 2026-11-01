package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

const (
	defaultControl = "http://127.0.0.1:9100"
	clientTimeout  = 5 * time.Second
)

func newFlags(name string, errOut io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(errOut)
	return fs
}

// run dispatches the client verbs and returns the exit code.
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	switch args[0] {
	case "call":
		return call(args[1:], out, errOut)
	case "set":
		return set(args[1:], out, errOut)
	case "ask":
		return ask(args[1:], out, errOut)
	case "events":
		return events(args[1:], out, errOut)
	}
	fmt.Fprintln(errOut, usage)
	return 2
}

func httpDo(method, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: clientTimeout}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }() // read in full below
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func call(args []string, out, errOut io.Writer) int {
	fs := newFlags("call", errOut)
	control := fs.String("control", defaultControl, "control URL")
	if fs.Parse(args) != nil || fs.NArg() < 2 || fs.NArg() > 3 {
		fmt.Fprintln(errOut, "usage: dns-mock call [--control URL] METHOD PATH [JSON-BODY]")
		return 2
	}
	var body []byte
	if fs.NArg() == 3 {
		body = []byte(fs.Arg(2))
	}
	status, b, err := httpDo(fs.Arg(0), strings.TrimRight(*control, "/")+fs.Arg(1), body)
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock: call failed:", err)
		return 1
	}
	fmt.Fprintf(out, "HTTP %d\n%s", status, b)
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		fmt.Fprintln(out)
	}
	return 0
}

func set(args []string, out, errOut io.Writer) int {
	fs := newFlags("set", errOut)
	control := fs.String("control", defaultControl, "control URL")
	run := fs.String("run", "", "run id (required)")
	fs.String("records", "", "records setting: a JSON array")
	fs.String("blocked", "", "blocked setting: a JSON array of names")
	fs.String("block-answer", "", "block_answer setting: nxdomain, refused or sinkhole")
	fs.String("sinkhole-ip", "", "sinkhole_ip setting")
	fs.Int("latency-ms", 0, "latency_ms setting")
	fs.Int("doh-http-status", 0, "doh_http_status setting")
	fs.Bool("servfail", false, "servfail setting (--servfail=false clears it)")
	fs.Bool("drop-udp", false, "drop_udp setting (--drop-udp=false clears it)")
	if fs.Parse(args) != nil || *run == "" {
		fmt.Fprintln(errOut, "usage: dns-mock set [--control URL] --run RUN_ID [setting flags]; at least one setting flag")
		return 2
	}
	doc := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "control" || f.Name == "run" {
			return
		}
		name := strings.ReplaceAll(f.Name, "-", "_")
		switch v := f.Value.(flag.Getter).Get().(type) {
		case string, bool:
			doc[name] = v
		case int:
			doc[name] = v
		}
	})
	if len(doc) == 0 {
		fmt.Fprintln(errOut, "dns-mock: set: no setting flag was given")
		return 2
	}
	body, err := json.Marshal(doc)
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock:", err)
		return 1
	}
	status, b, err := httpDo(http.MethodPut, strings.TrimRight(*control, "/")+"/_mock/v1/scopes/"+*run+"/settings", body)
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock: set failed:", err)
		return 1
	}
	fmt.Fprintf(out, "HTTP %d\n%s", status, b)
	return 0
}

func ask(args []string, out, errOut io.Writer) int {
	fs := newFlags("ask", errOut)
	server := fs.String("server", "", "host:port of a scope's listener (required)")
	transport := fs.String("transport", "udp", "udp, tcp or doh")
	name := fs.String("name", "www.example.test", "name to ask")
	typ := fs.String("type", "A", "record type: A, AAAA, CNAME, MX or TXT")
	if fs.Parse(args) != nil || *server == "" {
		fmt.Fprintln(errOut, "usage: dns-mock ask --server host:port [--transport udp|tcp|doh] [--name NAME] [--type TYPE]")
		return 2
	}
	qt, ok := dnsmsg.ParseType(*typ)
	if !ok {
		fmt.Fprintf(errOut, "dns-mock: ask: type %q is not A, AAAA, CNAME, MX or TXT\n", *typ)
		return 2
	}
	wire, err := (&dnsmsg.Message{Header: dnsmsg.Header{ID: uint16(time.Now().UnixNano()), RecursionDesired: true}, Questions: []dnsmsg.Question{{Name: *name, Type: qt, Class: dnsmsg.ClassIN}}}).Pack()
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock: ask:", err)
		return 2
	}
	var raw []byte
	switch *transport {
	case "udp":
		raw, err = askUDP(*server, wire)
	case "tcp":
		raw, err = askTCP(*server, wire)
	case "doh":
		raw, err = askDoH(*server, wire)
	default:
		fmt.Fprintf(errOut, "dns-mock: ask: transport %q is not udp, tcp or doh\n", *transport)
		return 2
	}
	if err != nil {
		fmt.Fprintf(out, "no answer over %s: %v\n", *transport, err)
		return 1
	}
	m, err := dnsmsg.Unpack(raw)
	if err != nil {
		fmt.Fprintf(out, "undecodable answer over %s: %v\n", *transport, err)
		return 1
	}
	fmt.Fprintf(out, "rcode=%s answers=%d truncated=%v\n", dnsmsg.RcodeName(m.Header.Rcode), len(m.Answers), m.Header.Truncated)
	for _, rr := range m.Answers {
		fmt.Fprintf(out, "%s %d %s %s\n", rr.Name, rr.TTL, dnsmsg.TypeName(rr.Type), rdata(rr))
	}
	return 0
}

func rdata(rr dnsmsg.RR) string {
	switch rr.Type {
	case dnsmsg.TypeA, dnsmsg.TypeAAAA:
		return rr.Addr.String()
	case dnsmsg.TypeCNAME:
		return rr.Target
	case dnsmsg.TypeMX:
		return fmt.Sprintf("%d %s", rr.Pref, rr.Target)
	case dnsmsg.TypeTXT:
		return fmt.Sprintf("%q", strings.Join(rr.Text, ""))
	}
	return ""
}

func askUDP(server string, wire []byte) ([]byte, error) {
	c, err := net.DialTimeout("udp", server, clientTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }() // one datagram each way
	_ = c.SetDeadline(time.Now().Add(clientTimeout))
	if _, err := c.Write(wire); err != nil {
		return nil, err
	}
	buf := make([]byte, 65535)
	n, err := c.Read(buf)
	return buf[:n], err
}

func askTCP(server string, wire []byte) ([]byte, error) {
	c, err := net.DialTimeout("tcp", server, clientTimeout)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }() // one exchange
	_ = c.SetDeadline(time.Now().Add(clientTimeout))
	if _, err := c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(wire))), wire...)); err != nil {
		return nil, err
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err = io.ReadFull(c, buf)
	return buf, err
}

func askDoH(server string, wire []byte) ([]byte, error) {
	status, b, err := httpDo(http.MethodGet, "http://"+server+"/dns-query?dns="+base64.RawURLEncoding.EncodeToString(wire), nil)
	if err == nil && status != http.StatusOK {
		err = fmt.Errorf("HTTP %d", status)
	}
	return b, err
}

func events(args []string, out, errOut io.Writer) int {
	fs := newFlags("events", errOut)
	control := fs.String("control", defaultControl, "control URL")
	runID := fs.String("run", "", "only this run's events")
	dur := fs.Duration("for", 2*time.Second, "how long to listen")
	if fs.Parse(args) != nil {
		return 2
	}
	url := strings.TrimRight(*control, "/") + "/_mock/v1/events"
	if *runID != "" {
		url += "?run_id=" + *runID
	}
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock:", err)
		return 2
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(errOut, "dns-mock: events failed:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }() // the stream is read until the deadline
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			fmt.Fprintln(out, line)
		}
	}
	return 0 // the deadline ends the read; that is the normal end of listening
}
