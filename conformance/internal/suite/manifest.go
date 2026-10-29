package suite

import (
	"fmt"
	"strings"
)

// SupportedContracts lists the adapter contract versions this suite checks.
var SupportedContracts = []string{"mocks.adapter/v1"}

// Manifest is the typed view of a manifest that already passed its JSON Schema.
type Manifest struct {
	Contract  string   `json:"contract"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Protocols []string `json:"protocols"`
	Control   struct {
		URL string `json:"url"`
	} `json:"control"`
	Settings        []Setting `json:"settings"`
	SafeFields      []string  `json:"safe_fields"`
	CorrelationKeys []string  `json:"correlation_keys"`
	EventKinds      []struct {
		Kind string `json:"kind"`
	} `json:"event_kinds"`
	Callouts []struct {
		Name          string `json:"name"`
		TargetSetting string `json:"target_setting"`
	} `json:"callouts"`
	Conformance struct {
		Probe struct {
			HTTP *HTTPProbe `json:"http"`
			TCP  *TCPProbe  `json:"tcp"`
		} `json:"probe"`
	} `json:"conformance"`
}

// HTTPProbe is the request the suite sends to an http listener.
type HTTPProbe struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body"`
}

// TCPProbe is the line the suite sends to a tcp listener.
type TCPProbe struct {
	Send string `json:"send"`
}

// Setting is one declared setting.
type Setting struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Default any      `json:"default"`
	Allowed []any    `json:"allowed"`
	Minimum *float64 `json:"minimum"`
	Maximum *float64 `json:"maximum"`
	Scope   string   `json:"scope"`
	Effect  *Effect  `json:"effect"`
}

// Effect is a conformance hint: Value, once set, makes the next probe satisfy Expect.
type Effect struct {
	Protocol string `json:"protocol"`
	Value    any    `json:"value"`
	Expect   struct {
		HTTPStatus int    `json:"http_status"`
		TCPReply   string `json:"tcp_reply"`
	} `json:"expect"`
}

// PerRun reports whether the setting applies per run (the default).
func (s Setting) PerRun() bool { return s.Scope == "" || s.Scope == "run" }

// drivable lists the protocols this runner can drive a listener for.
var drivable = []string{"http", "tcp"}

func (m *Manifest) drivableProtocols() []string {
	var out []string
	for _, p := range m.Protocols {
		for _, d := range drivable {
			if p == d {
				out = append(out, p)
			}
		}
	}
	return out
}

// semanticProblems are the manifest rules JSON Schema cannot state.
func (m *Manifest) semanticProblems() []string {
	var problems []string
	seen := map[string]bool{}
	byName := map[string]Setting{}
	for _, s := range m.Settings {
		if seen[s.Name] {
			problems = append(problems, fmt.Sprintf("setting %q is declared twice", s.Name))
		}
		seen[s.Name] = true
		byName[s.Name] = s
		problems = append(problems, settingProblems(s, m.Protocols)...)
	}
	for _, c := range m.Callouts {
		t, ok := byName[c.TargetSetting]
		if !ok || t.Type != "string" {
			problems = append(problems, fmt.Sprintf("call-out %q names target_setting %q, which is not a declared string setting", c.Name, c.TargetSetting))
		}
	}
	kinds := map[string]bool{}
	for _, k := range m.EventKinds {
		if kinds[k.Kind] {
			problems = append(problems, fmt.Sprintf("event kind %q is declared twice", k.Kind))
		}
		kinds[k.Kind] = true
	}
	return problems
}

func settingProblems(s Setting, protocols []string) []string {
	var out []string
	if !valueFitsType(s.Type, s.Default) {
		out = append(out, fmt.Sprintf("setting %q: default %v is not of type %s", s.Name, s.Default, s.Type))
	}
	if (s.Minimum != nil || s.Maximum != nil) && s.Type != "integer" && s.Type != "number" {
		out = append(out, fmt.Sprintf("setting %q: minimum/maximum apply to numbers, not %s", s.Name, s.Type))
	}
	if s.Minimum != nil && s.Maximum != nil && *s.Minimum > *s.Maximum {
		out = append(out, fmt.Sprintf("setting %q: minimum %v is above maximum %v", s.Name, *s.Minimum, *s.Maximum))
	}
	if why := s.violation(s.Default); why != "" {
		out = append(out, fmt.Sprintf("setting %q: default %v %s", s.Name, s.Default, why))
	}
	if s.Effect != nil {
		if !contains(protocols, s.Effect.Protocol) {
			out = append(out, fmt.Sprintf("setting %q: effect names protocol %s, which the mock does not declare", s.Name, s.Effect.Protocol))
		}
		if !valueFitsType(s.Type, s.Effect.Value) || s.violation(s.Effect.Value) != "" {
			out = append(out, fmt.Sprintf("setting %q: effect value %v is not a valid value of the setting", s.Name, s.Effect.Value))
		}
	}
	return out
}

// violation says why v is outside the setting's allowed values or range, or "".
func (s Setting) violation(v any) string {
	if len(s.Allowed) > 0 {
		for _, a := range s.Allowed {
			if fmt.Sprint(a) == fmt.Sprint(v) {
				return ""
			}
		}
		return "is not among the allowed values"
	}
	if f, ok := v.(float64); ok {
		if s.Minimum != nil && f < *s.Minimum || s.Maximum != nil && f > *s.Maximum {
			return "is outside the declared range"
		}
	}
	return ""
}

func valueFitsType(typ string, v any) bool {
	switch typ {
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := v.(float64)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return false
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func joinList(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	return strings.Join(items, ", ")
}
