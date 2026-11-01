package s3mock

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// DefaultControlURL is the control URL of the manifest file shipped beside the mock.
const DefaultControlURL = "http://127.0.0.1:9100"

// Setting names.
const (
	SettingGetLatency    = "get_latency_ms"
	SettingSlowDown      = "slow_down"
	SettingErrorStatus   = "error_status"
	SettingMissingPrefix = "missing_key_prefix"
	SettingMinPartBytes  = "min_part_bytes"
	SettingAccessKey     = "access_key_id"
	SettingSecretKey     = "secret_access_key"
)

// Fictional fixed credentials; never real, they sign nothing outside this mock.
const (
	DefaultAccessKey = "AKIAEXAMPLEKEY000001"
	DefaultSecretKey = "exampleSecretAccessKey0000000000EXAMPLEKEY"
)

// settingDef is the one declaration of a setting: the manifest is rendered from it
// and the control surface validates against it, so the two cannot disagree.
type settingDef struct {
	name, typ, desc string
	def             any
	min, max        *float64
	allowed         []any
	effect          map[string]any
}

func fptr(f float64) *float64 { return &f }

var settingDefs = []settingDef{
	{name: SettingGetLatency, typ: "integer", def: 0, min: fptr(0), max: fptr(60000),
		desc: "Milliseconds every GetObject waits before it answers (a slow read)."},
	{name: SettingSlowDown, typ: "boolean", def: false,
		desc:   "Answer every request 503 SlowDown, S3's throttling error.",
		effect: map[string]any{"protocol": "http", "value": true, "expect": map[string]any{"http_status": 503}}},
	{name: SettingErrorStatus, typ: "integer", def: 0, allowed: []any{0, 400, 403, 404, 409, 500, 501, 503},
		desc:   "Answer every request with this status and a matching S3 XML error; 0 is off.",
		effect: map[string]any{"protocol": "http", "value": 500, "expect": map[string]any{"http_status": 500}}},
	{name: SettingMissingPrefix, typ: "string", def: "",
		desc: "Object keys that start with this prefix answer NoSuchKey on GET and HEAD, whatever is stored; empty is off."},
	{name: SettingMinPartBytes, typ: "integer", def: 5242880, min: fptr(0), max: fptr(5368709120),
		desc: "Smallest size of a multipart part that is not the last; S3's own rule is 5242880."},
	{name: SettingAccessKey, typ: "string", def: DefaultAccessKey,
		desc: "The access key id the scope's requests must be signed with (fictional default)."},
	{name: SettingSecretKey, typ: "string", def: DefaultSecretKey,
		desc: "The secret access key the scope verifies signatures with (fictional default). Never logged, never in an event."},
}

// SafeFields are the payload fields the mock exports in clear.
var SafeFields = []string{"bucket", "key", "operation"}

// ManifestBytes renders mocks.manifest.json for a control URL.
func ManifestBytes(controlURL string) []byte {
	settings := make([]any, 0, len(settingDefs))
	for _, d := range settingDefs {
		s := map[string]any{"name": d.name, "type": d.typ, "default": d.def, "scope": "run", "description": d.desc}
		if d.min != nil {
			s["minimum"] = *d.min
		}
		if d.max != nil {
			s["maximum"] = *d.max
		}
		if d.allowed != nil {
			s["allowed"] = d.allowed
		}
		if d.effect != nil {
			s["effect"] = d.effect
		}
		settings = append(settings, s)
	}
	doc := map[string]any{
		"contract":    contractV1,
		"name":        mockName,
		"version":     mockVersion,
		"description": "A free S3-compatible object store: buckets, objects, multipart, presigned URLs, SigV4; slow reads, SlowDown and missing keys as scenarios.",
		"protocols":   []string{"http"},
		"control":     map[string]any{"url": controlURL},
		"settings":    settings,
		"safe_fields": SafeFields,
		"conformance": map[string]any{"probe": map[string]any{"http": map[string]any{"method": "GET", "path": "/"}}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // a map of plain values always marshals
	}
	return append(b, '\n')
}

func defByName(name string) (settingDef, bool) {
	for _, d := range settingDefs {
		if d.name == name {
			return d, true
		}
	}
	return settingDef{}, false
}

func settingNames() []string {
	out := make([]string, len(settingDefs))
	for i, d := range settingDefs {
		out[i] = d.name
	}
	sort.Strings(out)
	return out
}

// toJSONValue normalises Go ints to float64, the type a decoded JSON number has.
func toJSONValue(v any) any {
	if i, ok := v.(int); ok {
		return float64(i)
	}
	return v
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "string":
		_, ok := v.(string)
		return ok
	default:
		_, ok := v.(bool)
		return ok
	}
}

// check validates one value; code is empty when it is acceptable.
func (d settingDef) check(v any) (code, msg string, allowed []string) {
	if !typeMatches(d.typ, v) {
		return "setting-type-invalid", fmt.Sprintf("The value of %q is not of type %s and the request was refused; send a %s.", d.name, d.typ, d.typ), nil
	}
	if f, ok := v.(float64); ok && d.min != nil && d.max != nil && (f < *d.min || f > *d.max) {
		return "setting-out-of-range", fmt.Sprintf("The value %v of %q is outside %v..%v and the request was refused; send a value inside the range.", f, d.name, *d.min, *d.max), nil
	}
	if len(d.allowed) > 0 {
		list := make([]string, len(d.allowed))
		hit := false
		for i, a := range d.allowed {
			list[i] = fmt.Sprint(a)
			hit = hit || toJSONValue(a) == v
		}
		if !hit {
			return "setting-out-of-range", fmt.Sprintf("The value %v of %q is not an allowed value and the request was refused; use one of the allowed values.", v, d.name), list
		}
	}
	return "", "", nil
}
