//go:build dev

package cxcfuzz

import (
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The campaign evaluates up to --workers cases at a time (CRW-1066). Before, the workers were a pool of oracle
// processes and the campaign loop asked them one case after another, so one worker did all the work and the other
// three waited.

// concurrencyTarget is a helper-oracle target whose Go side holds each case for hold and counts how many cases are in
// it at once.
func concurrencyTarget(t *testing.T, hold time.Duration, peak *atomic.Int64) Target {
	t.Helper()
	target := helperTarget(t, func(rng *rand.Rand, size int) any {
		return pyjson.Object{{Key: "text", Value: fmt.Sprintf("v%d", rng.Intn(1000))}}
	})
	var inFlight atomic.Int64
	target.Go = func(input any, env Env) (any, error) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(hold)
		return input, nil
	}
	return target
}

func TestCampaignEvaluatesUpToTheWorkerCountCasesAtOnce(t *testing.T) {
	for _, row := range []struct {
		workers  int
		min, max int64
	}{{1, 1, 1}, {4, 3, 4}} {
		var peak atomic.Int64
		target := concurrencyTarget(t, 200*time.Millisecond, &peak)
		summary, err := Campaign(Config{Target: target, Cases: 16, Seed: 3, SeedSet: true, Workers: row.workers, Out: t.TempDir(), Env: helperEnvFor()})
		if err != nil {
			t.Fatal(err)
		}
		if summary.Cases != 16 || summary.Same != 16 {
			t.Fatalf("workers %d: summary %+v", row.workers, summary)
		}
		if got := peak.Load(); got < row.min || got > row.max {
			t.Errorf("workers %d: %d cases were in the Go side at once, want %d to %d", row.workers, got, row.min, row.max)
		}
	}
}

// outTree is every file below out, relative path to bytes.
func outTree(t *testing.T, out string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(out, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(out, path)
		tree[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// A campaign gives the same counts, divergence files and failure records at four workers as at one, for a fixed seed:
// the inputs are drawn in order, each case is judged alone, and the results are applied in case order. The target here
// diverges on some inputs (shrunk into a divergence file), stalls on one (a timeout record) and refuses none.
func TestCampaignGivesTheSameResultAtOneWorkerAndFour(t *testing.T) {
	run := func(workers int) (Summary, map[string]string) {
		count := 0
		target := helperTarget(t, func(rng *rand.Rand, size int) any {
			count++
			text := fmt.Sprintf("v%d", rng.Intn(60))
			if count == 11 {
				text = "STALL"
			}
			return pyjson.Object{{Key: "text", Value: text}, {Key: "n", Value: count % 7}}
		})
		target.Go = func(input any, env Env) (any, error) {
			if text, _ := field(input, "text"); strings.HasSuffix(fmt.Sprint(text), "7") {
				return pyjson.Object{{Key: "text", Value: "changed"}}, nil
			}
			return input, nil
		}
		out := t.TempDir()
		summary, err := Campaign(Config{Target: target, Cases: 60, Seed: 41, SeedSet: true, Workers: workers, Out: out,
			Timeout: 400 * time.Millisecond, Env: helperEnvFor()})
		if err != nil {
			t.Fatal(err)
		}
		return summary, outTree(t, out)
	}
	one, oneTree := run(1)
	four, fourTree := run(4)
	one.Seconds, four.Seconds, one.PerSecond, four.PerSecond = 0, 0, 0, 0
	if fmt.Sprintf("%+v", one) != fmt.Sprintf("%+v", four) {
		t.Fatalf("summary at 1 worker %+v, at 4 workers %+v", one, four)
	}
	if one.Differ == 0 || one.Timeouts != 1 || one.Same == 0 {
		t.Fatalf("the sample is not a mix: %+v", one)
	}
	delete(oneTree, "summary.json")
	delete(fourTree, "summary.json")
	if len(oneTree) != len(fourTree) {
		t.Fatalf("files at 1 worker %d, at 4 workers %d", len(oneTree), len(fourTree))
	}
	for name, text := range oneTree {
		if fourTree[name] != text {
			t.Errorf("%s differs between 1 and 4 workers:\n 1: %.300s\n 4: %.300s", name, text, fourTree[name])
		}
	}
}

// A campaign that ends on an error at four workers returns it, waits for the cases it started, and leaves no worker
// behind: the pool closes and its start-up root is removed.
func TestCampaignEndsOnAnErrorAtFourWorkers(t *testing.T) {
	target := helperTarget(t, func(rng *rand.Rand, size int) any { return pyjson.Object{{Key: "text", Value: "x"}} })
	var calls atomic.Int64
	target.Compare = func(goOut, oracleOut any) Verdict {
		if calls.Add(1) == 5 {
			return Verdict{Kind: "sameish"}
		}
		return Verdict{Kind: Same}
	}
	summary, err := Campaign(Config{Target: target, Cases: 40, Seed: 1, SeedSet: true, Workers: 4, Out: t.TempDir(), Env: helperEnvFor()})
	if err == nil || !strings.Contains(err.Error(), "unknown verdict kind") {
		t.Fatalf("err = %v, summary %+v", err, summary)
	}
	if summary.Cases >= 40 {
		t.Fatalf("the campaign ran all %d cases after the error", summary.Cases)
	}
}
