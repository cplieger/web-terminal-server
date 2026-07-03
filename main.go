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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cplieger/web-terminal-engine/v2/terminal"
)

// staticFS holds the bundled front end (the @cplieger/web-terminal-ui scaffold
// + compiled engine/UI JS + CSS). A fresh checkout commits only
// static/index.html; the dev-build script and the Dockerfile generate the
// compiled assets alongside it before `go build`.
//
//go:embed static
var staticFS embed.FS

const (
	defaultAddr        = "127.0.0.1:7681"
	defaultCmd         = "/bin/bash"
	defaultScrollback  = 5000
	defaultUsername    = "admin"
	defaultMaxSessions = 10
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// applyIntEnv parses an integer env var into *dst, leaving it unchanged when the
// var is unset. It rejects a value below min or a non-integer.
func applyIntEnv(key string, minVal int, dst *int) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < minVal {
		return fmt.Errorf("%s must be an integer >= %d, got %q", key, minVal, v)
	}
	*dst = n
	return nil
}

// applyDurationEnv parses a Go duration env var into *dst, leaving it unchanged
// when unset. It rejects a negative or unparseable duration.
func applyDurationEnv(key string, dst *time.Duration) error {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return fmt.Errorf("%s must be a non-negative Go duration, got %q", key, v)
	}
	*dst = d
	return nil
}

// config holds the resolved server settings parsed from the WT_* environment.
type config struct {
	addr        string
	workDir     string
	username    string
	password    string
	command     []string
	idleReaper  time.Duration
	scrollback  int
	maxSessions int
}

// loadConfig parses and validates the WT_* environment into a config. It
// returns an error rather than exiting so the caller owns the exit path (no
// os.Exit while a defer is pending).
func loadConfig() (config, error) {
	c := config{
		addr:        envOr("WT_ADDR", defaultAddr),
		command:     strings.Fields(envOr("WT_CMD", defaultCmd)),
		workDir:     os.Getenv("WT_WORKDIR"),
		scrollback:  defaultScrollback,
		username:    envOr("WT_USERNAME", defaultUsername),
		password:    os.Getenv("WT_PASSWORD"),
		maxSessions: defaultMaxSessions,
	}
	if len(c.command) == 0 {
		return config{}, errors.New("WT_CMD is empty")
	}
	if err := applyIntEnv("WT_SCROLLBACK", 0, &c.scrollback); err != nil {
		return config{}, err
	}
	if err := applyIntEnv("WT_MAX_SESSIONS", 1, &c.maxSessions); err != nil {
		return config{}, err
	}
	if err := applyDurationEnv("WT_IDLE_REAPER", &c.idleReaper); err != nil {
		return config{}, err
	}
	if c.workDir != "" {
		// WT_WORKDIR is operator-supplied configuration (the directory the
		// operator wants the shell to run in), not untrusted request input, so
		// an arbitrary absolute path is expected and correct here.
		fi, err := os.Stat(c.workDir) // #nosec G703 -- operator-controlled config path, not user input
		if err != nil {
			return config{}, fmt.Errorf("WT_WORKDIR missing: %w", err)
		}
		// Reject a non-directory up front: the engine sets cmd.Dir to this
		// path, so a regular file would pass startup and only fail when the
		// PTY child can't spawn on the first client connect.
		if !fi.IsDir() {
			return config{}, fmt.Errorf("WT_WORKDIR is not a directory: %q", c.workDir)
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

	// Each session gets its own PTY-backed handler; the factory scopes the
	// handler's logger to the session id for per-session log correlation.
	factory := func(id string) *terminal.Handler {
		opts := []terminal.Option{
			terminal.WithScrollbackCapacity(cfg.scrollback),
			terminal.WithLogger(slog.Default().With("session", id)),
		}
		if cfg.workDir != "" {
			opts = append(opts, terminal.WithWorkDir(cfg.workDir))
		}
		return terminal.NewHandler(cfg.command, opts...)
	}
	mgrOpts := []terminal.ManagerOption{
		terminal.WithManagerLogger(slog.Default()),
		terminal.WithMaxSessions(cfg.maxSessions),
	}
	if cfg.idleReaper > 0 {
		mgrOpts = append(mgrOpts, terminal.WithIdleReaper(cfg.idleReaper))
	}
	mgr := terminal.NewSessionManager(factory, mgrOpts...)

	var ready atomic.Bool

	handler, err := newHandler(&cfg, mgr.WebSocketHandler(), mgr.RESTHandler(), mgr.EventsHandler(), &ready)
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
			"max_sessions", cfg.maxSessions, "auth", cfg.password != "")
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
	mgr.Shutdown()
	slog.Info("web-terminal-server stopped")
}

// newHandler assembles the HTTP handler: the route mux (terminal WebSocket,
// session REST API, status SSE, health, static files) wrapped in the middleware
// chain. Middleware, outermost first: access log -> security headers -> basic
// auth (if configured) -> cross-origin protection -> routes. The session
// handlers are passed in (rather than a manager constructed here) so tests can
// exercise the routing and middleware with stubs, without a real PTY. ready
// gates /healthz so load balancers see the server as unavailable during startup
// and graceful shutdown. It returns an error only if the embedded static assets
// can't be opened.
func newHandler(cfg *config, ws, rest, events http.Handler, ready *atomic.Bool) (http.Handler, error) {
	mux := http.NewServeMux()
	// Mount only the session WebSocket endpoint (not the engine's /debug/*
	// routes, which dump raw PTY output and screen state — inappropriate to
	// expose on a network service). The UI connects here with ?session=<id>.
	mux.Handle("/ws", ws)
	// Session REST API. The create rate limit gates POST /api/sessions so a
	// bare (possibly unauthenticated) caller cannot fork PTY processes without
	// bound: WithMaxSessions caps concurrency, the limiter bounds churn. GET
	// (list) and DELETE (close) pass through. Mounted at both the exact path and
	// the subtree so /api/sessions and /api/sessions/{id} reach the REST mux.
	limitedRest := createRateLimit(rest)
	mux.Handle("/api/sessions", limitedRest)
	mux.Handle("/api/sessions/", limitedRest)
	// The status SSE is a distinct, more-specific path than the REST subtree, so
	// ServeMux routes it here rather than to the REST DELETE /{id} pattern.
	mux.Handle("/api/sessions/events", events)
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
	staticSrv, err := staticHandler(sub)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", staticSrv)

	handler := http.NewCrossOriginProtection().Handler(mux)
	if cfg.password != "" {
		handler = basicAuth(handler, cfg.username, cfg.password)
	}
	handler = securityHeaders(handler)
	handler = accessLog(handler)
	return handler, nil
}

// Create-rate-limit tuning: a token bucket with a small burst (open several
// tabs at once) refilling at a steady rate, so sustained create churn is
// throttled while normal use is unaffected.
const (
	createBurst        = 6.0
	createRefillPerSec = 1.0
)

// tokenBucket is a minimal mutex-guarded token bucket (no external dependency).
type tokenBucket struct {
	last   time.Time
	tokens float64
	mu     sync.Mutex
}

// allow refills the bucket for the elapsed time and consumes one token,
// returning false when none is available.
func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.last.IsZero() {
		b.tokens = createBurst
	} else {
		b.tokens += now.Sub(b.last).Seconds() * createRefillPerSec
		if b.tokens > createBurst {
			b.tokens = createBurst
		}
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// createRateLimit wraps the session REST handler, gating POST /api/sessions
// (session creation) behind a token bucket so a caller cannot fork PTY
// processes without bound. List (GET) and close (DELETE) pass through
// unthrottled.
func createRateLimit(next http.Handler) http.Handler {
	bucket := &tokenBucket{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/sessions" && !bucket.allow() {
			http.Error(w, "session creation rate exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gzAsset is a precomputed gzip representation of an embedded text asset.
type gzAsset struct {
	contentType string
	body        []byte
}

// staticHandler serves the embedded front end with a per-file content-hash
// ETag, Cache-Control: no-cache, and precomputed gzip bodies for compressible
// assets. embed.FS reports a zero ModTime, so http.FileServer emits neither
// Last-Modified nor ETag on its own, leaving the browser no way to
// revalidate: every full page load (refresh, reconnect-by-reload, iOS Safari
// resuming from background) re-downloads the whole vendored JS bundle, CSS,
// and terminal font. The embedded bytes are fixed at build time, so hashing
// each file once at startup yields a stable ETag that lets http.ServeContent
// answer If-None-Match with 304 Not Modified. "no-cache" forces revalidation
// on every load rather than relying on a TTL: the vendored asset paths are
// stable (not content-hashed), so a long max-age would serve stale JS after
// an engine/UI version bump.
//
// The vendored JS+CSS is large and otherwise ships uncompressed over plain
// HTTP/1.1 in the WT_PASSWORD-only (no reverse proxy) deployment the README
// supports, lengthening time-to-interactive on the touch-first-over-cellular
// target. Each asset is therefore gzip-compressed once at startup (mirroring
// the ETag precompute) and served with Content-Encoding: gzip + Vary:
// Accept-Encoding when the client offers gzip on a non-Range GET/HEAD.
// Compression is gzip-only to stay on the standard library (no brotli
// dependency); front with a reverse proxy to layer brotli/HTTP-2 on top.
func staticHandler(sub fs.FS) (http.Handler, error) {
	etags, gzipped, err := buildAssetMaps(sub)
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		etag, known := etags[name]
		if known {
			h := w.Header()
			h.Set("ETag", etag)
			h.Set("Cache-Control", "no-cache")
			// Asset bodies vary by Accept-Encoding (some carry a gzip
			// representation), so shared caches must key on it on every path.
			h.Add("Vary", "Accept-Encoding")
		}
		if gz, ok := gzipped[name]; ok && serveGzip(w, r, etag, gz) {
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

// buildAssetMaps walks the embedded static tree once, computing a content-hash
// ETag for every file and a gzip body for each asset that compresses smaller.
func buildAssetMaps(sub fs.FS) (etags map[string]string, gzipped map[string]gzAsset, err error) {
	etags = make(map[string]string)
	gzipped = make(map[string]gzAsset)
	err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, readErr := fs.ReadFile(sub, p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(b)
		etags[p] = fmt.Sprintf(`"%x"`, sum[:])
		if gz, ok := gzipAsset(b, p); ok {
			gzipped[p] = gz
		}
		return nil
	})
	return etags, gzipped, err
}

// gzipAsset returns the gzip representation of b, or ok=false when gzip does
// not shrink it: already-compressed assets (woff2, which embeds Brotli) and
// small files whose gzip framing outweighs the savings. Plain otf/ttf outline
// fonts are NOT pre-compressed and do shrink (~30%), so the bundled .otf fonts
// ARE stored and served gzipped.
func gzipAsset(b []byte, name string) (gzAsset, bool) {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // level is constant-valid
	if _, err := zw.Write(b); err != nil {
		return gzAsset{}, false
	}
	if err := zw.Close(); err != nil {
		return gzAsset{}, false
	}
	if buf.Len() >= len(b) {
		return gzAsset{}, false
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = http.DetectContentType(b)
	}
	return gzAsset{contentType: ct, body: bytes.Clone(buf.Bytes())}, true
}

// serveGzip writes the precompressed representation of an asset and reports
// true when it handled the response. It returns false — leaving the caller to
// fall back to the identity http.FileServer — for Range requests, non-GET/HEAD
// methods, or clients that do not offer gzip.
func serveGzip(w http.ResponseWriter, r *http.Request, etag string, gz gzAsset) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Range") != "" || !acceptsGzip(r) {
		return false
	}
	h := w.Header()
	// A gzip body is a distinct representation, so it carries its own ETag;
	// Vary: Accept-Encoding (set by the caller) keeps caches from crossing it
	// with the identity body.
	gzEtag := `"` + strings.Trim(etag, `"`) + `-gz"`
	h.Set("ETag", gzEtag)
	if ifNoneMatchContains(r.Header.Get("If-None-Match"), gzEtag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Type", gz.contentType)
	h.Set("Content-Length", strconv.Itoa(len(gz.body)))
	if r.Method == http.MethodHead {
		return true
	}
	_, _ = w.Write(gz.body)
	return true
}

// acceptsGzip reports whether the request's Accept-Encoding header offers gzip
// with a non-zero quality value.
func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		token, qual := part, "1"
		if i := strings.IndexByte(part, ';'); i >= 0 {
			token = part[:i]
			if j := strings.Index(part[i:], "q="); j >= 0 {
				qual = part[i+j+2:]
			}
		}
		if !strings.EqualFold(strings.TrimSpace(token), "gzip") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(qual), 64)
		return err != nil || q != 0
	}
	return false
}

// ifNoneMatchContains reports whether an If-None-Match header value matches the
// given ETag (or the "*" wildcard).
func ifNoneMatchContains(header, etag string) bool {
	for tok := range strings.SplitSeq(header, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "*" || tok == etag {
			return true
		}
	}
	return false
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
// observability, matching the cplieger Go apps — no Prometheus endpoint).
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// The Docker HEALTHCHECK polls /healthz every 30s; logging each probe
		// at Info would dominate the Loki log stream. Log /healthz at Debug so
		// routine probes stay quiet while all other traffic stays at Info.
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug
		}
		slog.Log(context.Background(), level, "request",
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
