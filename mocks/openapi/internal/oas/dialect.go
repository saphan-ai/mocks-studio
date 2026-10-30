package oas

import (
	"sort"
	"strings"
)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// schemaMapKeywords hold a map of name → schema; schemaKeywords hold one schema; schemaListKeywords a list.
var (
	schemaMapKeywords  = []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"}
	schemaKeywords     = []string{"items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames", "unevaluatedItems", "unevaluatedProperties"}
	schemaListKeywords = []string{"allOf", "anyOf", "oneOf", "prefixItems"}
)

// eachSchema calls fn on every schema object of the document that is reachable by the structure of OpenAPI:
// components.schemas and every `schema` of a parameter, header or media type.
func eachSchema(root any, fn func(map[string]any)) {
	m, ok := root.(map[string]any)
	if !ok {
		return
	}
	if comps, ok := m["components"].(map[string]any); ok {
		if schemas, ok := comps["schemas"].(map[string]any); ok {
			for _, k := range sortedKeys(schemas) {
				walkSchema(schemas[k], fn)
			}
		}
	}
	visitStructure(root, fn, false)
}

func visitStructure(n any, fn func(map[string]any), inComponents bool) {
	switch node := n.(type) {
	case map[string]any:
		for _, k := range sortedKeys(node) {
			v := node[k]
			switch {
			case dataKeys[k]:
			case k == "examples":
			case k == "schema":
				walkSchema(v, fn)
			case k == "schemas" && inComponents:
			default:
				visitStructure(v, fn, k == "components")
			}
		}
	case []any:
		for _, v := range node {
			visitStructure(v, fn, false)
		}
	}
}

func walkSchema(n any, fn func(map[string]any)) {
	m, ok := n.(map[string]any)
	if !ok {
		return
	}
	fn(m)
	for _, kw := range schemaMapKeywords {
		if sub, ok := m[kw].(map[string]any); ok {
			for _, k := range sortedKeys(sub) {
				walkSchema(sub[k], fn)
			}
		}
	}
	for _, kw := range schemaKeywords {
		walkSchema(m[kw], fn)
	}
	for _, kw := range schemaListKeywords {
		if list, ok := m[kw].([]any); ok {
			for _, s := range list {
				walkSchema(s, fn)
			}
		}
	}
	if list, ok := m["items"].([]any); ok { // OpenAPI 3.0 never has it, a 3.1 author may
		for _, s := range list {
			walkSchema(s, fn)
		}
	}
}

// toDraft2020 rewrites the OpenAPI 3.0 schema dialect (nullable, boolean exclusive bounds) in place into
// the JSON Schema 2020-12 vocabulary the validator speaks. OpenAPI 3.1 documents already use it.
func toDraft2020(m map[string]any) {
	if nullable, ok := m["nullable"].(bool); ok {
		delete(m, "nullable")
		if nullable {
			allowNull(m)
		}
	}
	for _, bound := range [][2]string{{"minimum", "exclusiveMinimum"}, {"maximum", "exclusiveMaximum"}} {
		flag, isBool := m[bound[1]].(bool)
		if !isBool {
			continue
		}
		delete(m, bound[1])
		if v, has := m[bound[0]]; flag && has {
			delete(m, bound[0])
			m[bound[1]] = v
		}
	}
}

func allowNull(m map[string]any) {
	switch t := m["type"].(type) {
	case string:
		m["type"] = []any{t, "null"}
	case []any:
		if !containsValue(t, "null") {
			m["type"] = append(t, "null")
		}
	}
	if enum, ok := m["enum"].([]any); ok && !containsValue(enum, nil) {
		m["enum"] = append(enum, nil)
	}
}

func containsValue(list []any, v any) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// schemaType is the first non-null type a schema declares, or "" when it declares none.
func schemaType(m map[string]any) string {
	switch t := m["type"].(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// mediaIsJSON reports whether a media type carries JSON.
func mediaIsJSON(mt string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mt, ";", 2)[0]))
	return base == "application/json" || strings.HasSuffix(base, "+json") || base == "*/*"
}
