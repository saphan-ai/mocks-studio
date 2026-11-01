package kubemock

import (
	"fmt"
	"regexp"
	"strings"
)

type selectorOp int

const (
	opEquals selectorOp = iota
	opNotEquals
	opIn
	opNotIn
	opExists
	opAbsent
)

type requirement struct {
	key    string
	op     selectorOp
	values []string
}

// selector is a parsed label selector or field selector; the zero value matches everything.
type selector []requirement

var (
	keyPattern    = `[A-Za-z0-9][-A-Za-z0-9_./]*`
	setReqPattern = regexp.MustCompile(`^(` + keyPattern + `)\s+(in|notin)\s*\(([^)]*)\)$`)
	eqReqPattern  = regexp.MustCompile(`^(` + keyPattern + `)\s*(==|=|!=)\s*(.*)$`)
	keyOnly       = regexp.MustCompile(`^!?` + keyPattern + `$`)
)

// parseSelector parses the labelSelector grammar: comma separated requirements
// `k=v`, `k==v`, `k!=v`, `k`, `!k`, `k in (a,b)` and `k notin (a,b)`.
func parseSelector(s string) (selector, error) {
	var out selector
	for _, part := range splitTop(s) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		req, err := parseRequirement(part)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, nil
}

func parseRequirement(part string) (requirement, error) {
	if m := setReqPattern.FindStringSubmatch(part); m != nil {
		op := opIn
		if m[2] == "notin" {
			op = opNotIn
		}
		var vals []string
		for _, v := range strings.Split(m[3], ",") {
			vals = append(vals, strings.TrimSpace(v))
		}
		return requirement{key: m[1], op: op, values: vals}, nil
	}
	if m := eqReqPattern.FindStringSubmatch(part); m != nil {
		op := opEquals
		if m[2] == "!=" {
			op = opNotEquals
		}
		return requirement{key: m[1], op: op, values: []string{strings.TrimSpace(m[3])}}, nil
	}
	if keyOnly.MatchString(part) {
		if strings.HasPrefix(part, "!") {
			return requirement{key: part[1:], op: opAbsent}, nil
		}
		return requirement{key: part, op: opExists}, nil
	}
	return requirement{}, fmt.Errorf("unable to parse requirement %q", part)
}

// splitTop splits on commas that are not inside parentheses.
func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// matches reports whether the labels satisfy every requirement.
func (sel selector) matches(labels map[string]string) bool {
	for _, r := range sel {
		v, has := labels[r.key]
		switch r.op {
		case opEquals:
			if !has || v != r.values[0] {
				return false
			}
		case opNotEquals:
			if has && v == r.values[0] {
				return false
			}
		case opIn:
			if !has || !contains(r.values, v) {
				return false
			}
		case opNotIn:
			if has && contains(r.values, v) {
				return false
			}
		case opExists:
			if !has {
				return false
			}
		case opAbsent:
			if has {
				return false
			}
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// parseFieldSelector parses a fieldSelector. Only metadata.name and
// metadata.namespace with = == != are supported, and anything else is refused
// rather than silently ignored.
func parseFieldSelector(s string) (selector, error) {
	sel, err := parseSelector(s)
	if err != nil {
		return nil, err
	}
	for _, r := range sel {
		if (r.key != "metadata.name" && r.key != "metadata.namespace") || (r.op != opEquals && r.op != opNotEquals) {
			return nil, fmt.Errorf("field selector %q is not supported; use metadata.name or metadata.namespace with =, == or !=", r.key)
		}
	}
	return sel, nil
}
