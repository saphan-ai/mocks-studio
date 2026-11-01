package oas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// MaxDocumentBytes bounds the size of a document the loader reads.
const MaxDocumentBytes = 8 << 20

const resourceName = "openapi.json"

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

var versionPattern = regexp.MustCompile(`^3\.[01]\.[0-9]+(-.+)?$`)

// Document is a loaded OpenAPI 3.x document, ready to route requests.
type Document struct {
	OpenAPI  string
	Title    string
	Version  string
	BasePath string // the path of the first server, without a trailing slash; "" when it has none
	Routes   []*Route
	Ops      []*Operation // every operation, ordered by path template then method

	root any
}

// Route is one path template with the operations declared on it.
type Route struct {
	Template string
	Methods  map[string]*Operation // keyed by upper-case method

	re    *regexp.Regexp
	names []string
}

// Operation is one method on one path.
type Operation struct {
	ID       string // the document's operationId, or one derived from method and path
	Method   string // upper case
	Template string
	Params   []*Param
	Body     *Body

	Responses []*Response
	doc       *Document

	success       sync.Once
	successAnswer Answer
}

// Param is a path, query, header or cookie parameter.
type Param struct {
	Name     string
	In       string
	Required bool
	Style    string
	Explode  bool
	Schema   *jsonschema.Schema // nil when the parameter declares none
	Node     map[string]any     // the schema as written, for coercion and generation
}

// Body is a request body.
type Body struct {
	Required bool
	Media    []*Media
}

// Media is one media type of a request body or a response.
type Media struct {
	Type       string
	Schema     *jsonschema.Schema // request bodies only
	Node       any                // the schema as written
	Example    any
	HasExample bool
}

// Response is one declared response.
type Response struct {
	Status string // "200", "2XX", "default"
	Code   int    // the numeric status it answers with; 0 only before resolution
	Media  []*Media
}

// LoadError is a document refused at load, with what failed and where.
type LoadError struct {
	Path string
	Msg  string
}

func (e *LoadError) Error() string { return fmt.Sprintf("%s: %s", e.Path, e.Msg) }

// Load reads, checks and indexes the document at path.
func Load(path string) (*Document, error) {
	fail := func(format string, a ...any) error { return &LoadError{Path: path, Msg: fmt.Sprintf(format, a...)} }
	f, err := os.Open(path)
	if err != nil {
		return nil, fail("the document cannot be read: %v", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxDocumentBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fail("the document cannot be read: %v", err)
	}
	if len(data) > MaxDocumentBytes {
		return nil, fail("the document is larger than %d bytes", MaxDocumentBytes)
	}
	root, err := parseTree(data)
	if err != nil {
		return nil, fail("%v", err)
	}
	d, err := build(root)
	if err != nil {
		return nil, fail("%v", err)
	}
	return d, nil
}

func build(root any) (*Document, error) {
	top, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("the document is not an object at its top level")
	}
	version, _ := top["openapi"].(string)
	if !versionPattern.MatchString(version) {
		return nil, fmt.Errorf("the field \"openapi\" is %q, want a 3.0.x or 3.1.x version", version)
	}
	if err := checkRefs(root, root, nil); err != nil {
		return nil, err
	}
	if strings.HasPrefix(version, "3.0") {
		eachSchema(root, toDraft2020)
	}
	d := &Document{OpenAPI: version, root: root}
	if info, ok := top["info"].(map[string]any); ok {
		d.Title, _ = info["title"].(string)
		d.Version, _ = info["version"].(string)
	}
	d.BasePath = basePath(top["servers"])

	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("the document cannot be re-encoded: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	c.AssertFormat = true
	if err := c.AddResource(resourceName, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("the document cannot be indexed: %w", err)
	}
	if err := d.buildRoutes(top, c); err != nil {
		return nil, err
	}
	if len(d.Ops) == 0 {
		return nil, errors.New("the document declares no operation under \"paths\"")
	}
	return d, nil
}

var serverVar = regexp.MustCompile(`\{([^}]*)\}`)

func basePath(servers any) string {
	list, ok := servers.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		return ""
	}
	raw, _ := first["url"].(string)
	vars, _ := first["variables"].(map[string]any)
	raw = serverVar.ReplaceAllStringFunc(raw, func(m string) string {
		if v, ok := vars[m[1:len(m)-1]].(map[string]any); ok {
			if def, ok := v["default"].(string); ok {
				return def
			}
		}
		return m
	})
	p := raw
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		p = u.Path
	}
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	return strings.TrimRight(p, "/")
}

func (d *Document) buildRoutes(top map[string]any, c *jsonschema.Compiler) error {
	paths, _ := top["paths"].(map[string]any)
	ids := map[string]string{}
	for _, tmpl := range sortedKeys(paths) {
		if !strings.HasPrefix(tmpl, "/") {
			return fmt.Errorf("the path %q at /paths must start with /", tmpl)
		}
		where := tokens{"paths", tmpl}
		item, itemWhere, err := deref(d.root, paths[tmpl], where)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		pi, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: a path item must be an object", where)
		}
		route, err := newRoute(d.BasePath + tmpl)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		route.Template = tmpl
		for _, m := range httpMethods {
			opNode, ok := pi[m].(map[string]any)
			if !ok {
				continue
			}
			op, err := d.buildOperation(c, strings.ToUpper(m), tmpl, pi, itemWhere, opNode)
			if err != nil {
				return err
			}
			if prev, dup := ids[op.ID]; dup {
				return fmt.Errorf("the operationId %q is declared twice: by %s and by %s %s", op.ID, prev, op.Method, tmpl)
			}
			ids[op.ID] = op.Method + " " + tmpl
			route.Methods[op.Method] = op
			d.Ops = append(d.Ops, op)
		}
		if len(route.Methods) > 0 {
			d.Routes = append(d.Routes, route)
		}
	}
	sort.SliceStable(d.Routes, func(i, j int) bool {
		a, b := d.Routes[i], d.Routes[j]
		if len(a.names) != len(b.names) {
			return len(a.names) < len(b.names)
		}
		return len(a.Template) > len(b.Template)
	})
	return nil
}

var templateVar = regexp.MustCompile(`\{([^/{}]+)\}`)

func newRoute(full string) (*Route, error) {
	r := &Route{Methods: map[string]*Operation{}}
	var re strings.Builder
	re.WriteString("^")
	last := 0
	for _, m := range templateVar.FindAllStringSubmatchIndex(full, -1) {
		re.WriteString(regexp.QuoteMeta(full[last:m[0]]))
		re.WriteString("([^/]+)")
		r.names = append(r.names, full[m[2]:m[3]])
		last = m[1]
	}
	re.WriteString(regexp.QuoteMeta(full[last:]))
	re.WriteString("$")
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil, fmt.Errorf("the path template cannot be matched: %w", err)
	}
	r.re = compiled
	return r, nil
}

// Match finds the route for a request path and returns its path parameters.
func (d *Document) Match(path string) (*Route, map[string]string, bool) {
	for _, r := range d.Routes {
		m := r.re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		params := make(map[string]string, len(r.names))
		for i, n := range r.names {
			v, err := url.PathUnescape(m[i+1])
			if err != nil {
				v = m[i+1]
			}
			params[n] = v
		}
		return r, params, true
	}
	return nil, nil, false
}

// Allowed lists the methods a route declares, sorted.
func (r *Route) Allowed() []string {
	return sortedKeys(r.Methods)
}

var nonIDChars = regexp.MustCompile(`[^A-Za-z0-9]+`)

func derivedID(method, tmpl string) string {
	return strings.ToLower(method) + "_" + strings.Trim(nonIDChars.ReplaceAllString(tmpl, "_"), "_")
}

func (d *Document) buildOperation(c *jsonschema.Compiler, method, tmpl string, pathItem map[string]any, itemWhere tokens, opNode map[string]any) (*Operation, error) {
	opWhere := itemWhere.with(strings.ToLower(method))
	op := &Operation{Method: method, Template: tmpl, doc: d}
	op.ID, _ = opNode["operationId"].(string)
	if op.ID == "" {
		op.ID = derivedID(method, tmpl)
	}
	params, err := d.collectParams(c, pathItem, itemWhere, opNode, opWhere)
	if err != nil {
		return nil, err
	}
	op.Params = params
	if rb, ok := opNode["requestBody"]; ok {
		body, err := d.buildBody(c, rb, opWhere.with("requestBody"))
		if err != nil {
			return nil, err
		}
		op.Body = body
	}
	if rs, ok := opNode["responses"].(map[string]any); ok {
		for _, status := range sortedKeys(rs) {
			resp, err := d.buildResponse(status, rs[status], opWhere.with("responses", status))
			if err != nil {
				return nil, err
			}
			op.Responses = append(op.Responses, resp)
		}
	}
	return op, nil
}

func (d *Document) compile(c *jsonschema.Compiler, where tokens) (*jsonschema.Schema, error) {
	s, err := c.Compile(resourceName + "#" + where.fragment())
	if err != nil {
		return nil, fmt.Errorf("the schema at %s is invalid: %w", where, err)
	}
	return s, nil
}

func (d *Document) collectParams(c *jsonschema.Compiler, pathItem map[string]any, itemWhere tokens, opNode map[string]any, opWhere tokens) ([]*Param, error) {
	var out []*Param
	index := map[string]int{}
	add := func(list any, where tokens) error {
		items, _ := list.([]any)
		for i, raw := range items {
			at := where.with("parameters", strconv.Itoa(i))
			node, loc, err := deref(d.root, raw, at)
			if err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			p, err := d.buildParam(c, node, loc)
			if err != nil {
				return err
			}
			key := p.In + ":" + p.Name
			if at, dup := index[key]; dup { // an operation's parameter overrides the path item's
				out[at] = p
				continue
			}
			index[key] = len(out)
			out = append(out, p)
		}
		return nil
	}
	if err := add(pathItem["parameters"], itemWhere); err != nil {
		return nil, err
	}
	if err := add(opNode["parameters"], opWhere); err != nil {
		return nil, err
	}
	return out, nil
}

func (d *Document) buildParam(c *jsonschema.Compiler, node any, loc tokens) (*Param, error) {
	m, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a parameter must be an object", loc)
	}
	p := &Param{}
	p.Name, _ = m["name"].(string)
	p.In, _ = m["in"].(string)
	switch p.In {
	case "path", "query", "header", "cookie":
	default:
		return nil, fmt.Errorf("%s: the parameter %q is declared \"in\": %q, want path, query, header or cookie", loc, p.Name, p.In)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("%s: a parameter has no name", loc)
	}
	p.Required, _ = m["required"].(bool)
	if p.In == "path" {
		p.Required = true
	}
	p.Style, _ = m["style"].(string)
	p.Explode = p.In == "query" || p.In == "cookie" // style form is the default for both, and explodes
	if p.Style == "simple" || p.Style == "label" || p.Style == "matrix" {
		p.Explode = false
	}
	if e, ok := m["explode"].(bool); ok {
		p.Explode = e
	}
	schemaAt := loc.with("schema")
	if sn, ok := m["schema"]; ok {
		p.Node, _ = sn.(map[string]any)
	} else if content, ok := m["content"].(map[string]any); ok {
		for _, mt := range sortedKeys(content) {
			if mm, ok := content[mt].(map[string]any); ok {
				if sn, ok := mm["schema"]; ok {
					p.Node, _ = sn.(map[string]any)
					schemaAt = loc.with("content", mt, "schema")
					break
				}
			}
		}
	}
	if p.Node != nil {
		s, err := d.compile(c, schemaAt)
		if err != nil {
			return nil, err
		}
		p.Schema = s
	}
	return p, nil
}

func (d *Document) buildBody(c *jsonschema.Compiler, raw any, at tokens) (*Body, error) {
	node, loc, err := deref(d.root, raw, at)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", at, err)
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a request body must be an object", loc)
	}
	b := &Body{}
	b.Required, _ = m["required"].(bool)
	content, _ := m["content"].(map[string]any)
	for _, mt := range sortedKeys(content) {
		media, err := d.buildMedia(c, mt, content[mt], loc.with("content", mt), true)
		if err != nil {
			return nil, err
		}
		b.Media = append(b.Media, media)
	}
	return b, nil
}

func (d *Document) buildMedia(c *jsonschema.Compiler, mt string, raw any, loc tokens, compile bool) (*Media, error) {
	media := &Media{Type: mt}
	m, ok := raw.(map[string]any)
	if !ok {
		return media, nil
	}
	if sn, has := m["schema"]; has {
		media.Node = sn
		if compile {
			s, err := d.compile(c, loc.with("schema"))
			if err != nil {
				return nil, err
			}
			media.Schema = s
		}
	}
	if ex, has := m["example"]; has {
		media.Example, media.HasExample = ex, true
	} else if exs, ok := m["examples"].(map[string]any); ok {
		for _, name := range sortedKeys(exs) {
			node, _, err := deref(d.root, exs[name], loc.with("examples", name))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", loc.with("examples", name), err)
			}
			if em, ok := node.(map[string]any); ok {
				if v, has := em["value"]; has {
					media.Example, media.HasExample = v, true
					break
				}
			}
		}
	}
	return media, nil
}

func (d *Document) buildResponse(status string, raw any, at tokens) (*Response, error) {
	node, loc, err := deref(d.root, raw, at)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", at, err)
	}
	r := &Response{Status: status}
	switch {
	case status == "default":
		r.Code = 200
	case len(status) == 3 && strings.HasSuffix(strings.ToUpper(status), "XX"):
		r.Code = int(status[0]-'0') * 100
	default:
		n, err := strconv.Atoi(status)
		if err != nil || n < 100 || n > 599 {
			return nil, fmt.Errorf("%s: %q is not a status code, a class such as 2XX or default", at, status)
		}
		r.Code = n
	}
	if m, ok := node.(map[string]any); ok {
		content, _ := m["content"].(map[string]any)
		for _, mt := range sortedKeys(content) {
			media, err := d.buildMedia(nil, mt, content[mt], loc.with("content", mt), false)
			if err != nil {
				return nil, err
			}
			r.Media = append(r.Media, media)
		}
	}
	return r, nil
}
