package main_test

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const startupWait = 10 * time.Second

func build(t *testing.T, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
}

// startNullMock runs the built fixture as a process and returns its manifest path.
func startNullMock(t *testing.T, bin, mode, mutant string) string {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "mocks.manifest.json")
	cmd := exec.Command(bin, "-mode", mode, "-mutant", mutant, "-manifest-out", manifest)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // the fixture only serves; killing it is its normal end
		_ = cmd.Wait()         // the kill error is expected
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if !strings.HasPrefix(line, "control=") {
			t.Fatalf("nullmock printed %q, want control=<url>", line)
		}
	case <-time.After(startupWait):
		t.Fatal("nullmock did not report its control URL")
	}
	return manifest
}

func runRunner(t *testing.T, bin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, out.String()
	case errors.As(err, &exit):
		return exit.ExitCode(), out.String()
	}
	t.Fatalf("runner did not run: %v", err)
	return -1, ""
}

// The before-and-after control of the acceptance list, through the real binaries:
// the null mock exits 0, the v2 mutant exits 2 naming the supported versions, and
// a manifest whose mock is not running exits 3.
func TestRunnerExitCodesThroughRealBinaries(t *testing.T) {
	dir := t.TempDir()
	runner := filepath.Join(dir, "mocks-conformance")
	nullmock := filepath.Join(dir, "nullmock")
	build(t, runner, ".")
	build(t, nullmock, "../../testdata/nullmock/cmd/nullmock")

	good := startNullMock(t, nullmock, "http", "")
	if code, out := runRunner(t, runner, "--manifest", good); code != 0 {
		t.Errorf("null mock: exit %d, want 0\n%s", code, out)
	}
	v2 := startNullMock(t, nullmock, "http", "contract-v2")
	code, out := runRunner(t, runner, "--manifest", v2)
	if code != 2 || !strings.Contains(out, "supported versions: mocks.adapter/v1") {
		t.Errorf("v2 mutant: exit %d, want 2 naming the supported versions\n%s", code, out)
	}
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	// Point the manifest at a control URL nothing serves.
	dead := filepath.Join(dir, "dead.manifest.json")
	if err := os.WriteFile(dead, []byte(redirectControl(string(data))), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := runRunner(t, runner, "--manifest", dead); code != 3 {
		t.Errorf("no mock behind the manifest: exit %d, want 3\n%s", code, out)
	}
}

// redirectControl rewrites control.url to a port nothing is assigned to.
func redirectControl(manifest string) string {
	const key = `"url": "`
	i := strings.Index(manifest, key)
	if i < 0 {
		return manifest
	}
	start := i + len(key)
	end := start + strings.Index(manifest[start:], `"`)
	return manifest[:start] + "http://127.0.0.1:1" + manifest[end:]
}
