package scimmock

import (
	"net/url"
	"strconv"
	"testing"
)

// seedFilterUsers creates five fictional users and returns nothing: every case names its expected userNames.
func seedFilterUsers(e *env) {
	e.createUser("alice@acme-example.test", jm{"displayName": "Alice Anders", "active": true, "title": "Engineer", "externalId": "EXT-1",
		"name": jm{"givenName": "Alice", "familyName": "Anders"}, "emails": []any{jm{"value": "alice@acme-example.test", "type": "work"}}})
	e.createUser("bob@acme-example.test", jm{"displayName": "Bob Brown", "active": false, "title": "Manager",
		"name": jm{"givenName": "Bob", "familyName": "Brown"}, "emails": []any{jm{"value": "bob@acme-example.test", "type": "work"}, jm{"value": "bob@home-example.test", "type": "home"}}})
	e.createUser("carol@acme-example.test", jm{"displayName": "Carol Clark", "active": true,
		"name": jm{"givenName": "Carol", "familyName": "Clark"}})
	e.createUser("dave.anders@acme-example.test", jm{"displayName": "Dave Anders", "active": true, "title": "Engineer",
		"name": jm{"givenName": "Dave", "familyName": "Anders"}})
	e.createUser("erin@acme-example.test", jm{"active": false})
}

func TestFilterReturnsExactlyTheMatchingSeededUsers(t *testing.T) {
	e := newEnv(t)
	seedFilterUsers(e)
	const (
		alice = "alice@acme-example.test"
		bob   = "bob@acme-example.test"
		carol = "carol@acme-example.test"
		dave  = "dave.anders@acme-example.test"
		erin  = "erin@acme-example.test"
	)
	cases := []struct {
		name, filter string
		want         []string
	}{
		{"eq", `userName eq "bob@acme-example.test"`, []string{bob}},
		{"eq-case-insensitive", `USERNAME eq "BOB@ACME-EXAMPLE.TEST"`, []string{bob}},
		{"eq-bool", `active eq false`, []string{bob, erin}},
		{"eq-no-match", `userName eq "nobody@acme-example.test"`, nil},
		{"ne", `active ne true`, []string{bob, erin}},
		{"co", `displayName co "Anders"`, []string{alice, dave}},
		{"sw", `userName sw "dave"`, []string{dave}},
		{"ew", `userName ew "@acme-example.test"`, []string{alice, bob, carol, dave, erin}},
		{"pr", `title pr`, []string{alice, bob, dave}},
		{"pr-absent-attribute", `nickName pr`, nil},
		{"and", `title eq "Engineer" and active eq true`, []string{alice, dave}},
		{"or", `userName sw "carol" or userName sw "erin"`, []string{carol, erin}},
		{"and-binds-tighter-than-or", `userName sw "bob" or title eq "Engineer" and active eq true`, []string{alice, bob, dave}},
		{"parentheses", `(userName sw "bob" or title eq "Engineer") and active eq true`, []string{alice, dave}},
		{"not", `not (title pr)`, []string{carol, erin}},
		{"sub-attribute", `name.familyName eq "Anders"`, []string{alice, dave}},
		{"multi-valued-any-element", `emails.value co "home-example"`, []string{bob}},
		{"value-path", `emails[type eq "home" and value co "bob"]`, []string{bob}},
		{"case-exact-externalId-hit", `externalId eq "EXT-1"`, []string{alice}},
		{"case-exact-externalId-wrong-case", `externalId eq "ext-1"`, nil},
		{"eq-null", `title eq null`, []string{carol, erin}},
		{"gt-string", `userName gt "carol@acme-example.test"`, []string{dave, erin}},
		{"id", `id pr`, []string{alice, bob, carol, dave, erin}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, list := e.do("GET", "/Users?filter="+url.QueryEscape(c.filter), nil)
			if st != 200 {
				t.Fatalf("status %d: %v", st, list)
			}
			if got := userNames(list); !sameSet(got, c.want) {
				t.Fatalf("filter %q returned %v, want exactly %v", c.filter, got, c.want)
			}
			if int(list["totalResults"].(float64)) != len(c.want) {
				t.Fatalf("totalResults = %v, want %d", list["totalResults"], len(c.want))
			}
		})
	}
}

func TestFilterOnGroups(t *testing.T) {
	e := newEnv(t)
	e.mustCreate("/Groups", jm{"displayName": "acme-example-admins"})
	e.mustCreate("/Groups", jm{"displayName": "acme-example-readers"})
	_, _, list := e.do("GET", "/Groups?filter="+url.QueryEscape(`displayName sw "acme-example-r"`), nil)
	rs := asArr(list["Resources"])
	if len(rs) != 1 || rs[0].(map[string]any)["displayName"] != "acme-example-readers" {
		t.Fatalf("group filter: %v", list)
	}
}

func TestInvalidFilterIsRefused(t *testing.T) {
	e := newEnv(t)
	valid := `userName eq "x"`
	if st, _, _ := e.do("GET", "/Users?filter="+url.QueryEscape(valid), nil); st != 200 { // twin
		t.Fatalf("valid twin refused: %d", st)
	}
	for name, bad := range map[string]string{
		"unknown-operator":    `userName like "x"`,
		"missing-value":       `userName eq`,
		"unclosed-string":     `userName eq "x`,
		"unclosed-paren":      `(userName eq "x"`,
		"trailing-garbage":    `userName eq "x" "y"`,
		"co-needs-string":     `userName co 3`,
		"dangling-and":        `userName eq "x" and`,
		"not-without-paren":   `not userName eq "x"`,
		"null-with-gt":        `userName gt null`,
		"bad-attribute-chars": `user..Name eq "x"`,
	} {
		t.Run(name, func(t *testing.T) {
			st, _, out := e.do("GET", "/Users?filter="+url.QueryEscape(bad), nil)
			assertScimError(t, st, out, 400, "invalidFilter")
		})
	}
}

func TestPaging(t *testing.T) {
	e := newEnv(t)
	for i := 1; i <= 7; i++ {
		e.createUser("page"+strconv.Itoa(i)+"@acme-example.test", nil)
	}
	page := func(q string) jm {
		t.Helper()
		st, _, list := e.do("GET", "/Users"+q, nil)
		if st != 200 {
			t.Fatalf("GET /Users%s: %d %v", q, st, list)
		}
		return list
	}
	l := page("?startIndex=3&count=2")
	if got := userNames(l); len(got) != 2 || got[0] != "page3@acme-example.test" || got[1] != "page4@acme-example.test" {
		t.Fatalf("page = %v", got)
	}
	if l["totalResults"] != float64(7) || l["startIndex"] != float64(3) || l["itemsPerPage"] != float64(2) {
		t.Fatalf("list header = %v", l)
	}
	if got := userNames(page("?startIndex=6&count=10")); len(got) != 2 {
		t.Fatalf("last page = %v", got)
	}
	if l = page("?startIndex=99"); l["Resources"] != nil || l["itemsPerPage"] != float64(0) || l["totalResults"] != float64(7) {
		t.Fatalf("past the end = %v", l)
	}
	if l = page("?count=0"); l["Resources"] != nil || l["totalResults"] != float64(7) {
		t.Fatalf("count=0 = %v", l)
	}
	if got := userNames(page("?startIndex=-4&count=1")); len(got) != 1 || got[0] != "page1@acme-example.test" {
		t.Fatalf("startIndex below 1 must act as 1: %v", got)
	}
	st, _, out := e.do("GET", "/Users?startIndex=abc", nil)
	assertScimError(t, st, out, 400, "invalidValue")
	st, _, out = e.do("GET", "/Users?count=x", nil)
	assertScimError(t, st, out, 400, "invalidValue")
}

func TestPagingAppliesAfterFilter(t *testing.T) {
	e := newEnv(t)
	seedFilterUsers(e)
	_, _, l := e.do("GET", "/Users?filter="+url.QueryEscape(`active eq true`)+"&startIndex=2&count=1", nil)
	if l["totalResults"] != float64(3) || len(userNames(l)) != 1 || userNames(l)[0] != "carol@acme-example.test" {
		t.Fatalf("filtered page = %v", l)
	}
}

func TestFilterParserDirect(t *testing.T) {
	// A parser-level twin of the invalid cases: the valid form parses, the one changed token does not.
	if _, err := parseFilter(`title eq "x" and active eq true`); err != nil {
		t.Fatal(err)
	}
	if _, err := parseFilter(`title eq "x" xor active eq true`); err == nil {
		t.Fatal("xor must not parse")
	}
}
