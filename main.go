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
	"encoding/base64"
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
	"syscall"
	"time"

	"github.com/cplieger/slogx"
	"github.com/cplieger/web-terminal-engine/v2/terminal"
	"github.com/cplieger/webhttp"
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

// parseTrustedProxies reads a comma-separated list of CIDRs / bare IPs from the
// named env var into the trusted-proxy set the access log's client-IP resolver
// consults (webhttp.WithClientIP -> ClientIP). It delegates the CIDR/bare-IP
// parsing to the shared webhttp.ParseCIDRs helper.
//
// It is intentionally LENIENT: a malformed entry is logged (named) at Warn and
// skipped, and the valid subset is used, rather than aborting startup — one typo
// in an operator's proxy list should not disable proxy awareness entirely. An
// unset or empty var yields nil, i.e. "trust nothing", so ClientIP ignores
// X-Forwarded-For and logs the spoof-proof socket peer.
func parseTrustedProxies(key string) []*net.IPNet {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	nets, invalid := webhttp.ParseCIDRs(strings.Split(v, ","))
	if len(invalid) > 0 {
		slog.Warn("ignoring malformed "+key+" entries",
			"invalid", invalid,
			"hint", "each entry must be a CIDR (e.g. 10.0.0.0/8) or a bare IP (e.g. 192.168.1.5)")
	}
	return nets
}

// config holds the resolved server settings parsed from the WT_* environment.
type config struct {
	addr           string
	workDir        string
	username       string
	password       string
	command        []string
	trustedProxies []*net.IPNet
	idleReaper     time.Duration
	scrollback     int
}

// loadConfig parses and validates the WT_* environment into a config. It
// returns an error rather than exiting so the caller owns the exit path (no
// os.Exit while a defer is pending).
func loadConfig() (config, error) {
	c := config{
		addr:           envOr("WT_ADDR", defaultAddr),
		command:        strings.Fields(envOr("WT_CMD", defaultCmd)),
		workDir:        os.Getenv("WT_WORKDIR"),
		scrollback:     defaultScrollback,
		username:       envOr("WT_USERNAME", defaultUsername),
		password:       os.Getenv("WT_PASSWORD"),
		trustedProxies: parseTrustedProxies("WT_TRUSTED_PROXIES"),
	}
	if len(c.command) == 0 {
		return config{}, errors.New("WT_CMD is empty")
	}
	if err := applyIntEnv("WT_SCROLLBACK", 0, &c.scrollback); err != nil {
		return config{}, err
	}
	if err := applyDurationEnv("WT_IDLE_REAPER", &c.idleReaper); err != nil {
		return config{}, err
	}
	if c.workDir != "" {
		// WT_WORKDIR is operator-supplied configuration (the directory the
		// operator wants the shell to run in), not untrusted request input, so
		// an arbitrary absolute path is expected and correct here.
		fi, err := os.Stat(c.workDir) //nolint:gosec // G703 -- operator-controlled config path, not user input
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
	slogx.Setup(slogx.Options{})

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
	}
	if cfg.idleReaper > 0 {
		mgrOpts = append(mgrOpts, terminal.WithIdleReaper(cfg.idleReaper))
	}
	mgr := terminal.NewSessionManager(factory, mgrOpts...)

	// webhttp.Ready is the shared serving-state flag (zero value = not ready);
	// main owns its lifecycle, flipping it true after bind and false on the
	// shutdown signal. It is passed straight to webhttp.ReadinessHandler, so no
	// local adapter is needed.
	var ready webhttp.Ready

	handler, err := newHandler(&cfg, mgr.WebSocketHandler(), mgr.RESTHandler(), mgr.EventsHandler(), &ready)
	if err != nil {
		slog.Error("static assets unavailable", "error", err)
		os.Exit(1)
	}

	// webhttp.NewServer supplies the streaming-safe defaults: ReadHeaderTimeout
	// 10s, IdleTimeout 120s, MaxHeaderBytes 1 MiB, and ReadTimeout/WriteTimeout
	// left unset. Leaving Read/WriteTimeout unset is required, not incidental:
	// either would cap the lifetime of the hijacked /ws WebSocket stream.
	srv := webhttp.NewServer(handler,
		webhttp.WithErrorLog(slog.NewLogLogger(slog.Default().Handler(), slog.LevelError)),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.addr, "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop() called explicitly above; defer is a no-op safety net
	}

	// Flip readiness false the moment shutdown is signalled, before webhttp.Run
	// drains, so /healthz reports 503 during the drain window (Run's teardown
	// callback runs only after the drain completes).
	go func() {
		<-ctx.Done()
		ready.Set(false)
		slog.Info("shutting down", "cause", context.Cause(ctx))
	}()

	slog.Info("web-terminal-server listening",
		"addr", cfg.addr, "cmd", strings.Join(cfg.command, " "),
		"work_dir", cfg.workDir, "scrollback", cfg.scrollback,
		"auth", cfg.password != "", "idle_reaper", cfg.idleReaper)
	ready.Set(true)

	// webhttp.Run serves on the pre-bound listener and, on ctx cancellation,
	// drains within the grace window then runs the teardown (session manager
	// shutdown). A runtime serve failure returns a non-nil error.
	if err := webhttp.Run(ctx, srv, ln, func(context.Context) { mgr.Shutdown() },
		webhttp.WithShutdownGrace(5*time.Second)); err != nil {
		slog.Error("http server exited", "error", err)
		mgr.Shutdown()
		stop()
		os.Exit(1)
	}
	slog.Info("web-terminal-server stopped")
}

// newHandler assembles the HTTP handler: the route mux (terminal WebSocket,
// session REST API, status SSE, health, static files) wrapped in the middleware
// chain via webhttp.Chain. Middleware, outermost first: request logging
// (webhttp.Logging) -> panic recovery -> security headers -> basic auth (if
// configured) -> cross-origin protection -> routes. The session handlers are
// passed in (rather than a manager constructed here) so tests can exercise the
// routing and middleware with stubs, without a real PTY. ready gates /healthz
// so load balancers see
// the server as unavailable during startup and graceful shutdown. It returns an
// error if the embedded static assets can't be opened or the CSP can't be built
// from index.html.
func newHandler(cfg *config, ws, rest, events http.Handler, ready *webhttp.Ready) (http.Handler, error) {
	mux := http.NewServeMux()
	// Mount only the session WebSocket endpoint (not the engine's /debug/*
	// routes, which dump raw PTY output and screen state — inappropriate to
	// expose on a network service). The UI connects here with ?session=<id>.
	mux.Handle("/ws", ws)
	// Session REST API. The create rate limit gates POST /api/sessions so a
	// bare (possibly unauthenticated) caller cannot fork PTY processes without
	// bound: the limiter bounds create churn. GET (list) and DELETE (close) pass
	// through. Mounted at both the exact path and the subtree so /api/sessions
	// and /api/sessions/{id} reach the REST mux.
	limitedRest := createRateLimit(rest)
	mux.Handle("/api/sessions", limitedRest)
	mux.Handle("/api/sessions/", limitedRest)
	// The status SSE is a distinct, more-specific path than the REST subtree, so
	// ServeMux routes it here rather than to the REST DELETE /{id} pattern.
	mux.Handle("/api/sessions/events", events)
	// Serving-state gate for a load balancer: 200 {"status":"ok"} when ready,
	// 503 {"status":"unready",...} during startup/shutdown. main owns the flag
	// lifecycle (set true after bind, false on shutdown); *webhttp.Ready
	// satisfies webhttp.ReadinessChecker directly, so it is passed straight in.
	// This /healthz readiness gate is deliberately DISTINCT from a process
	// liveness marker: this app has no health-library file-marker, so /healthz
	// is its sole health endpoint (also the Docker HEALTHCHECK target).
	mux.Handle("/healthz", webhttp.ReadinessHandler(ready))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	staticSrv, err := staticHandler(sub)
	if err != nil {
		return nil, err
	}
	mux.Handle("/", staticSrv)

	// Build the CSP once from the embedded index.html so the sha256 tokens
	// pinned in script-src always match the inline scripts the browser runs
	// (no hand-maintained hash constant). FAIL LOUD: a malformed build —
	// missing/unreadable index.html or zero inline scripts — aborts startup
	// here rather than silently dropping the script-src hardening.
	cspPolicy, err := buildCSPPolicy(sub)
	if err != nil {
		return nil, fmt.Errorf("build CSP: %w", err)
	}

	// basicAuth is app policy, applied only when a password is configured. As a
	// webhttp.Middleware it slots into the Chain just inside the security
	// headers (so a 401 still carries them) and outside cross-origin protection;
	// a nil entry is skipped by Chain when no password is set.
	var authMW webhttp.Middleware
	if cfg.password != "" {
		authMW = func(next http.Handler) http.Handler {
			return basicAuth(next, cfg.username, cfg.password)
		}
	}

	// Assemble the stack with webhttp.Chain (first listed = outermost) rather
	// than hand-nesting. Order matches the fleet canonical Logging -> Recoverer
	// -> SecurityHeaders, with the app's basic-auth and cross-origin layers
	// innermost:
	//   - webhttp.Logging sits outermost so its access line records the final
	//     status, including a Recoverer-written 500. WithClientIP adds the
	//     spoof-proof "client_ip" attribute, resolved against the operator's
	//     WT_TRUSTED_PROXIES set (cfg.trustedProxies). With that set empty/unset
	//     — the default — no X-Forwarded-For is honored and the attribute is the
	//     socket peer host (the direct client on loopback, or the fronting
	//     proxy), spoof-safe. Behind a reverse proxy, set WT_TRUSTED_PROXIES to
	//     the proxy's CIDR(s) so the access log shows the real client.
	//     WithSkipPaths("/healthz") drops the routine Docker-probe access line
	//     entirely.
	//     requires webhttp >= v1.2.0 (WithClientIP + ParseCIDRs); local build via go.work replace until released.
	//     RELEASE-GATED: CI/Docker stay red until webhttp v1.2.0 ships WithClientIP + ParseCIDRs,
	//     go.mod bumps the require to it, and the go.work replace is dropped.
	//   - webhttp.Recoverer turns a downstream panic into a logged 500; inside
	//     Logging so the recovered request logs its 500, not the default 200.
	//   - webhttp.SecurityHeaders applies nosniff + the app's hash-pinned CSP
	//     (preserved byte-for-byte via WithCSP) plus the library baseline
	//     X-Frame-Options: DENY and Referrer-Policy (consistent with the CSP's
	//     frame-ancestors 'none' — this UI is never framed).
	//   - basicAuth (when configured) then http.CrossOriginProtection guard the
	//     routes.
	//
	// /healthz logging change: the former app-side accessLog logged /healthz at
	// Debug to keep the every-30s HEALTHCHECK probe quiet; WithSkipPaths now
	// omits its access line entirely. Quieter still (the routine-probe-noise
	// goal is preserved), but there is no longer a Debug-level /healthz line.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithLogger(slog.Default()), webhttp.WithSkipPaths("/healthz"), webhttp.WithClientIP(cfg.trustedProxies...)),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		webhttp.SecurityHeaders(webhttp.WithCSP(cspPolicy)),
		authMW,
		http.NewCrossOriginProtection().Handler,
	)
	return handler, nil
}

// Create-rate-limit tuning: a token bucket with a small burst (open several
// tabs at once) refilling at a steady rate, so sustained create churn is
// throttled while normal use is unaffected.
const (
	createBurst    = 6
	createInterval = time.Second // interval to accrue one create token
)

// createRateLimit gates POST /api/sessions (session creation) behind a shared
// token bucket via webhttp.RateLimiter, so a caller cannot fork PTY processes
// at an unbounded RATE (it bounds create churn, not the total live PTY
// population); list (GET) and close (DELETE) pass through unthrottled. The 429
// is the standard webhttp JSON error envelope.
func createRateLimit(next http.Handler) http.Handler {
	return webhttp.RateLimiter(createBurst, createInterval,
		webhttp.WithRateLimitWhen(func(r *http.Request) bool {
			return r.Method == http.MethodPost && r.URL.Path == "/api/sessions"
		}),
		webhttp.WithRateLimitError("rate_limited", "session creation rate exceeded"),
	)(next)
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
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if isLoopbackHost(host) {
		return
	}
	switch {
	case password == "":
		slog.Warn("listening on a non-loopback address WITHOUT authentication",
			"addr", addr,
			"risk", "anyone who can reach this address gets an interactive shell",
			"fix", "set WT_PASSWORD, bind 127.0.0.1, or front with an authenticating reverse proxy")
	case strings.TrimSpace(password) == "":
		slog.Warn("listening on a non-loopback address with a whitespace-only WT_PASSWORD",
			"addr", addr,
			"risk", "a blank/whitespace password provides negligible protection for a remote shell",
			"fix", "set a strong WT_PASSWORD or front with an authenticating reverse proxy")
	}
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

// cspTemplate is the Content-Security-Policy applied to every response, with a
// single %s placeholder for the script-src hash tokens. The tokens are computed
// once at server construction from the embedded index.html (see
// buildCSPPolicy), so an index.html edit — including a pure-whitespace prettier
// reformat of an inline script — is tracked automatically without hand-editing
// a constant. Directives other than script-src are fixed:
//
//	style-src 'unsafe-inline'  the terminal renderer sets dynamic per-cell
//	                           inline style attributes and would break without it
//	img-src 'self' data:        favicon/icon data URIs
//	connect-src 'self'          same-origin HTTP + the /ws WebSocket PTY
//	frame-ancestors 'none'      blocks clickjacking of the interactive terminal
const cspTemplate = "default-src 'self'; " +
	"script-src 'self' %s; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"form-action 'none'"

// buildCSPPolicy reads index.html from sub, hashes every inline <script>
// in it, and assembles the full CSP string. Called once at server construction
// (newHandler). It FAILS LOUD — returning an error rather than degrading to
// 'unsafe-inline' — when sub is nil, index.html can't be read, or the file
// holds no inline scripts: a valid build always embeds index.html with its two
// inline scripts (the importmap and the module bootstrap), so a failure here
// means a malformed build, which should abort startup with a clear message
// rather than silently drop the script-src hardening or serve a hash set that
// would block the browser's import-map and break ES module loading.
func buildCSPPolicy(sub fs.FS) (string, error) {
	if sub == nil {
		return "", errors.New("buildCSPPolicy: nil static FS")
	}
	html, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return "", fmt.Errorf("buildCSPPolicy: read index.html: %w", err)
	}
	hashes := inlineScriptHashes(html)
	if len(hashes) == 0 {
		return "", errors.New("buildCSPPolicy: no inline <script> blocks in index.html")
	}
	return fmt.Sprintf(cspTemplate, strings.Join(hashes, " ")), nil
}

// fallbackCSPPolicy assembles the CSP with script-src relaxed to
// 'unsafe-inline' instead of pinned hashes. It is used ONLY by tests that do
// not exercise the inline scripts; production always goes through
// buildCSPPolicy against the real embedded index.html and never relaxes
// script-src.
func fallbackCSPPolicy() string {
	return fmt.Sprintf(cspTemplate, "'unsafe-inline'")
}

// inlineScriptHashes scans HTML for inline <script> elements (those WITHOUT a
// src attribute) and returns a CSP source token 'sha256-<b64>' for each,
// hashing the exact bytes between the element's '>' and its '</script>' —
// precisely the content a browser hashes for a CSP script-src hash. External
// (src=) scripts are skipped; 'self' already covers them. It is byte-precise
// and dependency-free, and returns an empty slice on script-less or malformed
// input; buildCSPPolicy treats that empty result as a fatal build error.
func inlineScriptHashes(html []byte) []string {
	var out []string
	for i := 0; i < len(html); {
		open := findScriptOpen(html, i)
		if open < 0 {
			break
		}
		gt := openTagEnd(html, open+len("<script"))
		if gt < 0 {
			break
		}
		closeIdx := findScriptClose(html, gt+1)
		if closeIdx < 0 {
			break
		}
		if !hasSrcAttr(html[open+len("<script") : gt]) {
			out = append(out, cspHash(html[gt+1:closeIdx]))
		}
		i = closeIdx + len("</script")
	}
	return out
}

// cspHash returns the CSP source token 'sha256-<std-base64>' for content.
func cspHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// findScriptOpen returns the index at or after `from` of the next "<script"
// tag start — case-insensitive, and only where "<script" is followed by a tag
// boundary so "<scriptfoo" does not match — or -1.
func findScriptOpen(html []byte, from int) int {
	for i := from; ; {
		s := indexFoldASCII(html, i, "<script")
		if s < 0 {
			return -1
		}
		after := s + len("<script")
		if after >= len(html) || isTagNameBoundary(html[after]) {
			return s
		}
		i = after
	}
}

// findScriptClose returns the index at or after `from` of the next "</script"
// (case-insensitive), or -1.
func findScriptClose(html []byte, from int) int {
	return indexFoldASCII(html, from, "</script")
}

// openTagEnd returns the index of the '>' that closes an opening tag starting
// at `from`, skipping any '>' inside a quoted attribute value, or -1.
func openTagEnd(html []byte, from int) int {
	var quote byte
	for i := from; i < len(html); i++ {
		switch c := html[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i
		}
	}
	return -1
}

// hasSrcAttr reports whether the opening-tag attribute bytes of a <script>
// element (the bytes between "<script" and its closing '>') declare a src
// attribute. It matches `src` only at an attribute-name position and skips
// quoted values, so "srcset", "data-src", and a "src=" inside a value are not
// mistaken for it.
func hasSrcAttr(attrs []byte) bool {
	var quote byte
	atName := true
	for i := range attrs {
		switch c := attrs[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote, atName = c, false
		case isASCIISpace(c):
			atName = true
		case atName && matchesSrcHere(attrs, i):
			return true
		default:
			atName = false
		}
	}
	return false
}

// matchesSrcHere reports whether attrs at index i begins the attribute name
// "src" (case-insensitive) followed, after optional whitespace, by '=' — a real
// src attribute rather than a longer name such as "srcset".
func matchesSrcHere(attrs []byte, i int) bool {
	if !hasFoldPrefix(attrs[i:], "src") {
		return false
	}
	j := i + len("src")
	for j < len(attrs) && isASCIISpace(attrs[j]) {
		j++
	}
	return j < len(attrs) && attrs[j] == '='
}

// indexFoldASCII returns the index at or after `from` of the first
// ASCII-case-insensitive match of the lowercase literal `needle` in b, or -1.
// It scans b directly (no allocation), so returned indices address the original
// bytes — required for slicing the exact content a browser hashes.
func indexFoldASCII(b []byte, from int, needle string) int {
	for i := from; i <= len(b)-len(needle); i++ {
		if hasFoldPrefix(b[i:], needle) {
			return i
		}
	}
	return -1
}

// hasFoldPrefix reports whether b begins with the lowercase ASCII literal
// `lowerNeedle`, comparing ASCII letters case-insensitively.
func hasFoldPrefix(b []byte, lowerNeedle string) bool {
	if len(b) < len(lowerNeedle) {
		return false
	}
	for i := range len(lowerNeedle) {
		if lowerASCII(b[i]) != lowerNeedle[i] {
			return false
		}
	}
	return true
}

// lowerASCII returns c lowercased if it is an ASCII uppercase letter, else c.
func lowerASCII(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// isTagNameBoundary reports whether c ends an HTML tag name ('>', '/', or ASCII
// whitespace).
func isTagNameBoundary(c byte) bool {
	return c == '>' || c == '/' || isASCIISpace(c)
}

// isASCIISpace reports whether c is an HTML ASCII whitespace byte.
func isASCIISpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	default:
		return false
	}
}
