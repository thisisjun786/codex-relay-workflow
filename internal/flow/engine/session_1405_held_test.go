package engine

import (
	"fmt"
	"sync"
	"testing"
)

// CRW-1405: a held call goes on with its one repair used only when its rows hold the repair step.

// A call held at the gate of its second turn - its task turn's answer failed validation and the run's budget ran out before the
// repair step was published - has published no repair. The resumed call takes the task again on a new subagent, so it goes on with
// no repair used: a first answer that passes completes it with repair_used false, and one that fails gets the call's one repair
// (repair_used true), as the first process would have.
func TestSessionHeldBeforeTheRepairStepHasNoRepairUsed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fails makes the resumed subagent's first answer fail validation too
		fails      bool
		repairUsed bool
	}{
		{name: "the task passes"},
		{name: "the task fails again", fails: true, repairUsed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newSessionKit(t)
			var mu sync.Mutex
			phase := 1
			k.host.tokens["/rollout/agent-1"] = Tokens{Input: 10, Output: 5, Total: 15}
			k.h.cfg.TokenBudget = 15
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case phase == 1 && st.K == 1:
					return orchAnswer{text: "this is not JSON"}
				case phase == 1:
					return orchAnswer{fail: "usageLimitExceeded"}
				case tc.fails && st.Action == StepSpawn:
					return orchAnswer{text: "still not JSON"}
				}
				return orchAnswer{edit: "P:" + st.Op}
			}
			got := value[[]any](t, k.h.run(e2aBuild))
			rows := k.rowsOf("exe")
			if fmt.Sprint(got) != "[held budget_exhausted]" {
				t.Fatalf("the first run %v, want held budget_exhausted; rows %s", got, eventsOf(rows))
			}
			for _, r := range rowsNamed(rows, EvStepPublished) {
				if r.Data["repair"] == true {
					t.Fatalf("the first process published the repair step %v; the call is to be held before it", r.Data)
				}
			}
			if held := rows[len(rows)-1]; held.Event != EvHeld || held.Data["repair_used"] != true || rowStr(held.Data, "input_state") != "delivered" {
				t.Fatalf("the held row %s %v, want the delivered call that had decided on its repair", held.Event, held.Data)
			}
			mu.Lock()
			phase = 2
			mu.Unlock()
			k.h.cfg.TokenBudget = 0
			k.h.cfg.Resume = &ResumeOptions{}
			k.h.cfg.SpentBefore = RunSpent(k.h.journal.Events(), k.h.cfg.RunID).Total
			got = value[[]any](t, k.h.run(e2aBuild))
			rows = k.rowsOf("exe")
			if got[0] != "completed" {
				t.Fatalf("the resumed call %v; rows %s", got, eventsOf(rows))
			}
			last := rows[len(rows)-1]
			if last.Event != EvCompleted || last.Data["repair_used"] != tc.repairUsed {
				t.Errorf("the completed row %s repair_used %v, want %v; rows %s", last.Event, last.Data["repair_used"], tc.repairUsed, eventsOf(rows))
			}
			var repairs int
			for _, r := range rowsNamed(rows, EvStepPublished) {
				if r.Data["repair"] == true {
					repairs++
				}
			}
			if want := map[bool]int{false: 0, true: 1}[tc.fails]; repairs != want {
				t.Errorf("%d repair steps, want %d; rows %s", repairs, want, eventsOf(rows))
			}
		})
	}
}
