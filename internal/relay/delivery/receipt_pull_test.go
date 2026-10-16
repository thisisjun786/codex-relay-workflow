package delivery

// CRW-1310: a parent in its own live turn can pull a completion that the daemon deferred as busy into
// that turn. The pull sends nothing and starts no turn; it records the pulled turn, and the parent
// acknowledges the completion from that same turn. These tests drive PullReceipt, the acknowledgement
// and the pull's command over the delivery fixture and the fake host.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

const pullTurn = "pull-turn-1"

// busyCompletion is a completion the parent's live turn deferred: the parent thread is active with
// turn in progress on the host, and the daemon's attempt settles the delivery deferred_busy with no send.
func (f *fixture) busyCompletion(t *testing.T, turn string) string {
	t.Helper()
	event := f.queuedEvent(regOpts{})
	f.host.threads[parent].status = "active"
	f.host.startTurn(parent, turn, "inProgress", "")
	f.mustAttempt(event, nil)
	if state := f.row(event).S("state"); state != "deferred_busy" {
		t.Fatalf("a busy parent leaves the completion %q, want deferred_busy", state)
	}
	return event
}

func (f *fixture) pull(event, turn, task, caller string) (Obj, error) {
	return f.delivery.PullReceipt(f.ctx, f.host, PullRequest{Event: event, Turn: turn, Task: task, Caller: caller})
}

// requireBusyUntouched checks that a refused pull left the completion deferred_busy, wrote no pull
// record and sent nothing.
func (f *fixture) requireBusyUntouched(t *testing.T, event string, sends int) {
	t.Helper()
	if state := f.row(event).S("state"); state != "deferred_busy" {
		t.Fatalf("a refused pull left the completion %q, want deferred_busy", state)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'receipt_pulled' AND subject = ?", event); n != 0 {
		t.Fatalf("a refused pull wrote %d receipt_pulled rows", n)
	}
	if len(f.host.sends) != sends {
		t.Fatalf("a refused pull sent %d messages", len(f.host.sends)-sends)
	}
}

func TestReceiptPullDeliversABusyCompletionIntoTheCallersTurn(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	sends := len(f.host.sends)
	out, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	if state := pyjson.Text(out.Get("state")); state != "inbox_only" {
		t.Fatalf("the pull answers state %q, want inbox_only", state)
	}
	if turn := pyjson.Text(out.Get("turnId")); turn != pullTurn {
		t.Fatalf("the pull answers turn %q, want %q", turn, pullTurn)
	}
	if pyjson.Text(out.Get("message")) == "" {
		t.Fatal("the pull returns the receipt the parent reads in its turn, and it is empty")
	}
	row := f.row(event)
	if row.S("state") != "inbox_only" || row.S("dispatch_turn_id") != pullTurn || row.S("dispatch_evidence") != "receipt_pull" || row.Opt("hold_reason") != nil {
		t.Fatalf("the pulled row is state %q turn %q evidence %q hold %v", row.S("state"), row.S("dispatch_turn_id"), row.S("dispatch_evidence"), row.Opt("hold_reason"))
	}
	if len(f.host.sends) != sends {
		t.Fatalf("a pull sent %d messages, want none", len(f.host.sends)-sends)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'receipt_pulled' AND subject = ?", event); n != 1 {
		t.Fatalf("the pull journals %d receipt_pulled rows, want 1", n)
	}
}

// TestAParentCannotAckABusyCompletionBeforeAPull is the gap the pull closes: an acknowledgement
// refuses a completion that never reached the parent, so only a pull can put it in the parent's turn.
func TestAParentCannotAckABusyCompletionBeforeAPull(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := NewAck(f.delivery).Acknowledge(f.ctx, event, pullTurn, AckProof(event, pullTurn), true, nil, f.host)
	if err == nil {
		t.Fatal("a busy completion was acknowledged before any pull")
	}
	if state := f.row(event).S("state"); state != "deferred_busy" {
		t.Fatalf("the refused acknowledgement left the completion %q", state)
	}
}

func TestPulledCompletionIsAckedFromThePulledTurn(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	record, err := NewAck(f.delivery).Acknowledge(f.ctx, event, pullTurn, AckProof(event, pullTurn), true, nil, f.host)
	mustDo(t, err)
	if verified := pyjson.Text(record.Get("_verified")); verified != "verified" {
		t.Fatalf("the acknowledgement from the pulled turn is %q, want verified", verified)
	}
	if state := f.row(event).S("state"); state != "acknowledged" {
		t.Fatalf("the acknowledged completion is %q, want acknowledged", state)
	}
}

func TestPulledCompletionRefusesAnAckFromAnotherTurn(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	f.host.startTurn(parent, "other-turn", "inProgress", "")
	_, err = NewAck(f.delivery).Acknowledge(f.ctx, event, "other-turn", AckProof(event, "other-turn"), true, nil, f.host)
	requireReason(t, err, AckTurnUnverified)
	if n := f.count("SELECT COUNT(*) AS c FROM acks WHERE event_id = ?", event); n != 0 {
		t.Fatalf("a refused acknowledgement wrote %d ack rows", n)
	}
	if state := f.row(event).S("state"); state != "inbox_only" {
		t.Fatalf("the completion after a refused ack is %q, want inbox_only", state)
	}
}

func TestReceiptPullRefusesAnUnprovenCaller(t *testing.T) {
	cases := []struct{ name, task, caller string }{
		{"no CODEX_THREAD_ID", parent, ""},
		{"another thread's id", parent, child},
		{"a task that is not the recipient", child, child},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "")
			event := f.busyCompletion(t, pullTurn)
			sends := len(f.host.sends)
			_, err := f.pull(event, pullTurn, c.task, c.caller)
			requireReason(t, err, "pull_caller_unproven")
			f.requireBusyUntouched(t, event, sends)
		})
	}
}

func TestReceiptPullRequiresTheRecipientsLiveTurn(t *testing.T) {
	cases := []struct {
		name, turn string
		change     func(f *fixture)
	}{
		{"the turn has completed", pullTurn, func(f *fixture) { f.host.finishTurn(parent, pullTurn, "completed") }},
		{"the turn is not on the host", "missing-turn", func(f *fixture) {}},
		{"the recipient is idle", pullTurn, func(f *fixture) { f.host.threads[parent].status = "idle" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "")
			event := f.busyCompletion(t, pullTurn)
			c.change(f)
			sends := len(f.host.sends)
			_, err := f.pull(event, c.turn, parent, parent)
			requireReason(t, err, "pull_turn_not_live")
			f.requireBusyUntouched(t, event, sends)
		})
	}
}

func TestReceiptPullRefusesWhatIsNotPullable(t *testing.T) {
	cases := []struct {
		name   string
		change func(f *fixture, event string)
	}{
		{"withheld before its send", func(f *fixture, event string) {
			_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = 'withheld_pre_send' WHERE event_id = ?", event)
			mustDo(f.t, err)
		}},
		{"a send is in flight", func(f *fixture, event string) {
			_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = 'sending' WHERE event_id = ?", event)
			mustDo(f.t, err)
		}},
		{"already dispatched", func(f *fixture, event string) {
			_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = 'dispatched' WHERE event_id = ?", event)
			mustDo(f.t, err)
		}},
		{"held for another reason", func(f *fixture, event string) {
			_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET hold_reason = 'busy_cap' WHERE event_id = ?", event)
			mustDo(f.t, err)
		}},
		{"not a completion", func(f *fixture, event string) {
			_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET kind = 'revision' WHERE event_id = ?", event)
			mustDo(f.t, err)
		}},
		{"relationship superseded", func(f *fixture, event string) { f.supersede("rid-successor") }},
		{"an older generation", func(f *fixture, event string) {
			// The relationship moves on to generation 2, which is retained, so the completion's generation 1 is older.
			_, err := execSQL(f.ctx, f.store, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) SELECT relationship_id, 2, 'dispatch-gen-2', anchor_state, dispatch_turn_id, reason, opened_at, bound_at FROM generations WHERE relationship_id = ? AND execution_generation = 1", f.rid)
			mustDo(f.t, err)
			_, err = execSQL(f.ctx, f.store, "UPDATE relationships SET execution_generation = 2 WHERE relationship_id = ?", f.rid)
			mustDo(f.t, err)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, "")
			event := f.busyCompletion(t, pullTurn)
			c.change(f, event)
			sends := len(f.host.sends)
			_, err := f.pull(event, pullTurn, parent, parent)
			requireReason(t, err, "pull_not_pullable")
			if len(f.host.sends) != sends {
				t.Fatalf("a refused pull sent %d messages", len(f.host.sends)-sends)
			}
		})
	}
}

// TestReceiptPullWaitsBehindAnOlderDelivery keeps the order: a younger completion to the same parent
// is not pulled past an older one that is still waiting, and once the older is pulled the younger can be.
func TestReceiptPullWaitsBehindAnOlderDelivery(t *testing.T) {
	f := newFixture(t, "")
	older := f.busyCompletion(t, pullTurn)
	f.clock.Advance(5)
	younger := f.queuedEvent(regOpts{issue: "REL-2", dispatchRequest: "dispatch-2"})
	_, err := f.pull(younger, pullTurn, parent, parent)
	requireReason(t, err, "pull_behind_older_delivery")
	_, err = f.pull(older, pullTurn, parent, parent)
	mustDo(t, err)
	_, err = f.pull(younger, pullTurn, parent, parent)
	mustDo(t, err)
	if state := f.row(younger).S("state"); state != "inbox_only" {
		t.Fatalf("the younger completion after the older is pulled is %q, want inbox_only", state)
	}
}

func TestReceiptPullReplaysOnlyForThePullingTurn(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	f.host.startTurn(parent, "later-turn", "inProgress", "")
	_, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	again, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	if again.Get("_replay") != true {
		t.Fatalf("a repeated pull from the same turn answers %v, want a replay", again)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'receipt_pulled' AND subject = ?", event); n != 1 {
		t.Fatalf("a replayed pull journals %d receipt_pulled rows, want 1", n)
	}
	_, err = f.pull(event, "later-turn", parent, parent)
	requireReason(t, err, "pull_pulled_by_another_turn")
}

func TestReceiptPullAndTheDaemonClaimExcludeEachOther(t *testing.T) {
	t.Run("a claimed completion is not pulled", func(t *testing.T) {
		f := newFixture(t, "")
		event := f.busyCompletion(t, pullTurn)
		_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state = 'sending' WHERE event_id = ?", event)
		mustDo(t, err)
		_, err = f.pull(event, pullTurn, parent, parent)
		requireReason(t, err, "pull_not_pullable")
	})
	t.Run("a pulled completion is not claimed", func(t *testing.T) {
		f := newFixture(t, "")
		event := f.busyCompletion(t, pullTurn)
		_, err := f.pull(event, pullTurn, parent, parent)
		mustDo(t, err)
		sends := len(f.host.sends)
		record, err := f.attempt(event, nil)
		mustDo(t, err)
		if record != nil {
			t.Fatalf("the daemon claimed a pulled completion: %v", record)
		}
		if len(f.host.sends) != sends {
			t.Fatalf("the daemon sent %d messages for a pulled completion", len(f.host.sends)-sends)
		}
		if state := f.row(event).S("state"); state != "inbox_only" {
			t.Fatalf("the completion after the daemon's attempt is %q, want inbox_only", state)
		}
	})
}

func TestReceiptPullRequiresAuthorizedSettings(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := execSQL(f.ctx, f.store, "DELETE FROM authorized_settings WHERE task_id = ?", parent)
	mustDo(t, err)
	sends := len(f.host.sends)
	_, err = f.pull(event, pullTurn, parent, parent)
	requireReason(t, err, SettingsUnavailable)
	f.requireBusyUntouched(t, event, sends)
}

func TestReceiptPullCommandNeedsASocketAndSendsTheCallerIdentity(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", parent)
	run := &cliRun{command: "receipt-pull", ctx: context.Background(), state: t.TempDir(), clock: NewFakeClock(), args: map[string]any{
		"--event": strings.Repeat("a", 32), "--turn": pullTurn, "--task": parent,
	}}
	_, err := cmdReceiptPull(run)
	var usage *dispatch.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("without --socket the command answers %v, want a usage error", err)
	}
	run.socket = "/tmp/relay-test.sock"
	var gotCommand string
	var gotArgs map[string]any
	old := HostCommand
	HostCommand = func(_ context.Context, command, _, _ string, args map[string]any, _ Clock) (any, error) {
		gotCommand, gotArgs = command, args
		return Obj{}, nil
	}
	t.Cleanup(func() { HostCommand = old })
	_, err = cmdReceiptPull(run)
	mustDo(t, err)
	if gotCommand != "receipt-pull" || gotArgs["--caller-thread"] != parent {
		t.Fatalf("the host is asked %q with caller %v, want receipt-pull with the calling thread", gotCommand, gotArgs["--caller-thread"])
	}
}

// The corrections of the draft review (CRW-1310): a staged event, a stale generation, a recipient that
// moves during the host read, a replay judged against the current relationship, and a nil adapter.

// movingHost moves the delivery's recipient thread after the host has answered the turn read, so the
// store judgment has to find the row changed.
type movingHost struct {
	*fakeHost
	after func()
}

func (m *movingHost) ReadTurn(ctx context.Context, thread, turn string) (*TurnInfo, error) {
	out, err := m.fakeHost.ReadTurn(ctx, thread, turn)
	if m.after != nil {
		m.after()
		m.after = nil
	}
	return out, err
}

func TestReceiptPullRefusesAStagedEvent(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := execSQL(f.ctx, f.store, "UPDATE events SET stage = 'staged' WHERE event_id = ?", event)
	mustDo(t, err)
	_, err = f.pull(event, pullTurn, parent, parent)
	requireReason(t, err, "pull_not_pullable")
}

func TestReceiptPullRefusesAnEventOfAnotherGeneration(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := execSQL(f.ctx, f.store, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) SELECT relationship_id, 2, 'dispatch-gen-2', anchor_state, dispatch_turn_id, reason, opened_at, bound_at FROM generations WHERE relationship_id = ? AND execution_generation = 1", f.rid)
	mustDo(t, err)
	_, err = execSQL(f.ctx, f.store, "UPDATE relationships SET execution_generation = 2 WHERE relationship_id = ?", f.rid)
	mustDo(t, err)
	_, err = f.pull(event, pullTurn, parent, parent)
	requireReason(t, err, "pull_not_pullable")
}

func TestReceiptPullRefusesARecipientThatMovedDuringTheHostRead(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	host := &movingHost{fakeHost: f.host, after: func() {
		_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET recipient_thread_id = ? WHERE event_id = ?", "01moved-thread", event)
		mustDo(t, err)
	}}
	sends := len(f.host.sends)
	_, err := f.delivery.PullReceipt(f.ctx, host, PullRequest{Event: event, Turn: pullTurn, Task: parent, Caller: parent})
	requireReason(t, err, "pull_not_pullable")
	if n := f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'receipt_pulled' AND subject = ?", event); n != 0 {
		t.Fatalf("a pull refused for a moved recipient wrote %d receipt_pulled rows", n)
	}
	if len(f.host.sends) != sends {
		t.Fatalf("a refused pull sent %d messages", len(f.host.sends)-sends)
	}
}

func TestReceiptPullReplayAnswersTheStoredMessageOrRefuses(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	first, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	want := pyjson.Text(first.Get("message"))
	again, err := f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	if pyjson.Text(again.Get("message")) != want {
		t.Fatal("a replay answers a different message than the pull stored")
	}
	f.mustAttempt(event, nil)
	_, err = execSQL(f.ctx, f.store, "UPDATE events SET stage = 'staged' WHERE event_id = ?", event)
	mustDo(t, err)
	_, err = f.pull(event, pullTurn, parent, parent)
	requireReason(t, err, "pull_not_pullable")
}

func TestReceiptPullWithoutAHostAdapterRefusesBeforeAnyWrite(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	_, err := f.delivery.PullReceipt(f.ctx, nil, PullRequest{Event: event, Turn: pullTurn, Task: parent, Caller: parent})
	if err == nil {
		t.Fatal("a pull with no host adapter succeeded")
	}
	f.requireBusyUntouched(t, event, len(f.host.sends))
}

// TestPulledCompletionAckVerifiesAfterAFailedAttemptNewerThanTheTurn: an earlier attempt that failed
// its send, recorded after the pulled turn began, must not make the pulled turn look older than its own
// delivery. The pull names that turn and the host confirms it live.
func TestPulledCompletionAckVerifiesAfterAFailedAttemptNewerThanTheTurn(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	f.clock.Advance(10)
	_, err := execSQL(f.ctx, f.store, "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, record, sealed, sent_at, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?)", "req-failed", event, 1, "completion", "settled", "failed", "{}", 0, f.clock.ISO(), f.clock.ISO())
	mustDo(t, err)
	_, err = f.pull(event, pullTurn, parent, parent)
	mustDo(t, err)
	record, err := NewAck(f.delivery).Acknowledge(f.ctx, event, pullTurn, AckProof(event, pullTurn), true, nil, f.host)
	mustDo(t, err)
	if verified := pyjson.Text(record.Get("_verified")); verified != "verified" {
		t.Fatalf("the acknowledgement from the pulled turn is %q, want verified", verified)
	}
}

// TestReceiptPullRefusesATurnWithoutAStartTime: an acknowledgement from a turn the host gives no start
// time cannot be verified later, so the pull refuses it before writing.
func TestReceiptPullRefusesATurnWithoutAStartTime(t *testing.T) {
	f := newFixture(t, "")
	event := f.busyCompletion(t, pullTurn)
	f.host.threads[parent].turns = append(f.host.threads[parent].turns, TurnInfo{TurnID: "no-start", Status: "inProgress"})
	sends := len(f.host.sends)
	_, err := f.pull(event, "no-start", parent, parent)
	requireReason(t, err, "pull_turn_not_live")
	f.requireBusyUntouched(t, event, sends)
}
