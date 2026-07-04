# Contributing to web-terminal-server

`web-terminal-server` is a thin generic server that mounts the
[`github.com/cplieger/web-terminal-engine`](https://github.com/cplieger/web-terminal-engine)
engine and serves the
[`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) front
end. Org-wide defaults are inherited from
[cplieger/.github](https://github.com/cplieger/.github); CI, lint, and license
files are synced from [cplieger/ci](https://github.com/cplieger/ci) — do not
hand-edit `.golangci.yaml`, `.gremlins.yaml`, `.editorconfig`, `cliff.toml`, the
workflows, or `LICENSE`.

## Architecture

- `main.go` is the whole server: env parsing (`WT_*`), `terminal.NewSessionManager`
  (a per-session `terminal.NewHandler` factory), a `ServeMux` mounting
  `/ws?session=<id>`, the session REST API `/api/sessions` (+`/`) behind a create
  rate limit, the status SSE `/api/sessions/events`, `/healthz`, and the embedded
  static front end at `/` (the engine's `/debug/*` routes are intentionally **not**
  exposed). Middleware (outermost first): an slog
  access log, security headers (`X-Content-Type-Options: nosniff` + a
  Content-Security-Policy), optional HTTP Basic auth, and
  `http.CrossOriginProtection`. The `statusWriter`
  implements `Unwrap()` so the WebSocket hijack reaches the real
  `ResponseWriter` through the access-log wrapper.
- The browser bundle is **not** authored here. It is the engine + UI packages
  compiled to `static/vendor/` at build time; only `static/index.html` (the
  scaffold + importmap + inline `createTerminal(root, { features: presetTabbed() })` call) is committed, which is enough
  for `//go:embed static` to have content.

Keep the server thin: terminal behavior belongs in the engine or the UI
package, not here.

## Local development

The engine and UI are published, so a plain checkout builds against the
released packages: `go.mod` pins `github.com/cplieger/web-terminal-engine/v2`
(`go.sum` carries its checksums), and `scripts/dev-build.sh` and the Dockerfile
pull the published `@cplieger/web-terminal-*` npm tarballs.

```sh
go build ./...              # server only, against the published engine
bash scripts/dev-build.sh   # full build: compile engine+UI -> static/vendor,
                            # bundle CSS, fetch font, embed, -> ./web-terminal-server-bin
./web-terminal-server-bin   # runs on 127.0.0.1:7681
```

`scripts/dev-build.sh` builds the browser bundle from sibling working-tree
checkouts (`../web-terminal-engine` and `../web-terminal-ui`; override with
`ENGINE_DIR=` / `UI_DIR=`), so you can iterate on unreleased engine or UI
changes before they ship.

### Real-browser verification (CDP)

`scripts/cdp-*.cjs` are zero-dependency (Node 22) live-verify harnesses. Most
drive the server in a headless Chromium over the DevTools Protocol and assert
the rendered DOM — the display half the Go tests can't reach; one
(`cdp-scrollback`) is a wire-level check against the raw WebSocket. Each one
asserts and exits 0 (pass) or non-zero (fail); none needs a
human to read the output. They exercise the engine + UI stack (nothing
server-specific), so this generic server is the family's baseline testing
ground for them.

Run the whole suite with one command. It provisions everything locally — a
headless Chromium (a real one on `PATH`, or the Playwright-cached build) plus a
loopback server on the fixtures — runs every harness, and returns non-zero if
any fail:

```sh
bash scripts/run-cdp.sh
```

Individual harnesses run against an existing `CDP_URL=` / `WT_URL=` (e.g. a
shared Chromium sidecar for interactive debugging); see the `cdp-*.cjs` sources
for what each one asserts.

Fixtures: `emit-fixture.sh` (continuous numbered lines) and `emit-ed3.sh`
(bursts scrollback, then blocks on stdin until the client triggers an ED3).

To build the **Go server** against an unreleased local engine instead of the
pinned published one, add a `go.work` that redirects the module to your sibling
checkout:

```
go 1.26.4
use .
replace github.com/cplieger/web-terminal-engine/v2 => ../web-terminal-engine
```

`go.work` is gitignored and dockerignored (local-dev only); the `replace` reads
`../web-terminal-engine/go.mod` directly so the build uses your working-tree
engine instead of the version pinned in `go.mod`. Delete it to go back to the
published engine. The Dockerfile must never see a `go.work`: it does
`COPY . ./`, and a `replace` pointing at a path absent from the build context
would break the in-container build, which is why `.dockerignore` excludes it.

## Conventions

- Go module is domain-rooted (`github.com/cplieger/web-terminal-server`), built
  `CGO_ENABLED=0`. Formatting is gofumpt + gci (enforced by the synced
  `.golangci.yaml`); run `gofmt`/`golangci-lint fmt` before committing.
- slog-only observability (one structured line per request); no Prometheus
  endpoint.
- Dockerfile follows the shared `cplieger/ci` conventions: `# check=error=true`, native
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
