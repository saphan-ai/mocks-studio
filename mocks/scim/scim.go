package scimmock

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const (
	scimJSON    = "application/scim+json"
	listURN     = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	maxResults  = 200
	defaultPage = 100
)

// reply is one answer, written after its events are published.
type reply struct {
	status int
	header http.Header
	body   any
}

func newReply(status int, body any) *reply {
	return &reply{status: status, header: http.Header{}, body: body}
}

func errReply(e *scimErr) *reply { return newReply(e.status, e.body()) }

func (rp *reply) write(w http.ResponseWriter) {
	for k, v := range rp.header {
		w.Header()[k] = v
	}
	if rp.body == nil {
		w.WriteHeader(rp.status)
		return
	}
	w.Header().Set("Content-Type", scimJSON)
	w.WriteHeader(rp.status)
	_ = json.NewEncoder(w).Encode(rp.body) // the client hanging up mid-write is not the mock's problem
}

// route dispatches one SCIM call. The store lock is held by the caller.
func (s *scope) route(r *http.Request, body []byte) *reply {
	path, prefix := r.URL.Path, ""
	for _, p := range []string{"/scim/v2", "/v2"} {
		if path == p || strings.HasPrefix(path, p+"/") {
			path, prefix = strings.TrimPrefix(path, p), p
			break
		}
	}
	base := "http://" + r.Host + prefix
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch segs[0] {
	case "Users", "Groups":
		kind := strings.TrimSuffix(segs[0], "s")
		switch len(segs) {
		case 1:
			return s.collection(r, body, kind, base)
		case 2:
			return s.item(r, body, kind, segs[1], base)
		}
	case "ServiceProviderConfig", "Schemas", "ResourceTypes":
		if len(segs) <= 2 {
			if r.Method != http.MethodGet {
				return methodNotAllowed(http.MethodGet)
			}
			return discovery(segs, base)
		}
	}
	return errReply(newErr(http.StatusNotFound, "", "No SCIM endpoint exists at %s and the request was refused; use /Users, /Groups, /ServiceProviderConfig, /Schemas or /ResourceTypes.", r.URL.Path))
}

func methodNotAllowed(allow ...string) *reply {
	rp := errReply(newErr(http.StatusMethodNotAllowed, "", "This endpoint does not accept that method and the request was refused; use one of %s.", strings.Join(allow, ", ")))
	rp.header.Set("Allow", strings.Join(allow, ", "))
	return rp
}

func parseBody(body []byte) (map[string]any, *scimErr) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, newErr(http.StatusBadRequest, "invalidSyntax", "The request body is not a JSON object and the request was refused; send a SCIM resource as a JSON object.")
	}
	return obj, nil
}

func (s *scope) collection(r *http.Request, body []byte, kind, base string) *reply {
	switch r.Method {
	case http.MethodGet:
		return s.list(r, kind, base)
	case http.MethodPost:
		obj, err := parseBody(body)
		if err != nil {
			return errReply(err)
		}
		res, err := s.create(kind, obj)
		if err != nil {
			return errReply(err)
		}
		rp := newReply(http.StatusCreated, s.render(res, base))
		rp.header.Set("Location", base+"/"+kind+"s/"+res.id)
		rp.header.Set("ETag", res.etag())
		return rp
	}
	return methodNotAllowed(http.MethodGet, http.MethodPost)
}

func (s *scope) item(r *http.Request, body []byte, kind, id, base string) *reply {
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodPatch && r.Method != http.MethodDelete {
		return methodNotAllowed(http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete)
	}
	res := s.store.byID[id]
	if res == nil || res.kind != kind {
		return errReply(newErr(http.StatusNotFound, "", "No %s with id %q exists in this scope and the request was refused; list /%ss to see the ids.", kind, id, kind))
	}
	if r.Method == http.MethodGet {
		if matches(r.Header.Get("If-None-Match"), res) {
			rp := newReply(http.StatusNotModified, nil)
			rp.header.Set("ETag", res.etag())
			return rp
		}
		return s.resourceReply(http.StatusOK, res, base)
	}
	if preconditionFailed(r, res) {
		return errReply(newErr(http.StatusPreconditionFailed, "", "The If-Match value %q does not match the current ETag %s of this %s and the request was refused; GET it again and retry with the new ETag.", r.Header.Get("If-Match"), res.etag(), kind))
	}
	switch r.Method {
	case http.MethodDelete:
		s.destroy(res)
		return newReply(http.StatusNoContent, nil)
	case http.MethodPut:
		obj, err := parseBody(body)
		if err != nil {
			return errReply(err)
		}
		if err := s.replace(res, obj); err != nil {
			return errReply(err)
		}
	default:
		obj, err := parseBody(body)
		if err != nil {
			return errReply(err)
		}
		if err := s.patch(res, obj); err != nil {
			return errReply(err)
		}
	}
	return s.resourceReply(http.StatusOK, res, base)
}

func (s *scope) resourceReply(status int, res *resource, base string) *reply {
	rp := newReply(status, s.render(res, base))
	rp.header.Set("ETag", res.etag())
	return rp
}

// preconditionFailed is the If-Match check of RFC 7644 §3.14: a request that names an ETag
// other than the resource's current one is refused with 412.
func preconditionFailed(r *http.Request, res *resource) bool {
	h := r.Header.Get("If-Match")
	return h != "" && !matches(h, res)
}

// matches reports whether an If-Match / If-None-Match value names the resource's ETag (weak comparison) or is "*".
func matches(header string, res *resource) bool {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(res.etag(), "W/") {
			return true
		}
	}
	return false
}

func (s *scope) list(r *http.Request, kind, base string) *reply {
	q := r.URL.Query()
	var flt filterNode
	if f := q.Get("filter"); f != "" {
		var err error
		if flt, err = parseFilter(f); err != nil {
			return errReply(newErr(http.StatusBadRequest, "invalidFilter", "The filter %q is not valid (%v) and the request was refused; use RFC 7644 syntax such as userName eq \"bob\".", f, err))
		}
	}
	start, count := 1, defaultPage
	if v := q.Get("startIndex"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return errReply(newErr(http.StatusBadRequest, "invalidValue", "The startIndex %q is not an integer and the request was refused; send a whole number of 1 or more.", v))
		}
		start = max(n, 1)
	}
	if v := q.Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return errReply(newErr(http.StatusBadRequest, "invalidValue", "The count %q is not an integer and the request was refused; send a whole number of 0 or more.", v))
		}
		count = min(max(n, 0), maxResults)
	}
	var hits []any
	for _, res := range s.store.list(kind) {
		out := s.render(res, base)
		if flt == nil || flt.eval(out) {
			hits = append(hits, out)
		}
	}
	page := []any{}
	if start <= len(hits) {
		page = hits[start-1 : min(start-1+count, len(hits))]
	}
	resp := map[string]any{"schemas": []any{listURN}, "totalResults": len(hits), "startIndex": start, "itemsPerPage": len(page)}
	if len(page) > 0 {
		resp["Resources"] = page
	}
	return newReply(http.StatusOK, resp)
}
