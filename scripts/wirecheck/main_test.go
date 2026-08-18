package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/web-terminal-engine/v5/terminal"
)

// TestRun_delegatesToTheEngineRule pins that the gate's verdict IS the
// engine's verdict, in both directions and at the exclusive boundary. The rule
// itself (and its agreement with the runtime close-4002 floor) is pinned in
// the engine; what matters here is that run() routes that answer without
// inverting or swallowing it.
func TestRun_delegatesToTheEngineRule(t *testing.T) {
	cases := map[string]struct {
		clientRev, clientMinServer int
		wantCompatible             bool
	}{
		"current pairing":                    {terminal.WireProtocolVersion, terminal.MinSupportedClientWireVersion, true},
		"client exactly at the server floor": {terminal.MinSupportedClientWireVersion, terminal.MinSupportedClientWireVersion, true},
		"client below the server floor":      {terminal.MinSupportedClientWireVersion - 1, terminal.MinSupportedClientWireVersion, false},
		"client demands a newer server":      {terminal.WireProtocolVersion, terminal.WireProtocolVersion + 1, false},
		"client demands exactly this server": {terminal.WireProtocolVersion, terminal.WireProtocolVersion, true},
		"future client revision is accepted": {terminal.WireProtocolVersion + 3, terminal.MinSupportedClientWireVersion, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.clientRev, tc.clientMinServer, &stdout, &stderr)
			engineSaysCompatible := terminal.WirePairIncompatibility(
				terminal.WireEnd{Rev: terminal.WireProtocolVersion, MinPeer: terminal.MinSupportedClientWireVersion},
				terminal.WireEnd{Rev: tc.clientRev, MinPeer: tc.clientMinServer},
			) == ""
			if engineSaysCompatible != tc.wantCompatible {
				t.Fatalf("engine verdict for (%d,%d) = compatible:%v, test expected %v; the engine's rule changed and this table is stale",
					tc.clientRev, tc.clientMinServer, engineSaysCompatible, tc.wantCompatible)
			}
			if gateCompatible := code == 0; gateCompatible != engineSaysCompatible {
				t.Errorf("run(%d,%d) exit %d (compatible:%v) but the engine says compatible:%v; the gate does not follow the engine's rule",
					tc.clientRev, tc.clientMinServer, code, gateCompatible, engineSaysCompatible)
			}
		})
	}
}

// TestRun_exitCodeContract pins the process exit codes and output streams the
// Dockerfile wire-floor gate branches on: 0 compatible (ok line on stdout),
// 1 floor violated (mismatch on stderr), 2 usage error. A wiring regression
// (inverted check, swapped code, wrong stream) would silently neuter or break
// the image build gate. Client values derive from the engine's constants, so
// the cases track a future floor raise.
func TestRun_exitCodeContract(t *testing.T) {
	cases := []struct {
		name                       string
		clientRev, clientMinServer int
		wantCode                   int
		wantStdout, wantStderr     string
	}{
		{
			name:      "compatible pairing exits 0 and reports ok on stdout",
			clientRev: terminal.WireProtocolVersion, clientMinServer: terminal.WireProtocolVersion,
			wantCode: 0, wantStdout: "wirecheck ok:",
		},
		{
			name:      "violated floor exits 1 with the mismatch on stderr",
			clientRev: terminal.WireProtocolVersion, clientMinServer: terminal.WireProtocolVersion + 1,
			wantCode: 1, wantStderr: "ERROR wire-floor-mismatch:",
		},
		{
			name:      "zero client-rev is a usage error (exit 2)",
			clientRev: 0, clientMinServer: terminal.WireProtocolVersion,
			wantCode: 2, wantStderr: "wire-floor-gate-usage",
		},
		{
			name:      "negative client-min-server is a usage error (exit 2)",
			clientRev: terminal.WireProtocolVersion, clientMinServer: -1,
			wantCode: 2, wantStderr: "wire-floor-gate-usage",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tc.clientRev, tc.clientMinServer, &stdout, &stderr); got != tc.wantCode {
				t.Errorf("run(%d, %d) = %d, want exit code %d (the Dockerfile gate branches on it)",
					tc.clientRev, tc.clientMinServer, got, tc.wantCode)
			}
			if tc.wantStdout == "" {
				if stdout.Len() != 0 {
					t.Errorf("stdout = %q, want empty on a non-zero exit", stdout.String())
				}
			} else if !strings.Contains(stdout.String(), tc.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tc.wantStdout)
			}
			if tc.wantStderr == "" {
				if stderr.Len() != 0 {
					t.Errorf("stderr = %q, want empty on success", stderr.String())
				}
			} else if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

// TestRun_failureNamesBothPins keeps the remediation actionable: the engine's
// reason says WHICH half is behind, and this repo adds which pin to move.
func TestRun_failureNamesBothPins(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(terminal.WireProtocolVersion, terminal.WireProtocolVersion+1, &stdout, &stderr); code != 1 {
		t.Fatalf("run() = %d, want exit 1 for a violated floor", code)
	}
	out := stderr.String()
	for _, want := range []string{"go.mod", "CPLIEGER_WEB_TERMINAL_ENGINE_VERSION"} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr = %q, want it to name the %q pin", out, want)
		}
	}
}

// The gate's verdict is worthless if nothing runs it. run() is exercised above in
// process, and that suite stays green if the Dockerfile step is deleted, commented out,
// or reverted to `go run` — a one-word edit that collapses this program's two failure
// modes into one, because `go run` reports its OWN status 1 for any non-zero program
// exit, turning exit 2 ("the extraction is broken, do NOT bump a pin") into exit 1
// ("genuine wire incompatibility"). An incompatible pair then ships, answers /healthz
// healthy, and refuses every session at first connect with close 4002.
//
// So the tests below read the shipped Dockerfile as text and pin the invocation itself.
// They are matchers over shell text, which is easy to get subtly wrong, so each matcher
// has its own unit test underneath.

// dockerfileUnderTest returns the shipped Dockerfile's logical lines.
func dockerfileUnderTest(t *testing.T) []string {
	t.Helper()
	// Two levels up from scripts/wirecheck/, which is where `go test ./...` runs it.
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	return dockerfileLogicalLines(string(data))
}

// dockerfileLogicalLines folds backslash-continued physical lines into one logical line
// and drops comment and blank lines, so a matcher sees a RUN's whole shell pipeline as a
// single string. Without the fold, a gate split across continuations reads as several
// unrelated lines and every matcher below returns false on correct input.
func dockerfileLogicalLines(text string) []string {
	var out []string
	var b strings.Builder
	for raw := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(raw)
		if b.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if cut, ok := strings.CutSuffix(trimmed, `\`); ok {
			b.WriteString(cut)
			b.WriteString(" ")
			continue
		}
		b.WriteString(trimmed)
		out = append(out, b.String())
		b.Reset()
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// gateBuildOutput returns the -o path a logical line builds the gate to, or "".
func gateBuildOutput(line string) string {
	fields := strings.Fields(line)
	for i, f := range fields {
		if f != "-o" || i+2 >= len(fields) {
			continue
		}
		// The package argument must follow the output path for this to be OUR build.
		for _, later := range fields[i+2:] {
			if strings.HasSuffix(later, "/scripts/wirecheck") || later == "./scripts/wirecheck" {
				return fields[i+1]
			}
		}
	}
	return ""
}

// lineInvokesTheGate reports whether line builds the gate and then RUNS the built binary
// with a -manifest argument.
//
// The window matters. Everything before the `go build` may contain shell operators — this
// repo's step legitimately opens with a `test -f "$WIRE_MANIFEST" || { …; exit 2; }`
// pre-flight carrying `||`, `;` and `>&2` — so operators are only disqualifying AFTER the
// build, where they could divert the invocation. Rejecting `||` anywhere would return
// false on the real, correct Dockerfile.
func lineInvokesTheGate(line string) bool {
	out := gateBuildOutput(line)
	if out == "" {
		return false
	}
	idx := strings.Index(line, "-o "+out)
	if idx < 0 {
		return false
	}
	// Skip past the build's own `-o <out>` argument: the first occurrence of out is the
	// build target, not a call, so searching from idx would match the build itself.
	tail := line[idx+len("-o "+out):]
	runIdx := strings.Index(tail, out)
	if runIdx < 0 {
		// The binary is never invoked, only produced.
		return false
	}
	invocation := tail[runIdx:]
	// After the invocation begins, a shell operator could redirect or replace it.
	for _, op := range []string{"||", "&&", ";", "|", "&"} {
		if strings.Contains(invocation, op) {
			return false
		}
	}
	return strings.Contains(invocation, "-manifest ")
}

// lineRunsTheGateUnbuilt reports whether line reaches the gate through `go run`, which
// discards the exit code the Dockerfile branches on.
func lineRunsTheGateUnbuilt(line string) bool {
	return strings.Contains(line, "go run") &&
		(strings.Contains(line, "./scripts/wirecheck") || strings.Contains(line, "/scripts/wirecheck "))
}

// TestDockerfileInvokesTheGate pins that the shipped Dockerfile builds this program and
// then RUNS it with a manifest. A stage restructure that drops or comments out the RUN
// leaves this package compiling and every other test here green.
func TestDockerfileInvokesTheGate(t *testing.T) {
	t.Parallel()
	lines := dockerfileUnderTest(t)
	invocations := 0
	for _, line := range lines {
		if lineInvokesTheGate(line) {
			invocations++
		}
	}
	if invocations != 1 {
		t.Errorf("Dockerfile builds-and-runs the wire-floor gate %d times, want exactly 1; without it an incompatible Go/TS pair ships and refuses every session with close 4002 behind a healthy /healthz", invocations)
	}
}

// TestDockerfileBuildsTheGateInsteadOfGoRun pins the BUILT form. `go run` collapses exit
// 2 ("the extraction is broken") into exit 1 ("bump a pin"), which is the opposite
// remedy.
func TestDockerfileBuildsTheGateInsteadOfGoRun(t *testing.T) {
	t.Parallel()
	for i, line := range dockerfileUnderTest(t) {
		if lineRunsTheGateUnbuilt(line) {
			t.Errorf("Dockerfile logical line %d reaches the gate through `go run`, which discards its exit code: %s", i, line)
		}
	}
}

// TestDockerfileLogicalLines_foldsAContinuedChain pins the fold, because every matcher
// above is worthless without it: a gate split across backslash continuations would read
// as several unrelated lines and TestDockerfileInvokesTheGate would count zero.
func TestDockerfileLogicalLines_foldsAContinuedChain(t *testing.T) {
	t.Parallel()
	got := dockerfileLogicalLines("# comment\n\nRUN a && \\\n    b && \\\n    c\nCMD [\"x\"]\n")
	want := []string{"RUN a &&  b &&  c", `CMD ["x"]`}
	if len(got) != len(want) {
		t.Fatalf("dockerfileLogicalLines() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("dockerfileLogicalLines()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestLineInvokesTheGate_rejectsInertForms pins the matcher against the forms that look
// like an invocation and are not, so TestDockerfileInvokesTheGate cannot pass on a
// Dockerfile that never runs the gate. The first row is this repo's REAL step shape,
// pre-flight `||` included: a matcher that rejects operators anywhere fails on it.
func TestLineInvokesTheGate_rejectsInertForms(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"the real step, pre-flight included": {
			line: `RUN WIRE_MANIFEST=node_modules/@cplieger/web-terminal-engine/wire-compatibility.json && test -f "$WIRE_MANIFEST" || { echo "missing" >&2; exit 2; } && go build -o /tmp/wirecheck-bin/wirecheck ./scripts/wirecheck && /tmp/wirecheck-bin/wirecheck -manifest "$WIRE_MANIFEST"`,
			want: true,
		},
		"built but never run":       {line: `RUN go build -o /tmp/wc ./scripts/wirecheck`, want: false},
		"run without a manifest":    {line: `RUN go build -o /tmp/wc ./scripts/wirecheck && /tmp/wc`, want: false},
		"verdict discarded by true": {line: `RUN go build -o /tmp/wc ./scripts/wirecheck && /tmp/wc -manifest m.json || true`, want: false},
		"verdict swallowed by pipe": {line: `RUN go build -o /tmp/wc ./scripts/wirecheck && /tmp/wc -manifest m.json | tee log`, want: false},
		"backgrounded":              {line: `RUN go build -o /tmp/wc ./scripts/wirecheck && /tmp/wc -manifest m.json &`, want: false},
		"a different package":       {line: `RUN go build -o /tmp/other ./scripts/other && /tmp/other -manifest m.json`, want: false},
		"unrelated line":            {line: `RUN apt-get update`, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := lineInvokesTheGate(tc.line); got != tc.want {
				t.Errorf("lineInvokesTheGate(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestLineRunsTheGateUnbuilt pins the `go run` recognizer in both directions.
func TestLineRunsTheGateUnbuilt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"go run the gate":      {line: `RUN go run ./scripts/wirecheck -manifest m.json`, want: true},
		"built form":           {line: `RUN go build -o /tmp/wc ./scripts/wirecheck && /tmp/wc -manifest m.json`, want: false},
		"go run something els": {line: `RUN go run ./cmd/other`, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := lineRunsTheGateUnbuilt(tc.line); got != tc.want {
				t.Errorf("lineRunsTheGateUnbuilt(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestReadManifest pins every shape the engine's decoder can hand back, because each was
// previously a shell `sed` capture whose only guard was `${VAR:?}`, in a RUN no test
// could reach. The POLICY under test is this file's: every failure is exit 2, never a
// compatibility verdict, and stderr says "fix the gate, do not bump a pin".
func TestReadManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return p
	}

	good := write("good.json", `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":4,"minimumServerProtocolVersion":3,"incompatibleCloseCode":4002}}`)
	t.Run("the published shape", func(t *testing.T) {
		t.Parallel()
		var stderr bytes.Buffer
		rev, minServer, ok := readManifest(good, &stderr)
		if !ok {
			t.Fatalf("readManifest(good) ok = false, want true (stderr: %s)", stderr.String())
		}
		if rev != 4 || minServer != 3 {
			t.Errorf("readManifest(good) = (%d, %d), want (4, 3)", rev, minServer)
		}
		if stderr.Len() != 0 {
			t.Errorf("readManifest(good) wrote to stderr: %q", stderr.String())
		}
	})

	for name, path := range map[string]string{
		"no usable revisions": write("zero.json", `{"schemaVersion":1,"wireCompatibility":{"protocolVersion":0,"minimumServerProtocolVersion":0,"incompatibleCloseCode":4002}}`),
		"not JSON":            write("garbage.json", "not json at all"),
		"absent":              filepath.Join(dir, "does-not-exist.json"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stderr bytes.Buffer
			if _, _, ok := readManifest(path, &stderr); ok {
				t.Fatalf("readManifest(%s) ok = true, want false", name)
			}
			if !strings.Contains(stderr.String(), "fix the gate, do not bump a pin") {
				t.Errorf("readManifest(%s) stderr = %q, want it to say the gate is at fault rather than a pin", name, stderr.String())
			}
			if !strings.Contains(stderr.String(), "wire-floor-gate-usage") {
				t.Errorf("readManifest(%s) stderr = %q, want the greppable wire-floor-gate-usage marker", name, stderr.String())
			}
		})
	}
}

// gateEnvVar re-enters the test binary as the gate, so the PROCESS exit code is
// observable. main() collapsing a 2 into a 1 (an os.Exit(0), a != 0 -> 1 normalisation,
// a swallowed error) is invisible to every in-process test above, and 2-vs-1 is exactly
// the distinction the Dockerfile step branches on.
const gateEnvVar = "WIRECHECK_TEST_RUN_AS_GATE"

func TestMain(m *testing.M) {
	if os.Getenv(gateEnvVar) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// runGate invokes the test binary as the gate and returns its exit code and stderr.
func runGate(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...)
	cmd.Env = append(os.Environ(), gateEnvVar+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), stderr.String()
	}
	t.Fatalf("running the gate: %v", err)
	return -1, ""
}

// TestGateProcessExitCodes pins the contract the Dockerfile consumes at the PROCESS
// level: 0 compatible, 1 floor violated, 2 the gate is broken.
func TestGateProcessExitCodes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return p
	}
	// Derived from the engine's own constants so neither case can rot when a revision
	// moves. A client speaking this server's revision and demanding no more than it is
	// the compatible pairing.
	compatible := write("ok.json", fmt.Sprintf(
		`{"schemaVersion":1,"wireCompatibility":{"protocolVersion":%d,"minimumServerProtocolVersion":%d,"incompatibleCloseCode":4002}}`,
		terminal.WireProtocolVersion, terminal.WireProtocolVersion))
	// A client demanding a server NEWER than this one violates the declared floor.
	violating := write("bad.json", fmt.Sprintf(
		`{"schemaVersion":1,"wireCompatibility":{"protocolVersion":%d,"minimumServerProtocolVersion":%d,"incompatibleCloseCode":4002}}`,
		terminal.WireProtocolVersion+1, terminal.WireProtocolVersion+1))

	for name, tc := range map[string]struct {
		args     []string
		wantCode int
		wantErr  string
	}{
		"compatible manifest":  {args: []string{"-manifest", compatible}, wantCode: 0},
		"floor violated":       {args: []string{"-manifest", violating}, wantCode: 1, wantErr: "wire-floor-mismatch"},
		"manifest absent":      {args: []string{"-manifest", filepath.Join(dir, "nope.json")}, wantCode: 2, wantErr: "wire-floor-gate-usage"},
		"no manifest flag":     {args: nil, wantCode: 2, wantErr: "wire-floor-gate-usage"},
		"unknown flag":         {args: []string{"-nope"}, wantCode: 2, wantErr: "wire-floor-gate-usage"},
		"help is not a defect": {args: []string{"-h"}, wantCode: 0},
	} {
		t.Run(name, func(t *testing.T) {
			code, stderr := runGate(t, tc.args...)
			if code != tc.wantCode {
				t.Errorf("gate %v exited %d, want %d (stderr: %s)", tc.args, code, tc.wantCode, stderr)
			}
			if tc.wantErr != "" && !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("gate %v stderr = %q, want it to contain %q", tc.args, stderr, tc.wantErr)
			}
			if tc.wantCode == 0 && strings.Contains(stderr, "wire-floor") {
				t.Errorf("gate %v succeeded but stderr reports a gate failure: %q", tc.args, stderr)
			}
		})
	}
}
