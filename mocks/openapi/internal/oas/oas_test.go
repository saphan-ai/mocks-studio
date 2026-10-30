package oas

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exampleDocument = "../../examples/acme-example-orders/openapi.yaml"

// shop is a 3.1 document with one operation per kind of rule a request can break.
const shop = `
openapi: 3.1.0
info: {title: Shop, version: "2"}
servers:
  - url: https://shop.example.com/{base}
    variables: {base: {default: api}}
paths:
  /items/{itemId}:
    parameters:
      - {name: itemId, in: path, required: true, schema: {type: integer, minimum: 1}}
    get:
      operationId: getItem
      parameters:
        - {name: X-Tenant, in: header, required: true, schema: {type: string, pattern: "^[a-z]+$"}}
        - {name: sort, in: query, schema: {type: string, enum: [asc, desc]}}
        - {name: tags, in: query, schema: {type: array, maxItems: 2, items: {type: string}}}
        - {name: session, in: cookie, schema: {type: string, minLength: 4}}
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {type: object, properties: {name: {type: string}}}}}
  /items:
    post:
      operationId: addItem
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name, price]
              additionalProperties: false
              properties:
                name: {type: string, minLength: 2}
                price: {type: number, exclusiveMinimum: 0}
                tags: {type: array, items: {type: string}, maxItems: 2}
          application/x-www-form-urlencoded:
            schema: {type: object, required: [name], properties: {name: {type: string}, qty: {type: integer}}}
      responses:
        "201": {description: created}
  /health:
    get:
      responses:
        "204": {description: fine}
`

const legacy = `
openapi: 3.0.3
info: {title: Legacy, version: "1"}
paths:
  /n:
    post:
      operationId: legacyPost
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                age: {type: integer, minimum: 0, exclusiveMinimum: true, nullable: true}
      responses:
        "200": {description: ok}
`

func loadText(t *testing.T, text string) *Document {
	t.Helper()
	d, err := Load(writeDoc(t, text))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return d
}

func writeDoc(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "doc.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func op(t *testing.T, d *Document, id string) *Operation {
	t.Helper()
	for _, o := range d.Ops {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("no operation %q", id)
	return nil
}

func TestLoadExampleDocument(t *testing.T) {
	d, err := Load(exampleDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Ops) != 3 {
		t.Fatalf("operations = %d, want 3", len(d.Ops))
	}
	if d.OpenAPI != "3.1.0" || d.Title != "Acme Example Orders" {
		t.Fatalf("info = %q %q", d.OpenAPI, d.Title)
	}
}

func TestLoadRefusals(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"broken reference", strings.Replace(shop, "{type: string, minLength: 2}", "{$ref: '#/components/schemas/Gone'}", 1), "#/components/schemas/Gone"},
		{"external reference", strings.Replace(shop, "{type: string, minLength: 2}", "{$ref: 'other.yaml#/Name'}", 1), "other.yaml#/Name"},
		{"not openapi 3", "swagger: '2.0'\npaths: {}\n", `"openapi"`},
		{"wrong version", "openapi: 4.0.0\npaths: {}\n", `"openapi"`},
		{"not yaml", "openapi: [3.1\n", "not valid YAML"},
		{"not json", "{\"openapi\": ", "not valid JSON"},
		{"empty", "  \n", "empty"},
		{"no operations", "openapi: 3.1.0\ninfo: {title: t, version: '1'}\npaths: {}\n", "no operation"},
		{"duplicate operationId", strings.Replace(shop, "operationId: addItem", "operationId: getItem", 1), "getItem"},
		{"invalid schema", strings.Replace(shop, "{type: integer, minimum: 1}", "{type: integer, minimum: high}", 1), "invalid"},
		{"bad parameter location", strings.Replace(shop, "in: cookie", "in: body", 1), "in"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeDoc(t, c.doc))
			if err == nil {
				t.Fatalf("Load accepted the document; want a refusal naming %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refusal %q does not name %q", err, c.want)
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil || !strings.Contains(err.Error(), "cannot be read") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRoutingHonoursBasePathAndSpecificity(t *testing.T) {
	d := loadText(t, shop)
	if d.BasePath != "/api" {
		t.Fatalf("BasePath = %q", d.BasePath)
	}
	if _, params, ok := d.Match("/api/items/7"); !ok || params["itemId"] != "7" {
		t.Fatalf("Match = %v %v", params, ok)
	}
	if _, _, ok := d.Match("/items/7"); ok {
		t.Fatal("a path without the server's base path matched")
	}
	r, _, ok := d.Match("/api/items")
	if !ok || strings.Join(r.Allowed(), ",") != "POST" {
		t.Fatalf("/api/items allowed = %v", r)
	}
}

// each case breaks exactly one field of an otherwise valid request.
func TestValidationNamesTheBrokenField(t *testing.T) {
	d := loadText(t, shop)
	get, add := op(t, d, "getItem"), op(t, d, "addItem")
	okGet := func() Request {
		return Request{
			PathParams: map[string]string{"itemId": "7"},
			Query:      url.Values{},
			Header:     http.Header{"X-Tenant": {"acme"}},
		}
	}
	okBody := func(body string) Request {
		return Request{ContentType: "application/json", Body: []byte(body), Header: http.Header{}, Query: url.Values{}}
	}
	cases := []struct {
		name string
		op   *Operation
		req  Request
		want Violation
	}{
		{"path type", get, func() Request { r := okGet(); r.PathParams["itemId"] = "seven"; return r }(), Violation{"/path/itemId", "type"}},
		{"path minimum", get, func() Request { r := okGet(); r.PathParams["itemId"] = "0"; return r }(), Violation{"/path/itemId", "minimum"}},
		{"query enum", get, func() Request { r := okGet(); r.Query.Set("sort", "sideways"); return r }(), Violation{"/query/sort", "enum"}},
		{"query array maxItems", get, func() Request { r := okGet(); r.Query["tags"] = []string{"a", "b", "c"}; return r }(), Violation{"/query/tags", "maxItems"}},
		{"header missing", get, func() Request { r := okGet(); r.Header = http.Header{}; return r }(), Violation{"/header/X-Tenant", "required"}},
		{"header pattern", get, func() Request { r := okGet(); r.Header.Set("X-Tenant", "ACME1"); return r }(), Violation{"/header/X-Tenant", "pattern"}},
		{"cookie minLength", get, func() Request { r := okGet(); r.Header.Set("Cookie", "session=ab"); return r }(), Violation{"/cookie/session", "minLength"}},
		{"body type", add, okBody(`{"name":"ab","price":"free"}`), Violation{"/body/price", "type"}},
		{"body exclusiveMinimum", add, okBody(`{"name":"ab","price":0}`), Violation{"/body/price", "exclusiveMinimum"}},
		{"body minLength", add, okBody(`{"name":"a","price":1}`), Violation{"/body/name", "minLength"}},
		{"body required", add, okBody(`{"name":"ab"}`), Violation{"/body/price", "required"}},
		{"body additionalProperties", add, okBody(`{"name":"ab","price":1,"colour":"red"}`), Violation{"/body/colour", "additionalProperties"}},
		{"body nested array", add, okBody(`{"name":"ab","price":1,"tags":["a","b","c"]}`), Violation{"/body/tags", "maxItems"}},
		{"body not JSON", add, okBody(`{"name":`), Violation{"/body", "json"}},
		{"body missing", add, okBody(``), Violation{"/body", "required"}},
		{"content type", add, Request{ContentType: "text/plain", Body: []byte("hi")}, Violation{"/header/Content-Type", "content-type"}},
		{"form type", add, Request{ContentType: "application/x-www-form-urlencoded", Body: []byte("name=a&qty=many")}, Violation{"/body/qty", "type"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.op.Validate(c.req)
			if len(got) != 1 || got[0] != c.want {
				t.Fatalf("violations = %v, want exactly %v", got, c.want)
			}
		})
	}
	t.Run("a valid request breaks nothing", func(t *testing.T) {
		if got := get.Validate(okGet()); len(got) != 0 {
			t.Fatalf("GET: %v", got)
		}
		if got := add.Validate(okBody(`{"name":"ab","price":1.5,"tags":["x"]}`)); len(got) != 0 {
			t.Fatalf("POST: %v", got)
		}
		form := Request{ContentType: "application/x-www-form-urlencoded; charset=utf-8", Body: []byte("name=a&qty=3")}
		if got := add.Validate(form); len(got) != 0 {
			t.Fatalf("form: %v", got)
		}
	})
}

func TestValidationReportsNoValue(t *testing.T) {
	d := loadText(t, shop)
	got := op(t, d, "addItem").Validate(Request{ContentType: "application/json", Body: []byte(`{"name":"ab","price":"hunter2-secret"}`)})
	for _, v := range got {
		if strings.Contains(v.Pointer+v.Rule, "hunter2") {
			t.Fatalf("a violation carries the value: %v", v)
		}
	}
}

func TestOpenAPI30DialectIsUnderstood(t *testing.T) {
	d := loadText(t, legacy)
	o := op(t, d, "legacyPost")
	body := func(s string) Request { return Request{ContentType: "application/json", Body: []byte(s)} }
	if got := o.Validate(body(`{"age":null}`)); len(got) != 0 {
		t.Fatalf("nullable age refused: %v", got)
	}
	if got := o.Validate(body(`{"age":0}`)); len(got) != 1 || got[0].Rule != "exclusiveMinimum" {
		t.Fatalf("exclusiveMinimum: true at 0 gave %v", got)
	}
	if got := o.Validate(body(`{"age":1}`)); len(got) != 0 {
		t.Fatalf("age 1 refused: %v", got)
	}
}

func TestGeneratedAnswersAreDeterministicAndFitTheSchema(t *testing.T) {
	d, err := Load(exampleDocument)
	if err != nil {
		t.Fatal(err)
	}
	get := op(t, d, "getOrder")
	first, second := get.SuccessAnswer(), get.SuccessAnswer()
	if !bytes.Equal(first.Body, second.Body) || first.Status != 200 || first.ContentType != "application/json" {
		t.Fatalf("answers differ or are malformed: %q vs %q (%d %s)", first.Body, second.Body, first.Status, first.ContentType)
	}
	want := `{"id":1,"quantity":1,"sku":"string","status":"placed"}`
	if string(first.Body) != want {
		t.Fatalf("generated body = %s, want %s", first.Body, want)
	}
}

func TestExampleWinsOverSchema(t *testing.T) {
	d, err := Load(exampleDocument)
	if err != nil {
		t.Fatal(err)
	}
	a := op(t, d, "listOrders").SuccessAnswer()
	if !strings.Contains(string(a.Body), "acme-widget") || a.Status != 200 {
		t.Fatalf("answer = %d %s, want the document's example", a.Status, a.Body)
	}
	if a := op(t, d, "createOrder").SuccessAnswer(); a.Status != 201 {
		t.Fatalf("createOrder answers %d, want its first 2xx (201)", a.Status)
	}
}

func TestRecursiveSchemaTerminates(t *testing.T) {
	d := loadText(t, `
openapi: 3.1.0
info: {title: Tree, version: "1"}
paths:
  /tree:
    get:
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {$ref: '#/components/schemas/Node'}}}
components:
  schemas:
    Node:
      type: object
      properties:
        name: {type: string}
        children: {type: array, items: {$ref: '#/components/schemas/Node'}}
`)
	a := d.Ops[0].SuccessAnswer()
	if len(a.Body) == 0 || len(a.Body) > 1000 {
		t.Fatalf("recursive answer has %d bytes", len(a.Body))
	}
}

func TestAnswerForADeclaredStatus(t *testing.T) {
	d := loadText(t, `
openapi: 3.1.0
info: {title: T, version: "1"}
paths:
  /x:
    get:
      responses:
        "200": {description: ok}
        "404":
          description: gone
          content: {application/json: {example: {error: gone}}}
`)
	if a, ok := d.Ops[0].AnswerFor(404); !ok || string(a.Body) != `{"error":"gone"}` {
		t.Fatalf("AnswerFor(404) = %v %s", ok, a.Body)
	}
	if _, ok := d.Ops[0].AnswerFor(500); ok {
		t.Fatal("AnswerFor(500) found a declaration that does not exist")
	}
}
