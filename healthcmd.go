package main

import (
	"cmp"
	"fmt"
	"io"
	"time"

	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/health/v2/probe"
)

// healthProbeTimeout stays under the image's HEALTHCHECK --timeout=5s, so a hung
// endpoint fails with a written reason instead of being killed.
const healthProbeTimeout = 4 * time.Second

// listenAddr is the server's bind address, which the health command also probes.
func listenAddr() string {
	return cmp.Or(envx.String("LISTEN_ADDR"), defaultAddr)
}

// healthCommand is the image's healthcheck: it asks /healthz on the address
// listenAddr binds and returns the exit code, writing each failure to w.
func healthCommand(w io.Writer, addr string) int {
	url, err := probe.LocalURL(addr, healthzPath)
	if err != nil {
		// The error quotes the value, and a rejected value is never echoed.
		_, _ = fmt.Fprintln(w, "unhealthy: LISTEN_ADDR is not a host:port with a port from 1 to 65535")
		return 1
	}
	return probe.Check(w, healthProbeTimeout, url)
}
