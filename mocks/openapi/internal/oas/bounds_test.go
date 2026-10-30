package oas

import (
	"encoding/json"
	"testing"
)

const hugeArray = `
openapi: 3.1.0
info: {title: Huge, version: "1"}
paths:
  /list:
    get:
      operationId: list
      responses:
        "200":
          description: ok
          content: {application/json: {schema: {type: array, minItems: 300000, items: {type: string}}}}
`

// A schema that asks for 300000 items is answered with at most MaxGenItems: a mock never builds an answer of the
// document's choosing in size. The mutated input is minItems.
func TestGeneratedArrayLengthIsCapped(t *testing.T) {
	d := loadText(t, hugeArray)
	var got []any
	if err := json.Unmarshal(op(t, d, "list").SuccessAnswer().Body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || len(got) > MaxGenItems {
		t.Fatalf("generated %d items, want 1..%d", len(got), MaxGenItems)
	}
}

// Anything after the JSON value is refused as json, a stray closing bracket included. The mutated input is the tail.
func TestJSONBodyWithATrailingTailIsRefused(t *testing.T) {
	add := op(t, loadText(t, shop), "addItem")
	for _, c := range []struct {
		tail string
		want string
	}{{"", ""}, {"\n", ""}, {"}", RuleJSON}, {"]", RuleJSON}, {"x", RuleJSON}, {` {"a":1}`, RuleJSON}} {
		got := add.Validate(Request{ContentType: "application/json", Body: []byte(`{"name":"ab","price":1}` + c.tail)})
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("tail %q: %v, want accepted", c.tail, got)
		case c.want != "" && (len(got) != 1 || got[0] != Violation{"/body", c.want}):
			t.Errorf("tail %q: %v, want /body %s", c.tail, got, c.want)
		}
	}
}

// Brackets inside strings do not count as nesting, and the limit is exact.
func TestJSONDepthCountsNestingNotBracketsInStrings(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{}`, 1}, {`[[1],[2]]`, 2}, {`{"a":"[[[[{{"}`, 1}, {`{"a":"\"[[["}`, 1}, {`"x"`, 0}, {`{"a":{"b":[{}]}}`, 4},
	}
	for _, c := range cases {
		if got := jsonDepth([]byte(c.body)); got != c.want {
			t.Errorf("jsonDepth(%s) = %d, want %d", c.body, got, c.want)
		}
	}
}

// Each operation builds its success answer once, not on every call.
func TestSuccessAnswerIsBuiltOncePerOperation(t *testing.T) {
	o := op(t, loadText(t, hugeArray), "list")
	a, b := o.SuccessAnswer(), o.SuccessAnswer()
	if len(a.Body) == 0 || &a.Body[0] != &b.Body[0] {
		t.Fatal("two calls built two answers; the answer is not computed once")
	}
}
