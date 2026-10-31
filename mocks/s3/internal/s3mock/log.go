package s3mock

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// logRequest writes one access-log line. A secret a caller sends is logged by name
// only, never by value (law #56): header and query-parameter NAMES are listed, their
// values are not, and the path is the decoded object path alone. The Authorization
// header, the presigned X-Amz-Signature and the Content-MD5 therefore never appear.
func (m *Mock) logRequest(runID string, r *http.Request, operation string, status int) {
	_, _ = fmt.Fprintf(m.cfg.Log, "%s run=%s %s %s op=%s status=%d headers=[%s] query=[%s]\n",
		m.cfg.Now().UTC().Format(eventTime), runID, r.Method, r.URL.Path, operation, status,
		names(r.Header), queryNames(r.URL.Query()))
}

func names(h http.Header) string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, strings.ToLower(k))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func queryNames(q url.Values) string {
	out := make([]string, 0, len(q))
	for k := range q {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
