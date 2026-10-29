// Command nullmock runs the conformance suite's null mock.
//
//	nullmock -mode http|tcp [-mutant NAME] [-control 127.0.0.1:0] [-manifest-out PATH]
//
// It prints one line, "control=<url>", once the control endpoint is up, writes the
// manifest an operator would keep on disk to -manifest-out when given, and serves
// until it is interrupted.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"

	"example.com/mocks-studio/conformance/testdata/nullmock"
)

func main() {
	mode := flag.String("mode", nullmock.ModeHTTP, "protocol the scopes serve: http or tcp")
	mutant := flag.String("mutant", nullmock.MutantNone, "break exactly one rule, by name (empty: the correct mock)")
	control := flag.String("control", "127.0.0.1:0", "listen address of the control endpoint")
	manifestOut := flag.String("manifest-out", "", "write the on-disk manifest to this path")
	flag.Parse()

	m, err := nullmock.Start(nullmock.Config{Mode: *mode, Mutant: *mutant, ControlAddr: *control})
	if err != nil {
		fmt.Fprintln(os.Stderr, "nullmock:", err)
		os.Exit(1)
	}
	defer m.Close()
	if *manifestOut != "" {
		if err := os.WriteFile(*manifestOut, m.FileManifest(), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "nullmock:", err)
			os.Exit(1)
		}
	}
	fmt.Println("control=" + m.ControlURL())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop
}
