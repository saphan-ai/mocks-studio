package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

// maxProbeBody is the manifest schema's bound on a probe body.
const maxProbeBody = 4096

// probeMethods are the methods the manifest schema allows a probe to use, most harmless first.
var probeMethods = []string{"GET", "POST", "PUT", "DELETE"}

// buildProbe chooses the request the suite sends to make the mock record an event: the first operation whose
// generated request the document itself accepts, so the baseline answer is a 2xx and never a refusal.
func buildProbe(doc *oas.Document) (ProbeHTTP, *oas.Operation, error) {
	var fallback *ProbeHTTP
	var fallbackOp *oas.Operation
	for _, method := range probeMethods {
		for _, op := range doc.Ops {
			if op.Method != method {
				continue
			}
			p, req, ok := probeFor(doc, op)
			if !ok {
				continue
			}
			if len(op.Validate(req)) == 0 {
				return p, op, nil
			}
			if fallback == nil {
				fallback, fallbackOp = &p, op
			}
		}
	}
	if fallback != nil {
		return *fallback, fallbackOp, nil
	}
	return ProbeHTTP{}, nil, ErrNoProbe
}

// probeFor generates a request for the operation, or says it cannot: a probe carries no headers or cookies.
func probeFor(doc *oas.Document, op *oas.Operation) (ProbeHTTP, oas.Request, bool) {
	req := oas.Request{PathParams: map[string]string{}, Query: url.Values{}, Header: http.Header{}}
	path := op.Template
	for _, p := range op.Params {
		if p.In == "header" || p.In == "cookie" {
			if p.Required {
				return ProbeHTTP{}, req, false
			}
			continue
		}
		if p.In == "query" && !p.Required {
			continue
		}
		vals := paramStrings(doc, p)
		switch p.In {
		case "path":
			req.PathParams[p.Name] = vals[0]
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(vals[0]))
		case "query":
			req.Query[p.Name] = vals
		}
	}
	probe := ProbeHTTP{Method: op.Method, Path: doc.BasePath + path}
	if q := req.Query.Encode(); q != "" {
		probe.Path += "?" + q
	}
	if op.Body != nil && op.Body.Required {
		body, ok := probeBody(doc, op)
		if !ok {
			return ProbeHTTP{}, req, false
		}
		probe.Body = body
		req.ContentType, req.Body = "application/json", []byte(body)
	}
	return probe, req, true
}

func probeBody(doc *oas.Document, op *oas.Operation) (string, bool) {
	media := append([]*oas.Media(nil), op.Body.Media...)
	sort.SliceStable(media, func(i, j int) bool { return media[i].Type < media[j].Type })
	for _, m := range media {
		if !strings.EqualFold(m.Type, "application/json") || m.Node == nil {
			continue
		}
		b, err := json.Marshal(doc.Generate(m.Node))
		if err == nil && len(b) <= maxProbeBody {
			return string(b), true
		}
	}
	return "", false
}

// paramStrings renders a generated parameter value as the strings a request carries.
func paramStrings(doc *oas.Document, p *oas.Param) []string {
	var v any = "string"
	if p.Node != nil {
		v = doc.Generate(p.Node)
	}
	if list, ok := v.([]any); ok {
		out := make([]string, 0, len(list))
		for _, e := range list {
			out = append(out, scalarString(e))
		}
		if len(out) == 0 {
			return []string{""}
		}
		if !p.Explode {
			return []string{strings.Join(out, ",")}
		}
		return out
	}
	return []string{scalarString(v)}
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%v", x)
	case bool:
		return fmt.Sprintf("%t", x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
