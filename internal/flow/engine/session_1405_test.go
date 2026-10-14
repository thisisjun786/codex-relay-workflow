package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// CRW-1405: the e2a follow-ups of the session engine. A stop binds every turn it interrupts, with the place the step recorded, and
// stores the rows before the interrupt is sent, whichever path reached it; a held call goes on with its one repair used only when
// its rows hold the repair step (session_1405_held_test.go).

// startsAtRead is the host of a process whose run asked for the interrupt while the repair step was not bound: from then on the
// reads of the subagent's turns the stop makes (a live context; a find read under the cancelled turn is not one) are counted, and
// the read numbered at starts the send_input turn of the repair step first, as the owner that sends the input at that moment would.
// The process dies right after it interrupted a turn (killOnInterrupt).
type startsAtRead struct {
	*killOnInterrupt
	at int

	mu      sync.Mutex
	armed   bool
	reads   int
	step    map[string]any
	started string
}

// row is the journal hook of the dying process: it keeps the repair step's row and arms the count once the interrupt is asked for.
func (h *startsAtRead) row(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSendInput:
		h.step = ev.Data
	case ev.Event == EvInterruptRequested:
		h.armed = true
	}
}

func (h *startsAtRead) Turns(ctx context.Context, id string, limit int, full bool) ([]SessionTurn, error) {
	h.mu.Lock()
	if h.armed && h.started == "" && ctx.Err() == nil && h.step != nil && rowStr(h.step, "agent_id") == id {
		if h.reads++; h.reads == h.at {
			h.started = h.start(id)
		}
	}
	h.mu.Unlock()
	return h.killOnInterrupt.Turns(ctx, id, limit, full)
}

// start begins the repair step's turn on the subagent, with the message the step sends.
func (h *startsAtRead) start(id string) string {
	f := h.fakeSessionHost
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[id]
	turn := fmt.Sprintf("%s-turn%d", id, len(a.turns)+1)
	a.turns = append(a.turns, &SessionTurn{ID: turn, Status: "inProgress", FirstUserText: "crw-flow-op: " + rowStr(h.step, "marker") + "\n\n" + rowStr(h.step, "input")})
	a.Status = "active"
	return turn
}

// inputUnwritten is the bus of a process that cannot write a send_input step (the owner never gets it from this process).
type inputUnwritten struct {
	*dyingBus
}

func (b *inputUnwritten) Publish(st Step) error {
	if st.Action == StepSendInput {
		return errors.New("permission denied")
	}
	return b.dyingBus.Publish(st)
}

// A send_input turn that starts on the subagent while the run stops - after the read that would have bound it and before the read
// that finds it running - is bound before the stop interrupts it: its turn_started, with the conversation place the step recorded,
// is stored before the interrupt is sent, over the SQLite journal that batches it too. A process that dies right after the
// interrupt leaves the turn in its rows, so the resumed process holds the call engine_paused, the reason the engine interrupted it
// for, never host_interrupted_external. It holds on both ways to the stop: the stop of a published step waiting for its bind (whose
// bind read comes first), and the stop of a step the bus could not write (stepUnwritten, no bind read).
func TestSessionStopBindsEveryTurnItInterrupts(t *testing.T) {
	for _, dj := range deathJournals(t) {
		for _, tc := range []struct {
			name string
			// at is the stop's read of the subagent's turns at which the send_input turn starts
			at int
			// unwritten makes the bus refuse the send_input step
			unwritten bool
		}{
			{name: "waiting for the bind", at: 2},
			{name: "step unwritten", at: 1, unwritten: true},
		} {
			t.Run(dj.name+"/"+tc.name, func(t *testing.T) {
				var host *startsAtRead
				pause := EvStepPublished
				if tc.unwritten {
					pause = EvStepUnwritten
				}
				ctl := &Control{}
				k := newE2aKitOver(t, func(ev Event) bool {
					host.row(ev)
					if ev.Event == pause && (pause != EvStepPublished || rowStr(ev.Data, "action") == StepSendInput) {
						go ctl.Pause("operator", "test")
					}
					return false
				}, dj)
				host = &startsAtRead{killOnInterrupt: &killOnInterrupt{dyingHost: k.cfg.Host.(*dyingHost)}, at: tc.at}
				k.cfg.Host = host
				if tc.unwritten {
					k.cfg.Bus = &inputUnwritten{dyingBus: k.cfg.Bus.(*dyingBus)}
				}
				k.h.cfg.Control = ctl
				k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
					if st.K == 1 {
						return orchAnswer{text: "this is not JSON"}
					}
					// the owner sends the repair input only when the stop reads the turns (startsAtRead)
					return orchAnswer{skip: true}
				}
				k.die(e2aBuild)
				host.mu.Lock()
				started := host.started
				host.mu.Unlock()
				if started != "agent-1-turn2" {
					t.Fatalf("the send_input turn started as %q, want agent-1-turn2 during the stop; rows %s", started, eventsOf(k.rowsOf("exe")))
				}
				if got := k.host.interrupted(); strings.Join(got, ",") != "agent-1/agent-1-turn2" {
					t.Errorf("interrupts %v, want the send_input turn once", got)
				}
				res := k.resume(e2aBuild)
				got := value[[]any](t, res)
				rows := k.rowsOf("exe")
				if fmt.Sprint(got) != "[held engine_paused]" {
					t.Fatalf("the resumed call %v, want held engine_paused; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
				}
				var bound []string
				for _, r := range rowsNamed(rows, EvTurnStarted) {
					bound = append(bound, fmt.Sprintf("%s@%d", rowStr(r.Data, "turn_id"), rowInt(r.Data, "conversation_turn")))
					if r.Data["replaces_turn"] != nil {
						t.Errorf("turn_started %v replaces a turn; the call's conversation had no turn 2", r.Data)
					}
				}
				if strings.Join(bound, ",") != "agent-1-turn1@1,agent-1-turn2@2" {
					t.Errorf("bound turns %v, want the task turn at 1 and the repair turn the stop interrupted at 2, once each; rows %s", bound, eventsOf(rows))
				}
			})
		}
	}
}
