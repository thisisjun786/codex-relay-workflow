package bound

import (
	"sync"
	"testing"
	"time"
)

// CRW-1372 (4): two near-simultaneous binds of one owner thread for two runs read the thread's last line and append under one file
// lock, so they cannot both bind: the second reads the first's line, finds its run live, and is not bound.
func TestBindOrchestratorTwoNearSimultaneousStartsBindOne(t *testing.T) {
	home(t)
	w := &Writer{}
	// the thread's last line is a run whose process is gone: each bind asks whether it lives, and the two asks meet here if both binds
	// read the line before either writes
	if err := w.Append(Line{RunID: "run-old", ThreadID: threadA, Kind: KindOrchestrator, HelperPairs: sixPairs("old"), PolicyDigest: "sha256:old"}); err != nil {
		t.Fatal(err)
	}
	var arrived sync.WaitGroup
	arrived.Add(2)
	met := make(chan struct{})
	go func() { arrived.Wait(); close(met) }()
	live := func(run string) bool {
		if run != "run-old" {
			return true // the two new runs' processes hold their locks
		}
		arrived.Done()
		select {
		case <-met:
		case <-time.After(time.Second):
		}
		return false
	}
	results := make([]BindResult, 2)
	var done sync.WaitGroup
	for i, run := range []string{"run-a", "run-b"} {
		done.Add(1)
		go func() {
			defer done.Done()
			res, err := BindOrchestrator(OrchestratorBinding{Writer: w, RunID: run, Thread: threadA, Digest: "sha256:" + run, Pairs: sixPairs(run), Live: live})
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}()
	}
	done.Wait()
	bound := 0
	for _, res := range results {
		if res.Bound {
			bound++
		}
	}
	if bound != 1 {
		t.Fatalf("%d of two near-simultaneous starts bound the thread: %+v", bound, results)
	}
}
