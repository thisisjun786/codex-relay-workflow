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
// run to stop sending requests through it. It is bound to the engine when Run starts; a pause asked before that is applied
// the moment the run starts, and a pause asked once the run decided its end (its end row and its completion), or after the run
// ended, changes nothing and writes no row.
type Control struct {
	mu      sync.Mutex
	e       *Engine
	ended   bool
	pending []pauseAsk
}

type pauseAsk struct{ by, reason string }

// Pause asks the run to stop sending requests: a call that would send one ends held (`engine_paused`), and the turns in flight are
// interrupted, as a quota stop does. by says who asked ("operator", "deadline") and reason why; both are written to the journal as
// one `run_paused` flow row, once for each ask.
func (c *Control) Pause(by, reason string) {
	c.mu.Lock()
	e := c.e
	if c.ended {
		c.mu.Unlock()
		return
	}
	if e == nil {
		c.pending = append(c.pending, pauseAsk{by, reason})
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	e.pauseOnAsk(by, reason)
}

// end makes Pause a no-op: the run is over and its journal takes no more rows from the process.
func (c *Control) end() {
	c.mu.Lock()
	c.ended, c.e = true, nil
	c.mu.Unlock()
}

func (c *Control) bind(e *Engine) {
	c.mu.Lock()
	c.e = e
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	for _, p := range pending {
		e.pauseOnAsk(p.by, p.reason)
	}
}

// endsPaused are the askers whose pause ends the run paused (a run/paused row, no finished row, no completion notice): the operator's
// `crw flow pause`. A deadline ask is the run's own limit, as a token budget and a quota are: the run finishes completed with its calls
// held.
var endsPaused = map[string]bool{"operator": true}

// pauseOnAsk applies an ask of the Control. An ask that comes once the run decided its end (Run reads endAsk for the last time) is
// refused: it writes no run_paused row and pauses nothing, because the end row and the completion the run tells are already the ones
// of an end without it. The row and endAsk are set under endM, so an ask is either wholly before that decision or refused.
func (e *Engine) pauseOnAsk(by, reason string) {
	e.endM.Lock()
	if e.endDecided {
		e.endM.Unlock()
		return
	}
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvRunPaused, Data: map[string]any{"by": by, "reason": reason}})
	if endsPaused[by] {
		e.endAsk = &pauseAsk{by, reason}
	}
	e.endM.Unlock()
	e.pause(ReasonEnginePaused)
	e.interruptTurns(ReasonEnginePaused)
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
