//go:build dev

package cxcfuzz

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// helperEnv marks a re-execution of this test binary as the fake oracle worker: the same NDJSON
// protocol as testdata/echo/shim.mjs, in Go, so the pool's timeout and restart handling is tested
// with no Node on PATH.
const helperEnv = "CXCFUZZ_TEST_HELPER"

// helperBootDelay is how long the fake worker sleeps before it reads its first request, modelling a
// real shim's interpreter boot and top-level imports. It is an environment variable rather than a
// field because the delay must be in place before the worker reads anything, the way helperEnv is.
const helperBootDelay = "CXCFUZZ_TEST_BOOT_DELAY"

// helperBadHandshake makes the fake worker answer the start-up handshake (a null input) with a reply
// that names another request, so the pool's envelope check on the handshake reply can be driven
// through the pool rather than only through answer.
const helperBadHandshake = "CXCFUZZ_TEST_BAD_HANDSHAKE"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(helperMain())
	}
	testsupport.Main(m, testsupport.TempDirInRoot, func(root string) (func() error, error) {
		return nil, os.Setenv("CRW_HOME", filepath.Join(root, "crw-home"))
	})
}

// helperMain answers one line per request. A request whose input carries STALL never answers, so
// the pool must time it out, kill that worker and start another for the next request.
func helperMain() int {
	// A boot delay is slept before the scan loop, so nothing is read or answered while it runs: the
	// first request sits in the pipe exactly as it would while a shim boots.
	if delay, err := time.ParseDuration(os.Getenv(helperBootDelay)); err == nil && delay > 0 {
		time.Sleep(delay)
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	out := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var request struct {
			ID    int             `json:"id"`
			Input json.RawMessage `json:"input"`
			Root  string          `json:"root"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			return 1
		}
		if strings.Contains(string(request.Input), "STALL") {
			select {}
		}
		id := request.ID
		if os.Getenv(helperBadHandshake) == "1" && string(request.Input) == "null" {
			id++ // a reply for a request the pool never sent
		}
		reply, _ := json.Marshal(map[string]any{"id": id, "output": request.Input})
		out.Write(append(reply, '\n'))
		if err := out.Flush(); err != nil {
			return 1
		}
	}
	return 0
}

// helperEnvFor is the environment that turns a re-executed test binary into the fake worker.
func helperEnvFor() []string { return append(os.Environ(), helperEnv+"=1") }

// helperEnvForBoot is helperEnvFor with a start-up delay before the worker reads anything.
func helperEnvForBoot(delay time.Duration) []string {
	return append(helperEnvFor(), helperBootDelay+"="+delay.String())
}

// helperEnvForBadHandshake is helperEnvFor with a worker that mis-answers the handshake.
func helperEnvForBadHandshake() []string {
	return append(helperEnvFor(), helperBadHandshake+"=1")
}

// helperTarget is a target whose oracle is this test binary, so its campaign needs no Node.
func helperTarget(t *testing.T, generate func(rng *rand.Rand, size int) any) Target {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Target{
		Name:     "helper",
		Generate: generate,
		Go:       func(input any, env Env) (any, error) { return input, nil },
		Oracle:   Oracle{Command: exe},
		Compare:  compareJSON,
	}
}

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not on PATH")
	}
}

// requireOracleModule skips a test that starts one of this issue's shims when the oracle module that
// shim imports at its top level is not present under DefaultOracleRoot. A CI runner has node but no
// extracted CXC oracle tree, so such a shim exits at its top-level import (ERR_MODULE_NOT_FOUND)
// before it reads a request, and a test that started it would report a worker that answered nothing
// rather than the input the host is missing. Each shim that imports an oracle module names it here: the
// state and goalplan shims import the oracle's readers and writers, and the doctor, memorygate,
// shellwrite, spawn and worktreedel shims import theirs (CRW-932 widened the start-up handshake test to
// all of them). The pyjson shim imports none (it drives python3's json.tool through the standard
// library command), so it is never skipped here and keeps running wherever python3 is. The skip
// message names the module file that was looked for.
func requireOracleModule(t *testing.T, target string) {
	t.Helper()
	module := ""
	switch target {
	case "state":
		module = "pabcd-state/dist/state.js"
	case "goalplan":
		module = "pabcd-state/dist/goalplan.js"
	case "doctor":
		module = "cxc-ops/dist/doctor.js"
	case "memorygate":
		module = "pabcd-state/dist/memory-write-gate.js"
	case "shellwrite":
		module = "pabcd-state/dist/shell-write-destinations.js"
	case "spawn":
		module = "subagent-config/dist/spawn-attach-hook.js"
	case "worktreedel":
		module = "pabcd-state/dist/worktree-guard.js"
	}
	if module == "" {
		return
	}
	path := filepath.Join(DefaultOracleRoot, filepath.FromSlash(module))
	if _, err := os.Stat(path); err != nil {
		t.Skipf("the oracle module %s is not present: %v", path, err)
	}
}

// requireOracleCommands skips a test that starts one of this issue's shims when a command that shim's
// worker needs besides its interpreter is not on PATH. The pyjson shim drives python3's json.tool, so a
// host with node but no python3 must skip that target's oracle replay exactly as a host with no node
// skips every target: without this the test reports the worker's start-up failure instead of the
// command the host is missing, which is a red test on a host that is merely short one optional tool
// (CRW-708 generation 5, d3). The skip message names the missing command.
func requireOracleCommands(t *testing.T, target string) {
	t.Helper()
	entry, ok := Lookup(target)
	if !ok {
		return
	}
	for _, required := range entry.Oracle.Requires {
		if _, err := exec.LookPath(required); err != nil {
			t.Skipf("the oracle worker command %q is not on PATH: %v", required, err)
		}
	}
}

// A campaign over the echo target with the committed shim agrees everywhere.
func TestCampaignEchoAgreesWithTheShim(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	summary, err := Campaign(Config{Target: echoTarget(), Cases: 30, Seed: 7, Workers: 2, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Differ != 0 || summary.Timeouts != 0 || summary.Same != 30 {
		t.Fatalf("summary %+v", summary)
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if written.Target != "echo" || written.Cases != 30 || written.Seed != 7 {
		t.Fatalf("summary.json %+v", written)
	}
}

// A shim-side mutation of one input gives one divergence, one divergence file, and a shrunk input
// that still produces the difference.
func TestShimMutationGivesOneDivergenceAndItsShrunkInput(t *testing.T) {
	requireNode(t)
	count := 0
	target := echoTarget()
	target.Generate = func(rng *rand.Rand, size int) any {
		count++
		return pyjson.Object{{Key: "mutate", Value: count == 3}, {Key: "text", Value: "hello"}}
	}
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 6, Seed: 5, Workers: 1, Out: out,
		Env: append(os.Environ(), "CXCFUZZ_MUTATE=1")})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Differ != 1 {
		t.Fatalf("differ = %d, want 1: %+v", summary.Differ, summary)
	}
	files, err := filepath.Glob(filepath.Join(out, "divergences", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("divergence files %v, want one", files)
	}
	var d Divergence
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Kind != Differ {
		t.Fatalf("kind %q", d.Kind)
	}
	shrunk, err := decode(d.Input)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonical(shrunk); got != `{"mutate": true, "text": ""}` {
		t.Fatalf("shrunk input %s", got)
	}
	if !strings.HasPrefix(filepath.Base(files[0]), string(Differ)+"-") {
		t.Fatalf("divergence file %s is not named by kind and input hash", filepath.Base(files[0]))
	}
}

// A stalled request is a timeout case and the campaign carries on with the next input.
func TestTimeoutIsRecordedAndTheNextRequestProceeds(t *testing.T) {
	count := 0
	target := helperTarget(t, func(rng *rand.Rand, size int) any {
		count++
		if count == 2 {
			return pyjson.Object{{Key: "text", Value: "STALL"}}
		}
		return pyjson.Object{{Key: "text", Value: "ok"}}
	})
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 4, Seed: 3, Workers: 1, Out: out,
		Timeout: 300 * time.Millisecond, Env: helperEnvFor()})
	if err != nil {
		t.Fatal(err)
	}
	// Same 3 of 4 cases with one worker proves the stalled worker was replaced: the two cases after
	// the stall can only have been answered by the worker started in its place.
	if summary.Timeouts != 1 || summary.Same != 3 || summary.Cases != 4 {
		t.Fatalf("summary %+v", summary)
	}
}

// A worker that takes a long time to become ready is not a timeout case: its start-up is charged to
// the startup deadline, not to the first case's deadline. This is the defect the issue reports -- on a
// slow runner the shim's boot exceeded the 5 s per-case deadline and the agreement test counted a
// case as a timeout. Red before the fix: the first case times out at 50 ms.
func TestStartupDelayIsNotChargedToTheCaseDeadline(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any {
		return pyjson.Object{{Key: "text", Value: "ok"}}
	})
	out := t.TempDir()
	summary, err := Campaign(Config{Target: target, Cases: 2, Seed: 1, Workers: 1, Out: out,
		Timeout: 50 * time.Millisecond, StartupTimeout: 5 * time.Second,
		Env: helperEnvForBoot(750 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Same != 2 || summary.Timeouts != 0 {
		t.Fatalf("summary %+v", summary)
	}
}

// The command reports a real divergence as a failure: a target whose oracle answer differs from the
// port's ends with Differ > 0 and exit 1. The echo shim's CXCFUZZ_MUTATE knob makes the oracle
// append to a "mutate" input, so a fixed seed that generates one gives a deterministic divergence.
func TestRunExitsOneOnADivergence(t *testing.T) {
	requireNode(t)
	t.Setenv("CXCFUZZ_MUTATE", "1")
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"echo", "--cases", "30", "--seed", "7", "--workers", "2", "--out", out}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if written.Differ == 0 {
		t.Fatalf("summary.json %+v, want a divergence", written)
	}
}

// Without node the command stops before fuzzing: exit 2 and one line.
func TestRunWithoutNodeExitsTwo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := Run([]string{"echo", "--seconds", "1", "--out", t.TempDir()}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit %d, stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if got := strings.TrimSpace(stderr.String()); got == "" || strings.Contains(got, "\n") {
		t.Fatalf("stderr %q, want one line", got)
	}
}

// A lone surrogate reaches a target as the three WTF-8 bytes a Go string holds it in, and the
// canonical form writes it back as its escape, so both sides compare equal.
func TestALoneSurrogateReachesTheGoFunctionAsWTF8(t *testing.T) {
	value := "\xed\xa0\x80"
	if got := canonical(value); got != `"\ud800"` {
		t.Fatalf("canonical %q", got)
	}
	back, err := decode(canonical(value))
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := back.(string); !ok || text != value {
		t.Fatalf("decoded %#v, want the WTF-8 bytes", back)
	}
}

// The divergence file is named by the kind and the first 12 hex characters of the input hash.
func TestDivergenceName(t *testing.T) {
	got := divergenceName(Differ, `{"a": 1}`)
	if !strings.HasPrefix(got, "differ-") || !strings.HasSuffix(got, ".json") {
		t.Fatalf("name %q", got)
	}
	if body := strings.TrimSuffix(strings.TrimPrefix(got, "differ-"), ".json"); len(body) != 12 {
		t.Fatalf("name %q does not carry a 12-character hash", got)
	}
	if divergenceName(Differ, `{"a": 1}`) != got {
		t.Fatal("the name is not stable")
	}
	if divergenceName(Differ, `{"a": 2}`) == got {
		t.Fatal("two inputs share a name")
	}
}

// A reply is matched to its request: a mismatched id, or a reply carrying neither an output nor an
// error, is a transport failure rather than an answer, so the worker is replaced instead of the
// campaign comparing something the oracle never said.
func TestAnswerRejectsAnUnmatchedOrEmptyReply(t *testing.T) {
	if _, err := answer(1, `{"id":2,"output":1}`); err == nil {
		t.Fatal("a reply for another request was accepted")
	}
	if _, err := answer(1, `{"id":1}`); err == nil {
		t.Fatal("a reply with neither an output nor an error was accepted")
	}
	got, err := answer(1, `{"id":1,"output":null}`)
	if err != nil || got != "null" {
		t.Fatalf("an explicit null output: %q, %v", got, err)
	}
}

// A case root carries the homes and the temporary directory a target reads, so a target that makes
// its own file under TMPDIR finds a directory that exists.
func TestPrepareRootMakesTheHomesAndTmp(t *testing.T) {
	root := t.TempDir()
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	env := RootEnv(root)
	for _, dir := range []string{env.Home, env.CodexHome, env.CrwHome, env.TmpDir} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s: %v", dir, err)
		}
	}
}

// A campaign refuses an output directory that already holds results, so a second run cannot leave
// its divergences beside another run's summary.
func TestRunRefusesANonEmptyOutputDirectory(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "summary.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"echo", "--cases", "1", "--out", out}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "is not empty") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

// A comparator that answers outside the four kinds is a campaign error, not a silent zero.
func TestCampaignRejectsAnUnknownVerdictKind(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return "x" })
	target.Compare = func(goOut, oracleOut any) Verdict { return Verdict{Kind: "sameish"} }
	_, err := Campaign(Config{Target: target, Cases: 1, Seed: 1, Workers: 1, Out: t.TempDir(), Env: helperEnvFor()})
	if err == nil || !strings.Contains(err.Error(), "unknown verdict kind") {
		t.Fatalf("err = %v", err)
	}
}
