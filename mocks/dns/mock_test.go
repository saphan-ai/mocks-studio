package dnsmock

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

func TestDeclaredRecordsAnswered(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t,
		rec("www.example.test", "A", "192.0.2.10"),
		rec("www.example.test", "AAAA", "2001:db8::10"),
		rec("alias.example.test", "CNAME", "www.example.test"),
		rec("example.test", "MX", "10 mail.example.test"),
		rec("example.test", "TXT", "v=spf1 -all"),
		rec("api.acme-example.test", "A", "203.0.113.7"),
	)
	cases := []struct {
		name  string
		typ   uint16
		check func(t *testing.T, m *dnsmsg.Message)
	}{
		{"www.example.test", dnsmsg.TypeA, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 1 || m.Answers[0].Addr != netip.MustParseAddr("192.0.2.10") {
				t.Fatalf("A answers = %+v", m.Answers)
			}
		}},
		{"www.example.test", dnsmsg.TypeAAAA, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 1 || m.Answers[0].Addr != netip.MustParseAddr("2001:db8::10") {
				t.Fatalf("AAAA answers = %+v", m.Answers)
			}
		}},
		{"alias.example.test", dnsmsg.TypeA, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 2 || m.Answers[0].Type != dnsmsg.TypeCNAME || m.Answers[1].Addr != netip.MustParseAddr("192.0.2.10") {
				t.Fatalf("CNAME chain = %+v", m.Answers)
			}
		}},
		{"example.test", dnsmsg.TypeMX, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 1 || m.Answers[0].Pref != 10 || m.Answers[0].Target != "mail.example.test" {
				t.Fatalf("MX answers = %+v", m.Answers)
			}
		}},
		{"EXAMPLE.test", dnsmsg.TypeTXT, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 1 || len(m.Answers[0].Text) != 1 || m.Answers[0].Text[0] != "v=spf1 -all" {
				t.Fatalf("TXT answers (case-insensitive name) = %+v", m.Answers)
			}
		}},
		{"api.acme-example.test", dnsmsg.TypeA, func(t *testing.T, m *dnsmsg.Message) {
			if len(m.Answers) != 1 || m.Answers[0].Addr != netip.MustParseAddr("203.0.113.7") {
				t.Fatalf("acme zone A answers = %+v", m.Answers)
			}
		}},
	}
	for _, tr := range transports {
		for _, c := range cases {
			t.Run(tr.name+"/"+c.name+"/"+dnsmsg.TypeName(c.typ), func(t *testing.T) {
				m := answer(t, tr, s, query(t, 0x1234, c.name, c.typ))
				if m.Header.Rcode != dnsmsg.RcodeNoError || !m.Header.Response || m.Header.ID != 0x1234 {
					t.Fatalf("header = %+v, want a NOERROR response with the query's id", m.Header)
				}
				c.check(t, m)
			})
		}
	}
}

func TestUnknownNameAndMissingTypeAnswerDifferently(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	for _, tr := range transports {
		t.Run(tr.name+"/unknown name is NXDOMAIN", func(t *testing.T) {
			m := answer(t, tr, s, query(t, 1, "nobody.example.test", dnsmsg.TypeA))
			if m.Header.Rcode != dnsmsg.RcodeNXDomain || len(m.Answers) != 0 {
				t.Fatalf("rcode %s answers %d, want NXDOMAIN and none", dnsmsg.RcodeName(m.Header.Rcode), len(m.Answers))
			}
		})
		t.Run(tr.name+"/known name without the type is NOERROR and empty", func(t *testing.T) {
			m := answer(t, tr, s, query(t, 1, "www.example.test", dnsmsg.TypeMX))
			if m.Header.Rcode != dnsmsg.RcodeNoError || len(m.Answers) != 0 {
				t.Fatalf("rcode %s answers %d, want NOERROR and none", dnsmsg.RcodeName(m.Header.Rcode), len(m.Answers))
			}
		})
	}
}

func TestBlockedNameReturnsTheDeclaredBlockAnswer(t *testing.T) {
	cases := []struct {
		name     string
		answer   string
		sinkhole string
		qtype    uint16
		rcode    uint8
		addr     string // the one expected answer address, "" for none
	}{
		{"nxdomain", "nxdomain", "", dnsmsg.TypeA, dnsmsg.RcodeNXDomain, ""},
		{"refused", "refused", "", dnsmsg.TypeA, dnsmsg.RcodeRefused, ""},
		{"sinkhole v4 answers A", "sinkhole", "198.51.100.66", dnsmsg.TypeA, dnsmsg.RcodeNoError, "198.51.100.66"},
		{"sinkhole v4 has no AAAA", "sinkhole", "198.51.100.66", dnsmsg.TypeAAAA, dnsmsg.RcodeNoError, ""},
		{"sinkhole v6 answers AAAA", "sinkhole", "2001:db8::66", dnsmsg.TypeAAAA, dnsmsg.RcodeNoError, "2001:db8::66"},
	}
	for _, tr := range transports {
		for _, c := range cases {
			t.Run(tr.name+"/"+c.name, func(t *testing.T) {
				tm := startMock(t)
				s := tm.open(t, "dns")
				// The blocked name also has a record: the block must win over it.
				s.records(t, rec("ads.example.test", "A", "192.0.2.10"), rec("ok.example.test", "A", "192.0.2.11"))
				set := map[string]any{"blocked": jsonString(t, []string{"ads.example.test"}), "block_answer": c.answer}
				if c.sinkhole != "" {
					set["sinkhole_ip"] = c.sinkhole
				}
				s.set(t, set)
				for _, name := range []string{"ads.example.test", "deep.ads.example.test"} {
					m := answer(t, tr, s, query(t, 9, name, c.qtype))
					if m.Header.Rcode != c.rcode {
						t.Fatalf("%s: rcode %s, want %s", name, dnsmsg.RcodeName(m.Header.Rcode), dnsmsg.RcodeName(c.rcode))
					}
					switch {
					case c.addr == "" && len(m.Answers) != 0:
						t.Fatalf("%s: answers %+v, want none", name, m.Answers)
					case c.addr != "" && (len(m.Answers) != 1 || m.Answers[0].Addr != netip.MustParseAddr(c.addr)):
						t.Fatalf("%s: answers %+v, want the sinkhole %s", name, m.Answers, c.addr)
					}
				}
				if m := answer(t, tr, s, query(t, 9, "ok.example.test", dnsmsg.TypeA)); len(m.Answers) != 1 || m.Answers[0].Addr != netip.MustParseAddr("192.0.2.11") {
					t.Fatalf("a name that is not blocked was disturbed: %+v", m.Answers)
				}
			})
		}
	}
}

func TestMalformedMessageGetsFormerr(t *testing.T) {
	valid := query(t, 0xABCD, "www.example.test", dnsmsg.TypeA)
	// Each case breaks exactly one field of the valid query.
	cases := []struct {
		field string
		wire  func() []byte
	}{
		{"message length: header cut by one byte", func() []byte { return valid[:11] }},
		{"message length: question without its class", func() []byte { return valid[:len(valid)-2] }},
		{"QDCOUNT: zero questions", func() []byte { b := clone(valid); b[4], b[5] = 0, 0; return b }},
		{"QDCOUNT: two counted, one present", func() []byte { b := clone(valid); b[5] = 2; return b }},
		{"QR: a response sent as a query", func() []byte { b := clone(valid); b[2] |= 0x80; return b }},
		{"first label length: runs past the message", func() []byte { b := clone(valid); b[12] = 0x3F; return b }},
		{"first label length: reserved bits", func() []byte { b := clone(valid); b[12] = 0x80; return b }},
		{"name: pointer to itself", func() []byte { b := clone(valid); b[12], b[13] = 0xC0, 0x0C; return b }},
	}
	for _, tr := range transports {
		for _, c := range cases {
			t.Run(tr.name+"/"+c.field, func(t *testing.T) {
				tm := startMock(t)
				s := tm.open(t, "dns")
				m := answer(t, tr, s, c.wire())
				if m.Header.Rcode != dnsmsg.RcodeFormErr || !m.Header.Response || len(m.Answers) != 0 {
					t.Fatalf("answer %+v, want a FORMERR response with no answers", m.Header)
				}
				if m.Header.ID != 0xABCD {
					t.Fatalf("answer id %#x, want the query's 0xabcd", m.Header.ID)
				}
			})
		}
	}
}

func TestEmptyMessageGetsFormerr(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			if m := answer(t, tr, s, nil); m.Header.Rcode != dnsmsg.RcodeFormErr {
				t.Fatalf("rcode %s for an empty message, want FORMERR", dnsmsg.RcodeName(m.Header.Rcode))
			}
		})
	}
}

func TestNonQueryOpcodeGetsNotimp(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	valid := query(t, 5, "www.example.test", dnsmsg.TypeA)
	valid[2] |= 2 << 3 // opcode STATUS, the only field changed
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			if m := answer(t, tr, s, valid); m.Header.Rcode != dnsmsg.RcodeNotImp {
				t.Fatalf("rcode %s for opcode 2, want NOTIMP", dnsmsg.RcodeName(m.Header.Rcode))
			}
		})
	}
}

func TestNonInternetClassIsRefused(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	valid := query(t, 5, "www.example.test", dnsmsg.TypeA)
	binary.BigEndian.PutUint16(valid[len(valid)-2:], 3) // class CH, the only field changed
	if m := answer(t, transports[0], s, valid); m.Header.Rcode != dnsmsg.RcodeRefused {
		t.Fatalf("rcode %s for class CH, want REFUSED", dnsmsg.RcodeName(m.Header.Rcode))
	}
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func TestServfailSettingAnswersEveryQuerySERVFAIL(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	s.set(t, map[string]any{"servfail": true})
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			m := answer(t, tr, s, query(t, 3, "www.example.test", dnsmsg.TypeA))
			if m.Header.Rcode != dnsmsg.RcodeServFail || len(m.Answers) != 0 {
				t.Fatalf("rcode %s answers %d, want SERVFAIL and none", dnsmsg.RcodeName(m.Header.Rcode), len(m.Answers))
			}
		})
	}
}

func TestDropUDPLeavesUDPSilentAndTheOthersAnswering(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	s.set(t, map[string]any{"drop_udp": true})
	c, err := net.Dial("udp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }() // one datagram
	_ = c.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	if _, err := c.Write(query(t, 3, "www.example.test", dnsmsg.TypeA)); err != nil {
		t.Fatal(err)
	}
	if n, err := c.Read(make([]byte, 512)); err == nil {
		t.Fatalf("UDP answered %d bytes with drop_udp set", n)
	}
	for _, tr := range []transport{transports[1], transports[2], transports[3]} {
		if m := answer(t, tr, s, query(t, 3, "www.example.test", dnsmsg.TypeA)); len(m.Answers) != 1 {
			t.Fatalf("%s: answers %+v with drop_udp set, want the declared record", tr.name, m.Answers)
		}
	}
}

func TestLatencyHoldsEveryTransportBack(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.set(t, map[string]any{"latency_ms": 200})
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			start := time.Now()
			answer(t, tr, s, query(t, 3, "www.example.test", dnsmsg.TypeA))
			if d := time.Since(start); d < 200*time.Millisecond {
				t.Fatalf("answered in %s with latency_ms=200", d)
			}
		})
	}
}

func TestDoHHTTPStatusReplacesTheAnswer(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "http")
	s.set(t, map[string]any{"doh_http_status": 503})
	status, _, err := doh(http.MethodGet, dohGetURL(s.addr, query(t, 1, "www.example.test", dnsmsg.TypeA), ""), nil, nil)
	if err != nil || status != 503 {
		t.Fatalf("DoH status %d err %v, want 503", status, err)
	}
	if m := answer(t, transports[0], s, query(t, 1, "www.example.test", dnsmsg.TypeA)); m.Header.Rcode != dnsmsg.RcodeNXDomain {
		t.Fatalf("UDP rcode %s while only DoH was set to fail, want NXDOMAIN", dnsmsg.RcodeName(m.Header.Rcode))
	}
}

func TestFaultsEmitFaultInjected(t *testing.T) {
	for _, c := range []struct {
		setting string
		value   any
		tr      transport
	}{
		{"servfail", true, transports[0]},
		{"drop_udp", true, transports[0]},
		{"doh_http_status", 503, transports[2]},
	} {
		t.Run(c.setting, func(t *testing.T) {
			tm := startMock(t)
			s := tm.open(t, "dns")
			s.set(t, map[string]any{c.setting: c.value})
			tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "scenario.set") })
			_, _ = c.tr.ask(t, s.addr, query(t, 2, "www.example.test", dnsmsg.TypeA)) // a drop or a 503 is an error here, by design
			evs := tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "fault.injected") })
			ev := kindOf(evs, "fault.injected")
			if ev["fault"] != c.setting || ev["outcome"] != "fault" || ev["run_id"] != s.runID {
				t.Fatalf("fault.injected = %v, want fault %q outcome fault run %s", ev, c.setting, s.runID)
			}
		})
	}
}

func TestEveryQueryIsAnEventWithOnlySafeFields(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "scenario.set") })
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			answer(t, tr, s, query(t, 3, "www.example.test", dnsmsg.TypeA))
			evs := tm.events(t, func(evs []map[string]any) bool {
				return hasKind(evs, "request.received") && hasKind(evs, "response.sent")
			})
			wantTransport := tr.name
			if strings.HasPrefix(wantTransport, "doh") {
				wantTransport = "doh"
			}
			for kind, want := range map[string]map[string]any{
				"request.received": {"qname": "www.example.test", "qtype": "A", "transport": wantTransport},
				"response.sent":    {"qname": "www.example.test", "qtype": "A", "transport": wantTransport, "rcode": "NOERROR"},
			} {
				got := payloadFields(kindOf(evs, kind))
				if len(got) != len(want) {
					t.Fatalf("%s payload fields = %v, want exactly %v", kind, got, want)
				}
				for k, v := range want {
					if got[k] != v {
						t.Fatalf("%s payload field %s = %v, want %v", kind, k, got[k], v)
					}
				}
			}
			if kindOf(evs, "response.sent")["outcome"] != "ok" {
				t.Fatalf("response.sent outcome = %v, want ok", kindOf(evs, "response.sent")["outcome"])
			}
		})
	}
}

func TestMalformedQueryIsAnEventWithARedactedPayload(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	answer(t, transports[0], s, []byte{1, 2, 3})
	evs := tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "response.sent") })
	p, _ := kindOf(evs, "request.received")["payload"].(map[string]any)
	if p["redacted"] != true || p["size"] != float64(3) {
		t.Fatalf("payload of an unparseable message = %v, want redacted with size 3", p)
	}
	if got := payloadFields(kindOf(evs, "response.sent"))["rcode"]; got != "FORMERR" {
		t.Fatalf("response.sent rcode = %v, want FORMERR", got)
	}
}

func TestBlockedAnswerIsRecordedAsRefused(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.blocked(t, "refused", "ads.example.test")
	tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "scenario.set") })
	answer(t, transports[0], s, query(t, 3, "ads.example.test", dnsmsg.TypeA))
	evs := tm.events(t, func(evs []map[string]any) bool { return hasKind(evs, "response.sent") })
	if ev := kindOf(evs, "response.sent"); ev["outcome"] != "refused" || ev["fault"] != "blocked" {
		t.Fatalf("response.sent for a blocked name = %v, want outcome refused and fault blocked", ev)
	}
}

func TestUDPAnswerTooBigIsTruncatedAndTCPCarriesItWhole(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	big := strings.Repeat("x", 200)
	s.records(t, rec("big.example.test", "TXT", big), rec("big.example.test", "TXT", big+"1"), rec("big.example.test", "TXT", big+"2"))
	udp := answer(t, transports[0], s, query(t, 4, "big.example.test", dnsmsg.TypeTXT))
	if !udp.Header.Truncated || len(udp.Answers) != 0 {
		t.Fatalf("UDP answer %+v with %d answers, want the TC bit and none", udp.Header, len(udp.Answers))
	}
	if tcp := answer(t, transports[1], s, query(t, 4, "big.example.test", dnsmsg.TypeTXT)); tcp.Header.Truncated || len(tcp.Answers) != 3 {
		t.Fatalf("TCP answer %+v with %d answers, want all 3 whole", tcp.Header, len(tcp.Answers))
	}
}

func TestTCPConnectionCarriesSeveralQueries(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	c, err := net.DialTimeout("tcp", s.addr, wait)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }() // the test is done with the connection
	for i, name := range []string{"www.example.test", "nobody.example.test", "www.example.test"} {
		raw, err := exchangeTCP(c, query(t, uint16(i), name, dnsmsg.TypeA))
		if err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		m, err := dnsmsg.Unpack(raw)
		if err != nil || m.Header.ID != uint16(i) {
			t.Fatalf("query %d: answer %+v err %v", i, m, err)
		}
	}
}

func TestScopesKeepTheirRecordsApart(t *testing.T) {
	tm := startMock(t)
	a, b := tm.open(t, "dns"), tm.open(t, "dns")
	a.records(t, rec("www.example.test", "A", "192.0.2.1"))
	b.records(t, rec("www.example.test", "A", "192.0.2.2"))
	for _, tr := range transports {
		for s, want := range map[*scopeT]string{a: "192.0.2.1", b: "192.0.2.2"} {
			if m := answer(t, tr, s, query(t, 1, "www.example.test", dnsmsg.TypeA)); len(m.Answers) != 1 || m.Answers[0].Addr.String() != want {
				t.Fatalf("%s: run %s answered %+v, want %s", tr.name, s.runID, m.Answers, want)
			}
		}
	}
}

func TestResetReturnsSettingsToTheirDefaults(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	s.records(t, rec("www.example.test", "A", "192.0.2.10"))
	if status, _ := tm.control(t, "POST", "/scopes/"+s.runID+"/reset", nil); status != http.StatusNoContent {
		t.Fatalf("reset status %d", status)
	}
	if m := answer(t, transports[0], s, query(t, 1, "www.example.test", dnsmsg.TypeA)); m.Header.Rcode != dnsmsg.RcodeNXDomain {
		t.Fatalf("after reset rcode %s, want NXDOMAIN (no records)", dnsmsg.RcodeName(m.Header.Rcode))
	}
}

func TestClosingAScopeSilencesUDPTCPAndDoH(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "dns")
	for _, tr := range transports {
		answer(t, tr, s, query(t, 1, "www.example.test", dnsmsg.TypeA))
	}
	if status, _ := tm.control(t, "DELETE", "/scopes/"+s.runID, nil); status != http.StatusNoContent {
		t.Fatalf("close status %d", status)
	}
	for _, tr := range transports {
		if _, err := tr.ask(t, s.addr, query(t, 1, "www.example.test", dnsmsg.TypeA)); err == nil {
			t.Fatalf("%s still answers after the scope was closed", tr.name)
		}
	}
}

func TestDoHRefusesWhatIsNotADoHRequest(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "http")
	valid := query(t, 1, "www.example.test", dnsmsg.TypeA)
	cases := []struct {
		field  string
		method string
		url    string
		body   []byte
		ctype  string
		want   int
	}{
		{"path", "GET", strings.Replace(dohGetURL(s.addr, valid, ""), dohPath, "/other", 1), nil, "", 404},
		{"method", "PUT", dohGetURL(s.addr, valid, ""), nil, "", 405},
		{"dns parameter missing", "GET", "http://" + s.addr + dohPath, nil, "", 400},
		{"dns parameter not base64url", "GET", "http://" + s.addr + dohPath + "?dns=***", nil, "", 400},
		{"content type", "POST", "http://" + s.addr + dohPath, valid, "text/plain", 415},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			var h map[string]string
			if c.ctype != "" {
				h = map[string]string{"Content-Type": c.ctype}
			}
			status, _, err := doh(c.method, c.url, c.body, h)
			if err != nil || status != c.want {
				t.Fatalf("status %d err %v, want %d", status, err, c.want)
			}
		})
	}
}

func TestDoHBaggageForAnotherRunIsRecordedAndServed(t *testing.T) {
	tm := startMock(t)
	s := tm.open(t, "http")
	status, _, err := doh(http.MethodGet, dohGetURL(s.addr, query(t, 1, "www.example.test", dnsmsg.TypeA), ""), nil, map[string]string{"baggage": "mocks.run=other-run"})
	if err != nil || status != 200 {
		t.Fatalf("status %d err %v, want 200", status, err)
	}
	evs := tm.events(t, func(evs []map[string]any) bool {
		return hasKind(evs, "scope.mismatch") && hasKind(evs, "response.sent")
	})
	if ev := kindOf(evs, "scope.mismatch"); ev["run_id"] != s.runID || !strings.Contains(ev["summary"].(string), "other-run") {
		t.Fatalf("scope.mismatch = %v, want run %s naming other-run", ev, s.runID)
	}
}

func TestOpenRefusals(t *testing.T) {
	tm := startMock(t)
	mut := func(f func(m map[string]any)) map[string]any {
		doc := map[string]any{"run_id": "refusal-run", "listener": map[string]any{"protocol": "dns", "host": "127.0.0.1", "port": 1}}
		f(doc)
		return doc
	}
	cases := []struct {
		field  string
		body   map[string]any
		status int
		code   string
	}{
		{"run_id", mut(func(m map[string]any) { m["run_id"] = "A1" }), 422, "run-id-invalid"},
		{"listener.protocol", mut(func(m map[string]any) { m["listener"].(map[string]any)["protocol"] = "tcp" }), 422, "listener-protocol-unsupported"},
		{"listener.host", mut(func(m map[string]any) { m["listener"].(map[string]any)["host"] = "0.0.0.0" }), 409, "listener-unavailable"},
	}
	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			status, doc := tm.control(t, "POST", "/scopes", c.body)
			if status != c.status || doc["reason_code"] != c.code {
				t.Fatalf("status %d body %v, want %d %s", status, doc, c.status, c.code)
			}
		})
	}
}

func TestControlAddressMustBeLoopback(t *testing.T) {
	if m, err := Start(Config{ControlAddr: "0.0.0.0:0"}); err == nil {
		m.Close()
		t.Fatal("a control endpoint on 0.0.0.0 was started")
	}
}
