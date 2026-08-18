# Contributing to web-terminal-server

`web-terminal-server` is a thin generic server that mounts the
[`github.com/cplieger/web-terminal-engine`](https://github.com/cplieger/web-terminal-engine)
engine and serves the
[`@cplieger/web-terminal-ui`](https://github.com/cplieger/web-terminal-ui) front
end. Org-wide defaults are inherited from
[cplieger/.github](https://github.com/cplieger/.github); CI, lint, and license
files are synced from [cplieger/ci](https://github.com/cplieger/ci); do not
hand-edit `.golangci.yaml`, `.gremlins.yaml`, `.editorconfig`, `cliff.toml`, the
workflows, or `LICENSE`.

## Architecture

- `main.go` is the server: env parsing, `terminal.NewSessionManager`
  (a per-session `terminal.NewHandler` factory), a `ServeMux` mounting
  `/ws?session=<id>`, the session REST API `/api/sessions` (+`/`) behind a create
  rate limit, the status SSE `/api/sessions/events`, `/healthz`, and the embedded
  static front end at `/` (the engine's `/debug/*` routes are intentionally **not**
  exposed). Middleware (outermost first): request logging (one structured slog
  line per request, with a spoof-safe `client_ip`), panic recovery, the `/ws`
  attach record, security headers (`nosniff`, a Content-Security-Policy whose
  `script-src` and `style-src` pin a sha256 of each inline block in the embedded
  `index.html`, computed at construction and fail-loud on a malformed embed, plus
  COOP, `Referrer-Policy: same-origin` and a `Permissions-Policy`), the
  `ALLOWED_HOSTS` host allowlist, the failed-auth throttle, optional HTTP Basic
  auth, `http.CrossOriginProtection`, and the canonical-path guard innermost. The
  logging wrapper's response recorder implements `Unwrap()` so the WebSocket hijack
  reaches the real `ResponseWriter`.
- `static_persist.go` is the second production file, and it holds the one
  invariant most likely to trip a contributor. `PERSIST_SCROLLBACK` has to reach
  an inline module script in a COMMITTED, embedded file, and nothing here templates:
  the CSP pins a sha256 of that script and `webhttp.StaticHandler` precomputes an
  ETag and a gzip body per file, so a per-request rewrite would fight all three. The
  flag is applied ONCE at startup, over the embedded tree, BEFORE either the static
  handler or the CSP builder reads it. Do not move that overlay after either
  consumer, and do not reach for templating: the failure mode is a blank terminal
  with a console CSP violation. The marker is verified on every boot in both
  spellings and a build that lost it aborts startup.
- The browser bundle is **not** authored here. It is the engine + UI packages
  compiled to `static/vendor/` at build time; only `static/index.html` (the
  scaffold + importmap + inline `createTerminal(root, { features: presetTabbed() })` call) is committed, which is enough
  for `//go:embed static` to have content.

Keep the server thin: terminal behavior belongs in the engine or the UI
package, not here.

## Conventions

- Go module is domain-rooted (`github.com/cplieger/web-terminal-server`), built
  `CGO_ENABLED=0`. Formatting is gofumpt + gci (enforced by the synced
  `.golangci.yaml`); run `gofmt`/`golangci-lint fmt` before committing.
- **Nothing below `main` may exit.** `run()` reports every failure by returning an
  attributed error and `main` is the only `os.Exit`, which is what lets the deferred
  teardown run on every path. Each failure site tags itself with `atStage`, and
  `main` renders one `ERROR` line carrying that `stage`. The stage VALUES are the
  log surface an operator queries, pinned by `TestStageValuesAreStable`, so renaming
  one is a breaking change. Do not reintroduce an `os.Exit` in `run`, and do not
  drop the error return on a path that looks terminal: the duplicated teardown that
  omission requires is the defect this shape removed.
- **A rejected environment value never reaches the log.** Warnings and startup
  errors name the variable and the accepted shape, never the value, because a
  compose interpolation mistake is what puts a credential on a variable. The same
  rule covers the session id, which is the `/ws` attach capability: it is logged
  only through `terminal.LogID`.
- slog-only observability (one structured line per request), with UTC
  timestamps via `slogx` (its `UTCTime` `ReplaceAttr`, so the image needs no `TZ`
  and embeds no `time/tzdata`); no Prometheus endpoint.
- Dockerfile follows the shared `cplieger/ci` conventions: `# check=error=true`, native
  per-arch builds (no QEMU/xx), `GOTOOLCHAIN=auto`, cache-mounted `go mod
  download` and `go build`. The `# renovate:` ARGs track tool and package versions,
  and every build-time download is verified against a recorded sha256 — a `# repin:`
  marker above each digest ARG is what lets a bot recompute it, so keep the marker
  on the line directly above its ARG.
- The three build steps shared with `scripts/dev-build.sh` live in
  `scripts/vendor-tsc.sh`, `scripts/assert-emit.sh` and `scripts/css-bundle.sh`.
  Change them there rather than inlining a copy: the copies had already drifted
  once, and each script refuses a failure a shell one-liner swallowed (an empty
  source list that emits nothing and exits 0, a page importing a module tsc never
  wrote, a truncated CSS member replacing a working bundle).

## Local development

The engine and UI are published, so a plain checkout builds against the
released packages: `go.mod` pins `github.com/cplieger/web-terminal-engine/v5`
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
the rendered DOM (the display half the Go tests can't reach); one
(`cdp-scrollback`) is a wire-level check against the raw WebSocket. Each one
asserts and exits 0 (pass) or non-zero (fail); none needs a
human to read the output. They exercise the engine + UI stack (nothing
server-specific), so this generic server is the family's baseline testing
ground for them.

Run the whole suite with one command. It provisions a headless Chromium (a
real one on `PATH`, or the Playwright-cached build) and a loopback server on
the fixtures, runs every harness, and returns non-zero if
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

```text
go 1.26.6
use .
replace github.com/cplieger/web-terminal-engine/v5 => ../web-terminal-engine
```

Use `use .` plus `replace`, not a `use (...)` list naming the sibling: as a
workspace main module the engine resolves its OWN requirements, which fails
against any version not yet published.

`go.work` is gitignored and dockerignored (local-dev only); the `replace` reads
`../web-terminal-engine/go.mod` directly so the build uses your working-tree
engine instead of the version pinned in `go.mod`. Delete it to go back to the
published engine. The Dockerfile must never see a `go.work`: it does
`COPY . ./`, and a `replace` pointing at a path absent from the build context
would break the in-container build, which is why `.dockerignore` excludes it.

## Running checks

The same battery CI runs, from the repo root:

```sh
go build ./... && go vet ./...
go test -count=1 -race ./...
golangci-lint run
hadolint Dockerfile
shellcheck scripts/*.sh
bash scripts/run-cdp.sh          # real-browser display checks (see below)
bash scripts/smoke.sh            # needs a built image; see .github/workflows/smoke.yml
```

`go test` covers the env parsing, the CSP contract, the middleware chain, the
canonical-path guard and the wire-floor gate — including tests that read the
shipped `Dockerfile` as text, so deleting the gate's build step fails the suite
rather than shipping an incompatible client.

## Commits and PRs

Branch from `main`, keep changes focused, open a PR. Conventional Commits;
git-cliff parses them for the changelog and the version bump
(`feat: add an environment passthrough`,
`fix: clamp scrollback to a sane minimum`).

## Conduct & security

By participating you agree to the
[Code of Conduct](https://github.com/cplieger/.github/blob/main/CODE_OF_CONDUCT.md).
Report security issues through the
[security policy](https://github.com/cplieger/.github/blob/main/SECURITY.md),
never in a public issue.
