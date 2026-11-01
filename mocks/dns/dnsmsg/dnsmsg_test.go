package dnsmsg

import (
	"bytes"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func validQuery(t *testing.T) []byte {
	t.Helper()
	b, err := (&Message{
		Header:    Header{ID: 0xBEEF, RecursionDesired: true},
		Questions: []Question{{Name: "www.example.test", Type: TypeA, Class: ClassIN}},
	}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTripAllTypes(t *testing.T) {
	in := &Message{
		Header:    Header{ID: 7, Response: true, RecursionDesired: true, RecursionAvailable: true, Rcode: RcodeNoError},
		Questions: []Question{{Name: "www.example.test", Type: TypeANY, Class: ClassIN}},
		Answers: []RR{
			{Name: "www.example.test", Type: TypeA, Class: ClassIN, TTL: 60, Addr: netip.MustParseAddr("192.0.2.1")},
			{Name: "www.example.test", Type: TypeAAAA, Class: ClassIN, TTL: 61, Addr: netip.MustParseAddr("2001:db8::1")},
			{Name: "alias.example.test", Type: TypeCNAME, Class: ClassIN, TTL: 62, Target: "www.example.test"},
			{Name: "example.test", Type: TypeMX, Class: ClassIN, TTL: 63, Pref: 10, Target: "mail.example.test"},
			{Name: "example.test", Type: TypeTXT, Class: ClassIN, TTL: 64, Text: []string{"v=spf1 -all", "second"}},
		},
	}
	wire, err := in.Pack()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unpack(wire)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the message:\n in %+v\nout %+v", in, out)
	}
}

func TestUnpackFollowsCompression(t *testing.T) {
	// header, question www.example.test A IN, answer owner is a pointer to offset 12.
	wire := validQuery(t)
	wire[2] |= 0x80 // response
	wire[7] = 1     // ancount
	wire = append(wire, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 9)
	m, err := Unpack(wire)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Answers[0].Name; got != "www.example.test" {
		t.Fatalf("compressed owner decoded as %q", got)
	}
}

func TestUnpackRefusesEachDefect(t *testing.T) {
	base := validQuery(t)
	mutate := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), base...)) }
	cases := map[string][]byte{
		"empty":                     nil,
		"header one byte short":     base[:11],
		"question without class":    base[:len(base)-2],
		"label past the end":        mutate(func(b []byte) []byte { b[12] = 0x3F; return b }),
		"reserved label bits":       mutate(func(b []byte) []byte { b[12] = 0x80; return b }),
		"pointer to itself":         mutate(func(b []byte) []byte { b[12], b[13] = 0xC0, 0x0C; return b }),
		"pointer outside":           mutate(func(b []byte) []byte { b[12], b[13] = 0xC3, 0xFF; return b }),
		"answer counted but absent": mutate(func(b []byte) []byte { b[7] = 1; return b }),
		"name over 255 bytes": func() []byte {
			b := append([]byte(nil), base[:12]...)
			b[5] = 1
			for i := 0; i < 5; i++ {
				b = append(b, 63)
				b = append(b, bytes.Repeat([]byte("a"), 63)...)
			}
			return append(b, 0, 0, 1, 0, 1)
		}(),
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Unpack(wire); !errors.Is(err, ErrFormat) {
				t.Fatalf("Unpack error = %v, want ErrFormat", err)
			}
		})
	}
}

func TestPackRefusesBadNames(t *testing.T) {
	for name, n := range map[string]string{
		"empty label":    "a..example.test",
		"label over 63":  strings.Repeat("a", 64) + ".test",
		"lone backslash": "a\\",
		"short escape":   "a\\12",
	} {
		t.Run(name, func(t *testing.T) {
			if err := CheckName(n); err == nil {
				t.Fatalf("CheckName(%q) accepted it", n)
			}
		})
	}
}

func TestNameEscapesRoundTrip(t *testing.T) {
	name := `a\.b.exa\032mple.test`
	wire, err := (&Message{Questions: []Question{{Name: name, Type: TypeA, Class: ClassIN}}}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	m, err := Unpack(wire)
	if err != nil {
		t.Fatal(err)
	}
	if m.Questions[0].Name != name {
		t.Fatalf("escaped name came back as %q, want %q", m.Questions[0].Name, name)
	}
}

func TestPackRefusesWrongFamily(t *testing.T) {
	_, err := (&Message{Answers: []RR{{Name: "a.example.test", Type: TypeA, Class: ClassIN, Addr: netip.MustParseAddr("2001:db8::1")}}}).Pack()
	if err == nil {
		t.Fatal("an A record with an IPv6 address was packed")
	}
}

func FuzzUnpack(f *testing.F) {
	f.Add([]byte{0, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		// The property is that no input panics; a decoded message whose names cannot be
		// packed again (a label that decoded to nothing) is a legal outcome of either call.
		if m, err := Unpack(b); err == nil {
			_, _ = m.Pack()
		}
	})
}
