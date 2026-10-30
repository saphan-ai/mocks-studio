// Package oas loads an OpenAPI 3.0 or 3.1 document, resolves its local references and answers the
// questions a mock asks of it: which operation a request is for, whether the request keeps the
// document's rules, and what the operation answers.
package oas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// maxTreeNodes bounds the nodes one document may expand to, so an alias bomb is refused rather than expanded.
const maxTreeNodes = 2_000_000

// maxRefDepth bounds a chain of references that point at references.
const maxRefDepth = 32

// parseTree reads JSON or YAML into a tree of map[string]any, []any, string, float64, bool and nil.
func parseTree(data []byte) (any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, errors.New("the document is empty")
	}
	if trimmed[0] == '{' {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("the document is not valid JSON: %w", err)
		}
		return v, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("the document is not valid YAML: %w", err)
	}
	budget := maxTreeNodes
	return fromNode(&doc, &budget)
}

func fromNode(n *yaml.Node, budget *int) (any, error) {
	*budget--
	if *budget < 0 {
		return nil, errors.New("the document expands to more nodes than the loader accepts (an alias bomb?)")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, errors.New("the document is empty")
		}
		return fromNode(n.Content[0], budget)
	case yaml.AliasNode:
		return fromNode(n.Alias, budget)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a mapping key must be a scalar", k.Line)
			}
			v, err := fromNode(n.Content[i+1], budget)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := fromNode(c, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	default:
		return scalar(n)
	}
}

func scalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return b, nil
	case "!!int", "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return f, nil
	default: // strings, timestamps and binary stay the text the author wrote
		return n.Value, nil
	}
}

// tokens is a JSON pointer as its unescaped reference tokens.
type tokens []string

func (t tokens) with(more ...string) tokens {
	out := make(tokens, 0, len(t)+len(more))
	out = append(out, t...)
	return append(out, more...)
}

// String renders the pointer in its RFC 6901 form.
func (t tokens) String() string {
	var b strings.Builder
	for _, tok := range t {
		b.WriteByte('/')
		b.WriteString(escapeToken(tok))
	}
	return b.String()
}

func escapeToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// fragment renders the pointer as a URI fragment the schema compiler accepts.
func (t tokens) fragment() string {
	var b strings.Builder
	for _, tok := range t {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(escapeToken(tok)))
	}
	return b.String()
}

// refTokens parses a local reference ("#/components/schemas/Order") into tokens.
func refTokens(ref string) (tokens, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, errors.New("only references into this document (starting with #) are supported")
	}
	frag, err := url.PathUnescape(ref[1:])
	if err != nil {
		return nil, fmt.Errorf("the reference is not a valid URI fragment: %w", err)
	}
	if frag == "" {
		return tokens{}, nil
	}
	if !strings.HasPrefix(frag, "/") {
		return nil, errors.New("the reference must be a JSON pointer (#/…)")
	}
	parts := strings.Split(frag[1:], "/")
	out := make(tokens, len(parts))
	for i, p := range parts {
		out[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return out, nil
}

// at follows the pointer from the root.
func at(root any, t tokens) (any, bool) {
	cur := root
	for _, tok := range t {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// deref follows a chain of $ref objects and returns the final node and where it lives.
func deref(root, node any, where tokens) (any, tokens, error) {
	for range maxRefDepth {
		m, ok := node.(map[string]any)
		if !ok {
			return node, where, nil
		}
		ref, isRef := m["$ref"].(string)
		if !isRef {
			return node, where, nil
		}
		t, err := refTokens(ref)
		if err != nil {
			return nil, nil, fmt.Errorf("reference %q: %w", ref, err)
		}
		target, found := at(root, t)
		if !found {
			return nil, nil, fmt.Errorf("reference %q does not resolve to anything in the document", ref)
		}
		node, where = target, t
	}
	return nil, nil, errors.New("a chain of references does not end (a reference cycle)")
}

// dataKeys hold example data, not document structure: a $ref inside one is text, not a reference.
var dataKeys = map[string]bool{"example": true, "enum": true, "const": true, "default": true}

// checkRefs verifies that every $ref of the document is local and resolves.
func checkRefs(root, n any, where tokens) error {
	switch node := n.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok {
			if _, _, err := deref(root, node, where); err != nil {
				return &refError{Ref: ref, Where: where.String(), Err: err}
			}
		}
		for _, k := range sortedKeys(node) {
			v := node[k]
			if dataKeys[k] {
				continue
			}
			if k == "examples" {
				if em, isMap := v.(map[string]any); isMap {
					for _, name := range sortedKeys(em) {
						ex := em[name] // Example Objects may be references; their values are data
						if ref := refOf(ex); ref != "" {
							if _, _, err := deref(root, ex, where.with(k, name)); err != nil {
								return &refError{Ref: ref, Where: where.with(k, name).String(), Err: err}
							}
						}
					}
				}
				continue
			}
			if err := checkRefs(root, v, where.with(k)); err != nil {
				return err
			}
		}
	case []any:
		for i, v := range node {
			if err := checkRefs(root, v, where.with(strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func refOf(n any) string {
	if m, ok := n.(map[string]any); ok {
		if s, ok := m["$ref"].(string); ok {
			return s
		}
	}
	return ""
}

// refError is a reference that cannot be followed.
type refError struct {
	Ref   string
	Where string
	Err   error
}

func (e *refError) Error() string {
	return fmt.Sprintf("the reference %q at %s cannot be followed: %v", e.Ref, e.Where, e.Err)
}

func (e *refError) Unwrap() error { return e.Err }
