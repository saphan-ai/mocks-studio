package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	dnsmock "github.com/saphan-ai/mocks-studio/mocks/dns"
)

// udpHost is 127.0.0.1 unless this host refuses to send a UDP datagram to it (the fleet seat does,
// with EPERM), in which case ::1, so the UDP verb still crosses a real kernel socket.
func udpHost() string {
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

func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func freeAddr(t *testing.T, host string) (string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }() // released so the mock can bind it
	port := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
	return net.JoinHostPort(host, port), port
}

func TestVerbsDriveTheMockOverEveryTransport(t *testing.T) {
	m, err := dnsmock.Start(dnsmock.Config{ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	host := udpHost()
	addr, port := freeAddr(t, host)
	open := fmt.Sprintf(`{"run_id":"cli-run-1","listener":{"protocol":"dns","host":%q,"port":%s}}`, host, port)
	if code, out, _ := cli(t, "call", "--control", m.ControlURL(), "POST", "/_mock/v1/scopes", open); code != 0 || !strings.HasPrefix(out, "HTTP 201") {
		t.Fatalf("open: exit %d out %q", code, out)
	}
	records := `[{"name":"www.example.test","type":"A","value":"192.0.2.10"},{"name":"example.test","type":"TXT","value":"hello"}]`
	if code, out, _ := cli(t, "set", "--control", m.ControlURL(), "--run", "cli-run-1", "--records", records, "--blocked", `["ads.example.test"]`, "--block-answer", "refused"); code != 0 || !strings.HasPrefix(out, "HTTP 200") {
		t.Fatalf("set: exit %d out %q", code, out)
	}
	for _, tr := range []string{"udp", "tcp", "doh"} {
		code, out, _ := cli(t, "ask", "--server", addr, "--transport", tr, "--name", "www.example.test")
		if code != 0 || !strings.Contains(out, "rcode=NOERROR answers=1") || !strings.Contains(out, "A 192.0.2.10") {
			t.Fatalf("%s ask: exit %d out %q", tr, code, out)
		}
		if _, out, _ := cli(t, "ask", "--server", addr, "--transport", tr, "--name", "ads.example.test"); !strings.Contains(out, "rcode=REFUSED") {
			t.Fatalf("%s ask of a blocked name: %q", tr, out)
		}
	}
	if _, out, _ := cli(t, "ask", "--server", addr, "--transport", "tcp", "--name", "example.test", "--type", "TXT"); !strings.Contains(out, `TXT "hello"`) {
		t.Fatalf("TXT ask: %q", out)
	}
	code, out, _ := cli(t, "events", "--control", m.ControlURL(), "--run", "cli-run-1", "--for", "300ms")
	if code != 0 {
		t.Fatalf("events: exit %d", code)
	}
	_ = out // the stream is live: it holds only what arrives while listening, which is nothing here
}

func TestEventsVerbPrintsWhatArrivesWhileListening(t *testing.T) {
	m, err := dnsmock.Start(dnsmock.Config{ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	host := udpHost()
	addr, port := freeAddr(t, host)
	open := fmt.Sprintf(`{"run_id":"cli-run-2","listener":{"protocol":"dns","host":%q,"port":%s}}`, host, port)
	if code, out, _ := cli(t, "call", "--control", m.ControlURL(), "POST", "/_mock/v1/scopes", open); code != 0 {
		t.Fatalf("open: exit %d out %q", code, out)
	}
	done := make(chan string, 1)
	go func() {
		_, out, _ := cli(t, "events", "--control", m.ControlURL(), "--run", "cli-run-2", "--for", "1500ms")
		done <- out
	}()
	// The stream is live: a query sent before the verb has subscribed is not replayed to it, so
	// the query is repeated until the verb's listening window ends.
	var out string
	for deadline := time.Now().Add(10 * time.Second); out == "" && time.Now().Before(deadline); {
		cli(t, "ask", "--server", addr, "--transport", "tcp", "--name", "www.example.test")
		select {
		case out = <-done:
		default:
		}
	}
	if out == "" {
		t.Fatal("the events verb printed nothing within 10s while queries were being asked")
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("an events line is not JSON: %q", line)
		}
		seen[ev["kind"].(string)] = true
	}
	if !seen["request.received"] || !seen["response.sent"] {
		t.Fatalf("events seen = %v, want request.received and response.sent", seen)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for name, args := range map[string][]string{
		"no verb":                  {},
		"unknown verb":             {"frobnicate"},
		"call without a path":      {"call", "GET"},
		"set without a run":        {"set", "--servfail"},
		"set without a setting":    {"set", "--run", "x"},
		"ask without a server":     {"ask"},
		"ask with a bad type":      {"ask", "--server", "127.0.0.1:1", "--type", "SRV"},
		"ask with a bad transport": {"ask", "--server", "127.0.0.1:1", "--transport", "carrier-pigeon"},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := cli(t, args...); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
		})
	}
}

func TestAskReportsNoAnswerAsFailure(t *testing.T) {
	addr, _ := freeAddr(t, "127.0.0.1")
	if code, out, _ := cli(t, "ask", "--server", addr, "--transport", "tcp"); code != 1 || !strings.Contains(out, "no answer over tcp") {
		t.Fatalf("exit %d out %q, want 1 and no answer", code, out)
	}
}
