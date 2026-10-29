package suite

import (
	"fmt"
	"net/http"
)

func (e *env) settingsPath(h *scopeHandle) string {
	return controlBase + "/scopes/" + h.runID + "/settings"
}

func (e *env) putSettings(h *scopeHandle, body map[string]any) reply {
	return e.call(http.MethodPut, e.settingsPath(h), body)
}

func (e *env) checkUnknownSetting(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	const unknown = "conformance-no-such-setting"
	r := e.putSettings(h, map[string]any{unknown: 1})
	doc, problem := refusalProblem(r, http.StatusUnprocessableEntity, "setting-unknown")
	if problem != "" {
		return fail("setting %q, which the manifest does not declare: %s", unknown, problem)
	}
	var declared []string
	for _, s := range e.manifest.Settings {
		declared = append(declared, s.Name)
	}
	if got := stringList(doc["allowed"]); !sameSet(got, declared) {
		return fail("the refusal's allowed list is [%s], want the declared setting names [%s]", joinList(got), joinList(declared))
	}
	return pass("unknown setting %q was refused with 422 setting-unknown and the allowed names [%s]", unknown, joinList(declared))
}

// firstPerRun returns the first per-run setting satisfying want.
func (e *env) firstPerRun(want func(Setting) bool) (Setting, bool) {
	for _, s := range e.manifest.Settings {
		if s.PerRun() && want(s) {
			return s, true
		}
	}
	return Setting{}, false
}

// outOfRangeValue is a value of the setting's type outside its allowed values or range.
func outOfRangeValue(s Setting) (any, bool) {
	switch {
	case s.Maximum != nil:
		return *s.Maximum + 1, true
	case s.Minimum != nil:
		return *s.Minimum - 1, true
	case len(s.Allowed) > 0 && s.Type == "string":
		return "conformance-not-allowed", true
	case len(s.Allowed) > 0 && (s.Type == "integer" || s.Type == "number"):
		top := 0.0
		for _, a := range s.Allowed {
			if f, ok := a.(float64); ok && f > top {
				top = f
			}
		}
		return top + 1, true
	}
	return nil, false
}

func (e *env) checkOutOfRange(protocol string) outcome {
	s, ok := e.firstPerRun(func(s Setting) bool { _, ok := outOfRangeValue(s); return ok })
	if !ok {
		return notApplicable("no per-run setting declares a range or a list of allowed values")
	}
	value, _ := outOfRangeValue(s)
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	doc, problem := refusalProblem(e.putSettings(h, map[string]any{s.Name: value}), http.StatusUnprocessableEntity, "setting-out-of-range")
	if problem != "" {
		return fail("setting %s=%v, outside what the manifest allows: %s", s.Name, value, problem)
	}
	if len(s.Allowed) > 0 {
		var want []string
		for _, a := range s.Allowed {
			want = append(want, fmt.Sprint(a))
		}
		if got := stringList(doc["allowed"]); !sameSet(got, want) {
			return fail("the refusal's allowed list is [%s], want the declared values [%s]", joinList(got), joinList(want))
		}
	}
	return pass("%s=%v was refused with 422 setting-out-of-range", s.Name, value)
}

func (e *env) checkWrongType(protocol string) outcome {
	s, ok := e.firstPerRun(func(Setting) bool { return true })
	if !ok {
		return notApplicable("the mock declares no per-run setting")
	}
	var value any = "conformance-wrong-type"
	if s.Type == "string" {
		value = 12345
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	if _, problem := refusalProblem(e.putSettings(h, map[string]any{s.Name: value}), http.StatusUnprocessableEntity, "setting-type-invalid"); problem != "" {
		return fail("setting %s=%v, not of type %s: %s", s.Name, value, s.Type, problem)
	}
	return pass("%s=%v was refused with 422 setting-type-invalid", s.Name, value)
}

// effectSetting is the per-run setting that declares an observable effect on protocol.
func (e *env) effectSetting(protocol string) (Setting, bool) {
	return e.firstPerRun(func(s Setting) bool { return s.Effect != nil && s.Effect.Protocol == protocol })
}

// applyEffect sets the effect value and checks the next probe shows it. It returns
// the baseline description for later comparison, or an outcome when something failed.
func (e *env) applyEffect(h *scopeHandle, s Setting) *outcome {
	before := e.probe(h, probeOpts{})
	if hit, saw := before.satisfies(s.Effect); hit {
		o := fail("before %s was set, the probe already showed the effect (%s); the manifest's effect must differ from the default behaviour", s.Name, saw)
		return &o
	}
	r := e.putSettings(h, map[string]any{s.Name: s.Effect.Value})
	if r.err != nil || r.status != http.StatusOK {
		o := fail("PUT settings %s=%v: %s, want 200", s.Name, s.Effect.Value, describeReply(r))
		return &o
	}
	if hit, saw := e.probe(h, probeOpts{}).satisfies(s.Effect); !hit {
		o := fail("after %s=%v was accepted, the next response did not change as declared (saw %s)", s.Name, s.Effect.Value, saw)
		return &o
	}
	return nil
}

func (e *env) checkSettingChangesResponse(protocol string) outcome {
	s, ok := e.effectSetting(protocol)
	if !ok {
		return notApplicable("no per-run setting declares a conformance effect for %s", protocol)
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	if o := e.applyEffect(h, s); o != nil {
		return *o
	}
	return pass("%s=%v was accepted and changed the next %s response as declared", s.Name, s.Effect.Value, protocol)
}

func (e *env) checkScenarioSetEvent(protocol string) outcome {
	s, ok := e.firstPerRun(func(Setting) bool { return true })
	if !ok {
		return notApplicable("the mock declares no per-run setting")
	}
	value := s.Default
	if s.Effect != nil && s.Effect.Protocol == protocol {
		value = s.Effect.Value
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	mark := e.events.mark()
	if r := e.putSettings(h, map[string]any{s.Name: value}); r.err != nil || r.status != http.StatusOK {
		return fail("PUT settings %s=%v: %s, want 200", s.Name, value, describeReply(r))
	}
	ev, _, found := e.await(mark, isKind("scenario.set", h.runID))
	if !found {
		return fail("no scenario.set event for run %s arrived within %s of setting %s", h.runID, e.opts.EventWait, s.Name)
	}
	if problem := e.eventProblem(ev, h.runID); problem != "" {
		return fail("%s", problem)
	}
	return pass("setting %s emitted a valid scenario.set event for run %s", s.Name, h.runID)
}

func (e *env) checkResetRestoresDefaults(protocol string) outcome {
	s, ok := e.effectSetting(protocol)
	if !ok {
		return notApplicable("no per-run setting declares a conformance effect for %s", protocol)
	}
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	if o := e.applyEffect(h, s); o != nil {
		return *o
	}
	if r := e.call(http.MethodPost, controlBase+"/scopes/"+h.runID+"/reset", nil); r.err != nil || r.status != http.StatusNoContent {
		return fail("POST reset: %s, want 204", describeReply(r))
	}
	if hit, saw := e.probe(h, probeOpts{}).satisfies(s.Effect); hit {
		return fail("after reset, %s still behaves as %v (saw %s); reset must return every setting to its default", s.Name, s.Effect.Value, saw)
	}
	return pass("reset returned %s to its default behaviour", s.Name)
}

func (e *env) checkResetEvent(protocol string) outcome {
	h, done, bad := e.openScoped(protocol)
	if bad != nil {
		return *bad
	}
	defer done()
	mark := e.events.mark()
	if r := e.call(http.MethodPost, controlBase+"/scopes/"+h.runID+"/reset", nil); r.err != nil || r.status != http.StatusNoContent {
		return fail("POST reset: %s, want 204", describeReply(r))
	}
	ev, _, found := e.await(mark, isKind("scope.reset", h.runID))
	if !found {
		return fail("no scope.reset event for run %s arrived within %s of the reset", h.runID, e.opts.EventWait)
	}
	if problem := e.eventProblem(ev, h.runID); problem != "" {
		return fail("%s", problem)
	}
	return pass("reset emitted a valid scope.reset event for run %s", h.runID)
}
