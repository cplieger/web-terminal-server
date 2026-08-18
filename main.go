// Command web-terminal-server is a thin, generic web terminal: it runs a
// configured command in a PTY and serves the @cplieger/web-terminal-ui front
// end over HTTP + WebSocket, using github.com/cplieger/web-terminal-engine/v5.
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

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/slogx"
	"github.com/cplieger/web-terminal-engine/v5/terminal"
	"github.com/cplieger/webhttp/v2"
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

// applyIntEnv parses an integer env var into *dst via envx.IntStrict, leaving it
// unchanged when the var is unset or empty, and rejects a non-integer or a value below
// min.
//
// A rejection names the KEY and the accepted shape, never the value. This error is
// rendered into main's one startup ERROR line, and a compose interpolation mistake can
// put a credential on any variable, so echoing it would leave a durable queryable copy
// in the log store (CWE-532). A below-min value already parsed, so the int itself is safe.
func applyIntEnv(key envx.Key, minVal int, dst *int) error {
	n, ok, err := envx.IntStrict(key)
	if err != nil {
		return fmt.Errorf("%s must be an integer >= %d", key, minVal)
	}
	if ok && n < minVal {
		return fmt.Errorf("%s must be an integer >= %d, got %d", key, minVal, n)
	}
	if ok {
		*dst = n
	}
	return nil
}

// applyDurationEnv parses a Go duration env var into *dst via envx.DurationStrict,
// leaving it unchanged when unset or empty, and rejects a negative or unparseable
// duration. Same name-only rejection as applyIntEnv, for the same reason. String() is
// deliberate on the negative arm: %q on a time.Duration renders a rune literal, since its
// underlying kind is int64, and a duration that already parsed cannot be a secret.
func applyDurationEnv(key envx.Key, dst *time.Duration) error {
	d, ok, err := envx.DurationStrict(key)
	if err != nil {
		return fmt.Errorf("%s must be a non-negative Go duration", key)
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
// unchanged when the var is unset or empty. Strict rather than "anything non-empty is
// true": this flag decides whether terminal output is written to browser storage, so an
// operator who typed WT_PERSIST_SCROLLBACK=flase deserves a startup error rather than a
// container that quietly persists. The library's error is returned verbatim — it names
// the key and the accepted vocabulary already, and carries no fragment of the value.
func applyBoolEnv(key envx.Key, dst *bool) error {
	v, ok, err := envx.BoolStrict(key)
	if err != nil {
		return err
	}
	if ok {
		*dst = v
	}
	return nil
}

// parseTrustedProxies reads a comma-separated list of CIDRs / bare IPs from the named
// env var into the trusted-proxy set the access log's client-IP resolver consults.
//
// Intentionally LENIENT: a malformed entry is logged and skipped and the valid subset
// used, so one typo in an operator's proxy list cannot disable proxy awareness
// entirely. It never fails open — unset yields nil, "trust nothing", which makes
// ClientIP ignore X-Forwarded-For and log the unspoofable socket peer.
func parseTrustedProxies(key string) []*net.IPNet {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	nets, invalid := webhttp.ParseCIDRs(strings.Split(v, ","))
	if len(invalid) > 0 {
		// Count-only: a compose interpolation mistake can put a credential on any
		// variable, and echoing the rejected entries would leave a durable copy in the
		// log store (CWE-532). The hint is a FIXED string for the same reason.
		slog.Warn("ignoring malformed "+key+" entries; using the valid proxy set",
			"invalid_count", len(invalid),
			"hint", "each entry must be a CIDR (e.g. 10.0.0.0/8) or a bare IP (e.g. 192.168.1.5)")
	}
	return nets
}

// parseAllowedHosts reads the comma-separated WT_ALLOWED_HOSTS list of exact hostnames
// / IPs this server answers for into a webhttp.HostPolicy. It closes the DNS-rebinding
// hole a same-origin check alone leaves open: rebinding makes Origin and Host AGREE, so
// CrossOriginProtection admits it (CWE-346). Unset yields an INACTIVE policy, the
// backward-compatible default run warns about; entries that are ALL malformed yield an
// active EMPTY policy — deny-all but the loopback carve-out, warned by name here since
// every browser request would otherwise 403 with no hint why.
func parseAllowedHosts(key string) *webhttp.HostPolicy {
	policy, invalid := webhttp.ParseHostList(strings.Split(os.Getenv(key), ","),
		webhttp.WithLoopbackExempt(true),
		webhttp.WithHostAllowlistError("host_not_allowed",
			"host not allowed; add it to WT_ALLOWED_HOSTS to serve this hostname"))
	if len(invalid) > 0 {
		// Count-only, like parseTrustedProxies: a mis-expanded variable could put a
		// credential in an entry, so the rejected values never reach the log.
		slog.Warn("dropping malformed "+key+" entries; they cannot match any browser-sent Host",
			"invalid_count", len(invalid),
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
	// scrollback is the operator's retained-history depth, or nil when they set nothing —
	// the handler is then built WITHOUT WithScrollbackCapacity and inherits the engine's
	// default, since the depth is shared with the sibling apps and a copy per app is how
	// three consumers drift. A POINTER so "unset" is the ZERO VALUE: 0 is a legal depth
	// meaning "retain nothing", so an int sentinel would silently disable scrollback in
	// every config{} a test builds by hand.
	scrollback     *int
	addr           string
	workDir        string
	username       string
	password       string
	command        []string
	trustedProxies []*net.IPNet
	idleReaper     time.Duration
	// persistScrollback lets the browser keep each session's recent scrollback in
	// localStorage, so a reloaded or browser-discarded tab resumes with a delta instead of
	// refilling its whole buffer over the wire (the visible symptom on iOS, which evicts
	// backgrounded tabs). ON by default; static_persist.go carries the rest.
	persistScrollback bool
}

// loadConfig parses and validates the WT_* environment into a config. It returns an
// error rather than exiting because nothing below main may exit: run owns every failure
// path and main owns the single exit.
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
		// WT_WORKDIR is operator-supplied configuration, not untrusted request input, so an
		// arbitrary absolute path is expected here.
		fi, err := os.Stat(c.workDir) //nolint:gosec // G703 -- operator-controlled config path, not user input
		switch {
		// Three shapes with three remedies, kept apart because one message for all
		// three tells an operator with an unreadable mount to add a mount that is
		// already there.
		case errors.Is(err, fs.ErrNotExist):
			return config{}, fmt.Errorf("WT_WORKDIR does not exist: %q", c.workDir)
		case err != nil:
			return config{}, fmt.Errorf("WT_WORKDIR is not readable: %w", err)
		// The engine sets cmd.Dir to this path, so a regular file would pass startup and
		// fail only when the PTY child cannot spawn on the first client connect.
		case !fi.IsDir():
			return config{}, fmt.Errorf("WT_WORKDIR is not a directory: %q", c.workDir)
		}
	}
	return c, nil
}

// main is the process's ONLY exit site. Everything else lives in run, which
// reports failure by returning an error, so no startup branch can exit past a
// pending defer and skip the deferred teardown.
func main() {
	if err := run(); err != nil {
		// Rendered once, here: the failure branches in run carry their operator hint
		// inside the returned error, so a startup failure produces exactly one ERROR
		// line. stage is what a log query keys on, and a stable VALUE beats prose names:
		// prose is rewritten by any edit to the message, a stage token is not.
		slog.Error("web-terminal-server exited with error", "stage", stageOf(err), "error", err)
		os.Exit(1)
	}
}

// shutdownGrace bounds the whole graceful stop: the drain, then the session
// teardown inside whatever the drain left. The engine's own budget guidance is
// larger than this (cmd.WaitDelay alone reaps a stubborn child for 5s, and the
// containment and marker ladders each spend several grace windows on top), so a
// heavily-loaded stop can overrun it. That is the deliberate choice: this server
// would rather log the overrun than hold the container past its stop timeout.
const shutdownGrace = 5 * time.Second

// The startup stages a failure can be attributed to. Values, not messages: these
// are the strings an operator's log query or alert rule matches, so changing one
// is a breaking change to the log surface.
const (
	stageConfig = "config" // the WT_* environment is invalid
	stageStatic = "static" // the embedded static tree or its CSP is unusable
	stageListen = "listen" // the listener could not bind
	stageServe  = "serve"  // the HTTP server exited with an error
	// stageUnknown is emitted for a failure nobody attributed, so the field is
	// ALWAYS present: an absent field would make a query distinguish "no stage"
	// from "no match", and a new failure path that forgets to attribute itself
	// shows up as an explicit unknown rather than as silence.
	stageUnknown = "unknown"
)

// stageError attributes a startup failure to a stage without changing what the
// error says. It carries no message of its own precisely so the wrapped text stays
// the operator's hint, unchanged.
type stageError struct {
	err   error
	stage string
}

func (e *stageError) Error() string { return e.err.Error() }

func (e *stageError) Unwrap() error { return e.err }

// atStage attributes err to a stage.
func atStage(stage string, err error) error {
	return &stageError{stage: stage, err: err}
}

// stageOf reports the stage a failure was attributed to, or stageUnknown.
func stageOf(err error) string {
	var se *stageError
	if errors.As(err, &se) {
		return se.stage
	}
	return stageUnknown
}

// setupLogging installs the slog handler. WT_LOG_LEVEL is parsed here, not in
// loadConfig: the level must be known BEFORE the handler installs so every later
// record (loadConfig errors included) emits at the configured level, and the
// parse-failure warning emits AFTER Setup through the configured handler (the
// slogx contract). A bad value is diagnosable-not-fatal: warn and run at info.
func setupLogging() {
	logLevel, logLevelOK := slogx.ParseLevel(envx.String("WT_LOG_LEVEL", ""), slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: logLevel})
	if !logLevelOK {
		// Field-name-only: a compose expansion mistake could put a secret in
		// the value, so the raw string never reaches the log.
		slog.Warn("unparseable WT_LOG_LEVEL; using the info default",
			"hint", "use debug, info, warn, or error")
	}
}

// run is the composition root: it wires the session manager, the route table and
// the HTTP server, then blocks on the signal-driven lifecycle. Keeping the body
// here rather than in main is what lets the deferred teardown run on every
// failure path.
func run() error {
	setupLogging()

	cfg, err := loadConfig()
	if err != nil {
		return atStage(stageConfig, fmt.Errorf("invalid configuration: %w", err))
	}

	warnIfExposed(cfg.addr, cfg.password)
	warnIfPID1()

	// DNS rebinding rides the victim's BROWSER, so it reaches even a loopback
	// or LAN bind — "keep it loopback" does not cover it. WT_PASSWORD blocks
	// it (the attacker's page cannot present credentials cross-origin), so
	// only the unauthenticated posture warrants the warning.
	if cfg.password == "" && !cfg.hostPolicy.Active() {
		slog.Warn("WT_ALLOWED_HOSTS is unset or blank and no WT_PASSWORD is set; any Host header is accepted, leaving DNS rebinding open even on loopback binds",
			"hint", "set WT_ALLOWED_HOSTS to the exact hostnames/IPs you browse to (e.g. localhost,192.168.1.5,term.example.com), or set WT_PASSWORD")
	}

	// Each session gets its own PTY-backed handler; sessionFactory carries what the
	// per-session logger may say about the session id.
	mgrOpts := []terminal.ManagerOption{
		terminal.WithManagerLogger(slog.Default()),
	}
	if cfg.idleReaper > 0 {
		mgrOpts = append(mgrOpts, terminal.WithIdleReaper(cfg.idleReaper))
	}
	mgr := terminal.NewSessionManager(sessionFactory(&cfg), mgrOpts...)

	// The engine's blocking teardown, on whatever budget the caller hands it. An
	// expiry means a session's teardown (reaping the child, ending its cgroup,
	// sweeping /proc for escapees) did not finish inside the grace, which is
	// otherwise silent: there is no branch to take, because the process is
	// stopping either way, so the only useful thing to do is say so.
	shutdownSessions := func(ctx context.Context) {
		if teardownErr := mgr.Shutdown(ctx); teardownErr != nil {
			slog.Warn("session teardown did not finish within the shutdown grace",
				"grace", shutdownGrace, "error", teardownErr)
		}
	}

	// webhttp.Ready is the shared serving-state flag (zero value = not ready). run owns
	// its lifecycle, flipping it true after bind and false on the shutdown signal.
	var ready webhttp.Ready

	handler, err := newHandler(&cfg, terminal.SessionHandlers{
		WS:     mgr.WebSocketHandler(),
		REST:   mgr.RESTHandler(),
		Events: mgr.EventsHandler(),
	}, &ready)
	if err != nil {
		// The remedy rides inside the error because main renders exactly one ERROR line.
		// This stage is a BUILD defect rather than a runtime setting, which is the one
		// thing an operator reading it needs to know: no env change can fix it.
		return atStage(stageStatic, fmt.Errorf("static assets unavailable: %w"+
			" (a build defect, not a setting: the embedded static/index.html must carry at least one inline <script>,"+
			" exactly one inline <style>, and exactly one wt-persist-scrollback meta marker."+
			" Rebuild the image, or run scripts/dev-build.sh for a local tree."+
			" The container will crash-loop under its restart policy until it is rebuilt)", err))
	}

	// webhttp.NewServer supplies the streaming-safe defaults: ReadHeaderTimeout 10s,
	// IdleTimeout 120s, MaxHeaderBytes 1 MiB, and Read/WriteTimeout left unset — required,
	// since either would cap the lifetime of the hijacked /ws stream. WithSlogErrorLog
	// routes net/http's own connection-level lines, above all the "Accept error" trace of
	// an exhausted fd budget that no request-scoped line reports, into slog at Error: the
	// process exists only to serve the terminal, so an accept loop that cannot accept is
	// an outage. It resolves slog.Default() as applied, so setupLogging must precede it.
	srv := webhttp.NewServer(handler,
		webhttp.WithSlogErrorLog(slog.LevelError),
	)

	// BaseContext hands every request a context run can cancel on shutdown: the
	// always-open /api/sessions/events SSE handler returns only on r.Context().Done()
	// and srv.Shutdown never interrupts an active stream, so cancelling baseCtx is what
	// unblocks the drain instead of holding it for the full grace window whenever a
	// browser tab is open.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.addr)
	if err != nil {
		return atStage(stageListen, fmt.Errorf("listen on %s: %w", cfg.addr, err))
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

	// webhttp.Run serves on the pre-bound listener and, on ctx cancellation, runs the
	// pre-drain hook, drains within the grace window, then runs the teardown. The
	// pre-drain hook flips readiness false and cancels in-flight request contexts
	// before the drain starts, so /healthz reports 503 during the window and the open
	// SSE streams unblock (see BaseContext above).
	if err := webhttp.Run(ctx, srv, ln, shutdownSessions,
		webhttp.WithShutdownGrace(shutdownGrace),
		webhttp.WithPreDrain(func(context.Context) {
			ready.Set(false)
			cancelBase()
			slog.Info("shutting down", "cause", context.Cause(ctx))
		})); err != nil {
		// webhttp.Run does NOT run its teardown on a fatal serve error (it returns
		// before the graceful sequence), so this is the only session teardown on
		// this path rather than a duplicate of the deferred one. It gets its own
		// budget, because the one webhttp would have handed the teardown hook is
		// only created on the graceful path.
		tctx, cancelTeardown := context.WithTimeout(context.Background(), shutdownGrace)
		shutdownSessions(tctx)
		cancelTeardown()
		return atStage(stageServe, fmt.Errorf("http server exited: %w", err))
	}
	slog.Info("web-terminal-server stopped")
	return nil
}

// newHandler assembles the HTTP handler: the route mux (terminal WebSocket, session
// REST API, status SSE, health, static files) wrapped in the middleware chain via
// webhttp.Chain, outermost first — logging, panic recovery, security headers, host
// allowlist, failed-auth throttle, basic auth, cross-origin protection,
// canonical-path guard, routes. The session handlers are passed in rather than
// constructed here so a test can exercise routing and middleware with stubs and no
// real PTY. ready gates /healthz. It returns an error when the embedded static assets
// cannot be opened or the CSP cannot be built from index.html.
func newHandler(cfg *config, h terminal.SessionHandlers, ready *webhttp.Ready) (http.Handler, error) {
	mux := http.NewServeMux()
	// The engine owns its route topology: MountSessionRoutes wires exactly its documented
	// set — /ws, /api/sessions (+ subtree), /api/sessions/events — so no engine-internal
	// route can reach this network surface unannounced. The create gate rides webhttp's
	// shared session-create preset (burst 6, 1/s refill), so a bare and possibly
	// unauthenticated caller cannot fork PTY processes without bound.
	terminal.MountSessionRoutes(mux, h,
		terminal.WithCreateGate(webhttp.SessionCreateRateLimit(terminal.SessionsPath)))
	// Serving-state gate for a load balancer: 200 when ready, 503 during startup and
	// shutdown. run owns the flag lifecycle. This app has no health-library file marker,
	// so /healthz is its sole health endpoint and the Docker HEALTHCHECK target.
	mux.Handle(healthzPath, webhttp.ReadinessHandler(ready))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	// Apply the one server fact the front end reads BEFORE either consumer below sees the
	// tree, so the static handler's ETag and gzip body and the CSP's script hash are all
	// computed over the bytes the browser receives. Fails loud on a lost marker.
	sub, err = applyPersistFlag(sub, cfg.persistScrollback)
	if err != nil {
		return nil, fmt.Errorf("apply WT_PERSIST_SCROLLBACK: %w", err)
	}
	// webhttp.StaticHandler supplies the embedded-static mechanism this app used to
	// hand-roll: per-file content-hash ETags (embed.FS reports a zero ModTime, so
	// http.FileServer alone emits no validator and every load re-downloads the bundle),
	// precomputed gzip, and Vary: Accept-Encoding. The cache POLICY stays this app's.
	staticSrv, err := webhttp.StaticHandler(sub, webhttp.WithStaticCacheControl(staticCacheControl))
	if err != nil {
		return nil, err
	}
	mux.Handle("/", staticSrv)

	// Build the CSP once from the embedded index.html so the sha256 tokens pinned in
	// script-src always match the inline scripts the browser runs. FAIL LOUD: a
	// malformed build aborts startup here rather than silently dropping the hardening.
	cspPolicy, err := buildCSPPolicy(sub)
	if err != nil {
		return nil, fmt.Errorf("build CSP: %w", err)
	}

	// basicAuth is app policy, applied only when a password is configured: as a
	// webhttp.Middleware it slots into the Chain just inside the security headers, so a
	// 401 still carries them. The throttle in front of it is built from the SAME gate, so
	// the two read one verdict about one parse of the Authorization header. Both stay nil
	// without WT_PASSWORD — nothing is CONSTRUCTED rather than built and then bypassed by
	// a predicate that always returns false, and Chain skips a nil entry.
	var authMW, authThrottleMW webhttp.Middleware
	if cfg.password != "" {
		gate := newBasicAuthGate(cfg.username, cfg.password)
		// webhttp.FailedAuthRateLimit (burst 10, one token per 6s) bounds the guessing
		// RATE against the single static WT_PASSWORD. Without it every route sits behind
		// a credential check answering in microseconds and nothing above it counts
		// attempts: SessionCreateRateLimit is further in AND gates only POST
		// /api/sessions, so a wrong password never reaches it. Only a request the gate is
		// about to REFUSE draws a token, so a valid credential is never throttled, which
		// is what keeps the baked healthcheck and a real browser working mid-flood.
		authThrottleMW = webhttp.FailedAuthRateLimit(
			func(r *http.Request) bool { return !gate.presentsValidCredentials(r) },
			"too many failed authentication attempts; check the credentials in WT_USERNAME/WT_PASSWORD")
		authMW = gate.middleware
	}

	// Assembled with webhttp.Chain, first listed = outermost, nil entries skipped. The
	// order is the fleet canonical Logging -> Recoverer -> SecurityHeaders with this app's
	// auth and cross-origin layers innermost. Recoverer sits inside Logging so a recovered
	// request logs its 500 rather than the default 200. WithClientIP resolves the
	// spoof-proof client_ip against cfg.trustedProxies; unset, no X-Forwarded-For is
	// honoured and the attribute is the socket peer. ProbeLogLevel keeps the HEALTHCHECK
	// line at Debug and raises a FAILING probe to Warn/Error, which a path skip hid.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithLogger(slog.Default()), webhttp.ProbeLogLevel(healthzPath), webhttp.WithClientIP(cfg.trustedProxies...),
			// Every /api/sessions/{id}... route embeds the FULL session id, the /ws attach
			// capability token the engine itself declares log-sensitive. Those access lines are
			// KEPT, with the recorded path rewritten to the route template the mux matched, so a
			// live token never reaches a log-read consumer; a path under the subtree that routes
			// nowhere records "(unmatched)". The prefix comes from the engine, which DECLARES
			// these routes, and the template from r.Pattern, so a route added later logs
			// correctly with no change here.
			webhttp.WithTemplatePathsUnder(terminal.SessionsSubtreePath),
			// A COMPLETED /ws upgrade gets no access line: the handshake ends the HTTP exchange
			// rather than completing it, so the only line emittable arrives at socket close,
			// hours later, carrying a session-length duration and a status net/http never sent.
			// WithSkipUpgrades reads that off the RESPONSE, so every handshake REFUSAL keeps its
			// record — the uniform 426, a malformed-key 400, the CrossOriginProtection 403, the
			// basicAuth 401 — which is why this is not WithSkipPaths("/ws"): a path skip is
			// decided before the handler runs and would take the refusals with it.
			webhttp.WithSkipUpgrades(true),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		// One record per /ws UPGRADE attempt, outside the host gate so a rejected Host
		// still leaves one. See wsAttachLog: without it the request that PRESENTS the
		// session capability token is the only request here with no record at all.
		wsAttachLog(cfg.trustedProxies),
		webhttp.SecurityHeaders(
			webhttp.WithCSP(cspPolicy),
			// COOP severs window.opener for any cross-origin page a session opens. The
			// terminal renders clickable OSC 8 hyperlinks straight out of untrusted child
			// output — WT_CMD is arbitrary — so a session can be induced to open an attacker
			// page; the vendored UI already sets rel="noopener noreferrer", and this header
			// plus the tightened Referrer-Policy make that guarantee independent of the
			// Renovate-bumped UI pin. COOP and the Permissions-Policy features are
			// secure-context-gated, so inert on a plain-HTTP LAN bind; Referrer-Policy is not.
			webhttp.WithCOOP("same-origin"),
			webhttp.WithReferrerPolicy("same-origin"),
			webhttp.WithPermissionsPolicy("camera=(), microphone=(), geolocation=()"),
		),
		// The exact-Host check (rationale at parseAllowedHosts) precedes basicAuth so a
		// disallowed host never reaches credential evaluation, and precedes
		// CrossOriginProtection because rebinding makes Origin and Host agree, so the
		// origin check alone cannot reject it. An inactive policy is a pass-through.
		cfg.hostPolicy.Middleware(),
		// Directly in FRONT of basicAuth, so a credential flood is answered 429 before the
		// gate answers 401 — but INSIDE Logging, because slog is this app's only
		// observability channel and a throttle that fires invisibly on a remote shell is
		// the wrong trade, and inside the host gate, so a rebinding probe cannot drain the
		// bucket and throttle the real operator.
		authThrottleMW,
		authMW,
		// ONE gap worth stating: CrossOriginProtection.Check returns early for GET, HEAD
		// and OPTIONS as safe methods, and a WebSocket handshake is a GET. So this layer
		// never inspects the /ws upgrade, and the cross-origin gate on the terminal socket
		// is entirely the engine's (coder/websocket's same-origin default, widened only by
		// an explicit engine origin policy). Do not read this as covering /ws.
		http.NewCrossOriginProtection().Handler,
		// Innermost, wrapping the mux directly: it must see the path the mux is about to
		// route, and nothing above it needs the verdict. Being last also means an
		// unauthenticated or cross-origin caller is answered 401/403 and learns nothing
		// about route spelling.
		canonicalPathGuard(healthzPath, terminal.SessionsPath),
	)
	return handler, nil
}

// staticCacheControl is the per-asset Cache-Control policy handed to
// webhttp.StaticHandler, which supplies the ETag and gzip mechanism (asset paths arrive
// normalized, with no leading slash). Everything but the fonts revalidates on every load:
// the vendored asset paths are stable rather than content-hashed, so a TTL there would
// serve stale JS after an engine/UI bump. Fonts get 30 days but NOT `immutable`,
// deliberately — their @font-face URLs use fixed names, so the bytes change under one
// filename on a Monaspace bump and a reload must still revalidate against the ETag.
func staticCacheControl(assetPath string) string {
	if strings.HasPrefix(assetPath, "vendor/fonts/") {
		return "public, max-age=2592000"
	}
	return "no-cache, must-revalidate"
}

// wsAttachMsg is the /ws attach record's message, named so a test can pin it without a
// second copy of the literal drifting from this one.
const wsAttachMsg = "terminal attach attempt"

// wsAttachLog records one line per /ws UPGRADE attempt. The access logger skips admitted
// streams (WithSkipUpgrades) and neither the engine's WebSocketHandler nor the per-session
// Handler logs an attach, so without this the request that PRESENTS the session capability
// token is the only request to this server with no record at all: a leaked id could be
// replayed with nothing to show an operator afterwards (CWE-778). Logged at request start,
// so a rejected Host or an unknown-session close is recorded too, and the caller-chosen
// session param is bound through terminal.LogID rather than logged raw.
func wsAttachLog(trustedProxies []*net.IPNet) webhttp.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == terminal.WSPath && isWebSocketUpgrade(r) {
				slog.Info(wsAttachMsg,
					"session", terminal.LogID(terminal.SessionID(r.URL.Query().Get("session"))),
					"client_ip", webhttp.ClientIP(r, trustedProxies...),
					"request_id", webhttp.RequestIDFromContext(r.Context()))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isWebSocketUpgrade is wsAttachLog's ATTEMPT predicate, a different question from the
// access log's: that one asks whether a request ENDED as a stream, after the fact. This
// asks whether a request is trying to attach, BEFORE the handshake runs, because the
// session capability token is in the query string and every attempt that looks like an
// attach must leave its audit record even when the handshake is malformed. An outcome
// cannot replace it: by then the request that presented the token may already be refused.
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r, "Upgrade", "websocket") &&
		headerHasToken(r, "Connection", "upgrade")
}

// headerHasToken reports whether a comma-separated header names token, case-insensitively.
func headerHasToken(r *http.Request, name, token string) bool {
	for _, v := range r.Header.Values(name) {
		for opt := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(opt), token) {
				return true
			}
		}
	}
	return false
}

// sessionFactory returns the per-session handler factory the session manager calls for
// each new session: a PTY-backed handler scoped to cfg's command, scrollback and workdir.
// The id is bound through terminal.LogID rather than raw because it doubles as the /ws
// attach and resume capability token, so logging it whole would put a session-access
// credential into aggregated logs (CWE-532) — and WT_PASSWORD is optional, so in the
// documented unauthenticated posture the token alone is enough to attach.
func sessionFactory(cfg *config) func(terminal.SessionID) *terminal.Handler {
	return func(id terminal.SessionID) *terminal.Handler {
		opts := []terminal.Option{
			terminal.WithLogger(slog.Default().With("session", terminal.LogID(id))),
			// The engine logs the child's full argv at process start. WT_CMD is
			// operator-supplied and whitespace-split, and the README's own guidance sends
			// complex invocations through it, so an argv carrying a token is plausible —
			// once per SESSION into aggregated logs (CWE-532). The startup line stays the
			// one authoritative record of what this container runs.
			terminal.WithCommandLogValue("[redacted]"),
			// Keep the colours an arbitrary WT_CMD paints legible on the UI's near-black
			// background. A program picks a palette SLOT (SGR 34 for blue) and cannot know
			// what RGB this terminal resolves it to, so the terminal is the only layer that
			// can hold a legibility floor. 4.5 is the WCAG AA floor for body text.
			// Backgrounds and default foregrounds are never touched, so a consumer's own
			// theme keeps control of --bg and --text.
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

// warnIfPID1 reports that no container init is present, which is detectable here in one
// comparison. Orphan reaping is the init's job, not this server's: whatever WT_CMD runs can
// fork a child that outlives its own parent, the kernel reparents that orphan onto PID 1,
// and os/exec waits only for children this process started — so with the server AT PID 1
// every orphan stays a zombie for the container's lifetime, and the engine's session reaping
// makes it worse, since each descendant it kills is another status only PID 1 can collect.
// Warn rather than fatal: the container still serves a terminal, and the fix is one flag.
func warnIfPID1() {
	if os.Getpid() != 1 {
		return
	}
	slog.Warn("running as PID 1 with no container init; processes orphaned by a session will accumulate as zombies for the container's lifetime",
		"hint", "start the image with `docker run --init`, or set `init: true` on the compose service")
}

// warnIfExposed logs a prominent warning when the server is reachable beyond the
// loopback interface without authentication, which is an unauthenticated remote shell.
//
// Classification follows webhttp.ClassifyBind's classify-the-unsplit-input recipe: a
// WT_ADDR that is not host:port (a portless "127.0.0.1", a bare hostname) is read as a
// bare host and classified anyway, so a portless loopback stays silent and everything
// unrecognized warns. Fail-public.
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
// operator-configured Basic credential pair and answers the one question two layers of
// this stack both need: does this request present valid credentials? One parse, because a
// second independent parse of the Authorization header would be free to drift — one side
// reading a missing header as a failure while the other did not would either charge the
// healthcheck a token or leave a guessing run unthrottled.
type basicAuthGate struct {
	verifyUser webhttp.StaticTokenVerifier
	verifyPass webhttp.StaticTokenVerifier
}

// newBasicAuthGate builds the gate for the configured credential pair, hashing both
// values ONCE via webhttp's static-token verifiers (SHA-256 digests compared in constant
// time) so per-request work hashes only what the client sent. An empty configured
// username or password fails CLOSED — the verifier rejects every presented value — so
// the open-endpoint case is only ever the explicit one where newHandler builds no gate.
func newBasicAuthGate(username, password string) *basicAuthGate {
	return &basicAuthGate{
		verifyUser: webhttp.NewStaticTokenVerifier(username),
		verifyPass: webhttp.NewStaticTokenVerifier(password),
	}
}

// presentsValidCredentials reports whether r carries HTTP Basic credentials matching the
// configured pair. A request with no Authorization header, a non-Basic scheme, or an
// undecodable value reports false, the same answer as wrong credentials — right for both
// callers, since the gate must refuse it and the throttle must count it.
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

// canonicalPathRefusal is the guard's response message, a const so a test can pin the
// wording without a second copy of the literal drifting from this one (the same reason
// wsAttachMsg is one). It names the remedy AND why a refusal beats a redirect, and echoes
// no part of the request: net/http accepts up to MaxHeaderBytes (1 MiB by default) of
// request line, so reflecting the path would make the body caller-sized.
const canonicalPathRefusal = "request path is not canonical; send the route path exactly, without empty, dot, or dot-dot segments " +
	"(this route refuses rather than redirecting, because a redirect is a success status to a client without -L)"

// canonicalPathGuard refuses a request whose path is not the spelling http.ServeMux
// will route, when the path the mux WOULD route it as falls under one of prefixes.
// ServeMux answers 307 when the cleaned path differs, and to a curl without -L a 307 is
// a SUCCESS: this image's HEALTHCHECK would exit 0 having never consulted readiness,
// and a scripted POST would create no session yet read as success. Out of scope on
// purpose: the static mount, where a browser follows the redirect and a GET hides no
// side effect, and /ws, where a 3xx handshake already fails and the engine's uniform
// 426 keeps the route unprobeable. The refusal is a 400 — the target is malformed.
func canonicalPathGuard(prefixes ...string) webhttp.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// EscapedPath is the value ServeMux itself cleans, so this verdict is exactly
			// "would the mux answer a 307 here?" and no wider. The decoded r.URL.Path was
			// the alternative and would also refuse encoded dot segments (%2e%2e), which
			// ServeMux does NOT redirect — inventing a refusal for requests that reach the
			// handler and work today, on routes where nothing traverses a filesystem path.
			clean, canonical := webhttp.CanonicalRequestPath(r.URL.EscapedPath())
			if !canonical && pathUnderAny(clean, prefixes) {
				webhttp.WriteError(w, r, http.StatusBadRequest, "non_canonical_path", canonicalPathRefusal)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pathUnderAny reports whether clean equals one of prefixes or sits beneath it. It
// matches whole path segments, so a prefix can never match a longer sibling name
// (/api/sessionsfoo is not under /api/sessions), and a prefix is NORMALIZED so the
// engine's SessionsPath and SessionsSubtreePath name one scope — the exact path included,
// which a trailing-slash prefix used to miss, silently dropping the create route. It tests
// the CLEANED path deliberately: a non-canonical request carries the wrong prefix by
// construction, so scoping on the raw spelling would let every attack spelling escape.
func pathUnderAny(clean string, prefixes []string) bool {
	for _, p := range prefixes {
		root := strings.TrimSuffix(p, "/")
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}

// cspTemplate is the Content-Security-Policy applied to every response, with two %s
// placeholders: the script-src hash tokens and the style-src hash token. Both are
// computed once at construction from the embedded index.html, so an index.html edit is
// tracked without hand-editing a constant. style-src is hash-pinned rather than
// 'unsafe-inline' because the renderer needs no relaxation: it styles via CSSOM property
// setters, which style-src does not govern, and nothing emits a style= attribute.
const cspTemplate = "default-src 'self'; " +
	"script-src 'self' %s; " +
	"style-src 'self' %s; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"form-action 'none'"

// buildCSPPolicy reads index.html from sub, hashes every inline <script> in it plus its
// single inline <style> block with webhttp's byte-precise scanners, and assembles the full
// CSP string. Called once at construction, with an FS its one caller already validated:
// newHandler returns on both fs.Sub's error and applyPersistFlag's, so a nil guard here
// would be unreachable. It FAILS LOUD rather than degrading to 'unsafe-inline' when
// index.html cannot be read, holds no inline script, or holds other than exactly one
// inline style block — a valid build embeds two scripts and one style, so that is a
// malformed build.
func buildCSPPolicy(sub fs.FS) (string, error) {
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
