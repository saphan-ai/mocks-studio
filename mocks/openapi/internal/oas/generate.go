package oas

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// Limits of value generation, named so that no number hides in the code.
const (
	maxRefRevisits = 1 // how many times a schema may contain itself before the expansion is cut
	maxDepth       = 12
	maxGenLength   = 4096
)

var formatValues = map[string]string{
	"date-time": "2000-01-01T00:00:00Z",
	"date":      "2000-01-01",
	"time":      "00:00:00Z",
	"uuid":      "00000000-0000-4000-8000-000000000000",
	"email":     "user@example.com",
	"hostname":  "example.com",
	"uri":       "https://example.com/",
	"uri-ref":   "/example",
	"ipv4":      "192.0.2.1",
	"ipv6":      "2001:db8::1",
	"byte":      "AAAA",
	"duration":  "PT1S",
	"password":  "example",
}

// Generate returns a value that fits the schema. It is deterministic: the same schema yields the same value.
func (d *Document) Generate(schema any) any {
	g := &generator{doc: d, visits: map[string]int{}}
	return g.value(schema, 0)
}

type generator struct {
	doc    *Document
	visits map[string]int
}

func (g *generator) value(node any, depth int) any {
	m, ok := node.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	if ref, isRef := m["$ref"].(string); isRef {
		t, err := refTokens(ref)
		if err != nil {
			return nil
		}
		target, found := at(g.doc.root, t)
		if !found {
			return nil
		}
		if g.visits[ref] > maxRefRevisits || depth > maxDepth {
			return g.minimal(target)
		}
		g.visits[ref]++
		defer func() { g.visits[ref]-- }()
		return g.value(target, depth+1)
	}
	if v, ok := declared(m); ok {
		return v
	}
	if all, ok := m["allOf"].([]any); ok && len(all) > 0 {
		return g.allOf(m, all, depth)
	}
	for _, kw := range []string{"oneOf", "anyOf"} {
		if list, ok := m[kw].([]any); ok && len(list) > 0 && schemaType(m) == "" && m["properties"] == nil {
			return g.value(list[0], depth+1)
		}
	}
	t := schemaType(m)
	if t == "" {
		switch {
		case m["properties"] != nil:
			t = "object"
		case m["items"] != nil:
			t = "array"
		}
	}
	switch t {
	case "string":
		return genString(m)
	case "integer", "number":
		return genNumber(m, t == "integer")
	case "boolean":
		return false
	case "array":
		return g.array(m, depth)
	case "object":
		return g.object(m, depth)
	case "null":
		return nil
	}
	return map[string]any{}
}

// declared is a value the schema's author wrote down: const, example, examples, default or the first enum member.
func declared(m map[string]any) (any, bool) {
	if v, ok := m["const"]; ok {
		return v, true
	}
	if v, ok := m["example"]; ok {
		return v, true
	}
	if l, ok := m["examples"].([]any); ok && len(l) > 0 {
		return l[0], true
	}
	if v, ok := m["default"]; ok {
		return v, true
	}
	if l, ok := m["enum"].([]any); ok && len(l) > 0 {
		return l[0], true
	}
	return nil, false
}

// minimal is the smallest value of a schema's type, used where an expansion is cut.
func (g *generator) minimal(node any) any {
	m, _ := node.(map[string]any)
	switch schemaType(m) {
	case "object":
		return map[string]any{}
	case "array":
		return []any{}
	case "string":
		return ""
	case "integer", "number":
		return 0.0
	case "boolean":
		return false
	}
	return nil
}

func (g *generator) allOf(m map[string]any, all []any, depth int) any {
	merged := map[string]any{}
	var first any
	parts := all
	if m["properties"] != nil || schemaType(m) != "" {
		parts = append([]any{withoutKeys(m, "allOf")}, all...)
	}
	for _, p := range parts {
		v := g.value(p, depth+1)
		obj, isObj := v.(map[string]any)
		if !isObj {
			if first == nil {
				first = v
			}
			continue
		}
		for k, e := range obj {
			if _, has := merged[k]; !has {
				merged[k] = e
			}
		}
	}
	if len(merged) == 0 && first != nil {
		return first
	}
	return merged
}

func withoutKeys(m map[string]any, drop ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range drop {
		delete(out, k)
	}
	return out
}

func (g *generator) object(m map[string]any, depth int) any {
	out := map[string]any{}
	props, _ := m["properties"].(map[string]any)
	for _, name := range sortedKeys(props) {
		if pm, ok := props[name].(map[string]any); ok && pm["writeOnly"] == true {
			continue
		}
		out[name] = g.value(props[name], depth+1)
	}
	return out
}

func (g *generator) array(m map[string]any, depth int) any {
	n := 1
	if v, ok := number(m["minItems"]); ok && int(v) > n {
		n = int(v)
	}
	if v, ok := number(m["maxItems"]); ok && int(v) < n {
		n = int(v)
	}
	if n < 0 {
		n = 0
	}
	out := make([]any, 0, n)
	var items any = map[string]any{}
	if it, ok := m["items"]; ok {
		items = it
	}
	for range n {
		out = append(out, g.value(items, depth+1))
	}
	return out
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func genString(m map[string]any) any {
	s := "string"
	if f, ok := m["format"].(string); ok {
		if v, known := formatValues[f]; known {
			s = v
		}
	}
	if v, ok := number(m["minLength"]); ok && int(v) > len(s) && int(v) <= maxGenLength {
		s += strings.Repeat("x", int(v)-len(s))
	}
	if v, ok := number(m["maxLength"]); ok && int(v) >= 0 && int(v) < len(s) {
		s = s[:int(v)]
	}
	return s
}

func genNumber(m map[string]any, integer bool) any {
	v := 0.0
	step := 1.0
	if !integer {
		step = 0.5
	}
	if lo, ok := number(m["minimum"]); ok {
		v = lo
	}
	if lo, ok := number(m["exclusiveMinimum"]); ok {
		v = lo + step
	}
	if mult, ok := number(m["multipleOf"]); ok && mult > 0 {
		v = math.Ceil(v/mult) * mult
	}
	hi, hasHi := number(m["maximum"])
	if x, ok := number(m["exclusiveMaximum"]); ok {
		hi, hasHi = x-step, true
	}
	if hasHi && v > hi {
		v = hi
		if mult, ok := number(m["multipleOf"]); ok && mult > 0 {
			v = math.Floor(v/mult) * mult
		}
	}
	return v
}

// pickMedia orders media types for an answer: JSON first, then the rest by name.
func pickMedia(list []*Media) *Media {
	if len(list) == 0 {
		return nil
	}
	sorted := append([]*Media(nil), list...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ji, jj := mediaIsJSON(sorted[i].Type) && sorted[i].Type != "*/*", mediaIsJSON(sorted[j].Type) && sorted[j].Type != "*/*"
		return ji && !jj
	})
	return sorted[0]
}

// Answer is what an operation answers with.
type Answer struct {
	Status      int
	ContentType string // empty when the answer has no body
	Body        []byte
}

// SuccessAnswer answers from the operation's first 2xx response: its example, else a value generated from its schema.
func (op *Operation) SuccessAnswer() Answer {
	var chosen *Response
	for _, r := range op.Responses {
		if r.Status == "default" || r.Code < 200 || r.Code > 299 {
			continue
		}
		if chosen == nil || r.Code < chosen.Code || (r.Code == chosen.Code && !strings.HasSuffix(strings.ToUpper(r.Status), "XX")) {
			chosen = r
		}
	}
	if chosen == nil {
		for _, r := range op.Responses {
			if r.Status == "default" {
				chosen = r
			}
		}
	}
	if chosen == nil {
		return Answer{Status: 200}
	}
	return op.render(chosen, chosen.Code)
}

// AnswerFor answers a status a setting forces: the declared response for that status when there is one.
func (op *Operation) AnswerFor(status int) (Answer, bool) {
	var class, dflt *Response
	for _, r := range op.Responses {
		switch {
		case r.Status == "default":
			dflt = r
		case r.Code == status && !strings.HasSuffix(strings.ToUpper(r.Status), "XX"):
			return op.render(r, status), true
		case strings.HasSuffix(strings.ToUpper(r.Status), "XX") && r.Code/100 == status/100:
			class = r
		}
	}
	if class != nil {
		return op.render(class, status), true
	}
	if dflt != nil {
		return op.render(dflt, status), true
	}
	return Answer{}, false
}

func (op *Operation) render(r *Response, status int) Answer {
	a := Answer{Status: status}
	if status == 204 || status == 205 || status == 304 || status/100 == 1 {
		return a
	}
	media := pickMedia(r.Media)
	if media == nil {
		return a
	}
	var v any
	switch {
	case media.HasExample:
		v = media.Example
	case media.Node != nil:
		v = op.doc.Generate(media.Node)
	default:
		return a
	}
	a.ContentType = media.Type
	if media.Type == "*/*" {
		a.ContentType = "application/json"
	}
	if s, ok := v.(string); ok && !mediaIsJSON(media.Type) {
		a.Body = []byte(s)
		return a
	}
	a.Body = encodeJSON(v)
	return a
}

func encodeJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
