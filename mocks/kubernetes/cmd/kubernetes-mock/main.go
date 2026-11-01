// Command kubernetes-mock serves the Kubernetes slice mock: its control surface
// on --control, and one listener per scope the core opens.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/saphan-ai/mocks-studio/mocks/kubernetes/kubemock"
)

// defaultControlURL is the control URL the published mocks.manifest.json names.
const defaultControlURL = "http://127.0.0.1:9100"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "manifest" {
		_, _ = os.Stdout.Write(kubemock.ManifestBytes(defaultControlURL)) // a closed stdout is the caller's choice
		return 0
	}
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: kubernetes-mock serve --control <host:port> [--retain <events>] | kubernetes-mock manifest")
		return 2
	}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	control := fs.String("control", "", "address the control endpoint listens on, e.g. 127.0.0.1:9100")
	retain := fs.Int("retain", kubemock.DefaultRetainEvents, "how many changes a scope keeps for watch replay")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *control == "" {
		fmt.Fprintln(os.Stderr, "serve needs --control <host:port>, the address of the control endpoint; none was given")
		return 2
	}
	m, err := kubemock.Start(kubemock.Config{ControlAddr: *control, RetainEvents: *retain, Log: os.Stderr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "the control endpoint could not be opened on %s: %v\n", *control, err)
		return 1
	}
	defer m.Close()
	fmt.Fprintf(os.Stdout, "kubernetes-mock serving control at %s\n", m.ControlURL())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	return 0
}
