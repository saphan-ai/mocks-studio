package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const moduleRoot = "../.."

const exampleSpec = "examples/acme-example-orders/openapi.yaml"

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }() // released so the command can bind it
	return ln.Addr().String()
}

// serving runs `serve` until the test calls stop, and returns once the control surface answers.
type serving struct {
	stop func() int
}

func startServe(t *testing.T, args []string) *serving {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &serving{}
	var out, errw syncWriter
	exit := make(chan int, 1)
	go func() { exit <- Run(ctx, args, &out, &errw) }()
	var control string
	for i, a := range args {
		if a == "--control" {
			control = args[i+1]
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://" + control + "/_mock/v1/health")
		if err == nil {
			_ = resp.Body.Close() // only the answer matters
			break
		}
		select {
		case code := <-exit:
			cancel()
			t.Fatalf("serve exited %d before answering: %s", code, errw.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the control surface at %s never answered", control)
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.stop = func() int {
		cancel()
		select {
		case code := <-exit:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not stop after its context ended")
			return -1
		}
	}
	return s
}

// syncWriter is an output the command writes while the test reads it.
type syncWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestServeAnswersAndStopsCleanly(t *testing.T) {
	t.Chdir(moduleRoot)
	control := freeAddr(t)
	s := startServe(t, []string{"serve", "--spec", exampleSpec, "--name", "acme-example-orders", "--control", control})
	resp, err := http.Post("http://"+control+"/_mock/v1/scopes", "application/json",
		strings.NewReader(`{"run_id":"cli-run","listener":{"protocol":"http","host":"127.0.0.1","port":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	var opened struct {
		Listener struct {
			Port int `json:"port"`
		} `json:"listener"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil || resp.StatusCode != 201 {
		t.Fatalf("open = %d %v", resp.StatusCode, err)
	}
	_ = resp.Body.Close() // decoded
	got, err := http.Get("http://127.0.0.1:" + strconv.Itoa(opened.Listener.Port) + "/orders")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close() // read
	if got.StatusCode != 200 || !strings.Contains(string(body), "acme-widget") {
		t.Fatalf("GET /orders = %d %s", got.StatusCode, body)
	}
	if code := s.stop(); code != ExitOK {
		t.Fatalf("serve exited %d", code)
	}
	if _, err := http.Get("http://" + control + "/_mock/v1/health"); err == nil {
		t.Fatal("the control surface still answers after serve stopped")
	}
}

func TestRefusalsExitTwoAndName(t *testing.T) {
	t.Chdir(moduleRoot)
	dir := t.TempDir()
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("openapi: 3.1.0\ninfo: {title: t, version: '1'}\npaths:\n  /x:\n    get:\n      responses:\n        '200':\n          description: ok\n          content: {application/json: {schema: {$ref: '#/components/schemas/Missing'}}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "usage"},
		{"unknown command", []string{"run"}, "not a command"},
		{"serve without flags", []string{"serve"}, "--spec"},
		{"manifest without flags", []string{"manifest"}, "--control-url"},
		{"unreadable document", []string{"manifest", "--spec", filepath.Join(dir, "absent.yaml"), "--name", "x-mock", "--control-url", "http://127.0.0.1:9100", "--out", "-"}, "cannot be read"},
		{"broken reference", []string{"manifest", "--spec", broken, "--name", "x-mock", "--control-url", "http://127.0.0.1:9100", "--out", "-"}, "#/components/schemas/Missing"},
		{"bad name", []string{"manifest", "--spec", exampleSpec, "--name", "Bad Name", "--control-url", "http://127.0.0.1:9100", "--out", "-"}, "mock name"},
		{"bad control url", []string{"manifest", "--spec", exampleSpec, "--name", "x-mock", "--control-url", "127.0.0.1:9100", "--out", "-"}, "control URL"},
		{"credential safe field", []string{"manifest", "--spec", exampleSpec, "--name", "x-mock", "--control-url", "http://127.0.0.1:9100", "--safe-field", "api_key", "--out", "-"}, "credential"},
		{"unknown flag", []string{"manifest", "--nope"}, "nope"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errw bytes.Buffer
			if code := Run(context.Background(), c.args, &out, &errw); code != ExitRefused {
				t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitRefused, errw.String())
			}
			if !strings.Contains(errw.String(), c.want) {
				t.Fatalf("stderr %q does not name %q", errw.String(), c.want)
			}
			if out.Len() != 0 {
				t.Fatalf("a refusal wrote to stdout: %q", out.String())
			}
		})
	}
}

func TestServeExitsOneWhenTheControlAddressIsTaken(t *testing.T) {
	t.Chdir(moduleRoot)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }() // held for the duration of the test
	var out, errw bytes.Buffer
	code := Run(context.Background(), []string{"serve", "--spec", exampleSpec, "--name", "x-mock", "--control", ln.Addr().String()}, &out, &errw)
	if code != ExitFailed || !strings.Contains(errw.String(), "cannot be opened") {
		t.Fatalf("exit = %d, stderr %q", code, errw.String())
	}
}

func TestExampleManifestIsWhatManifestWrites(t *testing.T) {
	t.Chdir(moduleRoot)
	var out, errw bytes.Buffer
	args := []string{"manifest", "--spec", exampleSpec, "--name", "acme-example-orders", "--control-url", "http://127.0.0.1:9100", "--out", "-"}
	if code := Run(context.Background(), args, &out, &errw); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errw.String())
	}
	committed, err := os.ReadFile("examples/acme-example-orders/mocks.manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), committed) {
		t.Fatal("the committed example manifest is not what `manifest` writes for the example document; regenerate it")
	}
	file := filepath.Join(t.TempDir(), "mocks.manifest.json")
	args[len(args)-1] = file
	if code := Run(context.Background(), args, &out, &errw); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, errw.String())
	}
	if written, err := os.ReadFile(file); err != nil || !bytes.Equal(written, committed) {
		t.Fatalf("--out wrote different bytes than stdout: %v", err)
	}
}

// Every `$ openapi-mock …` command in SKILL.md is run; the `go -C conformance …` line is the acceptance run of the example.
func TestEveryCommandInTheSkillIsRun(t *testing.T) {
	t.Chdir(moduleRoot)
	skill, err := os.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	ran, acceptance := 0, 0
	for _, line := range strings.Split(string(skill), "\n") {
		cmd, ok := strings.CutPrefix(line, "$ ")
		if !ok {
			continue
		}
		fields := strings.Fields(cmd)
		if fields[0] == "go" {
			acceptance++
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
			s := startServe(t, args)
			resp, err := http.Get("http://" + control + "/_mock/v1/manifest")
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("%q: the manifest is not served: %v", cmd, err)
			}
			_ = resp.Body.Close() // only the status matters
			if code := s.stop(); code != ExitOK {
				t.Fatalf("%q stopped with %d", cmd, code)
			}
		default:
			t.Fatalf("SKILL.md carries an unknown subcommand: %q", cmd)
		}
	}
	if ran < 2 || acceptance != 1 {
		t.Fatalf("SKILL.md carries %d openapi-mock commands and %d acceptance runs, want at least 2 and exactly 1", ran, acceptance)
	}
}
