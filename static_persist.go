package main

// The one server fact this app's client needs at boot. Nothing templates the
// committed static page, on purpose: the CSP pins a sha256 of its inline module
// script and the static handler precomputes an ETag and a gzip body per file, so a
// per-request rewrite would fight all three. The flag is applied ONCE at startup
// over the embedded tree, before either reader sees it, and the resolved value is
// ALWAYS stamped in either direction — so index.html's committed value documents
// the default rather than deciding anything. Persistence is ON; the env var is the
// opt-OUT, and README states what enabling it leaves in the browser.

import (
	"bytes"
	"fmt"
	"io"
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
// The marker is verified on EVERY boot, in both spellings, whichever way the flag is
// set. A build that lost it is malformed, and startup is a much better place to find
// that out than the first boot an operator flips the env var — months later, on a
// container whose whole purpose is then to change something that cannot be changed.
func applyPersistFlag(base fs.FS, enabled bool) (fs.FS, error) {
	html, err := fs.ReadFile(base, indexName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", indexName, err)
	}
	on, off := bytes.Count(html, []byte(persistFlagOn)), bytes.Count(html, []byte(persistFlagOff))
	if on+off != 1 {
		return nil, fmt.Errorf(
			"want exactly one wt-persist-scrollback marker in %s, found %d", indexName, on+off,
		)
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

// overlayFS serves one replacement file over a base fs.FS and delegates everything
// else. It implements fs.ReadDirFS as well as fs.FS because the static handler walks
// the tree once at construction: the walk enumerates through ReadDir (the base's
// answer, since the overlay adds and removes no entries) and reads content through
// Open (this type's answer for the one overlaid name). That split is what makes the
// byte swap invisible downstream.
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

// The three interfaces net/http drives this overlay through. Stated as assertions
// because every method below exists only to satisfy one of them: the caller holds
// the interface, never the concrete type, so a dropped method is invisible to a
// reference search (see .punused-ignore) and would surface as a 500 on one asset at
// runtime. Here it is a compile error.
var (
	_ fs.ReadDirFS = overlayFS{}
	_ fs.File      = (*memFile)(nil)
	_ io.Seeker    = (*memFile)(nil)
	_ fs.FileInfo  = memInfo{}
)

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
