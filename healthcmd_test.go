package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/go/health/v2"
)

// serveOnLoopback serves h on 127.0.0.1 and returns the ":port" listen address
// the health command is given, as with the image's LISTEN_ADDR=:7681.
func serveOnLoopback(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return ":" + port
}

// TestHealthCommand_readsTheServedVerdict runs the health command against the
// app's own handler chain, with ALLOWED_HOSTS naming only an outside name and a
// password set: the probe's loopback request must still be admitted, and the
// exit code must follow the /healthz verdict.
func TestHealthCommand_readsTheServedVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		serving bool
		want    int
	}{
		{"serving", true, 0},
		{"starting or draining", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ready health.Status
			ready.Set(tc.serving)
			cfg := config{hostPolicy: hostPolicyFor(t, "term.example.com"), username: "admin", password: "pw"}
			addr := serveOnLoopback(t, newTestHandler(t, cfg, &ready, nil))

			var out bytes.Buffer
			if got := healthCommand(&out, addr); got != tc.want {
				t.Fatalf("healthCommand(%q) = %d, want %d (output %q)", addr, got, tc.want, out.String())
			}
			if tc.want != 0 && !strings.Contains(out.String(), "503") {
				t.Errorf("output = %q, want the failure to name the 503", out.String())
			}
		})
	}
}

// TestHealthCommand_badListenAddr pins that an address the probe cannot turn
// into a URL fails the check with a reason, and that the reason never echoes
// the value: a compose interpolation mistake can put a credential on any
// variable.
func TestHealthCommand_badListenAddr(t *testing.T) {
	for _, addr := range []string{"no-port", "host:0", "secret-token:99999"} {
		var out bytes.Buffer
		if got := healthCommand(&out, addr); got != 1 {
			t.Fatalf("healthCommand(%q) = %d, want 1", addr, got)
		}
		if !strings.Contains(out.String(), "LISTEN_ADDR") {
			t.Errorf("output = %q, want it to name LISTEN_ADDR", out.String())
		}
		if strings.Contains(out.String(), addr) {
			t.Errorf("output = %q, want the rejected value left out", out.String())
		}
	}
}

// TestListenAddr_default pins that the health command and the server read the
// same address, so an unset LISTEN_ADDR is probed where the server binds.
func TestListenAddr_default(t *testing.T) {
	setServerEnv(t, nil)
	if got := listenAddr(); got != defaultAddr {
		t.Errorf("listenAddr() = %q, want %q", got, defaultAddr)
	}
	t.Setenv("LISTEN_ADDR", ":7000")
	if got := listenAddr(); got != ":7000" {
		t.Errorf("listenAddr() = %q, want :7000", got)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != listenAddr() {
		t.Errorf("loadConfig().addr = %q, want listenAddr() = %q", cfg.addr, listenAddr())
	}
}

// TestHealthCommand_probesTheNamedAddress serves on 127.0.0.2, which nothing
// listens on at 127.0.0.1, so a command that ignored the host part of
// LISTEN_ADDR would fail.
func TestHealthCommand_probesTheNamedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 is not usable here: %v", err)
	}
	var ready health.Status
	ready.Set(true)
	srv := httptest.NewUnstartedServer(newTestHandler(t, config{hostPolicy: hostPolicyFor(t, "term.example.com")}, &ready, nil))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	addr := ln.Addr().String()
	var out bytes.Buffer
	if got := healthCommand(&out, addr); got != 0 {
		t.Fatalf("healthCommand(%q) = %d, want 0 (output %q)", addr, got, out.String())
	}
}

// TestHealthProbe_singleAddressBindAdmitted pins the host gate for a bind on
// one non-loopback address that ALLOWED_HOSTS does not list: a request whose
// peer and Host are both the bound address is admitted, and another peer naming
// that Host is refused.
func TestHealthProbe_singleAddressBindAdmitted(t *testing.T) {
	var ready health.Status
	ready.Set(true)
	h := newTestHandler(t, config{hostPolicy: hostPolicyFor(t, "term.example.com")}, &ready, nil)

	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 7681}
	for _, tc := range []struct {
		name, remoteAddr string
		want             int
	}{
		{"probe from the bound address", "192.0.2.5:54321", http.StatusOK},
		{"another peer naming the bound address", "192.0.2.9:54321", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://192.0.2.5:7681"+healthzPath, http.NoBody)
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, local))
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("GET %s from %s = %d, want %d", healthzPath, tc.remoteAddr, rec.Code, tc.want)
			}
		})
	}
}
