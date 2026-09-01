// Command web-terminal-server is a thin, generic web terminal: it runs a
// configured command in a PTY and serves the @cplieger/web-terminal-ui front
// end over HTTP + WebSocket, using github.com/cplieger/web-terminal-engine/v5.
//
// SECURITY: this is a remote shell. Anyone who can reach the listen address
// and pass auth (if any) gets an interactive process running SESSION_CMD with this
// server's privileges. It binds loopback (127.0.0.1) by default; only expose
// it on a public interface behind an authenticating reverse proxy, or set
// AUTH_PASSWORD. See README.md.
package main

import (
	"cmp"
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

// staticFS holds the bundled front end. A fresh checkout commits only
// static/index.html; the dev-build script and the Dockerfile generate the
// compiled assets before `go build`.
//
//go:embed static
var staticFS embed.FS

const (
	defaultAddr     = "127.0.0.1:7681"
	defaultCmd      = "/bin/bash"
	defaultUsername = "admin"
)

// healthzPath is the readiness route: the image's baked HEALTHCHECK target,
// the path ProbeLogLevel quiets, and one of the two prefixes the
// canonical-path guard covers.
const healthzPath = "/healthz"

// applyIntEnv parses an integer env var into *dst, leaving it unchanged when
// unset/empty, and rejects a non-integer or a value below min.
//
// A rejection names the key and accepted shape only, never the value: a
// compose interpolation mistake can put a credential on any variable, and
// this error reaches main's one startup log line (CWE-532).
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

// applyDurationEnv parses a Go duration env var into *dst, same name-only
// rejection as applyIntEnv. String() on the negative arm is deliberate: %q on
// a time.Duration renders a rune literal (its underlying kind is int64).
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

// applyBoolEnv parses a boolean env var into *dst. Strict, not "any non-empty
// is true": this flag decides whether output is written to browser storage,
// so a typo like PERSIST_SCROLLBACK=flase should fail startup, not persist
// silently.
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

// parseTrustedProxies reads a comma-separated CIDR/IP list into the
// trusted-proxy set the access log's client-IP resolver consults.
//
// Lenient: a malformed entry is skipped rather than disabling proxy
// awareness entirely. Never fails open — unset yields nil, so ClientIP
// ignores X-Forwarded-For and logs the unspoofable socket peer.
func parseTrustedProxies(key string) []*net.IPNet {
	v := os.Getenv(key)
	if v == "" {
		return nil
	}
	nets, invalid := webhttp.ParseCIDRs(strings.Split(v, ","))
	if len(invalid) > 0 {
		// Count-only: a compose interpolation mistake could put a credential
		// on this variable, so the rejected entries never reach the log.
		slog.Warn("ignoring malformed "+key+" entries; using the valid proxy set",
			"invalid_count", len(invalid),
			"hint", "each entry must be a CIDR (e.g. 10.0.0.0/8) or a bare IP (e.g. 192.168.1.5)")
	}
	return nets
}

// parseAllowedHosts reads the comma-separated ALLOWED_HOSTS list into a
// webhttp.HostPolicy. Closes the DNS-rebinding hole a same-origin check alone
// leaves open: rebinding makes Origin and Host agree, so CrossOriginProtection
// admits it (CWE-346). Unset yields an inactive policy; all-malformed yields
// an active empty policy (deny-all but loopback).
func parseAllowedHosts(key string) *webhttp.HostPolicy {
	policy, invalid := webhttp.ParseHostList(strings.Split(os.Getenv(key), ","),
		webhttp.WithLoopbackExempt(true),
		webhttp.WithHostAllowlistError("host_not_allowed",
			"host not allowed; add it to ALLOWED_HOSTS to serve this hostname"))
	if len(invalid) > 0 {
		// Count-only, like parseTrustedProxies: a mis-expanded variable could
		// carry a credential.
		slog.Warn("dropping malformed "+key+" entries; they cannot match any browser-sent Host",
			"invalid_count", len(invalid),
			"hint", "use bare hostnames or IPs only (no scheme, path, or CIDR), e.g. localhost,192.168.1.5,term.example.com; a lone port like :7681 belongs in LISTEN_ADDR")
	}
	if policy.Active() && policy.Size() == 0 {
		slog.Warn(key+" has no usable entries; rejecting every non-loopback request (fail closed)",
			"hint", "fix the entries listed in the preceding warning to restore browser access")
	}
	return policy
}

// config holds the resolved server settings parsed from the environment.
type config struct {
	hostPolicy *webhttp.HostPolicy
	// scrollback is the operator's depth, or nil when unset — the handler is
	// then built without WithScrollbackCapacity and inherits the engine's
	// default (shared across sibling apps). A pointer so "unset" is the zero
	// value: 0 is a legal depth meaning "retain nothing".
	scrollback     *int
	addr           string
	workDir        string
	username       string
	password       string
	command        []string
	trustedProxies []*net.IPNet
	idleReaper     time.Duration
	// persistScrollback lets the browser keep recent scrollback in
	// localStorage so a reloaded/discarded tab resumes with a delta. ON by
	// default; static_persist.go carries the rest.
	persistScrollback bool
}

// loadConfig parses and validates the environment. Returns an error rather
// than exiting: run owns every failure path, main owns the single exit.
func loadConfig() (config, error) {
	c := config{
		addr:           cmp.Or(envx.String("LISTEN_ADDR"), defaultAddr),
		command:        strings.Fields(cmp.Or(envx.String("SESSION_CMD"), defaultCmd)),
		workDir:        os.Getenv("WORK_DIR"),
		username:       cmp.Or(envx.String("AUTH_USERNAME"), defaultUsername),
		password:       os.Getenv("AUTH_PASSWORD"),
		trustedProxies: parseTrustedProxies("TRUSTED_PROXIES"),
		hostPolicy:     parseAllowedHosts("ALLOWED_HOSTS"),
		// PERSIST_SCROLLBACK is the opt-OUT; see static_persist.go.
		persistScrollback: true,
	}
	if len(c.command) == 0 {
		return config{}, errors.New("SESSION_CMD is empty")
	}
	// Local sentinel, not a config field: exists only to detect "the operator
	// said nothing" via applyIntEnv, which writes only when set. Keeping it
	// out of the struct is what leaves config{}'s zero value meaning "engine
	// default" instead of "scrollback disabled".
	scrollbackUnset := -1
	scrollbackLines := scrollbackUnset
	// Both validators run before returning so two simultaneously malformed
	// values surface in one startup failure instead of one restart apart.
	if err := errors.Join(
		applyIntEnv(terminal.ScrollbackEnvVar, 0, &scrollbackLines),
		applyDurationEnv("IDLE_TIMEOUT", &c.idleReaper),
		applyBoolEnv("PERSIST_SCROLLBACK", &c.persistScrollback),
	); err != nil {
		return config{}, err
	}
	if scrollbackLines != scrollbackUnset {
		// The shallow-but-nonzero middle is honoured by the ring yet too
		// shallow for demand-paged history; the engine owns this clamp so all
		// three consumers apply it identically.
		capacity, reason := terminal.ClampScrollbackCapacity(scrollbackLines)
		if reason != "" {
			slog.Warn(reason)
		}
		c.scrollback = &capacity
	}
	if c.workDir != "" {
		// Operator-supplied configuration, not untrusted request input.
		fi, err := os.Stat(c.workDir)
		switch {
		// Three shapes, three remedies: one message for all three would tell
		// an operator with an unreadable mount to add a mount already there.
		case errors.Is(err, fs.ErrNotExist):
			return config{}, fmt.Errorf("WORK_DIR does not exist: %q", c.workDir)
		case err != nil:
			return config{}, fmt.Errorf("WORK_DIR is not readable: %w", err)
		// The engine sets cmd.Dir here, so a regular file passes startup and
		// fails only when the PTY child can't spawn on first connect.
		case !fi.IsDir():
			return config{}, fmt.Errorf("WORK_DIR is not a directory: %q", c.workDir)
		}
	}
	return c, nil
}

// main is the process's ONLY exit site. Everything else lives in run, which
// reports failure by returning an error, so no branch can exit past a pending
// defer.
func main() {
	if err := run(); err != nil {
		// stage is what a log query keys on: a stable value beats prose,
		// which any message edit would rewrite.
		slog.Error("web-terminal-server exited with error", "stage", stageOf(err), "error", err)
		os.Exit(1)
	}
}

// shutdownGrace bounds the whole graceful stop. The engine's own teardown
// budget can exceed this (cmd.WaitDelay alone reaps a stubborn child for 5s),
// so this server would rather log the overrun than hold the container past
// its stop timeout.
const shutdownGrace = 5 * time.Second

// The startup stages a failure can be attributed to. Values, not messages —
// an operator's log query or alert rule matches these, so renaming one is a
// breaking change to the log surface.
const (
	stageConfig = "config" // the environment is invalid
	stageStatic = "static" // the embedded static tree or its CSP is unusable
	stageListen = "listen" // the listener could not bind
	stageServe  = "serve"  // the HTTP server exited with an error
	// stageUnknown is emitted for a failure nobody attributed, so a query can
	// distinguish "no stage" from "no match".
	stageUnknown = "unknown"
)

// stageError attributes a startup failure to a stage without changing what
// the error says.
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
	if se, ok := errors.AsType[*stageError](err); ok {
		return se.stage
	}
	return stageUnknown
}

// setupLogging installs the slog handler. LOG_LEVEL is parsed here, not in
// loadConfig, so the level is known BEFORE the handler installs and every
// later record emits at the configured level. A bad value is
// diagnosable-not-fatal: warn and run at info.
func setupLogging() {
	logLevel, logLevelOK := slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	slogx.Setup(slogx.Options{Level: logLevel})
	if !logLevelOK {
		// Field-name-only: a compose expansion mistake could put a secret in
		// the value.
		slog.Warn("unparseable LOG_LEVEL; using the info default",
			"hint", "use debug, info, warn, or error")
	}
}

// run is the composition root: wires the session manager, route table and
// HTTP server, then blocks on the signal-driven lifecycle. Keeping the body
// here rather than in main lets the deferred teardown run on every failure path.
func run() error {
	setupLogging()

	cfg, err := loadConfig()
	if err != nil {
		return atStage(stageConfig, fmt.Errorf("invalid configuration: %w", err))
	}

	warnIfExposed(cfg.addr, cfg.password)
	warnIfPID1()

	// DNS rebinding rides the victim's browser, so it reaches even a
	// loopback/LAN bind. AUTH_PASSWORD blocks it (the attacker's page can't
	// present credentials cross-origin), so only the unauthenticated posture
	// warrants the warning.
	if cfg.password == "" && !cfg.hostPolicy.Active() {
		slog.Warn("ALLOWED_HOSTS is unset or blank and no AUTH_PASSWORD is set; any Host header is accepted, leaving DNS rebinding open even on loopback binds",
			"hint", "set ALLOWED_HOSTS to the exact hostnames/IPs you browse to (e.g. localhost,192.168.1.5,term.example.com), or set AUTH_PASSWORD")
	}

	// Each session gets its own PTY-backed handler.
	mgrOpts := []terminal.ManagerOption{
		terminal.WithManagerLogger(slog.Default()),
	}
	if cfg.idleReaper > 0 {
		mgrOpts = append(mgrOpts, terminal.WithIdleReaper(cfg.idleReaper))
	}
	mgr := terminal.NewSessionManager(sessionFactory(&cfg), mgrOpts...)

	// Blocking teardown on whatever budget the caller hands it. An expiry
	// means a session's teardown didn't finish inside the grace — otherwise
	// silent, since the process is stopping either way.
	shutdownSessions := func(ctx context.Context) {
		if teardownErr := mgr.Shutdown(ctx); teardownErr != nil {
			slog.Warn("session teardown did not finish within the shutdown grace",
				"grace", shutdownGrace, "error", teardownErr)
		}
	}

	// webhttp.Ready is the shared serving-state flag. run owns its lifecycle.
	var ready webhttp.Ready

	handler, err := newHandler(&cfg, terminal.SessionHandlers{
		WS:     mgr.WebSocketHandler(),
		REST:   mgr.RESTHandler(),
		Events: mgr.EventsHandler(),
	}, &ready)
	if err != nil {
		// The remedy rides inside the error since main renders exactly one
		// line. This stage is a BUILD defect, not a runtime setting.
		return atStage(stageStatic, fmt.Errorf("static assets unavailable: %w"+
			" (a build defect, not a setting: the embedded static/index.html must carry at least one inline <script>,"+
			" exactly one inline <style>, and exactly one wt-persist-scrollback meta marker."+
			" Rebuild the image, or run scripts/dev-build.sh for a local tree."+
			" The container will crash-loop under its restart policy until it is rebuilt)", err))
	}

	// webhttp.NewServer supplies streaming-safe defaults (ReadHeaderTimeout
	// 10s, IdleTimeout 120s, no Read/WriteTimeout — either would cap the
	// hijacked /ws stream's lifetime). WithSlogErrorLog routes net/http's own
	// connection-level lines into slog at Error, since an accept loop that
	// can't accept is an outage for a process that exists only to serve this.
	srv := webhttp.NewServer(handler,
		webhttp.WithSlogErrorLog(slog.LevelError),
	)

	// BaseContext lets run cancel every in-flight request on shutdown: the
	// always-open SSE handler returns only on r.Context().Done(), and
	// srv.Shutdown never interrupts an active stream.
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

	// The effective retained-history depth, resolved for the log: when this
	// app omits the option that number lives in the engine.
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
	// runs the pre-drain hook, drains within the grace window, then runs
	// teardown. The pre-drain hook flips readiness false and cancels
	// in-flight contexts before the drain starts, so /healthz reports 503
	// during the window and open SSE streams unblock (see BaseContext above).
	if err := webhttp.Run(ctx, srv, ln, shutdownSessions,
		webhttp.WithShutdownGrace(shutdownGrace),
		webhttp.WithPreDrain(func(context.Context) {
			ready.Set(false)
			cancelBase()
			slog.Info("shutting down", "cause", context.Cause(ctx))
		})); err != nil {
		// webhttp.Run does not run its teardown on a fatal serve error (it
		// returns before the graceful sequence), so this is the only
		// teardown on this path, with its own budget.
		tctx, cancelTeardown := context.WithTimeout(context.Background(), shutdownGrace)
		shutdownSessions(tctx)
		cancelTeardown()
		return atStage(stageServe, fmt.Errorf("http server exited: %w", err))
	}
	slog.Info("web-terminal-server stopped")
	return nil
}

// newHandler assembles the HTTP handler: the route mux wrapped in the
// middleware chain (logging, panic recovery, security headers, host
// allowlist, auth throttle, basic auth, cross-origin protection,
// canonical-path guard, routes). Session handlers are passed in so a test can
// exercise routing with stubs and no real PTY. ready gates /healthz.
func newHandler(cfg *config, h terminal.SessionHandlers, ready *webhttp.Ready) (http.Handler, error) {
	mux := http.NewServeMux()
	// The engine owns its route topology (MountSessionRoutes wires exactly
	// its documented set), so no engine-internal route reaches this surface
	// unannounced. The create gate bounds unbounded PTY forking.
	terminal.MountSessionRoutes(mux, h,
		terminal.WithCreateGate(webhttp.SessionCreateRateLimit(terminal.SessionsPath)))
	// 200 when ready, 503 during startup/shutdown; the Docker HEALTHCHECK
	// target.
	mux.Handle(healthzPath, webhttp.ReadinessHandler(ready))

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	// Apply PERSIST_SCROLLBACK before either consumer below sees the tree,
	// so the static handler's ETag/gzip and the CSP's script hash are
	// computed over the bytes the browser receives.
	sub, err = applyPersistFlag(sub, cfg.persistScrollback)
	if err != nil {
		return nil, fmt.Errorf("apply PERSIST_SCROLLBACK: %w", err)
	}
	staticSrv, err := webhttp.StaticHandler(sub, webhttp.WithStaticCacheControl(staticCacheControl))
	if err != nil {
		return nil, err
	}
	mux.Handle("/", staticSrv)

	// Built once from the embedded index.html so script-src's sha256 tokens
	// always match. Fails loud rather than silently dropping the hardening.
	cspPolicy, err := buildCSPPolicy(sub)
	if err != nil {
		return nil, fmt.Errorf("build CSP: %w", err)
	}

	// Built only when a password is configured; nil skips the Chain entry
	// rather than a predicate that always returns false.
	var authMW, authThrottleMW webhttp.Middleware
	if cfg.password != "" {
		gate := newBasicAuthGate(cfg.username, cfg.password)
		// Bounds the guessing RATE against the single static AUTH_PASSWORD:
		// only a request about to be REFUSED draws a token, so a valid
		// credential (including the baked healthcheck) is never throttled.
		authThrottleMW = webhttp.FailedAuthRateLimit(
			func(r *http.Request) bool { return !gate.presentsValidCredentials(r) },
			"too many failed authentication attempts; check the credentials in AUTH_USERNAME/AUTH_PASSWORD",
		)
		authMW = gate.middleware
	}

	// Outermost first, nil entries skipped. Recoverer sits inside Logging so
	// a recovered request logs its 500 rather than the default 200.
	handler := webhttp.Chain(mux,
		webhttp.Logging(webhttp.WithLogger(slog.Default()), webhttp.ProbeLogLevel(healthzPath), webhttp.WithClientIP(cfg.trustedProxies...),
			// Session-id path segments are the /ws attach capability token;
			// rewriting the logged path to the matched route template keeps
			// a live token out of the access log.
			webhttp.WithTemplatePathsUnder(terminal.SessionsSubtreePath),
			// A COMPLETED /ws upgrade gets no access line (the handshake
			// ends the exchange rather than completing it), but every
			// handshake REFUSAL keeps its record — deliberately not
			// WithSkipPaths, which would drop those too.
			webhttp.WithSkipUpgrades(true),
		),
		webhttp.Recoverer(webhttp.WithRecoverLogger(slog.Default())),
		// Outside the host gate so a rejected Host still leaves a record; see
		// wsAttachLog.
		wsAttachLog(cfg.trustedProxies),
		webhttp.SecurityHeaders(
			webhttp.WithCSP(cspPolicy),
			// The terminal renders clickable OSC 8 hyperlinks straight out
			// of untrusted SESSION_CMD output, so a session can be induced
			// to open an attacker page; COOP severs window.opener
			// independently of the vendored UI's own rel="noopener".
			webhttp.WithCOOP("same-origin"),
			webhttp.WithReferrerPolicy("same-origin"),
			webhttp.WithPermissionsPolicy("camera=(), microphone=(), geolocation=()"),
		),
		// Precedes basicAuth (a disallowed host never reaches credential
		// evaluation) and CrossOriginProtection (rebinding makes Origin and
		// Host agree, defeating the origin check alone).
		cfg.hostPolicy.Middleware(),
		// Inside Logging (a silent throttle on a remote shell is the wrong
		// trade) and inside the host gate (a rebinding probe can't drain the
		// bucket meant for the real operator).
		authThrottleMW,
		authMW,
		// CrossOriginProtection.Check exempts GET/HEAD/OPTIONS, and a
		// WebSocket handshake is a GET, so this layer never covers /ws — the
		// cross-origin gate there is entirely the engine's.
		http.NewCrossOriginProtection().Handler,
		// Innermost: an unauthenticated or cross-origin caller learns
		// nothing about route spelling.
		canonicalPathGuard(healthzPath, terminal.SessionsPath),
	)
	return handler, nil
}

// staticCacheControl is the per-asset Cache-Control policy for
// webhttp.StaticHandler. Everything but fonts revalidates on every load: the
// vendored asset paths are stable rather than content-hashed, so a TTL would
// serve stale JS after a bump. Fonts get 30 days but not `immutable` — their
// @font-face URLs use fixed names, so bytes change under one filename on a
// Monaspace bump and a reload must still revalidate against the ETag.
func staticCacheControl(assetPath string) string {
	if strings.HasPrefix(assetPath, "vendor/fonts/") {
		return "public, max-age=2592000"
	}
	return "no-cache, must-revalidate"
}

// wsAttachMsg is the /ws attach record's message, named so a test can pin it
// without a second copy of the literal.
const wsAttachMsg = "terminal attach attempt"

// wsAttachLog records one line per /ws UPGRADE attempt. The access logger
// skips admitted streams (WithSkipUpgrades) and nothing else logs an attach,
// so without this, presenting the session capability token leaves no record
// at all (CWE-778). Logged at request start so a rejected Host or an
// unknown-session close is recorded too.
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

// isWebSocketUpgrade is wsAttachLog's ATTEMPT predicate — a different
// question from the access log's ended-as-a-stream check. This asks before
// the handshake runs, since the capability token is in the query string and
// a malformed handshake must still leave its audit record.
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

// sessionFactory returns the per-session handler factory the manager calls
// for each new session. The id is bound through terminal.LogID rather than
// raw because it doubles as the /ws attach and resume capability token, and
// AUTH_PASSWORD is optional (CWE-532).
func sessionFactory(cfg *config) func(terminal.SessionID) *terminal.Handler {
	return func(id terminal.SessionID) *terminal.Handler {
		opts := []terminal.Option{
			terminal.WithLogger(slog.Default().With("session", terminal.LogID(id))),
			// SESSION_CMD is operator-supplied and whitespace-split, so an
			// argv carrying a token is plausible; the startup line stays the
			// one authoritative record of what runs.
			terminal.WithCommandLogValue("[redacted]"),
			// A program picks an SGR colour SLOT and can't know what RGB
			// this terminal resolves it to, so the terminal holds the
			// legibility floor. 4.5 is the WCAG AA floor for body text;
			// backgrounds and default foregrounds are never touched.
			terminal.WithMinimumContrast(4.5),
		}
		// Omitted, not defaulted, when unset: the engine's own default then
		// applies, so a future change to it reaches this app unmodified.
		if cfg.scrollback != nil {
			opts = append(opts, terminal.WithScrollbackCapacity(*cfg.scrollback))
		}
		if cfg.workDir != "" {
			opts = append(opts, terminal.WithWorkDir(cfg.workDir))
		}
		return terminal.NewHandler(cfg.command, opts...)
	}
}

// warnIfPID1 reports that no container init is present. Orphan reaping is
// the init's job: a child SESSION_CMD forks can outlive its parent, the
// kernel reparents it onto PID 1, and os/exec waits only for children this
// process started — so at PID 1, every orphan is a zombie for the
// container's lifetime, worsened by the engine's own session reaping.
func warnIfPID1() {
	if os.Getpid() != 1 {
		return
	}
	slog.Warn("running as PID 1 with no container init; processes orphaned by a session will accumulate as zombies for the container's lifetime",
		"hint", "start the image with `docker run --init`, or set `init: true` on the compose service")
}

// warnIfExposed logs a prominent warning when the server is reachable beyond
// loopback without authentication — an unauthenticated remote shell.
//
// A portless LISTEN_ADDR is classified as a bare host rather than left
// unrecognized, so a portless loopback stays silent. Fail-public otherwise.
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
			"fix", "set AUTH_PASSWORD, bind 127.0.0.1, or front with an authenticating reverse proxy")
	case strings.TrimSpace(password) == "":
		slog.Warn("listening on a non-loopback address with a whitespace-only AUTH_PASSWORD",
			"addr", addr,
			"risk", "a blank/whitespace password provides negligible protection for a remote shell",
			"fix", "set a strong AUTH_PASSWORD or front with an authenticating reverse proxy")
	}
}

// basicAuthGate holds the two constant-time verifiers for the configured
// Basic credential pair. One parse serves both the throttle and the auth
// check, so they can't drift on how a missing header is read.
type basicAuthGate struct {
	verifyUser webhttp.StaticTokenVerifier
	verifyPass webhttp.StaticTokenVerifier
}

// newBasicAuthGate builds the gate, hashing both values once (SHA-256,
// constant-time compare) so per-request work hashes only what the client
// sent. An empty configured value fails CLOSED.
func newBasicAuthGate(username, password string) *basicAuthGate {
	return &basicAuthGate{
		verifyUser: webhttp.NewStaticTokenVerifier(username),
		verifyPass: webhttp.NewStaticTokenVerifier(password),
	}
}

// presentsValidCredentials reports whether r carries Basic credentials
// matching the configured pair. A missing header, wrong scheme, or bad
// encoding reports false — same answer as wrong credentials.
func (g *basicAuthGate) presentsValidCredentials(r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	// Evaluate both before combining so a rejection's duration never reveals
	// which credential was wrong.
	userOK := g.verifyUser.Verify(u)
	passOK := g.verifyPass.Verify(p)
	return ok && userOK && passOK
}

// middleware gates every request behind the configured Basic credentials,
// answering 401 with a challenge on failure. The browser caches credentials
// after page load and replays them on the same-origin WebSocket handshake.
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

// canonicalPathRefusal is the guard's response message, a const so a test
// can pin the wording. Echoes no part of the request, since net/http accepts
// up to MaxHeaderBytes of request line.
const canonicalPathRefusal = "request path is not canonical; send the route path exactly, without empty, dot, or dot-dot segments " +
	"(this route refuses rather than redirecting, because a redirect is a success status to a client without -L)"

// canonicalPathGuard refuses a request whose path is not the spelling
// http.ServeMux will route, when the cleaned path falls under one of
// prefixes. ServeMux answers 307 on a cleaned-path mismatch, and to curl
// without -L a 307 reads as success — this image's HEALTHCHECK would exit 0
// having never consulted readiness. Out of scope: the static mount (a
// browser follows the redirect) and /ws (a 3xx handshake already fails).
func canonicalPathGuard(prefixes ...string) webhttp.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// EscapedPath is what ServeMux itself cleans, so this verdict is
			// exactly "would the mux answer 307 here?" — the decoded
			// r.URL.Path would also refuse encoded dot segments ServeMux
			// does not redirect.
			clean, canonical := webhttp.CanonicalRequestPath(r.URL.EscapedPath())
			if !canonical && pathUnderAny(clean, prefixes) {
				webhttp.WriteError(w, r, http.StatusBadRequest, "non_canonical_path", canonicalPathRefusal)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// pathUnderAny reports whether clean equals one of prefixes or sits beneath
// it, matching whole path segments (so /api/sessionsfoo is not under
// /api/sessions). Tests the CLEANED path deliberately: a non-canonical
// request carries the wrong prefix by construction.
func pathUnderAny(clean string, prefixes []string) bool {
	for _, p := range prefixes {
		root := strings.TrimSuffix(p, "/")
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}

// cspTemplate is the Content-Security-Policy applied to every response, with
// two %s placeholders for the script-src and style-src hash tokens, computed
// once from the embedded index.html. style-src is hash-pinned rather than
// 'unsafe-inline' since the renderer styles via CSSOM property setters,
// which style-src doesn't govern.
const cspTemplate = "default-src 'self'; " +
	"script-src 'self' %s; " +
	"style-src 'self' %s; " +
	"img-src 'self' data:; font-src 'self'; connect-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'; " +
	"form-action 'none'"

// buildCSPPolicy reads index.html from sub, hashes every inline <script> and
// its single inline <style> block, and assembles the CSP string. Called once
// at construction; fails loud rather than degrading to 'unsafe-inline' when
// index.html is missing a script or carries other than exactly one style
// block — a valid build embeds two scripts and one style.
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
