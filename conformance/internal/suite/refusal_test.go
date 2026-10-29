package suite_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"example.com/mocks-studio/conformance/internal/suite"
	"example.com/mocks-studio/conformance/testdata/nullmock"
)

// unusedControl is a control URL with nothing behind it: port 1 is never assigned to a
// mock, so the suite must be refused or unreach it, never reach a mock.
const unusedControl = "http://127.0.0.1:1"

func staticManifest(t *testing.T, mutant string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mocks.manifest.json")
	if err := os.WriteFile(path, nullmock.ManifestBytes(nullmock.ModeHTTP, mutant, unusedControl), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// These three need no listener: the manifest alone decides.
func TestManifestRefusalsNeedNoMock(t *testing.T) {
	cases := []struct {
		mutant   string
		wantExit int
		wantID   string
		wantText string
	}{
		{nullmock.MutantContractV2, suite.ExitRefused, "contract-known", "supported versions: mocks.adapter/v1"},
		{nullmock.MutantManifestMissingName, suite.ExitRefused, "manifest-valid", "name"},
		{nullmock.MutantNone, suite.ExitUnreachable, "control-reachable", "got no answer"},
	}
	for _, c := range cases {
		t.Run(c.wantID, func(t *testing.T) {
			r := runSuite(t, suite.Options{ManifestPath: staticManifest(t, c.mutant)})
			if r.code != c.wantExit {
				t.Errorf("exit %d, want %d\n%s", r.code, c.wantExit, r.out)
			}
			if r.checks[c.wantID] != "FAIL" || !strings.Contains(r.out, c.wantText) {
				t.Errorf("want FAIL %s mentioning %q\n%s", c.wantID, c.wantText, r.out)
			}
			if fails := r.with("FAIL"); len(fails) != 1 {
				t.Errorf("failed checks %v, want exactly [%s]", fails, c.wantID)
			}
		})
	}
}
