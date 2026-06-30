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
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
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
	defaultUsername   = "admin"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// config holds the resolved server settings parsed from the WT_* environment.
type config struct {
	addr       string
	workDir    string
	username   string
	password   string
	command    []string
	scrollback int
}

// loadConfig parses and validates the WT_* environment into a config. It
// returns an error rather than exiting so the caller owns the exit path (no
// os.Exit while a defer is pending).
func loadConfig() (config, error) {
	c := config{
		addr:       envOr("WT_ADDR", defaultAddr),
		command:    strings.Fields(envOr("WT_CMD", defaultCmd)),
		workDir:    os.Getenv("WT_WORKDIR"),
		scrollback: defaultScrollback,
		username:   envOr("WT_USERNAME", defaultUsername),
		password:   os.Getenv("WT_PASSWORD"),
	}
	if len(c.command) == 0 {
		return config{}, errors.New("WT_CMD is empty")
	}
	if v := os.Getenv("WT_SCROLLBACK"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return config{}, fmt.Errorf("WT_SCROLLBACK must be a non-negative integer, got %q", v)
		}
		c.scrollback = n
	}
	if c.workDir != "" {
		// WT_WORKDIR is operator-supplied configuration (the directory the
		// operator wants the shell to run in), not untrusted request input, so
		// an arbitrary absolute path is expected and correct here.
		if _, err := os.Stat(c.workDir); err != nil { // #nosec G703 -- operator-controlled config path, not user input
			return config{}, fmt.Errorf("WT_WORKDIR missing: %w", err)
		}
	}
	return c, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	warnIfExposed(cfg.addr, cfg.password)

	opts := []terminal.Option{
		terminal.WithScrollbackCapacity(cfg.scrollback),
		terminal.WithLogger(slog.Default()),
	}
	if cfg.workDir != "" {
		opts = append(opts, terminal.WithWorkDir(cfg.workDir))
	}
	term := terminal.NewHandler(cfg.command, opts...)

	var ready atomic.Bool

	handler, err := newHandler(&cfg, term, &ready)
	if err != nil {
		slog.Error("static assets unavailable", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		// ReadTimeout/WriteTimeout are intentionally unset: either would cap
		// the lifetime of the hijacked /ws WebSocket stream. IdleTimeout only
		// bounds idle keep-alive connections between requests (it does not
		// apply to a hijacked conn), so it is safe and bounds resource use.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.addr, "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop() called explicitly above; defer is a no-op safety net
	}

	go func() {
		slog.Info("web-terminal-server listening",
			"addr", cfg.addr, "cmd", strings.Join(cfg.command, " "),
			"work_dir", cfg.workDir, "scrollback", cfg.scrollback,
			"auth", cfg.password != "")
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

// newHandler assembles the HTTP handler: the route mux (WebSocket, health,
// static files) wrapped in the middleware chain. Middleware, outermost first:
// access log -> security headers -> basic auth (if configured) ->
// cross-origin protection -> routes. The terminal handler is passed in
// (rather than constructed here) so tests can exercise the routing and
// middleware without a real PTY. ready
// gates /healthz so load balancers see the server as unavailable during
// startup and graceful shutdown. It returns an error only if the embedded
// static assets can't be opened.
func newHandler(cfg *config, term http.Handler, ready *atomic.Bool) (http.Handler, error) {
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
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	handler := http.NewCrossOriginProtection().Handler(mux)
	if cfg.password != "" {
		handler = basicAuth(handler, cfg.username, cfg.password)
	}
	handler = securityHeaders(handler)
	handler = accessLog(handler)
	return handler, nil
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
	userHash := sha256.Sum256([]byte(username))
	passHash := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		gotUser := sha256.Sum256([]byte(u))
		gotPass := sha256.Sum256([]byte(p))
		userOK := subtle.ConstantTimeCompare(gotUser[:], userHash[:]) == 1
		passOK := subtle.ConstantTimeCompare(gotPass[:], passHash[:]) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="web-terminal", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets defense-in-depth response headers on every
// response: nosniff stops MIME-confusion, the CSP scopes assets to the
// same origin (with 'unsafe-inline' for the importmap + inline mount() in
// static/index.html and the renderer's inline cell styles), connect-src
// 'self' covers the same-origin /ws WebSocket, and frame-ancestors 'none'
// blocks clickjacking of the interactive terminal.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; " +
		"script-src 'self' 'unsafe-inline'; " +
		"style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
		"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
		"form-action 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", csp)
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
// implements Unwrap (not http.Hijacker) so http.ResponseController - used
// by coder/websocket's Accept - can reach the underlying ResponseWriter
// and hijack the /ws connection through the access-log middleware.
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
