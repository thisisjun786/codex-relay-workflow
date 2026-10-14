//go:build dev

package cxcfuzz

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A reused json.tool process that ends while the shim is idle is a dead worker for the campaign the first time the
// shim sees it dead, whatever the next request is (CRW-1066 c5). The shim answers some requests without asking a
// reused process (a remembered answer, a document sent to a fresh process) and asks another process for others (a
// different option set, which may also evict the dead one), and none of those may let the death pass.

// killAndWait ends the given json.tool processes with SIGKILL and waits until they are gone and the shim has had time
// to read the news, which it reads from the process's pipes.
func (r *pyjsonRig) killAndWait(pids ...int) {
	r.t.Helper()
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			r.t.Fatalf("kill %d: %v", pid, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			alive = alive || jsonTool(pid)
		}
		if !alive {
			time.Sleep(300 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("the json.tool processes %v are still running after SIGKILL", pids)
}

func TestPyjsonShimReportsAReusedProcessThatDiedWhileIdleWhateverTheNextRequestIs(t *testing.T) {
	first := pyjsonRequest{Text: `{"a": 1}`, Indent: 4, EnsureAscii: true}
	// otherSets are option sets that fill the shim's open processes up to its limit of eight, the first one's included.
	var otherSets []pyjsonRequest
	for _, indent := range []int{1, 2, 3, 5, 6, 7, 8} {
		otherSets = append(otherSets, pyjsonRequest{Text: `{"b": 2}`, Indent: indent, EnsureAscii: true})
	}
	for _, row := range []struct {
		name string
		// others are asked after the first request, each under an option set of its own.
		others []pyjsonRequest
		// next is the request after the death.
		next pyjsonRequest
	}{
		{name: "same option set, another document", next: pyjsonRequest{Text: `{"a": 2}`, Indent: 4, EnsureAscii: true}},
		{name: "the document the shim remembers", next: first},
		{name: "a document json.tool refuses, asked of a fresh process", next: pyjsonRequest{Text: `{"a": `, Indent: 4, EnsureAscii: true}},
		{name: "another option set", next: pyjsonRequest{Text: `{"a": 3}`, Indent: 2, EnsureAscii: true}},
		{name: "a ninth option set, which makes the shim end its oldest process", others: otherSets, next: pyjsonRequest{Text: `{"a": 4}`, Indent: 9, EnsureAscii: true}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newPyjsonRig(t, 30*time.Second)
			if _, err := rig.call(pyjsonWire(first)); err != nil {
				t.Fatal(err)
			}
			for _, other := range row.others {
				if _, err := rig.call(pyjsonWire(other)); err != nil {
					t.Fatal(err)
				}
			}
			pids := rig.pids()
			if len(pids) == 0 || len(pids) != len(rig.live()) {
				t.Fatalf("started %v, running %v: the shim must hold one reused process per option set", pids, rig.live())
			}
			// The first request's process is the oldest; it is the only one that dies.
			rig.killAndWait(pids[0])
			_, err := rig.call(pyjsonWire(row.next))
			var failure CaseFailure
			if !errors.As(err, &failure) || failure.Cause != CauseDeadWorker {
				t.Fatalf("err = %v, want a dead-worker case failure for the process that ended while idle", err)
			}
			again := row.next
			again.Text = `{"after": true}`
			got, err := rig.call(pyjsonWire(again))
			if err != nil {
				t.Fatalf("the replacement worker did not answer: %v", err)
			}
			want, err := pyjsonReference(rig.real, again)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("answer %+v, want %+v", got, want)
			}
		})
	}
}

// A campaign whose reused process dies while the shim is idle counts exactly the next case as a dead worker, records
// it under its number, replaces the worker and finishes the other cases, whatever path that next case takes in the shim.
func TestCampaignCountsAReusedProcessThatDiedWhileIdleAtTheNextCase(t *testing.T) {
	for _, row := range []struct {
		name string
		// after draws the case that follows the death (its number is n, counted from one).
		after func(n int) pyjson.Object
	}{
		{"a remembered document", func(n int) pyjson.Object { return pyjson.Object{{Key: "text", Value: `{"n": 1}`}} }},
		{"a document for a fresh process", func(n int) pyjson.Object { return pyjson.Object{{Key: "text", Value: `[1,`}} }},
		{"another option set", func(n int) pyjson.Object {
			return pyjson.Object{{Key: "text", Value: fmt.Sprintf(`{"n": %d}`, n)}, {Key: "indent", Value: 2}}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			rig := newPyjsonRig(t, 30*time.Second)
			target := pyjsonTarget()
			var drawn, cases atomic.Int64
			target.Generate = func(rng *rand.Rand, size int) any {
				n := int(drawn.Add(1))
				if n <= 5 {
					return pyjson.Object{{Key: "text", Value: fmt.Sprintf(`{"n": %d}`, n)}}
				}
				return row.after(n)
			}
			goSide := target.Go
			killed := 0
			target.Go = func(input any, env Env) (any, error) {
				if cases.Add(1) == 6 {
					live := rig.live()
					killed = len(live)
					rig.killAndWait(live...)
				}
				return goSide(input, env)
			}
			out := t.TempDir()
			summary, err := Campaign(Config{Target: target, Cases: 12, Seed: 1066, SeedSet: true, Workers: 1, Out: out, Env: rig.env})
			if err != nil {
				t.Fatal(err)
			}
			if killed == 0 {
				t.Fatal("no reused json.tool process was running when the sixth case began")
			}
			if summary.Cases != 12 || summary.DeadWorkers != 1 || summary.Same != 11 || summary.Timeouts != 0 {
				t.Fatalf("summary %+v, want 12 cases, 1 dead worker and 11 agreements", summary)
			}
			records, err := filepath.Glob(filepath.Join(out, "failures", "*.json"))
			if err != nil || len(records) != 1 {
				t.Fatalf("failure records %v (%v), want one", records, err)
			}
			raw, err := os.ReadFile(records[0])
			if err != nil {
				t.Fatal(err)
			}
			var record Failure
			if err := json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			if record.Cause != CauseDeadWorker || record.Case != 6 {
				t.Fatalf("failure record %+v, want the dead-worker record of case 6", record)
			}
		})
	}
}
