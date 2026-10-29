// Package suite is the mock adapter conformance suite: given a mock's manifest or
// control URL, it checks the running mock against every rule of
// spec/MOCK_ADAPTER_V1.md and prints one line per check.
package suite

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"example.com/mocks-studio/conformance/internal/schemas"
)

// Run checks one mock and returns the runner's exit code.
func Run(ctx context.Context, o Options) int {
	o = withDefaults(o)
	rep := newReport(o.Out)
	e := newEnv(ctx, o, rep)
	code := e.run()
	rep.finish(code)
	return code
}

func withDefaults(o Options) Options {
	if o.ListenHost == "" {
		o.ListenHost = DefaultListenHost
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.EventWait <= 0 {
		o.EventWait = DefaultEventWait
	}
	return o
}

func (e *env) run() int {
	raw, code := e.loadManifest()
	if code != ExitPass {
		return code
	}
	if code := e.checkManifest(raw); code != ExitPass {
		return code
	}
	if !e.reachable() {
		return ExitUnreachable
	}
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	e.runChecks(ctx, raw)
	if e.rep.failed() {
		return ExitFail
	}
	return ExitPass
}

// loadManifest reads the manifest from its file, or from the control endpoint when no
// file was given. It exits 3 only when the control endpoint was needed and silent.
func (e *env) loadManifest() ([]byte, int) {
	if e.opts.ManifestPath == "" && e.opts.ControlURL == "" {
		e.rep.record("manifest-valid", fail("neither --manifest nor --control was given, so there is no mock to check; pass one of them"))
		return nil, ExitRefused
	}
	e.control = strings.TrimRight(e.opts.ControlURL, "/")
	if e.opts.ManifestPath != "" {
		raw, err := os.ReadFile(e.opts.ManifestPath)
		if err != nil {
			e.rep.record("manifest-valid", fail("the manifest file %s cannot be read (%v); pass an existing file", e.opts.ManifestPath, err))
			return nil, ExitRefused
		}
		return raw, ExitPass
	}
	r := e.call(http.MethodGet, controlBase+"/manifest", nil)
	if r.err != nil {
		e.rep.record("control-reachable", fail("GET %s%s/manifest got no answer (%v); start the mock or fix the URL", e.control, controlBase, r.err))
		return nil, ExitUnreachable
	}
	if r.status != http.StatusOK {
		e.rep.record("manifest-valid", fail("GET %s/manifest answered %d, not 200; the manifest could not be read", controlBase, r.status))
		return nil, ExitRefused
	}
	return r.body, ExitPass
}

// checkManifest runs contract-known then manifest-valid; a failure of either refuses the mock.
func (e *env) checkManifest(raw []byte) int {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		e.rep.record("manifest-valid", fail("the manifest is not a JSON object (%v); publish mocks.manifest.json as one", err))
		return ExitRefused
	}
	if contract, ok := doc["contract"].(string); ok && !contains(SupportedContracts, contract) {
		e.rep.record("contract-known", fail("the manifest declares contract %s, which this suite does not know; supported versions: %s (refusal contract-unknown)", contract, joinList(SupportedContracts)))
		return ExitRefused
	} else if ok {
		e.rep.record("contract-known", pass("contract %s is a supported version (%s)", contract, joinList(SupportedContracts)))
	}
	problem, err := schemas.Validate(schemas.ManifestID, doc)
	if err != nil {
		e.rep.record("manifest-valid", fail("the suite's own manifest schema failed to load: %v", err))
		return ExitRefused
	}
	if problem != "" {
		e.rep.record("manifest-valid", fail("the manifest breaks mocks-manifest-v1.schema.json: %s", problem))
		return ExitRefused
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		e.rep.record("manifest-valid", fail("the manifest does not decode (%v)", err))
		return ExitRefused
	}
	if problems := m.semanticProblems(); len(problems) > 0 {
		e.rep.record("manifest-valid", fail("the manifest is inconsistent: %s", strings.Join(problems, "; ")))
		return ExitRefused
	}
	if len(m.drivableProtocols()) == 0 {
		e.rep.record("manifest-valid", fail("the manifest declares protocols %s, none of which this suite can drive (it drives %s); the mock cannot be checked", joinList(m.Protocols), joinList(drivable)))
		return ExitRefused
	}
	e.rep.record("manifest-valid", pass("the manifest of %s %s validates and is consistent", m.Name, m.Version))
	e.manifest = &m
	if e.control == "" {
		e.control = strings.TrimRight(m.Control.URL, "/")
	}
	return ExitPass
}

// reachable dials the control endpoint once; a silent endpoint is exit 3, not a failed check.
func (e *env) reachable() bool {
	r := e.call(http.MethodGet, controlBase+"/health", nil)
	if r.err != nil {
		e.rep.record("control-reachable", fail("GET %s%s/health got no answer (%v); start the mock or fix control.url", e.control, controlBase, r.err))
		return false
	}
	e.rep.record("control-reachable", pass("the control endpoint %s answered", e.control))
	return true
}

func (e *env) runChecks(ctx context.Context, raw []byte) {
	store, info, err := openStream(ctx, e.control+controlBase+"/events", e.opts.RequestTimeout)
	e.events = store
	e.record("health-answers", e.checkHealth())
	e.record("control-manifest-served", e.checkManifestServed(raw))
	e.record("events-stream-opens", checkStreamOpens(info, err))
	for _, p := range e.manifest.drivableProtocols() {
		e.runScoped(p)
	}
	e.runGlobal("scope-rejects-unsupported-protocol", []string{needScope}, e.checkUnsupportedProtocol)
}

func (e *env) record(id string, o outcome) { e.rep.record(id, o) }

func checkStreamOpens(info streamInfo, err error) outcome {
	if err != nil {
		return fail("GET /_mock/v1/events could not be opened (%v); serve the event stream", err)
	}
	if info.status != http.StatusOK || !strings.HasPrefix(info.contentType, "text/event-stream") {
		return fail("GET /_mock/v1/events answered %d with content type %q, want 200 and text/event-stream", info.status, info.contentType)
	}
	return pass("the event stream opened as text/event-stream")
}

func (e *env) checkHealth() outcome {
	r := e.call(http.MethodGet, controlBase+"/health", nil)
	if r.err != nil || r.status != http.StatusOK {
		return fail("GET /_mock/v1/health: %s, want 200", describeReply(r))
	}
	return pass("GET /_mock/v1/health answered 200")
}

func (e *env) checkManifestServed(fileRaw []byte) outcome {
	r := e.call(http.MethodGet, controlBase+"/manifest", nil)
	if r.err != nil || r.status != http.StatusOK {
		return fail("GET /_mock/v1/manifest: %s, want 200", describeReply(r))
	}
	if e.opts.ManifestPath == "" {
		return pass("GET /_mock/v1/manifest answered 200 (it is the manifest under test)")
	}
	var served, file any
	if err := json.Unmarshal(r.body, &served); err != nil {
		return fail("GET /_mock/v1/manifest did not return JSON (%v)", err)
	}
	if err := json.Unmarshal(fileRaw, &file); err != nil {
		return fail("the manifest file does not decode (%v)", err)
	}
	if !jsonEqual(served, file) {
		return fail("the manifest served at /_mock/v1/manifest differs from the manifest file %s; publish one manifest", e.opts.ManifestPath)
	}
	return pass("the served manifest equals the manifest file")
}

func jsonEqual(a, b any) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ab) == string(bb)
}

// deadlineAfter returns the instant d from now.
func deadlineAfter(d time.Duration) time.Time { return time.Now().Add(d) }
