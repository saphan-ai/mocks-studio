// Command mocks-conformance checks a running mock against the mock adapter
// specification, spec/MOCK_ADAPTER_V1.md. Run it with -h for the exit codes.
package main

import (
	"context"
	"os"
	"os/signal"

	"example.com/mocks-studio/conformance/internal/suite"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := suite.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
