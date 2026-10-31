package s3mock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConformanceSuite runs the repository's conformance suite (spec §9) against a
// started mock and requires fail=0 and exit 0. Every SKIP and its reason is logged.
func TestConformanceSuite(t *testing.T) {
	suiteDir, err := filepath.Abs("../../../../conformance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(suiteDir, "cmd", "mocks-conformance")); err != nil {
		t.Skipf("conformance suite not found at %s: %v", suiteDir, err)
	}
	m, err := Start(Config{ControlAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer m.Close()
	manifest := filepath.Join(t.TempDir(), "mocks.manifest.json")
	if err := os.WriteFile(manifest, ManifestBytes(m.ControlURL()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, runErr := exec.Command("go", "-C", suiteDir, "run", "./cmd/mocks-conformance", "--manifest", manifest, "--control", m.ControlURL()).CombinedOutput()
	t.Logf("%s", out)
	if runErr != nil {
		t.Fatalf("suite failed: %v", runErr)
	}
	if !strings.Contains(string(out), "fail=0") {
		t.Fatalf("suite summary does not say fail=0:\n%s", out)
	}
}
