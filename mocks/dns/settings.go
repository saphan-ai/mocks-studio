package dnsmock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/saphan-ai/mocks-studio/mocks/dns/dnsmsg"
)

// zones are the only names the mock declares or answers records for.
var zones = []string{"example.test", "acme-example.test"}

// reservedRanges are the only addresses a record or the sinkhole may carry (RFC 5737, RFC 3849).
var reservedRanges = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

const (
	maxDeclared  = 256
	defaultTTL   = 60
	maxTTL       = 86400
	maxTXTString = 255
)

func reservedRangeNames() []string {
	out := make([]string, len(reservedRanges))
	for i, p := range reservedRanges {
		out[i] = p.String()
	}
	return out
}

func inReserved(a netip.Addr) bool {
	if a.Is4In6() {
		return false
	}
	for _, p := range reservedRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// normName lowercases a presentation name and drops its trailing dot.
func normName(s string) string {
	return strings.TrimSuffix(strings.ToLower(s), ".")
}

func inZone(name string) bool {
	for _, z := range zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			return true
		}
	}
	return false
}

// declaredRecord is one entry of the records setting after validation.
type declaredRecord struct {
	name   string
	typ    uint16
	ttl    uint32
	addr   netip.Addr
	target string
	pref   uint16
	text   string
}

// runSettings is the parsed form of one run's settings, rebuilt whenever they change.
type runSettings struct {
	dohStatus   int
	latency     int
	servfail    bool
	dropUDP     bool
	records     []declaredRecord
	blocked     []string
	blockAnswer string
	sinkhole    netip.Addr
}

// problem is why a setting value was refused: a wrong shape is setting-type-invalid, a right
// shape with a value the mock will not serve is setting-out-of-range with the allowed list.
type problem struct {
	code    string
	msg     string
	allowed []string
}

func shapeProblem(format string, args ...any) *problem {
	return &problem{code: "setting-type-invalid", msg: fmt.Sprintf(format, args...)}
}

func rangeProblem(allowed []string, format string, args ...any) *problem {
	return &problem{code: "setting-out-of-range", msg: fmt.Sprintf(format, args...), allowed: allowed}
}

var zoneList = func() []string {
	out := make([]string, len(zones))
	for i, z := range zones {
		out[i] = "*." + z
	}
	return out
}()

func parseDeclaredName(field, raw string) (string, *problem) {
	n := normName(raw)
	if err := dnsmsg.CheckName(n); err != nil || n == "" {
		return "", shapeProblem("The %s %q is not a valid DNS name and the setting was refused; send names such as www.example.test.", field, raw)
	}
	if !inZone(n) {
		return "", rangeProblem(zoneList, "The %s %q is outside the zones this mock serves and the setting was refused; use a name under example.test or acme-example.test.", field, raw)
	}
	return n, nil
}

func decodeStrict(raw string, into any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

func parseRecords(raw string) ([]declaredRecord, *problem) {
	var in []struct {
		Name  string `json:"name"`
		Type  string `json:"type"`
		Value string `json:"value"`
		TTL   *int   `json:"ttl"`
	}
	if err := decodeStrict(raw, &in); err != nil {
		return nil, shapeProblem("The records value is not a JSON array of {name,type,value,ttl} objects (%v) and the setting was refused; send such an array.", err)
	}
	if len(in) > maxDeclared {
		return nil, rangeProblem(nil, "The records value declares %d records and the setting was refused; declare at most %d.", len(in), maxDeclared)
	}
	var out []declaredRecord
	cnames := map[string]bool{}
	others := map[string]bool{}
	for i, r := range in {
		name, p := parseDeclaredName(fmt.Sprintf("name of record %d", i), r.Name)
		if p != nil {
			return nil, p
		}
		typ, ok := dnsmsg.ParseType(r.Type)
		if !ok {
			return nil, rangeProblem([]string{"A", "AAAA", "CNAME", "TXT", "MX"}, "The type %q of record %d is not one this mock answers and the setting was refused; use A, AAAA, CNAME, TXT or MX.", r.Type, i)
		}
		rec := declaredRecord{name: name, typ: typ, ttl: defaultTTL}
		if r.TTL != nil {
			if *r.TTL < 0 || *r.TTL > maxTTL {
				return nil, rangeProblem(nil, "The ttl %d of record %d is outside 0..%d and the setting was refused; send a ttl in that range.", *r.TTL, i, maxTTL)
			}
			rec.ttl = uint32(*r.TTL)
		}
		if p := fillValue(&rec, i, r.Value); p != nil {
			return nil, p
		}
		if typ == dnsmsg.TypeCNAME {
			if cnames[name] || others[name] {
				return nil, shapeProblem("The name %q has a CNAME and another record and the setting was refused; a CNAME owns its name alone (RFC 1034 section 3.6.2).", name)
			}
			cnames[name] = true
		} else {
			if cnames[name] {
				return nil, shapeProblem("The name %q has a CNAME and another record and the setting was refused; a CNAME owns its name alone (RFC 1034 section 3.6.2).", name)
			}
			others[name] = true
		}
		out = append(out, rec)
	}
	return out, nil
}

func fillValue(rec *declaredRecord, i int, value string) *problem {
	switch rec.typ {
	case dnsmsg.TypeA, dnsmsg.TypeAAAA:
		a, err := netip.ParseAddr(value)
		if err != nil || (rec.typ == dnsmsg.TypeA && !a.Is4()) || (rec.typ == dnsmsg.TypeAAAA && (!a.Is6() || a.Is4In6())) {
			return shapeProblem("The value %q of record %d is not an address of the record's family and the setting was refused; send an IPv4 address for A and an IPv6 address for AAAA.", value, i)
		}
		if !inReserved(a) {
			return rangeProblem(reservedRangeNames(), "The address %s of record %d is outside the reserved documentation ranges and the setting was refused; use one of the allowed ranges.", a, i)
		}
		rec.addr = a
	case dnsmsg.TypeCNAME:
		t, p := parseDeclaredName(fmt.Sprintf("target of record %d", i), value)
		if p != nil {
			return p
		}
		rec.target = t
	case dnsmsg.TypeMX:
		prefRaw, host, ok := strings.Cut(strings.TrimSpace(value), " ")
		pref, err := strconv.Atoi(prefRaw)
		if !ok || err != nil || pref < 0 || pref > math.MaxUint16 {
			return shapeProblem("The MX value %q of record %d is not \"<preference> <host>\" with a preference of 0..65535 and the setting was refused; send for example \"10 mail.example.test\".", value, i)
		}
		t, p := parseDeclaredName(fmt.Sprintf("exchange of record %d", i), strings.TrimSpace(host))
		if p != nil {
			return p
		}
		rec.pref, rec.target = uint16(pref), t
	case dnsmsg.TypeTXT:
		if len(value) == 0 || len(value) > maxTXTString {
			return rangeProblem(nil, "The TXT value of record %d is %d bytes and the setting was refused; send 1 to %d bytes.", i, len(value), maxTXTString)
		}
		rec.text = value
	}
	return nil
}

func parseBlocked(raw string) ([]string, *problem) {
	var in []string
	if err := decodeStrict(raw, &in); err != nil {
		return nil, shapeProblem("The blocked value is not a JSON array of names (%v) and the setting was refused; send such an array.", err)
	}
	if len(in) > maxDeclared {
		return nil, rangeProblem(nil, "The blocked value lists %d names and the setting was refused; list at most %d.", len(in), maxDeclared)
	}
	out := make([]string, 0, len(in))
	for i, s := range in {
		n, p := parseDeclaredName(fmt.Sprintf("blocked name %d", i), s)
		if p != nil {
			return nil, p
		}
		out = append(out, n)
	}
	return out, nil
}

func parseSinkhole(raw string) (netip.Addr, *problem) {
	a, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Addr{}, shapeProblem("The sinkhole_ip value %q is not an IP address and the setting was refused; send an address from a reserved range.", raw)
	}
	if !inReserved(a) {
		return netip.Addr{}, rangeProblem(reservedRangeNames(), "The sinkhole address %s is outside the reserved documentation ranges and the setting was refused; use one of the allowed ranges.", a)
	}
	return a, nil
}

// settingDef is one declared setting, decoded from the manifest so the mock cannot declare one
// thing and enforce another.
type settingDef struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Default any      `json:"default"`
	Allowed []any    `json:"allowed"`
	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "number":
		_, ok := v.(float64)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	default:
		_, ok := v.(bool)
		return ok
	}
}

// check judges one value against the declaration: type, then range or list, then the shape of
// the document the setting carries.
func (d settingDef) check(v any) *problem {
	if !typeMatches(d.Type, v) {
		return shapeProblem("The value of %q is not of type %s and the request was refused; send a %s.", d.Name, d.Type, d.Type)
	}
	if f, ok := v.(float64); ok && d.Minimum != nil && d.Maximum != nil && (f < *d.Minimum || f > *d.Maximum) {
		return rangeProblem(nil, "The value %v of %q is outside %v..%v and the request was refused; send a value inside the range.", f, d.Name, *d.Minimum, *d.Maximum)
	}
	if len(d.Allowed) > 0 {
		list := make([]string, len(d.Allowed))
		hit := false
		for i, a := range d.Allowed {
			list[i] = fmt.Sprint(a)
			hit = hit || a == v
		}
		if !hit {
			return rangeProblem(list, "The value %v of %q is not an allowed value and the request was refused; use one of the allowed values.", v, d.Name)
		}
	}
	if s, ok := v.(string); ok {
		var p *problem
		switch d.Name {
		case "records":
			_, p = parseRecords(s)
		case "blocked":
			_, p = parseBlocked(s)
		case "sinkhole_ip":
			_, p = parseSinkhole(s)
		}
		return p
	}
	return nil
}

// parse turns a validated settings map into its working form.
func parseSettings(raw map[string]any) runSettings {
	var st runSettings
	st.dohStatus = int(raw["doh_http_status"].(float64))
	st.latency = int(raw["latency_ms"].(float64))
	st.servfail = raw["servfail"].(bool)
	st.dropUDP = raw["drop_udp"].(bool)
	st.blockAnswer = raw["block_answer"].(string)
	// Every value was validated when it was set, so the problems are nil here.
	st.records, _ = parseRecords(raw["records"].(string))
	st.blocked, _ = parseBlocked(raw["blocked"].(string))
	st.sinkhole, _ = parseSinkhole(raw["sinkhole_ip"].(string))
	return st
}

func sortedNames(defs map[string]settingDef) []string {
	out := make([]string, 0, len(defs))
	for k := range defs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
