// Command wirecheck asserts wire-protocol compatibility between the Go
// server half (the web-terminal-engine module go.mod pins) and the served
// TS client half (the Dockerfile-ARG-pinned npm artifact). The two halves are
// pinned INDEPENDENTLY — Renovate moves the Go module and the npm ARGs in
// separate PRs, and a Go-only engine release publishes no npm package at all —
// so nothing but this gate proves the pair the image ships is one the engine
// considers compatible.
//
// The compatibility RULE is the engine's (terminal.WirePairIncompatibility —
// the same verdict its runtime handshake reaches, so this gate can never
// disagree with the close-4002 refusal). This program supplies the Go side
// from the engine's public constants; the client side is read from the vendored
// artifact's own published wire-compatibility manifest.
//
// Without the gate a declared-incompatible pairing builds green, deploys,
// answers /healthz healthy, and then refuses every session at first connect
// with close code 4002 — an outage that looks like a healthy container. Fail
// the build instead.
//
// Exit 0: the pairing is declared-compatible. Exit 1: a declared floor is
// violated. Exit 2: usage error — the client revisions could not be resolved, so
// the gate itself is broken and no pin should move.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/cplieger/web-terminal-engine/v3/terminal"
)

// usageErrMsg is the one line every exit-2 path prints last, so the broken-gate case is
// greppable in a build log and cannot be mistaken for a compatibility verdict. A const
// because three call sites emit it and a fourth (the test) pins it.
const usageErrMsg = "ERROR wire-floor-gate-usage: the client wire revisions are unusable — pass -manifest <engine-pkg>/wire-compatibility.json (the extraction is broken — fix the gate, do not bump a pin)"

// readManifest resolves the client half from the engine artifact's own published
// manifest, via the engine's exported decoder.
//
// This is the ONLY input path. A hand-typed revision was the alternative and it is worse
// than no input at all: it is a second, unverifiable source for the one program whose job
// is to be trusted about whether the gate or a pin is at fault. The DECODING is the
// engine's — it owns the format, its schema check and its unusable-revisions check. What
// stays here is the POLICY: every failure is exit 2, never a compatibility verdict,
// because a manifest the gate cannot read means the gate is broken. An unknown schema is
// named separately since its remedy is the opposite one: bump this gate.
func readManifest(path string, stderr io.Writer) (clientRev, clientMinServer int, ok bool) {
	m, err := terminal.ReadWireManifest(path)
	if err != nil {
		if errors.Is(err, terminal.ErrWireManifestSchema) {
			fmt.Fprintf(stderr, "ERROR wire-floor-gate-usage: %s: the manifest format moved ahead of this gate (fix the gate, do not bump a pin): %v\n", path, err)
		} else {
			fmt.Fprintf(stderr, "ERROR wire-floor-gate-usage: cannot read the engine's wire-compatibility manifest at %s (fix the gate, do not bump a pin): %v\n", path, err)
		}
		return 0, 0, false
	}
	return m.ProtocolVersion, m.MinimumServerProtocolVersion, true
}

func main() {
	// ContinueOnError so -h and a genuine parse error are distinguishable: help is a
	// success, an unknown flag is a broken gate. flag's ExitOnError default collapses
	// both into status 2 with no line saying which happened.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	manifest := flag.String("manifest", "", "path to the vendored engine artifact's wire-compatibility.json")
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, usageErrMsg)
		os.Exit(2)
	}
	if *manifest == "" {
		fmt.Fprintln(os.Stderr, usageErrMsg)
		os.Exit(2)
	}
	rev, minServer, ok := readManifest(*manifest, os.Stderr)
	if !ok {
		fmt.Fprintln(os.Stderr, usageErrMsg)
		os.Exit(2)
	}
	os.Exit(run(rev, minServer, os.Stdout, os.Stderr))
}

// run performs the wire-floor gate against the engine's exported constants and
// returns the process exit code main hands to os.Exit — the contract the
// Dockerfile consumes: 0 declared-compatible, 1 floor violated (fail the
// build), 2 usage error (revisions the manifest could not supply).
//
// The revisions are validated here rather than left to the engine's comparator so
// a missing extraction is reported as the usage error it is (exit 2, "fix the
// gate") instead of a compatibility verdict (exit 1, "bump a pin").
func run(clientRev, clientMinServer int, stdout, stderr io.Writer) int {
	if clientRev <= 0 || clientMinServer <= 0 {
		fmt.Fprintln(stderr, usageErrMsg)
		return 2
	}
	if reason := terminal.WirePairIncompatibility(
		terminal.WireProtocolVersion, terminal.MinSupportedClientWireVersion,
		clientRev, clientMinServer,
	); reason != "" {
		fmt.Fprintf(stderr, "ERROR wire-floor-mismatch: %s\n%s\n", reason, remediation())
		return 1
	}
	fmt.Fprintf(stdout, "wirecheck ok: server wire rev %d (min client %d) <-> client wire rev %d (min server %d)\n",
		terminal.WireProtocolVersion, terminal.MinSupportedClientWireVersion, clientRev, clientMinServer)
	return 0
}

// remediation names this repo's two engine pins. Which pin to move is build-
// layout knowledge the engine deliberately does not carry, so the app supplies
// it alongside the engine's reason.
func remediation() string {
	return "fix: bump go.mod's web-terminal-engine (Go half) or the Dockerfile's CPLIEGER_WEB_TERMINAL_ENGINE_VERSION ARG (TS half) so both halves resolve to a compatible pair"
}
