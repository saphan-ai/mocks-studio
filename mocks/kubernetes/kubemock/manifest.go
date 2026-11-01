package kubemock

import (
	"encoding/json"
	"math"
)

// Names of the declared settings.
const (
	SettingLatencyMS           = "latency_ms"
	SettingErrorStatus         = "error_status"
	SettingThrottle            = "throttle"
	SettingDropWatchAfterEvent = "drop_watch_after_events"
)

const (
	maxLatencyMS      = 60000
	maxDropAfter      = 1000000
	minForcedStatus   = 400
	maxForcedStatus   = 599
	throttleRetrySecs = 1
	effectErrorStatus = 503
	probePath         = "/api/v1/namespaces"
)

// settingDef is one declared setting: the manifest entry and the rules the
// control surface enforces come from this one table, so the mock cannot declare
// one thing and enforce another.
type settingDef struct {
	name        string
	typ         string
	def         any
	min, max    *float64
	description string
	effect      map[string]any // nil when the suite cannot observe the setting through a status
}

func fptr(v float64) *float64 { return &v }

var settingDefs = []settingDef{
	{
		name: SettingErrorStatus, typ: "integer", def: 0, min: fptr(0), max: fptr(maxForcedStatus),
		description: "Forced HTTP status for every call on the listener; 0 is off, otherwise 400 to 599.",
		effect:      map[string]any{"protocol": "http", "value": effectErrorStatus, "expect": map[string]any{"http_status": effectErrorStatus}},
	},
	{
		name: SettingThrottle, typ: "boolean", def: false,
		description: "When true every call is answered 429 with a Retry-After header.",
		effect:      map[string]any{"protocol": "http", "value": true, "expect": map[string]any{"http_status": 429}},
	},
	{
		name: SettingLatencyMS, typ: "integer", def: 0, min: fptr(0), max: fptr(maxLatencyMS),
		description: "Milliseconds every call waits before it is answered; 0 is off.",
	},
	{
		name: SettingDropWatchAfterEvent, typ: "integer", def: 0, min: fptr(0), max: fptr(maxDropAfter),
		description: "A watch opened while this is N ends its stream after N events; 0 means never.",
	},
}

func lookupSetting(name string) (settingDef, bool) {
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
	return out
}

// check validates one value against the declared setting; code is empty when it fits.
func (d settingDef) check(v any) (code, msg string) {
	if !typeMatches(d.typ, v) {
		return "setting-type-invalid", "The value of \"" + d.name + "\" is not of type " + d.typ + " and the request was refused; send a " + d.typ + "."
	}
	if f, ok := v.(float64); ok {
		if (d.min != nil && f < *d.min) || (d.max != nil && f > *d.max) {
			return "setting-out-of-range", "The value of \"" + d.name + "\" is outside its declared range and the request was refused; send a value inside the range."
		}
		if d.name == SettingErrorStatus && f != 0 && f < minForcedStatus {
			return "setting-out-of-range", "The value of \"error_status\" is neither 0 nor a status from 400 to 599 and the request was refused; send 0 or a status from 400 to 599."
		}
	}
	return "", ""
}

func typeMatches(typ string, v any) bool {
	switch typ {
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "number":
		_, ok := v.(float64)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	default:
		_, ok := v.(bool)
		return ok
	}
}

// ManifestBytes renders the manifest this mock publishes for a control URL.
func ManifestBytes(controlURL string) []byte {
	settings := make([]any, 0, len(settingDefs))
	for _, d := range settingDefs {
		s := map[string]any{"name": d.name, "type": d.typ, "default": d.def, "scope": "run", "description": d.description}
		if d.min != nil {
			s["minimum"] = *d.min
		}
		if d.max != nil {
			s["maximum"] = *d.max
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
		"description": "A slice of the Kubernetes API: namespaces, pods, configmaps and deployments, with watch and scenario settings.",
		"protocols":   []string{"http"},
		"control":     map[string]any{"url": controlURL},
		"settings":    settings,
		"safe_fields": []string{"kind", "namespace", "name"},
		"conformance": map[string]any{"probe": map[string]any{"http": map[string]any{"method": "GET", "path": probePath}}},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // a map of plain values always marshals
	}
	return append(b, '\n')
}

// Manifest is the manifest this running mock serves.
func (m *Mock) Manifest() []byte { return ManifestBytes(m.controlURL) }
