// Command web-terminal-server is a thin, generic web terminal: it runs a
// configured command in a PTY and serves the @cplieger/web-terminal-ui front
// end over HTTP + WebSocket, using the github.com/cplieger/web-terminal-engine engine.
//
// SECURITY: this is a remote shell. Anyone who can reach the listen address
// and pass auth (if any) gets an interactive process running WT_CMD with this
// server's privileges. It binds loopback (127.0.0.1) by default; only expose
// it on a public interface behind an authenticating reverse proxy, or set
// WT_PASSWORD. See README.md.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/web-terminal-engine/terminal"
)

// staticFS holds the bundled front end (the @cplieger/web-terminal-ui scaffold
// + compiled engine/UI JS + CSS). A fresh checkout commits only
// static/index.html; the dev-build script and the Dockerfile generate the
// compiled assets alongside it before `go build`.
//
//go:embed static
var staticFS embed.FS

const (
	defaultAddr       = "127.0.0.1:7681"
	defaultCmd        = "/bin/bash"
	defaultScrollback = 5000
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	addr := envOr("WT_ADDR", defaultAddr)
	command := strings.Fields(envOr("WT_CMD", defaultCmd))
	if len(command) == 0 {
		slog.Error("WT_CMD is empty")
		os.Exit(1)
	}
	workDir := os.Getenv("WT_WORKDIR")
	scrollback := defaultScrollback
	if v := os.Getenv("WT_SCROLLBACK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Error("WT_SCROLLBACK must be a non-negative integer", "value", v)
			os.Exit(1)
		}
		scrollback = n
	}
	username := envOr("WT_USERNAME", "admin")
	password := os.Getenv("WT_PASSWORD")

	if workDir != "" {
		if _, err := os.Stat(workDir); err != nil {
			slog.Error("WT_WORKDIR missing", "work_dir", workDir, "error", err)
			os.Exit(1)
		}
	}

	warnIfExposed(addr, password)

	opts := []terminal.Option{
		terminal.WithScrollbackCapacity(scrollback),
		terminal.WithLogger(slog.Default()),
	}
	if workDir != "" {
		opts = append(opts, terminal.WithWorkDir(workDir))
	}
	term := terminal.NewHandler(command, opts...)

	var ready atomic.Bool

	mux := http.NewServeMux()
	// Mount only the WebSocket endpoint (not the engine's /debug/* routes,
	// which dump raw PTY output and screen state — inappropriate to expose on
	// a network service). The UI connects here by default (wsPath "/ws").
	mux.Handle("/ws", term)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "starting up or shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("static assets unavailable", "error", err)
		os.Exit(1)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	// Middleware, outermost first: access log -> basic auth (if configured)
	// -> cross-origin protection -> routes.
	var handler http.Handler = http.NewCrossOriginProtection().Handler(mux)
	if password != "" {
		handler = basicAuth(handler, username, password)
	}
	handler = accessLog(handler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
		stop()
		os.Exit(1)
	}

	go func() {
		slog.Info("web-terminal-server listening",
			"addr", addr, "cmd", strings.Join(command, " "),
			"work_dir", workDir, "scrollback", scrollback,
			"auth", password != "")
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server exited", "error", err)
			stop()
		}
	}()
	ready.Store(true)

	<-ctx.Done()
	ready.Store(false)
	slog.Info("shutting down", "cause", context.Cause(ctx))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("server shutdown returned error", "error", err)
	}
	term.Shutdown()
}

// warnIfExposed logs a prominent warning when the server is reachable beyond
// the loopback interface without authentication — i.e. an unauthenticated
// remote shell. Bind loopback (the default) or set WT_PASSWORD, or front it
// with an authenticating reverse proxy.
func warnIfExposed(addr, password string) {
	if password != "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if isLoopbackHost(host) {
		return
	}
	slog.Warn("listening on a non-loopback address WITHOUT authentication",
		"addr", addr,
		"risk", "anyone who can reach this address gets an interactive shell",
		"fix", "set WT_PASSWORD, bind 127.0.0.1, or front with an authenticating reverse proxy")
}

// isLoopbackHost reports whether host names the loopback interface. An empty
// or wildcard host ("", "0.0.0.0", "::") binds all interfaces and is treated
// as exposed.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost":
		return true
	case "", "0.0.0.0", "::":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// basicAuth gates every request behind HTTP Basic credentials, using
// constant-time comparison so a wrong username or password can't be timed.
// The browser caches the credentials after the page load and replays them on
// the same-origin WebSocket handshake, so the terminal works behind it.
func basicAuth(next http.Handler, username, password string) http.Handler {
	userHash := []byte(username)
	passHash := []byte(password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), userHash) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), passHash) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="web-terminal", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// accessLog emits one structured slog line per request (slog-only
// observability, matching the cplieger fleet — no Prometheus endpoint).
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
	})
}

// statusWriter captures the response status code for the access log. It
// implements http.Hijacker so the WebSocket upgrade (which hijacks the
// connection) continues to work through the middleware.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController (used by coder/websocket's Accept to
// hijack the connection) reach the underlying ResponseWriter through this
// wrapper, so the /ws upgrade works behind the access-log middleware.
func (s *statusWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
