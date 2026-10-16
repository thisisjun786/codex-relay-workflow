package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1548: the plan-summary notice goes as `crw manage send-parent --logical-id summary-<id>`, and the manage outbox keys a logical id to
// the sha256 of the text it first carried. The flow sends the text the summary_notice row holds (the words of the earlier builds for a row
// that holds none), so a retry of a send another build left undetermined carries the same bytes and settles from the bridge's answer,
// never from a local refusal.

// summaryLegacyText is the notice in the words every build before CRW-1548 sent. It is this test's own copy, kept apart from the report
// package's.
func summaryLegacyText(id, plan, document string) string {
	return fmt.Sprintf("[crw flow] summary_owed summary=%s plan=%s document=%s\nThe plan summary is owed to the document. You are the parent of the project: take the newest entry (`crw relay dag-summary-status --plan %s`), claim it (`dag-summary-claim`), write the document with your Linear connector, read it back and complete it (`dag-summary-complete`). The run and the manager do not claim.",
		id, plan, document, plan)
}

// summaryOldBuild stands in for an earlier build's sender: the same crw manage send-parent, with the notice composed in its words
// whatever the intent row holds.
type summaryOldBuild struct{ report.ManageSender }

func (o summaryOldBuild) SendParent(ctx context.Context, p report.ParentSend) (report.SendResult, error) {
	p.Text = summaryLegacyText(strings.TrimPrefix(p.LogicalID, "summary-"), "plan-1", "doc-1")
	return o.ManageSender.SendParent(ctx, p)
}

// summaryResendEnv is a relay store holding a plan of a project, the project's registered 자식 (orch-task) and a run of the plan that
// keeps its summary, and the summary hand-off of that run over crw manage send-parent run in this process. Wake proves the wake-up, so
// the notice is sent, not queued. stderr collects what the commands wrote there.
func summaryResendEnv(t *testing.T) (report.SummaryEnv, *strings.Builder) {
	t.Helper()
	ctx := context.Background()
	_, st := flowTestStore(t)
	raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": "plan-1", "project_key": "PRJ-1", "request_id": "plan-1-r1",
		"expected_parent_revision": 0, "author_task_id": "task-test", "changes": []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc("A", dag.NodeNonPR)}}})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&dag.Repo{Store: st}).Put(ctx, revision); err != nil {
		t.Fatal(err)
	}
	if err := storeseed.InsertScopeBinding(ctx, st, store.ScopeBindingsRow{BindingID: "bnd-parent", Role: "parent", ScopeKind: "project", ScopeKey: "PRJ-1",
		TaskID: "orch-task", HostID: "host", Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	flowTestRun(t, st, "run-1", "orch-task", at, false)
	stderr := &strings.Builder{}
	run := func(ctx context.Context, program string, args ...string) ([]byte, int, error) {
		if len(args) == 0 || args[0] != "manage" {
			return nil, 0, fmt.Errorf("not a crw manage command: %v", args)
		}
		var out, errOut strings.Builder
		code := Run(ctx, args[1:], strings.NewReader(""), &out, &errOut)
		stderr.WriteString(errOut.String())
		t.Logf("crw %s: exit %d %s %s", strings.Join(args[:2], " "), code, strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()))
		return []byte(out.String()), code, nil
	}
	return report.SummaryEnv{Store: st, Sched: &dagsched.Scheduler{Store: st}, Rows: report.ForeignAppender{Store: st, Run: "run-1", Now: func() time.Time { return at }},
		Run: "run-1", Plan: "plan-1", Document: "doc-1", Actor: "orch-task",
		Sender: report.ManageSender{Program: "crw", Dir: t.TempDir(), Run: run}, Wake: func(context.Context, string) bool { return true }}, stderr
}

// summaryResendIntent asks for the plan's summary entry and writes its summary_notice row the way a build wrote it: with text, or (text
// "") without one, as every build before CRW-1548 did. It returns the summary id.
func summaryResendIntent(t *testing.T, env report.SummaryEnv, text string) string {
	t.Helper()
	ctx := context.Background()
	enq, err := env.Sched.EnqueueSummary(ctx, env.Plan, env.Actor, env.Document, false)
	if err != nil {
		t.Fatal(err)
	}
	id := enq.Entry.SummaryID
	payload := map[string]any{"summary_id": id, "plan": env.Plan, "document": env.Document, "logical_id": "summary-" + id, "run": env.Run}
	if text != "" {
		payload["text"] = text
	}
	if wrote, _, err := env.Rows.AppendOnce(ctx, report.NewRow{Kind: report.KindSummaryNotice, Payload: payload}, "summary_id"); err != nil || !wrote {
		t.Fatalf("the intent: %v %v", wrote, err)
	}
	return id
}

// summaryResendSeed is the send another build made of summary-<id> in text: the bridge holds the attempt and places nothing, so the
// outbox record stays unknown, keyed to the sha256 of text. It points send-parent at a configuration of its own and returns it; the test
// then points the configuration at the bridge it dials next.
func summaryResendSeed(t *testing.T, env report.SummaryEnv, id, text string) *Config {
	t.Helper()
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "in_progress_or_unknown"}},
	})
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	sent, err := env.Sender.SendParent(context.Background(), report.ParentSend{Thread: env.Actor, LogicalID: "summary-" + id, Text: text})
	if err != nil || sent.Class != deliverClassUnknown {
		t.Fatalf("the seed send: %+v %v, want unknown", sent, err)
	}
	record, known, err := deliverLoad(cfg, "summary-"+id)
	if err != nil || !known || record.State != deliverStateUnknown || record.MessageSHA256 != deliverMessageSHA256(text) {
		t.Fatalf("the seeded record %+v %v %v", record, known, err)
	}
	return cfg
}

// summaryResendSends are the send_message_to_thread calls a bridge saw, oldest first: request id and message.
func summaryResendSends(t *testing.T, log string) [][2]string {
	t.Helper()
	var sends [][2]string
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] != deliverToolSend {
			continue
		}
		args, _ := call["args"].(map[string]any)
		message, _ := args["message"].(string)
		sends = append(sends, [2]string{deliverSendRequestIDOf(t, call), message})
	}
	return sends
}

// summaryResendRows are the results the notice settled on and the escalations the run raised.
func summaryResendRows(t *testing.T, env report.SummaryEnv) (settled []string, escalations []report.Row) {
	t.Helper()
	rows, err := report.ReadRows(context.Background(), env.Store.DB, "", report.KindSummarySettled, engine.EvEscalation)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		switch r.Kind {
		case report.KindSummarySettled:
			settled = append(settled, r.Str("result"))
		case engine.EvEscalation:
			escalations = append(escalations, r)
		}
	}
	return settled, escalations
}

var (
	summaryResendAccepted = map[string]any{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}}
	summaryResendUnknown  = map[string]any{"isError": true, "text": "Error executing tool get_operation: Unknown request_id"}
	summaryResendIdle     = map[string]any{"payload": map[string]any{"observation": "idle"}}
)

// The DI repro: the intent row holds T1 and the outbox holds summary-<id> keyed to sha256(T1), undetermined. This build composes other
// words, and its reconcile still settles from the bridge's answer: what the first send did is answered as it stands, and a resend carries
// T1. It is never refused locally and raises no summary_notice_refused.
func TestSummaryResendInOtherWordsSettlesOnce(t *testing.T) {
	const t1 = "[crw flow] summary_owed: the plan summary is owed, in the words of another build"
	for _, c := range []struct {
		name  string
		steps []map[string]any
		sends []string
	}{
		{name: "the first send went", steps: []map[string]any{summaryResendAccepted}},
		{name: "the bridge never recorded it", steps: []map[string]any{summaryResendUnknown, summaryResendIdle, summaryResendAccepted}, sends: []string{"summary-%s-r1"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			coreTempHome(t)
			env, stderr := summaryResendEnv(t)
			id := summaryResendIntent(t, env, t1)
			if t1 == report.SummaryNoticeText(id, env.Plan, env.Document) {
				t.Fatal("T1 is this build's words; the repro needs other words")
			}
			cfg := summaryResendSeed(t, env, id, t1)
			bridge, log := deliverFakeBridge(t, c.steps)
			cfg.Bridge.Binary = bridge
			if _, err := report.ReconcileSummary(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			settled, esc := summaryResendRows(t, env)
			if len(settled) != 1 || settled[0] != report.SettledAccepted {
				t.Errorf("the notice settled %v, want accepted once", settled)
			}
			for _, r := range esc {
				t.Errorf("the retry raised an escalation: %s %s", r.Str("reason"), r.Str("detail"))
			}
			if strings.Contains(stderr.String(), "already belongs to another message") {
				t.Errorf("the retry was refused locally: %s", stderr.String())
			}
			sends := summaryResendSends(t, log)
			if len(sends) != len(c.sends) {
				t.Fatalf("the bridge was sent %v, want %d messages", sends, len(c.sends))
			}
			for i, want := range c.sends {
				if sends[i] != [2]string{fmt.Sprintf(want, id), t1} {
					t.Errorf("send %d is %v, want T1 under %s", i, sends[i], fmt.Sprintf(want, id))
				}
			}
		})
	}
}

// Guard: a row an earlier build wrote holds no text, and that build's send is in the outbox in its words, undetermined. The reconcile is
// not refused locally, and the resend carries the earlier words.
func TestSummaryLegacyIntentKeepsItsFirstWords(t *testing.T) {
	coreTempHome(t)
	env, stderr := summaryResendEnv(t)
	id := summaryResendIntent(t, env, "")
	legacy := summaryLegacyText(id, env.Plan, env.Document)
	cfg := summaryResendSeed(t, env, id, legacy)
	bridge, log := deliverFakeBridge(t, []map[string]any{summaryResendUnknown, summaryResendIdle, summaryResendAccepted})
	cfg.Bridge.Binary = bridge
	if _, err := report.ReconcileSummary(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "already belongs to another message") {
		t.Errorf("the legacy row's retry was refused locally: %s", stderr.String())
	}
	settled, esc := summaryResendRows(t, env)
	if len(settled) != 1 || settled[0] != report.SettledAccepted || len(esc) != 0 {
		t.Errorf("settled %v escalations %d, want accepted once and none", settled, len(esc))
	}
	if sends := summaryResendSends(t, log); len(sends) != 1 || sends[0] != [2]string{"summary-" + id + "-r1", legacy} {
		t.Errorf("sends %v, want the earlier words once under summary-%s-r1", sends, id)
	}
}

// The hazard the rollback note of docs/plugin-packaging.md describes: this build sent summary-<id> in its words and the outcome is
// undetermined; an earlier build, put back before the notice settled, composes its own words, and send-parent refuses the retry locally
// ("already belongs to another message") without dialling the bridge, so that build settles the notice refused and raises a false
// summary_notice_refused.
func TestSummaryRollbackBeforeSettleIsRefusedLocally(t *testing.T) {
	coreTempHome(t)
	env, stderr := summaryResendEnv(t)
	enqueued, err := env.Sched.EnqueueSummary(context.Background(), env.Plan, env.Actor, env.Document, false)
	if err != nil {
		t.Fatal(err)
	}
	id := enqueued.Entry.SummaryID
	words := report.SummaryNoticeText(id, env.Plan, env.Document)
	if words == summaryLegacyText(id, env.Plan, env.Document) {
		t.Fatal("this build's words are the earlier words; there is nothing to roll back")
	}
	if wrote, _, err := env.Rows.AppendOnce(context.Background(), report.NewRow{Kind: report.KindSummaryNotice, Payload: map[string]any{
		"summary_id": id, "plan": env.Plan, "document": env.Document, "logical_id": "summary-" + id, "run": env.Run, "text": words}}, "summary_id"); err != nil || !wrote {
		t.Fatalf("the intent: %v %v", wrote, err)
	}
	cfg := summaryResendSeed(t, env, id, words)
	bridge, log := deliverFakeBridge(t, []map[string]any{summaryResendAccepted})
	cfg.Bridge.Binary = bridge
	old := env
	old.Sender = summaryOldBuild{env.Sender.(report.ManageSender)}
	if _, err := report.ReconcileSummary(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "already belongs to another message") {
		t.Errorf("the earlier build's retry was not refused locally: %q", stderr.String())
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("the local refusal dialled the bridge: %v", calls)
	}
	settled, esc := summaryResendRows(t, env)
	if len(settled) != 1 || settled[0] != report.SettledRefused || len(esc) != 1 || esc[0].Str("reason") != "summary_notice_refused" {
		t.Errorf("settled %v escalations %v, want refused once and the summary_notice_refused the note warns of", settled, esc)
	}
}
