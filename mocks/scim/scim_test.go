package scimmock

import (
	"strconv"
	"strings"
	"testing"
)

func TestCreateGetListDeleteUser(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("alice@acme-example.test", jm{"displayName": "Alice Example", "name": jm{"givenName": "Alice"}})
	id := str(u, "id")
	if id == "" || u["active"] != true {
		t.Fatalf("created user lacks id or default active=true: %v", u)
	}
	meta := u["meta"].(map[string]any)
	if meta["resourceType"] != "User" || meta["version"] != `W/"1"` || !strings.HasSuffix(str(meta, "location"), "/Users/"+id) {
		t.Fatalf("meta = %v", meta)
	}
	st, h, got := e.do("GET", "/Users/"+id, nil)
	if st != 200 || str(got, "userName") != "alice@acme-example.test" || h.Get("ETag") != `W/"1"` {
		t.Fatalf("GET: %d etag=%q %v", st, h.Get("ETag"), got)
	}
	if ct := h.Get("Content-Type"); ct != scimJSON {
		t.Fatalf("Content-Type = %q", ct)
	}
	if st, _, _ = e.do("DELETE", "/Users/"+id, nil); st != 204 {
		t.Fatalf("DELETE: %d", st)
	}
	if st, _, _ = e.do("GET", "/Users/"+id, nil); st != 404 {
		t.Fatalf("GET after DELETE: %d", st)
	}
}

func TestPostAnswersCreatedWithLocationAndETag(t *testing.T) {
	e := newEnv(t)
	st, h, u := e.do("POST", "/Users", jm{"userName": "loc@acme-example.test"})
	if st != 201 || h.Get("Location") != str(u["meta"].(map[string]any), "location") || h.Get("ETag") == "" {
		t.Fatalf("POST: %d location=%q etag=%q", st, h.Get("Location"), h.Get("ETag"))
	}
}

// errorCases: each request differs from its valid twin in exactly one field (law #27).
func TestErrors(t *testing.T) {
	valid := func() jm { return jm{"schemas": []any{userURN}, "userName": "dup@acme-example.test", "active": true} }
	mutate := func(k string, v any) jm { b := valid(); b[k] = v; return b }
	cases := []struct {
		name   string
		mutant jm // the valid body with one field changed
		field  string
		status int
		typ    string
	}{
		{"userName-empty", mutate("userName", ""), "userName", 400, "invalidValue"},
		{"userName-wrong-type", mutate("userName", 42), "userName", 400, "invalidValue"},
		{"active-wrong-type", mutate("active", "yes"), "active", 400, "invalidValue"},
		{"name-wrong-type", mutate("name", "Alice"), "name", 400, "invalidValue"},
		{"emails-not-array", mutate("emails", jm{"value": "a@acme-example.test"}), "emails", 400, "invalidValue"},
		{"emails-element-not-object", mutate("emails", []any{"a@acme-example.test"}), "emails", 400, "invalidValue"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			// the twin: same body, the guarded field valid, accepted
			st, _, twin := e.do("POST", "/Users", valid())
			if st != 201 {
				t.Fatalf("valid twin refused: %d %v", st, twin)
			}
			e.do("DELETE", "/Users/"+str(twin, "id"), nil)
			st, _, out := e.do("POST", "/Users", c.mutant)
			assertScimError(t, st, out, c.status, c.typ)
		})
	}
}

func assertScimError(t *testing.T, st int, out jm, wantStatus int, wantType string) {
	t.Helper()
	if st != wantStatus {
		t.Fatalf("status = %d, want %d (%v)", st, wantStatus, out)
	}
	schemas := asArr(out["schemas"])
	if len(schemas) != 1 || schemas[0] != errSchema {
		t.Fatalf("error schemas = %v", out["schemas"])
	}
	if out["status"] != strconv.Itoa(wantStatus) {
		t.Fatalf("error status = %v, want the string %q (RFC 7644 §3.12)", out["status"], strconv.Itoa(wantStatus))
	}
	if str(out, "scimType") != wantType {
		t.Fatalf("scimType = %q, want %q (%v)", str(out, "scimType"), wantType, out)
	}
	if str(out, "detail") == "" {
		t.Fatalf("error has no detail: %v", out)
	}
}

func TestDuplicateUserNameIsUniquenessConflict(t *testing.T) {
	e := newEnv(t)
	e.createUser("taken@acme-example.test", nil)
	e.createUser("free@acme-example.test", nil) // the twin: a different userName is accepted
	st, _, out := e.do("POST", "/Users", jm{"userName": "TAKEN@acme-example.test"})
	assertScimError(t, st, out, 409, "uniqueness")
}

func TestMissingUserNameIsRefused(t *testing.T) {
	e := newEnv(t)
	st, _, out := e.do("POST", "/Users", jm{"displayName": "No Name"})
	assertScimError(t, st, out, 400, "invalidValue")
}

func TestMalformedJSONIsInvalidSyntax(t *testing.T) {
	e := newEnv(t)
	st, _, out := e.do("POST", "/Users", `{"userName": `)
	assertScimError(t, st, out, 400, "invalidSyntax")
}

func TestUnknownIDIsNotFound(t *testing.T) {
	e := newEnv(t)
	st, _, out := e.do("GET", "/Users/no-such-id", nil)
	assertScimError(t, st, out, 404, "")
}

func TestUserIDDoesNotResolveAsGroup(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("kind@acme-example.test", nil)
	st, _, out := e.do("GET", "/Groups/"+str(u, "id"), nil)
	assertScimError(t, st, out, 404, "")
}

func TestUnknownEndpointAndMethod(t *testing.T) {
	e := newEnv(t)
	st, _, out := e.do("GET", "/Bulk", nil)
	assertScimError(t, st, out, 404, "")
	st, h, out := e.do("DELETE", "/Users", nil)
	assertScimError(t, st, out, 405, "")
	if h.Get("Allow") == "" {
		t.Fatal("405 without an Allow header")
	}
}

func TestVersionPrefixIsAccepted(t *testing.T) {
	e := newEnv(t)
	st, _, u := e.do("POST", "/scim/v2/Users", jm{"userName": "pre@acme-example.test"})
	if st != 201 || !strings.Contains(str(u["meta"].(map[string]any), "location"), "/scim/v2/Users/") {
		t.Fatalf("prefixed POST: %d %v", st, u)
	}
	if st, _, _ = e.do("GET", "/v2/Users/"+str(u, "id"), nil); st != 200 {
		t.Fatalf("GET under /v2: %d", st)
	}
}

func TestPutReplacesAndBumpsVersion(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("put@acme-example.test", jm{"displayName": "Before", "title": "Engineer"})
	id := str(u, "id")
	st, h, out := e.do("PUT", "/Users/"+id, jm{"userName": "put@acme-example.test", "displayName": "After"})
	if st != 200 || str(out, "displayName") != "After" || out["title"] != nil {
		t.Fatalf("PUT must replace, dropping title: %d %v", st, out)
	}
	if h.Get("ETag") != `W/"2"` {
		t.Fatalf("ETag after change = %q, want W/\"2\"", h.Get("ETag"))
	}
	_, h, _ = e.do("PUT", "/Users/"+id, jm{"userName": "put@acme-example.test", "displayName": "After"})
	if h.Get("ETag") != `W/"2"` {
		t.Fatalf("an unchanged PUT bumped the version to %q", h.Get("ETag"))
	}
}

func TestPutCannotTakeAnotherUsersName(t *testing.T) {
	e := newEnv(t)
	e.createUser("one@acme-example.test", nil)
	two := e.createUser("two@acme-example.test", nil)
	st, _, out := e.do("PUT", "/Users/"+str(two, "id"), jm{"userName": "one@acme-example.test"})
	assertScimError(t, st, out, 409, "uniqueness")
}

// ETag: the request is the same in both halves; only If-Match differs.
func TestStaleETagIsPreconditionFailed(t *testing.T) {
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t)
			u := e.createUser("etag@acme-example.test", nil)
			id := str(u, "id")
			send := func(ifMatch string) (int, jm) {
				var body any
				switch method {
				case "PUT":
					body = jm{"userName": "etag@acme-example.test", "displayName": "X"}
				case "PATCH":
					body = jm{"schemas": []any{patchOpURN}, "Operations": []any{jm{"op": "replace", "path": "displayName", "value": "X"}}}
				}
				st, _, out := e.do(method, "/Users/"+id, body, "If-Match", ifMatch)
				return st, out
			}
			// make W/"1" stale
			e.do("PATCH", "/Users/"+id, jm{"Operations": []any{jm{"op": "replace", "path": "title", "value": "t"}}})
			st, out := send(`W/"1"`)
			assertScimError(t, st, out, 412, "")
			if st, _, got := e.do("GET", "/Users/"+id, nil); st != 200 || got["displayName"] != nil {
				t.Fatalf("the refused %s changed the resource: %v", method, got)
			}
			// the twin: the current ETag is accepted
			want := 200
			if method == "DELETE" {
				want = 204
			}
			if st, _ := send(`W/"2"`); st != want {
				t.Fatalf("current ETag: %d, want %d", st, want)
			}
		})
	}
}

func TestIfMatchStarAndIfNoneMatch(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("star@acme-example.test", nil)
	id := str(u, "id")
	body := jm{"userName": "star@acme-example.test", "displayName": "S"}
	if st, _, _ := e.do("PUT", "/Users/"+id, body, "If-Match", "*"); st != 200 {
		t.Fatalf("If-Match * : %d", st)
	}
	if st, _, _ := e.do("GET", "/Users/"+id, nil, "If-None-Match", `W/"2"`); st != 304 {
		t.Fatalf("If-None-Match current: %d, want 304", st)
	}
	if st, _, _ := e.do("GET", "/Users/"+id, nil, "If-None-Match", `W/"1"`); st != 200 {
		t.Fatalf("If-None-Match stale: %d, want 200", st)
	}
}

func TestPatchOperations(t *testing.T) {
	op := func(o, path string, value any) jm {
		m := jm{"op": o}
		if path != "" {
			m["path"] = path
		}
		if value != nil {
			m["value"] = value
		}
		return m
	}
	patch := func(ops ...jm) jm {
		arr := make([]any, len(ops))
		for i, o := range ops {
			arr[i] = o
		}
		return jm{"schemas": []any{patchOpURN}, "Operations": arr}
	}
	e := newEnv(t)
	u := e.createUser("patch@acme-example.test", jm{
		"emails": []any{jm{"value": "w@acme-example.test", "type": "work"}, jm{"value": "h@acme-example.test", "type": "home"}},
	})
	id := str(u, "id")
	apply := func(body jm) jm {
		t.Helper()
		st, _, out := e.do("PATCH", "/Users/"+id, body)
		if st != 200 {
			t.Fatalf("PATCH %v: %d %v", body, st, out)
		}
		return out
	}
	out := apply(patch(op("Replace", "active", false)))
	if out["active"] != false {
		t.Fatalf("replace active: %v", out["active"])
	}
	out = apply(patch(op("add", "name.givenName", "Pat"), op("add", "displayName", "Pat Example")))
	if out["name"].(map[string]any)["givenName"] != "Pat" || out["displayName"] != "Pat Example" {
		t.Fatalf("add: %v", out)
	}
	out = apply(patch(op("replace", `emails[type eq "work"].value`, "new@acme-example.test")))
	if asArr(out["emails"])[0].(map[string]any)["value"] != "new@acme-example.test" || asArr(out["emails"])[1].(map[string]any)["value"] != "h@acme-example.test" {
		t.Fatalf("valuePath replace changed the wrong element: %v", out["emails"])
	}
	out = apply(patch(op("add", "emails", []any{jm{"value": "x@acme-example.test", "type": "other"}})))
	if len(asArr(out["emails"])) != 3 {
		t.Fatalf("add to multi-valued: %v", out["emails"])
	}
	out = apply(patch(op("remove", `emails[type eq "home"]`, nil)))
	if len(asArr(out["emails"])) != 2 {
		t.Fatalf("filtered remove: %v", out["emails"])
	}
	out = apply(patch(op("remove", "displayName", nil)))
	if _, there := out["displayName"]; there {
		t.Fatalf("remove displayName: %v", out)
	}
	out = apply(patch(op("replace", "", jm{"title": "Lead", "active": true})))
	if out["title"] != "Lead" || out["active"] != true {
		t.Fatalf("replace without path: %v", out)
	}
	if out["meta"].(map[string]any)["version"] == `W/"1"` {
		t.Fatal("version never moved")
	}
}

func TestPatchErrors(t *testing.T) {
	good := func() []any {
		return []any{jm{"op": "replace", "path": "displayName", "value": "Fine"}}
	}
	with := func(o jm) []any { return []any{o} }
	cases := []struct {
		name  string
		ops   any
		field string
		typ   string
	}{
		{"operations-missing", nil, "Operations", "invalidSyntax"},
		{"op-unknown", with(jm{"op": "move", "path": "displayName", "value": "x"}), "op", "invalidSyntax"},
		{"readonly-id", with(jm{"op": "replace", "path": "id", "value": "x"}), "path", "mutability"},
		{"readonly-groups", with(jm{"op": "replace", "path": "groups", "value": []any{}}), "path", "mutability"},
		{"required-removed", with(jm{"op": "remove", "path": "userName"}), "path", "mutability"},
		{"remove-without-path", with(jm{"op": "remove"}), "path", "noTarget"},
		{"path-unknown-attribute", with(jm{"op": "replace", "path": "shoeSize", "value": "9"}), "path", "invalidPath"},
		{"filter-matches-nothing", with(jm{"op": "remove", "path": `emails[type eq "nothing"]`}), "path", "noTarget"},
		{"filter-invalid", with(jm{"op": "remove", "path": `emails[type bogus "x"]`}), "path", "invalidFilter"},
		{"value-wrong-type", with(jm{"op": "replace", "path": "active", "value": "no"}), "value", "invalidValue"},
		{"simple-attribute-with-sub", with(jm{"op": "replace", "path": "displayName.x", "value": "v"}), "path", "invalidPath"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			u := e.createUser("pe@acme-example.test", jm{"displayName": "Keep"})
			id := str(u, "id")
			if st, _, out := e.do("PATCH", "/Users/"+id, jm{"Operations": good()}); st != 200 { // twin
				t.Fatalf("valid twin refused: %d %v", st, out)
			}
			body := jm{"Operations": c.ops}
			if c.ops == nil {
				body = jm{}
			}
			st, _, out := e.do("PATCH", "/Users/"+id, body)
			assertScimError(t, st, out, 400, c.typ)
		})
	}
}

func TestPatchIsAtomic(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("atom@acme-example.test", jm{"displayName": "Before"})
	id := str(u, "id")
	st, _, _ := e.do("PATCH", "/Users/"+id, jm{"Operations": []any{
		jm{"op": "replace", "path": "displayName", "value": "After"},
		jm{"op": "replace", "path": "id", "value": "x"},
	}})
	if st != 400 {
		t.Fatalf("status %d", st)
	}
	_, _, got := e.do("GET", "/Users/"+id, nil)
	if got["displayName"] != "Before" {
		t.Fatalf("the first operation survived a failed PATCH: %v", got["displayName"])
	}
}

func TestPatchWithoutChangeKeepsVersion(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("same@acme-example.test", jm{"displayName": "Same"})
	_, h, _ := e.do("PATCH", "/Users/"+str(u, "id"), jm{"Operations": []any{jm{"op": "replace", "path": "displayName", "value": "Same"}}})
	if h.Get("ETag") != `W/"1"` {
		t.Fatalf("ETag = %q after a no-op PATCH", h.Get("ETag"))
	}
}

func TestPatchUserNameUniqueness(t *testing.T) {
	e := newEnv(t)
	e.createUser("a1@acme-example.test", nil)
	b := e.createUser("b1@acme-example.test", nil)
	st, _, out := e.do("PATCH", "/Users/"+str(b, "id"), jm{"Operations": []any{jm{"op": "replace", "path": "userName", "value": "a1@acme-example.test"}}})
	assertScimError(t, st, out, 409, "uniqueness")
}

func TestGroupsMembersAndMembership(t *testing.T) {
	e := newEnv(t)
	u1 := e.createUser("g1@acme-example.test", jm{"displayName": "G One"})
	u2 := e.createUser("g2@acme-example.test", nil)
	g := e.mustCreate("/Groups", jm{"displayName": "acme-example-admins", "members": []any{jm{"value": str(u1, "id")}}})
	gid := str(g, "id")
	m := asArr(g["members"])[0].(map[string]any)
	if m["type"] != "User" || m["display"] != "G One" {
		t.Fatalf("member not enriched: %v", m)
	}
	_, _, got := e.do("GET", "/Users/"+str(u1, "id"), nil)
	if gs := asArr(got["groups"]); len(gs) != 1 || gs[0].(map[string]any)["value"] != gid {
		t.Fatalf("user.groups = %v", got["groups"])
	}
	st, _, out := e.do("PATCH", "/Groups/"+gid, jm{"Operations": []any{jm{"op": "add", "path": "members", "value": []any{jm{"value": str(u2, "id")}}}}})
	if st != 200 || len(asArr(out["members"])) != 2 {
		t.Fatalf("add member: %d %v", st, out)
	}
	st, _, out = e.do("PATCH", "/Groups/"+gid, jm{"Operations": []any{jm{"op": "remove", "path": `members[value eq "` + str(u1, "id") + `"]`}}})
	if st != 200 || len(asArr(out["members"])) != 1 {
		t.Fatalf("remove member: %d %v", st, out)
	}
	st, _, out = e.do("PATCH", "/Groups/"+gid, jm{"Operations": []any{jm{"op": "remove", "path": "members", "value": []any{jm{"value": str(u2, "id")}}}}})
	if st != 200 || out["members"] != nil {
		t.Fatalf("remove member by value: %d %v", st, out)
	}
	e.do("PATCH", "/Groups/"+gid, jm{"Operations": []any{jm{"op": "add", "path": "members", "value": []any{jm{"value": str(u2, "id")}}}}})
	e.do("DELETE", "/Users/"+str(u2, "id"), nil)
	_, _, out = e.do("GET", "/Groups/"+gid, nil)
	if out["members"] != nil {
		t.Fatalf("a deleted user stayed a member: %v", out["members"])
	}
}

func TestGroupMemberMustExist(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("real@acme-example.test", nil)
	if st, _, out := e.do("POST", "/Groups", jm{"displayName": "ok", "members": []any{jm{"value": str(u, "id")}}}); st != 201 { // twin
		t.Fatalf("valid group refused: %d %v", st, out)
	}
	st, _, out := e.do("POST", "/Groups", jm{"displayName": "bad", "members": []any{jm{"value": "no-such-user"}}})
	assertScimError(t, st, out, 400, "invalidValue")
}

func TestScopesDoNotShareState(t *testing.T) {
	e := newEnv(t)
	other := e.open("run-other")
	e.createUser("mine@acme-example.test", nil)
	st, _, list := e.doAt(other, "GET", "/Users", nil)
	if st != 200 || list["totalResults"] != float64(0) {
		t.Fatalf("the second scope sees the first scope's user: %v", list)
	}
	if st, _, _ := e.doAt(other, "POST", "/Users", jm{"userName": "mine@acme-example.test"}); st != 201 {
		t.Fatalf("the same userName in another scope was refused: %d", st)
	}
}

func TestDiscoveryEndpoints(t *testing.T) {
	e := newEnv(t)
	st, _, spc := e.do("GET", "/ServiceProviderConfig", nil)
	if st != 200 || spc["patch"].(map[string]any)["supported"] != true || spc["etag"].(map[string]any)["supported"] != true || spc["bulk"].(map[string]any)["supported"] != false {
		t.Fatalf("ServiceProviderConfig: %d %v", st, spc)
	}
	st, _, rt := e.do("GET", "/ResourceTypes", nil)
	if st != 200 || rt["totalResults"] != float64(2) {
		t.Fatalf("ResourceTypes: %d %v", st, rt)
	}
	if st, _, _ = e.do("GET", "/ResourceTypes/User", nil); st != 200 {
		t.Fatalf("ResourceTypes/User: %d", st)
	}
	st, _, sc := e.do("GET", "/Schemas", nil)
	if st != 200 || sc["totalResults"] != float64(2) {
		t.Fatalf("Schemas: %d %v", st, sc)
	}
	if st, _, one := e.do("GET", "/Schemas/"+userURN, nil); st != 200 || one["name"] != "User" {
		t.Fatalf("Schemas/User: %d %v", st, one)
	}
	st, _, out := e.do("GET", "/Schemas/urn:nope", nil)
	assertScimError(t, st, out, 404, "")
	st, _, out = e.do("POST", "/ServiceProviderConfig", jm{})
	assertScimError(t, st, out, 405, "")
}

func TestPasswordIsNeverReturned(t *testing.T) {
	e := newEnv(t)
	u := e.createUser("pw@acme-example.test", jm{"password": "hunter2-example"})
	if _, there := u["password"]; there {
		t.Fatalf("password echoed on create: %v", u)
	}
	_, _, got := e.do("GET", "/Users/"+str(u, "id"), nil)
	if _, there := got["password"]; there {
		t.Fatalf("password returned on GET: %v", got)
	}
	st, _, _ := e.do("PATCH", "/Users/"+str(u, "id"), jm{"Operations": []any{jm{"op": "replace", "path": "password", "value": "hunter3-example"}}})
	if st != 200 {
		t.Fatalf("PATCH password: %d", st)
	}
}
