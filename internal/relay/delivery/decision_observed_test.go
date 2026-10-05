package delivery

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-659: decision-reply --decision answer also answers a final, unsuppressed receipt the relay
// itself observed (producer daemon_observation) ending interrupted or failed, so the parent
// continues the child in the SAME generation through the relay's own delivery path. Every existing
// check stays; stop, split_approval and scope_change are refused for these receipts, and every
// other producer/outcome pair stays refused. These tests use temporary synthetic stores and a fake
// host only.

// newObsWorld registers a relationship and takes a daemon observation of its anchor turn ending in
// outcome, as the daemon does once it has seen that turn end.
func newObsWorld(t *testing.T, outcome string) *decWorld {
	t.Helper()
	h := newRulingHL(t)
	rid := h.register(regOpts{recipients: []string{parent, child}})
	h.registerCriteria(rrSet)
	d := &decWorld{hl: h, rid: rid}
	d.blocked = d.observe(outcome)
	return d
}

// observe takes one daemon observation of generation 1's anchor turn ending in outcome and returns
// its event id.
func (d *decWorld) observe(outcome string) string {
	d.t.Helper()
	stored, err := d.intake.DaemonObservation(d.ctx, d.rid, store.TurnReference{ThreadID: child, TurnID: dispatchTurn, Status: outcome})
	mustDo(d.t, err)
	if stored.Stage != store.StageFinal {
		d.t.Fatalf("a daemon observation of a %s turn is %s, want final", outcome, stored.Stage)
	}
	return stored.EventID
}

// recordChildProfile replaces the child's recorded settings with the fixture's, carrying an MCP
// profile, as a child released under that profile has them recorded.
func (d *decWorld) recordChildProfile(profile string) {
	d.t.Helper()
	data := loadsObj(taskSettings(d.root)).Set("mcpProfile", profile)
	d.exec("UPDATE authorized_settings SET settings = ? WHERE task_id = ?", dumps(data), child)
}

// profileHost is the fake host with the one thing the delivery layer hands the transport visible:
// the settings of every send. mismatch answers as the adapter does when the host reports MCP
// settings other than the record: a completed pre-send refusal, settings_not_preserved.
type profileHost struct {
	*fakeHost
	handed   []*TaskSettings
	mismatch bool
}

func (h *profileHost) SendMessage(ctx context.Context, requestID, thread, message string, settings *TaskSettings) (Obj, error) {
	h.handed = append(h.handed, settings)
	if !h.mismatch {
		return h.fakeHost.SendMessage(ctx, requestID, thread, message, settings)
	}
	receipt := Obj{
		{Key: "requestId", Value: requestID}, {Key: "operation", Value: "send_message_to_thread"},
		{Key: "status", Value: FailedStatus}, {Key: "threadId", Value: thread},
		{Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}},
		{Key: "settingsFindings", Value: []any{Obj{{Key: "code", Value: registry.SettingsNotPreserved}, {Key: "field", Value: "mcpServers"}}}},
		{Key: "rpcError", Value: Obj{{Key: "code", Value: registry.SettingsNotPreserved}, {Key: "message", Value: "settings_not_preserved: mcpServers returned other settings; message withheld"}}},
		{Key: "error", Value: "thread/resume: settings_not_preserved: mcpServers returned other settings; message withheld"},
	}
	h.ledger[requestID] = receipt
	h.sends = append(h.sends, fakeSend{requestID, thread, message, "settings_not_preserved"})
	return receipt, nil
}

// c1: an answer on a receipt the relay saw end interrupted or failed is recorded, keeps the
// generation, is queued to the child, and the message says the relay observed the end and keeps the
// continuation claim.
func TestDR13_an_answer_reaches_a_child_the_relay_saw_end(t *testing.T) {
	for _, outcome := range []string{"interrupted", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			d := newObsWorld(t, outcome)
			out := d.mustReply(DecisionAnswer, "continue from where you stopped", "")
			if field(out, "decision") != "answer" || field(out, "generationEffect") != "stays" ||
				field(out, "answersEvent") != d.blocked || field(out, "answersOutcome") != outcome {
				t.Fatalf("the answer record = %v", out)
			}
			if got := d.generation(); got != 1 {
				t.Fatalf("an answer moved the relationship to generation %d", got)
			}
			if n := d.count("SELECT COUNT(*) AS c FROM generations WHERE relationship_id = ?", d.rid); n != 1 {
				t.Fatalf("an answer opened a generation: %d rows", n)
			}
			event := decEvent(out)
			row := d.row(event)
			if row.S("kind") != Revision || row.S("recipient_task_id") != child || row.S("state") != Queued {
				t.Fatalf("the decision delivery = %v, want a queued revision_request to the child", row)
			}
			if n := d.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'decision_recorded' AND subject = ?", event); n != 1 {
				t.Fatalf("decision_recorded journal rows = %d", n)
			}
			d.mustAttempt(event, nil)
			if state := d.row(event).S("state"); state != Dispatched {
				t.Fatalf("the decision delivery is %s after an attempt, want dispatched", state)
			}
			message := d.sent().message
			for _, want := range []string{
				"The relay observed that your previous turn ended " + outcome + " without a receipt (event " + d.blocked + ")",
				"re-read your worktree, branch, commits and pull request and continue the work from where you stopped",
				"--continues-anchor " + dispatchTurn,
				"--continuation-actor " + child,
			} {
				if !strings.Contains(message, want) {
					t.Fatalf("the message does not carry %q:\n%s", want, message)
				}
			}
			if strings.Contains(message, "This answers the question your blocked_needs_input receipt asked") {
				t.Fatalf("the observed end is answered with the blocked receipt's wording:\n%s", message)
			}
		})
	}
}

// c1: stop, split_approval and scope_change are refused disposition_conflict on a receipt the relay
// saw end, with a reason that says a turn the relay saw end is continued with answer, and the
// refusal writes nothing.
func TestDR14_the_other_kinds_are_refused_on_a_receipt_the_relay_saw_end(t *testing.T) {
	for _, outcome := range []string{"interrupted", "failed"} {
		for _, kind := range []string{DecisionStop, DecisionSplitApproval, DecisionScopeChange} {
			t.Run(outcome+"/"+kind, func(t *testing.T) {
				d := newObsWorld(t, outcome)
				digest := ""
				if advances(kind) {
					digest = pyjson.Text(d.setDigest())
				}
				text := d.refused(kind, "go on", digest, DispositionConflict)
				rcMentions(t, text, "a turn the relay saw end is continued with answer")
			})
		}
	}
}

// c1: every check a blocked receipt's answer keeps applies to an observed end too.
func TestDR15_the_kept_refusals_hold_for_an_observed_end(t *testing.T) {
	t.Run("a child's own interrupted receipt", func(t *testing.T) {
		d := newDecWorld(t)
		d.blocked = d.emit("interrupted", "completed", store.AcceptOptions{})
		d.refused(DecisionAnswer, "go on", "", DispositionConflict)
	})
	t.Run("an older generation", func(t *testing.T) {
		d := newObsWorld(t, "interrupted")
		mustDo(t, d.store.Transaction(d.ctx, func(ctx context.Context, _ *sql.Conn) error {
			_, err := OpenGenerationIn(ctx, d.store, d.clock, d.rid, "other-dispatch", "initial_assignment", nil)
			return err
		}))
		d.refused(DecisionAnswer, "go on", "", StaleGeneration)
	})
	t.Run("the child reported again", func(t *testing.T) {
		d := newObsWorld(t, "failed")
		d.clock.Advance(5)
		d.emit("failed", "completed", store.AcceptOptions{})
		d.refused(DecisionAnswer, "go on", "", SupersededRevision)
	})
	t.Run("not final", func(t *testing.T) {
		d := newObsWorld(t, "interrupted")
		d.exec("UPDATE events SET stage = 'staged' WHERE event_id = ?", d.blocked)
		d.refused(DecisionAnswer, "go on", "", DispositionConflict)
	})
}

// c1: the answer to a blocked_needs_input receipt keeps its wording and gains answersOutcome.
func TestDR16_the_blocked_receipts_answer_keeps_its_wording(t *testing.T) {
	d := newDecWorld(t)
	out := d.mustReply(DecisionAnswer, "use the shorter table", "")
	if field(out, "answersOutcome") != "blocked_needs_input" {
		t.Fatalf("the answer record = %v", out)
	}
	d.mustAttempt(decEvent(out), nil)
	message := d.sent().message
	if !strings.Contains(message, "This answers the question your blocked_needs_input receipt asked") {
		t.Fatalf("the blocked receipt's wording changed:\n%s", message)
	}
	if strings.Contains(message, "The relay observed that your previous turn ended") {
		t.Fatalf("the blocked receipt's answer carries the observed-end wording:\n%s", message)
	}
}

// c2: the decision rides the transport a child delivery uses: a notLoaded child is resumed under
// the settings the record holds, carrying its MCP profile, and the delivery holds
// settings_not_preserved when the host reports other MCP settings than the record.
func TestDR17_a_decision_to_a_not_loaded_child_resumes_under_its_recorded_profile(t *testing.T) {
	t.Run("the resume carries the recorded profile", func(t *testing.T) {
		d := newObsWorld(t, "interrupted")
		d.recordChildProfile("ui-qa")
		d.host.threads[child].status = "notLoaded"
		event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
		host := &profileHost{fakeHost: d.host}
		if record := d.attemptOn(event, host, nil); pyjson.Text(record.Get("deliveryState")) != Dispatched {
			t.Fatalf("the decision to a notLoaded child = %v, want dispatched", record)
		}
		if len(host.handed) != 1 || pyjson.Text(host.handed[0].Data.Get("mcpProfile")) != "ui-qa" {
			t.Fatalf("the transport was handed %v, want the child's recorded MCP profile", host.handed)
		}
		if message := d.sent().message; !strings.Contains(message, "--continues-anchor "+dispatchTurn) {
			t.Fatalf("the decision to the notLoaded child does not carry the continuation claim:\n%s", message)
		}
	})
	t.Run("a mismatch holds the decision", func(t *testing.T) {
		d := newObsWorld(t, "failed")
		d.recordChildProfile("ui-qa")
		d.host.threads[child].status = "notLoaded"
		event := decEvent(d.mustReply(DecisionAnswer, "go on", ""))
		if record := d.attemptOn(event, &profileHost{fakeHost: d.host, mismatch: true}, nil); pyjson.Text(record.Get("deliveryState")) != WithheldPreSend {
			t.Fatalf("the decision on an MCP mismatch = %v, want withheld_pre_send", record)
		}
		if state := d.row(event).S("state"); state != WithheldPreSend {
			t.Fatalf("the decision delivery is %s on an MCP mismatch, want withheld_pre_send", state)
		}
		failure := d.one("SELECT error_code, retry_safe FROM failed_operations WHERE scope_key = ?", event)
		if failure == nil || failure.S("error_code") != registry.SettingsNotPreserved || failure.I("retry_safe") != 1 {
			t.Fatalf("the recorded failure = %v, want settings_not_preserved, retry-safe", failure)
		}
	})
}
