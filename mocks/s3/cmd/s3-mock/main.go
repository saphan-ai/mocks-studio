// Command s3-mock serves the S3-compatible mock: s3-mock serve --control <host:port>.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/saphan-ai/mocks-studio/mocks/s3/internal/s3mock"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "manifest" {
		_, _ = os.Stdout.Write(s3mock.ManifestBytes(s3mock.DefaultControlURL)) // stdout closing early is the caller's choice
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: s3-mock serve --control <host:port> | s3-mock manifest")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	control := fs.String("control", "127.0.0.1:9100", "address of the control endpoint")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(2)
	}
	m, err := s3mock.Start(s3mock.Config{ControlAddr: *control, Log: os.Stderr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3-mock: cannot serve the control endpoint on %s: %v\n", *control, err)
		os.Exit(1)
	}
	fmt.Printf("s3-mock control endpoint: %s\n", m.ControlURL())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	m.Close()
}
