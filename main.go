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
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/envx"
	"github.com/cplieger/slogx"
	"github.com/cplieger/web-terminal-engine/v3/terminal"
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

// applyIntEnv parses an integer env var into *dst via envx.IntStrict, leaving
// it unchanged when the var is unset or empty. It rejects a value below min
// or a non-integer.
func applyIntEnv(key string, minVal int, dst *int) error {
	n, ok, err := envx.IntStrict(key)
	if err != nil || (ok && n < minVal) {
		return fmt.Errorf("%s must be an integer >= %d, got %q", key, minVal, os.Getenv(key))
	}
	if ok {
		*dst = n
	}
	return nil
}

// applyDurationEnv parses a Go duration env var into *dst via
// envx.DurationStrict, leaving it unchanged when unset or empty. It rejects a
// negative or unparseable duration.
func applyDurationEnv(key string, dst *time.Duration) error {
	d, ok, err := envx.DurationStrict(key)
	if err != nil || (ok && d < 0) {
		return fmt.Errorf("%s must be a non-negative Go duration, got %q", key, os.Getenv(key))
	}
	if ok {
		*dst = d
	}
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

// parseAllowedHosts reads the comma-separated WT_ALLOWED_HOSTS list of exact
// hostnames / IPs this server answers for into a webhttp.HostPolicy — the
// shared exact-match Host allowlist that closes the DNS-rebinding hole
// same-origin checks alone leave open (a rebinding attack makes Origin and
// Host AGREE, so CrossOriginProtection admits it; only an exact-Host check
// breaks that chain, CWE-346). The library owns the mechanism
// (webhttp.CanonicalHost canonicalization, X-Forwarded-Host ignored, the
// loopback peer+Host carve-out that keeps the image's own healthcheck and
// same-host curls working under any allowlist); this parser owns the app
// policy: the carve-out is enabled, the 403 names WT_ALLOWED_HOSTS, and
// malformed entries are logged (named, like parseTrustedProxies) and dropped
// per ParseHostList's drop-and-report contract.
//
// An unset or all-blank var yields an INACTIVE policy — "any Host accepted",
// the backward-compatible default; main warns when that leaves the
// unauthenticated posture open to rebinding. Any non-blank entry engages the
// gate, so a var whose entries are ALL malformed (a pasted URL, a lone
// ":7681") yields an active EMPTY policy: deny-all except the loopback
// carve-out, failing closed rather than silently unprotected — warned here by
// name, since every browser request would otherwise 403 with no hint why.
func parseAllowedHosts(key string) *webhttp.HostPolicy {
	policy, invalid := webhttp.ParseHostList(strings.Split(os.Getenv(key), ","),
		webhttp.WithLoopbackExempt(),
		webhttp.WithHostAllowlistError("host_not_allowed",
			"host not allowed; add it to WT_ALLOWED_HOSTS to serve this hostname"))
	if len(invalid) > 0 {
		slog.Warn("dropping malformed "+key+" entries; they cannot match any browser-sent Host",
			"invalid", invalid,
			"hint", "use bare hostnames or IPs only (no scheme, path, or CIDR), e.g. localhost,192.168.1.5,term.example.com; a lone port like :7681 belongs in WT_ADDR")
	}
	if policy.Active() && policy.Size() == 0 {
		slog.Warn(key+" has no usable entries; rejecting every non-loopback request (fail closed)",
			"hint", "fix the entries listed in the preceding warning to restore browser access")
	}
	return policy
}

// config holds the resolved server settings parsed from the WT_* environment.
type config struct {
	hostPolicy     *webhttp.HostPolicy
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
		addr:           envx.String("WT_ADDR", defaultAddr),
		command:        strings.Fields(envx.String("WT_CMD", defaultCmd)),
		workDir:        os.Getenv("WT_WORKDIR"),
		scrollback:     defaultScrollback,
		username:       envx.String("WT_USERNAME", defaultUsername),
		password:       os.Getenv("WT_PASSWORD"),
		trustedProxies: parseTrustedProxies("WT_TRUSTED_PROXIES"),
		hostPolicy:     parseAllowedHosts("WT_ALLOWED_HOSTS"),
	}
	if len(c.command) == 0 {
		return config{}, errors.New("WT_CMD is empty")
	}
	// Both validators run before returning so two simultaneously malformed
	// WT_* values surface in one startup failure instead of one restart apart.
	if err := errors.Join(
		applyIntEnv("WT_SCROLLBACK", 0, &c.scrollback),
		applyDurationEnv("WT_IDLE_REAPER", &c.idleReaper),
	); err != nil {
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
	// WT_LOG_LEVEL is parsed here, not in loadConfig: the level must be known
	// BEFORE the handler installs so every later record (loadConfig errors
	// included) emits at the configured level, and the parse-failure warning
	// emits AFTER Setup through the configured handler (the slogx contract).
	// A bad value is diagnosable-not-fatal: warn and run at info.
	logLevel, logLevelOK := slogx.ParseLevel(envx.String("WT_LOG_LEVEL", ""), slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: logLevel})
	if !logLevelOK {
		// Field-name-only: a compose expansion mistake could put a secret in
		// the value, so the raw string never reaches the log.
		slog.Warn("unparseable WT_LOG_LEVEL; using the info default",
			"hint", "use debug, info, warn, or error")
	}

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	warnIfExposed(cfg.addr, cfg.password)

	// DNS rebinding rides the victim's BROWSER, so it reaches even a loopback
	// or LAN bind — "keep it loopback" does not cover it. WT_PASSWORD blocks
	// it (the attacker's page cannot present credentials cross-origin), so
	// only the unauthenticated posture warrants the warning.
	if cfg.password == "" && !cfg.hostPolicy.Active() {
		slog.Warn("WT_ALLOWED_HOSTS is unset or blank and no WT_PASSWORD is set; any Host header is accepted, leaving DNS rebinding open even on loopback binds",
			"hint", "set WT_ALLOWED_HOSTS to the exact hostnames/IPs you browse to (e.g. localhost,192.168.1.5,term.example.com), or set WT_PASSWORD")
	}

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

	// BaseContext hands every request a context main can cancel on shutdown:
	// the always-open /api/sessions/events SSE handler returns only on
	// r.Context().Done(), and srv.Shutdown does not interrupt an active
	// stream, so cancelling baseCtx is what unblocks the drain instead of
	// holding it for the full grace window whenever a browser tab is open.
	// (Ported from web-terminal-kiro, which grew this for exactly this reason.)
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		slog.Error("listen failed", "addr", cfg.addr, "error", err)
		stop()
		cancelBase()
		os.Exit(1) //nolint:gocritic // stop() and cancelBase() called explicitly above; the defers are no-op safety nets
	}

	slog.Info("web-terminal-server listening",
		"addr", cfg.addr, "cmd", strings.Join(cfg.command, " "),
		"work_dir", cfg.workDir, "scrollback", cfg.scrollback,
		"auth", cfg.password != "", "idle_reaper", cfg.idleReaper)
	ready.Set(true)

	// webhttp.Run serves on the pre-bound listener and, on ctx cancellation,
	// runs the pre-drain hook, drains within the grace window, then runs the
	// teardown (session manager shutdown). The pre-drain hook flips readiness
	// false and cancels in-flight request contexts before the drain starts, so
	// /healthz reports 503 during the drain window and the open SSE streams
	// unblock (see the BaseContext comment above). A runtime serve failure
	// returns a non-nil error.
	if err := webhttp.Run(ctx, srv, ln, func(context.Context) { mgr.Shutdown() },
		webhttp.WithShutdownGrace(5*time.Second),
		webhttp.WithPreDrain(func(context.Context) {
			ready.Set(false)
			cancelBase()
			slog.Info("shutting down", "cause", context.Cause(ctx))
		})); err != nil {
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
// (webhttp.Logging) -> panic recovery -> security headers -> host allowlist
// (if configured) -> basic auth (if configured) -> cross-origin protection ->
// routes. The session handlers are
// passed in (rather than a manager constructed here) so tests can exercise the
// routing and middleware with stubs, without a real PTY. ready gates /healthz
// so load balancers see
// the server as unavailable during startup and graceful shutdown. It returns an
// error if the embedded static assets can't be opened or the CSP can't be built
// from index.html.
func newHandler(cfg *config, ws, rest, events http.Handler, ready *webhttp.Ready) (http.Handler, error) {
	mux := http.NewServeMux()
	// The engine owns its route topology: MountSessionRoutes wires exactly its
	// documented set — /ws, /api/sessions (+ subtree), /api/sessions/events —
	// and nothing else, so no engine-internal route can appear on this network
	// surface unannounced. The create gate rides webhttp's shared
	// session-create preset (burst 6, 1/s refill, standard 429 envelope), so a
	// bare (possibly unauthenticated) caller cannot fork PTY processes without
	// bound and this app cannot drift from web-terminal-kiro on tuning, path,
	// or envelope: the topology lives in the engine, the throttle policy in
	// webhttp, and this app just composes the two.
	terminal.MountSessionRoutes(mux, ws, rest, events,
		terminal.WithCreateGate(webhttp.SessionCreateRateLimit(terminal.SessionsPath)))
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
	// webhttp.StaticHandler supplies the embedded-static mechanism this app
	// used to hand-roll: per-file content-hash ETags (embed.FS reports a zero
	// ModTime, so http.FileServer alone emits no validator and every load
	// re-downloads the bundle), precomputed gzip for assets that shrink, and
	// Vary: Accept-Encoding. The default "no-cache" policy is this app's
	// policy: the vendored asset paths are stable (not content-hashed), so
	// every load revalidates (cheap 304) rather than trusting a TTL that would
	// serve stale JS after an engine/UI version bump.
	staticSrv, err := webhttp.StaticHandler(sub)
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
	//   - webhttp.Recoverer turns a downstream panic into a logged 500; inside
	//     Logging so the recovered request logs its 500, not the default 200.
	//   - webhttp.SecurityHeaders applies nosniff + the app's hash-pinned CSP
	//     (preserved byte-for-byte via WithCSP) plus the library baseline
	//     X-Frame-Options: DENY and Referrer-Policy (consistent with the CSP's
	//     frame-ancestors 'none' — this UI is never framed).
	//   - cfg.hostPolicy.Middleware — the WT_ALLOWED_HOSTS exact-host check
	//     (see parseAllowedHosts for the DNS-rebinding rationale). Placed
	//     before basicAuth so an unauthorized host is rejected 403 before any
	//     credential evaluation, and before CrossOriginProtection because
	//     rebinding makes Origin and Host agree, so the origin check alone
	//     cannot reject it. An inactive policy (env unset/blank) collapses to
	//     a pass-through per the library's off-contract.
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
		cfg.hostPolicy.Middleware(),
		authMW,
		http.NewCrossOriginProtection().Handler,
	)
	return handler, nil
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

// basicAuth gates every request behind HTTP Basic credentials, verifying each
// via webhttp's static-token verifiers (SHA-256 digests compared in constant
// time) so a wrong username or password can't be timed. Both verifiers are
// built ONCE here, pre-hashing the configured credentials, so per-request
// work hashes only what the client sent. An empty configured username or
// password fails CLOSED — the verifier rejects every presented value,
// including the empty string — so the open-endpoint case is only ever the
// explicit one: newHandler skips this middleware entirely when no password is
// configured. The browser caches the credentials after the page load and
// replays them on the same-origin WebSocket handshake, so the terminal works
// behind it.
func basicAuth(next http.Handler, username, password string) http.Handler {
	verifyUser := webhttp.NewStaticTokenVerifier(username)
	verifyPass := webhttp.NewStaticTokenVerifier(password)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		// Evaluate BOTH verifications before combining: no short-circuit may
		// skip the second compare, so a rejection's duration never reveals
		// which credential was wrong.
		userOK := verifyUser.Verify(u)
		passOK := verifyPass.Verify(p)
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
//	style-src 'unsafe-inline'  bound by index.html's inline loading-overlay
//	                           <style> (hashable if ever tightened). The
//	                           terminal renderer itself needs no relaxation:
//	                           it styles via CSSOM property setters, which
//	                           style-src does not govern
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
// in it (via webhttp.InlineScriptHashes — the byte-precise scanner that hashes
// exactly the content a browser hashes), and assembles the full CSP string.
// Called once at server construction (newHandler). It FAILS LOUD — returning
// an error rather than degrading to 'unsafe-inline' — when sub is nil,
// index.html can't be read, or the file holds no inline scripts: a valid build
// always embeds index.html with its two inline scripts (the importmap and the
// module bootstrap), so a failure here means a malformed build, which should
// abort startup with a clear message rather than silently drop the script-src
// hardening or serve a hash set that would block the browser's import-map and
// break ES module loading.
func buildCSPPolicy(sub fs.FS) (string, error) {
	if sub == nil {
		return "", errors.New("buildCSPPolicy: nil static FS")
	}
	html, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return "", fmt.Errorf("buildCSPPolicy: read index.html: %w", err)
	}
	hashes := webhttp.InlineScriptHashes(html)
	if len(hashes) == 0 {
		return "", errors.New("buildCSPPolicy: no inline <script> blocks in index.html")
	}
	return fmt.Sprintf(cspTemplate, strings.Join(hashes, " ")), nil
}
