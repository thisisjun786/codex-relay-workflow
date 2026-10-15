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

// CRW-1478 (CRW-1448 DI round 2): the plan-summary notice goes as `crw manage send-parent --logical-id summary-<id>` with the text
// report.SummaryNoticeText composes on every reconcile, and the outbox keys a logical id to the sha256 of the text it first carried. A
// notice an earlier build sent in its old words, and got no determined answer for, is retried by this build in the new words. The retry
// settles what that first send did from the bridge, by the record's request id: it sends again only when the bridge holds nothing sent
// under it, and it never answers the notice as refused or raises a summary_notice_refused decision escalation.

// summaryRewordOldText is report.SummaryNoticeText as dev 7f21f0069 wrote it.
func summaryRewordOldText(id, plan, document string) string {
	return fmt.Sprintf("[crw flow] summary_owed summary=%s plan=%s document=%s\nThe plan summary is owed to the document. You are the parent of the project: take the newest entry (`crw relay dag-summary-status --plan %s`), claim it (`dag-summary-claim`), write the document with your Linear connector, read it back and complete it (`dag-summary-complete`). The run and the manager do not claim.",
		id, plan, document, plan)
}

// summaryRewordOldBuild stands in for the earlier build's sender: the same crw manage send-parent, with the notice in its old words.
type summaryRewordOldBuild struct{ report.ManageSender }

func (o summaryRewordOldBuild) SendParent(ctx context.Context, p report.ParentSend) (report.SendResult, error) {
	p.Text = summaryRewordOldText(strings.TrimPrefix(p.LogicalID, "summary-"), "plan-1", "doc-1")
	return o.ManageSender.SendParent(ctx, p)
}

// summaryRewordEnv is a relay store holding a plan of a project, the project's registered 자식 (the orchestrator, orch-task) and a run
// of the plan that keeps its summary, and the summary hand-off of that run over crw manage send-parent run in this process. Wake proves
// the wake-up, so the notice is sent, not queued.
func summaryRewordEnv(t *testing.T) report.SummaryEnv {
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
	run := func(ctx context.Context, program string, args ...string) ([]byte, int, error) {
		if len(args) == 0 || args[0] != "manage" {
			return nil, 0, fmt.Errorf("not a crw manage command: %v", args)
		}
		var out, errOut strings.Builder
		code := Run(ctx, args[1:], strings.NewReader(""), &out, &errOut)
		t.Logf("crw %s: exit %d %s %s", strings.Join(args[:2], " "), code, strings.TrimSpace(out.String()), strings.TrimSpace(errOut.String()))
		return []byte(out.String()), code, nil
	}
	return report.SummaryEnv{Store: st, Sched: &dagsched.Scheduler{Store: st}, Rows: report.ForeignAppender{Store: st, Run: "run-1", Now: func() time.Time { return at }},
		Run: "run-1", Plan: "plan-1", Document: "doc-1", Actor: "orch-task",
		Sender: report.ManageSender{Program: "crw", Dir: t.TempDir(), Run: run}, Wake: func(context.Context, string) bool { return true }}
}

// summaryRewordSends are the send_message_to_thread calls the bridges saw, oldest first: request id and message.
func summaryRewordSends(t *testing.T, logs ...string) [][2]string {
	t.Helper()
	var sends [][2]string
	for _, log := range logs {
		for _, call := range deliverSendCallsOf(t, log) {
			if call["tool"] != deliverToolSend {
				continue
			}
			args, _ := call["args"].(map[string]any)
			message, _ := args["message"].(string)
			sends = append(sends, [2]string{deliverSendRequestIDOf(t, call), message})
		}
	}
	return sends
}

func TestSummaryNoticeRetryAfterRewordSettlesTheFirstSend(t *testing.T) {
	accepted := map[string]any{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}}
	for _, c := range []struct {
		name string
		// steps is what the bridge this build dials answers: get_operation of the first send's request id first.
		steps []map[string]any
		// settled is the result the notice settles on ("" none: it stays owed and is asked about again), sends the request ids
		// send_message_to_thread was called with across both builds.
		settled string
		sends   []string
	}{
		{name: "the first send went", steps: []map[string]any{accepted}, settled: report.SettledAccepted, sends: []string{"summary-%s"}},
		{name: "nothing was sent under it", steps: []map[string]any{
			{"payload": map[string]any{"status": "failed", "delivery": "not_delivered"}},
			{"payload": map[string]any{"observation": "idle"}},
			accepted,
		}, settled: report.SettledAccepted, sends: []string{"summary-%s", "summary-%s-r1"}},
		{name: "the bridge never recorded it", steps: []map[string]any{
			{"isError": true, "text": "Error executing tool get_operation: Unknown request_id"},
			{"payload": map[string]any{"observation": "idle"}},
			accepted,
		}, settled: report.SettledAccepted, sends: []string{"summary-%s", "summary-%s-r1"}},
		{name: "still undetermined", steps: []map[string]any{{"payload": map[string]any{"status": "in_progress_or_unknown"}}}, sends: []string{"summary-%s"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			coreTempHome(t)
			ctx := context.Background()
			// the earlier build: the intent is written and the notice goes in the old words; the bridge holds the attempt but places nothing
			oldBridge, oldLog := deliverFakeBridge(t, []map[string]any{
				{"payload": map[string]any{"observation": "idle"}},
				{"payload": map[string]any{"status": "in_progress_or_unknown"}},
			})
			cfg := deliverSendConfig(t, oldBridge)
			sendParentWithConfig(t, cfg)
			env := summaryRewordEnv(t)
			oldEnv := env
			oldEnv.Sender = summaryRewordOldBuild{env.Sender.(report.ManageSender)}
			first, err := report.ReconcileSummary(ctx, oldEnv)
			if err != nil || first.Intents != 1 || first.Sent != 1 || first.Settled != 0 {
				t.Fatalf("the earlier build's reconcile: %+v %v, want one intent, one send and no settlement", first, err)
			}
			id := first.SummaryID
			// this build retries the notice in the new words, against a bridge that answers for the first send
			newBridge, newLog := deliverFakeBridge(t, c.steps)
			cfg.Bridge.Binary = newBridge
			for round := 1; round <= 2; round++ {
				if _, err := report.ReconcileSummary(ctx, env); err != nil {
					t.Fatalf("this build's reconcile %d: %v", round, err)
				}
			}
			rows, err := report.ReadRows(ctx, env.Store.DB, "", report.KindSummarySettled, engine.EvEscalation)
			if err != nil {
				t.Fatal(err)
			}
			var settled []string
			for _, r := range rows {
				switch r.Kind {
				case report.KindSummarySettled:
					settled = append(settled, r.Str("result"))
				case engine.EvEscalation:
					t.Errorf("the retry raised an escalation: %s %s", r.Str("reason"), r.Str("detail"))
				}
			}
			switch {
			case c.settled == "" && len(settled) != 0:
				t.Errorf("an undetermined first send was settled %v; it stays owed", settled)
			case c.settled != "" && (len(settled) != 1 || settled[0] != c.settled):
				t.Errorf("the notice settled %v, want %s once", settled, c.settled)
			}
			sends := summaryRewordSends(t, oldLog, newLog)
			if len(sends) != len(c.sends) {
				t.Fatalf("the bridges were sent %d messages %v, want %d", len(sends), sends, len(c.sends))
			}
			for i, want := range c.sends {
				if sends[i][0] != fmt.Sprintf(want, id) {
					t.Errorf("send %d went under %q, want %q", i, sends[i][0], fmt.Sprintf(want, id))
				}
			}
			if sends[0][1] != summaryRewordOldText(id, "plan-1", "doc-1") {
				t.Errorf("the first send is not the earlier build's text: %q", sends[0][1])
			}
			if len(sends) == 2 && sends[1][1] != report.SummaryNoticeText(id, "plan-1", "doc-1") {
				t.Errorf("the resend is not this build's text: %q", sends[1][1])
			}
			// every bridge call of this build is about the first send's request id or this build's one resend
			for _, call := range deliverSendCallsOf(t, newLog) {
				if call["tool"] == deliverToolOperation && deliverSendRequestIDOf(t, call) != "summary-"+id {
					t.Errorf("get_operation asked about %q, want summary-%s", deliverSendRequestIDOf(t, call), id)
				}
			}
			if len(deliverSendCallsOf(t, newLog)) == 0 {
				t.Error("this build never asked the bridge what the first send did")
			}
		})
	}
}

// Message.Rewordable at its edges: a record of another thread is still refused locally; a settled record answers as it stands; a record
// the bridge recorded but never attempted takes a new request id even with the retry cap spent, since only the new words force it; an
// undetermined record keeps the words it was written for. A logical id that is not a summary notice is never rewordable through
// send-parent.
func TestDeliverRewordableNoticeEdges(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, cfg *Config, requestID, state string) {
		t.Helper()
		record := deliverSeedRecord(state)
		record.RequestID = requestID
		if err := deliverSave(cfg, record); err != nil {
			t.Fatal(err)
		}
	}
	load := func(t *testing.T, cfg *Config) deliverRecord {
		t.Helper()
		record, known, err := deliverLoad(cfg, "m1")
		if err != nil || !known {
			t.Fatalf("the record: %v %v", known, err)
		}
		return record
	}
	t.Run("another thread stays refused", func(t *testing.T) {
		e := deliverSendTestEnv(t)
		bridge, log := deliverFakeBridge(t, nil)
		cfg := deliverSendConfig(t, bridge)
		seed(t, cfg, "m1", deliverStateUnknown)
		out, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-2", Text: "hello again", Rewordable: true})
		if err == nil || out.Class != deliverClassRefused || len(deliverSendCallsOf(t, log)) != 0 {
			t.Fatalf("Deliver: %q %v, bridge calls %v; want a local refusal", out.Class, err, deliverSendCallsOf(t, log))
		}
	})
	t.Run("a settled record answers as it stands", func(t *testing.T) {
		e := deliverSendTestEnv(t)
		bridge, log := deliverFakeBridge(t, nil)
		cfg := deliverSendConfig(t, bridge)
		seed(t, cfg, "m1", deliverStateAccepted)
		out, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello again", Rewordable: true})
		if err != nil || out.Class != deliverClassAccepted || len(deliverSendCallsOf(t, log)) != 0 || load(t, cfg).MessageSHA256 != deliverMessageSHA256("hello") {
			t.Fatalf("Deliver: %q %v, bridge calls %v; want the accepted replay and the record unchanged", out.Class, err, deliverSendCallsOf(t, log))
		}
	})
	t.Run("not attempted at the retry cap", func(t *testing.T) {
		e := deliverSendTestEnv(t)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"status": "not_attempted", "retrySafe": true}},
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		})
		cfg := deliverSendConfig(t, bridge)
		seed(t, cfg, "m1-r2", deliverStateUnknown)
		out, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello again", Rewordable: true})
		if err != nil || out.Class != deliverClassAccepted || out.RequestID != "m1-r3" {
			t.Fatalf("Deliver: %+v %v, want accepted under m1-r3", out, err)
		}
		sends := summaryRewordSends(t, log)
		if len(sends) != 1 || sends[0] != [2]string{"m1-r3", "hello again"} {
			t.Fatalf("sends %v, want the new words once under m1-r3", sends)
		}
		if record := load(t, cfg); record.MessageSHA256 != deliverMessageSHA256("hello again") || record.RequestID != "m1-r3" || record.State != deliverStateAccepted {
			t.Fatalf("the record %+v does not name the text its request id carries", record)
		}
	})
	t.Run("the reword spends no retry", func(t *testing.T) {
		// m1 was refused not_delivered once (m1-r1 is retry 1 of 2), and the bridge recorded m1-r1 but never attempted it. In the old
		// words the next call resends under m1-r1 and has one retry left; in new words it moves to m1-r2 for its words alone, and
		// still has that one retry (m1-r3) after a not_delivered answer.
		e := deliverSendTestEnv(t)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"status": "not_attempted", "retrySafe": true}},
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "failed", "delivery": "not_delivered"}},
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		})
		cfg := deliverSendConfig(t, bridge)
		seed(t, cfg, "m1-r1", deliverStateUnknown)
		out, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello again", Rewordable: true})
		if err != nil || out.Class != deliverClassAccepted || out.RequestID != "m1-r3" {
			t.Fatalf("Deliver: %+v %v, want accepted under m1-r3", out, err)
		}
		if sends := summaryRewordSends(t, log); len(sends) != 2 || sends[0][0] != "m1-r2" || sends[1][0] != "m1-r3" {
			t.Fatalf("sends %v, want m1-r2 then m1-r3", sends)
		}
		// a later call keeps the count: the record names the id it took for its words
		record := load(t, cfg)
		if got := deliverRetryCap(record); got != deliverRetryLimit+1 {
			t.Errorf("the retry cap read back from the record is %d, want %d", got, deliverRetryLimit+1)
		}
	})
	t.Run("undetermined keeps the record's words", func(t *testing.T) {
		e := deliverSendTestEnv(t)
		bridge, log := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"status": "in_progress_or_unknown"}}})
		cfg := deliverSendConfig(t, bridge)
		seed(t, cfg, "m1", deliverStateUnknown)
		out, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello again", Rewordable: true})
		if err != nil || out.Class != deliverClassUnknown || len(summaryRewordSends(t, log)) != 0 {
			t.Fatalf("Deliver: %q %v, sends %v; want unknown and nothing sent", out.Class, err, summaryRewordSends(t, log))
		}
		if record := load(t, cfg); record.MessageSHA256 != deliverMessageSHA256("hello") || record.RequestID != "m1" {
			t.Fatalf("the record %+v changed words or id", record)
		}
	})
	t.Run("send-parent: only a summary notice is rewordable", func(t *testing.T) {
		coreTempHome(t)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "in_progress_or_unknown"}},
		})
		cfg := deliverSendConfig(t, bridge)
		sendParentWithConfig(t, cfg)
		if code, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--logical-id", "note-1", "--message-file", sendParentMessageFile(t, "a notice")); code != sendParentExitUnknown {
			t.Fatalf("the first send: exit %d %s", code, stdout)
		}
		calls := len(deliverSendCallsOf(t, log))
		code, stdout := sendParentRunCommand(t, "--thread", "thread-1", "--logical-id", "note-1", "--message-file", sendParentMessageFile(t, "the notice reworded"))
		if code != sendParentExitRefused || len(deliverSendCallsOf(t, log)) != calls {
			t.Fatalf("a reworded note-1: exit %d %s, bridge calls %d -> %d; want the local refusal", code, stdout, calls, len(deliverSendCallsOf(t, log)))
		}
	})
}
