package engine

import (
	"context"
	"sync"
)

// EvRunPaused is the flow row of a pause the process was asked for from outside (an operator, a deadline): who asked and why.
// A pause the run makes itself (a token budget, a quota) writes the rows of its own cause (budget_warning, escalation) and
// the held calls say `budget_exhausted` or `engine_paused`.
const EvRunPaused = "run_paused"

// Control is the handle of a running script that its process keeps: the process of `crw flow pause` and of a run deadline asks the
// run to stop sending requests through it (Pause), and an answer that `crw flow answer` applies to a run a process holds asks it to
// stop at its next boundary (Drain). It is bound to the engine when Run starts; an ask made before that is applied the moment the run
// starts, and an ask made once the run decided its end (its end row and its completion), or after the run ended, changes nothing and
// writes no row.
type Control struct {
	// applyM orders the asks: an ask is accepted and applied to the engine under it (and bind applies the queued asks under it), so
	// the asks reach the engine in the order they were accepted and a drain accepted before a pause never overwrites the pause's
	// end asker.
	applyM sync.Mutex
	mu     sync.Mutex
	e      *Engine
	ended  bool
	// asked is set by every ask applied or queued: a Drain after it is a no-op.
	asked   bool
	pending []pauseAsk
}

// pauseAsk is one ask of the Control: who asked, why, and whether the turns in flight are interrupted (a Pause) or left to end (a
// Drain).
type pauseAsk struct {
	by, reason string
	interrupt  bool
}

// Pause asks the run to stop sending requests: a call that would send one ends held (`engine_paused`), and the turns in flight are
// interrupted, as a quota stop does. by says who asked ("operator", "deadline") and reason why; both are written to the journal as
// one `run_paused` flow row, once for each ask. A Pause is never a no-op while the run goes on: it interrupts a drained run's turns.
func (c *Control) Pause(by, reason string) { c.ask(by, reason, true) }

// Drain asks the run to stop at its next boundary: the turns in flight run to their end with their tokens and results (a session step
// published before the drain among them), and a call that would send its next request ends held (`engine_paused`). by ("answer") and
// reason are written as one `run_paused` flow row. A drain is asked once: a Drain after any ask of this Control (a second drain, a
// pause, a deadline ask) writes no row and changes nothing.
func (c *Control) Drain(by, reason string) { c.ask(by, reason, false) }

// ask is the body of Pause (interrupt) and Drain (not interrupt).
func (c *Control) ask(by, reason string, interrupt bool) {
	c.applyM.Lock()
	defer c.applyM.Unlock()
	c.mu.Lock()
	e := c.e
	if c.ended || (!interrupt && c.asked) {
		c.mu.Unlock()
		return
	}
	c.asked = true
	if e == nil {
		c.pending = append(c.pending, pauseAsk{by, reason, interrupt})
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	e.pauseOnAsk(by, reason, interrupt)
}

// end makes Pause and Drain no-ops: the run is over and its journal takes no more rows from the process.
func (c *Control) end() {
	c.mu.Lock()
	c.ended, c.e = true, nil
	c.mu.Unlock()
}

func (c *Control) bind(e *Engine) {
	c.applyM.Lock()
	defer c.applyM.Unlock()
	c.mu.Lock()
	c.e = e
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, p := range pending {
		e.pauseOnAsk(p.by, p.reason, p.interrupt)
	}
}

// endsPaused are the askers whose ask ends the run paused (a run/paused row, no finished row, no completion notice): the operator's
// `crw flow pause` and the drain of `crw flow answer` (the run stops for the answer, which resumes it). A deadline ask is the run's
// own limit, as a token budget and a quota are: the run finishes completed with its calls held.
var endsPaused = map[string]bool{"operator": true, "answer": true}

// EndsPaused is whether a pause asked by the asker by ends the run paused (endsPaused): a reader of a run's rows names the last such ask
// as who paused it, as the run's end row will.
func EndsPaused(by string) bool { return endsPaused[by] }

// pauseOnAsk applies an ask of the Control. An ask that comes once the run decided its end (Run reads endAsk for the last time) is
// refused: it writes no run_paused row and pauses nothing, because the end row and the completion the run tells are already the ones
// of an end without it. The row and endAsk are set under endM, so an ask is either wholly before that decision or refused. The turns
// in flight are interrupted only when interrupt (a Pause); a drain leaves them to end and the gate holds the next request.
func (e *Engine) pauseOnAsk(by, reason string, interrupt bool) {
	e.endM.Lock()
	if e.endDecided {
		e.endM.Unlock()
		return
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvRunPaused, Data: map[string]any{"by": by, "reason": reason}})
	if endsPaused[by] {
		e.endAsk = &pauseAsk{by, reason, interrupt}
	}
	e.endM.Unlock()
	e.pause(ReasonEnginePaused)
	if interrupt {
		e.interruptTurns(ReasonEnginePaused)
	}
}

type controlKey struct{}

// WithControl hands the engine's resume command (`RunCLI` with "resume") the Control of the process that calls it: the run of
// the command is bound to it, so the caller can pause that run (`crw flow resume RUN SCRIPT` at the deadline of the run's spec).
func WithControl(ctx context.Context, c *Control) context.Context {
	return context.WithValue(ctx, controlKey{}, c)
}

func controlOf(ctx context.Context) *Control {
	c, _ := ctx.Value(controlKey{}).(*Control)
	return c
}
