package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// CRW-1409: a stop never binds a step again. A step already bound to a turn keeps that turn: a second running turn that carries
// the same step marker is no turn of the call's rows, so the stop neither binds it (no second turn_started) nor interrupts it.

// startsAtReads is startsAtRead with a send_input turn of the repair step started at each of several of the stop's reads: the
// owner that sent the step's input twice, as a second actor (an owner thread that took the step file again) would.
type startsAtReads struct {
	*killOnInterrupt
	at []int

	mu      sync.Mutex
	armed   bool
	reads   int
	step    map[string]any
	started []string
}

// row is the journal hook of the dying process: it keeps the repair step's row and arms the count once the interrupt is asked for.
func (h *startsAtReads) row(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSendInput:
		h.step = ev.Data
	case ev.Event == EvInterruptRequested:
		h.armed = true
	}
}

func (h *startsAtReads) Turns(ctx context.Context, id string, limit int, full bool) ([]SessionTurn, error) {
	h.mu.Lock()
	if h.armed && ctx.Err() == nil && h.step != nil && rowStr(h.step, "agent_id") == id {
		h.reads++
		for _, at := range h.at {
			if at == h.reads {
				h.started = append(h.started, h.start(id))
			}
		}
	}
	h.mu.Unlock()
	return h.killOnInterrupt.Turns(ctx, id, limit, full)
}

// start begins a turn of the repair step on the subagent, with the message the step sends.
func (h *startsAtReads) start(id string) string {
	f := h.fakeSessionHost
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[id]
	turn := fmt.Sprintf("%s-turn%d", id, len(a.turns)+1)
	a.turns = append(a.turns, &SessionTurn{ID: turn, Status: "inProgress", FirstUserText: "crw-flow-op: " + rowStr(h.step, "marker") + "\n\n" + rowStr(h.step, "input")})
	a.Status = "active"
	return turn
}

// The repair step's turn is bound by the stop's bind read (sessionBindStopped); a second send_input turn with the same marker
// starts before the stop's read of the running turns (the host lists it first, newest first). The stop interrupts the bound turn
// only, and the call's rows bind the repair step once: one turn_started for it, naming the turn the bind read found, over the
// memory and the SQLite journal. The process dies right after the interrupt, and the resumed process holds the call engine_paused
// over those rows and binds nothing more. Before CRW-1409 sessionStopBinds called sessionVerify for the second turn, which bound
// the step again to the newest turn carrying its marker: two more turn_started rows, and the bound turn left running.
func TestSessionStopNeverBindsABoundStepAgain(t *testing.T) {
	for _, dj := range deathJournals(t) {
		t.Run(dj.name, func(t *testing.T) {
			var host *startsAtReads
			ctl := &Control{}
			k := newE2aKitOver(t, func(ev Event) bool {
				host.row(ev)
				if ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSendInput {
					go ctl.Pause("operator", "test")
				}
				return false
			}, dj)
			host = &startsAtReads{killOnInterrupt: &killOnInterrupt{dyingHost: k.cfg.Host.(*dyingHost)}, at: []int{1, 2}}
			k.cfg.Host = host
			k.h.cfg.Control = ctl
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				if st.K == 1 {
					return orchAnswer{text: "this is not JSON"}
				}
				// the owner sends the repair input only when the stop reads the turns (startsAtReads)
				return orchAnswer{skip: true}
			}
			k.die(e2aBuild)
			host.mu.Lock()
			started := strings.Join(host.started, ",")
			host.mu.Unlock()
			rows := k.rowsOf("exe")
			if started != "agent-1-turn2,agent-1-turn3" {
				t.Fatalf("the send_input turns started as %q, want agent-1-turn2 at the bind read and agent-1-turn3 at the stop's read; rows %s", started, eventsOf(rows))
			}
			if got := k.host.interrupted(); strings.Join(got, ",") != "agent-1/agent-1-turn2" {
				t.Errorf("interrupts %v, want the bound repair turn once", got)
			}
			var bound []string
			for _, r := range rowsNamed(rows, EvTurnStarted) {
				bound = append(bound, fmt.Sprintf("%s@k%d", rowStr(r.Data, "turn_id"), rowInt(r.Data, "k")))
			}
			if strings.Join(bound, ",") != "agent-1-turn1@k1,agent-1-turn2@k2" {
				t.Errorf("bound turns %v, want the task turn and the repair turn the bind read found, once each; rows %s", bound, eventsOf(rows))
			}
			res := k.resume(e2aBuild)
			got := value[[]any](t, res)
			rows = k.rowsOf("exe")
			if fmt.Sprint(got) != "[held engine_paused]" {
				t.Fatalf("the resumed call %v, want held engine_paused; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
			}
			if n := len(rowsNamed(rows, EvTurnStarted)); n != 2 {
				t.Errorf("%d turn_started rows after the resume, want the 2 the stop left; rows %s", n, eventsOf(rows))
			}
		})
	}
}
