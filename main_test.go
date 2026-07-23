package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/cplieger/web-terminal-engine/v3/terminal"
	"github.com/cplieger/webhttp"
)

// setWTEnv clears every WT_* variable then applies the given overrides, so each
// loadConfig case runs against a known-clean environment regardless of the host
// shell or test ordering. t.Setenv restores the prior values at test end.
func setWTEnv(t *testing.T, over map[string]string) {
	t.Helper()
	for _, k := range []string{
		"WT_ADDR", "WT_CMD", "WT_WORKDIR", "WT_SCROLLBACK",
		"WT_USERNAME", "WT_PASSWORD", "WT_IDLE_REAPER", "WT_TRUSTED_PROXIES",
		"WT_ALLOWED_HOSTS",
	} {
		t.Setenv(k, "")
	}
	for k, v := range over {
		t.Setenv(k, v)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setWTEnv(t, nil)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() unexpected error: %v", err)
	}
	if cfg.addr != defaultAddr {
		t.Errorf("addr = %q, want %q", cfg.addr, defaultAddr)
	}
	if len(cfg.command) != 1 || cfg.command[0] != defaultCmd {
		t.Errorf("command = %v, want [%q]", cfg.command, defaultCmd)
	}
	if cfg.scrollback != defaultScrollback {
		t.Errorf("scrollback = %d, want %d", cfg.scrollback, defaultScrollback)
	}
	if cfg.username != "admin" {
		t.Errorf("username = %q, want %q", cfg.username, "admin")
	}
	if cfg.password != "" {
		t.Errorf("password = %q, want empty", cfg.password)
	}
}

func TestLoadConfigCommandSplitting(t *testing.T) {
	setWTEnv(t, map[string]string{"WT_CMD": "  /usr/bin/env   bash  -l "})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	want := []string{"/usr/bin/env", "bash", "-l"}
	if len(cfg.command) != len(want) {
		t.Fatalf("command = %v, want %v", cfg.command, want)
	}
	for i := range want {
		if cfg.command[i] != want[i] {
			t.Errorf("command[%d] = %q, want %q", i, cfg.command[i], want[i])
		}
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"empty command", map[string]string{"WT_CMD": "   "}},
		{"scrollback not an int", map[string]string{"WT_SCROLLBACK": "lots"}},
		{"scrollback negative", map[string]string{"WT_SCROLLBACK": "-5"}},
		{"workdir missing", map[string]string{"WT_WORKDIR": "/no/such/dir/web-terminal-test"}},
		{"idle reaper negative", map[string]string{"WT_IDLE_REAPER": "-5s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWTEnv(t, tt.env)
			if _, err := loadConfig(); err == nil {
				t.Fatalf("loadConfig() = nil error, want error for %s", tt.name)
			}
		})
	}
}

func TestLoadConfigWorkDirAccepted(t *testing.T) {
	dir := t.TempDir()
	setWTEnv(t, map[string]string{"WT_WORKDIR": dir})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.workDir != dir {
		t.Errorf("workDir = %q, want %q", cfg.workDir, dir)
	}
}

func TestLoadConfigWorkDirNotDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	setWTEnv(t, map[string]string{"WT_WORKDIR": file})
	_, err := loadConfig()
	if err == nil {
		t.Fatal("loadConfig() = nil error, want error when WT_WORKDIR is a regular file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("loadConfig() error = %q, want it to mention %q", err, "not a directory")
	}
}

func TestLoadConfigScrollbackZeroAllowed(t *testing.T) {
	setWTEnv(t, map[string]string{"WT_SCROLLBACK": "0"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.scrollback != 0 {
		t.Errorf("scrollback = %d, want 0", cfg.scrollback)
	}
}

// TestWarnIfExposed asserts the warn decision (warn vs. stay silent) across
// every WT_ADDR form by capturing slog.Default() into a buffer: loopback
// (v4/v6/name) and password-set cases must stay silent, while wildcard,
// routable, and unparseable hosts without a password must warn. warnIfExposed
// is the only guardrail against an accidental open shell, so this log-only
// security contract is pinned here. Cases run serially (no t.Parallel) because
// they swap the process-global default logger.
func TestWarnIfExposed(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		pass     string
		wantWarn bool
	}{
		{"password set on exposed addr", "0.0.0.0:7681", "pw", false},
		{"whitespace-only password on exposed addr", "0.0.0.0:7681", "   ", true},
		{"loopback ipv4", "127.0.0.1:7681", "", false},
		{"loopback name", "localhost:7681", "", false},
		{"loopback ipv6", "[::1]:7681", "", false},
		{"wildcard ipv4 no auth", "0.0.0.0:7681", "", true},
		{"wildcard ipv6 no auth", "[::]:7681", "", true},
		{"routable ip no auth", "192.168.1.10:7681", "", true},
		{"unparseable addr no auth", "garbage", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			warnIfExposed(tc.addr, tc.pass)

			gotWarn := buf.Len() > 0
			if gotWarn != tc.wantWarn {
				t.Errorf("warnIfExposed(addr=%q, passwordSet=%t) warned=%v, want %v (log=%q)",
					tc.addr, tc.pass != "", gotWarn, tc.wantWarn, buf.String())
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", false},
		{"::", false},
		{"", false},
		{"192.168.1.10", false},
		{"example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := isLoopbackHost(tt.host); got != tt.want {
				t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

// basicAuthRequest drives a request through basicAuth with the given
// credentials and returns the response recorder. A nil creds pair sends no
// Authorization header.
func basicAuthRequest(user, pass string, creds *[2]string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("inner"))
	})
	h := basicAuth(next, user, pass)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if creds != nil {
		req.SetBasicAuth(creds[0], creds[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBasicAuth(t *testing.T) {
	const user, pass = "admin", "s3cret"

	t.Run("correct credentials pass through", func(t *testing.T) {
		rec := basicAuthRequest(user, pass, &[2]string{user, pass})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.String() != "inner" {
			t.Errorf("body = %q, want %q", rec.Body.String(), "inner")
		}
	})

	t.Run("no credentials -> 401 with challenge", func(t *testing.T) {
		rec := basicAuthRequest(user, pass, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got == "" {
			t.Error("missing WWW-Authenticate challenge header on 401")
		}
	})

	t.Run("wrong password -> 401", func(t *testing.T) {
		rec := basicAuthRequest(user, pass, &[2]string{user, "wrong"})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong username -> 401", func(t *testing.T) {
		rec := basicAuthRequest(user, pass, &[2]string{"root", pass})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	// The empty-configured cases pin the webhttp verifier's fail-CLOSED
	// contract at the app level: an empty configured secret matches nothing,
	// not everything — even a client presenting the same empty string gets
	// 401. Production wiring never configures an empty pair (newHandler skips
	// the middleware entirely when no password is set, and envx.String
	// defaults an empty WT_USERNAME to "admin"), so an open endpoint is only
	// ever the explicit skip, never an accidental empty-secret match.
	t.Run("empty configured password fails closed", func(t *testing.T) {
		rec := basicAuthRequest(user, "", &[2]string{user, ""})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("empty configured username fails closed", func(t *testing.T) {
		rec := basicAuthRequest("", pass, &[2]string{"", pass})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// cspDirective returns the single CSP directive named `name` (e.g.
// "script-src") from a policy string, failing the test if it is absent.
func cspDirective(t *testing.T, csp, name string) string {
	t.Helper()
	for d := range strings.SplitSeq(csp, ";") {
		d = strings.TrimSpace(d)
		if d == name || strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	t.Fatalf("CSP %q has no %q directive", csp, name)
	return ""
}

// hashToken computes the CSP 'sha256-<std-base64>' source token for content,
// mirroring what a browser hashes for an inline script. It derives the value
// from the input (never a hardcoded literal) so the tests track index.html.
func hashToken(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// TestSecurityHeadersSetsCSPAndNosniff drives the fully assembled handler and
// asserts a response carries nosniff and the hash-pinned CSP — i.e. that
// webhttp.SecurityHeaders is wired into the Chain with the app's WithCSP policy.
func TestSecurityHeadersSetsCSPAndNosniff(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	csp := rec.Header().Get("Content-Security-Policy")

	// script-src is hardened: 'self' plus at least one pinned sha256 hash, and
	// NO 'unsafe-inline'.
	scriptSrc := cspDirective(t, csp, "script-src")
	if !strings.Contains(scriptSrc, "'self'") {
		t.Errorf("script-src = %q, want it to contain 'self'", scriptSrc)
	}
	if !strings.Contains(scriptSrc, "'sha256-") {
		t.Errorf("script-src = %q, want it to pin at least one 'sha256-...' hash", scriptSrc)
	}
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src = %q, want 'unsafe-inline' dropped", scriptSrc)
	}

	// style-src keeps 'unsafe-inline' (the renderer's dynamic per-cell inline
	// styles depend on it).
	styleSrc := cspDirective(t, csp, "style-src")
	if !strings.Contains(styleSrc, "'unsafe-inline'") {
		t.Errorf("style-src = %q, want it to keep 'unsafe-inline'", styleSrc)
	}

	// Every other directive is unchanged.
	for _, want := range []string{
		"default-src 'self'", "img-src 'self' data:", "font-src 'self'",
		"connect-src 'self'", "frame-ancestors 'none'", "base-uri 'none'",
		"object-src 'none'", "form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP = %q, want it to contain %q", csp, want)
		}
	}
}

// The inline-script scanner's own unit and format tests (TestInlineScriptHashes,
// TestCSPHashTokenFormat) moved to webhttp with the scanner (csp_test.go there,
// plus a fuzz target). What stays here is this app's contract: the CSP it
// SERVES matches the inline scripts it EMBEDS (the anti-drift oracle below) and
// buildCSPPolicy's fail-loud arms.

// TestCSPScriptHashesMatchEmbeddedInlineScripts is the anti-drift guard for the
// script-src hardening. It independently re-extracts every inline <script> in
// the embedded index.html with a regexp (a different implementation from the
// production byte scanner, so agreement is a genuine cross-check) and asserts
// the sha256 hash of each appears in the CSP the server actually sends. The
// header can therefore never silently stop matching the scripts the page runs.
// Hashes are computed from the embed, never hardcoded, so the test tracks
// index.html automatically.
func TestCSPScriptHashesMatchEmbeddedInlineScripts(t *testing.T) {
	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded static/index.html: %v", err)
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	// The CSP the app actually sends is built by buildCSPPolicy from the embedded
	// index.html; assert directly against that (the anti-drift subject) rather
	// than round-tripping it through the security-headers middleware.
	csp, err := buildCSPPolicy(sub)
	if err != nil {
		t.Fatalf("buildCSPPolicy: %v", err)
	}

	// Independent oracle: a regexp extractor, structurally different from the
	// production scanner. `(?is)` makes `.` span newlines and matching
	// case-insensitive; `.*?` stops at the first closing tag.
	scriptRE := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script\s*>`)
	srcRE := regexp.MustCompile(`(?i)(^|[\s/])src\s*=`)

	found := 0
	for _, m := range scriptRE.FindAllSubmatch(indexHTML, -1) {
		if srcRE.Match(m[1]) {
			continue // external script, allowed by 'self'
		}
		found++
		token := hashToken(string(m[2]))
		if !strings.Contains(csp, token) {
			t.Errorf("CSP is missing the hash for an inline script.\ncontent=%q\nwant token %s\nCSP: %s",
				m[2], token, csp)
		}
	}
	if found < 2 {
		t.Fatalf("oracle found %d inline scripts in index.html, want >= 2 (importmap + module bootstrap); the regexp or the file changed", found)
	}
}

// fallbackCSPPolicy assembles the CSP with script-src relaxed to
// 'unsafe-inline' instead of pinned hashes. It lives in the TEST file by
// design: production always goes through buildCSPPolicy against the real
// embedded index.html and never relaxes script-src, so a relaxed builder must
// not be reachable from (or even compiled into) the production binary. Tests
// that do not exercise the inline scripts use it as their policy stand-in.
func fallbackCSPPolicy() string {
	return fmt.Sprintf(cspTemplate, "'unsafe-inline'")
}

// TestFallbackCSPPolicy exercises the test-only helper above: it relaxes
// script-src to 'unsafe-inline' (no pinned hashes) while keeping the other
// directives, including style-src's 'unsafe-inline'.
func TestFallbackCSPPolicy(t *testing.T) {
	policy := fallbackCSPPolicy()
	scriptSrc := cspDirective(t, policy, "script-src")
	if !strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("fallback script-src = %q, want it to contain 'unsafe-inline' (test-only relaxation)", scriptSrc)
	}
	if strings.Contains(scriptSrc, "'sha256-") {
		t.Errorf("fallback script-src = %q, want no pinned sha256 hash", scriptSrc)
	}
	if styleSrc := cspDirective(t, policy, "style-src"); !strings.Contains(styleSrc, "'unsafe-inline'") {
		t.Errorf("fallback style-src = %q, want it to keep 'unsafe-inline'", styleSrc)
	}
}

// TestBuildCSPPolicyFailsLoud pins the fail-loud contract: buildCSPPolicy
// returns an error (never a silent 'unsafe-inline' degrade) when the static FS
// is nil, index.html is missing, or index.html holds no inline <script>. A
// production build always embeds index.html with its two inline scripts, so any
// of these means a malformed build that must abort startup, not serve a policy
// that drops the script-src hardening.
func TestBuildCSPPolicyFailsLoud(t *testing.T) {
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{"nil FS", nil},
		{"missing index.html", fstest.MapFS{}},
		{"only external scripts", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><body><script src="/vendor/x.js"></script></body></html>`)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildCSPPolicy(tc.fsys); err == nil {
				t.Errorf("buildCSPPolicy(%s) = nil error, want a fail-loud error", tc.name)
			}
		})
	}
}

// fakeHijacker is a ResponseWriter that implements http.Hijacker so a test can
// assert the hijack call reaches the underlying writer through the middleware
// chain's webhttp.Logging StatusRecorder wrapper (the path the /ws WebSocket
// upgrade depends on).
type fakeHijacker struct {
	http.ResponseWriter
	hijacked bool
}

func (f *fakeHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	return nil, nil, errors.New("test hijacker: no real connection")
}

// TestWebSocketHijackReachesThroughChain drives GET /ws through the fully
// assembled newHandler middleware chain (webhttp.Logging -> Recoverer ->
// SecurityHeaders -> CrossOriginProtection -> mux) with an underlying
// ResponseWriter that implements http.Hijacker, and asserts the hijack is
// actually reached via http.ResponseController. webhttp.Logging wraps the writer
// in a StatusRecorder, so this pins that the recorder stays transparent to the
// hijack the /ws WebSocket upgrade needs, through the real production chain.
func TestWebSocketHijackReachesThroughChain(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	var reached bool
	ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _, _ = http.NewResponseController(w).Hijack()
	})
	h, err := newHandler(&config{}, ws, stubHandler{}, stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	fh := &fakeHijacker{ResponseWriter: httptest.NewRecorder()}
	h.ServeHTTP(fh, httptest.NewRequest(http.MethodGet, "/ws", nil))

	if !reached {
		t.Fatal("handler never ran")
	}
	if !fh.hijacked {
		t.Error("Hijack did not reach the underlying ResponseWriter through the middleware chain; the /ws WebSocket upgrade would break")
	}
}

// stubHandler is a stand-in for the engine's terminal handler so route tests
// don't need a real PTY.
type stubHandler struct{ hit *atomic.Bool }

func (s stubHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if s.hit != nil {
		s.hit.Store(true)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ws-stub"))
}

// newTestHandler builds the real handler with stub session handlers. It fails
// the test if the embedded static assets can't be opened. Only the WS hit flag
// is wired here (the routes most tests care about); the session-route tests
// below call newHandler directly with their own hit-tracking stubs.
func newTestHandler(t *testing.T, cfg config, ready *webhttp.Ready, wsHit *atomic.Bool) http.Handler {
	t.Helper()
	h, err := newHandler(&cfg, stubHandler{hit: wsHit}, stubHandler{}, stubHandler{}, ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	return h
}

func TestHealthzReadinessGate(t *testing.T) {
	var ready webhttp.Ready
	h := newTestHandler(t, config{}, &ready, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not-ready /healthz = %d, want 503", rec.Code)
	}

	ready.Set(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("ready /healthz = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"status":"ok"`) {
		t.Errorf("ready /healthz body = %q, want it to contain %q", got, `"status":"ok"`)
	}
}

func TestRouteWSReachesTerminal(t *testing.T) {
	var ready webhttp.Ready
	var wsHit atomic.Bool
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, &wsHit)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if !wsHit.Load() {
		t.Error("/ws did not reach the terminal handler")
	}
	if rec.Body.String() != "ws-stub" {
		t.Errorf("/ws body = %q, want %q", rec.Body.String(), "ws-stub")
	}
}

func TestLoadConfigIdleReaper(t *testing.T) {
	t.Run("idle reaper duration parsed and validated", func(t *testing.T) {
		setWTEnv(t, map[string]string{"WT_IDLE_REAPER": "30m"})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if cfg.idleReaper != 30*time.Minute {
			t.Errorf("idleReaper = %v, want 30m", cfg.idleReaper)
		}
		setWTEnv(t, map[string]string{"WT_IDLE_REAPER": "nonsense"})
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig() with WT_IDLE_REAPER=nonsense = nil error, want error")
		}
	})
}

// trustedContains reports whether ip is inside any of the parsed trusted nets.
func trustedContains(nets []*net.IPNet, ip string) bool {
	parsed := net.ParseIP(ip)
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// TestLoadConfigTrustedProxies covers WT_TRUSTED_PROXIES parsing via the shared
// webhttp.ParseCIDRs helper and its threading onto cfg.trustedProxies (consumed
// by webhttp.WithClientIP in newHandler). Three contracts: unset yields nil (so
// ClientIP ignores X-Forwarded-For and logs the spoof-proof socket peer), a
// valid CIDR + bare-IP mix is parsed into containment-correct nets, and a
// malformed entry is warned (named) and skipped while the valid subset is kept —
// startup is never aborted. These cases mutate the process-global default logger
// and WT_* env, so they run serially (no t.Parallel).
func TestLoadConfigTrustedProxies(t *testing.T) {
	t.Run("unset yields nil (socket-peer default)", func(t *testing.T) {
		setWTEnv(t, nil)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if cfg.trustedProxies != nil {
			t.Errorf("trustedProxies = %v, want nil when WT_TRUSTED_PROXIES is unset", cfg.trustedProxies)
		}
	})

	t.Run("empty string yields nil", func(t *testing.T) {
		setWTEnv(t, map[string]string{"WT_TRUSTED_PROXIES": "   "})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if cfg.trustedProxies != nil {
			t.Errorf("trustedProxies = %v, want nil for a blank WT_TRUSTED_PROXIES", cfg.trustedProxies)
		}
	})

	t.Run("valid CIDR and bare-IP mix parsed", func(t *testing.T) {
		setWTEnv(t, map[string]string{"WT_TRUSTED_PROXIES": "10.0.0.0/8, 192.168.1.5 , ::1"})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if len(cfg.trustedProxies) != 3 {
			t.Fatalf("trustedProxies len = %d, want 3 (%v)", len(cfg.trustedProxies), cfg.trustedProxies)
		}
		// The CIDR contains its range; the bare IP became a single-host net.
		for _, c := range []struct {
			ip   string
			want bool
		}{
			{"10.255.0.1", true},   // inside 10.0.0.0/8
			{"192.168.1.5", true},  // the bare host itself
			{"192.168.1.6", false}, // a neighbor of the bare host is NOT trusted
			{"172.16.0.1", false},  // outside every entry
		} {
			if got := trustedContains(cfg.trustedProxies, c.ip); got != c.want {
				t.Errorf("trustedProxies contains %s = %v, want %v", c.ip, got, c.want)
			}
		}
	})

	t.Run("malformed entries are warned and skipped, valid subset kept", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		setWTEnv(t, map[string]string{"WT_TRUSTED_PROXIES": "10.0.0.0/8, not-an-ip, 999.999.999.999"})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		// Startup is not aborted; only the one valid CIDR is kept.
		if len(cfg.trustedProxies) != 1 {
			t.Fatalf("trustedProxies len = %d, want 1 (only the valid CIDR kept)", len(cfg.trustedProxies))
		}
		if !trustedContains(cfg.trustedProxies, "10.1.2.3") {
			t.Error("kept net does not contain 10.1.2.3; want the 10.0.0.0/8 entry retained")
		}
		// A Warn line naming each malformed entry was emitted.
		log := buf.String()
		if log == "" {
			t.Fatal("no slog output; want a Warn naming the malformed entries")
		}
		for _, bad := range []string{"not-an-ip", "999.999.999.999"} {
			if !strings.Contains(log, bad) {
				t.Errorf("warn log %q does not name malformed entry %q", log, bad)
			}
		}
	})
}

// TestLoadConfigAllowedHosts covers WT_ALLOWED_HOSTS parsing via the shared
// webhttp.ParseHostList helper and its threading onto cfg.hostPolicy (consumed
// by the host-allowlist middleware in newHandler). Contracts: unset yields an
// INACTIVE policy (any Host accepted, the backward-compatible default), a
// valid list is canonicalized into an exact-match gate, a malformed entry is
// warned (named) and DROPPED while the valid subset is kept, and an
// all-invalid list stays ACTIVE and empty — deny-all, fail closed, with a
// second Warn naming the deny-all state. These cases mutate the process-global
// default logger and WT_* env, so they run serially (no t.Parallel).
func TestLoadConfigAllowedHosts(t *testing.T) {
	allows := func(t *testing.T, policy *webhttp.HostPolicy, host, remoteAddr string) bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/probe", nil)
		if remoteAddr != "" {
			req.RemoteAddr = remoteAddr
		}
		return policy.Allows(req)
	}

	t.Run("unset yields an inactive policy (any Host accepted)", func(t *testing.T) {
		setWTEnv(t, nil)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if cfg.hostPolicy.Active() {
			t.Error("hostPolicy is active for an unset WT_ALLOWED_HOSTS; want the permissive backward-compatible default")
		}
		if !allows(t, cfg.hostPolicy, "anything.example:7681", "") {
			t.Error("inactive policy rejected a request; unset WT_ALLOWED_HOSTS must accept every Host")
		}
	})

	t.Run("valid list canonicalizes into an exact-match gate", func(t *testing.T) {
		setWTEnv(t, map[string]string{"WT_ALLOWED_HOSTS": "localhost, 192.168.1.5, Term.Example.COM."})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if !cfg.hostPolicy.Active() || cfg.hostPolicy.Size() != 3 {
			t.Fatalf("hostPolicy active=%v size=%d, want active with 3 entries", cfg.hostPolicy.Active(), cfg.hostPolicy.Size())
		}
		for _, c := range []struct {
			host string
			want bool
		}{
			{"localhost:7681", true},
			{"192.168.1.5:7681", true},
			{"TERM.example.com:1234", true}, // case + port canonicalize
			{"attacker.evil:7681", false},
		} {
			if got := allows(t, cfg.hostPolicy, c.host, "192.168.1.50:44444"); got != c.want {
				t.Errorf("Allows(Host %q) = %v, want %v", c.host, got, c.want)
			}
		}
	})

	t.Run("malformed entries are warned and dropped, valid subset kept", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		setWTEnv(t, map[string]string{"WT_ALLOWED_HOSTS": "http://term.example.com, localhost"})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if got := cfg.hostPolicy.Size(); got != 1 {
			t.Fatalf("hostPolicy size = %d, want 1 (the malformed entry dropped, the valid one kept)", got)
		}
		if !allows(t, cfg.hostPolicy, "localhost:7681", "192.168.1.50:44444") {
			t.Error("valid entry localhost missing from the allowlist")
		}
		if !strings.Contains(buf.String(), "http://term.example.com") {
			t.Errorf("warn log %q does not name the malformed entry", buf.String())
		}
	})

	t.Run("all-invalid list fails closed (active empty, deny-all warned)", func(t *testing.T) {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		setWTEnv(t, map[string]string{"WT_ALLOWED_HOSTS": ":7681"})
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig() error: %v", err)
		}
		if !cfg.hostPolicy.Active() || cfg.hostPolicy.Size() != 0 {
			t.Fatalf("hostPolicy active=%v size=%d, want an active empty policy (fail closed, never fall open)", cfg.hostPolicy.Active(), cfg.hostPolicy.Size())
		}
		if allows(t, cfg.hostPolicy, "term.example.com:7681", "192.168.1.50:44444") {
			t.Error("non-loopback request admitted by an active empty policy; all-invalid configuration must deny-all")
		}
		if !allows(t, cfg.hostPolicy, "127.0.0.1:7681", "127.0.0.1:54321") {
			t.Error("loopback healthcheck shape rejected; the carve-out must survive an all-invalid configuration")
		}
		if !strings.Contains(buf.String(), "no usable entries") {
			t.Errorf("warn log %q does not name the deny-all state", buf.String())
		}
	})
}

// hostPolicyFor builds an active HostPolicy from entries with this app's
// options (loopback carve-out + the WT_ALLOWED_HOSTS 403 message), failing the
// test on any invalid entry — handler tests configure only valid lists.
func hostPolicyFor(t *testing.T, entries ...string) *webhttp.HostPolicy {
	t.Helper()
	policy, invalid := webhttp.ParseHostList(entries,
		webhttp.WithLoopbackExempt(),
		webhttp.WithHostAllowlistError("host_not_allowed",
			"host not allowed; add it to WT_ALLOWED_HOSTS to serve this hostname"))
	if len(invalid) > 0 {
		t.Fatalf("test allowlist has invalid entries: %v", invalid)
	}
	return policy
}

// TestHostAllowlistGatesRoutes pins the WT_ALLOWED_HOSTS anti-DNS-rebinding
// gate through the real middleware stack (newHandler): a rebinding attack
// makes an attacker-controlled hostname resolve to this server, so Origin and
// Host AGREE and CrossOriginProtection alone admits the request — the
// exact-host allowlist must reject it BEFORE the terminal routes, while an
// allowed Host still reaches them. Also pins that X-Forwarded-Host cannot
// smuggle an allowed name, the loopback peer+Host carve-out (the image's own
// healthcheck keeps working under a browser-facing allowlist; a forged
// loopback Host from a remote peer does not), and that a zero-value config
// (no policy) stays permissive.
func TestHostAllowlistGatesRoutes(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	cfg := config{hostPolicy: hostPolicyFor(t, "term.example.com")}
	h := newTestHandler(t, cfg, &ready, nil)

	do := func(host, xfh, remoteAddr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/healthz", nil)
		if xfh != "" {
			req.Header.Set("X-Forwarded-Host", xfh)
		}
		if remoteAddr != "" {
			req.RemoteAddr = remoteAddr
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		name       string
		host       string
		xfh        string
		remoteAddr string
		want       int
	}{
		{name: "rebound host rejected", host: "attacker.evil:7681", remoteAddr: "192.168.1.50:44444", want: http.StatusForbidden},
		{name: "allowed host passes", host: "term.example.com:7681", remoteAddr: "192.168.1.50:44444", want: http.StatusOK},
		{name: "X-Forwarded-Host cannot smuggle an allowed name", host: "attacker.evil:7681", xfh: "term.example.com", remoteAddr: "192.168.1.50:44444", want: http.StatusForbidden},
		{name: "healthcheck shape: loopback peer + loopback Host admitted", host: "127.0.0.1:7681", remoteAddr: "127.0.0.1:54321", want: http.StatusOK},
		{name: "rebinding via same-host browser: loopback peer + attacker Host rejected", host: "attacker.evil:7681", remoteAddr: "127.0.0.1:54321", want: http.StatusForbidden},
		{name: "forged loopback Host from remote peer rejected", host: "127.0.0.1:7681", remoteAddr: "192.168.1.50:44444", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(tc.host, tc.xfh, tc.remoteAddr)
			if rec.Code != tc.want {
				t.Errorf("GET Host %s (peer %s) = %d, want %d", tc.host, tc.remoteAddr, rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden {
				if body := rec.Body.String(); !strings.Contains(body, "host_not_allowed") || !strings.Contains(body, "WT_ALLOWED_HOSTS") {
					t.Errorf("403 body = %q, want the host_not_allowed envelope naming WT_ALLOWED_HOSTS", body)
				}
			}
		})
	}

	t.Run("zero-value config stays permissive", func(t *testing.T) {
		open := newTestHandler(t, config{}, &ready, nil)
		rec := httptest.NewRecorder()
		open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://anything.example:7681/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET /healthz with no host policy = %d, want %d (unset WT_ALLOWED_HOSTS must stay backward compatible)", rec.Code, http.StatusOK)
		}
	})
}

// TestHostAllowlistRunsBeforeBasicAuth pins the middleware ordering contract:
// the host gate rejects an unauthorized Host with 403 BEFORE any credential
// evaluation (valid credentials do not rescue a disallowed Host), while an
// allowed Host still hits basic auth (401 without credentials, 200 with).
func TestHostAllowlistRunsBeforeBasicAuth(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	cfg := config{
		username:   "admin",
		password:   "pw",
		hostPolicy: hostPolicyFor(t, "term.example.com"),
	}
	h := newTestHandler(t, cfg, &ready, nil)

	do := func(host string, withCreds bool) int {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/healthz", nil)
		req.RemoteAddr = "192.168.1.50:44444"
		if withCreds {
			req.SetBasicAuth("admin", "pw")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := do("attacker.evil:7681", true); got != http.StatusForbidden {
		t.Errorf("disallowed Host with valid credentials = %d, want 403 (the host gate must run before auth)", got)
	}
	if got := do("term.example.com:7681", false); got != http.StatusUnauthorized {
		t.Errorf("allowed Host without credentials = %d, want 401 (auth still gates an allowed host)", got)
	}
	if got := do("term.example.com:7681", true); got != http.StatusOK {
		t.Errorf("allowed Host with valid credentials = %d, want 200", got)
	}
}

func TestRouteSessionsReachesREST(t *testing.T) {
	var ready webhttp.Ready
	var wsHit, restHit, eventsHit atomic.Bool
	ready.Set(true)
	h, err := newHandler(&config{}, stubHandler{hit: &wsHit}, stubHandler{hit: &restHit}, stubHandler{hit: &eventsHit}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	// GET /api/sessions (list) and DELETE /api/sessions/{id} (close) both reach
	// the REST handler; neither reaches the SSE handler.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/sessions"},
		{http.MethodDelete, "/api/sessions/abc123"},
	} {
		restHit.Store(false)
		eventsHit.Store(false)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if !restHit.Load() {
			t.Errorf("%s %s did not reach the REST handler", tc.method, tc.path)
		}
		if eventsHit.Load() {
			t.Errorf("%s %s wrongly reached the SSE handler", tc.method, tc.path)
		}
	}
}

func TestRouteEventsReachesSSE(t *testing.T) {
	var ready webhttp.Ready
	var wsHit, restHit, eventsHit atomic.Bool
	ready.Set(true)
	h, err := newHandler(&config{}, stubHandler{hit: &wsHit}, stubHandler{hit: &restHit}, stubHandler{hit: &eventsHit}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions/events", nil))
	// The status SSE path must route to the events handler, NOT the REST subtree
	// (a precedence check: /api/sessions/events is more specific than /api/sessions/).
	if !eventsHit.Load() {
		t.Error("/api/sessions/events did not reach the SSE handler")
	}
	if restHit.Load() {
		t.Error("/api/sessions/events wrongly reached the REST handler")
	}
}

// TestEventsRouteStreamsThroughMiddleware is the server-side regression guard
// for the SSE status stream. It drives the REAL engine EventsHandler through the
// full newHandler middleware chain (webhttp.Logging -> Recoverer -> security
// headers -> cross-origin -> mux) over a real socket, and asserts the stream
// opens and flushes an event. webhttp.Logging wraps the ResponseWriter in a
// StatusRecorder (the /api/sessions/events path is not skipped), so this pins
// that the SSE stream still flushes through the logging wrapper.
func TestEventsRouteStreamsThroughMiddleware(t *testing.T) {
	factory := func(string) *terminal.Handler {
		return terminal.NewHandler([]string{"/bin/cat"}, terminal.WithLogger(nil))
	}
	mgr := terminal.NewSessionManager(factory, terminal.WithManagerLogger(nil))
	t.Cleanup(mgr.Shutdown)
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var ready webhttp.Ready
	ready.Set(true)
	h, err := newHandler(&config{}, stubHandler{}, stubHandler{}, mgr.EventsHandler(), &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/sessions/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/sessions/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (SSE must flush through the access-log recorder, not 500)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, id) {
			return // an event flushed through the middleware chain
		}
	}
	t.Fatalf("SSE stream delivered no data through the middleware (scan err: %v)", sc.Err())
}

// sessionCreateBurst pins the burst of webhttp.SessionCreateRateLimit as THIS
// app's documented contract (six creates, then 429). A deliberate tuning
// change in the shared preset fails these tests loudly so the app's docs and
// expectations are updated consciously rather than drifting silently.
const sessionCreateBurst = 6

func TestCreateRateLimit(t *testing.T) {
	var ready webhttp.Ready
	var restHit atomic.Bool
	ready.Set(true)
	h, err := newHandler(&config{}, stubHandler{}, stubHandler{hit: &restHit}, stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	post := func() int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
		return rec.Code
	}
	// The preset burst of creates is allowed; the next is throttled 429.
	allowed := 0
	for range sessionCreateBurst {
		if post() == http.StatusOK {
			allowed++
		}
	}
	if allowed != sessionCreateBurst {
		t.Errorf("allowed %d creates in the burst, want %d", allowed, sessionCreateBurst)
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Errorf("create past the burst = %d, want 429", code)
	}
	// GET (list) is never rate-limited even after the create burst is exhausted.
	restHit.Store(false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if !restHit.Load() {
		t.Error("GET /api/sessions was blocked by the create rate limiter")
	}
}

// TestCreateRateLimitRefillsOverTime pins token-bucket recovery: after the burst is
// exhausted, idle time refills tokens so creation is permitted again.
func TestCreateRateLimitRefillsOverTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ready webhttp.Ready
		ready.Set(true)
		h, err := newHandler(&config{}, stubHandler{}, stubHandler{}, stubHandler{}, &ready)
		if err != nil {
			t.Fatalf("newHandler() error: %v", err)
		}
		post := func() int {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sessions", nil))
			return rec.Code
		}
		for range sessionCreateBurst {
			post()
		}
		if code := post(); code != http.StatusTooManyRequests {
			t.Fatalf("post immediately after exhausting the burst = %d, want 429", code)
		}
		time.Sleep(2 * time.Second) // virtual clock: refills ~2 tokens
		if code := post(); code != http.StatusOK {
			t.Errorf("post after a 2s refill = %d, want 200 (bucket must recover)", code)
		}
	})
}

func TestRouteStaticServesIndex(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200 (static index should be served from embed.FS)", rec.Code)
	}
}

func TestStaticHandlerETagAndRevalidation(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static response has no ETag; the browser cannot revalidate the embedded bundle and re-downloads it every load")
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("ETag = %q, want a quoted opaque validator", etag)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional GET / with matching If-None-Match = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 response body = %q, want empty", rec2.Body.String())
	}
}

func TestHandlerAuthGatesAllRoutes(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	cfg := config{username: "admin", password: "pw"}
	h := newTestHandler(t, cfg, &ready, nil)

	// Even /healthz sits behind auth when a password is configured.
	for _, path := range []string{"/", "/healthz", "/ws"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d, want 401", path, rec.Code)
		}
	}

	// With credentials, /healthz returns 200.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.SetBasicAuth("admin", "pw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated /healthz = %d, want 200", rec.Code)
	}
}

// The gzip negotiation/precompute unit tests (TestAcceptsGzip,
// TestIfNoneMatchContains, TestGzipAsset, TestGzipAssetContentTypeFallback,
// TestServeGzip) moved to webhttp with the static-serving mechanism
// (static_test.go there). What stays here is the app-level contract below:
// the assembled handler still negotiates gzip and revalidates via ETags.

func TestStaticHandlerGzipNegotiation(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	t.Run("offering gzip yields a compressed body that decodes to the identity bytes", func(t *testing.T) {
		idRec := httptest.NewRecorder()
		h.ServeHTTP(idRec, httptest.NewRequest(http.MethodGet, "/", nil))
		identity := bytes.Clone(idRec.Body.Bytes())

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / (gzip) = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
			t.Errorf("Vary = %q, want it to contain %q", got, "Accept-Encoding")
		}
		if etag := rec.Header().Get("ETag"); !strings.HasSuffix(etag, `-gz"`) {
			t.Errorf("gzip ETag = %q, want a distinct tag ending in -gz\"", etag)
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("response body is not valid gzip: %v", err)
		}
		decoded, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(decoded, identity) {
			t.Error("gzip response body did not decode to the identity (uncompressed) response bytes")
		}
	})

	t.Run("without Accept-Encoding the identity path serves uncompressed bytes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Encoding = %q, want empty on the identity path", got)
		}
		if etag := rec.Header().Get("ETag"); strings.HasSuffix(etag, `-gz"`) {
			t.Errorf("identity ETag = %q, must not carry the -gz suffix", etag)
		}
	})

	t.Run("conditional gzip GET with the gz ETag yields 304", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(rec, req)
		gzEtag := rec.Header().Get("ETag")
		if gzEtag == "" {
			t.Fatal("first gzip GET returned no ETag")
		}

		rec2 := httptest.NewRecorder()
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.Header.Set("Accept-Encoding", "gzip")
		req2.Header.Set("If-None-Match", gzEtag)
		h.ServeHTTP(rec2, req2)
		if rec2.Code != http.StatusNotModified {
			t.Fatalf("conditional gzip GET with the gz ETag = %d, want 304", rec2.Code)
		}
		if rec2.Body.Len() != 0 {
			t.Errorf("304 body len = %d, want 0", rec2.Body.Len())
		}
	})
}
