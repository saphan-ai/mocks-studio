package suite

import "fmt"

// scopedCheck runs once per protocol the suite can drive, with the id suffixed
// "/<protocol>". needs lists the checks that must have passed for it to be meaningful;
// when one has not, the check is skipped, never failed, so a mutant that breaks one
// rule is reported on that rule alone.
type scopedCheck struct {
	id    string
	needs []string
	run   func(e *env, protocol string) outcome
}

const (
	needStream   = "events-stream-opens"
	needScope    = "scope-opens"
	needListener = "listener-answers"
	needEvent    = "event-valid"
	needSetting  = "setting-changes-response"
)

func scopedChecks() []scopedCheck {
	return []scopedCheck{
		{"scope-opens", nil, (*env).checkScopeOpens},
		{"listener-answers", []string{needScope}, (*env).checkListenerAnswers},
		{"scope-rejects-invalid-run-id", []string{needScope}, (*env).checkInvalidRunID},
		{"scope-rejects-duplicate", []string{needScope, needListener}, (*env).checkDuplicateScope},
		{"event-valid", []string{needListener, needStream}, (*env).checkEventValid},
		{"scope-lifecycle-events", []string{needEvent}, (*env).checkLifecycleEvents},
		{"correlation-traceparent", []string{needEvent}, (*env).checkCorrelationTraceparent},
		{"correlation-generated", []string{needEvent}, (*env).checkCorrelationGenerated},
		{"scope-mismatch-recorded", []string{needEvent}, (*env).checkScopeMismatch},
		{"scopes-isolated", []string{needEvent}, (*env).checkScopesIsolated},
		{"events-filter-by-run", []string{needEvent}, (*env).checkEventsFilter},
		{"setting-unknown-refused", []string{needScope}, (*env).checkUnknownSetting},
		{"setting-out-of-range-refused", []string{needScope}, (*env).checkOutOfRange},
		{"setting-type-refused", []string{needScope}, (*env).checkWrongType},
		{"setting-changes-response", []string{needListener}, (*env).checkSettingChangesResponse},
		{"scenario-set-event", []string{needEvent}, (*env).checkScenarioSetEvent},
		{"reset-restores-defaults", []string{needSetting}, (*env).checkResetRestoresDefaults},
		{"scope-reset-event", []string{needEvent}, (*env).checkResetEvent},
		{"payload-safe-fields-only", []string{needEvent}, (*env).checkPayloadSafeFields},
		{"no-credential-in-event", []string{needEvent}, (*env).checkNoCredentialInEvent},
		{"listener-closed-after-delete", []string{needListener}, (*env).checkListenerClosed},
	}
}

func (e *env) runScoped(protocol string) {
	for _, c := range scopedChecks() {
		id := fmt.Sprintf("%s/%s", c.id, protocol)
		needs := make([]string, len(c.needs))
		for i, n := range c.needs {
			needs[i] = n
			if _, scoped := e.rep.verdicts[n+"/"+protocol]; scoped {
				needs[i] = n + "/" + protocol
			}
		}
		if culprit, ok := e.unmet(needs); !ok {
			e.record(id, outcome{verdictSkip, "prerequisite " + culprit + " did not pass"})
			continue
		}
		e.record(id, c.run(e, protocol))
	}
}

func (e *env) runGlobal(id string, needs []string, run func() outcome) {
	if culprit, ok := e.unmet(needs); !ok {
		e.record(id, outcome{verdictSkip, "prerequisite " + culprit + " did not pass"})
		return
	}
	e.record(id, run())
}

func (e *env) unmet(needs []string) (string, bool) {
	for _, n := range needs {
		if met, culprit := e.rep.prerequisiteMet(n); !met {
			return culprit, false
		}
	}
	return "", true
}
