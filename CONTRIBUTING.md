# Contributing to web-terminal-server

The [shared rules](https://github.com/cplieger/.github/blob/main/CONTRIBUTING.md) for commits, releases, synced files and checks apply here.

## Scope

Changes to how the terminal draws, takes input or keeps scrollback belong in [web-terminal-engine](https://github.com/cplieger/web-terminal-engine) or [web-terminal-ui](https://github.com/cplieger/web-terminal-ui). This repository builds their published packages into its page, so a fix made here reaches none of the other apps that use them.

## Checks

`bash scripts/run-cdp.sh` checks the page in a headless Chromium against a real terminal, which the Go tests cannot reach. CI does not run it. Run it after a change to `static/index.html`, the security headers, or the engine or UI version.

The suite needs Node 22 or later and a Chromium on `PATH` or in the Playwright cache. It reuses an existing `./web-terminal-server-bin`, so run `bash scripts/dev-build.sh` again after each change.

`scripts/dev-build.sh` compiles the page from sibling checkouts at `../web-terminal-engine` and `../web-terminal-ui`, which `ENGINE_DIR=` and `UI_DIR=` override.

The script compiles whatever those checkouts hold and never reads the versions the Dockerfile pins. For an engine or UI version change, first check out each sibling at the tag for the version in `CPLIEGER_WEB_TERMINAL_ENGINE_VERSION` or `CPLIEGER_WEB_TERMINAL_UI_VERSION`.

The Go server it builds uses the engine version `go.mod` pins. To build the Go server against an unreleased engine too, add a `go.work`, which git ignores:

```sh
go work init .
go work edit -replace=github.com/cplieger/web-terminal-engine/v6=../web-terminal-engine
```

CI builds without it, so `go.mod` must still pin a published engine version.
