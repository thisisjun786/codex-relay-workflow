//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A campaign can be stopped between cases and carried on from where it stopped (CRW-1066): a host load gate ends a
// two-hour campaign early, and the rest of the same seed's cases run later. Stop closes the draw of new cases and
// lets the cases already started finish, so what was drawn was judged; Skip draws the first cases of the seed
// without judging them, so a later segment starts at the case after the last one the earlier segment judged.

// resumeTarget draws a mix of agreeing, diverging and stalling cases from the seed. stallAt is the draw (counted from
// one, skipped draws included) whose case stalls; drawn counts the draws.
func resumeTarget(t *testing.T, stallAt int, drawn *atomic.Int64) Target {
	t.Helper()
	target := helperTarget(t, func(rng *rand.Rand, size int) any {
		n := int(drawn.Add(1))
		text := fmt.Sprintf("v%d", rng.Intn(60))
		if n == stallAt {
			text = "STALL"
		}
		return pyjson.Object{{Key: "text", Value: text}, {Key: "n", Value: n % 7}}
	})
	target.Go = func(input any, env Env) (any, error) {
		if text, _ := field(input, "text"); strings.HasSuffix(fmt.Sprint(text), "7") {
			return pyjson.Object{{Key: "text", Value: "changed"}}, nil
		}
		return input, nil
	}
	return target
}

func resumeConfig(target Target, out string, cases, skip int) Config {
	return Config{Target: target, Cases: cases, Skip: skip, Seed: 41, SeedSet: true, Workers: 3, Out: out,
		Timeout: 400 * time.Millisecond, Env: helperEnvFor()}
}

// Two segments of a seed, the second skipping what the first judged, give together the counts, the divergence files
// and the failure records of the whole run, and a failure record carries the case's number in the whole run.
func TestCampaignSegmentsGiveTheWholeRun(t *testing.T) {
	var drawn atomic.Int64
	wholeOut := t.TempDir()
	whole, err := Campaign(resumeConfig(resumeTarget(t, 31, &drawn), wholeOut, 60, 0))
	if err != nil {
		t.Fatal(err)
	}
	drawn.Store(0)
	firstOut, secondOut := t.TempDir(), t.TempDir()
	first, err := Campaign(resumeConfig(resumeTarget(t, 31, &drawn), firstOut, 25, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Campaign(resumeConfig(resumeTarget(t, 31, &drawn), secondOut, 35, 25))
	if err != nil {
		t.Fatal(err)
	}
	if second.Skipped != 25 || first.Skipped != 0 {
		t.Fatalf("skipped: first %d, second %d, want 0 and 25", first.Skipped, second.Skipped)
	}
	if first.Cases+second.Cases != whole.Cases || first.Same+second.Same != whole.Same || first.Differ+second.Differ != whole.Differ ||
		first.Timeouts+second.Timeouts != whole.Timeouts {
		t.Fatalf("first %+v\nsecond %+v\nwhole %+v", first, second, whole)
	}
	if whole.Timeouts != 1 || whole.Differ == 0 || second.Timeouts != 1 || first.Timeouts != 0 {
		t.Fatalf("the sample is not what the test needs: first %+v second %+v whole %+v", first, second, whole)
	}
	merged := outTree(t, firstOut)
	for name, text := range outTree(t, secondOut) {
		if old, ok := merged[name]; ok && old != text && name != "summary.json" {
			t.Errorf("%s differs between the segments", name)
		}
		merged[name] = text
	}
	wholeTree := outTree(t, wholeOut)
	for _, name := range []string{"summary.json"} {
		delete(merged, name)
		delete(wholeTree, name)
	}
	if len(merged) != len(wholeTree) {
		t.Fatalf("files of the segments %d, of the whole run %d", len(merged), len(wholeTree))
	}
	for name, text := range wholeTree {
		if merged[name] != text {
			t.Errorf("%s differs between the whole run and the segments:\n whole: %.300s\n parts: %.300s", name, text, merged[name])
		}
	}
	var record Failure
	for name, text := range outTree(t, secondOut) {
		if strings.HasPrefix(name, "failures/") {
			if err := json.Unmarshal([]byte(text), &record); err != nil {
				t.Fatal(err)
			}
		}
	}
	if record.Case != 31 {
		t.Errorf("the failure record of the second segment holds case %d, want 31 (its number in the whole run)", record.Case)
	}
}

// Closing Stop ends the draw: the cases already drawn are judged and counted, the summary says the campaign was
// stopped and is written, and a second segment skipping the judged cases finishes the run.
func TestCampaignStopLetsTheDrawnCasesFinishAndAResumeFinishesTheRun(t *testing.T) {
	var drawn atomic.Int64
	stop := make(chan struct{})
	target := resumeTarget(t, 0, &drawn)
	inner := target.Go
	var calls atomic.Int64
	target.Go = func(input any, env Env) (any, error) {
		if calls.Add(1) == 8 {
			close(stop)
		}
		return inner(input, env)
	}
	out := t.TempDir()
	cfg := resumeConfig(target, out, 60, 0)
	cfg.Stop = stop
	first, err := Campaign(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Stopped || first.Cases >= 60 || int64(first.Cases) != drawn.Load() {
		t.Fatalf("stopped campaign: %+v, drawn %d", first, drawn.Load())
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil || !written.Stopped || written.Cases != first.Cases {
		t.Fatalf("summary.json %s (%v)", raw, err)
	}
	drawn.Store(0)
	rest, err := Campaign(resumeConfig(resumeTarget(t, 0, &drawn), t.TempDir(), 60-first.Cases, first.Cases))
	if err != nil {
		t.Fatal(err)
	}
	if rest.Stopped || first.Cases+rest.Cases != 60 || rest.Skipped != first.Cases {
		t.Fatalf("first %+v rest %+v", first, rest)
	}
}

// --skip reaches the campaign and summary.json records it.
func TestRunPassesSkipToTheCampaign(t *testing.T) {
	requireNode(t)
	out := t.TempDir()
	var stdout, stderr strings.Builder
	if code := Run([]string{"echo", "--cases", "5", "--skip", "3", "--seed", "7", "--out", out}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	raw, err := os.ReadFile(filepath.Join(out, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written Summary
	if err := json.Unmarshal(raw, &written); err != nil || written.Skipped != 3 || written.Cases != 5 {
		t.Fatalf("summary.json %s (%v)", raw, err)
	}
}
