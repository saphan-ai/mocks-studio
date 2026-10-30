package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every `$ …` command in SKILL.md is run from the directory the document names (the module root, mocks/openapi/): the
// `openapi-mock` lines through Run, and the `go -C … conformance` line as the real suite against the serve line's mock.
// Where the serve line's control address cannot be bound the test skips, saying so.
func TestEveryCommandInTheSkillIsRun(t *testing.T) {
	t.Chdir(moduleRoot)
	skill, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	var live *serving
	stopLive := func() {
		if live != nil {
			if code := live.stop(); code != ExitOK {
				t.Fatalf("serve stopped with %d", code)
			}
			live = nil
		}
	}
	t.Cleanup(func() {
		if live != nil {
			live.stop()
		}
	})
	ran, suite := 0, 0
	for _, line := range strings.Split(string(skill), "\n") {
		cmd, ok := strings.CutPrefix(line, "$ ")
		if !ok {
			continue
		}
		fields := strings.Fields(cmd)
		if fields[0] == "go" {
			suite++
			if live == nil {
				t.Fatalf("%q has no mock to check: no `serve` line came before it", cmd)
			}
			out, err := exec.Command("go", fields[1:]...).CombinedOutput()
			if err != nil {
				t.Fatalf("%q failed from the directory SKILL.md names: %v\n%s", cmd, err, out)
			}
			if !bytes.Contains(out, []byte("fail=0")) {
				t.Fatalf("%q did not report fail=0:\n%s", cmd, out)
			}
			stopLive()
			continue
		}
		if fields[0] != "openapi-mock" {
			t.Fatalf("SKILL.md carries a command this test does not run: %q", cmd)
		}
		ran++
		args := fields[1:]
		switch args[0] {
		case "manifest":
			var out, errw bytes.Buffer
			if code := Run(context.Background(), args, &out, &errw); code != ExitOK {
				t.Fatalf("%q exited %d: %s", cmd, code, errw.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc["contract"] != "mocks.adapter/v1" {
				t.Fatalf("%q did not write a manifest: %v", cmd, err)
			}
		case "serve":
			control := args[len(args)-1]
			ln, err := net.Listen("tcp", control)
			if err != nil {
				t.Skipf("%q cannot run here: its control address %s cannot be bound (%v)", cmd, control, err)
			}
			_ = ln.Close() // released so the command can bind it
			stopLive()
			live = startServe(t, args)
			resp, err := http.Get("http://" + control + "/_mock/v1/manifest")
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%q: the manifest is not served: %v", cmd, err)
			}
			_ = resp.Body.Close() // only the status matters
		default:
			t.Fatalf("SKILL.md carries an unknown subcommand: %q", cmd)
		}
	}
	stopLive()
	if ran < 2 || suite != 1 {
		t.Fatalf("SKILL.md carries %d openapi-mock commands and %d suite runs, want at least 2 and exactly 1", ran, suite)
	}
}

// The example README gives the same suite command as SKILL.md, and says where to run it from.
func TestExampleReadmeAgreesWithTheSkill(t *testing.T) {
	t.Chdir(moduleRoot)
	skill, _ := os.ReadFile("SKILL.md")
	readme, err := os.ReadFile("examples/acme-example-orders/README.md")
	if err != nil {
		t.Fatal(err)
	}
	var suiteLine string
	for _, line := range strings.Split(string(skill), "\n") {
		if cmd, ok := strings.CutPrefix(line, "$ go "); ok {
			suiteLine = "go " + cmd
		}
	}
	if suiteLine == "" || !strings.Contains(string(readme), "`"+suiteLine+"`") {
		t.Errorf("the README does not carry SKILL.md's suite command %q", suiteLine)
	}
	if !strings.Contains(string(readme), "from `mocks/openapi/`") {
		t.Error("the README does not name the directory its commands run from")
	}
}
