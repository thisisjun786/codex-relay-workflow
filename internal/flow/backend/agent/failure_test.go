package agent_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func failing(info any) func(fakehost.TurnRequest) fakehost.TurnPlan {
	return func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "the turn failed", CodexErrorInfo: info}}
	}
}

// A turn that fails for the host's own reasons is held and is no evidence for a tier step: the node gets no new thread
// and no new worktree, and crw.classify says host_held. Only a call whose output stays invalid is null_model.
func TestInfraTurnFailureNotTierEvidence(t *testing.T) {
	e := newEnv(t)
	e.plan(failing("serverOverloaded"))
	res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
return [a.status, a.reason, crw.classify(a), a.handle !== null]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held host_infra_serverOverloaded host_held true]" {
		t.Fatalf("%v", got)
	}
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d threads: a held call starts no new thread", n)
	}
	if n := e.worktreeAdds(); n != 1 {
		t.Errorf("%d worktrees were made", n)
	}
	held := e.row("a", engine.EvHeld)
	if held.Data["codex_error_info"] != "serverOverloaded" || held.Data["input_state"] != "delivered" || held.Data["p_held"] == nil {
		t.Errorf("held row %v", held.Data)
	}
	// the same node, when the model's output stays invalid, is null_model: the tier signal
	e2 := newEnv(t)
	e2.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "not json"} })
	res = e2.run(`const S = ` + verdictSchema + `
const a = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
return [a.status, crw.classify(a)]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"null_model", "null_model"}) {
		t.Errorf("%v", got)
	}
}

// A usage or rate limit holds the call and pauses the run: the next call is held before any request, and one
// escalation quota_or_rate is written.
func TestQuotaFailurePausesRun(t *testing.T) {
	for _, info := range []string{"usageLimitExceeded", "rateLimitExceeded"} {
		t.Run(info, func(t *testing.T) {
			e := newEnv(t)
			e.plan(failing(info))
			res := e.run(`const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
return [a.status, a.reason, b.status, b.reason]`)
			if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_infra_" + info, "held", "engine_paused"}) {
				t.Fatalf("%v", got)
			}
			if len(e.requests("thread/start")) != 1 || len(e.requests("turn/start")) != 1 {
				t.Errorf("the paused run sent more requests: %d threads, %d turns", len(e.requests("thread/start")), len(e.requests("turn/start")))
			}
			if held := e.row("b", engine.EvHeld); held.Data["input_state"] != "not_delivered" {
				t.Errorf("held before any request: %v", held.Data)
			}
			esc := e.flowRows("escalation")
			if len(esc) != 1 || esc[0].Data["reason"] != "quota_or_rate" || esc[0].Data["codex_error_info"] != info {
				t.Errorf("escalations %+v", esc)
			}
		})
	}
}

// A request the host rejects as a bad request - on the turn or on its start - is a configuration failure: refused, never
// null_model and never a tier signal.
func TestBadRequestSchemaIsRefused(t *testing.T) {
	t.Run("failed turn", func(t *testing.T) {
		e := newEnv(t)
		e.plan(failing("badRequest"))
		res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('x', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
return [a.status, a.reason, crw.classify(a)]`)
		if got := value[[]string](t, res); !slices.Equal(got, []string{"refused", "host_config_badRequest", "refused"}) {
			t.Fatalf("%v", got)
		}
	})
	t.Run("turn/start answered with the error", func(t *testing.T) {
		e := newEnv(t)
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
			return fakehost.TurnPlan{StartError: fakehost.ErrorWithInfo(-32600, "the outputSchema is invalid", "badRequest")}
		})
		res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('x', { agentType: 'explorer', schema: S, label: 'a' })
return [a.status, a.reason, a.handle !== null]`)
		if got := value[[]any](t, res); fmt.Sprint(got) != "[refused host_config_badRequest true]" {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("turn/start")); n != 1 {
			t.Errorf("%d turn/start requests: a refused call is not retried", n)
		}
	})
	t.Run("thread/start answered with the error", func(t *testing.T) {
		e := newEnv(t)
		e.threads.OnStart(func(map[string]any) map[string]any {
			return fakehost.ErrorWithInfo(-32600, "unsupported model", "badRequest")
		})
		res := e.run(`const a = await agent.call('x', { agentType: 'explorer', label: 'a' }); return [a.status, a.reason, a.handle]`)
		if got := value[[]any](t, res); fmt.Sprint(got) != "[refused host_config_badRequest <nil>]" {
			t.Fatalf("%v", got)
		}
		if held := e.row("a", engine.EvRefused); held.Data["codex_error_info"] != "badRequest" {
			t.Errorf("%v", held.Data)
		}
	})
	for _, info := range []string{"unauthorized", "sandboxError"} {
		t.Run(info, func(t *testing.T) {
			e := newEnv(t)
			e.plan(failing(info))
			res := e.run(`const a = await agent.call('x', { agentType: 'explorer', label: 'a' }); return [a.status, a.reason]`)
			if got := value[[]string](t, res); !slices.Equal(got, []string{"refused", "host_config_" + info}) {
				t.Fatalf("%v", got)
			}
		})
	}
}

// When a failure pauses the run, the turns still in flight are interrupted by the engine - the intent is journaled before
// the host is asked - and the interrupted turn is held engine_paused, never null_model.
func TestEnginePauseInterruptNotNullModel(t *testing.T) {
	e := newEnv(t)
	// the quota failure comes only once the other call's turn is in flight
	gate := make(chan struct{})
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "quota-call") {
			return fakehost.TurnPlan{Gate: gate, Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "usageLimitExceeded"}}
		}
		return fakehost.TurnPlan{Hang: true}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.host.WaitCount(ctx, "turn/start", 2)
		close(gate)
	}()
	res := e.run(`const out = await parallel([
  async () => agent.call('quota-call', { agentType: 'executor', node: 'a', label: 'quota' }),
  async () => agent.call('long-call', { agentType: 'executor', node: 'b', label: 'long' }),
])
return out.map((r) => [r.status, r.reason])`)
	got := value[[][]string](t, res)
	if fmt.Sprint(got) != "[[held host_infra_usageLimitExceeded] [held engine_paused]]" {
		t.Fatalf("%v", got)
	}
	long := calls(res)["long"]
	if long.Status == engine.StatusNullModel {
		t.Fatalf("an engine interrupt must not be null_model")
	}
	rows := kinds(e.rows("long"))
	intent := slices.Index(rows, engine.EvInterruptRequested)
	held := slices.Index(rows, engine.EvHeld)
	if intent < 0 || held < intent || slices.Index(rows, engine.EvTurnStarted) > intent {
		t.Errorf("rows %v: the interrupt intent must follow turn_started and precede the terminal row", rows)
	}
	if r := e.row("long", engine.EvInterruptRequested); r.Data["reason"] != "engine_paused" {
		t.Errorf("intent %v", r.Data)
	}
	if h := e.row("long", engine.EvHeld); h.Data["input_state"] != "delivered" || h.Data["p_held"] == nil {
		t.Errorf("held %v", h.Data)
	}
	if n := len(e.threads.Interrupts()); n != 1 {
		t.Errorf("%d turn/interrupt requests reached the host, want 1", n)
	}
}

// When the host never ends the turn the engine interrupted, the intent still decides the call: it is held engine_paused
// once the wait for the interrupted turn is over, not left open as a lost request.
func TestEnginePauseWhenTheHostIgnoresTheInterrupt(t *testing.T) {
	e := newEnv(t, withInterruptWait(300*time.Millisecond))
	gate := make(chan struct{})
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "quota-call") {
			return fakehost.TurnPlan{Gate: gate, Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "rateLimitExceeded"}}
		}
		return fakehost.TurnPlan{Hang: true, IgnoreInterrupt: true}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.host.WaitCount(ctx, "turn/start", 2)
		close(gate)
	}()
	res := e.run(`const out = await parallel([
  async () => agent.call('quota-call', { agentType: 'executor', node: 'a', label: 'quota' }),
  async () => agent.call('long-call', { agentType: 'executor', node: 'b', label: 'long' }),
])
return out.map((r) => [r.status, r.reason])`)
	if got := value[[][]string](t, res); fmt.Sprint(got) != "[[held host_infra_rateLimitExceeded] [held engine_paused]]" {
		t.Fatalf("%v", got)
	}
	rows := kinds(e.rows("long"))
	if rows[len(rows)-1] != engine.EvHeld || !slices.Contains(rows, engine.EvInterruptRequested) {
		t.Errorf("rows %v", rows)
	}
	if h := e.row("long", engine.EvHeld); h.Data["input_state"] != "delivered" || h.Data["p_held"] == nil {
		t.Errorf("held %v", h.Data)
	}
}

// A turn the host reports interrupted with no interrupt intent of the engine's is held host_interrupted_external.
func TestExternalInterruptHolds(t *testing.T) {
	e := newEnv(t)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Interrupted: true} })
	res := e.run(`const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason, crw.classify(a)]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_interrupted_external", "host_held"}) {
		t.Fatalf("%v", got)
	}
	if slices.Contains(kinds(e.rows("a")), engine.EvInterruptRequested) {
		t.Errorf("no engine intent was journaled")
	}
	if n := len(e.threads.Interrupts()); n != 0 {
		t.Errorf("the engine sent %d interrupts", n)
	}
	if h := e.row("a", engine.EvHeld); h.Data["input_state"] != "delivered" {
		t.Errorf("%v", h.Data)
	}
}

// contextWindowExceeded is the one host failure that is null_model.
func TestContextWindowExceededIsNullModel(t *testing.T) {
	e := newEnv(t)
	e.plan(failing("contextWindowExceeded"))
	res := e.run(`const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason, crw.classify(a)]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"null_model", "context_window_exceeded", "null_model"}) {
		t.Fatalf("%v", got)
	}
}

// A schema that breaks the strict rules is refused before a thread exists: nothing reaches the host and no worktree is made.
func TestAdhocSchemaNotStrictRefused(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const bad = { type: 'object', properties: { a: { type: 'string' } }, required: [], additionalProperties: false }
const r = await agent.call('x', { agentType: 'executor', node: 'n1', schema: bad, label: 'x' })
return [r.status, r.reason, r.handle]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[refused schema_not_strict <nil>]" {
		t.Fatalf("%v", got)
	}
	if reqs := e.requests(); len(reqs) != 0 {
		t.Errorf("a refused schema sent %d requests: %v", len(reqs), reqs)
	}
	if e.worktreeAdds() != 0 {
		t.Errorf("a worktree was made for a call that cannot start")
	}
	if got := kinds(e.rows("x")); !slices.Equal(got, []string{"prepared", "refused"}) {
		t.Errorf("rows %v", got)
	}
}

// turn/start refused with activeTurnNotSteerable is held (host_infra_activeTurnNotSteerable) with the packet not delivered;
// no thread is started in its place.
func TestActiveTurnNotSteerableHolds(t *testing.T) {
	e := newEnv(t)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{StartError: fakehost.ErrorWithInfo(-32600, "the active turn cannot be steered", map[string]any{"activeTurnNotSteerable": map[string]any{"turnKind": "review"}})}
	})
	res := e.run(`const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason, a.handle !== null]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held host_infra_activeTurnNotSteerable true]" {
		t.Fatalf("%v", got)
	}
	held := e.row("a", engine.EvHeld)
	if held.Data["input_state"] != "not_delivered" || held.Data["codex_error_info"] != "activeTurnNotSteerable" {
		t.Errorf("held %v", held.Data)
	}
	if held.Data["p_held"] == nil {
		t.Errorf("an executor's held row carries its worktree snapshot: %v", held.Data)
	}
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d threads: the held call starts no new thread", n)
	}
}

// A failed turn with no Turn.error, with a null codexErrorInfo or with a variant the pinned schema does not list is held
// host_infra_unknown, the packet delivered - never null_model and never refused.
func TestNullCodexErrorInfoHoldsInfraUnknown(t *testing.T) {
	for name, fail := range map[string]*fakehost.Failure{
		"no Turn.error":          {NoError: true},
		"null codexErrorInfo":    {Message: "no info", CodexErrorInfo: nil},
		"a variant not in 0.154": {Message: "new", CodexErrorInfo: "aVariantThePinnedSchemaLacks"},
		"an object variant":      {Message: "new", CodexErrorInfo: map[string]any{"aNewVariant": map[string]any{"x": 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Fail: fail} })
			res := e.run(`const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason, crw.classify(a)]`)
			if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_infra_unknown", "host_held"}) {
				t.Fatalf("%v", got)
			}
			if h := e.row("a", engine.EvHeld); h.Data["input_state"] != "delivered" {
				t.Errorf("%v", h.Data)
			}
		})
	}
	// an RPC error that carries no CodexErrorInfo is the same
	e := newEnv(t)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{StartError: map[string]any{"code": -32603, "message": "internal"}}
	})
	res := e.run(`const a = await agent.call('x', { agentType: 'explorer', label: 'a' }); return [a.status, a.reason]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"held", "host_infra_unknown"}) {
		t.Errorf("%v", got)
	}
}

// A turn/start that got no answer - the connection was lost, or the answer did not come in time - is not held: the call
// keeps turn_requested as its last row and the script sees an unknown outcome.
func TestTurnStartTransportLossIsNotHeld(t *testing.T) {
	cases := map[string]struct {
		ack   time.Duration
		reply fakehost.Reply
	}{
		"connection closed": {reply: fakehost.Reply{Close: &fakehost.CloseFrame{Code: websocket.StatusInternalError, Reason: "gone"}}},
		"no answer in time": {ack: 150 * time.Millisecond, reply: fakehost.Reply{Delay: 3 * time.Second}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if c.ack > 0 {
				e.client.BoundAckOn("turn/start", c.ack)
			}
			e.host.Script("turn/start", c.reply)
			res := e.run(`const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason, a.handle !== null]`)
			if got := value[[]any](t, res); fmt.Sprint(got) != "[unknown dispatch_unknown true]" {
				t.Fatalf("%v", got)
			}
			rows := kinds(e.rows("a"))
			if rows[len(rows)-1] != engine.EvTurnRequested {
				t.Errorf("rows %v: turn_requested must stay the last row", rows)
			}
			for _, k := range rows {
				if k == engine.EvHeld || k == engine.EvOutcomeUnknown {
					t.Errorf("rows %v: a lost request is neither held nor decided by the engine", rows)
				}
			}
			if n := len(e.requests("thread/start")); n != 1 {
				t.Errorf("%d threads", n)
			}
		})
	}
}

// A run that is cancelled while a turn is in flight asks the host to interrupt it, and the call ends superseded (cancelled),
// not held and not null_model.
func TestCancelledRunInterruptsTheTurn(t *testing.T) {
	e := newEnv(t)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Hang: true} })
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// the run is cancelled once the call's turn is known to be running
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvTurnStarted {
			cancel()
		}
	})
	res := e.runWith(ctx, `const a = await agent.call('x', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	if res.Status != engine.RunScriptError || res.ErrorKind != engine.ErrKindCancelled {
		t.Fatalf("run %s/%s", res.Status, res.ErrorKind)
	}
	call := calls(res)["a"]
	if call.Status != engine.StatusSuperseded || call.Reason != engine.ReasonCancelled {
		t.Errorf("%s/%s", call.Status, call.Reason)
	}
	if n := len(e.threads.Interrupts()); n != 1 {
		t.Errorf("%d turn/interrupt requests reached the host, want 1", n)
	}
}
