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
		reply, _ := json.Marshal(map[string]any{"id": request.ID, "output": request.Input})
		out.Write(append(reply, '\n'))
		if err := out.Flush(); err != nil {
			return 1
		}
	}
	return 0
}

// helperEnvFor is the environment that turns a re-executed test binary into the fake worker.
func helperEnvFor() []string { return append(os.Environ(), helperEnv+"=1") }

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

// A shim-side mutation of one input gives one divergence, and one divergence file holding the
// input as it was generated.
func TestShimMutationGivesOneDivergenceHoldingTheGeneratedInput(t *testing.T) {
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
	if d.Input != `{"mutate": true, "text": "hello"}` {
		t.Fatalf("the divergence holds %s, want the input as generated", d.Input)
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
	if summary.Timeouts != 1 || summary.Same != 3 {
		t.Fatalf("summary %+v", summary)
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
