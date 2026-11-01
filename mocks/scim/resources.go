package scimmock

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"
)

const (
	kindUser  = "User"
	kindGroup = "Group"

	errSchema = "urn:ietf:params:scim:api:messages:2.0:Error"
)

// scimErr is an RFC 7644 §3.12 error.
type scimErr struct {
	status  int
	scimTyp string
	detail  string
}

func (e *scimErr) Error() string { return e.detail }

func newErr(status int, scimType, format string, a ...any) *scimErr {
	return &scimErr{status: status, scimTyp: scimType, detail: fmt.Sprintf(format, a...)}
}

func (e *scimErr) body() map[string]any {
	b := map[string]any{"schemas": []any{errSchema}, "status": fmt.Sprint(e.status), "detail": e.detail}
	if e.scimTyp != "" {
		b["scimType"] = e.scimTyp
	}
	return b
}

// attribute kinds of the core schemas this mock stores.
const (
	aString  = "string"
	aBool    = "bool"
	aComplex = "complex"
	aMulti   = "multi"
)

var userAttrs = map[string]string{
	"userName": aString, "externalId": aString, "displayName": aString, "nickName": aString, "profileUrl": aString,
	"title": aString, "userType": aString, "preferredLanguage": aString, "locale": aString, "timezone": aString,
	"password": aString, "active": aBool, "name": aComplex,
	"emails": aMulti, "phoneNumbers": aMulti, "addresses": aMulti, "ims": aMulti, "photos": aMulti,
	"roles": aMulti, "entitlements": aMulti, "x509Certificates": aMulti,
}

var groupAttrs = map[string]string{"displayName": aString, "externalId": aString, "members": aMulti}

func attrTable(kind string) map[string]string {
	if kind == kindUser {
		return userAttrs
	}
	return groupAttrs
}

// canonAttr maps a case-insensitive attribute name to its canonical name and kind.
func canonAttr(kind, name string) (string, string, bool) {
	for k, t := range attrTable(kind) {
		if strings.EqualFold(k, name) {
			return k, t, true
		}
	}
	return "", "", false
}

func isReadOnly(kind, name string) bool {
	switch strings.ToLower(name) {
	case "id", "meta", "schemas":
		return true
	case "groups":
		return kind == kindUser
	}
	return false
}

// resource is one stored User or Group. password is never stored.
type resource struct {
	id       string
	kind     string
	attrs    map[string]any
	created  time.Time
	modified time.Time
	version  int
}

func (r *resource) etag() string { return fmt.Sprintf(`W/"%d"`, r.version) }

// store holds one scope's resources in creation order.
type store struct {
	byID  map[string]*resource
	order []string
}

func newStore() *store { return &store{byID: map[string]*resource{}} }

func (st *store) list(kind string) []*resource {
	var out []*resource
	for _, id := range st.order {
		if r := st.byID[id]; r.kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (st *store) add(r *resource) {
	st.byID[r.id] = r
	st.order = append(st.order, r.id)
}

func (st *store) remove(id string) {
	delete(st.byID, id)
	for i, o := range st.order {
		if o == id {
			st.order = append(st.order[:i], st.order[i+1:]...)
			return
		}
	}
}

func newID() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:32]
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = deepCopy(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = deepCopy(x)
		}
		return out
	}
	return v
}

// normalize turns a request body into the stored attribute map: names are made canonical,
// read-only attributes and unknown ones are dropped, password is dropped, types are checked.
func normalize(kind string, body map[string]any) (map[string]any, *scimErr) {
	attrs := map[string]any{}
	for k, v := range body {
		canon, _, ok := canonAttr(kind, k)
		if !ok || canon == "password" {
			continue // read-only (id, meta, groups), write-only (password) or not modelled by this mock
		}
		attrs[canon] = deepCopy(v)
	}
	if err := checkTypes(kind, attrs); err != nil {
		return nil, err
	}
	return attrs, nil
}

func checkTypes(kind string, attrs map[string]any) *scimErr {
	for name, v := range attrs {
		_, t, _ := canonAttr(kind, name)
		switch t {
		case aString:
			if _, ok := v.(string); !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The attribute %q must be a string and the request was refused; send a string.", name)
			}
		case aBool:
			if _, ok := v.(bool); !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The attribute %q must be a boolean and the request was refused; send true or false.", name)
			}
		case aComplex:
			if _, ok := v.(map[string]any); !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The attribute %q must be an object and the request was refused; send an object.", name)
			}
		case aMulti:
			arr, ok := v.([]any)
			if !ok {
				return newErr(http.StatusBadRequest, "invalidValue", "The attribute %q is multi-valued and must be an array; the request was refused, send an array.", name)
			}
			for _, e := range arr {
				if _, ok := e.(map[string]any); !ok {
					return newErr(http.StatusBadRequest, "invalidValue", "Every element of %q must be an object and the request was refused; send objects such as {\"value\":\"...\"}.", name)
				}
			}
		}
	}
	return nil
}

func requiredAttr(kind string) string {
	if kind == kindUser {
		return "userName"
	}
	return "displayName"
}

// validate checks what a stored resource must satisfy: its required attribute is a
// non-empty string, userName is unique, and group members exist.
func (s *scope) validate(kind, selfID string, attrs map[string]any) *scimErr {
	req := requiredAttr(kind)
	if v, ok := attrs[req].(string); !ok || v == "" {
		return newErr(http.StatusBadRequest, "invalidValue", "The attribute %q is required and was missing or empty, so the request was refused; send a non-empty %s.", req, req)
	}
	if kind == kindUser {
		name := attrs["userName"].(string)
		for _, u := range s.store.list(kindUser) {
			if u.id != selfID && strings.EqualFold(u.attrs["userName"].(string), name) {
				return newErr(http.StatusConflict, "uniqueness", "The userName %q is already taken by another user and the request was refused; pick a different userName.", name)
			}
		}
		return nil
	}
	return s.normalizeMembers(selfID, attrs)
}

// normalizeMembers checks every member of a group exists and fills type, display and $ref.
func (s *scope) normalizeMembers(selfID string, attrs map[string]any) *scimErr {
	arr, _ := attrs["members"].([]any)
	if len(arr) == 0 {
		delete(attrs, "members")
		return nil
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(arr))
	for _, e := range arr {
		m := e.(map[string]any)
		id, _ := m["value"].(string)
		target := s.store.byID[id]
		if id == "" || target == nil || id == selfID {
			return newErr(http.StatusBadRequest, "invalidValue", "The member %q does not name an existing user or another group and the request was refused; create it first or remove it from members.", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		display, _ := target.attrs["displayName"].(string)
		if display == "" {
			display, _ = target.attrs["userName"].(string)
		}
		out = append(out, map[string]any{"value": id, "display": display, "type": target.kind, "$ref": "/" + target.kind + "s/" + id})
	}
	attrs["members"] = out
	return nil
}

// render is the resource as the API returns it. base is the URL prefix up to and excluding "/Users".
func (s *scope) render(r *resource, base string) map[string]any {
	out := map[string]any{"id": r.id}
	for k, v := range r.attrs {
		out[k] = v
	}
	if r.kind == kindUser {
		out["schemas"] = []any{userURN}
		var groups []any
		for _, g := range s.store.list(kindGroup) {
			for _, m := range asArr(g.attrs["members"]) {
				if mm := m.(map[string]any); mm["value"] == r.id {
					groups = append(groups, map[string]any{"value": g.id, "display": g.attrs["displayName"], "$ref": base + "/Groups/" + g.id, "type": "direct"})
				}
			}
		}
		if len(groups) > 0 {
			out["groups"] = groups
		}
	} else {
		out["schemas"] = []any{groupURN}
		if arr := asArr(out["members"]); len(arr) > 0 {
			ms := make([]any, len(arr))
			for i, m := range arr {
				c := deepCopy(m).(map[string]any)
				c["$ref"] = base + c["$ref"].(string)
				ms[i] = c
			}
			out["members"] = ms
		}
	}
	out["meta"] = map[string]any{
		"resourceType": r.kind,
		"created":      r.created.UTC().Format(time.RFC3339),
		"lastModified": r.modified.UTC().Format(time.RFC3339),
		"location":     base + "/" + r.kind + "s/" + r.id,
		"version":      r.etag(),
	}
	return out
}

func asArr(v any) []any {
	a, _ := v.([]any)
	return a
}

// create stores a new resource from a request body.
func (s *scope) create(kind string, body map[string]any) (*resource, *scimErr) {
	attrs, err := normalize(kind, body)
	if err != nil {
		return nil, err
	}
	if _, ok := attrs["active"]; !ok && kind == kindUser {
		attrs["active"] = true
	}
	id := newID()
	if err := s.validate(kind, id, attrs); err != nil {
		return nil, err
	}
	now := s.m.now()
	r := &resource{id: id, kind: kind, attrs: attrs, created: now, modified: now, version: 1}
	s.store.add(r)
	return r, nil
}

// replace implements PUT: the stored attributes become exactly the body's.
func (s *scope) replace(r *resource, body map[string]any) *scimErr {
	attrs, err := normalize(r.kind, body)
	if err != nil {
		return err
	}
	if _, ok := attrs["active"]; !ok && r.kind == kindUser {
		attrs["active"] = true
	}
	return s.commit(r, attrs)
}

// commit validates and stores new attributes, bumping the version only if something changed.
func (s *scope) commit(r *resource, attrs map[string]any) *scimErr {
	if err := s.validate(r.kind, r.id, attrs); err != nil {
		return err
	}
	if reflect.DeepEqual(r.attrs, attrs) {
		return nil
	}
	r.attrs = attrs
	r.modified = s.m.now()
	r.version++
	return nil
}

// destroy deletes a resource and removes it from the members of every group.
func (s *scope) destroy(r *resource) {
	s.store.remove(r.id)
	for _, g := range s.store.list(kindGroup) {
		arr := asArr(g.attrs["members"])
		kept := make([]any, 0, len(arr))
		for _, m := range arr {
			if m.(map[string]any)["value"] != r.id {
				kept = append(kept, m)
			}
		}
		if len(kept) != len(arr) {
			attrs := deepCopy(g.attrs).(map[string]any)
			if len(kept) == 0 {
				delete(attrs, "members")
			} else {
				attrs["members"] = kept
			}
			g.attrs = attrs
			g.modified = s.m.now()
			g.version++
		}
	}
}
