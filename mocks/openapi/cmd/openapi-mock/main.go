// Command openapi-mock serves any OpenAPI 3.x document as a mock that the Mocks Studio conformance suite accepts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/saphan-ai/mocks-studio/mocks/openapi/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
