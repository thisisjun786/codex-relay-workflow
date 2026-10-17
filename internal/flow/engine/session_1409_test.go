package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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

// stopWatch is the host of a stop that binds a step's turn (CRW-1412). From the process's interrupt_requested row on, it logs the live
// reads (a context the stop does not cancel) of its subagent: ReadAgent, Turns and the Interrupt calls. The stop's read of the running
// turns is the armed Turns read that directly follows another Turns read: sessionVerify reads ReadAgent and then Turns, and
// sessionStop reads Turns alone. That read starts two turns that carry the step's marker, the older first, and the host lists them
// newest first. pause, when set, is asked for once, at the first live ReadAgent that directly follows a live Turns read: the gone
// read of the adoption (sessionAdopt).
type stopWatch struct {
	SessionHost
	// fake is the subagent host the started turns are added to.
	fake  *fakeSessionHost
	pause func()

	mu     sync.Mutex
	armed  bool
	paused bool
	marker string
	// last is the live call before the current one: "read <agent>" or "turns <agent>".
	last    string
	started []string
	// log is what the armed host saw, in order: a read, the turns of the marker a Turns read listed, the turns a stop started, and an
	// interrupt.
	log []string
}

// row is the journal hook of the process under test: it keeps the marker of the send_input step and arms the log at interrupt_requested.
func (h *stopWatch) row(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSendInput:
		h.marker = rowStr(ev.Data, "marker")
	case ev.Event == EvInterruptRequested:
		h.armed = true
	}
}

func (h *stopWatch) ReadAgent(ctx context.Context, id string) (SessionAgent, error) {
	h.mu.Lock()
	live := ctx.Err() == nil
	ask := live && h.pause != nil && !h.paused && h.last == "turns "+id
	if ask {
		h.paused = true
	}
	if live {
		if h.armed {
			h.log = append(h.log, "read "+id)
		}
		h.last = "read " + id
	}
	h.mu.Unlock()
	if ask {
		go h.pause()
	}
	return h.SessionHost.ReadAgent(ctx, id)
}

func (h *stopWatch) Turns(ctx context.Context, id string, limit int, full bool) ([]SessionTurn, error) {
	h.mu.Lock()
	live := ctx.Err() == nil
	stopRead := live && h.armed && h.started == nil && h.last == "turns "+id
	if live {
		h.last = "turns " + id
	}
	if stopRead {
		h.started = []string{h.startTurn(id), h.startTurn(id)}
		h.log = append(h.log, "start "+strings.Join(h.started, ","))
	}
	h.mu.Unlock()
	turns, err := h.SessionHost.Turns(ctx, id, limit, full)
	if live {
		h.mu.Lock()
		if h.armed {
			h.log = append(h.log, "turns "+id+" "+markedList(turns, h.marker))
		}
		h.mu.Unlock()
	}
	return turns, err
}

func (h *stopWatch) Interrupt(ctx context.Context, id, turn string) error {
	h.mu.Lock()
	if ctx.Err() == nil && h.armed {
		h.log = append(h.log, "interrupt "+id+"/"+turn)
	}
	h.mu.Unlock()
	return h.SessionHost.Interrupt(ctx, id, turn)
}

// startTurn starts a turn of the step on the subagent, as a send of the step's input does, and returns its id.
func (h *stopWatch) startTurn(id string) string {
	f := h.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[id]
	turn := fmt.Sprintf("%s-turn%d", id, len(a.turns)+1)
	a.turns = append(a.turns, &SessionTurn{ID: turn, Status: "inProgress", FirstUserText: "crw-flow-op: " + h.marker + "\n\nthe turn the stop found"})
	a.Status = "active"
	return turn
}

// markedList lists the ids of the turns that carry the marker, newest first as the host lists them, in brackets ("[]" when none does).
func markedList(turns []SessionTurn, marker string) string {
	var ids []string
	for _, t := range turns {
		if first, _, _ := strings.Cut(t.FirstUserText, "\n"); first == "crw-flow-op: "+marker {
			ids = append(ids, t.ID)
		}
	}
	return "[" + strings.Join(ids, ",") + "]"
}

// path is the host log up to the first interrupt.
func (h *stopWatch) path() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.log {
		out = append(out, e)
		if strings.HasPrefix(e, "interrupt ") {
			break
		}
	}
	return out
}

// checkStopPath asserts the path of a stop that binds the step's turn, over the host log up to the interrupt (CRW-1412): the bind read
// (read, then turns) listed no turn of the step, the stop's read started the two turns, exactly one verify read (read, then turns)
// followed it, and the interrupt names the newer of the two, the turn the verify bound.
func checkStopPath(t *testing.T, log []string, older, newer string) {
	t.Helper()
	at := -1
	for i, e := range log {
		if strings.HasPrefix(e, "start ") {
			at = i
			break
		}
	}
	listed := "[" + newer + "," + older + "]"
	want := []string{"start " + older + "," + newer, "turns agent-1 " + listed, "read agent-1", "turns agent-1 " + listed, "interrupt agent-1/" + newer}
	if at < 2 || log[at-2] != "read agent-1" || log[at-1] != "turns agent-1 []" || strings.Join(log[at:], "\n") != strings.Join(want, "\n") {
		t.Errorf("the host log %q does not hold the stop path: the bind read finds no turn of the step, the stop's read starts %s, one verify read follows, and the interrupt names %s; want the tail %q", log, older+","+newer, newer, want)
	}
}

// CRW-1412: a stop whose read holds two running turns of the step binds the step once. The stop's own verify binds the newer of the
// two turns the host lists first; the older turn, which carries the same marker, is neither bound again (no second turn_started) nor
// interrupted. The send_input bind's stop is pinned over the memory and the SQLite journal, and the adoption's stop over the memory
// journal (TestSessionAdoptStopBindsTheAdoptedStepOnce).
func TestSessionStopBindsAStepOnceWhenItsReadHoldsTwoTurnsOfIt(t *testing.T) {
	for _, dj := range deathJournals(t) {
		t.Run(dj.name, func(t *testing.T) {
			var host *stopWatch
			ctl := &Control{}
			k := newE2aKitOver(t, func(ev Event) bool {
				host.row(ev)
				if ev.Event == EvStepPublished && rowStr(ev.Data, "action") == StepSendInput {
					go ctl.Pause("operator", "test")
				}
				return false
			}, dj)
			host = &stopWatch{SessionHost: &killOnInterrupt{dyingHost: k.cfg.Host.(*dyingHost)}, fake: k.host}
			k.cfg.Host = host
			k.h.cfg.Control = ctl
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				if st.K == 1 {
					return orchAnswer{text: "this is not JSON"}
				}
				// the owner sends the repair input only when the stop reads the turns (stopWatch)
				return orchAnswer{skip: true}
			}
			k.die(e2aBuild)
			checkStopPath(t, host.path(), "agent-1-turn2", "agent-1-turn3")
			if got := k.host.interrupted(); strings.Join(got, ",") != "agent-1/agent-1-turn3" {
				t.Errorf("interrupts %v, want the newer repair turn once", got)
			}
			rows := k.rowsOf("exe")
			var bound []string
			for _, r := range rowsNamed(rows, EvTurnStarted) {
				bound = append(bound, fmt.Sprintf("%s@k%d", rowStr(r.Data, "turn_id"), rowInt(r.Data, "k")))
			}
			if strings.Join(bound, ",") != "agent-1-turn1@k1,agent-1-turn3@k2" {
				t.Errorf("bound turns %v, want the task turn and the repair turn the stop's verify bound, once each; rows %s", bound, eventsOf(rows))
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

// CRW-1412, the adoption's stop: a process died after it stored thread_started, and the resumed process adopts that subagent. The host
// hides the dead process's turn and leaves the subagent idle, so the adoption's verify finds no turn of the spawn step and keeps
// looping; the operator's pause comes at the adoption's gone read. The stop's read starts two turns that carry the spawn step's marker
// and binds the newer one, and the older one is neither bound nor interrupted. Over the memory journal only: over the SQLite journal, a
// death at thread_started loses the bind's rows with its batch, so the resumed call does not adopt.
func TestSessionAdoptStopBindsTheAdoptedStepOnce(t *testing.T) {
	k := newE2aKit(t, deathAt(EvThreadStarted))
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if k.phaseNow() == 1 {
			return orchAnswer{hang: true}
		}
		return orchAnswer{edit: "P:" + st.Op}
	}
	k.die(e2aBuild)
	dead := k.rowsOf("exe")
	spawns := rowsNamed(dead, EvStepPublished)
	if len(spawns) != 1 || rowStr(spawns[0].Data, "action") != StepSpawn || len(rowsNamed(dead, EvThreadStarted)) != 1 {
		t.Fatalf("the dead process's rows %s, want the spawn step and its thread_started once", eventsOf(dead))
	}
	if ts := rowsNamed(dead, EvThreadStarted)[0]; rowStr(ts.Data, "thread_id") != "agent-1" || rowInt(ts.Data, "attempt") != 1 {
		t.Fatalf("the dead process's thread_started %v, want the spawned subagent agent-1 at attempt 1", ts.Data)
	}
	// the dead process's turn is hidden and the subagent idle: not gone, and no turn carries the spawn marker for the adoption to bind
	k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) {
		a.turns, a.Status = nil, "idle"
	})
	ctl := &Control{}
	host := &stopWatch{SessionHost: k.host, fake: k.host, marker: rowStr(spawns[0].Data, "marker"), pause: func() { ctl.Pause("operator", "test") }}
	res := k.resumeWithin(e2aBuild, 5*time.Second, func() {
		k.h.cfg.Control = ctl
		k.cfg.Host = host
		// the interrupt the pause asks for arms the host's log (stopWatch)
		k.h.cfg.Journal = &onRow{Journal: k.mem, event: EvInterruptRequested, f: host.row}
	})
	rows := k.rowsOf("exe")
	if n := len(rowsNamed(rows[len(dead):], EvThreadStarted)) + len(rowsNamed(rows[len(dead):], EvStepPublished)); n != 0 {
		t.Errorf("the resumed process wrote %d thread_started or step_published rows, want none: the adoption reuses the dead process's; rows %s", n, eventsOf(rows))
	}
	if started := rowsNamed(rows, EvTurnStarted); len(started) != 1 || rowInt(started[0].Data, "k") != 1 || rowStr(started[0].Data, "turn_id") != "agent-1-turn2" {
		t.Errorf("turn_started rows %s, want the one the stop bound: agent-1-turn2 at k 1; rows %s", eventsOf(rowsNamed(rows, EvTurnStarted)), eventsOf(rows))
	}
	checkStopPath(t, host.path(), "agent-1-turn1", "agent-1-turn2")
	if got := k.host.interrupted(); strings.Join(got, ",") != "agent-1/agent-1-turn2" {
		t.Errorf("interrupts %v, want the newer started turn once", got)
	}
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held engine_paused]" {
		t.Fatalf("the resumed call %v, want held engine_paused; rows %s", got, eventsOf(rows))
	}
}
