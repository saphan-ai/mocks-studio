// Command dns-mock runs the DNS and DoH filtering resolver mock and a few client verbs to drive it.
//
//	dns-mock serve  [--control host:port]
//	dns-mock call   [--control URL] METHOD PATH [JSON-BODY]
//	dns-mock set    [--control URL] --run RUN_ID [--records JSON] [--blocked JSON] ...
//	dns-mock ask    --server host:port [--transport udp|tcp|doh] [--name NAME] [--type TYPE]
//	dns-mock events [--control URL] [--run RUN_ID] [--for DURATION]
//
// serve prints one line, "control=<url>", once the control endpoint is up, logs to standard error
// and serves until it is interrupted. The other verbs print what they got and exit 0 when the
// exchange completed, 1 when it did not, 2 on a usage error.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	dnsmock "github.com/saphan-ai/mocks-studio/mocks/dns"
)

const usage = "usage: dns-mock serve|call|set|ask|events [flags]"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "serve" {
		os.Exit(serve(os.Args[2:]))
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func serve(args []string) int {
	fs := newFlags("serve", os.Stderr)
	control := fs.String("control", "127.0.0.1:9100", "listen address of the control endpoint (loopback only)")
	if fs.Parse(args) != nil {
		return 2
	}
	m, err := dnsmock.Start(dnsmock.Config{ControlAddr: *control, Log: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dns-mock:", err)
		return 1
	}
	defer m.Close()
	fmt.Println("control=" + m.ControlURL())
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	return 0
}
