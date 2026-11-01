package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

const jsonType = "application/json"

// callEvidence is everything the engine wrote about one call: the log and every event of the run.
func callEvidence(h *harness, f *feed, seen ...map[string]any) string {
	var all strings.Builder
	all.WriteString(h.log.String())
	for _, ev := range append(seen, f.drain()...) {
		b, _ := json.Marshal(ev)
		all.Write(b)
	}
	return all.String()
}

// A field the operator marks safe exports its scalar value and nothing it contains: an object in a safe field never
// carries a credential into an event or the log. The mutated input is the safe field's value type.
func TestSafeFieldNeverExportsANestedCredential(t *testing.T) {
	const nested = "Pw!-nested-77-hunter"
	cases := []struct{ name, body string }{
		{"object with a password", `{"user":"ann","profile":{"password":"` + nested + `"}}`},
		{"array of objects with a password", `{"user":"ann","profile":[{"password":"` + nested + `"}]}`},
		{"scalar under a credential-named parent", `{"user":"ann","session":{"user":"` + nested + `"}}`},
		{"deep object", `{"user":"ann","profile":{"a":{"b":{"token":"` + nested + `"}}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, apiDocument, func(o *Options) { o.SafeFields = []string{"user", "profile"}; o.Verbose = true })
			base := h.open("run-safe-object")
			f := h.feed("run-safe-object")
			if r := send(t, http.MethodPost, base+"/login", c.body, map[string]string{"Content-Type": jsonType}); r.status != 200 {
				t.Fatalf("POST /login = %d %s", r.status, r.body)
			}
			req := f.next("request.received")
			f.next("response.sent")
			got := callEvidence(h, f, req)
			if strings.Contains(got, nested) {
				t.Fatalf("the credential reached an event or the log:\n%s", got)
			}
			if !strings.Contains(got, `"user":"ann"`) {
				t.Fatalf("the safe scalar field was not exported, so the check above measured nothing:\n%s", got)
			}
		})
	}
}

// A request that matches no route is recorded without its path: a path can carry a token.
func TestUnmatchedPathIsNotInAnEventOrTheLog(t *testing.T) {
	const token = "tok-reset-5521-abcdef"
	h := newHarness(t, apiDocument, func(o *Options) { o.Verbose = true })
	base := h.open("run-unmatched")
	f := h.feed("run-unmatched")
	if r := send(t, http.MethodGet, base+"/password-reset/"+token, "", nil); r.status != 404 {
		t.Fatalf("unmatched path = %d %s", r.status, r.body)
	}
	ev := f.next("request.received")
	f.next("response.sent")
	got := callEvidence(h, f, ev)
	if strings.Contains(got, token) {
		t.Fatalf("the unmatched path reached an event or the log:\n%s", got)
	}
	if !strings.Contains(got, "GET") || !strings.Contains(got, "no path template") {
		t.Fatalf("the event no longer says the request matched no route:\n%s", got)
	}
}

// treeDocument validates a body against a schema that contains itself.
const treeDocument = `
openapi: 3.1.0
info: {title: Tree, version: "1"}
paths:
  /tree:
    post:
      operationId: putTree
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/Node'}
      responses:
        "200": {description: ok}
components:
  schemas:
    Node:
      type: object
      required: [name]
      properties:
        name: {type: string}
        child: {$ref: '#/components/schemas/Node'}
`

func nested(depth int, leaf string) string {
	return strings.Repeat(`{"child":`, depth) + leaf + strings.Repeat(`}`, depth)
}

// A body nested past oas.MaxBodyDepth is refused with the rule maxDepth before validation. The mutated input is the nesting
// depth; the time bound is what a missing limit breaks (6 KB of nesting cost over a second inside the schema library).
func TestDeeplyNestedBodyIsRefusedBeforeValidation(t *testing.T) {
	h := newHarness(t, treeDocument, nil)
	base := h.open("run-depth")
	hdr := map[string]string{"Content-Type": jsonType}

	start := time.Now()
	r := send(t, http.MethodPost, base+"/tree", nested(2000, `{}`), hdr)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a 2000-deep body took %v; it must be refused before the schema runs", took)
	}
	doc := r.json(t)
	if r.status != 400 || doc["rule"] != "maxDepth" || doc["pointer"] != "/body" {
		t.Fatalf("2000-deep body = %d %s, want 400 rule maxDepth at /body", r.status, r.body)
	}
	if !strings.Contains(doc["message"].(string), "deeper") {
		t.Errorf("the refusal does not name the depth limit: %v", doc["message"])
	}

	// One level inside the limit is judged by the document (its wrappers lack a name), one level past it by the limit.
	inside := nested(oas.MaxBodyDepth-1, `{"name":"leaf"}`) // the leaf object is the limit-th level
	if r := send(t, http.MethodPost, base+"/tree", inside, hdr); r.json2(t)["rule"] == "maxDepth" {
		t.Errorf("a body at the limit = %d %s, must not be refused as too deep", r.status, r.body)
	}
	past := nested(oas.MaxBodyDepth, `{"name":"leaf"}`)
	if r := send(t, http.MethodPost, base+"/tree", past, hdr); r.status != 400 || r.json2(t)["rule"] != "maxDepth" {
		t.Errorf("a body one level past the limit = %d %s, want maxDepth", r.status, r.body)
	}
}

// json2 is json for a reply whose body may be empty.
func (r reply) json2(t *testing.T) map[string]any {
	t.Helper()
	if len(r.body) == 0 {
		return map[string]any{}
	}
	return r.json(t)
}
