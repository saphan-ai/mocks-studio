package oas

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// Rule names a violation reports. They are the JSON Schema keyword that failed, or one of these.
const (
	RuleRequired    = "required"
	RuleType        = "type"
	RuleJSON        = "json"
	RuleContentType = "content-type"
	RuleSchema      = "schema"
)

// Violation is one place where a request breaks the document. It names where and which rule, never the value.
type Violation struct {
	Pointer string // a JSON pointer: /path/id, /query/limit, /header/X-Id, /cookie/s, /body/items/0/qty
	Rule    string
}

// Request is the part of an HTTP request the document has rules about.
type Request struct {
	PathParams  map[string]string
	Query       url.Values
	Header      http.Header
	ContentType string
	Body        []byte
}

// ignoredHeaders are header names the OpenAPI specification says a parameter definition must not govern.
var ignoredHeaders = map[string]bool{"accept": true, "content-type": true, "authorization": true}

// Validate checks the request against the operation's parameters and body. The result is ordered:
// path, query, header, cookie, then body, each in declaration order.
func (op *Operation) Validate(r Request) []Violation {
	var out []Violation
	for _, in := range []string{"path", "query", "header", "cookie"} {
		for _, p := range op.Params {
			if p.In == in {
				out = append(out, op.validateParam(p, r)...)
			}
		}
	}
	return append(out, op.validateBody(r)...)
}

func (op *Operation) validateParam(p *Param, r Request) []Violation {
	if p.In == "header" && ignoredHeaders[strings.ToLower(p.Name)] {
		return nil
	}
	where := "/" + p.In + "/" + escapeToken(p.Name)
	raw, present := paramValues(p, r)
	if !present {
		if p.Required {
			return []Violation{{where, RuleRequired}}
		}
		return nil
	}
	if p.Schema == nil || p.Node == nil {
		return nil
	}
	eff := op.doc.effective(p.Node)
	if schemaType(eff) == "object" {
		return nil // object styles (form, deepObject) are not validated
	}
	v, ok := op.doc.coerce(eff, raw, p)
	if !ok {
		return []Violation{{where, RuleType}}
	}
	return schemaViolations(p.Schema.Validate(v), where)
}

func paramValues(p *Param, r Request) ([]string, bool) {
	switch p.In {
	case "path":
		v, ok := r.PathParams[p.Name]
		return []string{v}, ok
	case "query":
		v, ok := r.Query[p.Name]
		return v, ok && len(v) > 0
	case "header":
		v := r.Header.Values(p.Name)
		return v, len(v) > 0
	default:
		for _, c := range (&http.Request{Header: r.Header}).Cookies() {
			if c.Name == p.Name {
				return []string{c.Value}, true
			}
		}
	}
	return nil, false
}

// effective follows references from a schema to the schema that carries its type.
func (d *Document) effective(node map[string]any) map[string]any {
	cur := node
	for range maxRefDepth {
		ref, ok := cur["$ref"].(string)
		if !ok {
			return cur
		}
		t, err := refTokens(ref)
		if err != nil {
			return cur
		}
		next, found := at(d.root, t)
		m, isMap := next.(map[string]any)
		if !found || !isMap {
			return cur
		}
		cur = m
	}
	return cur
}

var delimiters = map[string]string{"pipeDelimited": "|", "spaceDelimited": " ", "simple": ",", "form": ","}

// coerce turns the strings a parameter arrives as into the JSON value its schema describes.
func (d *Document) coerce(eff map[string]any, raw []string, p *Param) (any, bool) {
	if schemaType(eff) == "array" {
		items := raw
		if !p.Explode || p.In != "query" {
			sep := delimiters[p.Style]
			if sep == "" {
				sep = ","
			}
			items = strings.Split(raw[0], sep)
		}
		var itemSchema map[string]any
		if it, ok := eff["items"].(map[string]any); ok {
			itemSchema = d.effective(it)
		}
		out := make([]any, 0, len(items))
		for _, s := range items {
			v, ok := scalarOf(schemaType(itemSchema), s)
			if !ok {
				return nil, false
			}
			out = append(out, v)
		}
		return out, true
	}
	return scalarOf(schemaType(eff), raw[0])
}

func scalarOf(typ, s string) (any, bool) {
	switch typ {
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, false
		}
		return json.Number(strconv.FormatInt(n, 10)), true
	case "number":
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || strings.ContainsAny(s, "nNiI") { // NaN and Inf parse, and are not JSON numbers
			return nil, false
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), true
	case "boolean":
		switch s {
		case "true":
			return true, true
		case "false":
			return false, true
		}
		return nil, false
	}
	return s, true
}

func (op *Operation) validateBody(r Request) []Violation {
	if op.Body == nil {
		return nil
	}
	if len(bytes.TrimSpace(r.Body)) == 0 {
		if op.Body.Required {
			return []Violation{{"/body", RuleRequired}}
		}
		return nil
	}
	media := op.Body.pick(r.ContentType)
	if media == nil {
		return []Violation{{"/header/Content-Type", RuleContentType}}
	}
	if media.Schema == nil {
		return nil
	}
	switch {
	case mediaIsJSON(media.Type):
		dec := json.NewDecoder(bytes.NewReader(r.Body))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil || dec.More() {
			return []Violation{{"/body", RuleJSON}}
		}
		return schemaViolations(media.Schema.Validate(v), "/body")
	case strings.HasPrefix(strings.ToLower(media.Type), "application/x-www-form-urlencoded"):
		form, err := url.ParseQuery(string(r.Body))
		if err != nil {
			return []Violation{{"/body", RuleType}}
		}
		return op.validateForm(media, form)
	}
	return nil
}

func (op *Operation) validateForm(media *Media, form url.Values) []Violation {
	node, _ := media.Node.(map[string]any)
	eff := op.doc.effective(node)
	props, _ := eff["properties"].(map[string]any)
	obj := map[string]any{}
	var out []Violation
	for _, k := range sortedFormKeys(form) {
		var typ string
		if pn, ok := props[k].(map[string]any); ok {
			typ = schemaType(op.doc.effective(pn))
		}
		v, ok := scalarOf(typ, form.Get(k))
		if !ok {
			out = append(out, Violation{"/body/" + escapeToken(k), RuleType})
			continue
		}
		obj[k] = v
	}
	return append(out, schemaViolations(media.Schema.Validate(obj), "/body")...)
}

func sortedFormKeys(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pick finds the request body media type for a Content-Type header value.
func (b *Body) pick(contentType string) *Media {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if base == "" {
		return nil
	}
	var wild *Media
	for _, m := range b.Media {
		declared := strings.ToLower(strings.TrimSpace(strings.SplitN(m.Type, ";", 2)[0]))
		switch {
		case declared == base:
			return m
		case declared == "*/*" || (strings.HasSuffix(declared, "/*") && strings.HasPrefix(base, declared[:len(declared)-1])):
			if wild == nil {
				wild = m
			}
		}
	}
	return wild
}

// quoted matches a property name as the schema library quotes it in its messages: 'name', with \' for a quote.
var quoted = regexp.MustCompile(`'((?:[^'\\]|\\.)*)'`)

// schemaViolations flattens a validation error into the places and rules that failed, ordered and de-duplicated.
func schemaViolations(err error, prefix string) []Violation {
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Violation{{prefix, RuleSchema}}
	}
	seen := map[Violation]bool{}
	var out []Violation
	var walk func(*jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) > 0 {
			for _, c := range e.Causes {
				walk(c)
			}
			return
		}
		rule := lastToken(e.KeywordLocation)
		for _, v := range leafViolations(e, prefix, rule) {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	walk(ve)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pointer != out[j].Pointer {
			return out[i].Pointer < out[j].Pointer
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}

func leafViolations(e *jsonschema.ValidationError, prefix, rule string) []Violation {
	base := prefix + e.InstanceLocation
	if rule != "required" && rule != "additionalProperties" {
		return []Violation{{base, rule}}
	}
	var out []Violation
	for _, m := range quoted.FindAllStringSubmatch(e.Message, -1) {
		name := strings.ReplaceAll(m[1], `\'`, `'`)
		out = append(out, Violation{base + "/" + escapeToken(name), rule})
	}
	if len(out) == 0 {
		out = append(out, Violation{base, rule})
	}
	return out
}

func lastToken(keywordLocation string) string {
	i := strings.LastIndex(keywordLocation, "/")
	if i < 0 || i == len(keywordLocation)-1 {
		return RuleSchema
	}
	return keywordLocation[i+1:]
}
