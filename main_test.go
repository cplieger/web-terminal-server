package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	"strconv"
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
	// UNSET (nil), not defaulted: with no WT_SCROLLBACK the app holds no opinion
	// and omits the option, so the handler inherits the engine's own default. A
	// number here would be this app re-deciding a sizing question the engine
	// documents, and the three consumers sharing this knob would drift.
	if cfg.scrollback != nil {
		t.Errorf("scrollback = %d, want unset", *cfg.scrollback)
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

// TestLoadConfigRejectionNamesTheKeyAndNeverTheValue pins the confidentiality half of the
// strict env validators, and it is the inverse of what this test used to assert.
//
// These errors are rendered into main's ONE startup ERROR line, and a compose
// interpolation mistake can put a credential on any variable (`WT_SCROLLBACK: ${TOKEN}`
// resolves to a token that fails to parse as an int). Echoing the rejected value would
// leave a durable, queryable copy of it in the log store (CWE-532), so a rejection names
// the KEY and the accepted shape and nothing else. That is the same rule the WT_LOG_LEVEL
// warning and envx's own value-free BoolStrict error already follow here.
//
// The out-of-range cases are the deliberate exception: those values PARSED, so the number
// or duration is a bounds fact rather than an unknown string, and naming it is what makes
// the message actionable. The duration case also pins String(): %q on a time.Duration
// renders a rune literal, since its underlying kind is int64.
func TestLoadConfigRejectionNamesTheKeyAndNeverTheValue(t *testing.T) {
	// A value shaped like a credential, so a regression prints something obviously wrong.
	const secretish = "not-a-real-credential-AAAAAAAAAAAA"
	tests := []struct {
		name    string
		env     map[string]string
		want    string
		notWant string
	}{
		{"unparseable int names the key only", map[string]string{"WT_SCROLLBACK": "  lots\t"}, "WT_SCROLLBACK must be an integer >= 0", "lots"},
		{"unparseable int never leaks a secret-shaped value", map[string]string{"WT_SCROLLBACK": secretish}, "WT_SCROLLBACK must be an integer >= 0", secretish},
		{"below-min int names the parsed number", map[string]string{"WT_SCROLLBACK": "-5"}, "got -5", `"-5"`},
		{"unparseable duration names the key only", map[string]string{"WT_IDLE_REAPER": " 5x "}, "WT_IDLE_REAPER must be a non-negative Go duration", "5x"},
		{"unparseable duration never leaks a secret-shaped value", map[string]string{"WT_IDLE_REAPER": secretish}, "WT_IDLE_REAPER must be a non-negative Go duration", secretish},
		{"negative duration names the parsed duration", map[string]string{"WT_IDLE_REAPER": "-5s"}, `"-5s"`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setWTEnv(t, tt.env)

			_, err := loadConfig()
			if err == nil {
				t.Fatalf("loadConfig() = nil error, want a rejection for %v", tt.env)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error = %q, must not contain %q: a rejected env value never reaches the log", err, tt.notWant)
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
	// 0 passes through untouched: "retain nothing beyond the live screen" is a
	// coherent request, and it is the one shallow value the engine's clamp does
	// not raise (a client cannot page against a server holding no history, so
	// the inverted outcome the clamp exists to prevent cannot arise).
	if cfg.scrollback == nil || *cfg.scrollback != 0 {
		t.Errorf("scrollback = %v, want 0 set explicitly", cfg.scrollback)
	}
}

// TestLoadConfigScrollbackClampsBelowPagingFloor pins the one adjustment this
// app makes to an operator's number, and pins that it comes from the ENGINE
// rather than from a local copy of the threshold.
//
// A depth between 1 and the paging floor is honoured by the ring but too shallow
// for the server to offer demand-paged history — and the browser's fallback then
// retains its whole legacy buffer, so asking for less server history costs MORE
// phone memory. Clamping up and warning beats obeying that quietly.
func TestLoadConfigScrollbackClampsBelowPagingFloor(t *testing.T) {
	shallow := terminal.MinPagingCapacity - 1
	setWTEnv(t, map[string]string{"WT_SCROLLBACK": strconv.Itoa(shallow)})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.scrollback == nil || *cfg.scrollback != terminal.MinPagingCapacity {
		t.Errorf("scrollback = %v, want %d (clamped up to the paging floor)",
			cfg.scrollback, terminal.MinPagingCapacity)
	}
}

// TestLoadConfigScrollbackHonoursDeepValues pins that there is no upper bound to
// trip over: an operator asking for far more history than any session will reach
// is how this family spells "never truncate", and the engine's ring allocates
// only what it fills, so the number costs nothing until it is used.
func TestLoadConfigScrollbackHonoursDeepValues(t *testing.T) {
	const deep = 50_000_000
	setWTEnv(t, map[string]string{"WT_SCROLLBACK": strconv.Itoa(deep)})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig() error: %v", err)
	}
	if cfg.scrollback == nil || *cfg.scrollback != deep {
		t.Errorf("scrollback = %v, want %d honoured as given", cfg.scrollback, deep)
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
		// Case-variant loopback stays silent (2026-07 fix, approved behavior
		// change): the old case-sensitive match spuriously warned on
		// "Localhost"; webhttp.ClassifyBind folds like the sibling apps.
		{"loopback name mixed case", "Localhost:7681", "", false},
		{"loopback ipv6", "[::1]:7681", "", false},
		// The classify-the-unsplit-input recipe: a portless loopback WT_ADDR
		// is read as a bare host and stays silent, exactly as before the
		// webhttp migration (it fails at Listen with its own error).
		{"portless loopback stays silent", "127.0.0.1", "", false},
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

// basicAuthRequest drives a request through basicAuth with the given
// credentials and returns the response recorder. A nil creds pair sends no
// Authorization header.
func basicAuthRequest(user, pass string, creds *[2]string) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("inner"))
	})
	h := newBasicAuthGate(user, pass).middleware(next)
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

	// style-src is hash-pinned, not 'unsafe-inline'. The old comment here claimed
	// "the renderer's dynamic per-cell inline styles depend on it", which was
	// FALSE and contradicted main.go's own note 200 lines away: the renderer
	// styles via CSSOM property setters, which style-src does not govern, and
	// neither the UI nor the engine template emits a style= attribute. The
	// sibling web-terminal-kiro proved it by hash-pinning the same page shape in
	// production. A stale comment asserting a security relaxation is REQUIRED when
	// it is not is exactly what stops someone tightening it later, so this now
	// pins the tightened policy instead.
	styleSrc := cspDirective(t, csp, "style-src")
	if strings.Contains(styleSrc, "'unsafe-inline'") {
		t.Errorf("style-src = %q, want 'unsafe-inline' dropped in favour of a hash", styleSrc)
	}
	if !strings.Contains(styleSrc, "'sha256-") {
		t.Errorf("style-src = %q, want an inline-style sha256 hash", styleSrc)
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

// fallbackCSPPolicy assembles the CSP with BOTH hash slots relaxed to
// 'unsafe-inline' instead of pinned hashes. It lives in the TEST file by
// design: production always goes through buildCSPPolicy against the real
// embedded index.html and never relaxes script-src or style-src, so a relaxed
// builder must not be reachable from (or even compiled into) the production
// binary. Tests that do not exercise the inline scripts use it as their policy
// stand-in.
func fallbackCSPPolicy() string {
	return fmt.Sprintf(cspTemplate, "'unsafe-inline'", "'unsafe-inline'")
}

// TestFallbackCSPPolicy exercises the test-only helper above: it relaxes both
// script-src and style-src to 'unsafe-inline' (no pinned hashes) while keeping
// the other directives.
func TestFallbackCSPPolicy(t *testing.T) {
	policy := fallbackCSPPolicy()
	scriptSrc := cspDirective(t, policy, "script-src")
	if !strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("fallback script-src = %q, want it to contain 'unsafe-inline' (test-only relaxation)", scriptSrc)
	}
	if strings.Contains(scriptSrc, "'sha256-") {
		t.Errorf("fallback script-src = %q, want no pinned sha256 hash", scriptSrc)
	}
	// style-src is relaxed here for the same test-only reason. Production pins a
	// hash; TestCSPHeaderIsPresent asserts that on the real policy.
	styleSrc := cspDirective(t, policy, "style-src")
	if !strings.Contains(styleSrc, "'unsafe-inline'") {
		t.Errorf("fallback style-src = %q, want the test-only 'unsafe-inline' relaxation", styleSrc)
	}
	if strings.Contains(styleSrc, "'sha256-") {
		t.Errorf("fallback style-src = %q, want no pinned sha256 hash", styleSrc)
	}
}

// TestBuildCSPPolicyFailsLoud pins the fail-loud contract: buildCSPPolicy
// returns an error (never a silent 'unsafe-inline' degrade) when the static FS
// is nil, index.html is missing, index.html holds no inline <script>, or it does
// not hold exactly one inline <style>. A production build always embeds
// index.html with its two inline scripts and its one loading-overlay style, so
// any of these means a malformed build that must abort startup, not serve a
// policy that drops the script-src or style-src hardening.
func TestBuildCSPPolicyFailsLoud(t *testing.T) {
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{"missing index.html", fstest.MapFS{}},
		{"only external scripts", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><body><script src="/vendor/x.js"></script></body></html>`)},
		}},
		// The style half, mirroring web-terminal-kiro's cases: style-src is
		// hash-pinned now, so anything other than exactly one inline block is a
		// malformed build and must abort rather than degrade to 'unsafe-inline'.
		{"no style block", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><script type="importmap">{}</script></html>`)},
		}},
		{"unterminated style block", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><script type="importmap">{}</script><style>body{margin:0}`)},
		}},
		{"unterminated style open tag", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><script type="importmap">{}</script><style`)},
		}},
		{"two style blocks", fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<html><script type="importmap">{}</script><style>a{}</style><style>b{}</style>`)},
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
		// A Warn line reporting the malformed COUNT was emitted, and it carries none of
		// the rejected entries: a mis-expanded variable could put a credential in one, so
		// the values never reach the log (CWE-532).
		log := buf.String()
		if log == "" {
			t.Fatal("no slog output; want a Warn reporting the malformed entry count")
		}
		if !strings.Contains(log, "invalid_count=2") {
			t.Errorf("warn log %q does not report invalid_count=2", log)
		}
		for _, bad := range []string{"not-an-ip", "999.999.999.999"} {
			if strings.Contains(log, bad) {
				t.Errorf("warn log %q names the rejected entry %q; malformed values must never be logged", log, bad)
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
		if !strings.Contains(buf.String(), "invalid_count=1") {
			t.Errorf("warn log %q does not report invalid_count=1", buf.String())
		}
		if strings.Contains(buf.String(), "http://term.example.com") {
			t.Errorf("warn log %q names the rejected entry; malformed values must never be logged", buf.String())
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

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
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
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache, must-revalidate" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache, must-revalidate")
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

// TestAccessLogRedactsSessionTokenPaths pins the WithTemplatePathsUnder wiring
// in newHandler: the token-bearing /api/sessions/{id} REST paths (the /ws attach
// capability token the engine declares log-sensitive) must emit access lines
// whose recorded path is the token-free route template — never the raw id —
// while /healthz stays skipped and the exact-path create/list and SSE routes
// keep their real path. A regression dropping the policy would leak live session
// tokens to every log-read consumer (CWE-532). Serial: swaps the process-global
// default logger (newHandler binds slog.Default() at construction).
//
// It passes the ENGINE's real REST handler rather than a stub, and that is the
// point: the recorded template now comes from the pattern the mux actually
// matched, so this asserts the app agrees with the engine's route table instead
// of with a local copy of it. This app used to carry that table as a
// string-parsing transform — and so did web-terminal-kiro, independently, and the
// two had already diverged on the unmatched case. Mounting the real routes means
// there is one table, and a route the engine adds or renames shows up here.
// ws/events stay stubs: neither is template-rewritten, and the real SSE handler
// only returns when the client disconnects.
func TestAccessLogRedactsSessionTokenPaths(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var ready webhttp.Ready
	ready.Set(true)
	// The engine's own REST route table. The factory is never invoked (nothing
	// here creates a session), so no PTY is spawned.
	mgr := terminal.NewSessionManager(
		func(string) *terminal.Handler { return terminal.NewHandler([]string{"/bin/true"}) },
		terminal.WithManagerLogger(slog.New(slog.DiscardHandler)),
	)
	t.Cleanup(mgr.Shutdown)
	h, err := newHandler(&config{}, stubHandler{}, mgr.RESTHandler(), stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	// Each route with the METHOD the engine registers it under: the templates are
	// method-scoped, so a GET at a PUT-only route would route nowhere and prove
	// nothing.
	for _, req := range []struct{ method, path string }{
		{http.MethodPut, "/api/sessions/live-token-1234/title"},
		{http.MethodPut, "/api/sessions/live-token-abcd/pinned-title"},
		{http.MethodDelete, "/api/sessions/live-token-bcde/pinned-title"},
		{http.MethodGet, "/api/sessions/live-token-9999/future-subresource"},
		{http.MethodDelete, "/api/sessions/live-token-5678"},
		{http.MethodGet, "/api/sessions/events"},
		{http.MethodGet, "/api/sessions"},
		{http.MethodGet, "/healthz"},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(req.method, req.path, http.NoBody))
	}

	log := buf.String()
	for _, token := range []string{
		"live-token-1234", "live-token-abcd", "live-token-bcde", "live-token-9999",
		"live-token-5678",
	} {
		if strings.Contains(log, token) {
			t.Errorf("access log = %q, must never carry the raw session token %q (WithTemplatePathsUnder must rewrite the subtree's recorded path)", log, token)
		}
	}
	if !strings.Contains(log, "path=/api/sessions/{id}/title") {
		t.Errorf("access log = %q, want a template-path access line for the title route", log)
	}
	// The rename subresource must be DISTINGUISHABLE from the session-delete route,
	// not merely id-free: a suffix ladder matched "/title" and not "/pinned-title",
	// so every rename used to log as if it were a DELETE of the session.
	if !strings.Contains(log, "path=/api/sessions/{id}/pinned-title") {
		t.Errorf("access log = %q, want a template-path access line for the rename route", log)
	}
	// A subresource the engine does not serve is recorded as unmatched rather than
	// collapsed onto a route it is not — the failure mode above, made visible. The
	// marker is the library's now, so this app and web-terminal-kiro can no longer
	// disagree about what it looks like.
	if !strings.Contains(log, "path=/api/sessions/(unmatched)") {
		t.Errorf("access log = %q, want an unmatched-subresource access line", log)
	}
	if !strings.Contains(log, "path=/api/sessions/{id}") {
		t.Errorf("access log = %q, want a template-path access line for the id route", log)
	}
	if !strings.Contains(log, "path=/api/sessions/events") {
		t.Errorf("access log = %q, want the SSE route's REAL path (the events carve-out must not be template-rewritten)", log)
	}
	if !strings.Contains(log, "path=/api/sessions ") {
		t.Errorf("access log = %q, want the exact create/list path unchanged (it misses the subtree prefix)", log)
	}
	if strings.Contains(log, "path=/healthz") {
		t.Errorf("access log = %q, want /healthz skipped entirely", log)
	}
}

// TestFailingProbeSurfacesInAccessLog pins the ProbeLogLevel contract on
// /healthz: a healthy probe logs at Debug (dropped at the default level), a
// failing probe (the drain-window 503) surfaces at Error — the silent-skip
// idiom this replaced hid that signal. Serial: swaps the process-global
// default logger.
func TestFailingProbeSurfacesInAccessLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var ready webhttp.Ready // zero value: not ready -> /healthz answers 503
	h, err := newHandler(&config{}, stubHandler{}, stubHandler{}, stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if log := buf.String(); !strings.Contains(log, "path=/healthz") || !strings.Contains(log, "level=ERROR") {
		t.Errorf("access log = %q, want the failing /healthz probe at Error", log)
	}

	buf.Reset()
	ready.Set(true)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if log := buf.String(); strings.Contains(log, "path=/healthz") {
		t.Errorf("access log = %q, want no healthy-probe line at the default level (ProbeLogLevel maps 2xx to Debug)", log)
	}
}

// TestWebSocketUpgradeSkippedButRefusalLogged pins the WithSkipUpgrades wiring
// in newHandler, whose two halves fail in opposite directions. A COMPLETED
// upgrade must emit NO access line: the handshake ends the HTTP exchange, so a
// line would only appear when the socket closes, carrying a session-length
// duration and a status net/http never sent. A REFUSED handshake must still be
// logged — here the uniform 426 that keeps /ws unprobeable, and by the same
// path the 400 on a malformed key, the CrossOriginProtection 403 and the
// basicAuth 401 — because that is what an operator greps when a browser cannot
// attach. A regression to WithSkipPaths("/ws") satisfies the first half and
// silently breaks the second, which is the failure this pins. Serial: swaps the
// process-global default logger (newHandler binds slog.Default()).
func TestWebSocketUpgradeSkippedButRefusalLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// serveWS drives GET /ws through the real chain with a stub that answers
	// status, and returns whatever the access logger emitted.
	serveWS := func(t *testing.T, status int) string {
		t.Helper()
		buf.Reset()
		var ready webhttp.Ready
		ready.Set(true)
		ws := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		h, err := newHandler(&config{}, ws, stubHandler{}, stubHandler{}, &ready)
		if err != nil {
			t.Fatalf("newHandler() error: %v", err)
		}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ws", http.NoBody))
		return buf.String()
	}

	// 101 is what coder/websocket records before it hijacks, so this is the
	// shape a real completed upgrade takes through the StatusRecorder.
	if log := serveWS(t, http.StatusSwitchingProtocols); strings.Contains(log, "path=/ws") {
		t.Errorf("access log = %q, want no line for a completed /ws upgrade (the line describes a response that no longer exists)", log)
	}
	if log := serveWS(t, http.StatusUpgradeRequired); !strings.Contains(log, "path=/ws") {
		t.Errorf("access log = %q, want a refused /ws handshake logged (WithSkipUpgrades must suppress ONLY a completed upgrade)", log)
	}
}

// TestSessionLoggerTruncatesSessionID pins the session-token log boundary in
// main's handler factory. The session id doubles as the /ws attach + resume
// capability token, and WT_PASSWORD is optional, so in the documented
// unauthenticated posture a full id in an aggregated log is enough for anyone
// with log-read reach to attach to a live terminal (CWE-532). The factory
// therefore binds only terminal.LogID's truncated form.
//
// This test exists because the app previously logged the whole id and nothing
// failed: the leak was found by audit, not by CI. It drives the real engine
// session manager through the real factory so a regression (widening the
// prefix, dropping the LogID call, or binding the raw id again) fails here.
// Serial: swaps the process-global default logger the factory reads.
func TestSessionLoggerTruncatesSessionID(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	scrollbackLines := 100
	cfg := config{command: []string{"/bin/cat"}, scrollback: &scrollbackLines}
	mgr := terminal.NewSessionManager(sessionFactory(&cfg), terminal.WithManagerLogger(slog.Default()))
	t.Cleanup(mgr.Shutdown)
	id, err := mgr.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(id) <= 8 {
		t.Fatalf("session id %q is too short to exercise truncation", id)
	}

	log := buf.String()
	if strings.Contains(log, id) {
		t.Errorf("log = %q, must never carry the FULL session id %q (it is the /ws resume capability token)", log, id)
	}
	want := terminal.LogID(id)
	if !strings.Contains(log, want) {
		t.Errorf("log = %q, want the engine's LogID form %q for correlation", log, want)
	}
}

// failedAuthBurst pins the burst of webhttp.FailedAuthRateLimit as THIS app's
// documented contract (ten immediate failed attempts, then 429), the same way
// sessionCreateBurst pins the create preset. A deliberate tuning change in the
// shared preset fails these tests loudly so this app's docs and expectations
// are updated consciously rather than drifting silently.
const failedAuthBurst = 10

// authThrottleHandler builds the real stack with Basic auth configured, plus a
// requester that drives one GET /healthz through it either with the correct
// credentials or with a wrong password.
func authThrottleHandler(t *testing.T) (http.Handler, func(validCreds bool) *httptest.ResponseRecorder) {
	t.Helper()
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{username: "admin", password: "pw"}, &ready, nil)
	do := func(validCreds bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
		if validCreds {
			req.SetBasicAuth("admin", "pw")
		} else {
			req.SetBasicAuth("admin", "wrong")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	return h, do
}

// TestFailedAuthThrottle pins the failed-auth throttle
// (webhttp.FailedAuthRateLimit) fronting basicAuth: before it, a wrong password
// was answered 401 in microseconds with nothing counting attempts, so the single
// static WT_PASSWORD guarding an interactive remote shell could be guessed at
// wire speed. The properties that make the control correct are that failures
// cost a token, that success never does, and that the refusal is this app's own
// envelope.
func TestFailedAuthThrottle(t *testing.T) {
	t.Run("failed attempts draw tokens and are 429 past the burst", func(t *testing.T) {
		_, do := authThrottleHandler(t)
		for i := range failedAuthBurst {
			if code := do(false).Code; code != http.StatusUnauthorized {
				t.Fatalf("failed attempt %d = %d, want 401 (the burst must still reach the gate)", i+1, code)
			}
		}
		if code := do(false).Code; code != http.StatusTooManyRequests {
			t.Errorf("failed attempt past the burst = %d, want 429", code)
		}
	})

	t.Run("valid credentials never draw a token", func(t *testing.T) {
		_, do := authThrottleHandler(t)
		// Three times the burst in valid requests. If any of them consumed a
		// token, the single failed attempt afterwards would be 429 instead of
		// reaching the gate — which is the whole reason the predicate asks
		// "did this attempt FAIL?" rather than "is this route authenticated?".
		for i := range failedAuthBurst * 3 {
			if code := do(true).Code; code != http.StatusOK {
				t.Fatalf("valid request %d = %d, want 200", i+1, code)
			}
		}
		if code := do(false).Code; code != http.StatusUnauthorized {
			t.Errorf("first failed attempt after %d valid ones = %d, want 401 (valid requests must not spend tokens)",
				failedAuthBurst*3, code)
		}
	})

	t.Run("valid credentials still pass with the bucket empty", func(t *testing.T) {
		_, do := authThrottleHandler(t)
		for range failedAuthBurst + 1 {
			do(false)
		}
		if code := do(false).Code; code != http.StatusTooManyRequests {
			t.Fatalf("bucket is not empty: failed attempt = %d, want 429", code)
		}
		// The operator (and the image's baked healthcheck, which sends
		// WT_USERNAME/WT_PASSWORD) must keep working while an attacker is being
		// throttled on the same shared bucket.
		if code := do(true).Code; code != http.StatusOK {
			t.Errorf("valid request mid-flood = %d, want 200 (a correct credential is never throttled)", code)
		}
	})

	t.Run("429 carries the app envelope and a Retry-After hint", func(t *testing.T) {
		_, do := authThrottleHandler(t)
		for range failedAuthBurst {
			do(false)
		}
		rec := do(false)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, "too_many_auth_failures") {
			t.Errorf("429 body = %q, want the too_many_auth_failures code (what log queries and alert rules key on)", body)
		}
		if body := rec.Body.String(); !strings.Contains(body, "WT_USERNAME/WT_PASSWORD") {
			t.Errorf("429 body = %q, want the message naming the credentials to check", body)
		}
		if ra := rec.Header().Get("Retry-After"); ra == "" {
			t.Error("429 has no Retry-After; a legitimate operator retrying a rotated credential has nothing to wait on")
		}
	})

	t.Run("a disallowed Host is 403 and spends no token", func(t *testing.T) {
		var ready webhttp.Ready
		ready.Set(true)
		cfg := config{username: "admin", password: "pw", hostPolicy: hostPolicyFor(t, "term.example.com")}
		h := newTestHandler(t, cfg, &ready, nil)
		get := func(host, pass string) int {
			req := httptest.NewRequest(http.MethodGet, "http://"+host+healthzPath, nil)
			req.RemoteAddr = "192.168.1.50:44444"
			req.SetBasicAuth("admin", pass)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			return rec.Code
		}
		// The host gate stays OUTSIDE the throttle: a rebinding probe is not a
		// credential attempt, so it must not be able to drain the bucket and
		// throttle the real operator out of their own server.
		for i := range failedAuthBurst * 2 {
			if code := get("attacker.evil:7681", "wrong"); code != http.StatusForbidden {
				t.Fatalf("disallowed-Host request %d = %d, want 403", i+1, code)
			}
		}
		if code := get("term.example.com:7681", "wrong"); code != http.StatusUnauthorized {
			t.Errorf("first real failed attempt after %d rejected hosts = %d, want 401 (403s must not spend tokens)",
				failedAuthBurst*2, code)
		}
	})
}

// TestFailedAuthThrottleInertWithoutPassword pins the inert mode. With no
// WT_PASSWORD this app is deliberately unauthenticated — there is no credential
// to fail — so the throttle must not exist: newHandler builds neither it nor its
// token bucket, and Chain skips the nil entry. Any 429 here would be a
// regression that throttled the documented no-auth posture.
func TestFailedAuthThrottleInertWithoutPassword(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)
	for i := range failedAuthBurst * 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, healthzPath, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d with no WT_PASSWORD = %d, want 200 (the throttle must be absent, not merely lenient)",
				i+1, rec.Code)
		}
	}
	// Credentials nobody asked for are not a failed attempt either: with the
	// gate absent there is no verdict to count, so these must pass untouched.
	for i := range failedAuthBurst * 3 {
		req := httptest.NewRequest(http.MethodGet, healthzPath, nil)
		req.SetBasicAuth("nobody", "whatever")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unsolicited-credential request %d with no WT_PASSWORD = %d, want 200", i+1, rec.Code)
		}
	}
}

// TestCanonicalPathGuard pins the canonical-path guard (webhttp.
// CanonicalRequestPath). http.ServeMux cleans the request path before it selects
// a pattern and answers 307 when the cleaned path differs — before any handler
// runs. A 307 is a SUCCESS status to a client that does not follow redirects,
// and this image's baked HEALTHCHECK is `curl -sf` with no -L: a probe sent to
// //healthz would exit 0 having never invoked the readiness gate. The guard
// refuses those spellings on the probe and session-API surfaces instead.
//
// The other half of the contract is the SCOPE. The static mount keeps its
// redirects: a browser follows them, they are how relative asset paths resolve,
// and a static GET has no side effect a missed redirect could hide.
func TestCanonicalPathGuard(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	t.Run("guarded prefixes refuse a non-canonical spelling", func(t *testing.T) {
		// Each of these is a path ServeMux would answer 307 for, on a route
		// whose caller is a machine that may not follow it.
		for _, path := range []string{
			"//healthz",
			"/./healthz",
			"/anything/../healthz",
			"//api/sessions",
			"/api/./sessions",
			"//api/sessions/events",
			"/api/sessions/x/../events",
		} {
			rec := get(path)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %q = %d, want 400 (a 307 here reads as success to a probe without -L)", path, rec.Code)
			}
			if body := rec.Body.String(); !strings.Contains(body, "non_canonical_path") {
				t.Errorf("GET %q body = %q, want the non_canonical_path envelope", path, body)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Errorf("GET %q set Location %q; the guard must refuse, not redirect", path, loc)
			}
		}
	})

	t.Run("canonical spellings on the same routes pass", func(t *testing.T) {
		for path, want := range map[string]int{
			healthzPath:            http.StatusOK,
			"/api/sessions":        http.StatusOK,
			"/api/sessions/events": http.StatusOK,
			"/api/sessions/abc123": http.StatusOK,
			"/healthz/":            http.StatusNotFound, // canonical, simply not a route
		} {
			if code := get(path).Code; code != want {
				t.Errorf("GET %q = %d, want %d (the guard must not touch a canonical path)", path, code, want)
			}
		}
	})

	t.Run("static paths keep their redirects", func(t *testing.T) {
		// The cleaning redirect a browser follows, on the unguarded static
		// mount. Refusing these would break relative asset loads that work
		// today for no gain.
		for _, path := range []string{"//", "//index.html", "//favicon.svg"} {
			rec := get(path)
			if rec.Code != http.StatusTemporaryRedirect {
				t.Errorf("GET %q = %d, want 307 (static redirects are legitimate and must survive the guard)", path, rec.Code)
			}
			if rec.Header().Get("Location") == "" {
				t.Errorf("GET %q = 307 with no Location", path)
			}
		}
		// The file-server redirect on an ALREADY-canonical path, which the
		// guard never had a verdict on: /index.html is served as "./".
		if rec := get("/index.html"); rec.Code != http.StatusMovedPermanently {
			t.Errorf("GET /index.html = %d, want 301 (the file server's own redirect must survive)", rec.Code)
		}
	})

	t.Run("the WebSocket route is deliberately out of scope", func(t *testing.T) {
		// A WebSocket client cannot mistake a 3xx handshake for success, and
		// the engine answers a uniform 426 on /ws to keep it unprobeable — an
		// app-shaped 400 on some spellings would undo that.
		if rec := get("//ws"); rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("GET //ws = %d, want 307 (/ws is not guarded)", rec.Code)
		}
	})

	t.Run("an encoded dot segment is not refused", func(t *testing.T) {
		// The guard is fed r.URL.EscapedPath(), the value ServeMux itself
		// cleans, so its verdict is exactly "would the mux redirect this?".
		// %2e%2e is not a dot segment on the wire, ServeMux draws no redirect
		// for it, and the request reaches the handler as it did before this
		// control existed. Feeding the decoded path instead would refuse it —
		// a wider policy that was available and deliberately not taken.
		if code := get("/api/sessions/%2e%2e").Code; code != http.StatusOK {
			t.Errorf("GET /api/sessions/%%2e%%2e = %d, want 200 (the guard tracks the mux's cleaning, no wider)", code)
		}
	})

	t.Run("auth answers before the guard does", func(t *testing.T) {
		// The guard is innermost, so an unauthenticated caller learns nothing
		// about route spelling.
		var authReady webhttp.Ready
		authReady.Set(true)
		ah := newTestHandler(t, config{username: "admin", password: "pw"}, &authReady, nil)
		rec := httptest.NewRecorder()
		ah.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "//healthz", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET //healthz = %d, want 401 (auth outranks the path verdict)", rec.Code)
		}
	})
}

// TestAttentionIconVariantsAreServed pins the promise index.html makes when it
// passes attentionIcons: true to the UI preset.
//
// That option tells the library to swap every link[rel=icon] to a status variant
// while a background session wants the user, and the library derives those URLs
// from a NAMING CONVENTION rather than a map it can validate: it inserts -input,
// -done or -alert after the filename's `favicon` token. Nothing in the library can
// check the files exist, because they live here — the dot's colour is the app's
// theme, so the assets are a per-app artifact of
// web-terminal-ui/scripts/gen-attention-icons.py.
//
// So the failure mode of breaking the promise is a BLANK tab icon, not a missing
// dot, and this test is what stands between a renamed icon and that.
func TestAttentionIconVariantsAreServed(t *testing.T) {
	t.Parallel()

	indexHTML, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index.html: %v", err)
	}

	// The app has to be ASKING for the variants, or the rest of this test would
	// pass just as well on a page that never swaps an icon.
	if !bytes.Contains(indexHTML, []byte("attentionIcons: true")) {
		t.Fatal("index.html no longer opts into attentionIcons; delete this test or restore the option")
	}

	iconHref := regexp.MustCompile(`<link\s+rel="icon"[^>]*href="([^"]+)"`)
	matches := iconHref.FindAllSubmatch(indexHTML, -1)
	// Guard the guard: a regexp that stopped matching would make every assertion
	// below vacuous and the test would pass on a page with no icons at all.
	if len(matches) < 3 {
		t.Fatalf("expected at least 3 rel=icon links in index.html, found %d", len(matches))
	}

	// Derived the same way the library derives them, so a passing test cannot be
	// reading a stale literal list. Go's RE2 has no lookahead, so the separator is
	// captured and put back rather than merely asserted.
	faviconToken := regexp.MustCompile(`(^|/)favicon([-.])`)
	for _, m := range matches {
		href := strings.TrimPrefix(string(m[1]), "/")
		if _, err := staticFS.ReadFile("static/" + href); err != nil {
			t.Errorf("base icon %q is linked but not embedded: %v", href, err)
			continue
		}
		for _, variant := range []string{"input", "done", "alert"} {
			name := faviconToken.ReplaceAllString(href, "${1}favicon-"+variant+"${2}")
			// An unchanged name means the convention did not apply to this href,
			// so the library would leave that link alone and its dot would
			// silently never appear.
			if name == href {
				t.Errorf("icon %q does not match the favicon naming convention, so no %s variant can be derived", href, variant)
				continue
			}
			if _, err := staticFS.ReadFile("static/" + name); err != nil {
				t.Errorf("attention variant %q is missing: %v", name, err)
			}
		}
	}
}

// Startup-stage attribution. main used to render four different ERROR messages
// ("invalid configuration", "static assets unavailable", "listen failed", "http
// server exited") and exit in place, so there was no stable field a log query or
// alert rule could key on, and each os.Exit skipped the deferred teardown and had
// to hand-duplicate it — which is how the serve path came to omit cancelBase.
// These tests pin the replacement: a stable VALUE per startup stage, always
// present, and a run that RETURNS instead of exiting.

func TestStageOf(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"nil error":                  {err: nil, want: stageUnknown},
		"unattributed error":         {err: errors.New("boom"), want: stageUnknown},
		"attributed":                 {err: atStage(stageListen, errors.New("boom")), want: stageListen},
		"attributed then re-wrapped": {err: fmt.Errorf("outer: %w", atStage(stageServe, errors.New("boom"))), want: stageServe},
		// The outermost attribution wins, which is what lets a caller re-attribute
		// a failure it has reinterpreted.
		"doubly attributed": {err: atStage(stageStatic, atStage(stageListen, errors.New("boom"))), want: stageStatic},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := stageOf(tc.err); got != tc.want {
				t.Errorf("stageOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Attribution must not change what the operator reads: the wrapped text IS the
// hint, and a stage wrapper that prefixed or reworded it would defeat the
// consolidation it exists to make queryable.
func TestAtStagePreservesTheMessageAndTheChain(t *testing.T) {
	t.Parallel()
	inner := errors.New("mount target is a file")
	wrapped := fmt.Errorf("WT_WORKDIR is not a directory: %w", inner)
	got := atStage(stageConfig, wrapped)

	if got.Error() != wrapped.Error() {
		t.Errorf("Error() = %q, want the wrapped text unchanged %q", got.Error(), wrapped.Error())
	}
	if !errors.Is(got, inner) {
		t.Error("attribution broke the error chain; errors.Is no longer reaches the cause")
	}
}

// The stage values are the log surface, so they are pinned as literals here:
// renaming one is a breaking change to an operator's query and must fail a test
// rather than pass silently.
func TestStageValuesAreStable(t *testing.T) {
	t.Parallel()
	for got, want := range map[string]string{
		stageConfig:  "config",
		stageStatic:  "static",
		stageListen:  "listen",
		stageServe:   "serve",
		stageUnknown: "unknown",
	} {
		if got != want {
			t.Errorf("stage value = %q, want %q", got, want)
		}
	}
}

// The load-bearing property, and the reason main was restructured: a startup
// failure RETURNS from run, attributed, rather than calling os.Exit past the
// pending defers. This test can only pass if that holds — an os.Exit here kills
// the test binary — and the config stage is the one failure a test can drive
// without binding a port or breaking the embedded static tree.
//
// Not parallel: t.Setenv forbids it, and run installs the process-global slog
// handler, which is restored below so the rest of the package keeps its own.
func TestRunReturnsAnAttributedConfigFailureInsteadOfExiting(t *testing.T) {
	prior := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prior) })

	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	setWTEnv(t, map[string]string{"WT_WORKDIR": file})

	err := run()
	if err == nil {
		t.Fatal("run() = nil error, want error when WT_WORKDIR is a regular file")
	}
	if got := stageOf(err); got != stageConfig {
		t.Errorf("run() stage = %q, want %q", got, stageConfig)
	}
	// The operator hint rides inside the error, because main renders it once.
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("run() error = %q, want it to mention %q", err, "not a directory")
	}
}

// TestLoadConfigWorkDirNamesTheShapeItRejected pins that the three work-dir failures keep
// three distinct remedies. One message for all three used to tell an operator whose mount
// is present but unreadable to add a mount that is already there.
//
// The unreadable case needs a directory the process cannot stat through, so it is skipped
// as root: root traverses a 0000 parent, which makes the case describe the container
// rather than the rule.
func TestLoadConfigWorkDirNamesTheShapeItRejected(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "no-such-dir")

	regular := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for name, tc := range map[string]struct {
		path string
		want string
		skip bool
	}{
		"absent":       {path: absent, want: "does not exist"},
		"regular file": {path: regular, want: "not a directory"},
		"unreadable":   {path: unreadableChildPath(t), want: "not readable", skip: os.Geteuid() == 0},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.skip {
				t.Skip("running as root: a 0000 parent is traversable, so this case cannot fail closed")
			}
			setWTEnv(t, map[string]string{"WT_WORKDIR": tc.path})
			_, err := loadConfig()
			if err == nil {
				t.Fatalf("loadConfig() with WT_WORKDIR=%q = nil error, want an error", tc.path)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadConfig() with WT_WORKDIR=%q error = %q, want it to mention %q", tc.path, err, tc.want)
			}
		})
	}
}

// unreadableChildPath returns a path whose PARENT denies traversal, so os.Stat fails with
// a permission error rather than fs.ErrNotExist.
func unreadableChildPath(t *testing.T) string {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	child := filepath.Join(parent, "workdir")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("Mkdir child: %v", err)
	}
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	return child
}

// TestStaticCacheControl covers the policy FUNCTION's two branches. The wiring into
// webhttp.StaticHandler is pinned separately by TestStaticHandlerETagAndRevalidation,
// which asserts the non-font value on a served response and therefore fails if the
// WithStaticCacheControl option is dropped and the library default returns.
//
// Fonts carry no `immutable`: the @font-face URLs come from the vendored UI's own CSS
// under fixed names, so the bytes change under one filename on a Monaspace bump and a
// reload must be able to revalidate against the content-hash ETag.
func TestStaticCacheControl(t *testing.T) {
	t.Parallel()
	const (
		fontPolicy  = "public, max-age=2592000"
		assetPolicy = "no-cache, must-revalidate"
	)
	for name, tc := range map[string]struct {
		asset string
		want  string
	}{
		"font":                {asset: "vendor/fonts/MonaspaceNeonNF-Regular.woff2", want: fontPolicy},
		"index":               {asset: "index.html", want: assetPolicy},
		"vendored js":         {asset: "vendor/cplieger-web-terminal-ui/index.js", want: assetPolicy},
		"font-like sibling":   {asset: "vendor/fonts-notes.txt", want: assetPolicy},
		"root of the tree":    {asset: "", want: assetPolicy},
		"icon beside the app": {asset: "favicon.svg", want: assetPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := staticCacheControl(tc.asset); got != tc.want {
				t.Errorf("staticCacheControl(%q) = %q, want %q", tc.asset, got, tc.want)
			}
		})
	}
}

// TestSecurityHeadersCarryTheHardenedSet pins the three headers this app sets over
// webhttp's SecurityHeaders defaults. The terminal renders clickable OSC 8 hyperlinks
// straight out of an arbitrary WT_CMD's output, so a session can be induced to open an
// attacker page; COOP and the tightened Referrer-Policy make the vendored UI's
// rel="noopener noreferrer" independent of the Renovate-bumped UI pin.
//
// Dropping any one option leaves the response either header-free (COOP,
// Permissions-Policy) or on webhttp's looser strict-origin-when-cross-origin default,
// and no other test in this package would notice.
func TestSecurityHeadersCarryTheHardenedSet(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	for header, want := range map[string]string{
		"Cross-Origin-Opener-Policy": "same-origin",
		"Referrer-Policy":            "same-origin",
		"Permissions-Policy":         "camera=(), microphone=(), geolocation=()",
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("GET / header %s = %q, want %q", header, got, want)
		}
	}
}

// TestWSAttachLogRecordsEveryUpgradeAttempt pins the /ws audit record. The access logger
// skips admitted upgrades (WithSkipUpgrades) and neither the engine's WebSocketHandler nor
// the per-session Handler logs an attach, so without this middleware the request that
// PRESENTS the session capability token is the only request to this server with no record
// at all — a leaked id could be replayed with nothing to show an operator (CWE-778).
//
// It also pins that the record TRUNCATES the token: the whole point is an audit trail that
// is not itself a credential store.
//
// Not parallel: it swaps the process-global slog handler.
func TestWSAttachLogRecordsEveryUpgradeAttempt(t *testing.T) {
	// A session id, which is what /ws carries. Distinctive enough that a leak of it
	// into the log is unmistakable, and shaped like the engine's ids rather than a key.
	const sessionID = "0123456789abcdef0123456789abcdef"

	for name, tc := range map[string]struct {
		headers map[string]string
		want    bool
	}{
		"upgrade attempt": {
			headers: map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"},
			want:    true,
		},
		// A comma-separated Connection is what a proxy sends; the token match must not
		// require the header to be exactly "upgrade".
		"upgrade attempt behind a proxy": {
			headers: map[string]string{"Upgrade": "websocket", "Connection": "keep-alive, Upgrade"},
			want:    true,
		},
		// A malformed handshake still presented the token, so it still needs a record.
		"upgrade header without connection": {
			headers: map[string]string{"Upgrade": "websocket"},
			want:    false,
		},
		"plain GET on /ws": {headers: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			var reached atomic.Bool
			mw := wsAttachLog(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				reached.Store(true)
			}))

			req := httptest.NewRequest(http.MethodGet, terminal.WSPath+"?session="+sessionID, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			mw.ServeHTTP(httptest.NewRecorder(), req)

			if !reached.Load() {
				t.Error("wsAttachLog did not call the next handler; it must observe, never gate")
			}
			logged := strings.Contains(buf.String(), wsAttachMsg)
			if logged != tc.want {
				t.Errorf("wsAttachLog logged %q = %v, want %v (log: %s)", wsAttachMsg, logged, tc.want, buf.String())
			}
			if strings.Contains(buf.String(), sessionID) {
				t.Errorf("wsAttachLog logged the full session token; it must bind the id through terminal.LogID (log: %s)", buf.String())
			}
		})
	}
}

// TestWSAttachLogIsWiredIntoTheChain pins the middleware's PLACE in newHandler, which the
// unit test above cannot: it calls wsAttachLog directly, so dropping the entry from
// webhttp.Chain would leave that test green and the audit record gone.
//
// It also pins the placement relative to the host gate. A request with a Host the
// allowlist rejects must still leave a record, because a rebinding probe against an
// unauthenticated PTY is exactly the event an operator needs afterwards.
//
// Not parallel: it swaps the process-global slog handler.
func TestWSAttachLogIsWiredIntoTheChain(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var ready webhttp.Ready
	ready.Set(true)
	cfg := config{hostPolicy: hostPolicyFor(t, "term.example.com")}
	h := newTestHandler(t, cfg, &ready, nil)

	req := httptest.NewRequest(http.MethodGet, terminal.WSPath+"?session=deadbeefdeadbeef", nil)
	req.Host = "attacker.example.net"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET %s with a disallowed Host = %d, want 403", terminal.WSPath, rec.Code)
	}
	if !strings.Contains(buf.String(), wsAttachMsg) {
		t.Errorf("a refused /ws attach left no %q record; wsAttachLog must sit OUTSIDE the host gate (log: %s)", wsAttachMsg, buf.String())
	}
}

// TestPathUnderAnyMatchesWholeSegments pins the guard's SCOPE at the unit the chain
// cannot show cheaply. pathUnderAny makes two claims nothing else checks: it matches on
// whole segments, and it accepts a prefix written with or without a trailing slash.
//
// A regression to a bare strings.HasPrefix keeps every row of TestCanonicalPathGuard
// green — those rows only ever send in-scope spellings — while //api/sessionsfoo starts
// being answered 400 instead of falling through to the static catch-all.
func TestPathUnderAnyMatchesWholeSegments(t *testing.T) {
	t.Parallel()
	prefixes := []string{healthzPath, terminal.SessionsPath}
	for name, tc := range map[string]struct {
		clean string
		want  bool
	}{
		"the probe itself":       {clean: healthzPath, want: true},
		"the exact create/list":  {clean: terminal.SessionsPath, want: true},
		"the subtree root":       {clean: terminal.SessionsPath + "/", want: true},
		"a session id":           {clean: terminal.SessionsPath + "/abc123", want: true},
		"the SSE stream":         {clean: terminal.SessionsPath + "/events", want: true},
		"a deeper session route": {clean: terminal.SessionsPath + "/abc123/pinned-title", want: true},
		// The two shapes a careless prefix test gets wrong: a longer sibling NAME.
		"a longer sibling of the probe":    {clean: healthzPath + "extra", want: false},
		"a longer sibling of the sessions": {clean: terminal.SessionsPath + "foo", want: false},
		"the parent of the sessions":       {clean: "/api", want: false},
		"the static root":                  {clean: "/", want: false},
		"a static asset":                   {clean: "/index.html", want: false},
		"the terminal socket":              {clean: terminal.WSPath, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := pathUnderAny(tc.clean, prefixes); got != tc.want {
				t.Errorf("pathUnderAny(%q, %v) = %v, want %v", tc.clean, prefixes, got, tc.want)
			}
		})
	}

	// The engine names the same scope two ways, so the verdicts must agree: a prefix
	// spelled with a trailing slash must not silently narrow the guard.
	for _, clean := range []string{terminal.SessionsPath, terminal.SessionsPath + "/x", "/api", "/"} {
		bare := pathUnderAny(clean, []string{terminal.SessionsPath})
		slashed := pathUnderAny(clean, []string{terminal.SessionsSubtreePath})
		if bare != slashed {
			t.Errorf("pathUnderAny(%q) = %v for SessionsPath but %v for SessionsSubtreePath; the two constants must name one scope", clean, bare, slashed)
		}
	}
}

// TestCanonicalPathGuardRefusalEnvelope pins what the refusal BODY carries, which the
// status-code rows do not reach.
//
// Two properties. A refused control-plane call must correlate with its access-log line,
// so the envelope carries a request id. And the refusal must not echo the caller's own
// request target back: net/http accepts up to MaxHeaderBytes (1 MiB by default) of
// request line, so reflecting the path would turn a one-line refusal into a
// caller-sized response body, and the sender already has the value.
func TestCanonicalPathGuardRefusalEnvelope(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	// A distinctive, long, obviously-attacker-chosen segment: if any part of the request
	// target is reflected, this is what shows up.
	const marker = "zzmarkerzz-0123456789abcdef"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "//api/sessions/"+marker, nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET //api/sessions/%s = %d, want 400", marker, rec.Code)
	}
	var envelope webhttp.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal body is not a webhttp.ErrorResponse: %v (body: %s)", err, rec.Body.String())
	}
	if envelope.Code != "non_canonical_path" {
		t.Errorf("refusal code = %q, want %q", envelope.Code, "non_canonical_path")
	}
	if envelope.Error != canonicalPathRefusal {
		t.Errorf("refusal message = %q, want the canonicalPathRefusal const %q", envelope.Error, canonicalPathRefusal)
	}
	if envelope.RequestID == "" {
		t.Error("refusal carries no request_id; a refused call must correlate with its access-log line")
	}
	if strings.Contains(rec.Body.String(), marker) {
		t.Errorf("refusal body echoes the caller's request target: %s", rec.Body.String())
	}
}

// TestCanonicalPathGuardRefusesTheSideEffect pins the guard on the route that motivates
// it. Every other guard row is a GET; POST /api/sessions forks a PTY, and the guard exists
// because a 307 there is read as success by a client without -L — a caller that believes
// it created a session when nothing ran.
//
// The zero-hit assertion means "refused" only because the canonical spelling is asserted
// to reach the stub in the same test; otherwise it would also pass on an unreachable route.
func TestCanonicalPathGuardRefusesTheSideEffect(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	var restHit atomic.Bool
	h, err := newHandler(&config{}, stubHandler{}, stubHandler{hit: &restHit}, stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "//api/sessions", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("POST //api/sessions = %d, want 400", rec.Code)
	}
	if restHit.Load() {
		t.Error("POST //api/sessions reached the session REST handler; the guard must refuse before the side effect")
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Errorf("POST //api/sessions carries Location %q; a redirect is a success status to a client without -L", loc)
	}

	// The canonical spelling still creates: the zero above is a refusal, not an
	// unreachable route.
	restHit.Store(false)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, terminal.SessionsPath, nil))
	if !restHit.Load() {
		t.Errorf("POST %s did not reach the session REST handler (status %d); the guard must pass a canonical request through", terminal.SessionsPath, rec.Code)
	}
}

// TestSetupLoggingInstallsTheLevelAndWarnsByNameOnly pins both halves of the WT_LOG_LEVEL
// contract, neither of which anything checked.
//
// The level must actually be INSTALLED — a parse whose result is dropped is invisible
// today — and the unparseable warning must name the KEY and carry no copy of the VALUE. A
// compose interpolation mistake is what puts a credential on this key (CWE-532), and this
// app's own comment claims the posture without a test behind it.
//
// Not parallel: it replaces the process-global logger, and t.Setenv forbids it anyway.
func TestSetupLoggingInstallsTheLevelAndWarnsByNameOnly(t *testing.T) {
	// A value shaped like a credential, so a regression prints something unmistakable.
	const secretish = "not-a-real-credential-BBBBBBBBBBBB"
	for name, tc := range map[string]struct {
		value     string
		wantLevel slog.Level
		wantWarn  bool
	}{
		"debug":            {value: "debug", wantLevel: slog.LevelDebug, wantWarn: false},
		"warn":             {value: "warn", wantLevel: slog.LevelWarn, wantWarn: false},
		"unset":            {value: "", wantLevel: slog.LevelInfo, wantWarn: false},
		"secret-shaped":    {value: secretish, wantLevel: slog.LevelInfo, wantWarn: true},
		"ordinary garbage": {value: "verbose", wantLevel: slog.LevelInfo, wantWarn: true},
	} {
		t.Run(name, func(t *testing.T) {
			prev := slog.Default()
			t.Cleanup(func() { slog.SetDefault(prev) })
			t.Setenv("WT_LOG_LEVEL", tc.value)

			// setupLogging installs its own handler over slogx, so capture by swapping the
			// default afterwards would lose the warning. Redirect stderr instead.
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe: %v", err)
			}
			realStderr := os.Stderr
			os.Stderr = w
			setupLogging()
			installed := slog.Default()
			os.Stderr = realStderr
			_ = w.Close()
			out, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("read captured stderr: %v", err)
			}
			_ = r.Close()

			if !installed.Enabled(t.Context(), tc.wantLevel) {
				t.Errorf("WT_LOG_LEVEL=%q: installed logger is not enabled at %v; the parsed level was dropped", tc.value, tc.wantLevel)
			}
			// One level below the boundary must be off, or "enabled" proves nothing.
			if tc.wantLevel > slog.LevelDebug && installed.Enabled(t.Context(), tc.wantLevel-4) {
				t.Errorf("WT_LOG_LEVEL=%q: installed logger is enabled BELOW %v, so the level is not in force", tc.value, tc.wantLevel)
			}

			warned := strings.Contains(string(out), "unparseable WT_LOG_LEVEL")
			if warned != tc.wantWarn {
				t.Errorf("WT_LOG_LEVEL=%q: warned = %v, want %v (stderr: %s)", tc.value, warned, tc.wantWarn, out)
			}
			if tc.value != "" && strings.Contains(string(out), tc.value) {
				t.Errorf("WT_LOG_LEVEL=%q: the rejected value reached the log: %s", tc.value, out)
			}
		})
	}
}

// TestIsWebSocketUpgradeRequiresBothListTokens pins the attach-record predicate at the
// unit. Both header tokens are required, each may arrive as a repeated field line or a
// comma list, matching is case- and whitespace-insensitive, and a token SUBSTRING must not
// match. A divergence here is silent: it drops the audit record for a request that
// presented a session capability token, which is the one thing wsAttachLog exists for.
func TestIsWebSocketUpgradeRequiresBothListTokens(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		headers map[string][]string
		want    bool
	}{
		"both, plain": {
			headers: map[string][]string{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}},
			want:    true,
		},
		"comma list from a proxy": {
			headers: map[string][]string{"Upgrade": {"websocket"}, "Connection": {"keep-alive, Upgrade"}},
			want:    true,
		},
		"repeated field lines": {
			headers: map[string][]string{"Upgrade": {"websocket"}, "Connection": {"keep-alive", "Upgrade"}},
			want:    true,
		},
		"padded and mixed case": {
			headers: map[string][]string{"Upgrade": {"  WebSocket  "}, "Connection": {" UPGRADE "}},
			want:    true,
		},
		"upgrade only":    {headers: map[string][]string{"Upgrade": {"websocket"}}, want: false},
		"connection only": {headers: map[string][]string{"Connection": {"Upgrade"}}, want: false},
		"neither":         {headers: nil, want: false},
		// A substring must not match, or an unrelated protocol negotiation would be
		// recorded as a terminal attach.
		"upgrade token is a superstring":    {headers: map[string][]string{"Upgrade": {"websocket-v2"}, "Connection": {"Upgrade"}}, want: false},
		"connection token is a superstring": {headers: map[string][]string{"Upgrade": {"websocket"}, "Connection": {"upgrader"}}, want: false},
		"a different protocol":              {headers: map[string][]string{"Upgrade": {"h2c"}, "Connection": {"Upgrade"}}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, terminal.WSPath, nil)
			for k, vals := range tc.headers {
				for _, v := range vals {
					req.Header.Add(k, v)
				}
			}
			if got := isWebSocketUpgrade(req); got != tc.want {
				t.Errorf("isWebSocketUpgrade(%v) = %v, want %v", tc.headers, got, tc.want)
			}
		})
	}
}

// TestCrossOriginProtectionGatesUnsafeMethods pins the CSRF layer, which nothing reached:
// no test in this package sent an Origin header, so the whole entry could be deleted from
// webhttp.Chain with the suite green.
//
// The boundary is exactly what the chain comment states: http.CrossOriginProtection
// returns early for GET, HEAD and OPTIONS as safe methods, so the layer governs the
// state-changing session REST calls and never the /ws handshake, which is a GET. The
// same-origin case is asserted alongside the cross-origin one, because a handler that
// refused every request would satisfy the negative test alone.
func TestCrossOriginProtectionGatesUnsafeMethods(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	var restHit atomic.Bool
	h, err := newHandler(&config{}, stubHandler{}, stubHandler{hit: &restHit}, stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	post := func(origin string) (int, bool) {
		restHit.Store(false)
		req := httptest.NewRequest(http.MethodPost, terminal.SessionsPath, nil)
		req.Host = "term.example.com"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, restHit.Load()
	}

	code, reached := post("https://attacker.example.net")
	if code != http.StatusForbidden {
		t.Errorf("cross-origin POST %s = %d, want 403", terminal.SessionsPath, code)
	}
	if reached {
		t.Error("cross-origin POST reached the session REST handler; CrossOriginProtection must refuse it")
	}

	// Same-origin must pass, or the assertion above is satisfied by a layer that refuses
	// everything.
	restHit.Store(false)
	req := httptest.NewRequest(http.MethodPost, terminal.SessionsPath, nil)
	req.Host = "term.example.com"
	req.Header.Set("Origin", "http://term.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !restHit.Load() {
		t.Errorf("same-origin POST %s did not reach the REST handler (status %d); the CSRF layer must admit it", terminal.SessionsPath, rec.Code)
	}
}

// TestNoDiagnosticRoutesOnThisSurface pins the route table's shape. The engine's mount
// contract is that only its documented set appears, and this app adds exactly /healthz and
// the static root, so no profiling or engine-internal path may answer on what is an
// unauthenticated remote shell by default.
//
// The assertion is the POSITIVE observable — the probe resolves to the static catch-all —
// because ServeMux reports a SUBTREE registration by its own pattern: a handler registered
// at "/debug/" answers /debug/pprof while a pattern comparison still reads as a miss.
func TestNoDiagnosticRoutesOnThisSurface(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h := newTestHandler(t, config{}, &ready, nil)

	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/cmdline", "/debug/vars", "/metrics",
		"/api/tools", "/api/health", "/api/kiro-cli/rescan",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// The static handler owns "/", so an unregistered path is its 404 — never a 200
		// from a diagnostic handler, and never a 405 from one that exists.
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: something is answering on a path this surface must not expose", path, rec.Code)
		}
	}
}

// TestSessionSurfaceIsNeverCacheable pins that every response on the session surface
// refuses storage. The id in a session URL is the /ws attach capability, so a cached
// response — or a cached 404 whose KEY carries the id — leaves it readable from a shared
// cache after the session is gone.
//
// The header comes from the libraries (webhttp.ReadinessHandler, and the engine's own
// no-store wrapper over its REST mux and SSE stream), so this is a synchronizing check
// against them rather than a restatement: a library regression or a consumer override
// fails here on the bump PR.
func TestSessionSurfaceIsNeverCacheable(t *testing.T) {
	var ready webhttp.Ready
	ready.Set(true)
	h, err := newHandler(&config{}, stubHandler{}, terminal.NewSessionManager(
		func(string) *terminal.Handler { return terminal.NewHandler([]string{"/bin/true"}) },
	).RESTHandler(), stubHandler{}, &ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, healthzPath},
		{http.MethodGet, terminal.SessionsPath},
		{http.MethodGet, terminal.SessionsPath + "/does-not-exist"},
		{http.MethodDelete, terminal.SessionsPath + "/does-not-exist"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") {
			t.Errorf("%s %s (status %d) Cache-Control = %q, want it to carry no-store: the session id is an attach capability", tc.method, tc.path, rec.Code, cc)
		}
	}
}

// TestWarnIfPID1StaysSilentUnderAnInit pins the half of the PID-1 guard a test can reach:
// with an init present this process is not PID 1, so nothing is warned. It is what
// regresses if the condition is ever inverted.
//
// Not parallel: it replaces the process-global logger.
func TestWarnIfPID1StaysSilentUnderAnInit(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("the test binary IS pid 1 here, so the silent case is unreachable")
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	warnIfPID1()

	if buf.Len() != 0 {
		t.Errorf("warnIfPID1() warned while not running as PID 1: %s", buf.String())
	}
}
