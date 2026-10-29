package schemas_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/saphan-ai/mocks-studio/conformance/internal/schemas"
)

// specRoot is the repository's spec directory, relative to this package.
const specRoot = "../../../spec"

func readSpec(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(specRoot, rel))
	if err != nil {
		t.Fatalf("spec file %s: %v", rel, err)
	}
	return b
}

func decode(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

// The embedded copies are the originals: a drifted copy would validate against a
// contract nobody published.
func TestEmbeddedSchemasMatchSpec(t *testing.T) {
	origins := map[string]string{
		"mocks-event-v1.schema.json":    "ecosystem-v1/mocks-event-v1.schema.json",
		"mocks-manifest-v1.schema.json": "schemas/mocks-manifest-v1.schema.json",
		"mocks-refusal-v1.schema.json":  "schemas/mocks-refusal-v1.schema.json",
	}
	for name, raw := range schemas.Embedded() {
		origin, ok := origins[name]
		if !ok {
			t.Fatalf("embedded schema %s has no declared origin in spec/", name)
		}
		if !bytes.Equal(raw, readSpec(t, origin)) {
			t.Errorf("embedded %s differs from spec/%s; copy the spec file over it", name, origin)
		}
	}
}

type examples struct {
	Valid   []map[string]any `json:"valid"`
	Invalid []struct {
		Breaks string         `json:"breaks"`
		Why    string         `json:"why"`
		Patch  map[string]any `json:"patch"`
	} `json:"invalid"`
}

// Appendix B: every valid example passes, and every invalid one is refused on the
// field it names (negative control: the patch is the only difference from valid[0]).
func TestEcosystemExamplesAgainstEventSchema(t *testing.T) {
	var ex examples
	if err := json.Unmarshal(readSpec(t, "ecosystem-v1/mocks-event-v1.examples.json"), &ex); err != nil {
		t.Fatal(err)
	}
	for i, v := range ex.Valid {
		problem, err := schemas.Validate(schemas.EventID, v)
		if err != nil || problem != "" {
			t.Errorf("valid[%d] refused: %v %s", i, err, problem)
		}
	}
	base := ex.Valid[0]
	for _, inv := range ex.Invalid {
		t.Run(inv.Breaks, func(t *testing.T) {
			ev := map[string]any{}
			for k, v := range base {
				ev[k] = v
			}
			for k, v := range inv.Patch {
				ev[k] = v
			}
			problem, err := schemas.Validate(schemas.EventID, ev)
			if err != nil {
				t.Fatal(err)
			}
			if problem == "" {
				t.Fatalf("invalid example %q was accepted", inv.Breaks)
			}
			field := strings.TrimPrefix(inv.Breaks, "mock.")
			if inv.Breaks == "additionalProperties" {
				field = "correlationId"
			}
			if !strings.Contains(problem, field) && !strings.Contains(problem, strings.ReplaceAll(inv.Breaks, ".", "/")) {
				t.Errorf("refused, but not on %q: %s", inv.Breaks, problem)
			}
		})
	}
}

// The published non-zero trace-id rule (32-hex pattern plus `not` const all-zero) compiles
// under RE2 and keeps its meaning: a non-zero id passes; all zeros, a 31-character id,
// upper case and a non-hex character are refused on correlation_id.
func TestEventSchemaRefusesAllZeroTraceID(t *testing.T) {
	var ex examples
	if err := json.Unmarshal(readSpec(t, "ecosystem-v1/mocks-event-v1.examples.json"), &ex); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"4bf92f3577b34da6a3ce929d0e0e4736": true,
		"00000000000000000000000000000000": false,
		"4bf92f3577b34da6a3ce929d0e0e473":  false,
		"4BF92F3577B34DA6A3CE929D0E0E4736": false,
		"00000000000000000000000000000001": true,
		"0000000000000000000000000000000g": false,
	}
	for id, wantValid := range cases {
		ev := map[string]any{}
		for k, v := range ex.Valid[0] {
			ev[k] = v
		}
		ev["correlation_id"] = id
		problem, err := schemas.Validate(schemas.EventID, ev)
		if err != nil {
			t.Fatal(err)
		}
		if (problem == "") != wantValid {
			t.Errorf("correlation_id %q: valid=%v, want %v (%s)", id, problem == "", wantValid, problem)
		}
	}
}

// Appendix C parses as YAML, is OpenAPI 3.1, names its six paths, and every local
// $ref resolves inside the document. Full OpenAPI linting is UNRUN (no linter is
// vendored); this is the structural floor.
func TestOpenAPIAppendixIsWellFormed(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(readSpec(t, "ecosystem-v1/mocks-runs-v1.openapi.yaml"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v", doc["openapi"])
	}
	const wantPaths = 6
	if n := len(doc["paths"].(map[string]any)); n != wantPaths {
		t.Errorf("paths = %d, want %d", n, wantPaths)
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				resolveRef(t, doc, ref)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
}

func resolveRef(t *testing.T, doc map[string]any, ref string) {
	t.Helper()
	if !strings.HasPrefix(ref, "#/") {
		t.Errorf("$ref %q leaves the document", ref)
		return
	}
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Errorf("$ref %q does not resolve at %q", ref, part)
			return
		}
		cur, ok = m[part]
		if !ok {
			t.Errorf("$ref %q does not resolve at %q", ref, part)
			return
		}
	}
}
