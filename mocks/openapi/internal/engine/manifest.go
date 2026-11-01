// Package engine is the mock: the control surface of the mock adapter specification, one HTTP listener per
// scope, and the events, settings and refusals around them. It serves whatever an oas.Document declares.
package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

// Contract is the mock adapter version this engine implements.
const Contract = "mocks.adapter/v1"

// EngineVersion identifies the engine in the manifest's version.
const EngineVersion = "0.1.0"

// Names of the settings every generated mock declares.
const (
	SettingStatusCode = "status_code"
	SettingDelayMS    = "delay_ms"
	SettingFailRate   = "fail_rate"
	// OperationStatusPrefix starts the name of each per-operation status setting.
	OperationStatusPrefix = "operation_status."
)

// Bounds of the settings; each one is declared as a minimum or maximum in the manifest.
const (
	MinStatus      = 0
	MaxStatus      = 599
	MinForceStatus = 200 // statuses below this cannot be forced: a 1xx is not a final answer
	MaxDelayMS     = 60000
	FaultStatus    = 503 // what fail_rate answers
	maxVersionLen  = 64
	maxDescription = 512
	maxFieldName   = 63
)

var (
	namePattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	fieldNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
	controlPattern   = regexp.MustCompile(`^https?://[^\s/?#]+$`)
	camelBoundary    = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	notFieldChars    = regexp.MustCompile(`[^a-z0-9_.-]+`)
)

// Manifest is the mocks.manifest.json document (spec §2).
type Manifest struct {
	Contract    string        `json:"contract"`
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Description string        `json:"description,omitempty"`
	Protocols   []string      `json:"protocols"`
	Control     ControlRef    `json:"control"`
	Settings    []SettingDecl `json:"settings"`
	SafeFields  []string      `json:"safe_fields"`
	Conformance Conformance   `json:"conformance"`
}

// ControlRef locates the control endpoint.
type ControlRef struct {
	URL string `json:"url"`
}

// SettingDecl declares one setting (spec §2.1).
type SettingDecl struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Default     any      `json:"default"`
	Minimum     *float64 `json:"minimum,omitempty"`
	Maximum     *float64 `json:"maximum,omitempty"`
	Scope       string   `json:"scope"`
	Effect      *Effect  `json:"effect,omitempty"`
}

// Effect is the conformance hint of a setting.
type Effect struct {
	Protocol string       `json:"protocol"`
	Value    any          `json:"value"`
	Expect   EffectExpect `json:"expect"`
}

// EffectExpect is what the probe shows once the effect value is set.
type EffectExpect struct {
	HTTPStatus int `json:"http_status"`
}

// Conformance tells the suite how to exercise a listener (spec §2.2).
type Conformance struct {
	Probe ProbeSet `json:"probe"`
}

// ProbeSet holds the probe per protocol; this engine serves HTTP only.
type ProbeSet struct {
	HTTP ProbeHTTP `json:"http"`
}

// ProbeHTTP is one harmless request of the document's own API.
type ProbeHTTP struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   string `json:"body,omitempty"`
}

// Config is everything a mock is told at start.
type Config struct {
	Name       string
	ControlURL string   // the base URL the manifest advertises for the control endpoint
	SafeFields []string // payload fields that may be exported in clear; empty by default
}

// Validate refuses a configuration the manifest could not carry.
func (c Config) Validate() error {
	if !namePattern.MatchString(c.Name) {
		return fmt.Errorf("the mock name %q does not match %s", c.Name, namePattern)
	}
	if !controlPattern.MatchString(c.ControlURL) {
		return fmt.Errorf("the control URL %q is not a scheme, host and port with no path (for example http://127.0.0.1:9100)", c.ControlURL)
	}
	seen := map[string]bool{}
	for _, f := range c.SafeFields {
		switch {
		case !fieldNamePattern.MatchString(f):
			return fmt.Errorf("the safe field %q does not match %s", f, fieldNamePattern)
		case IsSecretName(f):
			return fmt.Errorf("the safe field %q is named like a credential; a credential is never exported in clear", f)
		case seen[f]:
			return fmt.Errorf("the safe field %q is given twice", f)
		}
		seen[f] = true
	}
	return nil
}

// settingName turns an operationId into the setting that overrides its status.
func settingName(id string, taken map[string]bool) string {
	s := strings.ToLower(camelBoundary.ReplaceAllString(id, "${1}_${2}"))
	s = strings.Trim(notFieldChars.ReplaceAllString(s, "_"), "_.-")
	if s == "" {
		s = "operation"
	}
	room := maxFieldName - len(OperationStatusPrefix) - 4 // leaves room for a numeric suffix
	if len(s) > room {
		s = s[:room]
	}
	name := OperationStatusPrefix + s
	for n := 2; taken[name]; n++ {
		name = fmt.Sprintf("%s%s_%d", OperationStatusPrefix, s, n)
	}
	taken[name] = true
	return name
}

func ptr(f float64) *float64 { return &f }

// BuildManifest derives the manifest a mock serves from the document and the configuration. The same inputs give the
// same bytes, so `manifest` and the served manifest are one document.
func BuildManifest(doc *oas.Document, cfg Config) (*Manifest, map[string]string, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	probe, probeOp, err := buildProbe(doc)
	if err != nil {
		return nil, nil, err
	}
	version := doc.Version
	if version == "" {
		version = "0.0.0"
	}
	suffix := "+openapi-mock." + EngineVersion
	if len(version)+len(suffix) > maxVersionLen {
		version = version[:maxVersionLen-len(suffix)]
	}
	m := &Manifest{
		Contract:    Contract,
		Name:        cfg.Name,
		Version:     version + suffix,
		Description: truncate(strings.TrimSpace(doc.Title+" (mock served from its OpenAPI document)"), maxDescription),
		Protocols:   []string{"http"},
		Control:     ControlRef{URL: cfg.ControlURL},
		SafeFields:  append([]string{}, cfg.SafeFields...),
		Conformance: Conformance{Probe: ProbeSet{HTTP: probe}},
	}
	forced503 := &Effect{Protocol: "http", Value: float64(FaultStatus), Expect: EffectExpect{HTTPStatus: FaultStatus}}
	m.Settings = []SettingDecl{
		{
			Name: SettingStatusCode, Type: "integer", Default: float64(0), Minimum: ptr(MinStatus), Maximum: ptr(MaxStatus), Scope: "run",
			Description: "Answer every call that reaches an operation with this HTTP status; 0 turns it off. Values from 1 to 199 are not final statuses and act as 0.",
			Effect:      forced503,
		},
		{
			Name: SettingDelayMS, Type: "integer", Default: float64(0), Minimum: ptr(0), Maximum: ptr(MaxDelayMS), Scope: "run",
			Description: "Wait this many milliseconds before answering any call; 0 turns it off.",
		},
		{
			Name: SettingFailRate, Type: "number", Default: float64(0), Minimum: ptr(0), Maximum: ptr(1), Scope: "run",
			Description: "The share of calls, from 0 to 1, answered 503 and recorded as fault.injected; 0 turns it off.",
			Effect:      &Effect{Protocol: "http", Value: float64(1), Expect: EffectExpect{HTTPStatus: FaultStatus}},
		},
	}
	names := map[string]string{} // operationId → setting name
	taken := map[string]bool{SettingStatusCode: true, SettingDelayMS: true, SettingFailRate: true}
	for _, op := range doc.Ops {
		name := settingName(op.ID, taken)
		names[op.ID] = name
		decl := SettingDecl{
			Name: name, Type: "integer", Default: float64(0), Minimum: ptr(MinStatus), Maximum: ptr(MaxStatus), Scope: "run",
			Description: fmt.Sprintf("Answer %s %s (operationId %s) with this HTTP status; 0 turns it off. It wins over %s.", op.Method, op.Template, op.ID, SettingStatusCode),
		}
		if op == probeOp {
			decl.Effect = forced503
		}
		m.Settings = append(m.Settings, decl)
	}
	return m, names, nil
}

// JSON renders the manifest as the bytes of mocks.manifest.json.
func (m *Manifest) JSON() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		panic(err) // the manifest holds only strings, numbers and slices; encoding cannot fail
	}
	return buf.Bytes()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// decl finds a declared setting by name.
func (m *Manifest) decl(name string) (SettingDecl, bool) {
	for _, s := range m.Settings {
		if s.Name == name {
			return s, true
		}
	}
	return SettingDecl{}, false
}

// ErrNoProbe is returned when a document offers nothing the suite can probe.
var ErrNoProbe = errors.New("the document declares no operation the suite can probe with GET, POST, PUT or DELETE")
