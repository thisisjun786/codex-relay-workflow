package engine

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type ExecutorProvider interface {
	ExecutorKind() executor.Kind
}

func (c *callRun) prepareExecutorAdmission() {
	p := c.plan
	if c.e.cfg.ExecutorAdmission == nil || !c.writesNodeWorktree() || p.refusal != nil {
		return
	}
	if c.e.cfg.Resume != nil && c.e.prior != nil {
		rows := c.e.prior.rowsOf(c.id, "agent")
		if stateOf(c.id, rows).held() {
			return
		}
	}
	if c.e.cfg.Session == nil {
		provider, ok := c.e.cfg.Backend.(ExecutorProvider)
		if !ok || provider.ExecutorKind() != executor.Codex {
			p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: "the backend declares no codex executor"}
			return
		}
	}
	a, err := c.e.cfg.ExecutorAdmission(c.e.ctx, SnapshotRequest{RunID: c.e.runID, CallID: c.id, Node: p.node, Role: p.role, Head: p.opts.Head})
	if err != nil {
		p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: err.Error()}
		return
	}
	fail := func(detail string) {
		p.refusal = &Refusal{Reason: "host_config_executor_envelope", Detail: detail}
	}
	if err := executor.ValidateAdmission(a); err != nil {
		fail("the admission is invalid: " + err.Error())
		return
	}
	frozen, err := executor.Freeze(a.Task)
	if err != nil {
		fail("the admission is invalid: " + err.Error())
		return
	}
	if frozen.TaskDigest != a.TaskDigest {
		fail("the admission digest was mutated")
		return
	}
	if a.Task.Executor.Kind != executor.Codex {
		fail("the admission binds a non-codex executor")
		return
	}
	if a.Task.Identity.Node != p.node {
		fail("the admission is of another node")
		return
	}
	if a.Task.Input.Prompt != p.prompt {
		fail("the admission is of another prompt")
		return
	}
	if a.Task.Requested.Model != p.spec.Pair.Model || a.Task.Requested.Effort != p.spec.Pair.Effort {
		fail("the admission asks for another model or effort")
		return
	}
	c.executorAdmission = &frozen
}

func (c *callRun) recordExecutorProvenance(terminal Event) {
	if terminal.Seq == 0 || terminal.Kind != KindCall || terminal.Op != "agent" || !isTerminal(terminal.Event) {
		return
	}
	events := c.e.j.Events()
	admission, attempts, err := executorNativeEvidence(events, terminal)
	if err != nil {
		c.e.journalFailed(err)
		return
	}
	if len(attempts) == 0 {
		return
	}
	seen := map[executorNativeKey]bool{}
	for _, ev := range events {
		if ev.RunID != terminal.RunID || ev.CallID != terminal.CallID || ev.Kind != KindFlow ||
			ev.Op != "executor" || ev.Event != "executor_native" || rowInt(ev.Data, "native_call_seq") != terminal.Seq {
			continue
		}
		var provenance executor.Provenance
		if err := decodeExecutorNative(ev.Data["provenance"], &provenance); err != nil {
			c.e.journalFailed(fmt.Errorf("executor provenance at row %d: %w", ev.Seq, err))
			return
		}
		key := executorNativeKey{session: provenance.NativeSession.Value, attempt: rowInt(ev.Data, "session_attempt")}
		// Before attempts were recorded, an unambiguous single-session
		// projection already covered this terminal. Keep that record.
		if _, explicit := ev.Data["session_attempt"]; !explicit && len(attempts) == 1 && key.session == attempts[0].key.session {
			key = attempts[0].key
		}
		seen[key] = true
	}
	wrote := false
	for _, attempt := range attempts {
		if seen[attempt.key] {
			continue
		}
		data := map[string]any{
			"admission": admission, "provenance": attempt.provenance,
			"terminal_event": terminal.Event, "native_call_seq": terminal.Seq,
		}
		if attempt.key.attempt > 0 {
			data["session_attempt"] = attempt.key.attempt
		}
		c.e.j.Append(Event{RunID: terminal.RunID, Kind: KindFlow, Op: "executor",
			CallID: terminal.CallID, Event: "executor_native", Data: data})
		wrote = true
		if err := journalErr(c.e.j); err != nil {
			c.e.journalFailed(err)
			return
		}
	}
	if wrote {
		if err := journalFlush(c.e.j); err != nil {
			c.e.journalFailed(err)
			return
		}
		if err := journalErr(c.e.j); err != nil {
			c.e.journalFailed(err)
		}
	}
}

type executorNativeKey struct {
	session string
	attempt int
}

type executorNativeAttempt struct {
	key        executorNativeKey
	provenance executor.Provenance
}

func decodeExecutorNative(v any, dst any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// executorNativeEvidence projects only the native rows of the generation ending
// at terminal. The prepared row owns the frozen admission; thread/fork bindings
// own observed sessions and pairs. A respawn starts another attempt, not another
// list of turns attributed to the last session. Nothing here rewrites a native row.
func executorNativeEvidence(events []Event, terminal Event) (executor.Admission, []executorNativeAttempt, error) {
	var admission executor.Admission
	var rows []Event
	for _, ev := range events {
		if ev.Seq > terminal.Seq || ev.RunID != terminal.RunID || ev.CallID != terminal.CallID || ev.Kind != KindCall || ev.Op != "agent" {
			continue
		}
		if ev.Event == EvPrepared {
			rows = nil
		}
		rows = append(rows, ev)
	}
	if len(rows) == 0 || rows[0].Event != EvPrepared {
		return admission, nil, nil
	}
	prepared := rows[0]
	raw, admitted := prepared.Data["executor_admission"]
	if !admitted {
		return admission, nil, nil
	}
	if err := decodeExecutorNative(raw, &admission); err != nil {
		return admission, nil, fmt.Errorf("executor admission at row %d: %w", prepared.Seq, err)
	}
	if err := executor.ValidateAdmission(admission); err != nil {
		return admission, nil, fmt.Errorf("executor admission at row %d: %w", prepared.Seq, err)
	}
	if admission.Task.Executor.Kind != executor.Codex {
		return admission, nil, fmt.Errorf("executor admission at row %d is not native codex", prepared.Seq)
	}
	observed := func(v string) executor.Observation {
		if v != "" {
			return executor.Observation{Value: v}
		}
		return executor.Observation{UnknownReason: "not observed by the flow backend"}
	}
	var attempts []executorNativeAttempt
	indices := map[executorNativeKey]int{}
	bind := func(session string, attempt int, pair Pair) int {
		if session == "" {
			return -1
		}
		key := executorNativeKey{session: session, attempt: attempt}
		if at, exists := indices[key]; exists {
			return at
		}
		at := len(attempts)
		indices[key] = at
		attempts = append(attempts, executorNativeAttempt{key: key, provenance: executor.Provenance{
			Provider: executor.Codex, NativeSession: executor.Observation{Value: session},
			NativeTurns: []string{}, Model: observed(pair.Model), Effort: observed(pair.Effort),
			CPUMethod: observed(""), Tools: []executor.Tool{},
		}})
		return at
	}
	current := -1
	replaced := replacedTurns(rows)
	for _, ev := range rows {
		switch ev.Event {
		case EvThreadStarted, EvForkStarted:
			current = bind(rowStr(ev.Data, "thread_id"), rowInt(ev.Data, "attempt"),
				Pair{Model: rowStr(ev.Data, "model"), Effort: rowStr(ev.Data, "effort")})
		case EvTurnStarted:
			// A continuation may start a turn on the handle recorded by its
			// prepared row without creating a new native thread.
			if current < 0 {
				current = bind(rowStr(prepared.Data, "thread_id"), rowInt(ev.Data, "attempt"),
					Pair{Model: rowStr(prepared.Data, "configured_model"), Effort: rowStr(prepared.Data, "configured_effort")})
			}
			if current < 0 {
				return admission, nil, fmt.Errorf("executor turn at row %d has no native session binding", ev.Seq)
			}
			if attempt := rowInt(ev.Data, "attempt"); attempt != 0 && attempt != attempts[current].key.attempt {
				return admission, nil, fmt.Errorf("executor turn at row %d has no binding for session attempt %d", ev.Seq, attempt)
			}
			turn := rowStr(ev.Data, "turn_id")
			p := &attempts[current].provenance
			if turn != "" && !replaced[turn] && !slices.Contains(p.NativeTurns, turn) {
				p.NativeTurns = append(p.NativeTurns, turn)
			}
		}
	}
	return admission, attempts, nil
}
