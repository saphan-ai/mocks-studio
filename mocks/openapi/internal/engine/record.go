package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

// maxLoggedFields bounds the fields one verbose log line lists.
const maxLoggedFields = 64

// requestPayload is the payload of request.received: redacted by default, and in clear only the operator's safe fields.
func (s *scope) requestPayload(c *call, _ *oas.Operation) map[string]any {
	if len(s.e.safe) == 0 {
		return redactedPayload(c.body)
	}
	fields := map[string]any{}
	for _, f := range requestFields(c) {
		if s.e.safe[f.Name] && !f.secret && f.value != nil {
			if _, dup := fields[f.Name]; !dup {
				fields[f.Name] = f.value
			}
		}
	}
	return clearPayload(fields)
}

// requestField is one named value a request carries, before any redaction.
type requestField struct {
	Where, Name string
	value       any
	secret      bool // inside a field named like a credential: logged through the redactor, never exported
}

// requestFields lists the query parameters and the body fields of a call. Headers are never candidates for export.
func requestFields(c *call) []requestField {
	var out []requestField
	q := c.r.URL.Query()
	for _, name := range sortedNames(q) {
		out = append(out, requestField{Where: "query." + name, Name: name, value: q.Get(name)})
	}
	ctype := strings.ToLower(c.r.Header.Get("Content-Type"))
	switch {
	case len(bytes.TrimSpace(c.body)) == 0:
	case strings.Contains(ctype, "json"):
		dec := json.NewDecoder(bytes.NewReader(c.body))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil {
			flatten("", "", v, &out)
		}
	case strings.Contains(ctype, "x-www-form-urlencoded"):
		if form, err := url.ParseQuery(string(c.body)); err == nil {
			for _, name := range sortedNames(form) {
				out = append(out, requestField{Where: "body./" + name, Name: name, value: form.Get(name)})
			}
		}
	}
	return out
}

func sortedNames(v url.Values) []string {
	names := make([]string, 0, len(v))
	for n := range v {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// flatten lists the leaves of a JSON value with their pointers. Only a scalar can be exported (a safe field never carries
// an object or an array out whole), and a leaf inside a field named like a credential is marked secret and never exported.
func flatten(pointer, name string, v any, out *[]requestField) {
	flattenUnder(pointer, name, v, false, out)
}

func flattenUnder(pointer, name string, v any, secret bool, out *[]requestField) {
	if len(*out) >= maxLoggedFields {
		return
	}
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := pointer + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
			flattenUnder(child, k, x[k], secret || IsSecretName(k), out)
		}
	case []any:
		for i, e := range x {
			// an element is named by its place, never by the array's name: a safe name covers the field, not what it holds
			flattenUnder(pointer+"/"+strconv.Itoa(i), "", e, secret, out)
		}
	default:
		if pointer == "" {
			return
		}
		*out = append(*out, requestField{Where: "body." + pointer, Name: name, value: v, secret: secret})
	}
}

// logCall writes the access line of a call and, in verbose mode, the names of its fields with redacted values.
func (s *scope) logCall(c *call, summary string, a answer, viol []oas.Violation) {
	line := fmt.Sprintf("call run=%s correlation=%s %s -> %d outcome=%s", s.runID, c.corr, logSummary(summary), a.status, a.outcome)
	if a.fault != "" {
		line += " fault=" + a.fault
	}
	s.e.log.printf("%s", line)
	if !s.e.opts.Verbose {
		return
	}
	var fields []loggedField
	for name, vals := range c.r.Header {
		fields = append(fields, loggedField{"header." + name, name, strings.Join(vals, ",")})
	}
	for _, f := range requestFields(c) {
		name := f.Name
		if f.secret {
			name = "" // a leaf of a credential is never on the operator's safe list, whatever its own name
		}
		fields = append(fields, loggedField{f.Where, name, fmt.Sprint(f.value)})
	}
	s.e.log.printf("fields run=%s correlation=%s %s", s.runID, c.corr, render(fields, s.e.opts.Redact))
}

// logSummary keeps the unmatched path of a 404 out of the log: a path can carry what a caller meant to keep private.
func logSummary(summary string) string {
	if i := strings.Index(summary, " — no path template"); i >= 0 {
		method, _, _ := strings.Cut(summary, " ")
		return method + " <unmatched path>"
	}
	return summary
}
