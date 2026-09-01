package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/cplieger/webhttp/v2"
)

// PERSIST_SCROLLBACK reaches the page by a startup byte swap rather than a
// template. The tests below hold that the swap is invisible downstream: the
// CSP hash, ETag and gzip body are all derived from the SERVED bytes, and a
// build that lost the marker fails at startup rather than on the first boot
// an operator enables the flag.

func TestApplyPersistFlagLeavesTheTreeAloneOnTheDefault(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	// The committed page already says what the default resolves to, so the common
	// path must add no indirection between the static handler and the embedded tree.
	out, err := applyPersistFlag(sub, true)
	if err != nil {
		t.Fatalf("applyPersistFlag(on): %v", err)
	}
	if out != sub {
		t.Errorf("applyPersistFlag(on) returned a wrapper; want the base FS unchanged")
	}
	html, err := fs.ReadFile(out, indexName)
	if err != nil {
		t.Fatalf("read %s: %v", indexName, err)
	}
	if !strings.Contains(string(html), persistFlagOn) {
		t.Errorf("the committed %s does not carry the on marker; the default and the page disagree", indexName)
	}
}

func TestApplyPersistFlagFlipsExactlyTheMarker(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	before, err := fs.ReadFile(sub, indexName)
	if err != nil {
		t.Fatalf("read base %s: %v", indexName, err)
	}
	// The opt-out direction, which is the one that needs an overlay now.
	out, err := applyPersistFlag(sub, false)
	if err != nil {
		t.Fatalf("applyPersistFlag(off): %v", err)
	}
	after, err := fs.ReadFile(out, indexName)
	if err != nil {
		t.Fatalf("read overlaid %s: %v", indexName, err)
	}
	if strings.Contains(string(after), persistFlagOn) {
		t.Errorf("overlaid %s still carries the on marker", indexName)
	}
	if !strings.Contains(string(after), persistFlagOff) {
		t.Fatalf("overlaid %s does not carry the off marker", indexName)
	}
	// The swap is one marker, so the two files differ by exactly the length
	// of the added "f".
	if len(string(after))-len(string(before)) != len(persistFlagOff)-len(persistFlagOn) {
		t.Errorf("overlay changed more than the marker: %d bytes before, %d after",
			len(before), len(after))
	}
	// The overlay must be transparent for every other file, or the static
	// handler's construction-time walk would lose assets.
	for _, name := range []string{"manifest.json", "favicon.svg"} {
		want, err := fs.ReadFile(sub, name)
		if err != nil {
			t.Fatalf("read base %s: %v", name, err)
		}
		got, err := fs.ReadFile(out, name)
		if err != nil {
			t.Fatalf("read overlaid %s: %v", name, err)
		}
		if string(got) != string(want) {
			t.Errorf("overlay changed %s", name)
		}
	}
}

func TestApplyPersistFlagStampsAPageThatShipsTheOffMarker(t *testing.T) {
	// The committed page's spelling documents the default; it does not decide
	// it, so a build whose index.html ships the off marker is still valid.
	shipsOff := fstest.MapFS{indexName: &fstest.MapFile{
		Data: []byte(`<html><head>` + persistFlagOff + `</head><body></body></html>`),
	}}

	t.Run("off", func(t *testing.T) {
		out, err := applyPersistFlag(shipsOff, false)
		if err != nil {
			t.Fatalf("applyPersistFlag(page carrying the off marker, enabled=false): %v", err)
		}
		html, err := fs.ReadFile(out, indexName)
		if err != nil {
			t.Fatalf("read %s: %v", indexName, err)
		}
		if !strings.Contains(string(html), persistFlagOff) {
			t.Errorf("resolved %s = %q, want it to carry %q", indexName, html, persistFlagOff)
		}
		if strings.Contains(string(html), persistFlagOn) {
			t.Errorf("resolved %s = %q, want no %q", indexName, html, persistFlagOn)
		}
	})

	t.Run("on", func(t *testing.T) {
		out, err := applyPersistFlag(shipsOff, true)
		if err != nil {
			t.Fatalf("applyPersistFlag(page carrying the off marker, enabled=true): %v", err)
		}
		html, err := fs.ReadFile(out, indexName)
		if err != nil {
			t.Fatalf("read %s: %v", indexName, err)
		}
		if !strings.Contains(string(html), persistFlagOn) {
			t.Errorf("resolved %s = %q, want it to carry %q", indexName, html, persistFlagOn)
		}
		if strings.Contains(string(html), persistFlagOff) {
			t.Errorf("resolved %s = %q, want no %q", indexName, html, persistFlagOff)
		}
	})
}

func TestApplyPersistFlagFailsLoudOnAMissingMarker(t *testing.T) {
	// A build that lost the marker is malformed; failing loud beats
	// discovering it months later on the first boot that flips the env var.
	for _, enabled := range []bool{false, true} {
		broken := fstest.MapFS{
			indexName: &fstest.MapFile{Data: []byte("<html><head></head></html>")},
		}
		if _, err := applyPersistFlag(broken, enabled); err == nil {
			t.Errorf("applyPersistFlag(enabled=%v) accepted a page with no marker", enabled)
		}
	}
}

func TestApplyPersistFlagRefusesADuplicatedMarker(t *testing.T) {
	// Two markers would flip one and leave the other, so the page would carry
	// contradictory answers. Counted across BOTH spellings, or a page
	// carrying one of each would pass while being the contradiction itself.
	for name, data := range map[string]string{
		"both off":    persistFlagOff + persistFlagOff,
		"both on":     persistFlagOn + persistFlagOn,
		"one of each": persistFlagOn + persistFlagOff,
	} {
		t.Run(name, func(t *testing.T) {
			doubled := fstest.MapFS{indexName: &fstest.MapFile{Data: []byte(data)}}
			_, err := applyPersistFlag(doubled, true)
			if err == nil {
				t.Fatal("applyPersistFlag accepted a page with two markers")
			}
			// The count is the whole diagnostic: "found 0" for a page carrying
			// one of each spelling sends an operator hunting for a missing
			// marker instead of the contradiction that is actually there.
			if !strings.Contains(err.Error(), "found 2") {
				t.Errorf("applyPersistFlag error = %q, want it to report two markers", err)
			}
		})
	}
}

func TestApplyPersistFlagFailsOnAnUnreadableIndex(t *testing.T) {
	if _, err := applyPersistFlag(fstest.MapFS{}, false); err == nil {
		t.Error("applyPersistFlag accepted a tree with no index.html")
	}
}

// TestPersistFlagReachesTheServedPage is the integration assertion: the flag
// the operator set is what the browser receives.
func TestPersistFlagReachesTheServedPage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		want    string
		reject  string
	}{
		{name: "off", enabled: false, want: persistFlagOff, reject: persistFlagOn},
		{name: "on", enabled: true, want: persistFlagOn, reject: persistFlagOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ready webhttp.Ready
			ready.Set(true)
			h := newTestHandler(t, config{persistScrollback: tc.enabled}, &ready, nil)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "localhost"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET / = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("served page is missing %q", tc.want)
			}
			if strings.Contains(body, tc.reject) {
				t.Errorf("served page still carries %q", tc.reject)
			}
			// The static handler precomputed its validator from the same bytes
			// it serves, or a browser would revalidate against a hash of the
			// other variant and be handed a 304 for a page it does not have.
			if rec.Header().Get("ETag") == "" {
				t.Error("served page carries no ETag")
			}
		})
	}
}

// TestPersistFlagOverlayKeepsTheServedBytesSelfConsistent covers what the
// ordering is actually load-bearing for. Flipping the marker cannot change
// any inline script's bytes (it is a <meta> element), so it cannot change a
// script-src hash; what the ordering DOES buy is that the static handler
// hashes and gzips the bytes it serves. This drives the overlay direction
// (the opt-out), asserts the CSP still admits the served page's scripts,
// and asserts the ETag differs between the two variants — the property a
// browser depends on and a shared cache would break.
func TestPersistFlagOverlayKeepsTheServedBytesSelfConsistent(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	overlaid, err := applyPersistFlag(sub, false) // the direction that rewrites
	if err != nil {
		t.Fatalf("applyPersistFlag(off): %v", err)
	}
	if overlaid == sub {
		t.Fatal("applyPersistFlag(off) returned the base FS; the overlay was not exercised")
	}
	csp, err := buildCSPPolicy(overlaid)
	if err != nil {
		t.Fatalf("buildCSPPolicy over the overlaid tree: %v", err)
	}
	html, err := fs.ReadFile(overlaid, indexName)
	if err != nil {
		t.Fatalf("read overlaid %s: %v", indexName, err)
	}
	hashes := webhttp.InlineScriptHashes(html)
	if len(hashes) < 2 {
		t.Fatalf("found %d inline scripts, want >= 2 (importmap + module bootstrap)", len(hashes))
	}
	for _, token := range hashes {
		if !strings.Contains(csp, token) {
			t.Errorf("CSP built from the overlaid page is missing %s\nCSP: %s", token, csp)
		}
	}

	// The ETag depends on the handler seeing the served bytes: two different
	// pages must not share a validator.
	etag := func(enabled bool) string {
		t.Helper()
		var ready webhttp.Ready
		ready.Set(true)
		h := newTestHandler(t, config{persistScrollback: enabled}, &ready, nil)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "localhost"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / = %d, want 200", rec.Code)
		}
		return rec.Header().Get("ETag")
	}
	on, off := etag(true), etag(false)
	if on == "" || off == "" {
		t.Fatal("a served page carried no ETag")
	}
	if on == off {
		t.Error("the on and off pages share an ETag; a cache would serve one for the other")
	}
}

func TestApplyBoolEnvRejectsANonBoolean(t *testing.T) {
	// Strict rather than "non-empty is true": a typo must be a startup
	// error rather than a container that quietly persists terminal output.
	t.Setenv("PERSIST_SCROLLBACK", "flase")
	dst := false
	err := applyBoolEnv("PERSIST_SCROLLBACK", &dst)
	if err == nil {
		t.Fatal("applyBoolEnv accepted \"flase\"")
	}
	if !strings.Contains(err.Error(), "PERSIST_SCROLLBACK") {
		t.Errorf("error %q does not name the key", err)
	}
	// The value is deliberately NOT echoed (CWE-532); this pins that the app
	// does not reintroduce it by rewording.
	if strings.Contains(err.Error(), "flase") {
		t.Errorf("error %q echoes the rejected value", err)
	}
	if dst {
		t.Error("applyBoolEnv set the destination on a rejected value")
	}
}

func TestApplyBoolEnvLeavesTheDefaultWhenUnset(t *testing.T) {
	var dst bool
	// A name nothing sets, not a knob: it exists only to exercise the unset arm.
	if err := applyBoolEnv("PERSIST_SCROLLBACK_ABSENT", &dst); err != nil {
		t.Fatalf("applyBoolEnv(unset): %v", err)
	}
	if dst {
		t.Error("an unset var turned the flag on")
	}
	// An empty value is "unset" too, so a compose entry left blank keeps
	// the default rather than failing the boot.
	t.Setenv("PERSIST_SCROLLBACK", "")
	if err := applyBoolEnv("PERSIST_SCROLLBACK", &dst); err != nil {
		t.Fatalf("applyBoolEnv(empty): %v", err)
	}
	if dst {
		t.Error("an empty var turned the flag on")
	}
}

func TestApplyBoolEnvAcceptsTheUsualSpellings(t *testing.T) {
	for _, raw := range []string{"true", "TRUE", "1", "yes", "on"} {
		t.Setenv("PERSIST_SCROLLBACK", raw)
		var dst bool
		if err := applyBoolEnv("PERSIST_SCROLLBACK", &dst); err != nil {
			t.Fatalf("applyBoolEnv(%q): %v", raw, err)
		}
		if !dst {
			t.Errorf("applyBoolEnv(%q) did not set the flag", raw)
		}
	}
	for _, raw := range []string{"false", "0", "no", "off"} {
		t.Setenv("PERSIST_SCROLLBACK", raw)
		dst := true
		if err := applyBoolEnv("PERSIST_SCROLLBACK", &dst); err != nil {
			t.Fatalf("applyBoolEnv(%q): %v", raw, err)
		}
		if dst {
			t.Errorf("applyBoolEnv(%q) did not clear the flag", raw)
		}
	}
}

func TestPersistScrollbackDefaultsOn(t *testing.T) {
	// An opt-OUT rather than opt-in: an off-by-default entry in an env
	// table is off for everyone in practice.
	t.Setenv("SESSION_CMD", "/bin/true")
	// Cleared explicitly, since an ambient value would make the default
	// this asserts unobservable.
	t.Setenv("PERSIST_SCROLLBACK", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig(): %v", err)
	}
	if !cfg.persistScrollback {
		t.Error("persistScrollback defaults off; want on")
	}
}

func TestPersistScrollbackHonoursTheOptOut(t *testing.T) {
	t.Setenv("SESSION_CMD", "/bin/true")
	t.Setenv("PERSIST_SCROLLBACK", "false")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig(): %v", err)
	}
	if cfg.persistScrollback {
		t.Error("PERSIST_SCROLLBACK=false did not turn it off")
	}
}

func TestPersistScrollbackRejectsAMalformedOptOut(t *testing.T) {
	// A typo must abort startup rather than silently leaving the default
	// on: an operator writing "flase" was trying to turn this OFF.
	t.Setenv("SESSION_CMD", "/bin/true")
	t.Setenv("PERSIST_SCROLLBACK", "flase")
	if _, err := loadConfig(); err == nil {
		t.Fatal("loadConfig() accepted PERSIST_SCROLLBACK=flase")
	}
}
