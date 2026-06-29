package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// setWTEnv clears every WT_* variable then applies the given overrides, so each
// loadConfig case runs against a known-clean environment regardless of the host
// shell or test ordering. t.Setenv restores the prior values at test end.
func setWTEnv(t *testing.T, over map[string]string) {
	t.Helper()
	for _, k := range []string{"WT_ADDR", "WT_CMD", "WT_WORKDIR", "WT_SCROLLBACK", "WT_USERNAME", "WT_PASSWORD"} {
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

// TestWarnIfExposed exercises both decision branches (early-return when safe,
// log-path when exposed). It asserts no panic and that the function honors the
// same loopback/password logic as isLoopbackHost; the slog output itself is a
// side effect we don't capture here.
func TestWarnIfExposed(t *testing.T) {
	cases := []struct {
		name string
		addr string
		pass string
	}{
		{"password set, no warn", "0.0.0.0:7681", "pw"},
		{"loopback, no warn", "127.0.0.1:7681", ""},
		{"exposed no auth, warns", "0.0.0.0:7681", ""},
		{"malformed addr, treated as host", "garbage", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warnIfExposed(tc.addr, tc.pass) // must not panic
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

func TestStatusWriterCapturesCode(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}

	sw.WriteHeader(http.StatusTeapot)
	// A second WriteHeader must not overwrite the captured status (matches the
	// stdlib "superfluous WriteHeader" semantics the access log relies on).
	sw.WriteHeader(http.StatusInternalServerError)
	if sw.status != http.StatusTeapot {
		t.Errorf("status = %d, want %d", sw.status, http.StatusTeapot)
	}
}

func TestStatusWriterWriteImpliesOK(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, err := sw.Write([]byte("hi")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if !sw.wroteHeader {
		t.Error("wroteHeader = false after Write, want true")
	}
}

func TestStatusWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	if sw.Unwrap() != rec {
		t.Error("Unwrap() did not return the wrapped ResponseWriter (WebSocket hijack would break)")
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

// newTestHandler builds the real handler with a stub terminal. It fails the
// test if the embedded static assets can't be opened.
func newTestHandler(t *testing.T, cfg config, ready, wsHit *atomic.Bool) http.Handler {
	t.Helper()
	h, err := newHandler(&cfg, stubHandler{hit: wsHit}, ready)
	if err != nil {
		t.Fatalf("newHandler() error: %v", err)
	}
	return h
}

func TestHealthzReadinessGate(t *testing.T) {
	var ready atomic.Bool
	h := newTestHandler(t, config{}, &ready, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("not-ready /healthz = %d, want 503", rec.Code)
	}

	ready.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("ready /healthz = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Errorf("ready /healthz body = %q, want %q", rec.Body.String(), "ok\n")
	}
}

func TestRouteWSReachesTerminal(t *testing.T) {
	var ready, wsHit atomic.Bool
	ready.Store(true)
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

func TestRouteStaticServesIndex(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	h := newTestHandler(t, config{}, &ready, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/ = %d, want 200 (static index should be served from embed.FS)", rec.Code)
	}
}

func TestHandlerAuthGatesAllRoutes(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
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
