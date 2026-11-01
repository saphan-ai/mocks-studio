package scimmock

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A filter is the RFC 7644 §3.4.2.2 expression: attribute comparisons (eq ne co sw ew gt ge lt le),
// pr, and/or/not, parentheses and the valuePath form attr[expr]. It is evaluated against a resource
// as JSON (map[string]any).

type filterNode interface {
	eval(res any) bool
}

type orNode struct{ l, r filterNode }
type andNode struct{ l, r filterNode }
type notNode struct{ n filterNode }

type cmpNode struct {
	path  []string
	op    string // eq ne co sw ew gt ge lt le pr
	value any    // string, float64, bool or nil (JSON null)
}

type pathNode struct { // attr[expr]
	path []string
	expr filterNode
}

func (n orNode) eval(res any) bool  { return n.l.eval(res) || n.r.eval(res) }
func (n andNode) eval(res any) bool { return n.l.eval(res) && n.r.eval(res) }
func (n notNode) eval(res any) bool { return !n.n.eval(res) }

func (n pathNode) eval(res any) bool {
	for _, v := range lookup(res, n.path) {
		if n.expr.eval(v) {
			return true
		}
	}
	return false
}

// lookup follows a dotted attribute path through objects and arrays (names are case-insensitive)
// and returns every value reached; a multi-valued attribute contributes one value per element.
func lookup(res any, path []string) []any {
	cur := []any{res}
	for _, seg := range path {
		var next []any
		for _, c := range cur {
			switch t := c.(type) {
			case map[string]any:
				for k, v := range t {
					if strings.EqualFold(k, seg) {
						next = append(next, v)
					}
				}
			}
		}
		cur = flatten(next)
	}
	return cur
}

func flatten(vs []any) []any {
	var out []any
	for _, v := range vs {
		if arr, ok := v.([]any); ok {
			out = append(out, flatten(arr)...)
		} else {
			out = append(out, v)
		}
	}
	return out
}

func (n cmpNode) eval(res any) bool {
	vals := lookup(res, n.path)
	switch n.op {
	case "pr":
		for _, v := range vals {
			if present(v) {
				return true
			}
		}
		return false
	case "ne":
		return !cmpNode{path: n.path, op: "eq", value: n.value}.eval(res)
	case "eq":
		if n.value == nil {
			for _, v := range vals {
				if present(v) {
					return false
				}
			}
			return true
		}
	}
	exact := caseExact(n.path)
	for _, v := range vals {
		if compare(n.op, v, n.value, exact) {
			return true
		}
	}
	return false
}

func caseExact(path []string) bool {
	last := strings.ToLower(path[len(path)-1])
	return len(path) == 1 && (last == "id" || last == "externalid")
}

func present(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func compare(op string, have, want any, exact bool) bool {
	switch w := want.(type) {
	case string:
		h, ok := have.(string)
		if !ok {
			return false
		}
		if !exact {
			h, w = strings.ToLower(h), strings.ToLower(w)
		}
		switch op {
		case "eq":
			return h == w
		case "co":
			return strings.Contains(h, w)
		case "sw":
			return strings.HasPrefix(h, w)
		case "ew":
			return strings.HasSuffix(h, w)
		case "gt":
			return h > w
		case "ge":
			return h >= w
		case "lt":
			return h < w
		case "le":
			return h <= w
		}
	case float64:
		h, ok := have.(float64)
		if !ok {
			return false
		}
		switch op {
		case "eq":
			return h == w
		case "gt":
			return h > w
		case "ge":
			return h >= w
		case "lt":
			return h < w
		case "le":
			return h <= w
		}
	case bool:
		h, ok := have.(bool)
		return ok && op == "eq" && h == w
	}
	return false
}

// parseFilter parses a filter expression; the error names what was wrong.
func parseFilter(src string) (filterNode, error) {
	toks, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &fparser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("unexpected %q after the end of the expression", p.toks[p.pos].text)
	}
	return n, nil
}

type token struct {
	kind string // word, string, "(", ")", "[", "]"
	text string
	val  string // decoded string value
}

func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := rune(s[i])
		switch {
		case unicode.IsSpace(c):
			i++
		case c == '(' || c == ')' || c == '[' || c == ']':
			toks = append(toks, token{kind: string(c), text: string(c)})
			i++
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("the string starting at position %d is not closed", i)
			}
			var v string
			if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
				return nil, fmt.Errorf("the string at position %d is not a valid JSON string", i)
			}
			toks = append(toks, token{kind: "string", text: s[i : j+1], val: v})
			i = j + 1
		default:
			j := i
			for j < len(s) && !unicode.IsSpace(rune(s[j])) && !strings.ContainsRune("()[]\"", rune(s[j])) {
				j++
			}
			toks = append(toks, token{kind: "word", text: s[i:j]})
			i = j
		}
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("the expression is empty")
	}
	return toks, nil
}

type fparser struct {
	toks []token
	pos  int
}

func (p *fparser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *fparser) isWord(w string) bool {
	t := p.peek()
	return t != nil && t.kind == "word" && strings.EqualFold(t.text, w)
}

func (p *fparser) parseOr() (filterNode, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.isWord("or") {
		p.pos++
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = orNode{l, r}
	}
	return l, nil
}

func (p *fparser) parseAnd() (filterNode, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.isWord("and") {
		p.pos++
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = andNode{l, r}
	}
	return l, nil
}

func (p *fparser) parseUnary() (filterNode, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("the expression ends where a condition was expected")
	}
	if p.isWord("not") {
		p.pos++
		if nt := p.peek(); nt == nil || nt.kind != "(" {
			return nil, fmt.Errorf("\"not\" must be followed by a parenthesised expression")
		}
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notNode{n}, nil
	}
	if t.kind == "(" {
		p.pos++
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c == nil || c.kind != ")" {
			return nil, fmt.Errorf("a parenthesis is not closed")
		}
		p.pos++
		return n, nil
	}
	if t.kind != "word" {
		return nil, fmt.Errorf("expected an attribute name, found %q", t.text)
	}
	path, err := attrPath(t.text)
	if err != nil {
		return nil, err
	}
	p.pos++
	if nt := p.peek(); nt != nil && nt.kind == "[" {
		p.pos++
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if c := p.peek(); c == nil || c.kind != "]" {
			return nil, fmt.Errorf("a bracket is not closed")
		}
		p.pos++
		return pathNode{path: path, expr: inner}, nil
	}
	opTok := p.peek()
	if opTok == nil || opTok.kind != "word" {
		return nil, fmt.Errorf("expected an operator after %q", t.text)
	}
	op := strings.ToLower(opTok.text)
	p.pos++
	switch op {
	case "pr":
		return cmpNode{path: path, op: op}, nil
	case "eq", "ne", "co", "sw", "ew", "gt", "ge", "lt", "le":
	default:
		return nil, fmt.Errorf("the operator %q is not one of eq, ne, co, sw, ew, gt, ge, lt, le, pr", opTok.text)
	}
	vt := p.peek()
	if vt == nil {
		return nil, fmt.Errorf("the operator %q has no value", op)
	}
	p.pos++
	var val any
	switch {
	case vt.kind == "string":
		val = vt.val
	case vt.kind == "word" && vt.text == "true":
		val = true
	case vt.kind == "word" && vt.text == "false":
		val = false
	case vt.kind == "word" && vt.text == "null":
		val = nil
	case vt.kind == "word":
		f, err := strconv.ParseFloat(vt.text, 64)
		if err != nil {
			return nil, fmt.Errorf("the value %q is not a string, number, true, false or null", vt.text)
		}
		val = f
	default:
		return nil, fmt.Errorf("expected a value after %q, found %q", op, vt.text)
	}
	if val == nil && op != "eq" && op != "ne" {
		return nil, fmt.Errorf("null can be compared only with eq or ne")
	}
	if _, isStr := val.(string); !isStr && (op == "co" || op == "sw" || op == "ew") {
		return nil, fmt.Errorf("the operator %q needs a string value", op)
	}
	return cmpNode{path: path, op: op, value: val}, nil
}

const (
	userURN  = "urn:ietf:params:scim:schemas:core:2.0:User"
	groupURN = "urn:ietf:params:scim:schemas:core:2.0:Group"
)

// attrPath splits "name.givenName" (optionally prefixed by a core schema URN) into segments.
func attrPath(s string) ([]string, error) {
	for _, urn := range []string{userURN + ":", groupURN + ":"} {
		if len(s) > len(urn) && strings.EqualFold(s[:len(urn)], urn) {
			s = s[len(urn):]
		}
	}
	segs := strings.Split(s, ".")
	for _, seg := range segs {
		if seg == "" || !validAttrName(seg) {
			return nil, fmt.Errorf("%q is not an attribute name", s)
		}
	}
	return segs, nil
}

func validAttrName(s string) bool {
	for i, c := range s {
		if !(unicode.IsLetter(c) || c == '$' || c == '_' || i > 0 && (unicode.IsDigit(c) || c == '-')) {
			return false
		}
	}
	return s != ""
}
