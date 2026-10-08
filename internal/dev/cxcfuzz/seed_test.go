//go:build dev

package cxcfuzz

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An explicit seed 0 is a seed like any other: the campaign runs it, writes 0 to the summary, and two runs of
// it agree. Only an omitted seed comes from the clock (CRW-978 c6, CRW-972 item 3).
func TestCampaignRunsAnExplicitSeedZero(t *testing.T) {
	requireNode(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	var runs []Summary
	for i := 0; i < 2; i++ {
		out := t.TempDir()
		summary, err := Campaign(Config{Target: echoTarget(), Cases: 5, Seed: 0, SeedSet: true, Workers: 1, Out: out, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if summary.Seed != 0 {
			t.Fatalf("summary seed %d, want the explicit 0", summary.Seed)
		}
		raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
		if err != nil {
			t.Fatal(err)
		}
		var written Summary
		if err := json.Unmarshal(raw, &written); err != nil || written.Seed != 0 {
			t.Fatalf("summary.json records seed %d (%v), want 0", written.Seed, err)
		}
		runs = append(runs, summary)
	}
	if runs[0].Same != runs[1].Same || runs[0].Differ != runs[1].Differ || runs[0].Cases != runs[1].Cases {
		t.Fatalf("two runs of seed 0 disagree: %+v and %+v", runs[0], runs[1])
	}
}

// Control: an omitted seed still comes from the clock, and the summary records the seed it used.
func TestCampaignOmittedSeedStillComesFromTheClock(t *testing.T) {
	requireNode(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	summary, err := Campaign(Config{Target: echoTarget(), Cases: 2, Workers: 1, Out: t.TempDir(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if want := now.UnixNano(); summary.Seed != want {
		t.Fatalf("summary seed %d, want the clock seed %d", summary.Seed, want)
	}
}

// The command line's --seed 0 reaches the campaign as an explicit seed.
func TestRunSeedFlagZeroIsExplicit(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"echo", "--cases", "3", "--seed", "0", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil || written.Seed != 0 {
		t.Fatalf("summary.json records seed %d (%v), want 0", written.Seed, err)
	}
}
