package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// Characterization of Channel.attempt, written before the function was split into steps and kept as
// the proof that the split moved nothing. Each test states what the code does today, not what it
// should do: three of them (the restatement cap, a refreshProposal error at the transport start and
// a row that moved before the obsolete-claim hold) reach branches no other test reached, and the
// write-failure table pins, for each statement an attempt writes, which error comes back and what
// the failed write leaves behind.
//
// A write failure is injected with a SQLite trigger that aborts exactly one statement (each trigger
// names its event, its table and a WHEN clause). A read cannot be failed that way, so the error
// returns of the reads (the first Get, the Gets after a reopen or a settle, the older-message
// lookup, the rate preflight) are not characterized here.

const (
	charNow  = 1_700_000_000
	charBoom = "crw421 boom"
)

// charHost is a sendHost that runs onRead each time the lifecycle gate reads the recipient's thread.
type charHost struct {
	*sendHost
	onRead func()
}

func (h *charHost) ReadThread(ctx context.Context, id string) (delivery.ThreadFacts, error) {
	if h.onRead != nil {
		h.onRead()
	}
	return h.sendHost.ReadThread(ctx, id)
}

// abortOn installs a trigger that aborts the statements event ("UPDATE", "UPDATE OF packet",
// "INSERT") performs on table while when holds.
func abortOn(t *testing.T, f *stageFixture, event, table, when string) {
	t.Helper()
	sweepExec(t, f, fmt.Sprintf("CREATE TRIGGER char_abort BEFORE %s ON %s WHEN %s BEGIN SELECT RAISE(ABORT,'%s'); END", event, table, when, charBoom))
}

func journalSeq(t *testing.T, f *stageFixture) int64 {
	t.Helper()
	var seq int64
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT COALESCE(MAX(seq),0) FROM journal").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func orDash(s string, valid bool) string {
	if !valid || s == "" {
		return "-"
	}
	return s
}

// charSummary says where an attempt left the message, its attempts, the journal entries written
// after since, and what the host was sent.
func charSummary(t *testing.T, f *stageFixture, id string, since int64, host *sendHost) string {
	t.Helper()
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	next := "-"
	if row.NextEligibleAt.Valid {
		next = fmt.Sprintf("%.0f", row.NextEligibleAt.Float64)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "message=%s hold=%s next=%s lease=%s count=%d", row.State, orDash(row.HoldReason.String, row.HoldReason.Valid), next, orDash(row.LeaseOwner.String, row.LeaseOwner.Valid), row.AttemptCount)
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		fmt.Fprintf(&b, " | a%d %s sent=%s safe=%d started=%t", a.AttemptNo, a.State, a.SendAttempted, a.RetrySafe, a.TransportStartedAt.Valid)
	}
	rows, err := f.s.DB.QueryContext(f.ctx, "SELECT kind FROM journal WHERE seq>? ORDER BY seq", since)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, " | journal=[%s] sends=%d", strings.Join(kinds, ","), len(host.sends))
	return b.String()
}

func charStaged(t *testing.T) (*stageFixture, string) {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	return f, stagedID(t, f)
}

// dischargeInLinear makes the staged completion already confirmed in the record the supervisor
// reads, so the claim refuses it as superseded_revision (what Test24_SCH_73 does).
func dischargeInLinear(t *testing.T, f *stageFixture) {
	t.Helper()
	sweepExec(t, f,
		"INSERT INTO sync_targets(relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','document-a','"+f.at+"')",
		"INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES ('sync-1','rel-1','REL-1','coordination_document','document-a','verdict','event-1','digest','ruled','confirmed','"+f.at+"','"+f.at+"')")
}

// The scenarios below each bring an attempt to one of its writes; the table pairs them with the
// write to fail.

func hierarchyMovesAtTransport(t *testing.T, f *stageFixture) {
	f.c.beforeTransport = func() { moveDevinSupervisor(t, f) }
}

func proposalRestatedAtTransport(t *testing.T, f *stageFixture) {
	f.c.beforeTransport = func() { reportNumber24(t, f, 2, 10) }
}

func proposalSupersededAtTransport(t *testing.T, f *stageFixture) {
	f.c.beforeTransport = func() {
		sweepExec(t, f, "DELETE FROM work_reports WHERE event_id='event-1'", "DELETE FROM events WHERE event_id='event-1'")
	}
}

func settingsChangeAtTransport(f *stageFixture) {
	current := &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/before"}}}
	f.c.Settings = nil
	f.c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return current, nil }
	f.c.beforeTransport = func() {
		current = &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/after"}}}
	}
}

func settingsUnreadable(f *stageFixture) {
	f.c.Settings = nil
	f.c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return nil, errors.New("loader down") }
}

func budgetSpentAtTransport(t *testing.T, f *stageFixture) {
	f.c.beforeTransport = func() {
		service := delivery.NewService(f.s, delivery.SystemClock{})
		if refused, err := service.ReserveSend(f.ctx, "other-relationship", "supervisor", charNow); err != nil || refused != "" {
			t.Errorf("other sender %q %v", refused, err)
		}
	}
}

func TestAttemptCharacterization_an_unknown_message_is_refused_before_anything_is_read(t *testing.T) {
	t.Parallel()
	f, _ := charStaged(t)
	host := &sendHost{status: "idle"}
	since := journalSeq(t, f)
	record, err := f.c.Attempt(f.ctx, "no-such-message", host, charNow)
	var refusal Refusal
	if record != nil || !errors.As(err, &refusal) || refusal.Reason != "not_claimable" || refusal.Detail != "no supervisor message is staged as 'no-such-message'" {
		t.Fatalf("record %v err %v", record, err)
	}
	if journalSeq(t, f) != since || len(host.sends) != 0 {
		t.Fatalf("journal moved or host sent: %d -> %d, sends %v", since, journalSeq(t, f), host.sends)
	}
}

// A write that fails stops the attempt with that error: never a quiet nil and never the refusal the
// attempt was about to return. charWant says what the failed write leaves behind.
func TestAttemptCharacterization_a_failed_write_stops_the_attempt_with_its_error(t *testing.T) {
	t.Parallel()
	const (
		toQueued   = "OLD.state='sending' AND NEW.state='queued'"
		attemptEnd = "NEW.state='withheld_pre_send'"
	)
	journal := func(kind string) string { return "NEW.kind='" + kind + "'" }
	for _, tc := range []struct {
		name                string
		status              string
		scenario            func(t *testing.T, f *stageFixture, host *sendHost)
		event, table, where string
	}{
		{"recovering a stranded send", "", func(t *testing.T, f *stageFixture, _ *sendHost) {
			sweepExec(t, f, "UPDATE supervisor_messages SET state='sending',attempt_count=1,lease_owner='gone',lease_until=1699999000")
		}, "UPDATE", "supervisor_messages", "OLD.state='sending' AND NEW.state='held_uncertain'"},
		{"reopening a superseded hold", "", func(t *testing.T, f *stageFixture, _ *sendHost) {
			sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='superseded_by_report'")
		}, "UPDATE", "supervisor_messages", "OLD.hold_reason='superseded_by_report' AND NEW.hold_reason IS NULL"},
		{"reopening an unaddressed hold", "", func(t *testing.T, f *stageFixture, _ *sendHost) {
			sweepExec(t, f, "UPDATE supervisor_messages SET hold_reason='hierarchy_unresolved'")
		}, "UPDATE", "supervisor_messages", "OLD.hold_reason='hierarchy_unresolved' AND NEW.hold_reason IS NULL"},
		{"holding a message whose hierarchy no longer resolves", "", func(t *testing.T, f *stageFixture, _ *sendHost) {
			if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "successor", f.at); err != nil {
				t.Fatal(err)
			}
		}, "UPDATE", "supervisor_messages", "NEW.hold_reason='hierarchy_unresolved'"},
		{"holding a message whose owner drifted", "", func(t *testing.T, f *stageFixture, _ *sendHost) { moveDevinSupervisor(t, f) },
			"UPDATE", "supervisor_messages", "NEW.hold_reason='hierarchy_unresolved'"},
		{"recording the recipient's lifecycle", "", nil, "INSERT", "recipient_lifecycle", "NEW.task_id='supervisor'"},
		{"deferring a busy recipient", "active", nil, "UPDATE", "supervisor_messages", "NEW.state='deferred_busy'"},
		{"withholding for a recipient that cannot take it", "", func(t *testing.T, f *stageFixture, host *sendHost) { host.archived = true },
			"UPDATE", "supervisor_messages", attemptEnd},
		{"journaling a withheld send", "", func(t *testing.T, f *stageFixture, host *sendHost) { host.archived = true },
			"INSERT", "journal", journal("supervisor_message_withheld")},
		{"withholding when the settings cannot be read", "", func(t *testing.T, f *stageFixture, _ *sendHost) { settingsUnreadable(f) },
			"UPDATE", "supervisor_messages", attemptEnd},
		{"journaling a send withheld for its settings", "", func(t *testing.T, f *stageFixture, _ *sendHost) { settingsUnreadable(f) },
			"INSERT", "journal", journal("supervisor_message_withheld")},
		{"recording the claim", "", nil, "INSERT", "supervisor_attempts", "NEW.message_id IS NOT NULL"},
		{"holding a message whose claim found it superseded", "", func(t *testing.T, f *stageFixture, _ *sendHost) { dischargeInLinear(t, f) },
			"UPDATE", "supervisor_messages", "NEW.hold_reason='superseded_by_report'"},
		{"starting the transport", "", nil, "UPDATE", "supervisor_attempts", "OLD.transport_started_at IS NULL AND NEW.transport_started_at IS NOT NULL"},
		{"settling the attempt of a hierarchy that moved before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { hierarchyMovesAtTransport(t, f) },
			"UPDATE", "supervisor_attempts", attemptEnd},
		{"settling the message of a hierarchy that moved before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { hierarchyMovesAtTransport(t, f) },
			"UPDATE", "supervisor_messages", toQueued},
		{"journaling a hierarchy that moved before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { hierarchyMovesAtTransport(t, f) },
			"INSERT", "journal", journal("supervisor_message_withheld")},
		{"settling the attempt of a proposal restated before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { proposalRestatedAtTransport(t, f) },
			"UPDATE", "supervisor_attempts", attemptEnd},
		{"settling the message of a proposal restated before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { proposalRestatedAtTransport(t, f) },
			"UPDATE", "supervisor_messages", toQueued},
		{"journaling a proposal restated before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { proposalRestatedAtTransport(t, f) },
			"INSERT", "journal", journal("supervisor_message_withheld")},
		{"settling the attempt of settings that changed before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { settingsChangeAtTransport(f) },
			"UPDATE", "supervisor_attempts", attemptEnd},
		{"settling the message of settings that changed before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { settingsChangeAtTransport(f) },
			"UPDATE", "supervisor_messages", toQueued},
		{"journaling settings that changed before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { settingsChangeAtTransport(f) },
			"INSERT", "journal", journal("supervisor_message_withheld")},
		{"reserving the send budget at the transport", "", nil, "INSERT", "recipient_rate", "NEW.recipient_task_id='supervisor'"},
		{"settling the attempt of a send the budget refused at the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { budgetSpentAtTransport(t, f) },
			"UPDATE", "supervisor_attempts", attemptEnd},
		{"settling the message of a send the budget refused at the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { budgetSpentAtTransport(t, f) },
			"UPDATE", "supervisor_messages", toQueued},
		{"journaling a send the budget refused at the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { budgetSpentAtTransport(t, f) },
			"INSERT", "journal", journal("supervisor_message_paced")},
		{"journaling a proposal superseded before the transport", "", func(t *testing.T, f *stageFixture, _ *sendHost) { proposalSupersededAtTransport(t, f) },
			"INSERT", "journal", journal("supervisor_message_superseded")},
		{"settling the attempt after the send", "", nil, "UPDATE", "supervisor_attempts", "OLD.transport_started_at IS NOT NULL"},
		{"settling the message after the send", "", nil, "UPDATE", "supervisor_messages", "OLD.state='sending' AND NEW.state='dispatched'"},
		{"journaling the attempt after the send", "", nil, "INSERT", "journal", journal("supervisor_message_attempted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, id := charStaged(t)
			status := tc.status
			if status == "" {
				status = "idle"
			}
			host := &sendHost{status: status}
			if tc.scenario != nil {
				tc.scenario(t, f, host)
			}
			abortOn(t, f, tc.event, tc.table, tc.where)
			since := journalSeq(t, f)
			record, err := f.c.Attempt(f.ctx, id, host, charNow)
			var refusal Refusal
			if record != nil || err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), charBoom) {
				t.Fatalf("want the injected error and no record, got record %v err %v", record, err)
			}
			if got := charSummary(t, f, id, since, host); got != charWant[tc.name] {
				t.Fatalf("CASE %q\n got: %s\nwant: %s", tc.name, got, charWant[tc.name])
			}
		})
	}
}

// The proposal can be restated at the transport start only so many times in one call.
func TestAttemptCharacterization_the_third_restatement_in_one_call_refuses_and_sends_nothing(t *testing.T) {
	t.Parallel()
	f, id := charStaged(t)
	host := &sendHost{status: "idle"}
	submission := 1
	f.c.beforeTransport = func() {
		submission++
		reportNumber24(t, f, submission, 10+submission)
	}
	since := journalSeq(t, f)
	record, err := f.c.Attempt(f.ctx, id, host, charNow)
	var refusal Refusal
	if record != nil || !errors.As(err, &refusal) || refusal.Reason != "superseded_revision" || refusal.Detail != "the proposal was restated at its transport start three times in one call and nothing was sent; send it again once what it reports has settled" {
		t.Fatalf("record %v err %v", record, err)
	}
	if got, want := charSummary(t, f, id, since, host), charWant["restatement cap"]; got != want {
		t.Fatalf("CASE \"restatement cap\"\n got: %s\nwant: %s", got, want)
	}
}

// A refreshProposal error at the transport start that is not a superseded_revision refusal cancels
// the claim with the error's own text as the reason, and is returned as it is.
func TestAttemptCharacterization_a_plain_error_refreshing_the_proposal_at_the_transport_cancels_the_claim(t *testing.T) {
	t.Parallel()
	f, id := charStaged(t)
	host := &sendHost{status: "idle"}
	proposalRestatedAtTransport(t, f)
	abortOn(t, f, "UPDATE OF packet", "supervisor_messages", "OLD.state='sending'")
	since := journalSeq(t, f)
	record, err := f.c.Attempt(f.ctx, id, host, charNow)
	var refusal Refusal
	if record != nil || err == nil || errors.As(err, &refusal) || !strings.Contains(err.Error(), charBoom) {
		t.Fatalf("record %v err %v", record, err)
	}
	attempts, attemptsErr := f.s.SupervisorAttempts(f.ctx, id)
	if attemptsErr != nil || len(attempts) != 1 || !strings.Contains(attempts[0].Record, charBoom) || !strings.Contains(attempts[0].Record, "\"reason\"") {
		t.Fatalf("attempts %+v %v", attempts, attemptsErr)
	}
	if got, want := charSummary(t, f, id, since, host), charWant["refresh error at transport"]; got != want {
		t.Fatalf("CASE \"refresh error at transport\"\n got: %s\nwant: %s", got, want)
	}
}

// holdObsoleteClaim compares the row to the one the attempt read and does nothing when they differ:
// the claim's refusal comes back, but no hold is set and nothing is journaled.
func TestAttemptCharacterization_a_row_that_moved_after_the_attempt_read_it_is_not_held_as_superseded(t *testing.T) {
	t.Parallel()
	f, id := charStaged(t)
	inner := &sendHost{status: "idle"}
	host := &charHost{sendHost: inner}
	host.onRead = func() {
		dischargeInLinear(t, f)
		sweepExec(t, f, "UPDATE supervisor_messages SET attempt_count=attempt_count+1")
		host.onRead = nil
	}
	since := journalSeq(t, f)
	record, err := f.c.Attempt(f.ctx, id, host, charNow)
	var refusal Refusal
	if record != nil || !errors.As(err, &refusal) || refusal.Reason != "superseded_revision" || !strings.Contains(refusal.Detail, "Linear record") {
		t.Fatalf("record %v err %v", record, err)
	}
	if got, want := charSummary(t, f, id, since, inner), charWant["moved row not held"]; got != want {
		t.Fatalf("CASE \"moved row not held\"\n got: %s\nwant: %s", got, want)
	}
}

// charWant is what each case above left behind, recorded by running the cases on the unsplit code.
var charWant = map[string]string{
	"recovering a stranded send":                                          "message=sending hold=- next=- lease=gone count=1 | journal=[] sends=0",
	"reopening a superseded hold":                                         "message=queued hold=superseded_by_report next=- lease=- count=0 | journal=[] sends=0",
	"reopening an unaddressed hold":                                       "message=queued hold=hierarchy_unresolved next=- lease=- count=0 | journal=[] sends=0",
	"holding a message whose hierarchy no longer resolves":                "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"holding a message whose owner drifted":                               "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"recording the recipient's lifecycle":                                 "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"deferring a busy recipient":                                          "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"withholding for a recipient that cannot take it":                     "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"journaling a withheld send":                                          "message=withheld_pre_send hold=- next=1700000060 lease=- count=0 | journal=[] sends=0",
	"withholding when the settings cannot be read":                        "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"journaling a send withheld for its settings":                         "message=withheld_pre_send hold=- next=1700000060 lease=- count=0 | journal=[] sends=0",
	"recording the claim":                                                 "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"holding a message whose claim found it superseded":                   "message=queued hold=- next=- lease=- count=0 | journal=[] sends=0",
	"starting the transport":                                              "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the attempt of a hierarchy that moved before the transport": "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the message of a hierarchy that moved before the transport": "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"journaling a hierarchy that moved before the transport":              "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the attempt of a proposal restated before the transport":    "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the message of a proposal restated before the transport":    "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"journaling a proposal restated before the transport":                 "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the attempt of settings that changed before the transport":  "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the message of settings that changed before the transport":  "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"journaling settings that changed before the transport":               "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"reserving the send budget at the transport":                          "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the attempt of a send the budget refused at the transport":  "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the message of a send the budget refused at the transport":  "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"journaling a send the budget refused at the transport":               "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"journaling a proposal superseded before the transport":               "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=false | journal=[] sends=0",
	"settling the attempt after the send":                                 "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=true | journal=[] sends=1",
	"settling the message after the send":                                 "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=true | journal=[] sends=1",
	"journaling the attempt after the send":                               "message=sending hold=- next=- lease=relay count=1 | a1 held_uncertain sent=unknown safe=0 started=true | journal=[] sends=1",
	"restatement cap":                                                     "message=queued hold=- next=- lease=- count=3 | a1 withheld_pre_send sent=no safe=1 started=false | a2 withheld_pre_send sent=no safe=1 started=false | a3 withheld_pre_send sent=no safe=1 started=false | journal=[supervisor_message_restated,supervisor_message_withheld,supervisor_message_restated,supervisor_message_withheld,supervisor_message_restated,supervisor_message_withheld] sends=0",
	"refresh error at transport":                                          "message=queued hold=- next=- lease=- count=1 | a1 withheld_pre_send sent=no safe=1 started=false | journal=[] sends=0",
	"moved row not held":                                                  "message=queued hold=- next=- lease=- count=1 | journal=[] sends=0",
}
