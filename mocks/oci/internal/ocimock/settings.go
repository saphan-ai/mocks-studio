package ocimock

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Setting names, the failure injections a test can switch on per run.
const (
	settingAuthRequired = "auth_required"
	settingRateLimit    = "rate_limit"
	settingMissingBlob  = "missing_blob"
)

const maxRateLimit = 10000

// settingDef is the one declaration of a setting: the manifest is rendered from it and
// the control surface validates against it, so the mock cannot declare one thing and
// enforce another.
type settingDef struct {
	name, typ, description string
	def                    any
	min, max               *float64
	effect                 map[string]any
}

func ptr(f float64) *float64 { return &f }

var settingDefs = []settingDef{
	{
		name: settingAuthRequired, typ: "boolean", def: false,
		description: "When true, every /v2/ call without a bearer token from the listener's /token endpoint answers 401 with a WWW-Authenticate challenge.",
		effect:      map[string]any{"protocol": "http", "value": true, "expect": map[string]any{"http_status": 401}},
	},
	{
		name: settingRateLimit, typ: "integer", def: 0, min: ptr(0), max: ptr(maxRateLimit),
		description: "Requests per rolling 60 seconds the /v2/ API answers before it answers 429; 0 is no limit. Every /v2/ request counts, whatever the limit was when it arrived.",
		effect:      map[string]any{"protocol": "http", "value": 1, "expect": map[string]any{"http_status": 429}},
	},
	{
		name: settingMissingBlob, typ: "string", def: "",
		description: "Comma-separated sha256 digests whose blob answers 404 BLOB_UNKNOWN, stored or not; empty is none.",
	},
}

func (d settingDef) manifestEntry() map[string]any {
	e := map[string]any{"name": d.name, "type": d.typ, "default": d.def, "scope": "run", "description": d.description}
	if d.min != nil {
		e["minimum"] = *d.min
	}
	if d.max != nil {
		e["maximum"] = *d.max
	}
	if d.effect != nil {
		e["effect"] = d.effect
	}
	return e
}

// normalised is the value as a decoded JSON document holds it (numbers are float64).
func (d settingDef) normalised() any {
	if i, ok := d.def.(int); ok {
		return float64(i)
	}
	return d.def
}

func declaredSettings() map[string]settingDef {
	out := make(map[string]settingDef, len(settingDefs))
	for _, d := range settingDefs {
		out[d.name] = d
	}
	return out
}

func declaredNames() []string {
	names := make([]string, 0, len(settingDefs))
	for _, d := range settingDefs {
		names = append(names, d.name)
	}
	sort.Strings(names)
	return names
}

// check validates one value; it returns a refusal reason code and message, or "".
func (d settingDef) check(v any) (code, msg string) {
	switch d.typ {
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "setting-type-invalid", fmt.Sprintf("The value of %q is not a boolean and the request was refused; send true or false.", d.name)
		}
	case "integer":
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) {
			return "setting-type-invalid", fmt.Sprintf("The value of %q is not an integer and the request was refused; send a whole number.", d.name)
		}
		if f < *d.min || f > *d.max {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is outside %v..%v and the request was refused; send a value inside the range.", f, d.name, *d.min, *d.max)
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return "setting-type-invalid", fmt.Sprintf("The value of %q is not a string and the request was refused; send a string.", d.name)
		}
		if d.name == settingMissingBlob {
			for _, dg := range splitDigests(s) {
				if _, ok := parseDigest(dg); !ok {
					return "setting-out-of-range", fmt.Sprintf("The entry %q of %q is not a sha256 digest and the request was refused; send comma-separated sha256:<64 hex> digests.", dg, d.name)
				}
			}
		}
	}
	return "", ""
}

func splitDigests(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ManifestBytes renders the manifest for a control URL.
func ManifestBytes(controlURL string) []byte {
	settings := make([]any, 0, len(settingDefs))
	for _, d := range settingDefs {
		settings = append(settings, d.manifestEntry())
	}
	doc := map[string]any{
		"contract":    contractV1,
		"name":        mockName,
		"version":     mockVersion,
		"description": "A free OCI Distribution v2 registry: push, pull, manifests, tags and blob uploads, with auth, a rate limit and missing blobs as scenario settings.",
		"protocols":   []string{"http"},
		"control":     map[string]any{"url": controlURL},
		"settings":    settings,
		"safe_fields": []string{"digest", "operation", "reference", "repository"},
		"conformance": map[string]any{"probe": map[string]any{"http": map[string]any{"method": "GET", "path": "/v2/"}}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // a map of plain values always marshals
	}
	return append(b, '\n')
}
