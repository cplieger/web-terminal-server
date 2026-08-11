package main

// The one server fact this app's client needs to know at boot.
//
// The front end is a committed static file with an inline module script; nothing
// here templates it, and that is worth keeping — the CSP pins a sha256 of that
// script, the static handler precomputes an ETag and a gzip body per file, and a
// per-request rewrite would fight all three. So WT_PERSIST_SCROLLBACK is applied
// ONCE at startup, by overlaying the one changed byte range over the embedded
// tree before either the static handler or the CSP builder reads it. Both see the
// same bytes, so the ETag, the gzip body and the script hash are all computed
// over what the browser actually receives.
//
// Persistence is ON by default, here as in the sibling apps, because the defect it
// fixes is universal: without it a reloaded or browser-discarded tab asks the
// server for its whole retained scrollback and refills the buffer over the wire,
// which is the normal case on a phone and reads as a fault rather than a reload.
// An off-by-default entry in an env table is off for everyone in practice, and the
// users who need it most are the least likely to go looking for a flag.
//
// The flag remains, as the opt-OUT, because enabling this DOES move something: up
// to a thousand lines of WT_CMD's output sit in the browser's localStorage,
// readable from that browser without reaching this server and outliving the tab.
// Almost every way to read it also hands over a live root shell, so the snapshot
// is rarely the weakest link — but not always: a laptop off the VPN, a stopped
// container or an expired credential leaves the snapshot readable while the shell
// is not. That is the case an operator with a shared device or a compliance
// constraint is turning off, and the README says so where they will look.
//
// The resolved value is ALWAYS stamped, in either direction, rather than the page
// carrying the default and being rewritten one way. That removes the question of
// which direction the rewrite runs, and leaves index.html's committed value as
// documentation of the default rather than a load-bearing input.

import (
	"bytes"
	"fmt"
	"io/fs"
	"time"
)

// The marker, in both spellings. A fixed pair of literals rather than an attribute
// rewrite: the substitution is then an exact-match swap that either happens or
// fails loudly, with no HTML parsing and no way to match a second element that
// happens to look similar.
const (
	persistFlagOff = `<meta name="wt-persist-scrollback" content="off">`
	persistFlagOn  = `<meta name="wt-persist-scrollback" content="on">`
)

// indexName is the file the flag lives in, relative to the static sub-FS.
const indexName = "index.html"

// applyPersistFlag returns the static FS the handler and the CSP builder should
// both read: the embedded tree with index.html's marker stamped to the resolved
// value, or the tree unchanged when it already says that.
//
// The marker is verified on EVERY boot, in both spellings, whichever way the flag
// is set. A build that lost it is malformed, and finding that out at startup is
// much better than finding out on the first boot an operator flips the env var —
// which could be months later, on a container whose whole purpose is then to
// change the thing that silently cannot be changed. Same fail-loud posture as
// buildCSPPolicy.
func applyPersistFlag(base fs.FS, enabled bool) (fs.FS, error) {
	html, err := fs.ReadFile(base, indexName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", indexName, err)
	}
	on, off := bytes.Count(html, []byte(persistFlagOn)), bytes.Count(html, []byte(persistFlagOff))
	if on+off != 1 {
		return nil, fmt.Errorf(
			"want exactly one wt-persist-scrollback marker in %s, found %d", indexName, on+off)
	}
	want, other := persistFlagOff, persistFlagOn
	if enabled {
		want, other = persistFlagOn, persistFlagOff
	}
	if bytes.Contains(html, []byte(want)) {
		// Already says the resolved value: no wrapper, so the common path adds no
		// indirection between the static handler and the embedded tree.
		return base, nil
	}
	return overlayFS{
		base: base,
		name: indexName,
		data: bytes.Replace(html, []byte(other), []byte(want), 1),
	}, nil
}

// overlayFS serves one replacement file over a base fs.FS and delegates
// everything else.
//
// It implements fs.ReadDirFS as well as fs.FS because the static handler walks
// the tree once at construction to precompute ETags and gzip bodies: the walk
// enumerates through ReadDir (the base's answer, since the overlay adds no
// entries and removes none) and reads content through Open (this type's answer
// for the one overlaid name). That split is what makes a byte swap invisible to
// everything downstream.
type overlayFS struct {
	base fs.FS
	name string
	data []byte
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if name == o.name {
		return &memFile{name: name, r: bytes.NewReader(o.data)}, nil
	}
	return o.base.Open(name)
}

func (o overlayFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(o.base, name)
}

// memFile is a read-only fs.File over a byte slice. It implements io.Seeker
// because http.ServeContent needs one to answer a Range request and to size the
// body; without it the identity fallback would refuse the overlaid file.
type memFile struct {
	r    *bytes.Reader
	name string
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	return memInfo{name: f.name, size: f.r.Size()}, nil
}
func (f *memFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	return f.r.Seek(offset, whence)
}
func (f *memFile) Close() error { return nil }

// memInfo is the fs.FileInfo for a memFile. ModTime is deliberately the zero
// time, matching what embed.FS reports: the static handler's content-hash ETag is
// the validator here, and a synthesized timestamp would put a Last-Modified on
// one file and not its neighbours.
type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }
