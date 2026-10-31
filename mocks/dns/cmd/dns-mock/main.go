// Command dns-mock runs the DNS and DoH filtering resolver mock.
//
//	dns-mock serve [--control 127.0.0.1:9100]
//
// It prints one line, "control=<url>", once the control endpoint is up, logs to standard error
// and serves until it is interrupted.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	dnsmock "github.com/saphan-ai/mocks-studio/mocks/dns"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: dns-mock serve [--control host:port]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	control := fs.String("control", "127.0.0.1:9100", "listen address of the control endpoint (loopback only)")
	_ = fs.Parse(os.Args[2:]) // ExitOnError: a bad flag ends the process
	m, err := dnsmock.Start(dnsmock.Config{ControlAddr: *control, Log: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dns-mock:", err)
		os.Exit(1)
	}
	defer m.Close()
	fmt.Println("control=" + m.ControlURL())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
