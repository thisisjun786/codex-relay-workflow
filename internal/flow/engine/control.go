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
// the moment the run starts, and a pause asked after the run ended changes nothing.
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

func (e *Engine) pauseOnAsk(by, reason string) {
	e.j.Append(Event{RunID: e.runID, Kind: KindFlow, Event: EvRunPaused, Data: map[string]any{"by": by, "reason": reason}})
	if endsPaused[by] {
		e.pauseM.Lock()
		e.endAsk = &pauseAsk{by, reason}
		e.pauseM.Unlock()
	}
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
