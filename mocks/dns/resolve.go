package dnsmock

import (
	"strings"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

const maxCNAMEHops = 8

// verdict is what the settings make of one question.
type verdict struct {
	rcode   uint8
	answers []dnsmsg.RR
	blocked bool
}

func (st runSettings) isBlocked(name string) bool {
	for _, b := range st.blocked {
		if name == b || strings.HasSuffix(name, "."+b) {
			return true
		}
	}
	return false
}

func (st runSettings) at(name string) []declaredRecord {
	var out []declaredRecord
	for _, r := range st.records {
		if r.name == name {
			out = append(out, r)
		}
	}
	return out
}

// nameExists is true for a name that owns a record or has one below it (an empty non-terminal
// answers NOERROR, RFC 8020).
func (st runSettings) nameExists(name string) bool {
	for _, r := range st.records {
		if r.name == name || strings.HasSuffix(r.name, "."+name) {
			return true
		}
	}
	return false
}

func (r declaredRecord) rr(owner string) dnsmsg.RR {
	return dnsmsg.RR{Name: owner, Type: r.typ, Class: dnsmsg.ClassIN, TTL: r.ttl, Addr: r.addr, Target: r.target, Pref: r.pref, Text: txtStrings(r.text)}
}

func txtStrings(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

// resolve answers one question from the declared blocks and records. A blocked name is judged
// before any record, so a block always wins over a declaration.
func (st runSettings) resolve(q dnsmsg.Question) verdict {
	if q.Class != dnsmsg.ClassIN {
		return verdict{rcode: dnsmsg.RcodeRefused}
	}
	name := normName(q.Name)
	if st.isBlocked(name) {
		return st.blockedAnswer(q)
	}
	var out []dnsmsg.RR
	cur, chained := name, false
	for hops := 0; hops <= maxCNAMEHops; hops++ {
		recs := st.at(cur)
		if len(recs) == 1 && recs[0].typ == dnsmsg.TypeCNAME && q.Type != dnsmsg.TypeCNAME {
			out = append(out, recs[0].rr(cur))
			cur, chained = recs[0].target, true
			continue
		}
		for _, r := range recs {
			if q.Type == dnsmsg.TypeANY || r.typ == q.Type {
				out = append(out, r.rr(cur))
			}
		}
		break
	}
	if len(out) == 0 && !chained && !st.nameExists(name) {
		return verdict{rcode: dnsmsg.RcodeNXDomain}
	}
	return verdict{rcode: dnsmsg.RcodeNoError, answers: out}
}

func (st runSettings) blockedAnswer(q dnsmsg.Question) verdict {
	switch st.blockAnswer {
	case "refused":
		return verdict{rcode: dnsmsg.RcodeRefused, blocked: true}
	case "sinkhole":
		v := verdict{rcode: dnsmsg.RcodeNoError, blocked: true}
		wantType := dnsmsg.TypeA
		if st.sinkhole.Is6() {
			wantType = dnsmsg.TypeAAAA
		}
		if q.Type == wantType || q.Type == dnsmsg.TypeANY {
			v.answers = []dnsmsg.RR{{Name: normName(q.Name), Type: wantType, Class: dnsmsg.ClassIN, TTL: defaultTTL, Addr: st.sinkhole}}
		}
		return v
	}
	return verdict{rcode: dnsmsg.RcodeNXDomain, blocked: true}
}
