//go:build dev

package cxcfuzz

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A campaign starts no case once it has been told to stop (CRW-1066): Config.Stop, the deadline and a case error each
// end the draw, and the campaign takes a worker slot before it checks them and draws the next input, so a case is
// never drawn while every worker is busy and then started after the stop. These tests hold every worker in a case,
// give the start goroutine time to reach the slot it waits for, and then stop the campaign.

// heldTarget is a helper-oracle target whose Go side holds each case until release is closed (the first holdFirst
// cases only when holdFirst is positive), counts the cases that began, and counts the inputs drawn.
type heldTarget struct {
	Target
	began, drawn atomic.Int64
	release      chan struct{}
	once         sync.Once
}

func newHeldTarget(t *testing.T, holdFirst int64) *heldTarget {
	t.Helper()
	held := &heldTarget{release: make(chan struct{})}
	held.Target = helperTarget(t, func(rng *rand.Rand, size int) any {
		return pyjson.Object{{Key: "text", Value: fmt.Sprintf("v%d", held.drawn.Add(1))}}
	})
	held.Target.Go = func(input any, env Env) (any, error) {
		if n := held.began.Add(1); holdFirst <= 0 || n <= holdFirst {
			<-held.release
		}
		return input, nil
	}
	t.Cleanup(held.let)
	return held
}

// let releases the held cases; it is safe to call twice.
func (h *heldTarget) let() { h.once.Do(func() { close(h.release) }) }

// waitBegan waits until n cases have begun on the Go side.
func (h *heldTarget) waitBegan(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for h.began.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d cases began, want %d", h.began.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle gives the start goroutine time to draw ahead and wait for a free worker, which is where the stop has to find it.
func settle() { time.Sleep(300 * time.Millisecond) }

type campaignOutcome struct {
	summary Summary
	err     error
}

func runCampaign(cfg Config) <-chan campaignOutcome {
	done := make(chan campaignOutcome, 1)
	go func() {
		summary, err := Campaign(cfg)
		done <- campaignOutcome{summary, err}
	}()
	return done
}

func waitCampaign(t *testing.T, done <-chan campaignOutcome) campaignOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(30 * time.Second):
		t.Fatal("the campaign did not end")
		return campaignOutcome{}
	}
}

// Stop closed while every worker is in a case: the cases in the workers finish, no other case is drawn or started,
// and the summary says the campaign was stopped even though the next case was the last one it would have run.
func TestCampaignStartsNoCaseAfterStopWhileEveryWorkerIsBusy(t *testing.T) {
	held := newHeldTarget(t, 0)
	stop := make(chan struct{})
	cfg := Config{Target: held.Target, Cases: 3, Seed: 5, SeedSet: true, Workers: 2, Out: t.TempDir(), Stop: stop, Env: helperEnvFor()}
	done := runCampaign(cfg)
	held.waitBegan(t, 2)
	settle()
	close(stop)
	held.let()
	out := waitCampaign(t, done)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.summary.Cases != 2 || !out.summary.Stopped || held.began.Load() != 2 || held.drawn.Load() != 2 {
		t.Fatalf("summary %+v, %d cases began, %d inputs drawn: want 2 cases, stopped, 2 began, 2 drawn", out.summary, held.began.Load(), held.drawn.Load())
	}
}

// The deadline passes while every worker is in a case: the case in the worker finishes, no other case is drawn or started.
func TestCampaignStartsNoCaseAfterTheDeadlineWhileEveryWorkerIsBusy(t *testing.T) {
	held := newHeldTarget(t, 0)
	cfg := Config{Target: held.Target, Seconds: 1, Seed: 5, SeedSet: true, Workers: 1, Out: t.TempDir(), Env: helperEnvFor()}
	done := runCampaign(cfg)
	held.waitBegan(t, 1)
	time.Sleep(1300 * time.Millisecond)
	held.let()
	out := waitCampaign(t, done)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if out.summary.Cases != 1 || held.began.Load() != 1 || held.drawn.Load() != 1 {
		t.Fatalf("summary %+v, %d cases began, %d inputs drawn: want 1 case, 1 began, 1 drawn", out.summary, held.began.Load(), held.drawn.Load())
	}
}

// A campaign that ends on a case error stops its stream: stopping the stream while every worker is in a case lets those
// cases finish and starts and draws nothing more.
func TestCampaignStreamStartsNoCaseAfterItIsStoppedWhileEveryWorkerIsBusy(t *testing.T) {
	held := newHeldTarget(t, 0)
	pool, err := NewPool(held.Target.Oracle, 2, DefaultTimeout, DefaultStartupTimeout, helperEnvFor())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	}()
	run := &campaign{cfg: Config{Target: held.Target, Cases: 3, Workers: 2, Seed: 5}, pool: pool, rng: rand.New(rand.NewSource(5)), seed: 5}
	stream := run.start(time.Time{})
	held.waitBegan(t, 2)
	settle()
	stream.stop()
	held.let()
	started := 0
	for pending := range stream.ordered {
		<-pending.done
		started++
	}
	if started != 2 || held.began.Load() != 2 || held.drawn.Load() != 2 {
		t.Fatalf("%d cases started, %d began, %d inputs drawn: want 2, 2 and 2", started, held.began.Load(), held.drawn.Load())
	}
}

// A campaign stopped while every worker is busy, and carried on with --skip of the cases it judged, gives together the
// counts and the files of the whole run: the inputs drawn are exactly the cases that ran, so the sequence the seed
// draws is the same however the run is cut.
func TestCampaignStoppedWhileBusyAndResumedGivesTheWholeRun(t *testing.T) {
	const total = 40
	build := func(drawn *atomic.Int64, gate func()) Target {
		target := helperTarget(t, func(rng *rand.Rand, size int) any {
			n := int(drawn.Add(1))
			return pyjson.Object{{Key: "text", Value: fmt.Sprintf("v%d", rng.Intn(60))}, {Key: "n", Value: n % 7}}
		})
		target.Go = func(input any, env Env) (any, error) {
			gate()
			if text, _ := field(input, "text"); strings.HasSuffix(fmt.Sprint(text), "7") {
				return pyjson.Object{{Key: "text", Value: "changed"}}, nil
			}
			return input, nil
		}
		return target
	}
	cfgFor := func(target Target, out string, cases, skip int) Config {
		return Config{Target: target, Cases: cases, Skip: skip, Seed: 41, SeedSet: true, Workers: 3, Out: out, Timeout: 400 * time.Millisecond, Env: helperEnvFor()}
	}
	var wholeDrawn atomic.Int64
	wholeOut := t.TempDir()
	whole, err := Campaign(cfgFor(build(&wholeDrawn, func() {}), wholeOut, total, 0))
	if err != nil {
		t.Fatal(err)
	}

	var firstDrawn, calls atomic.Int64
	stop := make(chan struct{})
	firstOut := t.TempDir()
	firstCfg := cfgFor(build(&firstDrawn, func() {
		// Every case holds its worker for a while, so the start goroutine waits for a free one; the stop comes in the middle
		// (a case that differs is shrunk, which calls the Go side again, so a call is not a case).
		if calls.Add(1) == 8 {
			close(stop)
		}
		time.Sleep(40 * time.Millisecond)
	}), firstOut, total, 0)
	firstCfg.Stop = stop
	first, err := Campaign(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Stopped || first.Cases >= total || int64(first.Cases) != firstDrawn.Load() {
		t.Fatalf("stopped campaign %+v, %d inputs drawn", first, firstDrawn.Load())
	}

	var restDrawn atomic.Int64
	restOut := t.TempDir()
	rest, err := Campaign(cfgFor(build(&restDrawn, func() {}), restOut, total-first.Cases, first.Cases))
	if err != nil {
		t.Fatal(err)
	}
	if first.Cases+rest.Cases != whole.Cases || first.Same+rest.Same != whole.Same || first.Differ+rest.Differ != whole.Differ {
		t.Fatalf("first %+v\nrest %+v\nwhole %+v", first, rest, whole)
	}
	if whole.Differ == 0 {
		t.Fatalf("the sample is not what the test needs: %+v", whole)
	}
	merged := outTree(t, firstOut)
	for name, text := range outTree(t, restOut) {
		merged[name] = text
	}
	wholeTree := outTree(t, wholeOut)
	delete(merged, "summary.json")
	delete(wholeTree, "summary.json")
	if len(merged) != len(wholeTree) {
		t.Fatalf("files of the two segments %d, of the whole run %d", len(merged), len(wholeTree))
	}
	for name, text := range wholeTree {
		if merged[name] != text {
			t.Errorf("%s differs between the whole run and the segments", name)
		}
	}
}
