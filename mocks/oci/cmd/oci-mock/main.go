// Command oci-mock runs the OCI registry mock.
//
//	oci-mock serve --control 127.0.0.1:9100 [--manifest-out PATH]
//
// It prints one line, "control=<url>", once the control endpoint is up, writes the
// manifest to --manifest-out when given, and serves until it is interrupted.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/saphan-ai/mocks-studio/mocks/oci/internal/ocimock"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: oci-mock serve --control <host:port> [--manifest-out PATH]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	control := fs.String("control", "127.0.0.1:9100", "listen address of the control endpoint")
	manifestOut := fs.String("manifest-out", "", "write the manifest to this path")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: a bad flag exits before this returns

	m, err := ocimock.Start(ocimock.Config{ControlAddr: *control, Logger: log.New(os.Stderr, "oci-mock: ", log.LstdFlags)})
	if err != nil {
		fmt.Fprintln(os.Stderr, "oci-mock:", err)
		os.Exit(1)
	}
	defer m.Close()
	if *manifestOut != "" {
		if err := os.WriteFile(*manifestOut, m.Manifest(), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "oci-mock:", err)
			os.Exit(1)
		}
	}
	fmt.Println("control=" + m.ControlURL())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
