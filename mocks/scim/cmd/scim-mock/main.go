// Command scim-mock serves the SCIM 2.0 mock: scim-mock serve --control 127.0.0.1:9100.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	scimmock "github.com/saphan-ai/mocks-studio/mocks/scim"
)

const usage = "usage: scim-mock serve [--control host:port]   (default 127.0.0.1:9100; loopback only)"

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	control := fs.String("control", "127.0.0.1:9100", "host:port of the control endpoint (loopback only)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	m, err := scimmock.Start(scimmock.Config{ControlAddr: *control, Logger: log.New(os.Stderr, "", log.LstdFlags|log.LUTC)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "scim-mock:", err)
		return 1
	}
	defer m.Close()
	fmt.Fprintln(os.Stderr, "scim-mock: control endpoint", m.ControlURL())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return 0
}
