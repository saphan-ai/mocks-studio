package dnsmock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

// queryReq is one DNS message as it arrived, with what the transport knows about it.
type queryReq struct {
	raw         []byte
	transport   string   // "udp", "tcp" or "doh"
	corrID      string   // from traceparent (doh only); empty means generate
	corrSource  string   // "traceparent" or "generated"
	mismatchRun string   // the run a baggage header named, when it is not this scope's
	creds       []string // the NAMES of the credentials the caller sent, never their values
}

// queryResp is what goes back: a DNS message, nothing (a dropped UDP query), or, for DoH, an
// injected HTTP status in place of the message.
type queryResp struct {
	wire       []byte
	drop       bool
	httpStatus int
}

func (q queryReq) protocol() string {
	if q.transport == "doh" {
		return "http"
	}
	return "dns"
}

// process answers one query; it registers itself as in flight and refuses once the scope closes.
func (s *scope) process(q queryReq) queryResp {
	if !s.enter() {
		return queryResp{drop: true}
	}
	defer s.wg.Done()
	return s.processEntered(q)
}

func digest(b []byte) map[string]any {
	sum := sha256.Sum256(b)
	return map[string]any{"redacted": true, "digest": "sha256:" + hex.EncodeToString(sum[:]), "size": len(b)}
}

func fields(f map[string]any) map[string]any { return map[string]any{"redacted": false, "fields": f} }

// processEntered is the one path every transport takes: parse, record the query, apply the run's
// faults, resolve, record the answer.
func (s *scope) processEntered(q queryReq) queryResp {
	start := time.Now()
	m := s.m
	proto := q.protocol()
	corr, source := q.corrID, q.corrSource
	if corr == "" {
		corr, source = randomHex(idBytes), "generated"
	}
	common := map[string]any{"correlation_id": corr, "correlation_source": source}
	if q.mismatchRun != "" && q.mismatchRun != s.runID {
		m.publish(s.runID, "scope.mismatch", "internal", proto,
			fmt.Sprintf("call on run %s endpoint carried baggage for run %s", s.runID, q.mismatchRun), common)
	}
	st := s.current()

	id, _ := dnsmsg.ReadID(q.raw)
	msg, perr := dnsmsg.Unpack(q.raw)
	var question *dnsmsg.Question
	var rd bool
	var opcode, invalid uint8
	switch {
	case perr != nil, msg.Header.Response, len(msg.Questions) != 1:
		invalid = dnsmsg.RcodeFormErr
	case msg.Header.Opcode != 0:
		invalid = dnsmsg.RcodeNotImp
	}
	if perr == nil {
		rd, opcode = msg.Header.RecursionDesired, msg.Header.Opcode
		if len(msg.Questions) == 1 {
			question = &msg.Questions[0]
		}
	}

	qname, qtype, what := "", "", fmt.Sprintf("malformed message (%d bytes)", len(q.raw))
	recv := mergeExtra(common, map[string]any{"payload": digest(q.raw)})
	if question != nil {
		qname, qtype = question.Name, dnsmsg.TypeName(question.Type)
		what = qtype + " " + qname
		recv["payload"] = fields(map[string]any{"qname": qname, "qtype": qtype, "transport": q.transport})
	}
	m.publish(s.runID, "request.received", "inbound", proto, what+" over "+q.transport, recv)

	if st.latency > 0 {
		t := time.NewTimer(time.Duration(st.latency) * time.Millisecond)
		select {
		case <-t.C:
		case <-s.done:
			t.Stop()
			return queryResp{drop: true}
		}
	}
	latency := func() int { return int(time.Since(start).Milliseconds()) }
	respFields := func(rcode string) map[string]any {
		f := map[string]any{"transport": q.transport}
		if question != nil {
			f["qname"], f["qtype"] = qname, qtype
		}
		if rcode != "" {
			f["rcode"] = rcode
		}
		return f
	}
	inject := func(setting, detail string) {
		m.publish(s.runID, "fault.injected", "internal", proto, fmt.Sprintf("%s injected for %s over %s%s", setting, what, q.transport, detail),
			mergeExtra(common, map[string]any{"outcome": "fault", "fault": setting}))
	}

	if q.transport == "doh" && st.dohStatus != 200 {
		inject("doh_http_status", fmt.Sprintf(" (HTTP %d)", st.dohStatus))
		m.publish(s.runID, "response.sent", "outbound", proto, fmt.Sprintf("HTTP %d for %s", st.dohStatus, what),
			mergeExtra(common, map[string]any{"outcome": "fault", "fault": "doh_http_status", "latency_ms": latency(), "payload": fields(respFields(""))}))
		m.log.Printf("query run=%s transport=doh qname=%s qtype=%s http=%d fault=doh_http_status credentials=%s", s.runID, qname, qtype, st.dohStatus, credList(q.creds))
		return queryResp{httpStatus: st.dohStatus}
	}
	if q.transport == "udp" && st.dropUDP {
		inject("drop_udp", "")
		m.log.Printf("query run=%s transport=udp qname=%s qtype=%s dropped fault=drop_udp", s.runID, qname, qtype)
		return queryResp{drop: true}
	}

	rcode, outcome, fault := invalid, "ok", ""
	var answers []dnsmsg.RR
	switch {
	case st.servfail:
		inject("servfail", "")
		rcode, outcome, fault = dnsmsg.RcodeServFail, "fault", "servfail"
	case invalid != dnsmsg.RcodeNoError:
		outcome = "refused"
	default:
		v := st.resolve(*question)
		rcode, answers = v.rcode, v.answers
		if v.blocked || v.rcode == dnsmsg.RcodeRefused {
			outcome, fault = "refused", ""
			if v.blocked {
				fault = "blocked"
			}
		}
	}

	wire := packResponse(dnsmsg.Header{ID: id, Response: true, Opcode: opcode, RecursionDesired: rd, RecursionAvailable: true, Rcode: rcode}, question, answers, q.transport == "udp")
	rname := dnsmsg.RcodeName(rcode)
	extra := mergeExtra(common, map[string]any{"outcome": outcome, "latency_ms": latency(), "payload": fields(respFields(rname))})
	if fault != "" {
		extra["fault"] = fault
	}
	m.publish(s.runID, "response.sent", "outbound", proto, fmt.Sprintf("%s for %s over %s", rname, what, q.transport), extra)
	m.log.Printf("query run=%s transport=%s qname=%s qtype=%s rcode=%s credentials=%s", s.runID, q.transport, qname, qtype, rname, credList(q.creds))
	return queryResp{wire: wire}
}

func credList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

// packResponse builds the answer; over UDP an answer past 512 bytes is cut to its header and
// question with the truncation bit set, so the client retries over TCP.
func packResponse(h dnsmsg.Header, question *dnsmsg.Question, answers []dnsmsg.RR, udp bool) []byte {
	out := dnsmsg.Message{Header: h, Answers: answers}
	if question != nil {
		out.Questions = []dnsmsg.Question{*question}
	}
	wire, err := out.Pack()
	if err == nil && udp && len(wire) > maxUDPWire {
		out.Answers, out.Header.Truncated = nil, true
		wire, err = out.Pack()
	}
	if err != nil { // an answer the codec cannot pack is the mock's defect; the client still gets a well-formed SERVFAIL
		h.Rcode = dnsmsg.RcodeServFail
		wire, _ = (&dnsmsg.Message{Header: h}).Pack() // a header alone always packs
	}
	return wire
}

func mergeExtra(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
