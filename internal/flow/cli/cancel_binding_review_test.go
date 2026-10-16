package cli

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

func reviewAppend(t *testing.T, l *laneLab, run Spec, events ...engine.Event) {
	t.Helper()
	j, err := journal.Open(ctx, l.w.st, run.RunID, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		e.RunID = run.RunID
		j.Append(e)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

func reviewBindingStatus(t *testing.T, l *laneLab) string {
	t.Helper()
	var status string
	if err := l.w.st.DB.QueryRow("SELECT status FROM scope_bindings WHERE binding_id='bind-parent'").Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestReviewReleaseWaitsForManagedStart(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	run := bindingTestRun(t, l)
	reviewAppend(t, l, run, engine.Event{Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled}})
	held, err := projectlock.Shared(ctx, l.w.st.Path, "P")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	bounded, done := context.WithTimeout(ctx, 100*time.Millisecond)
	defer done()
	released, err := (&registry.Registry{Store: l.w.st}).ReleaseProjectBinding(bounded, *run.Binding, run.RunID, func(context.Context, store.Querier) error { return nil })
	if released || reviewBindingStatus(t, l) != "active" {
		t.Fatalf("binding retired while managed-start shared lock held: released=%v err=%v status=%s", released, err, reviewBindingStatus(t, l))
	}
}

func TestReviewHeldRunKeepsBinding(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	held := bindingTestRun(t, l)
	reviewAppend(t, l, held,
		engine.Event{Kind: engine.KindFlow, Event: engine.EvEscalation, Data: map[string]any{"escalation_id": "esc-wait", "node": "A", "reason": "size_split", "ask": "land", "class": "size"}},
		engine.Event{Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted}})
	events, _, err := journal.Load(ctx, l.w.st, held.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if info := Summarize(RunRecord{RunID: held.RunID, Workflow: held.Workflow}, events, false, 0); info.State != StateHeld {
		t.Fatalf("expected held, got %s", info.State)
	}
	current := bindingTestRun(t, l)
	code, out, errOut := l.td.do("cancel", current.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if got := reviewBindingStatus(t, l); got != "active" {
		t.Fatalf("held run lost its binding: status=%s code=%d out=%s err=%s", got, code, out, errOut)
	}
}
