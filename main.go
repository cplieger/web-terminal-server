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
	defaultAddr     = "127.0.0.1:7681"
	defaultCmd      = "/bin/bash"
	defaultUsername = "admin"
)

// healthzPath is the readiness route: the target of the image's baked
// HEALTHCHECK, the path ProbeLogLevel quiets, and one of the two prefixes the
// canonical-path guard covers. Named once so those three cannot drift apart —
// the guard only protects the probe if it guards the path the probe calls.
const healthzPath = "/healthz"

// applyIntEnv parses an integer env var into *dst via envx.IntStrict, leaving
// it unchanged when the var is unset or empty. It rejects a value below min
// or a non-integer.
//
// Each rejection names what it actually rejected. A non-integer reads its value
// off envx's *ParseError (v1.6.0), never from a second os.Getenv: that returns
// the value UNTRIMMED, so it could report " 5x " while the parse that failed saw
// "5x". A below-min value has no ParseError — it parsed fine — so the number
// itself is the honest thing to name.
func applyIntEnv(key string, minVal int, dst *int) error {
	n, ok, err := envx.IntStrict(key)
	if err != nil {
		var perr *envx.ParseError
		raw := ""
		if errors.As(err, &perr) {
			raw = perr.Value
		}
		return fmt.Errorf("%s must be an integer >= %d, got %q", key, minVal, raw)
	}
	if ok && n < minVal {
		return fmt.Errorf("%s must be an integer >= %d, got %d", key, minVal, n)
	}
	if ok {
		*dst = n
	}
	return nil
}

// applyDurationEnv parses a Go duration env var into *dst via
// envx.DurationStrict, leaving it unchanged when unset or empty. It rejects a
// negative or unparseable duration.
//
// Same split as applyIntEnv: an unparseable value comes off the *ParseError, a
// negative one names the parsed duration. String() is deliberate — %q on a
// time.Duration renders a rune literal, since its underlying kind is int64.
func applyDurationEnv(key string, dst *time.Duration) error {
	d, ok, err := envx.DurationStrict(key)
	if err != nil {
		var perr *envx.ParseError
		raw := ""
		if errors.As(err, &perr) {
			raw = perr.Value
		}
		return fmt.Errorf("%s must be a non-negative Go duration, got %q", key, raw)
	}
	if ok && d < 0 {
		return fmt.Errorf("%s must be a non-negative Go duration, got %q", key, d.String())
	}
	if ok {
		*dst = d
	}
	return nil
}

// applyBoolEnv parses a boolean env var into *dst via envx.BoolStrict, leaving it
// unchanged when the var is unset or empty.
//
// Strict rather than "anything non-empty is true": this flag decides whether
// terminal output is written to browser storage, so an operator who typed
// WT_PERSIST_SCROLLBACK=flase deserves a startup error rather than a container
// that quietly persists. Accepted spellings are envx's (true/1/yes/on and
// false/0/no/off, case-insensitive).
//
// The library's error is returned verbatim, unlike applyIntEnv's and
// applyDurationEnv's rewordings, and the difference is deliberate on both sides:
// BoolStrict carries no fragment of the value (there is no parse error to wrap,
// and a boolean key is one an operator could wire to a secret by mistake), which
// is also this app's own posture everywhere it reports a bad env value — see the
// field-name-only WT_LOG_LEVEL warning in main. It already names the key and the
// accepted vocabulary, so rewording it here could only lose information.
func applyBoolEnv(key string, dst *bool) error {
	v, ok, err := envx.BoolStrict(key)
	if err != nil {
		return err
	}
	if ok {
		*dst = v
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
	hostPolicy *webhttp.HostPolicy
	// scrollback is the operator's retained-history depth, or nil when they set
	// nothing — the handler is then built WITHOUT WithScrollbackCapacity and
	// inherits the engine's default. This app holds no opinion on the depth: it
	// is a sizing decision the engine documents at
	// terminal.DefaultScrollbackCapacity, and three consumers each carrying a
	// copy is how they drift apart.
	//
	// A POINTER so "unset" is the ZERO VALUE, which matters because tests build
	// config{} by hand: 0 is a legal depth meaning "retain nothing", so an int
	// sentinel would have made every bare config{} silently disable scrollback.
	scrollback     *int
	addr           string
	workDir        string
	username       string
	password       string
	command        []string
	trustedProxies []*net.IPNet
	idleReaper     time.Duration
	// persistScrollback lets the browser keep each session's recent scrollback in
	// localStorage, so a reloaded or browser-discarded tab resumes with a delta
	// instead of refilling its whole buffer over the wire (the visible symptom on
	// iOS, which evicts backgrounded tabs). ON by default; WT_PERSIST_SCROLLBACK is
	// the opt-out, and static_persist.go carries what enabling it puts where.
	persistScrollback bool
}

// loadConfig parses and validates the WT_* environment into a config. It
// returns an error rather than exiting so the caller owns the exit path (no
// os.Exit while a defer is pending).
func loadConfig() (config, error) {
	c := config{
		addr:           envx.String("WT_ADDR", defaultAddr),
		command:        strings.Fields(envx.String("WT_CMD", defaultCmd)),
		workDir:        os.Getenv("WT_WORKDIR"),
		username:       envx.String("WT_USERNAME", defaultUsername),
		password:       os.Getenv("WT_PASSWORD"),
		trustedProxies: parseTrustedProxies("WT_TRUSTED_PROXIES"),
		hostPolicy:     parseAllowedHosts("WT_ALLOWED_HOSTS"),
		// On by default: without it a reloaded or browser-discarded tab asks for the
		// whole retained scrollback back over the wire, which is the normal case on a
		// phone and reads as a fault rather than a reload. WT_PERSIST_SCROLLBACK is
		// the opt-OUT; see static_persist.go for what enabling it puts where.
		persistScrollback: true,
	}
	if len(c.command) == 0 {
		return config{}, errors.New("WT_CMD is empty")
	}
	// Local sentinel, deliberately NOT a config field: it exists only to detect
	// "the operator said nothing" through applyIntEnv, which writes only when the
	// variable is set. Keeping it out of the struct is what leaves config{}'s
	// zero value meaning "engine default" instead of "scrollback disabled".
	scrollbackUnset := -1
	scrollbackLines := scrollbackUnset
	// Both validators run before returning so two simultaneously malformed
	// WT_* values surface in one startup failure instead of one restart apart.
	if err := errors.Join(
		applyIntEnv(terminal.ScrollbackEnvVar, 0, &scrollbackLines),
		applyDurationEnv("WT_IDLE_REAPER", &c.idleReaper),
		applyBoolEnv("WT_PERSIST_SCROLLBACK", &c.persistScrollback),
	); err != nil {
		return config{}, err
	}
	if scrollbackLines != scrollbackUnset {
		// The shallow-but-nonzero middle is honoured by the ring yet too shallow
		// for the engine to offer demand-paged history, and the browser's
		// fallback then holds MORE memory than the operator asked to save. The
		// engine owns that judgement so all three consumers apply it identically.
		capacity, reason := terminal.ClampScrollbackCapacity(scrollbackLines)
		if reason != "" {
			slog.Warn(reason)
		}
		c.scrollback = &capacity
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
	// The id is bound through terminal.LogID: it doubles as the /ws attach +
	// resume capability token, so logging it whole would put a session-access
	// credential into aggregated logs (CWE-532) — and WT_PASSWORD is optional,
	// so in the documented unauthenticated posture the token alone is enough
	// to attach. LogID is the engine's own definition of how much of it may be
	// logged (8-byte prefix + ellipsis), test-pinned there.
	mgrOpts := []terminal.ManagerOption{
		terminal.WithManagerLogger(slog.Default()),
	}
	if cfg.idleReaper > 0 {
		mgrOpts = append(mgrOpts, terminal.WithIdleReaper(cfg.idleReaper))
	}
	mgr := terminal.NewSessionManager(sessionFactory(&cfg), mgrOpts...)

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
	//
	// WithSlogErrorLog routes net/http's OWN connection-level lines — above all
	// "http: Accept error: ...; retrying", the trace of an exhausted fd budget
	// that no request-scoped line reports — into slog at Error instead of the
	// level-less standard logger no level-based log rule can match. Error is
	// this app's policy call: the process exists only to serve the terminal, so
	// an accept loop that cannot accept is an outage, not a degradation. The
	// option resolves slog.Default() as NewServer applies it, so the slogx.Setup
	// at the top of main must already have run — it has. It replaces the
	// hand-rolled slog.NewLogLogger recipe three consumers had each written out.
	srv := webhttp.NewServer(handler,
		webhttp.WithSlogErrorLog(slog.LevelError),
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

	// The effective retained-history depth, resolved for the log: an operator
	// debugging "my scrollback stops early" needs the number that is actually in
	// force, and when this app omits the option that number lives in the engine.
	scrollbackLines := terminal.DefaultScrollbackCapacity
	if cfg.scrollback != nil {
		scrollbackLines = *cfg.scrollback
	}
	slog.Info("web-terminal-server listening",
		"addr", cfg.addr, "cmd", strings.Join(cfg.command, " "),
		"work_dir", cfg.workDir, "scrollback", scrollbackLines,
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
// (if configured) -> failed-auth throttle (if configured) -> basic auth
// (if configured) -> cross-origin protection -> canonical-path guard ->
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
	mux.Handle(healthzPath, webhttp.ReadinessHandler(ready))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	// Apply the one server fact the front end reads (WT_PERSIST_SCROLLBACK) BEFORE
	// either consumer below sees the tree, so the static handler's ETag and gzip
	// body and the CSP's script hash are all computed over the bytes the browser
	// actually receives. Fails loud on a build that lost the marker.
	sub, err = applyPersistFlag(sub, cfg.persistScrollback)
	if err != nil {
		return nil, fmt.Errorf("apply WT_PERSIST_SCROLLBACK: %w", err)
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
	//
	// The failed-auth throttle in front of it is built from the SAME gate, so
	// the two read one verdict about one parse of the Authorization header (see
	// basicAuthGate). Both are left nil in the unauthenticated posture: with no
	// WT_PASSWORD there are no credentials to fail, so there is nothing to
	// throttle, and Chain skips a nil entry. That is deliberately how "inert"
	// is expressed — the middleware and its token bucket are not CONSTRUCTED at
	// all, rather than built and then bypassed by a predicate that always
	// returns false. It mirrors how basicAuth itself has always expressed
	// "absent", it leaves no live bucket in a process that can never draw from
	// it, and it makes the no-auth path byte-identical to before this control
	// existed instead of merely behaviourally equal.
	var authMW, authThrottleMW webhttp.Middleware
	if cfg.password != "" {
		gate := newBasicAuthGate(cfg.username, cfg.password)
		// webhttp.FailedAuthRateLimit (burst 10, one token per 6s, code
		// "too_many_auth_failures") bounds the guessing RATE against the single
		// static WT_PASSWORD. Without it every route sits behind a credential
		// check that answers in microseconds and nothing above it counts
		// attempts: SessionCreateRateLimit is further in AND only gates POST
		// /api/sessions, so a wrong password never reaches it, and a guessing
		// run against a remote shell proceeds as fast as the network allows.
		// Ten immediate attempts still absorb an operator retrying a rotated
		// credential by hand; sustained guessing drops to ten a minute.
		//
		// Only a request the gate is about to REFUSE draws a token, so a valid
		// credential is never throttled — not even mid-flood, which is what
		// keeps the baked healthcheck (it sends WT_USERNAME/WT_PASSWORD) and a
		// legitimate browser working while an attacker is being throttled.
		authThrottleMW = webhttp.FailedAuthRateLimit(
			func(r *http.Request) bool { return !gate.presentsValidCredentials(r) },
			"too many failed authentication attempts; check the credentials in WT_USERNAME/WT_PASSWORD")
		authMW = gate.middleware
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
	//     ProbeLogLevel(healthzPath) keeps the routine Docker-probe line at
	//     Debug and surfaces a failing probe at Warn/Error, and WithSkipUpgrades
	//     drops the line a completed /ws upgrade would emit at socket close
	//     while keeping every handshake refusal (rationale at the option).
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
	//   - authThrottleMW (when configured) — the failed-auth token bucket,
	//     placed directly in FRONT of basicAuth so a credential flood is
	//     answered 429 before the gate answers 401. It sits INSIDE Logging on
	//     purpose: slog is this app's only observability channel (no metrics
	//     endpoint), and it already refuses to silence refusals anywhere else
	//     in this stack, so the 429 must be greppable with its request id and
	//     client_ip. Outside Logging it would additionally suppress the
	//     one-line-per-attempt access flood, at the price of a throttle that
	//     fires invisibly on a remote shell — the wrong half of that trade
	//     here. It stays inside the host gate so a disallowed Host is a 403
	//     that never spends a token: a rebinding probe is not a credential
	//     attempt, and letting it drain the bucket would let an attacker
	//     throttle the real operator.
	//   - basicAuth (when configured) then http.CrossOriginProtection guard the
	//     routes — with ONE gap worth stating, because the wording used to
	//     imply otherwise: CrossOriginProtection.Check returns early for GET,
	//     HEAD and OPTIONS as safe methods, and a WebSocket handshake is a GET.
	//     So this layer never inspects the /ws upgrade, and the cross-origin
	//     gate on the terminal socket is entirely the engine's — coder/
	//     websocket's same-origin default, widened only by an explicit engine
	//     origin policy. Do not treat the CSRF middleware as covering /ws.
	//   - canonicalPathGuard is INNERMOST, wrapping the mux directly: it must
	//     see the path the mux is about to route, and nothing above it needs
	//     the verdict. Being last also means an unauthenticated or
	//     cross-origin caller is answered 401/403 and never learns anything
	//     about route spelling. Its scope is deliberately narrow — see the
	//     function for which prefixes and why the static mount is excluded.
	//
	// /healthz logging: the every-30s HEALTHCHECK probe rides the
	// fleet-standard ProbeLogLevel — healthy probes at Debug (out of the
	// shipped stream, visible under WT_LOG_LEVEL=debug), a failing probe
	// (the drain-window 503) at Warn/Error. Replaces the WithSkipPaths
	// idiom, which hid the failure signal along with the noise (and before
	// that the app-side accessLog's unconditional Debug line).
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithLogger(slog.Default()), webhttp.ProbeLogLevel(healthzPath), webhttp.WithClientIP(cfg.trustedProxies...),
			// Every /api/sessions/{id}... route embeds the FULL session id — the
			// /ws attach capability token the engine itself declares
			// log-sensitive (session_manager.go logID). Their access lines are
			// KEPT, with the recorded path rewritten to the route template the
			// mux actually matched, so a live token never reaches log-read
			// consumers; a path under the subtree that routes nowhere records
			// "/api/sessions/(unmatched)" rather than the raw path. The
			// exact-path create/list lines and the /api/sessions/events SSE line
			// are their own registered patterns, so they keep their real path.
			//
			// The prefix comes from the engine, which DECLARES these routes and
			// already treats the id as a credential, and the template comes from
			// r.Pattern -- so a route the engine adds in a future version logs
			// correctly with no change here. This replaced a local transform that
			// re-derived the engine's route table by string-parsing the path.
			// web-terminal-kiro had written the same transform independently and
			// the two had already DIVERGED on the unmatched case (it returned the
			// empty string, indistinguishable from a broken policy; this one
			// returned an "(unmapped)" marker), which is why the decision now
			// lives once in webhttp instead of once per app.
			webhttp.WithTemplatePathsUnder(terminal.SessionsSubtreePath),
			// A COMPLETED /ws upgrade gets no access line. The handshake ends the
			// HTTP exchange rather than completing it (coder/websocket records 101
			// through the ResponseWriter, then hijacks), so the only line that
			// could be emitted arrives when the socket finally closes — hours
			// later, carrying a session-length duration and a status net/http
			// never sent, describing a response that no longer exists.
			//
			// WithSkipUpgrades reads that fact from the RESPONSE (a recorded 101,
			// or a hijack taken with nothing recorded) instead of predicting from
			// the request which /ws calls will upgrade, so every handshake
			// REFUSAL on the same route KEEPS its record with its real status,
			// duration, request id and client_ip: the uniform 426 that makes /ws
			// unprobeable, a 400 on a malformed Sec-WebSocket-Key, the
			// CrossOriginProtection 403, the basicAuth 401. Those are the lines
			// an operator greps when a browser cannot attach, which is why this
			// is not WithSkipPaths("/ws") — a path skip is decided before the
			// handler runs and would take the refusals with it.
			//
			// It covers upgrades only, deliberately: /api/sessions/events is an
			// ordinary 200 that streams, so its line is still emitted at stream
			// close and still tells the truth about the status that was sent.
			webhttp.WithSkipUpgrades(),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		webhttp.SecurityHeaders(webhttp.WithCSP(cspPolicy)),
		cfg.hostPolicy.Middleware(),
		authThrottleMW,
		authMW,
		http.NewCrossOriginProtection().Handler,
		canonicalPathGuard(healthzPath, terminal.SessionsPath),
	)
	return handler, nil
}

// sessionFactory returns the per-session handler factory the session manager
// calls for each new session: a PTY-backed handler scoped to cfg's command,
// scrollback, and workdir, with a logger carrying the session id for
// correlation.
//
// The id is bound through terminal.LogID rather than raw: it doubles as the
// /ws attach + resume capability token, so logging it whole would put a
// session-access credential into aggregated logs (CWE-532) — and WT_PASSWORD
// is optional, so in the documented unauthenticated posture the token alone is
// enough to attach. LogID is the engine's own definition of how much of a
// session id may be logged (8-byte prefix + ellipsis), test-pinned there.
//
// Extracted from main so that boundary is reachable from a test; it was inline
// (and logging the full id) until an audit found the leak.
func sessionFactory(cfg *config) func(string) *terminal.Handler {
	return func(id string) *terminal.Handler {
		// terminal.WithInputTitle is deliberately NOT set. It names a session after
		// the first line typed into it, which fits a session-per-conversation agent
		// shell (web-terminal-kiro enables it) and not a general-purpose terminal:
		// here a session is a shell you run many unrelated commands in, so the
		// engine's foreground-process/cwd ladder is the better automatic label and
		// the shell's own OSC title is usually meaningful. A user who wants a fixed
		// name renames the tab (PUT /api/sessions/{id}/pinned-title), which
		// outranks every automatic source.
		opts := []terminal.Option{
			terminal.WithLogger(slog.Default().With("session", terminal.LogID(id))),
			// Keep the colours an arbitrary WT_CMD paints legible on the UI's
			// near-black background. A program picks a palette SLOT (SGR 34 for
			// blue) and cannot know what RGB this terminal resolves it to, so the
			// terminal is the only layer that can hold a legibility floor. 4.5 is
			// the WCAG AA floor for body text and VS Code's default for its
			// integrated terminal. Backgrounds and default foregrounds are never
			// touched, so a consumer's own theme keeps control of --bg and --text.
			terminal.WithMinimumContrast(4.5),
		}
		// Omitted, not defaulted, when the operator said nothing: the engine's
		// own default then applies, so a future change to it reaches this app
		// without a rebuild of its intent.
		if cfg.scrollback != nil {
			opts = append(opts, terminal.WithScrollbackCapacity(*cfg.scrollback))
		}
		if cfg.workDir != "" {
			opts = append(opts, terminal.WithWorkDir(cfg.workDir))
		}
		return terminal.NewHandler(cfg.command, opts...)
	}
}

// warnIfExposed logs a prominent warning when the server is reachable beyond
// the loopback interface without authentication — i.e. an unauthenticated
// remote shell. Bind loopback (the default) or set WT_PASSWORD, or front it
// with an authenticating reverse proxy.
//
// Classification is webhttp.ClassifyBind's classify-the-unsplit-input recipe:
// a WT_ADDR that is not host:port (a portless "127.0.0.1", a bare hostname)
// is read as a bare host and classified anyway, so a portless loopback stays
// silent and everything unrecognized warns (fail-public).
func warnIfExposed(addr, password string) {
	class := webhttp.ClassifyBind(addr)
	if class == webhttp.BindInvalid {
		class = webhttp.ClassifyBindHost(addr)
	}
	if class == webhttp.BindLoopback {
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

// basicAuthGate holds the two constant-time verifiers for the single
// operator-configured Basic credential pair and answers the one question two
// layers of this stack both need answered: does this request present valid
// credentials?
//
// It exists because that question is now asked twice and the two answers must
// never disagree. The 401 gate (middleware) refuses a request presenting
// anything else; the failed-auth throttle in front of it must draw a token from
// exactly the requests the gate is about to refuse and from no others. A second,
// independent parse of the Authorization header would be free to drift — one
// side reading a missing header as a failure while the other did not would
// either charge the healthcheck a token or leave a guessing run unthrottled.
// There is one parse here and both callers read its verdict.
//
// Both verifiers are built ONCE (newBasicAuthGate), pre-hashing the configured
// credentials, so per-request work hashes only what the client sent.
type basicAuthGate struct {
	verifyUser webhttp.StaticTokenVerifier
	verifyPass webhttp.StaticTokenVerifier
}

// newBasicAuthGate builds the gate for the configured credential pair, hashing
// both values once via webhttp's static-token verifiers (SHA-256 digests
// compared in constant time) so a wrong username or password can't be timed.
//
// An empty configured username or password fails CLOSED — the verifier rejects
// every presented value, including the empty string — so the open-endpoint case
// is only ever the explicit one: newHandler builds no gate at all when no
// password is configured.
func newBasicAuthGate(username, password string) *basicAuthGate {
	return &basicAuthGate{
		verifyUser: webhttp.NewStaticTokenVerifier(username),
		verifyPass: webhttp.NewStaticTokenVerifier(password),
	}
}

// presentsValidCredentials reports whether r carries HTTP Basic credentials
// matching the configured pair. It is the shared predicate behind both the 401
// gate and the failed-auth throttle's "did this attempt fail?" question, so
// those two can never classify the same request differently.
//
// A request with no Authorization header, a non-Basic scheme, or an
// undecodable value reports false — the same answer as wrong credentials.
// That is the right reading for both callers: the gate must refuse it, and the
// throttle must count it, because an unauthenticated flood is precisely the
// thing being bounded.
func (g *basicAuthGate) presentsValidCredentials(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	// Evaluate BOTH verifications before combining: no short-circuit may
	// skip the second compare, so a rejection's duration never reveals
	// which credential was wrong.
	userOK := g.verifyUser.Verify(u)
	passOK := g.verifyPass.Verify(p)
	return ok && userOK && passOK
}

// middleware gates every request behind the configured Basic credentials,
// answering 401 with a challenge when presentsValidCredentials says no. The
// browser caches the credentials after the page load and replays them on the
// same-origin WebSocket handshake, so the terminal works behind it.
func (g *basicAuthGate) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.presentsValidCredentials(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="web-terminal", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// canonicalPathGuard returns middleware that refuses a request whose path is
// not the spelling http.ServeMux will route, but only when the path the mux
// WOULD route it as falls under one of prefixes. Every other request passes
// through untouched.
//
// # What it prevents
//
// ServeMux cleans the request path before it selects a pattern, and answers a
// 307 with a Location when the cleaned path differs — before any handler runs,
// so no registered pattern can intercept it. To a browser that is invisible.
// To a client that does NOT follow redirects, a 307 is a SUCCESS: this image's
// baked HEALTHCHECK is `curl -sf` with no -L, so a probe sent to //healthz or
// /./healthz would exit 0 having never invoked the readiness gate — reporting
// a container healthy without ever asking it. The same shape lies to any
// scripted caller of the session API: a POST that never created a session, a
// DELETE that never closed one, each answered 307 and read as success.
//
// # Why the scope is narrow
//
// The static mount is deliberately NOT guarded. This app serves a browser
// bundle from "/", where ServeMux's cleaning redirect and http.FileServer's
// directory redirect are legitimate and wanted: a browser follows them, and
// refusing them would break relative-path asset loads that work today for no
// gain, since a static GET has no side effect a missed redirect could hide.
// The guarded prefixes are exactly the two surfaces whose callers are machines
// that may not follow redirects and whose requests mean something:
//
//   - healthzPath — the readiness probe, the motivating case above.
//   - terminal.SessionsPath ("/api/sessions") — a segment prefix, so it also
//     covers the REST subtree (/api/sessions/{id}, .../title) and the SSE
//     stream (/api/sessions/events). It comes from the engine, which owns
//     that route table, so a route the engine adds under it is covered with
//     no change here.
//
// /ws is deliberately outside the scope too. A WebSocket client cannot be
// fooled into believing a 3xx handshake succeeded (the protocol makes it a
// failure), so there is nothing to prevent — and the engine answers a uniform
// 426 there specifically to keep /ws unprobeable, which an app-shaped 400 on
// some spellings and not others would undo.
//
// # Refusal policy
//
// The status, code, and body are this app's, not the library's: a 400 in the
// same webhttp.WriteError envelope its host-allowlist 403 and rate-limit 429
// use, with a code in the same snake_case taxonomy. 400 because the request
// target is malformed, not absent (404 would deny a route that exists) and not
// unauthorized (403 would imply a permission). The message names the fix
// without echoing the received path back into the response.
func canonicalPathGuard(prefixes ...string) webhttp.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// EscapedPath is the value ServeMux itself cleans, so this verdict
			// is exactly "would the mux answer a 307 here?" — no wider. The
			// decoded r.URL.Path was the available alternative and would also
			// refuse encoded dot segments (%2e%2e), which ServeMux does NOT
			// redirect: that would invent a refusal for requests that reach
			// the handler and work today, on routes where nothing traverses a
			// filesystem path. Refuse what the redirect would have answered,
			// and nothing else.
			clean, canonical := webhttp.CanonicalRequestPath(r.URL.EscapedPath())
			if !canonical && pathUnderAny(clean, prefixes) {
				webhttp.WriteError(w, r, http.StatusBadRequest, "non_canonical_path",
					"request path is not canonical; send the route path exactly, without empty, dot, or dot-dot segments")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pathUnderAny reports whether clean equals one of prefixes or sits beneath it.
//
// It matches on whole path segments, so a prefix can never match a longer
// sibling name (/api/sessionsfoo is not under /api/sessions), and it accepts a
// prefix written with or without a trailing slash so the engine's SessionsPath
// and SessionsSubtreePath constants name the same scope.
//
// The CLEANED path is what it tests, deliberately: the raw path of a
// non-canonical request carries the wrong prefix by construction (//healthz has
// no /healthz prefix), so scoping on the raw spelling would let every attack
// spelling escape the guard. The cleaned path is where the request would have
// landed, which is the route whose semantics are at stake.
func pathUnderAny(clean string, prefixes []string) bool {
	for _, p := range prefixes {
		if clean == p || strings.HasPrefix(clean, strings.TrimSuffix(p, "/")+"/") {
			return true
		}
	}
	return false
}

// cspTemplate is the Content-Security-Policy applied to every response, with two
// %s placeholders: the script-src hash tokens and the style-src hash token. Both
// are computed once at server construction from the embedded index.html (see
// buildCSPPolicy), so an index.html edit — including a pure-whitespace prettier
// reformat of an inline script or style — is tracked automatically without
// hand-editing a constant. Directives other than script-src/style-src are fixed:
//
//	style-src 'self' <hash>    index.html's single inline loading-overlay
//	                           <style> is hash-pinned like the inline scripts,
//	                           so an injected style block cannot obscure or
//	                           spoof the terminal UI. The terminal renderer
//	                           itself needs no relaxation: it styles via CSSOM
//	                           property setters, which style-src does not
//	                           govern, and neither the UI nor the engine
//	                           template emits a style= attribute (which a hash
//	                           would NOT cover anyway — style-src-attr governs
//	                           those and needs 'unsafe-hashes')
//	img-src 'self' data:        favicon/icon data URIs
//	connect-src 'self'          same-origin HTTP + the /ws WebSocket PTY
//	frame-ancestors 'none'      blocks clickjacking of the interactive terminal
const cspTemplate = "default-src 'self'; " +
	"script-src 'self' %s; " +
	"style-src 'self' %s; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"form-action 'none'"

// buildCSPPolicy reads index.html from sub, hashes every inline <script> in it
// (via webhttp.InlineScriptHashes) plus its single inline <style> block (via
// webhttp.InlineStyleHashes) — both the byte-precise scanners that hash exactly
// the content a browser hashes — and assembles the full CSP string.
// Called once at server construction (newHandler). It FAILS LOUD — returning
// an error rather than degrading to 'unsafe-inline' — when sub is nil,
// index.html can't be read, the file holds no inline scripts, or it does not hold
// exactly one inline style block: a valid build always embeds index.html with its
// two inline scripts (the importmap and the module bootstrap) and its one
// loading-overlay style, so a failure here means a malformed build, which should
// abort startup with a clear message rather than silently drop the hardening or
// serve a hash set that would block the browser's import-map and break ES module
// loading.
//
// style-src was 'unsafe-inline' until webhttp gained InlineStyleHashes: the block
// was always hashable, but only script hashing was shared, so this server kept the
// relaxation while its web-terminal-kiro sibling hash-pinned the same page shape.
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
	styleHashes := webhttp.InlineStyleHashes(html)
	if len(styleHashes) != 1 {
		return "", fmt.Errorf(
			"buildCSPPolicy: want exactly one inline <style> block in index.html, found %d",
			len(styleHashes),
		)
	}
	return fmt.Sprintf(cspTemplate, strings.Join(hashes, " "), styleHashes[0]), nil
}
