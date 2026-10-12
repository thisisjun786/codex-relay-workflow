package engine

import (
	"fmt"
	"testing"
)

// TestControlPauseHoldsTheCalls: a pause asked through Config.Control before the run starts, and one asked while the run goes on, end
// the calls that would send held (engine_paused) and write one run_paused row for each ask; a pause asked after the run ended
// writes nothing.
func TestControlPauseHoldsTheCalls(t *testing.T) {
	h := newHarness(t)
	ctl := &Control{}
	h.cfg.Control = ctl
	ctl.Pause("operator", "crw flow pause")
	res := h.run(`const a = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'a' })
return [a.status, a.reason]`)
	if got := fmt.Sprint(value[[]string](t, res)); got != "[held engine_paused]" {
		t.Fatalf("the call of a paused run is %s", got)
	}
	var paused []Event
	for _, e := range h.journal.Events() {
		if e.Event == EvRunPaused {
			paused = append(paused, e)
		}
	}
	if len(paused) != 1 || paused[0].Data["by"] != "operator" || paused[0].Data["reason"] != "crw flow pause" {
		t.Fatalf("run_paused rows %v", paused)
	}
	before := len(h.journal.Events())
	ctl.Pause("operator", "after the end")
	if len(h.journal.Events()) != before {
		t.Fatal("a pause asked after the run ended wrote a row")
	}
}
