package supervisor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1486 fix round 2 (P1): an event-path completion parked by the recipient's acknowledgement is
// staged again by every daemon staging visit of its project while it is owed. Staging the same
// statement again must not release a park the recipient set; a new statement of the obligation (a
// corrected or resubmitted work report) is new content and still releases it.

// acknowledgedCompletion stages the fixture's event completion to "supervisor" and parks it with the
// bulk acknowledgement; it returns the message id.
func acknowledgedCompletion(t *testing.T, f *stageFixture) string {
	t.Helper()
	_, staged := f.staged(t)
	id, _ := staged["messageId"].(string)
	answer, err := f.c.Acknowledge(f.ctx, AcknowledgeRequest{Recipient: "supervisor", Actor: "supervisor", AllInformational: true, At: f.at})
	if err != nil {
		t.Fatal(err)
	}
	if parked, _ := answer["parked"].([]string); len(parked) != 1 || parked[0] != id {
		t.Fatalf("the acknowledgement parked %v, want %s", answer["parked"], id)
	}
	return id
}

func reopenedCount(t *testing.T, f *stageFixture, id string) int {
	t.Helper()
	var n int
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_message_reopened' AND subject=?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAcknowledgedCompletionStaysParkedThroughAStagingPass(t *testing.T) {
	f := fixture24(t)
	id := acknowledgedCompletion(t, f)
	for pass := range 2 {
		answer, err := f.c.StageUnsent(f.ctx, "PRJ-1", "2023-11-14T22:14:20.000000+00:00", 300)
		if err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.HoldReason.String != store.SupervisorHoldSuperseded || !row.Unsent() || row.ClaimableAt(1_900_000_000) {
			t.Fatalf("staging pass %d released the acknowledged park: state %s hold %q (%d staged)", pass, row.State, row.HoldReason.String, len(answer["staged"].([]any)))
		}
	}
	if n := reopenedCount(t, f, id); n != 0 {
		t.Fatalf("the staging passes journaled %d reopen(s) of the acknowledged message", n)
	}
	// The daemon's count of what a pass staged does not count the parked message either.
	if staged, err := f.c.stageUnsent(f.ctx, "PRJ-1", "2023-11-14T22:15:20.000000+00:00"); err != nil || staged != 0 {
		t.Fatalf("the daemon staging visit counted %d staged (%v)", staged, err)
	}
	// Whole daemon supervisor passes (staging visit, then sends) open no turn for it.
	f.c.Settings = &delivery.TaskSettings{}
	host := newGuardHost()
	for pass := range 3 {
		if _, err := f.c.AutoSend(f.ctx, host, guardNow+float64(60*pass), 10, 10, "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if host.sends != 0 || row.HoldReason.String != store.SupervisorHoldSuperseded {
		t.Fatalf("the daemon passes sent %d and left the message held %q", host.sends, row.HoldReason.String)
	}
}

func TestAcknowledgedCompletionIsReleasedByANewStatement(t *testing.T) {
	f := fixture24(t)
	id := acknowledgedCompletion(t, f)
	// The child resubmits its work report: the obligation is stated again with new content.
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:14:00.000000+00:00')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.StageUnsent(f.ctx, "PRJ-1", "2023-11-14T22:14:20.000000+00:00", 300); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.HoldReason.Valid || !row.Unsent() || !row.SubmissionNo.Valid || row.SubmissionNo.Int64 != 2 {
		t.Fatalf("a resubmitted report did not release and restate the parked completion: state %s hold %q submission %v", row.State, row.HoldReason.String, row.SubmissionNo)
	}
}

// A superseded hold the channel derived itself (no acknowledgement) is released by staging what is
// owed again, as before.
func TestAChannelSupersededHoldIsStillReleasedByStaging(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id, _ := staged["messageId"].(string)
	if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", store.SupervisorHoldSuperseded, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_superseded',?,'{"requestId": "r-1", "detail": "what is owed moved"}')`, f.at, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.StageUnsent(f.ctx, "PRJ-1", "2023-11-14T22:14:20.000000+00:00", 300); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.HoldReason.Valid || reopenedCount(t, f, id) != 1 {
		t.Fatalf("a derived superseded hold was not released by staging: state %s hold %q", row.State, row.HoldReason.String)
	}
}

// Sending the parked message by id (supervisor-send --message) is the operator's own release: the
// attempt re-derives the hold and sends what is still owed.
func TestAnAcknowledgedCompletionSentByIdGoesOut(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	id := acknowledgedCompletion(t, f)
	if _, err := f.c.StageUnsent(f.ctx, "PRJ-1", "2023-11-14T22:14:20.000000+00:00", 300); err != nil {
		t.Fatal(err)
	}
	host := newGuardHost()
	record, err := f.c.Attempt(f.ctx, id, host, guardNow)
	if err != nil {
		t.Fatal(err)
	}
	if record == nil || host.sends != 1 {
		t.Fatalf("the parked message sent by id: record %v sends %d", record, host.sends)
	}
}

// A direct caller that names no time gets the channel's clock, as every other channel writer does:
// no row or journal entry is written with an empty time.
func TestAcknowledgeWithNoTimeWritesTheChannelsClock(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id, _ := staged["messageId"].(string)
	if _, err := f.c.Acknowledge(f.ctx, AcknowledgeRequest{Recipient: "supervisor", Actor: "supervisor", Messages: []string{id}}); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var at string
	if err := f.s.DB.QueryRowContext(f.ctx, "SELECT at FROM journal WHERE kind='supervisor_message_superseded' AND subject=? ORDER BY rowid DESC LIMIT 1", id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if row.HoldReason.String != store.SupervisorHoldSuperseded || row.UpdatedAt == "" || at == "" {
		t.Fatalf("an acknowledgement with no time wrote updated_at %q and journal at %q (hold %q)", row.UpdatedAt, at, row.HoldReason.String)
	}
}

// The hold a message carries was set by the newest supervisor_message_superseded row of it, not the
// first: a message parked by an acknowledgement, released by a resubmission, held again by the
// channel (its own journal row) and released by a pass, then acknowledged again, is kept through the
// next pass only as that second acknowledgement; the derived hold in between was released.
func TestAcknowledgedParkIsReadFromTheNewestSupersededRow(t *testing.T) {
	f := fixture24(t)
	id := acknowledgedCompletion(t, f)
	hold := func() string {
		t.Helper()
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row.HoldReason.String
	}
	pass := func(at string) {
		t.Helper()
		if _, err := f.c.StageUnsent(f.ctx, "PRJ-1", at, 300); err != nil {
			t.Fatal(err)
		}
	}
	// A resubmitted report is a new statement: the pass releases the acknowledgement's park.
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:14:00.000000+00:00')`); err != nil {
		t.Fatal(err)
	}
	pass("2023-11-14T22:14:20.000000+00:00")
	if got := hold(); got != "" {
		t.Fatalf("the resubmission left the park: held %q", got)
	}
	// The channel derives a superseded hold of its own, journaled as it journals one.
	if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", store.SupervisorHoldSuperseded, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DB.ExecContext(f.ctx, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:15:00.000000+00:00','supervisor_message_superseded',?,'{"requestId": "r-1", "detail": "what is owed moved"}')`, id); err != nil {
		t.Fatal(err)
	}
	pass("2023-11-14T22:15:20.000000+00:00")
	if got := hold(); got != "" {
		t.Fatalf("a derived hold after an earlier acknowledgement was read as that acknowledgement and kept: held %q", got)
	}
	// A second acknowledgement is kept through the next pass.
	if _, err := f.c.Acknowledge(f.ctx, AcknowledgeRequest{Recipient: "supervisor", Actor: "supervisor", Messages: []string{id}, At: "2023-11-14T22:16:00.000000+00:00"}); err != nil {
		t.Fatal(err)
	}
	pass("2023-11-14T22:16:20.000000+00:00")
	if got := hold(); got != store.SupervisorHoldSuperseded {
		t.Fatalf("the second acknowledgement was released by a pass: held %q", got)
	}
}

// An acknowledged omission staged again with a reading that disagrees with the one it froze is
// refused as contradictory_observation, as it is when it carries no park; the park does not answer
// for a reading that contradicts the message.
func TestAnAcknowledgedOmissionStillRefusesAContradictoryReading(t *testing.T) {
	f := fixture24(t)
	base := omissionReading24(f)
	o := ObservationObligation(base)
	staged, err := f.c.StageWithReading(f.ctx, *o, base, "", f.at)
	if err != nil || staged["staged"] != true {
		t.Fatalf("the omission was not staged: %v %v", staged, err)
	}
	id, _ := staged["messageId"].(string)
	if answer, err := f.c.Acknowledge(f.ctx, AcknowledgeRequest{Recipient: "supervisor", Actor: "supervisor", Messages: []string{id}, At: f.at}); err != nil || len(answer["parked"].([]string)) != 1 {
		t.Fatalf("the omission was not parked: %v %v", answer, err)
	}
	raw, _ := json.Marshal(base)
	var changed map[string]any
	_ = json.Unmarshal(raw, &changed)
	changed["selectors"].(map[string]any)["session"] = "another"
	_, err = f.c.StageWithReading(f.ctx, *o, changed, "", f.at)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "contradictory_observation" || !strings.Contains(refusal.Detail, "keeps the reading it froze") {
		t.Fatalf("an acknowledged omission staged with a contradicting reading: %v", err)
	}
	// The same reading again is kept parked.
	again, err := f.c.StageWithReading(f.ctx, *o, base, "", f.at)
	if err != nil || again["reason"] != acknowledgedKeptReason {
		t.Fatalf("the same reading again: %v %v", again, err)
	}
}
