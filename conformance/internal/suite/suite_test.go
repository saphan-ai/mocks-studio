package suite_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/saphan-ai/mocks-studio/conformance/internal/suite"
	"github.com/saphan-ai/mocks-studio/conformance/testdata/nullmock"
)

const (
	testEventWait = 800 * time.Millisecond
	testTimeout   = 3 * time.Second
)

var lineRe = regexp.MustCompile(`^(PASS|FAIL|SKIP) (\S+) — (.*)$`)

type result struct {
	code   int
	out    string
	checks map[string]string // id → PASS|FAIL|SKIP
}

func (r result) with(verdict string) []string {
	var ids []string
	for id, v := range r.checks {
		if v == verdict {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// runAgainst starts the fixture and runs the suite against its manifest file.
func runAgainst(t *testing.T, mode, mutant string) result {
	t.Helper()
	m, err := nullmock.Start(nullmock.Config{Mode: mode, Mutant: mutant, ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	path := filepath.Join(t.TempDir(), "mocks.manifest.json")
	if err := os.WriteFile(path, m.FileManifest(), 0o600); err != nil {
		t.Fatal(err)
	}
	return runSuite(t, suite.Options{ManifestPath: path})
}

func runSuite(t *testing.T, o suite.Options) result {
	t.Helper()
	var out bytes.Buffer
	o.Out = &out
	o.EventWait = testEventWait
	o.RequestTimeout = testTimeout
	code := suite.Run(context.Background(), o)
	res := result{code: code, out: out.String(), checks: map[string]string{}}
	for _, line := range strings.Split(out.String(), "\n") {
		if m := lineRe.FindStringSubmatch(line); m != nil {
			res.checks[m[2]] = m[1]
		}
	}
	return res
}

// The null mock passes every check over HTTP, and over TCP where the listener alone
// carries the run. The SKIPs are the checks whose carrier the protocol does not have.
func TestNullMockPassesEveryCheck(t *testing.T) {
	cases := []struct {
		mode      string
		wantSkips []string
	}{
		{nullmock.ModeHTTP, nil},
		{nullmock.ModeTCP, []string{"correlation-traceparent/tcp", "no-credential-in-event/tcp", "scope-mismatch-recorded/tcp"}},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			r := runAgainst(t, c.mode, nullmock.MutantNone)
			if r.code != suite.ExitPass {
				t.Fatalf("exit %d, want 0:\n%s", r.code, r.out)
			}
			if fails := r.with("FAIL"); len(fails) > 0 {
				t.Errorf("failed checks: %v\n%s", fails, r.out)
			}
			if skips := r.with("SKIP"); strings.Join(skips, ",") != strings.Join(c.wantSkips, ",") {
				t.Errorf("skipped %v, want %v\n%s", skips, c.wantSkips, r.out)
			}
			const minPass = 25 // 28 over http; 25 over tcp, which skips three header-only checks
			if n := len(r.with("PASS")); n < minPass {
				t.Errorf("only %d checks passed, want at least %d\n%s", n, minPass, r.out)
			}
		})
	}
}

// mutantCase is one rule broken in the fixture, and the one check that must notice.
type mutantCase struct {
	mutant   string
	mode     string
	wantExit int
	wantFail string // exactly this check fails, and no other
}

func mutantCases() []mutantCase {
	h, tcp := nullmock.ModeHTTP, nullmock.ModeTCP
	return []mutantCase{
		{nullmock.MutantManifestMissingName, h, suite.ExitRefused, "manifest-valid"},
		{nullmock.MutantContractV2, h, suite.ExitRefused, "contract-known"},
		{nullmock.MutantHealthFailing, h, suite.ExitFail, "health-answers"},
		{nullmock.MutantServedManifestDiffers, h, suite.ExitFail, "control-manifest-served"},
		{nullmock.MutantEventsWrongContentType, h, suite.ExitFail, "events-stream-opens"},
		{nullmock.MutantScopeOpenRefused, h, suite.ExitFail, "scope-opens/http"},
		{nullmock.MutantListenerSilent, h, suite.ExitFail, "listener-answers/http"},
		{nullmock.MutantListenerSilent, tcp, suite.ExitFail, "listener-answers/tcp"},
		{nullmock.MutantAcceptsInvalidRunID, h, suite.ExitFail, "scope-rejects-invalid-run-id/http"},
		{nullmock.MutantMergesDuplicateScope, h, suite.ExitFail, "scope-rejects-duplicate/http"},
		{nullmock.MutantAcceptsUnsupportedProto, h, suite.ExitFail, "scope-rejects-unsupported-protocol"},
		{nullmock.MutantEventMissingRunID, h, suite.ExitFail, "event-valid/http"},
		{nullmock.MutantResponseSentInvalid, h, suite.ExitFail, "event-valid/http"},
		{nullmock.MutantNoScopeClosedEvent, h, suite.ExitFail, "scope-lifecycle-events/http"},
		{nullmock.MutantTraceparentIgnored, h, suite.ExitFail, "correlation-traceparent/http"},
		{nullmock.MutantGeneratedLabelledHeader, h, suite.ExitFail, "correlation-generated/http"},
		{nullmock.MutantMismatchNotRecorded, h, suite.ExitFail, "scope-mismatch-recorded/http"},
		{nullmock.MutantScopesLeakEvents, h, suite.ExitFail, "scopes-isolated/http"},
		{nullmock.MutantFilterIgnored, h, suite.ExitFail, "events-filter-by-run/http"},
		{nullmock.MutantUnknownSettingAccepted, h, suite.ExitFail, "setting-unknown-refused/http"},
		{nullmock.MutantRefusalWithoutReasonCode, h, suite.ExitFail, "setting-unknown-refused/http"},
		{nullmock.MutantUnknownSettingNoAllowed, h, suite.ExitFail, "setting-unknown-refused/http"},
		{nullmock.MutantOutOfRangeAccepted, h, suite.ExitFail, "setting-out-of-range-refused/http"},
		{nullmock.MutantOutOfRangeAccepted, tcp, suite.ExitFail, "setting-out-of-range-refused/tcp"},
		{nullmock.MutantWrongTypeAccepted, h, suite.ExitFail, "setting-type-refused/http"},
		{nullmock.MutantSettingNoEffect, h, suite.ExitFail, "setting-changes-response/http"},
		{nullmock.MutantNoScenarioSetEvent, h, suite.ExitFail, "scenario-set-event/http"},
		{nullmock.MutantResetKeepsSettings, h, suite.ExitFail, "reset-restores-defaults/http"},
		{nullmock.MutantNoScopeResetEvent, h, suite.ExitFail, "scope-reset-event/http"},
		{nullmock.MutantNonSafeFieldInClear, h, suite.ExitFail, "payload-safe-fields-only/http"},
		{nullmock.MutantCredentialInSummary, h, suite.ExitFail, "no-credential-in-event/http"},
		{nullmock.MutantListenerStaysOpen, h, suite.ExitFail, "listener-closed-after-delete/http"},
		{nullmock.MutantListenerStaysOpen, tcp, suite.ExitFail, "listener-closed-after-delete/tcp"},
	}
}

// Negative control for every rule (law #27): the fixture with exactly one rule broken
// fails exactly the check that guards it, and the runner exits with the code for it.
func TestEachMutantFailsExactlyItsCheck(t *testing.T) {
	for _, c := range mutantCases() {
		t.Run(c.mutant+"/"+c.mode, func(t *testing.T) {
			t.Parallel()
			r := runAgainst(t, c.mode, c.mutant)
			if r.code != c.wantExit {
				t.Errorf("exit %d, want %d\n%s", r.code, c.wantExit, r.out)
			}
			if fails := r.with("FAIL"); len(fails) != 1 || fails[0] != c.wantFail {
				t.Errorf("failed checks %v, want exactly [%s]\n%s", fails, c.wantFail, r.out)
			}
		})
	}
}

// A mutant that breaks a rule names it: the FAIL line says what was measured.
func TestContractRefusalNamesSupportedVersions(t *testing.T) {
	r := runAgainst(t, nullmock.ModeHTTP, nullmock.MutantContractV2)
	if !strings.Contains(r.out, "mocks.adapter/v2") || !strings.Contains(r.out, "supported versions: mocks.adapter/v1") {
		t.Errorf("the refusal must name the declared version and the supported list:\n%s", r.out)
	}
	if len(r.checks) != 1 {
		t.Errorf("a mock refused before checking runs no other check, got %v", r.checks)
	}
}

// No mock behind the manifest: exit 3, and the manifest itself was fine.
func TestUnreachableMockExitsThree(t *testing.T) {
	m, err := nullmock.Start(nullmock.Config{Mode: nullmock.ModeHTTP, ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mocks.manifest.json")
	if err := os.WriteFile(path, m.FileManifest(), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Close() // the manifest now points at a port nobody listens on
	r := runSuite(t, suite.Options{ManifestPath: path})
	if r.code != suite.ExitUnreachable {
		t.Errorf("exit %d, want 3\n%s", r.code, r.out)
	}
	if got := r.checks["control-reachable"]; got != "FAIL" {
		t.Errorf("control-reachable = %q, want FAIL\n%s", got, r.out)
	}
	if r.checks["manifest-valid"] != "PASS" {
		t.Errorf("the manifest is valid and must say so before the mock is dialled\n%s", r.out)
	}
}

// With only a control URL the manifest is read from the mock, and an unreachable
// control URL is exit 3 as well.
func TestControlURLOnly(t *testing.T) {
	m, err := nullmock.Start(nullmock.Config{Mode: nullmock.ModeHTTP, ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	url := m.ControlURL()
	r := runSuite(t, suite.Options{ControlURL: url})
	if r.code != suite.ExitPass {
		t.Errorf("exit %d, want 0\n%s", r.code, r.out)
	}
	m.Close()
	r = runSuite(t, suite.Options{ControlURL: url})
	if r.code != suite.ExitUnreachable {
		t.Errorf("after the mock stopped: exit %d, want 3\n%s", r.code, r.out)
	}
}

// Neither a manifest nor a control URL: there is nothing to check, refused (exit 2).
func TestNothingToCheckIsRefused(t *testing.T) {
	r := runSuite(t, suite.Options{})
	if r.code != suite.ExitRefused {
		t.Errorf("exit %d, want 2\n%s", r.code, r.out)
	}
}

// A manifest that is not JSON is refused before the mock is dialled.
func TestUnparsableManifestIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mocks.manifest.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runSuite(t, suite.Options{ManifestPath: path})
	if r.code != suite.ExitRefused || r.checks["manifest-valid"] != "FAIL" {
		t.Errorf("exit %d, checks %v, want exit 2 with manifest-valid FAIL\n%s", r.code, r.checks, r.out)
	}
}
