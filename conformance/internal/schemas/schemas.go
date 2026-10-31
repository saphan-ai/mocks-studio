// Package schemas embeds the JSON Schemas the conformance runner validates against.
//
// The files in this directory are copies of spec/schemas/*.json and
// spec/ecosystem-v1/mocks-event-v1.schema.json (go:embed cannot reach outside the
// module). TestEmbeddedSchemasMatchSpec fails when a copy drifts from its original.
package schemas

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed mocks-event-v1.schema.json
var eventSchema []byte

//go:embed mocks-manifest-v1.schema.json
var manifestSchema []byte

//go:embed mocks-refusal-v1.schema.json
var refusalSchema []byte

// Embedded returns the raw bytes of each embedded schema, keyed by file name.
func Embedded() map[string][]byte {
	return map[string][]byte{
		"mocks-event-v1.schema.json":    eventSchema,
		"mocks-manifest-v1.schema.json": manifestSchema,
		"mocks-refusal-v1.schema.json":  refusalSchema,
	}
}

// Schema ids, which are also the resource names the schemas are compiled under.
const (
	EventID    = "mocks.event/v1"
	ManifestID = "mocks.manifest/v1"
	RefusalID  = "mocks.refusal/v1"
)

var (
	once     sync.Once
	compiled map[string]*jsonschema.Schema
	initErr  error
)

func compileAll() {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	// The published bytes compile as they are: Appendix A states the non-zero trace-id rule
	// as a 32-hex pattern plus `not` const all-zero, which RE2 accepts (W0 amended 2026-10-29).
	sources := map[string][]byte{EventID: eventSchema, ManifestID: manifestSchema, RefusalID: refusalSchema}
	for id, raw := range sources {
		if err := c.AddResource(id, bytes.NewReader(raw)); err != nil {
			initErr = fmt.Errorf("embedded schema %s does not load: %w", id, err)
			return
		}
	}
	compiled = map[string]*jsonschema.Schema{}
	for id := range sources {
		s, err := c.Compile(id)
		if err != nil {
			initErr = fmt.Errorf("embedded schema %s does not compile: %w", id, err)
			return
		}
		compiled[id] = s
	}
}

// Validate checks one decoded JSON value against the schema with the given id and
// returns a one-line description of the first problems, or "" when the value is valid.
func Validate(id string, value any) (string, error) {
	once.Do(compileAll)
	if initErr != nil {
		return "", initErr
	}
	s, ok := compiled[id]
	if !ok {
		return "", fmt.Errorf("no embedded schema named %s", id)
	}
	err := s.Validate(value)
	if err == nil {
		return "", nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err.Error(), nil
	}
	return describe(ve), nil
}

// maxProblems bounds how many leaf problems one description names.
const maxProblems = 3

func describe(ve *jsonschema.ValidationError) string {
	var leaves []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := e.InstanceLocation
			if loc == "" {
				loc = "(document)"
			}
			leaves = append(leaves, loc+": "+e.Message)
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(leaves) > maxProblems {
		leaves = leaves[:maxProblems]
	}
	return strings.Join(leaves, "; ")
}
