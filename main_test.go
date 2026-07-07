package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/cplieger/web-terminal-engine/v2/terminal"
	"github.com/cplieger/webhttp"
)

// setWTEnv clears every WT_* variable then applies the given overrides, so each
// loadConfig case runs against a known-clean environment regardless of the host
// shell or test ordering. t.Setenv restores the prior values at test end.
func setWTEnv(t *testing.T, over map[string]string) {
	t.Helper()
	for _, k := range []string{
		"WT_ADDR", "WT_CMD", "WT_WORKDIR", "WT_SCROLLBACK",
		"WT_USERNAME", "WT_PASSWORD", "WT_IDLE_REAPER",
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

func TestInlineScriptHashes(t *testing.T) {
	cases := []struct {
		name string
		html string
		want []string
	}{
		{"no scripts", `<html><body>hi</body></html>`, nil},
		{"single inline", `<head><script>let a=1;</script></head>`, []string{hashToken("let a=1;")}},
		{"external skipped", `<script src="/vendor/x.js"></script>`, nil},
		{"external with type skipped", `<script type="module" src="/x.js"></script>`, nil},
		{"mixed inline and external", `<script src="/x.js"></script><script>b=2</script>`, []string{hashToken("b=2")}},
		{
			"two inline preserve order",
			`<script type="importmap">{"i":1}</script><script type="module">go()</script>`,
			[]string{hashToken(`{"i":1}`), hashToken("go()")},
		},
		{"case-insensitive tag", `<SCRIPT>x=3</SCRIPT>`, []string{hashToken("x=3")}},
		{"data-src is not a src attribute", `<script data-src="x">y=4</script>`, []string{hashToken("y=4")}},
		{"newlines hashed verbatim", "<script>\n  z=5\n</script>", []string{hashToken("\n  z=5\n")}},
		{"scriptfoo is not a script tag", `<scriptfoo>nope</scriptfoo>`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inlineScriptHashes([]byte(tc.html))
			if !slices.Equal(got, tc.want) {
				t.Errorf("inlineScriptHashes(%q) = %v, want %v", tc.html, got, tc.want)
			}
		})
	}
}

// TestCSPHashTokenFormat pins that cspHash emits a CSP-grammar source token: a
// standard-base64 encoding of a 32-byte sha256 digest wrapped as 'sha256-...'.
// It validates the encoding/format without hardcoding any expected hash value.
func TestCSPHashTokenFormat(t *testing.T) {
	tok := cspHash([]byte("console.log(1)"))
	if !strings.HasPrefix(tok, "'sha256-") || !strings.HasSuffix(tok, "'") {
		t.Fatalf("token = %q, want the 'sha256-<base64>' form", tok)
	}
	b64 := strings.TrimSuffix(strings.TrimPrefix(tok, "'sha256-"), "'")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("hash %q is not valid standard base64: %v", b64, err)
	}
	if len(raw) != sha256.Size {
		t.Errorf("decoded hash = %d bytes, want %d (sha256)", len(raw), sha256.Size)
	}
}

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

// TestFallbackCSPPolicy exercises the test-only escape hatch: fallbackCSPPolicy
// relaxes script-src to 'unsafe-inline' (no pinned hashes) while keeping the
// other directives, including style-src's 'unsafe-inline'. Production never
// takes this path — newHandler always builds the hash-pinned policy from the
// embedded index.html via buildCSPPolicy — so this both documents the contract
// and keeps the helper exercised.
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
	// The burst (createBurst) creates are allowed; the next is throttled 429.
	allowed := 0
	for range int(createBurst) {
		if post() == http.StatusOK {
			allowed++
		}
	}
	if allowed != int(createBurst) {
		t.Errorf("allowed %d creates in the burst, want %d", allowed, int(createBurst))
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
		for range int(createBurst) {
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

// TestTokenBucketCapsRefillAtBurst pins that refill clamps at createBurst.
func TestTokenBucketCapsRefillAtBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &tokenBucket{}
		if !b.allow() {
			t.Fatal("first allow() = false, want true")
		}
		time.Sleep(time.Hour) // virtual clock: would refill thousands if uncapped
		allowed := 0
		for b.allow() {
			allowed++
		}
		if allowed != int(createBurst) {
			t.Errorf("allowed %d after a long idle, want %d (refill must cap at burst)", allowed, int(createBurst))
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

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"plain gzip", "gzip", true},
		{"gzip with q=1", "gzip;q=1.0", true},
		{"gzip explicitly disabled q=0", "gzip;q=0", false},
		{"gzip disabled q=0.0", "gzip;q=0.0", false},
		{"gzip with fractional q", "gzip;q=0.5", true},
		{"gzip with space before params", "gzip ; q=0", false},
		{"second token offers gzip", "deflate, gzip", true},
		{"second token gzip disabled", "deflate, gzip;q=0", false},
		{"no gzip offered", "br, deflate", false},
		{"identity only", "identity", false},
		{"empty header", "", false},
		{"wildcard not treated as gzip", "*", false},
		{"malformed q is permissive", "gzip;q=bogus", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Accept-Encoding", tc.header)
			}
			if got := acceptsGzip(req); got != tc.want {
				t.Errorf("acceptsGzip(Accept-Encoding=%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestIfNoneMatchContains(t *testing.T) {
	const etag = `"abc-gz"`
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"empty header", "", false},
		{"exact match", `"abc-gz"`, true},
		{"wildcard", "*", true},
		{"present in comma list", `"x", "abc-gz", "y"`, true},
		{"absent from list", `"x", "y"`, false},
		{"whitespace trimmed around match", `  "abc-gz"  `, true},
		{"identity etag does not match gz etag", `"abc"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifNoneMatchContains(tc.header, etag); got != tc.want {
				t.Errorf("ifNoneMatchContains(%q, %q) = %v, want %v", tc.header, etag, got, tc.want)
			}
		})
	}
}

func TestGzipAsset(t *testing.T) {
	t.Run("compressible asset round-trips and carries extension content type", func(t *testing.T) {
		raw := []byte(strings.Repeat("body{color:#b48eff}\n", 500))
		gz, ok := gzipAsset(raw, "style.css")
		if !ok {
			t.Fatal("gzipAsset() ok = false for a highly compressible asset, want true")
		}
		if len(gz.body) >= len(raw) {
			t.Errorf("gzip body len = %d, want < raw len %d", len(gz.body), len(raw))
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz.body))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Error("gzip body did not round-trip to the original asset bytes")
		}
		if !strings.HasPrefix(gz.contentType, "text/css") {
			t.Errorf("contentType = %q, want it to start with %q", gz.contentType, "text/css")
		}
	})

	t.Run("incompressible tiny asset is not gzipped", func(t *testing.T) {
		if _, ok := gzipAsset([]byte("x"), "tiny.txt"); ok {
			t.Error("gzipAsset() ok = true for a 1-byte asset gzip cannot shrink, want false")
		}
	})
}

// TestGzipAssetContentTypeFallback pins the mime-miss branch: a compressible asset whose
// extension mime.TypeByExtension cannot resolve must fall back to http.DetectContentType.
func TestGzipAssetContentTypeFallback(t *testing.T) {
	raw := []byte(strings.Repeat("plain text body line\n", 500))
	gz, ok := gzipAsset(raw, "asset.unknownext")
	if !ok {
		t.Fatal("gzipAsset() ok = false for a highly compressible asset, want true")
	}
	if gz.contentType == "" {
		t.Fatal("contentType is empty; the mime-miss fallback did not run")
	}
	if !strings.HasPrefix(gz.contentType, "text/plain") {
		t.Errorf("contentType = %q, want text/plain prefix (DetectContentType on text bytes)", gz.contentType)
	}
}

func TestServeGzip(t *testing.T) {
	raw := []byte(strings.Repeat("hello world\n", 300))
	gz, ok := gzipAsset(raw, "x.txt")
	if !ok {
		t.Fatal("setup: gzipAsset() ok = false, want true")
	}
	const etag = `"deadbeef"`
	const gzEtag = `"deadbeef-gz"`

	t.Run("GET offering gzip serves the compressed body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true (it should have handled the gzip response)")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if got := rec.Header().Get("ETag"); got != gzEtag {
			t.Errorf("ETag = %q, want %q", got, gzEtag)
		}
		zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("gzip.NewReader: %v", err)
		}
		got, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("read gzip body: %v", err)
		}
		if !bytes.Equal(got, raw) {
			t.Error("served gzip body did not decode to the original bytes")
		}
	})

	t.Run("HEAD offering gzip sets headers but writes no body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodHead, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true")
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Encoding = %q, want %q", got, "gzip")
		}
		if rec.Body.Len() != 0 {
			t.Errorf("HEAD body len = %d, want 0", rec.Body.Len())
		}
	})

	t.Run("conditional GET with matching gz ETag yields 304", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x.txt", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("If-None-Match", gzEtag)
		if !serveGzip(rec, req, etag, gz) {
			t.Fatal("serveGzip() = false, want true")
		}
		if rec.Code != http.StatusNotModified {
			t.Errorf("status = %d, want 304", rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("304 body len = %d, want 0", rec.Body.Len())
		}
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("304 Content-Encoding = %q, want empty", got)
		}
	})

	t.Run("falls through (returns false) and writes nothing", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			method string
			accept string
			rangeH string
		}{
			{"non GET/HEAD method", http.MethodPost, "gzip", ""},
			{"client does not offer gzip", http.MethodGet, "identity", ""},
			{"gzip explicitly disabled", http.MethodGet, "gzip;q=0", ""},
			{"range request", http.MethodGet, "gzip", "bytes=0-10"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(tc.method, "/x.txt", nil)
				req.Header.Set("Accept-Encoding", tc.accept)
				if tc.rangeH != "" {
					req.Header.Set("Range", tc.rangeH)
				}
				if serveGzip(rec, req, etag, gz) {
					t.Errorf("serveGzip() = true, want false (should fall back to the identity file server)")
				}
				if got := rec.Header().Get("Content-Encoding"); got != "" {
					t.Errorf("Content-Encoding = %q, want empty on the fall-through path", got)
				}
			})
		}
	})
}

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
