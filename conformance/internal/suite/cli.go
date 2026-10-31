package suite

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usageText = `mocks-conformance checks a running mock against the mock adapter specification
(spec/MOCK_ADAPTER_V1.md) and prints one line per check:

  PASS <check-id> — <what was measured>
  FAIL <check-id> — <what was measured>
  SKIP <check-id> — <why the check does not apply or could not run>

Exit codes:
  0  every check passed
  1  a check failed
  2  the mock was refused before checking (unknown contract version, manifest invalid)
  3  the mock could not be reached

Usage:
  mocks-conformance --manifest <path> [flags]
  mocks-conformance --control <url> [flags]

Flags:
`

// Main runs the command line and returns the process exit code.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mocks-conformance", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o Options
	fs.StringVar(&o.ManifestPath, "manifest", "", "path of the mock's mocks.manifest.json; its control.url is where the mock is reached")
	fs.StringVar(&o.ControlURL, "control", "", "control endpoint URL (scheme://host:port); overrides the manifest's, or supplies the manifest when none is given")
	fs.StringVar(&o.ListenHost, "listen-host", DefaultListenHost, "host the suite asks the mock to open listeners on")
	fs.DurationVar(&o.RequestTimeout, "timeout", DefaultRequestTimeout, "bound on every request the suite makes")
	fs.DurationVar(&o.EventWait, "event-wait", DefaultEventWait, "how long the suite waits for an event it expects")
	fs.IntVar(&o.OpenAttempts, "open-attempts", DefaultOpenAttempts, "tries to open a scope while the mock answers 409 listener-unavailable (the port the suite chose was taken); each try uses a fresh port")
	var ports string
	fs.StringVar(&ports, "listen-ports", "", "first-last: offer the mock only ports in this range for its listeners (default: any free port)")
	fs.Usage = func() {
		fmt.Fprint(stderr, usageText)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitPass
		}
		return ExitRefused
	}
	if ports != "" {
		var err error
		if o.PortMin, o.PortMax, err = parsePortRange(ports); err != nil {
			fmt.Fprintf(stderr, "--listen-ports %q is refused: %v; give first-last, for example 41000-41999\n", ports, err)
			return ExitRefused
		}
	}
	o.Out = stdout
	return Run(ctx, o)
}

// parsePortRange reads "first-last" with 1 <= first <= last <= 65535.
func parsePortRange(s string) (lo, hi int, err error) {
	if _, err := fmt.Sscanf(s, "%d-%d", &lo, &hi); err != nil {
		return 0, 0, errors.New("not two numbers joined by a hyphen")
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return 0, 0, errors.New("the range must satisfy 1 <= first <= last <= 65535")
	}
	return lo, hi, nil
}
