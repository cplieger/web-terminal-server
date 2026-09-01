package main

// The one server fact this app's client needs at boot. No templating: the CSP
// pins a sha256 of the inline module script and the static handler
// precomputes an ETag and gzip body per file, so a per-request rewrite would
// fight all three. The flag is applied ONCE at startup over the embedded
// tree, before either reader sees it, and the resolved value is ALWAYS
// stamped in either direction — index.html's committed value documents the
// default rather than deciding anything. Persistence is ON; the env var is
// the opt-OUT.

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// The marker, in both spellings. Fixed literals rather than an attribute
// rewrite, so the substitution is an exact-match swap with no HTML parsing.
const (
	persistFlagOff = `<meta name="wt-persist-scrollback" content="off">`
	persistFlagOn  = `<meta name="wt-persist-scrollback" content="on">`
)

// indexName is the file the flag lives in, relative to the static sub-FS.
const indexName = "index.html"

// applyPersistFlag returns the static FS the handler and the CSP builder
// should both read: the embedded tree with index.html's marker stamped to the
// resolved value, or the tree unchanged when it already says that.
//
// The marker is verified on every boot, in both spellings — a build that lost
// it fails startup rather than surprising an operator later.
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
		// Already resolved: no wrapper needed.
		return base, nil
	}
	return overlayFS{
		base: base,
		name: indexName,
		data: bytes.Replace(html, []byte(other), []byte(want), 1),
	}, nil
}

// overlayFS serves one replacement file over a base fs.FS and delegates
// everything else. Implements fs.ReadDirFS as well as fs.FS because the
// static handler walks the tree once at construction via ReadDir, then reads
// content through Open — that split is what makes the byte swap invisible
// downstream.
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

// The three interfaces net/http drives this overlay through. Asserted because
// a dropped method here is invisible to a reference search and would surface
// as a runtime 500 rather than a compile error.
var (
	_ fs.ReadDirFS = overlayFS{}
	_ fs.File      = (*memFile)(nil)
	_ io.Seeker    = (*memFile)(nil)
	_ fs.FileInfo  = memInfo{}
)

// memFile is a read-only fs.File over a byte slice. Implements io.Seeker
// because http.ServeContent needs one to answer a Range request.
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

// memInfo is the fs.FileInfo for a memFile. ModTime is the zero time,
// matching embed.FS: the content-hash ETag is the real validator, and a
// synthesized timestamp would put a Last-Modified on one file and not its
// neighbours.
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
