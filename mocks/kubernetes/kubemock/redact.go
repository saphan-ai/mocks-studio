package kubemock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// redactionMarker stands where a value of a sensitive field would have been.
const redactionMarker = "<redacted>"

// sensitiveName matches field names that carry a secret.
var sensitiveName = regexp.MustCompile(`(?i)password|passwd|token|secret|credential|api[-_]?key|authorization|cookie`)

// logField renders one field of a request for the log. It is the only place a
// value could reach the log, and it never lets one through: a sensitive field
// is its name and the redaction marker, any other field is its name alone.
func logField(name, _ string) string {
	if sensitiveName.MatchString(name) {
		return name + "=" + redactionMarker
	}
	return name
}

// bodyBlocks are the configmap blocks whose keys the log names.
var bodyBlocks = []string{"data", "binaryData", "stringData"}

// requestLine is what the mock logs for one call: the run, the method and the
// path, then the names (never the values) of the query parameters, the headers
// and the keys of any data block the body carries.
func requestLine(runID string, r *http.Request, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run=%s %s %s", runID, r.Method, r.URL.Path)
	var query []string
	for k, v := range r.URL.Query() {
		query = append(query, logField(k, strings.Join(v, ",")))
	}
	if len(query) > 0 {
		sort.Strings(query)
		fmt.Fprintf(&b, " query=[%s]", strings.Join(query, " "))
	}
	var headers []string
	for k, v := range r.Header {
		headers = append(headers, logField(k, strings.Join(v, ",")))
	}
	if len(headers) > 0 {
		sort.Strings(headers)
		fmt.Fprintf(&b, " headers=[%s]", strings.Join(headers, " "))
	}
	if fields := bodyFields(body); len(fields) > 0 {
		fmt.Fprintf(&b, " fields=[%s]", strings.Join(fields, " "))
	}
	return b.String()
}

func bodyFields(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var doc map[string]any
	if err := decodeJSON(body, &doc); err != nil {
		return nil
	}
	var out []string
	for _, block := range bodyBlocks {
		entries, _ := doc[block].(map[string]any)
		for k, v := range entries {
			out = append(out, logField(block+"."+k, fmt.Sprint(v)))
		}
	}
	sort.Strings(out)
	return out
}

// logRequest writes one line to the mock's log.
func (m *Mock) logRequest(runID string, r *http.Request, body []byte) {
	_, _ = fmt.Fprintln(m.cfg.Log, requestLine(runID, r, body)) // a failing log sink must not fail a call
}

// digestPayload is the redacted payload of an event whose call has no safe field.
func digestPayload(body []byte) map[string]any {
	return map[string]any{"redacted": true, "digest": sha256Digest(body), "size": len(body)}
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
