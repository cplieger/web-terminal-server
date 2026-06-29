# Contributing to web-terminal-server

`web-terminal-server` is a thin generic server that mounts the
[`github.com/cplieger/web-terminal`](https://github.com/cplieger/web-terminal)
engine and serves the
[`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) front
end. Org-wide defaults are inherited from
[cplieger/.github](https://github.com/cplieger/.github); CI, lint, and license
files are synced from [cplieger/ci](https://github.com/cplieger/ci) — do not
hand-edit `.golangci.yaml`, `.gremlins.yaml`, `.editorconfig`, `cliff.toml`, the
workflows, or `LICENSE`.

## Architecture

- `main.go` is the whole server: env parsing (`WT_*`), `terminal.NewHandler`,
  a `ServeMux` mounting `/ws` (the engine, via `ServeHTTP` — the engine's
  `/debug/*` routes are intentionally **not** exposed), `/healthz`, and the
  embedded static front end at `/`. Middleware: an slog access log, optional
  HTTP Basic auth, and `http.CrossOriginProtection`. The `statusWriter`
  implements `Unwrap()` so the WebSocket hijack reaches the real
  `ResponseWriter` through the access-log wrapper.
- The browser bundle is **not** authored here. It is the engine + UI packages
  compiled to `static/vendor/` at build time; only `static/index.html` (the
  scaffold + importmap + inline `mount()` call) is committed, which is enough
  for `//go:embed static` to have content.

Keep the server thin: terminal behavior belongs in the engine or the UI
package, not here.

## Local development

The engine and UI are not published yet, so local builds resolve them from
sibling working-tree checkouts (`../vterm` and `../web-terminal-ui`).

```sh
go build ./...          # server only (needs the go.work below)
bash scripts/dev-build.sh   # full build: compile engine+UI -> static/vendor,
                            # bundle CSS, fetch font, embed, -> ./web-terminal-server-bin
./web-terminal-server-bin   # runs on 127.0.0.1:7681
```

`scripts/dev-build.sh` creates the browser bundle from the sibling checkouts
(override with `ENGINE_DIR=` / `UI_DIR=`). The Go build needs a `go.work` that
resolves the unpublished engine:

```
go 1.26.4
use .
replace github.com/cplieger/web-terminal => ../vterm
```

`go.work` is gitignored (local-dev only). The `replace` reads `../vterm/go.mod`
directly so Go doesn't try to fetch the placeholder version pinned in `go.mod`.

> **Pre-publish note.** Until the engine and UI packages are published, the
> committed `go.mod` pins placeholder versions and `go.sum` lacks the engine
> entry, so CI (which builds without `go.work` and from the published
> registries) cannot go green. The engine and UI must publish first; then
> `go.mod` is pinned to the real versions, `go mod tidy` records the engine in
> `go.sum`, and the Dockerfile `ARG` versions are bumped (Renovate tracks the
> `# renovate:` comments thereafter).

## Conventions

- Go module is domain-rooted (`github.com/cplieger/web-terminal-server`), built
  `CGO_ENABLED=0`. Formatting is gofumpt + gci (enforced by the synced
  `.golangci.yaml`); run `gofmt`/`golangci-lint fmt` before committing.
- slog-only observability (one structured line per request); no Prometheus
  endpoint.
- Dockerfile follows the fleet conventions: `# check=error=true`, native
  per-arch builds (no QEMU/xx), `GOTOOLCHAIN=auto`, layer-cached `go mod
  download`. The `# renovate:` ARGs track tool and package versions.

## Commits and PRs

Branch from `main`, keep changes focused, open a PR. Conventional Commits —
git-cliff parses them for the changelog and the version bump
(`feat: add WT_ENV passthrough`, `fix: clamp scrollback to a sane minimum`).

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md) —
never in a public issue.
