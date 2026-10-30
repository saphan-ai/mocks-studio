// Package cli is the openapi-mock command: `serve` runs the mock, `manifest` writes its manifest file.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/engine"
	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/oas"
)

// Exit codes of the command.
const (
	ExitOK      = 0
	ExitFailed  = 1 // the mock could not run: the control address is taken, a file could not be written
	ExitRefused = 2 // the request was refused before anything ran: bad flags, an unreadable or invalid document
)

const (
	defaultManifestFile = "mocks.manifest.json"
	headerTimeout       = 5 * time.Second
	shutdownGrace       = 3 * time.Second
	usage               = `usage:
  openapi-mock serve    --spec <file> --name <mock-name> --control <host:port> [--control-url <url>] [--safe-field <name>]... [--verbose]
  openapi-mock manifest --spec <file> --name <mock-name> --control-url <url> [--out <file>] [--safe-field <name>]...
`
)

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// Run executes one command line and returns its exit code. It returns when ctx ends, for `serve`.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage) // nowhere else to report a failed write
		return ExitRefused
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:], stdout, stderr)
	case "manifest":
		return manifest(args[1:], stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "openapi-mock: %q is not a command; use serve or manifest.\n%s", args[0], usage)
	return ExitRefused
}

type common struct {
	spec, name, controlURL string
	safe                   stringList
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.spec, "spec", "", "the OpenAPI 3.x document, JSON or YAML (required)")
	fs.StringVar(&c.name, "name", "", "the mock's name, as the manifest and every event carry it (required)")
	fs.StringVar(&c.controlURL, "control-url", "", "the base URL the manifest advertises for the control endpoint")
	fs.Var(&c.safe, "safe-field", "a payload field that may be exported in clear; repeatable; none by default")
}

func refuse(stderr io.Writer, format string, a ...any) int {
	_, _ = fmt.Fprintf(stderr, "openapi-mock: "+format+"\n", a...) // nowhere else to report a failed write
	return ExitRefused
}

func parse(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	fs.SetOutput(stderr)
	return fs.Parse(args) == nil
}

func serve(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var c common
	var control string
	var verbose bool
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	c.flags(fs)
	fs.StringVar(&control, "control", "", "host:port the control surface listens on (required)")
	fs.BoolVar(&verbose, "verbose", false, "also log every field name of each call, with its value redacted")
	if !parse(fs, args, stderr) {
		return ExitRefused
	}
	if c.spec == "" || c.name == "" || control == "" {
		return refuse(stderr, "serve needs --spec, --name and --control.")
	}
	doc, err := oas.Load(c.spec)
	if err != nil {
		return refuse(stderr, "the document was refused: %v", err)
	}
	ln, err := net.Listen("tcp", control)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "openapi-mock: the control address %s cannot be opened: %v\n", control, err)
		return ExitFailed
	}
	if c.controlURL == "" {
		c.controlURL = "http://" + ln.Addr().String()
	}
	eng, err := engine.New(doc, engine.Options{
		Config:  engine.Config{Name: c.name, ControlURL: c.controlURL, SafeFields: c.safe},
		Log:     stderr,
		Verbose: verbose,
	})
	if err != nil {
		_ = ln.Close() // the refusal below is the report; a close error adds nothing
		return refuse(stderr, "the mock was refused: %v", err)
	}
	return run(ctx, ln, eng, c, stdout, stderr)
}

func run(ctx context.Context, ln net.Listener, eng *engine.Engine, c common, stdout, stderr io.Writer) int {
	srv := &http.Server{Handler: eng, ReadHeaderTimeout: headerTimeout}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	_, _ = fmt.Fprintf(stdout, "openapi-mock: %s serves %s; control surface on %s\n", c.name, c.spec, c.controlURL)
	code := ExitOK
	select {
	case <-ctx.Done():
	case err := <-failed:
		if !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintf(stderr, "openapi-mock: the control surface stopped: %v\n", err)
			code = ExitFailed
		}
	}
	eng.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close() // streams that outlive the grace period are cut
	}
	return code
}

func manifest(args []string, stdout, stderr io.Writer) int {
	var c common
	var out string
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	c.flags(fs)
	fs.StringVar(&out, "out", defaultManifestFile, "where to write the manifest; - writes to standard output")
	if !parse(fs, args, stderr) {
		return ExitRefused
	}
	if c.spec == "" || c.name == "" || c.controlURL == "" {
		return refuse(stderr, "manifest needs --spec, --name and --control-url.")
	}
	doc, err := oas.Load(c.spec)
	if err != nil {
		return refuse(stderr, "the document was refused: %v", err)
	}
	eng, err := engine.New(doc, engine.Options{Config: engine.Config{Name: c.name, ControlURL: c.controlURL, SafeFields: c.safe}, Log: io.Discard})
	if err != nil {
		return refuse(stderr, "the manifest was refused: %v", err)
	}
	if out == "-" {
		_, _ = stdout.Write(eng.ManifestJSON()) // nowhere else to report a failed write
		return ExitOK
	}
	if err := os.WriteFile(out, eng.ManifestJSON(), 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "openapi-mock: the manifest cannot be written: %v\n", err)
		return ExitFailed
	}
	_, _ = fmt.Fprintf(stdout, "openapi-mock: wrote %s\n", out)
	return ExitOK
}
