// Package dnsmsg is a small codec for the DNS message format of RFC 1035 (section 4), with the
// AAAA record of RFC 3596. It reads the header and the questions of a query and packs and
// unpacks the five record types the mock answers: A, AAAA, CNAME, TXT and MX. Names are kept in
// presentation form without a trailing dot ("." is the root), with "\." "\\" and "\DDD" escapes.
package dnsmsg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Record types and classes.
const (
	TypeA     uint16 = 1
	TypeCNAME uint16 = 5
	TypeMX    uint16 = 15
	TypeTXT   uint16 = 16
	TypeAAAA  uint16 = 28
	TypeANY   uint16 = 255

	ClassIN uint16 = 1
)

// Response codes (RFC 1035 section 4.1.1).
const (
	RcodeNoError  uint8 = 0
	RcodeFormErr  uint8 = 1
	RcodeServFail uint8 = 2
	RcodeNXDomain uint8 = 3
	RcodeNotImp   uint8 = 4
	RcodeRefused  uint8 = 5
)

const (
	headerLen   = 12
	maxLabel    = 63
	maxNameWire = 255
	maxPointers = 10
)

// ErrFormat marks a message that is not a well-formed DNS message.
var ErrFormat = errors.New("malformed DNS message")

func formatErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFormat, fmt.Sprintf(format, args...))
}

// Header is the fixed part of a message.
type Header struct {
	ID                 uint16
	Response           bool
	Opcode             uint8
	Authoritative      bool
	Truncated          bool
	RecursionDesired   bool
	RecursionAvailable bool
	Rcode              uint8
}

// Question is one entry of the question section.
type Question struct {
	Name  string
	Type  uint16
	Class uint16
}

// RR is one resource record. Only the fields of its Type are used: Addr for A and AAAA, Target for
// CNAME (and the exchange of MX), Pref for MX, Text for TXT; Raw holds the rdata of any other type.
type RR struct {
	Name   string
	Type   uint16
	Class  uint16
	TTL    uint32
	Addr   netip.Addr
	Target string
	Pref   uint16
	Text   []string
	Raw    []byte
}

// Message is a whole message. A query that Unpack accepts has exactly one question.
type Message struct {
	Header     Header
	Questions  []Question
	Answers    []RR
	Authority  []RR
	Additional []RR
}

// ReadID returns the id of a message when it has at least two bytes, else 0 and false.
func ReadID(msg []byte) (uint16, bool) {
	if len(msg) < 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(msg), true
}

// Unpack decodes a message. Any defect is an error wrapping ErrFormat.
func Unpack(msg []byte) (*Message, error) {
	if len(msg) < headerLen {
		return nil, formatErr("%d bytes is shorter than the 12-byte header", len(msg))
	}
	m := &Message{Header: unpackHeader(msg)}
	qd := int(binary.BigEndian.Uint16(msg[4:]))
	an := int(binary.BigEndian.Uint16(msg[6:]))
	ns := int(binary.BigEndian.Uint16(msg[8:]))
	ar := int(binary.BigEndian.Uint16(msg[10:]))
	off := headerLen
	for i := 0; i < qd; i++ {
		name, next, err := unpackName(msg, off)
		if err != nil {
			return nil, err
		}
		if next+4 > len(msg) {
			return nil, formatErr("question %d ends inside its type and class", i+1)
		}
		m.Questions = append(m.Questions, Question{Name: name, Type: binary.BigEndian.Uint16(msg[next:]), Class: binary.BigEndian.Uint16(msg[next+2:])})
		off = next + 4
	}
	for _, sec := range []struct {
		n   int
		dst *[]RR
	}{{an, &m.Answers}, {ns, &m.Authority}, {ar, &m.Additional}} {
		for i := 0; i < sec.n; i++ {
			rr, next, err := unpackRR(msg, off)
			if err != nil {
				return nil, err
			}
			*sec.dst = append(*sec.dst, rr)
			off = next
		}
	}
	return m, nil
}

func unpackHeader(msg []byte) Header {
	flags := binary.BigEndian.Uint16(msg[2:])
	return Header{
		ID:                 binary.BigEndian.Uint16(msg),
		Response:           flags&0x8000 != 0,
		Opcode:             uint8(flags >> 11 & 0xF),
		Authoritative:      flags&0x0400 != 0,
		Truncated:          flags&0x0200 != 0,
		RecursionDesired:   flags&0x0100 != 0,
		RecursionAvailable: flags&0x0080 != 0,
		Rcode:              uint8(flags & 0xF),
	}
}

// Pack encodes the message without name compression.
func (m *Message) Pack() ([]byte, error) {
	var flags uint16
	if m.Header.Response {
		flags |= 0x8000
	}
	flags |= uint16(m.Header.Opcode&0xF) << 11
	if m.Header.Authoritative {
		flags |= 0x0400
	}
	if m.Header.Truncated {
		flags |= 0x0200
	}
	if m.Header.RecursionDesired {
		flags |= 0x0100
	}
	if m.Header.RecursionAvailable {
		flags |= 0x0080
	}
	flags |= uint16(m.Header.Rcode & 0xF)
	b := make([]byte, headerLen, 512)
	binary.BigEndian.PutUint16(b, m.Header.ID)
	binary.BigEndian.PutUint16(b[2:], flags)
	binary.BigEndian.PutUint16(b[4:], uint16(len(m.Questions)))
	binary.BigEndian.PutUint16(b[6:], uint16(len(m.Answers)))
	binary.BigEndian.PutUint16(b[8:], uint16(len(m.Authority)))
	binary.BigEndian.PutUint16(b[10:], uint16(len(m.Additional)))
	var err error
	for _, q := range m.Questions {
		if b, err = packName(b, q.Name); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint16(b, q.Type)
		b = binary.BigEndian.AppendUint16(b, q.Class)
	}
	for _, sec := range [][]RR{m.Answers, m.Authority, m.Additional} {
		for _, rr := range sec {
			if b, err = packRR(b, rr); err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

func packRR(b []byte, rr RR) ([]byte, error) {
	b, err := packName(b, rr.Name)
	if err != nil {
		return nil, err
	}
	b = binary.BigEndian.AppendUint16(b, rr.Type)
	b = binary.BigEndian.AppendUint16(b, rr.Class)
	b = binary.BigEndian.AppendUint32(b, rr.TTL)
	lenAt := len(b)
	b = append(b, 0, 0)
	switch rr.Type {
	case TypeA:
		if !rr.Addr.Is4() {
			return nil, fmt.Errorf("A record %q needs an IPv4 address, got %v", rr.Name, rr.Addr)
		}
		a := rr.Addr.As4()
		b = append(b, a[:]...)
	case TypeAAAA:
		if !rr.Addr.Is6() {
			return nil, fmt.Errorf("AAAA record %q needs an IPv6 address, got %v", rr.Name, rr.Addr)
		}
		a := rr.Addr.As16()
		b = append(b, a[:]...)
	case TypeCNAME:
		if b, err = packName(b, rr.Target); err != nil {
			return nil, err
		}
	case TypeMX:
		b = binary.BigEndian.AppendUint16(b, rr.Pref)
		if b, err = packName(b, rr.Target); err != nil {
			return nil, err
		}
	case TypeTXT:
		if len(rr.Text) == 0 {
			return nil, fmt.Errorf("TXT record %q has no string", rr.Name)
		}
		for _, s := range rr.Text {
			if len(s) > 255 {
				return nil, fmt.Errorf("TXT record %q has a string of %d bytes, the limit is 255", rr.Name, len(s))
			}
			b = append(b, byte(len(s)))
			b = append(b, s...)
		}
	default:
		b = append(b, rr.Raw...)
	}
	rdlen := len(b) - lenAt - 2
	if rdlen > 0xFFFF {
		return nil, fmt.Errorf("rdata of %q is %d bytes", rr.Name, rdlen)
	}
	binary.BigEndian.PutUint16(b[lenAt:], uint16(rdlen))
	return b, nil
}

func unpackRR(msg []byte, off int) (RR, int, error) {
	name, next, err := unpackName(msg, off)
	if err != nil {
		return RR{}, 0, err
	}
	if next+10 > len(msg) {
		return RR{}, 0, formatErr("a record ends inside its fixed fields")
	}
	rr := RR{Name: name, Type: binary.BigEndian.Uint16(msg[next:]), Class: binary.BigEndian.Uint16(msg[next+2:]), TTL: binary.BigEndian.Uint32(msg[next+4:])}
	rdlen := int(binary.BigEndian.Uint16(msg[next+8:]))
	start := next + 10
	end := start + rdlen
	if end > len(msg) {
		return RR{}, 0, formatErr("the rdata of %q runs past the end of the message", name)
	}
	rd := msg[start:end]
	switch rr.Type {
	case TypeA:
		if len(rd) != 4 {
			return RR{}, 0, formatErr("an A record has %d bytes of rdata, want 4", len(rd))
		}
		rr.Addr = netip.AddrFrom4([4]byte(rd))
	case TypeAAAA:
		if len(rd) != 16 {
			return RR{}, 0, formatErr("an AAAA record has %d bytes of rdata, want 16", len(rd))
		}
		rr.Addr = netip.AddrFrom16([16]byte(rd))
	case TypeCNAME:
		if rr.Target, _, err = unpackName(msg[:end], start); err != nil {
			return RR{}, 0, err
		}
	case TypeMX:
		if len(rd) < 3 {
			return RR{}, 0, formatErr("an MX record has %d bytes of rdata, want at least 3", len(rd))
		}
		rr.Pref = binary.BigEndian.Uint16(rd)
		if rr.Target, _, err = unpackName(msg[:end], start+2); err != nil {
			return RR{}, 0, err
		}
	case TypeTXT:
		for i := 0; i < len(rd); {
			n := int(rd[i])
			if i+1+n > len(rd) {
				return RR{}, 0, formatErr("a TXT string runs past its rdata")
			}
			rr.Text = append(rr.Text, string(rd[i+1:i+1+n]))
			i += 1 + n
		}
	default:
		rr.Raw = append([]byte(nil), rd...)
	}
	return rr, end, nil
}

// unpackName reads a possibly compressed name at off and returns it with the offset after it.
func unpackName(msg []byte, off int) (string, int, error) {
	var sb strings.Builder
	next, pointers, wire := -1, 0, 0
	for {
		if off >= len(msg) {
			return "", 0, formatErr("a name runs past the end of the message")
		}
		c := int(msg[off])
		off++
		switch c & 0xC0 {
		case 0x00:
			if c == 0 {
				if next < 0 {
					next = off
				}
				if sb.Len() == 0 {
					return ".", next, nil
				}
				return sb.String(), next, nil
			}
			if off+c > len(msg) {
				return "", 0, formatErr("a label of %d bytes runs past the end of the message", c)
			}
			if wire += c + 1; wire > maxNameWire-1 {
				return "", 0, formatErr("a name is longer than %d bytes", maxNameWire)
			}
			if sb.Len() > 0 {
				sb.WriteByte('.')
			}
			escapeLabel(&sb, msg[off:off+c])
			off += c
		case 0xC0:
			if off >= len(msg) {
				return "", 0, formatErr("a compression pointer is cut off")
			}
			ptr := (c&0x3F)<<8 | int(msg[off])
			off++
			if next < 0 {
				next = off
			}
			if pointers++; pointers > maxPointers {
				return "", 0, formatErr("a name follows more than %d compression pointers", maxPointers)
			}
			if ptr >= len(msg) {
				return "", 0, formatErr("a compression pointer targets offset %d, outside the message", ptr)
			}
			off = ptr
		default:
			return "", 0, formatErr("a label has the reserved length bits %#x", c&0xC0)
		}
	}
}

func escapeLabel(sb *strings.Builder, label []byte) {
	for _, c := range label {
		switch {
		case c == '.' || c == '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c <= 0x20 || c >= 0x7f:
			sb.WriteString(fmt.Sprintf("\\%03d", c))
		default:
			sb.WriteByte(c)
		}
	}
}

// packName appends the uncompressed wire form of a presentation name.
func packName(b []byte, name string) ([]byte, error) {
	labels, err := splitName(name)
	if err != nil {
		return nil, err
	}
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0), nil
}

// splitName parses a presentation name into its unescaped labels.
func splitName(name string) ([]string, error) {
	if name == "" || name == "." {
		return nil, nil
	}
	name = strings.TrimSuffix(name, ".")
	var labels []string
	var cur []byte
	total := 1
	flush := func() error {
		if len(cur) == 0 || len(cur) > maxLabel {
			return fmt.Errorf("name %q has a label of %d bytes, want 1 to %d", name, len(cur), maxLabel)
		}
		total += len(cur) + 1
		labels = append(labels, string(cur))
		cur = cur[:0]
		return nil
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c == '.':
			if err := flush(); err != nil {
				return nil, err
			}
		case c == '\\':
			if i+1 >= len(name) {
				return nil, fmt.Errorf("name %q ends in a lone backslash", name)
			}
			if d := name[i+1]; d >= '0' && d <= '9' {
				if i+3 >= len(name) {
					return nil, fmt.Errorf("name %q has a short \\DDD escape", name)
				}
				n, err := strconv.Atoi(name[i+1 : i+4])
				if err != nil || n > 255 {
					return nil, fmt.Errorf("name %q has a bad \\DDD escape", name)
				}
				cur = append(cur, byte(n))
				i += 3
			} else {
				cur = append(cur, d)
				i++
			}
		default:
			cur = append(cur, c)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if total > maxNameWire {
		return nil, fmt.Errorf("name %q is %d bytes on the wire, the limit is %d", name, total, maxNameWire)
	}
	return labels, nil
}

// CheckName reports whether name can be packed, and why not.
func CheckName(name string) error {
	_, err := splitName(name)
	return err
}

// TypeName is the mnemonic of a record type, or TYPE<n>.
func TypeName(t uint16) string {
	switch t {
	case TypeA:
		return "A"
	case TypeAAAA:
		return "AAAA"
	case TypeCNAME:
		return "CNAME"
	case TypeMX:
		return "MX"
	case TypeTXT:
		return "TXT"
	case TypeANY:
		return "ANY"
	}
	return "TYPE" + strconv.Itoa(int(t))
}

// ParseType is the inverse of TypeName for the five answered types.
func ParseType(s string) (uint16, bool) {
	for _, t := range []uint16{TypeA, TypeAAAA, TypeCNAME, TypeMX, TypeTXT} {
		if TypeName(t) == s {
			return t, true
		}
	}
	return 0, false
}

// RcodeName is the mnemonic of a response code, or RCODE<n>.
func RcodeName(r uint8) string {
	switch r {
	case RcodeNoError:
		return "NOERROR"
	case RcodeFormErr:
		return "FORMERR"
	case RcodeServFail:
		return "SERVFAIL"
	case RcodeNXDomain:
		return "NXDOMAIN"
	case RcodeNotImp:
		return "NOTIMP"
	case RcodeRefused:
		return "REFUSED"
	}
	return "RCODE" + strconv.Itoa(int(r))
}
