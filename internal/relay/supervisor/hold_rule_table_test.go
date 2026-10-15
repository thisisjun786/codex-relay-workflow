package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The hold rule of the supervisor channel (CRW-1500) in two tables.
//
// TestHoldDecisionTable is the pure rule, holdDecision, written out cell by cell: every entry path
// against every hold kind, after every change that matters to it, with what owed answers where the
// cell asks it. The verdict of each row is literal, never recomputed from the rule.
//
// TestSupervisorHoldRuleTable is the same rule over a real store: a row stages one message, sets
// its hold, applies its change, runs its entry, runs one daemon supervisor pass (not for a fault
// notice, which its own deliverer sends) and reads whether the message still carries the hold and
// whether a turn was opened for it. Its rows come with the units that turn them green.

// tri is a boolean column of the decision table: yes, no, or either (the row holds for both).
type tri int

const (
	tY tri = iota
	tN
	tA
)

func (v tri) values() []bool {
	switch v {
	case tY:
		return []bool{true}
	case tN:
		return []bool{false}
	}
	return []bool{false, true}
}

// owedAnswer is what the owed question answers in a row. owedUnasked: the cell must not ask it.
type owedAnswer string

const (
	owedUnasked owedAnswer = "-"
	owedTrue    owedAnswer = "true"
	owedFalse   owedAnswer = "false"
	owedError   owedAnswer = "error"
)

// anyWord is the SetWord column of a row that holds for every word.
const anyWord = "*"

type holdDecisionRow struct {
	entry      holdEntry
	held       holdKind
	stmt, rcpt tri
	sndr       tri
	setWord    string
	owed       owedAnswer
	want       holdVerdict
	wantErr    bool
	because    string // a phrase the answer must name, when the row pins one
}

// The table of plan-r3 section 2. Rows are grouped by entry; within an entry by the change, then by
// the hold kind in the order none, other, derived, acknowledged, notice.
var holdDecisionRows = []holdDecisionRow{
	// acknowledge, notice_park, derive: set only from no hold.
	{entryAcknowledge, holdNone, tA, tA, tA, anyWord, owedUnasked, holdSet, false, ""},
	{entryAcknowledge, holdOther, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryAcknowledge, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryAcknowledge, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryAcknowledge, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryNoticePark, holdNone, tA, tA, tA, anyWord, owedUnasked, holdSet, false, ""},
	{entryNoticePark, holdOther, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryNoticePark, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryNoticePark, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryNoticePark, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDerive, holdNone, tA, tA, tA, anyWord, owedUnasked, holdSet, false, ""},
	{entryDerive, holdOther, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDerive, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDerive, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDerive, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},

	// defer (deferMessage, deferBusy): writes its word (a cap, superseded through derive, or none)
	// only on a message with no hold, and writes nothing on a held one.
	{entryDefer, holdNone, tA, tA, tA, anyWord, owedUnasked, holdSet, false, ""},
	{entryDefer, holdOther, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDefer, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDefer, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryDefer, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},

	// claim: only a message with no hold is claimable.
	{entryClaim, holdNone, tA, tA, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryClaim, holdOther, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryClaim, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryClaim, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},
	{entryClaim, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdRefuse, false, ""},

	// settle: sets the word asked for on a message with no hold, and keeps any hold present.
	{entrySettle, holdNone, tA, tA, tA, store.SupervisorHoldSuperseded, owedUnasked, holdSet, false, ""},
	{entrySettle, holdNone, tA, tA, tA, "busy_cap", owedUnasked, holdSet, false, ""},
	{entrySettle, holdNone, tA, tA, tA, "", owedUnasked, holdKeep, false, ""},
	{entrySettle, holdOther, tA, tA, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entrySettle, holdDerived, tA, tA, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entrySettle, holdAcknowledged, tA, tA, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entrySettle, holdNotice, tA, tA, tA, anyWord, owedUnasked, holdKeep, false, ""},

	// readdress, the recipient moved: every hold is released (as dev).
	{entryReaddress, holdNone, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryReaddress, holdOther, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdDerived, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdAcknowledged, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdNotice, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	// readdress, the sender moved only: an acknowledgement's park is kept (P1-3); the rest as dev.
	{entryReaddress, holdNone, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, ""},
	{entryReaddress, holdOther, tA, tY, tN, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdDerived, tA, tY, tN, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdAcknowledged, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, acknowledgedKeptReason},
	{entryReaddress, holdNotice, tA, tY, tN, anyWord, owedUnasked, holdRelease, false, ""},
	// readdress with both endpoints unchanged is answered as a sender-only move.
	{entryReaddress, holdNone, tA, tY, tY, anyWord, owedUnasked, holdKeep, false, ""},
	{entryReaddress, holdOther, tA, tY, tY, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdDerived, tA, tY, tY, anyWord, owedUnasked, holdRelease, false, ""},
	{entryReaddress, holdAcknowledged, tA, tY, tY, anyWord, owedUnasked, holdKeep, false, acknowledgedKeptReason},
	{entryReaddress, holdNotice, tA, tY, tY, anyWord, owedUnasked, holdRelease, false, ""},

	// restage, the recipient moved: the superseded holds are released (re-addressed); another word
	// is kept here, its re-address releases it.
	{entryRestage, holdNone, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryRestage, holdOther, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryRestage, holdDerived, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	{entryRestage, holdAcknowledged, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	{entryRestage, holdNotice, tA, tN, tA, anyWord, owedUnasked, holdRelease, false, ""},
	// restage, the same recipient (whatever the sender session).
	{entryRestage, holdNone, tA, tY, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryRestage, holdOther, tA, tY, tA, anyWord, owedUnasked, holdKeep, false, ""},
	{entryRestage, holdDerived, tA, tY, tA, anyWord, owedTrue, holdRelease, false, ""},
	{entryRestage, holdDerived, tA, tY, tA, anyWord, owedFalse, holdKeep, false, ""},
	{entryRestage, holdDerived, tA, tY, tA, anyWord, owedError, holdKeep, true, ""},
	{entryRestage, holdAcknowledged, tY, tY, tA, anyWord, owedUnasked, holdKeep, false, acknowledgedKeptReason},
	{entryRestage, holdAcknowledged, tN, tY, tA, anyWord, owedTrue, holdRelease, false, ""},
	{entryRestage, holdAcknowledged, tN, tY, tA, anyWord, owedFalse, holdKeep, false, ""},
	{entryRestage, holdAcknowledged, tN, tY, tA, anyWord, owedError, holdKeep, true, ""},
	{entryRestage, holdNotice, tA, tY, tA, anyWord, owedUnasked, holdRelease, false, ""},

	// send_by_id, the recipient or the sender moved: never re-addressed; drift names supervisor-stage.
	{entrySendByID, holdNone, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdOther, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdDerived, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdAcknowledged, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdNotice, tA, tN, tA, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdNone, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdOther, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdDerived, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdAcknowledged, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	{entrySendByID, holdNotice, tA, tY, tN, anyWord, owedUnasked, holdKeep, false, sendByIDDriftReason},
	// send_by_id, the same endpoints: a superseded hold is released while the message is owed.
	{entrySendByID, holdNone, tA, tY, tY, anyWord, owedUnasked, holdKeep, false, ""},
	{entrySendByID, holdOther, tA, tY, tY, anyWord, owedUnasked, holdKeep, false, ""},
	{entrySendByID, holdDerived, tA, tY, tY, anyWord, owedTrue, holdRelease, false, ""},
	{entrySendByID, holdDerived, tA, tY, tY, anyWord, owedFalse, holdKeep, false, ""},
	{entrySendByID, holdDerived, tA, tY, tY, anyWord, owedError, holdKeep, true, ""},
	{entrySendByID, holdAcknowledged, tA, tY, tY, anyWord, owedTrue, holdRelease, false, ""},
	{entrySendByID, holdAcknowledged, tA, tY, tY, anyWord, owedFalse, holdKeep, false, ""},
	{entrySendByID, holdAcknowledged, tA, tY, tY, anyWord, owedError, holdKeep, true, ""},
	{entrySendByID, holdNotice, tA, tY, tY, anyWord, owedTrue, holdRelease, false, ""},
	{entrySendByID, holdNotice, tA, tY, tY, anyWord, owedFalse, holdKeep, false, ""},
	{entrySendByID, holdNotice, tA, tY, tY, anyWord, owedError, holdKeep, true, ""},
}

var (
	holdDecisionEntries = []holdEntry{entryAcknowledge, entryNoticePark, entryDerive, entryDefer, entryClaim, entrySettle, entryReaddress, entryRestage, entrySendByID}
	holdDecisionKinds   = []holdKind{holdNone, holdOther, holdDerived, holdAcknowledged, holdNotice}
	// holdDecisionWords are the SetWord values a row of anyWord stands for.
	holdDecisionWords = []string{"", store.SupervisorHoldSuperseded, "busy_cap"}
	errOwedProbe      = errors.New("owed probe failed")
)

// TestHoldDecisionTable checks each row of the table, for every value of its either columns, and
// that the rows cover every entry × kind × statement × recipient × sender × word exactly once (an
// ifOwed cell by its three owed rows, any other cell by one row that owed must not be asked in).
func TestHoldDecisionTable(t *testing.T) {
	type cell struct {
		entry            holdEntry
		held             holdKind
		stmt, rcpt, sndr bool
		word             string
	}
	covered := map[cell][]owedAnswer{}
	for i, row := range holdDecisionRows {
		words := []string{row.setWord}
		if row.setWord == anyWord {
			words = holdDecisionWords
		}
		for _, stmt := range row.stmt.values() {
			for _, rcpt := range row.rcpt.values() {
				for _, sndr := range row.sndr.values() {
					for _, word := range words {
						c := cell{row.entry, row.held, stmt, rcpt, sndr, word}
						covered[c] = append(covered[c], row.owed)
						name := fmt.Sprintf("%02d/%s/%s/stmt=%v,rcpt=%v,sndr=%v,word=%q/owed=%s", i, row.entry, row.held, stmt, rcpt, sndr, word, row.owed)
						t.Run(name, func(t *testing.T) {
							asked := 0
							owed := func() (bool, error) {
								asked++
								switch row.owed {
								case owedTrue:
									return true, nil
								case owedFalse:
									return false, nil
								case owedError:
									return false, errOwedProbe
								}
								t.Error("owed was asked in a cell that must not ask it")
								return true, nil
							}
							got, because, err := holdDecision(row.held, holdChange{Entry: row.entry, SameStatement: stmt, SameRecipient: rcpt, SameSender: sndr, SetWord: word}, owed)
							if got != row.want {
								t.Fatalf("verdict %q (%s), want %q", got, because, row.want)
							}
							if row.wantErr != (err != nil) || (row.wantErr && !errors.Is(err, errOwedProbe)) {
								t.Fatalf("error %v, want error %v", err, row.wantErr)
							}
							if wantAsked := row.owed != owedUnasked; (asked == 1) != wantAsked || asked > 1 {
								t.Fatalf("owed asked %d times, want asked %v", asked, wantAsked)
							}
							if err == nil && because == "" {
								t.Fatal("the answer names no reason")
							}
							if row.because != "" && because != row.because {
								t.Fatalf("reason %q, want %q", because, row.because)
							}
						})
					}
				}
			}
		}
	}
	for _, entry := range holdDecisionEntries {
		for _, held := range holdDecisionKinds {
			for _, stmt := range tA.values() {
				for _, rcpt := range tA.values() {
					for _, sndr := range tA.values() {
						for _, word := range holdDecisionWords {
							c := cell{entry, held, stmt, rcpt, sndr, word}
							got := covered[c]
							one := len(got) == 1 && got[0] == owedUnasked
							three := len(got) == 3 && strings.Join([]string{string(got[0]), string(got[1]), string(got[2])}, ",") == "true,false,error"
							if !one && !three {
								t.Errorf("cell %+v is covered by %v, want one unasked row or the rows true,false,error", c, got)
							}
						}
					}
				}
			}
		}
	}
}

// TestHoldDecisionRefusesWhatItDoesNotKnow: an ifOwed cell asked without an owed question, an entry
// the rule does not know and a kind it does not know are refused with an error, never released.
func TestHoldDecisionRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		held   holdKind
		change holdChange
		want   holdVerdict
	}{
		{"ifOwed cell without owed", holdDerived, holdChange{Entry: entryRestage, SameRecipient: true, SameSender: true}, holdKeep},
		{"unknown entry", holdDerived, holdChange{Entry: "unstage", SameRecipient: true, SameSender: true}, holdRefuse},
		{"unknown kind", "parked", holdChange{Entry: entryRestage, SameRecipient: true, SameSender: true}, holdRefuse},
		{"unknown kind on a set", "parked", holdChange{Entry: entryAcknowledge}, holdRefuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := holdDecision(tc.held, tc.change, nil)
			if got != tc.want || err == nil {
				t.Fatalf("verdict %q err %v, want %q with an error", got, err, tc.want)
			}
		})
	}
}

// TestHoldKindOfReadsTheNewestSupersededRow: the kind of a stored hold is read from its word, its
// obligation kind and the newest supervisor_message_superseded journal row of the message only.
func TestHoldKindOfReadsTheNewestSupersededRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		world string
		set   func(t *testing.T, w *holdWorld)
		want  holdKind
	}{
		{"no hold", "event", func(*testing.T, *holdWorld) {}, holdNone},
		{"another word", "event", func(t *testing.T, w *holdWorld) { w.setHoldSQL(t, store.SupervisorHoldUnaddressed, "") }, holdOther},
		{"a cap word", "event", func(t *testing.T, w *holdWorld) { w.setHoldSQL(t, "busy_cap", "") }, holdOther},
		{"derived by the channel", "event", func(t *testing.T, w *holdWorld) { w.derive(t) }, holdDerived},
		{"superseded with no journal row (an older store)", "event", func(t *testing.T, w *holdWorld) {
			w.exec(t, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", store.SupervisorHoldSuperseded, w.id)
		}, holdDerived},
		{"acknowledged", "event", func(t *testing.T, w *holdWorld) { w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand) }, holdAcknowledged},
		{"acknowledged on a direct message", "direct", func(t *testing.T, w *holdWorld) {
			w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand)
		}, holdAcknowledged},
		{"acknowledged, released, then derived (P1-2: the newest row)", "event", func(t *testing.T, w *holdWorld) {
			w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand)
			w.exec(t, "UPDATE supervisor_messages SET hold_reason=NULL WHERE message_id=?", w.id)
			w.exec(t, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:45.000000+00:00','supervisor_message_reopened',?,'{"proposal": "current"}')`, w.id)
			w.derive(t)
		}, holdDerived},
		{"derived, released, then acknowledged", "event", func(t *testing.T, w *holdWorld) {
			w.derive(t)
			w.exec(t, "UPDATE supervisor_messages SET hold_reason=NULL WHERE message_id=?", w.id)
			w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand)
		}, holdAcknowledged},
		{"another kind's journal row after the acknowledgement", "event", func(t *testing.T, w *holdWorld) {
			w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand)
			w.exec(t, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:50.000000+00:00','supervisor_message_restated',?,'{"by": "someone"}')`, w.id)
		}, holdAcknowledged},
		{"another message's acknowledgement", "event", func(t *testing.T, w *holdWorld) {
			w.derive(t)
			w.exec(t, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:50.000000+00:00','supervisor_message_superseded','another-message',?)`, `{"by": "`+AcknowledgeCommand+`"}`)
		}, holdDerived},
		{"a superseded row by someone else", "event", func(t *testing.T, w *holdWorld) { w.setHoldSQL(t, store.SupervisorHoldSuperseded, "supervisor-send") }, holdDerived},
		{"a superseded row whose detail is not an object", "event", func(t *testing.T, w *holdWorld) {
			w.exec(t, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", store.SupervisorHoldSuperseded, w.id)
			w.exec(t, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:50.000000+00:00','supervisor_message_superseded',?,'[1]')`, w.id)
		}, holdDerived},
		{"a fault notice's park", "notice", func(t *testing.T, w *holdWorld) { w.setHold(t, "notice") }, holdNotice},
		{"a fault notice with an acknowledgement row", "notice", func(t *testing.T, w *holdWorld) {
			w.setHoldSQL(t, store.SupervisorHoldSuperseded, AcknowledgeCommand)
		}, holdNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newHoldWorld(t, tc.world)
			tc.set(t, w)
			row, err := w.c.Get(w.ctx, w.id)
			if err != nil {
				t.Fatal(err)
			}
			got, err := holdKindOf(w.ctx, w.s, row)
			if err != nil || got != tc.want {
				t.Fatalf("kind %q err %v, want %q", got, err, tc.want)
			}
		})
	}
}

// TestAcknowledgeFormsAndTheirAnswers pins the request forms of the acknowledgement around the rows
// of the table: exactly one form, a message named by id that the store does not hold or that another
// recipient owns is left with its reason, and the bulk form parks the recipient's informational
// messages staged before its bound and nothing staged after it.
func TestAcknowledgeFormsAndTheirAnswers(t *testing.T) {
	w := newHoldWorld(t, "event")
	for _, req := range []AcknowledgeRequest{
		{Recipient: w.recipient()},
		{Recipient: w.recipient(), AllInformational: true, Messages: []string{w.id}},
	} {
		var refusal Refusal
		if _, err := w.c.Acknowledge(w.ctx, req); !errors.As(err, &refusal) || refusal.Reason != "malformed_receipt" {
			t.Fatalf("%+v answered %v, want a malformed_receipt refusal", req, err)
		}
	}
	answer, err := w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: "someone-else", Actor: "someone-else", Messages: []string{w.id, "no-such-message", w.id}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(answer["parked"], answer["skipped"]); got != "[] [map[messageId:"+w.id+" reason:"+AcknowledgeOtherRecipient+"] map[messageId:no-such-message reason:"+AcknowledgeNotFound+"]]" {
		t.Fatalf("answered %s", got)
	}
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := time.Parse(time.RFC3339Nano, row.StagedAt)
	if err != nil {
		t.Fatal(err)
	}
	early := staged.Add(-time.Second)
	answer, err = w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: w.recipient(), Actor: w.recipient(), AllInformational: true, StagedBefore: &early})
	if err != nil || len(answer["parked"].([]string)) != 0 || len(answer["skipped"].([]any)) != 0 || w.supersededRows(t) != 0 {
		t.Fatalf("the bulk form bounded before the staging answered %v %v", answer, err)
	}
	late := staged.Add(time.Second)
	answer, err = w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: w.recipient(), Actor: w.recipient(), AllInformational: true, StagedBefore: &late})
	if err != nil || fmt.Sprint(answer["parked"]) != "["+w.id+"]" {
		t.Fatalf("the bulk form answered %v %v, want %s parked", answer, err, w.id)
	}
	if row, err = w.c.Get(w.ctx, w.id); err != nil {
		t.Fatal(err)
	}
	if kind, err := holdKindOf(w.ctx, w.s, row); err != nil || kind != holdAcknowledged {
		t.Fatalf("the bulk park reads as %q (%v), want acknowledged", kind, err)
	}
	var detail string
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT detail FROM journal WHERE kind='supervisor_message_superseded' AND subject=? ORDER BY seq DESC LIMIT 1", w.id).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if want := `{"by": "` + AcknowledgeCommand + `", "actor": "` + w.recipient() + `", "reason": "` + acknowledgeReason + `"}`; detail != want {
		t.Fatalf("the acknowledgement journaled %s, want %s", detail, want)
	}
	answer, err = w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: w.recipient(), Actor: w.recipient(), AllInformational: true})
	if err != nil || fmt.Sprint(answer["parked"], answer["skipped"]) != "[] [map[messageId:"+w.id+" reason:"+AcknowledgeHeld+"]]" || w.supersededRows(t) != 1 {
		t.Fatalf("a second bulk acknowledgement answered %v %v, want the message left as held", answer, err)
	}
}

// TestHoldWorldChangesMoveWhatTheyName pins the world the store-backed table builds on: each world
// stages one unsent message addressed to its recipient, and each change of the hierarchy moves only
// what it names.
func TestHoldWorldChangesMoveWhatTheyName(t *testing.T) {
	for _, tc := range []struct{ world, change string }{
		{"event", "other_sender"}, {"event", "other_recipient"},
		{"direct", "other_sender"}, {"direct", "other_recipient"},
	} {
		t.Run(tc.world+"/"+tc.change, func(t *testing.T) {
			w := newHoldWorld(t, tc.world)
			row, err := w.c.Get(w.ctx, w.id)
			if err != nil || row.RecipientTaskID != w.recipient() || !store.SupervisorUnsent(row.State) {
				t.Fatalf("staged %s to %s (%s): %v, want an unsent message to %s", row.MessageID, row.RecipientTaskID, row.State, err, w.recipient())
			}
			w.apply(t, tc.change)
		})
	}
}

// holdRuleRow is one row of the store-backed table. answer, when the row pins it, is what its entry
// answered: for acknowledge, parked or the reason the message was left.
type holdRuleRow struct {
	name                string
	world, hold, change string
	entry               string
	held, turn          bool
	answer              string
}

// acknowledgeParked is the answer of an acknowledge entry that parked the message.
const acknowledgeParked = "parked"

// holdRuleRows are filled by the units that bring their behaviour (plan-r3 section 4).
var holdRuleRows = []holdRuleRow{
	// acknowledge (U11): parks only an unsent message with no hold, nothing of which can have gone,
	// that is not a decision and not a fault notice.
	{"acknowledge/none", "direct", "none", "same", "acknowledge", true, false, acknowledgeParked},
	{"acknowledge/held", "event", "derived", "same", "acknowledge", false, true, AcknowledgeHeld},
	{"acknowledge/sending", "event", "none", "sending", "acknowledge", false, false, AcknowledgeClaimed},
	{"acknowledge/decision", "event", "none", "decision", "acknowledge", false, true, AcknowledgeAnswerOwed},
	{"acknowledge/notice", "notice", "none", "same", "acknowledge", false, false, AcknowledgeFaultNotice},
}

func TestSupervisorHoldRuleTable(t *testing.T) {
	for _, row := range holdRuleRows {
		t.Run(row.name, func(t *testing.T) {
			w := newHoldWorld(t, row.world)
			w.setHold(t, row.hold)
			w.apply(t, row.change)
			w.enter(t, row.entry)
			if row.answer != "" && w.answer != row.answer {
				t.Fatalf("%s/%s/%s/%s answered %q, want %q", row.world, row.hold, row.change, row.entry, w.answer, row.answer)
			}
			w.pass(t)
			held, turn := w.read(t)
			if held != row.held || turn != row.turn {
				t.Fatalf("%s/%s/%s/%s: held %v turn %v, want held %v turn %v", row.world, row.hold, row.change, row.entry, held, turn, row.held, row.turn)
			}
		})
	}
}

// holdWorld is one message in one world, the host its turns are opened on (a turn is an attempt of
// the message that sent), and the clock of its daemon passes. Worlds: event (an event completion,
// fixture24), direct (a lead report to a management seat), notice (a fault notice, newNoticeWorld).
type holdWorld struct {
	kind   string
	ctx    context.Context
	s      *store.Store
	c      *Channel
	id     string
	host   *guardHost
	now    float64
	f      *stageFixture
	notice *noticeWorld
	direct *storeRecipientWorld
	text   string
	answer string // what the last entry answered, when it answers (acknowledge: parked or why not)
}

const holdRuleLead = "01leadNext"

func newHoldWorld(t *testing.T, kind string) *holdWorld {
	t.Helper()
	w := &holdWorld{kind: kind, host: newGuardHost(), now: guardNow}
	switch kind {
	case "event":
		w.f = fixture24(t)
		_, staged := w.f.staged(t)
		w.id, _ = staged["messageId"].(string)
		w.ctx, w.s, w.c = w.f.ctx, w.f.s, w.f.c
	case "direct":
		w.direct = newStoreRecipientWorld(t)
		w.direct.superviseInitiative()
		w.direct.manage(directManagement, directManagement, storeRecipientInitiative, storeRecipientSupervisor)
		w.ctx, w.s, w.c = w.direct.ctx, w.direct.s, w.direct.channel()
		w.text = "milestone one is done"
		w.id = w.stageDirect(t)
	case "notice":
		w.notice = newNoticeWorld(t)
		w.notice.seed(t, nsSeed{signature: nsRelationSig, scope: nsProjectScope})
		w.ctx, w.s, w.c, w.f = w.notice.ctx, w.notice.s, w.notice.c, w.notice.stageFixture
		w.id = w.stageNotice(t)
	default:
		t.Fatalf("no hold world %q", kind)
	}
	if w.id == "" {
		t.Fatalf("the %s world staged no message", kind)
	}
	w.c.Settings = &delivery.TaskSettings{}
	return w
}

func (w *holdWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func (w *holdWorld) stageDirect(t *testing.T) string {
	t.Helper()
	answer, err := w.c.StageDirect(w.ctx, DirectMessage{Kind: KindLeadReport, Relationship: "initiative:" + storeRecipientInitiative, Subject: "lrp_1",
		Summary: "milestone one", Text: w.text, At: "2026-10-14T00:00:00.000000+00:00"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	return id
}

func (w *holdWorld) stageNotice(t *testing.T) string {
	t.Helper()
	answer, err := w.notice.channel.StageNotice(w.ctx, w.notice.facts(t, nsID))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := answer["messageId"].(string)
	return id
}

// recipient is the task the world's message is addressed to.
func (w *holdWorld) recipient() string {
	if w.kind == "direct" {
		return directManagement
	}
	return "supervisor"
}

// setHoldSQL writes a hold word and, for the superseded word, its supervisor_message_superseded
// journal row in one transaction: by names who parked it (AcknowledgeCommand for an
// acknowledgement's park, "" for the channel's own). It reaches the scenes no API does, such as a
// hold on a message that is sending, which only a store an older build wrote can hold.
func (w *holdWorld) setHoldSQL(t *testing.T, word, by string) {
	t.Helper()
	tx, err := w.s.DB.BeginTx(w.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(w.ctx, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", word, w.id); err != nil {
		t.Fatal(err)
	}
	if word == store.SupervisorHoldSuperseded {
		detail := `{"requestId": "r-derived", "detail": "what is owed moved"}`
		if by != "" {
			detail = `{"by": "` + by + `", "actor": "` + w.recipient() + `", "reason": "parked by hand"}`
		}
		if _, err := tx.ExecContext(w.ctx, `INSERT INTO journal(at,kind,subject,detail) VALUES('2023-11-14T22:13:40.000000+00:00','supervisor_message_superseded',?,?)`, w.id, detail); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// derive holds the message as the channel holds one whose obligation moved between the claim and the
// transport: the hold word and the channel's own supervisor_message_superseded row.
func (w *holdWorld) derive(t *testing.T) {
	t.Helper()
	w.setHoldSQL(t, store.SupervisorHoldSuperseded, "")
}

// acknowledge parks the message by id through the real acknowledgement (Channel.Acknowledge) and
// keeps its answer: parked, or the reason it was left. It checks what the store holds afterwards: a
// parked message carries superseded_by_report read as an acknowledged park, with one new
// supervisor_message_superseded row naming AcknowledgeCommand; a message left keeps the hold it had
// and gets no such row.
func (w *holdWorld) acknowledge(t *testing.T) {
	t.Helper()
	before, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	rows := w.supersededRows(t)
	answer, err := w.c.Acknowledge(w.ctx, AcknowledgeRequest{Recipient: w.recipient(), Actor: w.recipient(), Messages: []string{w.id}, At: "2023-11-14T22:13:30.000000+00:00"})
	if err != nil {
		t.Fatal(err)
	}
	parked, _ := answer["parked"].([]string)
	skipped, _ := answer["skipped"].([]any)
	after, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case len(parked) == 1 && parked[0] == w.id && len(skipped) == 0:
		w.answer = acknowledgeParked
		kind, err := holdKindOf(w.ctx, w.s, after)
		if err != nil || kind != holdAcknowledged || w.supersededRows(t) != rows+1 {
			t.Fatalf("parked %s reads as %q (%v) with %d superseded rows after %d, want an acknowledged park with one new row", w.id, kind, err, w.supersededRows(t), rows)
		}
	case len(parked) == 0 && len(skipped) == 1:
		left, _ := skipped[0].(map[string]any)
		if left["messageId"] != w.id {
			t.Fatalf("the acknowledgement left %v, want %s", left, w.id)
		}
		w.answer, _ = left["reason"].(string)
		if after.HoldReason != before.HoldReason || after.State != before.State || w.supersededRows(t) != rows {
			t.Fatalf("a message left by the acknowledgement moved: hold %v -> %v, state %s -> %s, superseded rows %d -> %d", before.HoldReason, after.HoldReason, before.State, after.State, rows, w.supersededRows(t))
		}
	default:
		t.Fatalf("the acknowledgement of one message answered parked %v skipped %v", parked, skipped)
	}
}

// supersededRows counts the message's supervisor_message_superseded journal rows.
func (w *holdWorld) supersededRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM journal WHERE kind='supervisor_message_superseded' AND subject=?", w.id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// setHold gives the message the hold of a row.
func (w *holdWorld) setHold(t *testing.T, hold string) {
	t.Helper()
	switch hold {
	case "none":
	case "acknowledged":
		w.acknowledge(t)
		if w.answer != acknowledgeParked {
			t.Fatalf("the acknowledgement left the message: %s", w.answer)
		}
	case "derived":
		w.derive(t)
	case "notice":
		if err := w.notice.channel.Park(w.ctx, w.id, "superseded elsewhere"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no hold %q in this world yet", hold)
	}
}

func (w *holdWorld) resubmit(t *testing.T) {
	t.Helper()
	w.exec(t, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',2,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow','DONE','proved','v1','the work is done, corrected','merge','2023-11-14T22:14:00.000000+00:00')`)
}

func (w *holdWorld) stagePass(t *testing.T, at string) {
	t.Helper()
	if _, err := w.c.StageUnsent(w.ctx, "PRJ-1", at, 300); err != nil {
		t.Fatal(err)
	}
}

func (w *holdWorld) apply(t *testing.T, change string) {
	t.Helper()
	switch change {
	case "same":
	case "resubmitted":
		w.resubmit(t)
	case "corrected_in_place":
		w.exec(t, "UPDATE work_reports SET pr_number=11,head_sha='a1b2c3d4e5f60718293a4b5c6d7e8f9012345611' WHERE event_id='event-1' AND submission_no=1")
	case "changed":
		w.text = "milestone one is done, and two started"
	case "sending":
		// A send claimed the message and its lease still runs past the row's daemon pass.
		w.exec(t, "UPDATE supervisor_messages SET state='sending',lease_owner='relay-daemon',lease_until=?,attempt_count=attempt_count+1 WHERE message_id=?", w.now+3600, w.id)
	case "decision":
		// The message is a decision: an answer is owed to it.
		w.exec(t, "UPDATE supervisor_messages SET kind='decision' WHERE message_id=?", w.id)
	case "other_sender":
		if w.kind == "direct" {
			w.exec(t, "UPDATE scope_bindings SET task_id=? WHERE binding_id='bnd-supervisor'", holdRuleLead)
			w.exec(t, "UPDATE scope_links SET upper_task_id=? WHERE link_id='lnk-project'", holdRuleLead)
			w.exec(t, "UPDATE scope_links SET lower_task_id=? WHERE link_id=?", holdRuleLead, "lnk-m-"+directManagement+"-"+storeRecipientInitiative)
		} else {
			w.exec(t, "UPDATE scope_bindings SET task_id='parent2' WHERE binding_id='b-parent'")
			w.exec(t, "UPDATE scope_links SET upper_task_id='parent2' WHERE link_id='lnk-issue'")
			w.exec(t, "UPDATE scope_links SET lower_task_id='parent2' WHERE link_id='lnk-project'")
		}
		w.expectResolution(t, true)
	case "other_recipient":
		if w.kind == "direct" {
			w.exec(t, "UPDATE scope_bindings SET task_id='01mgmtNext' WHERE binding_id=?", "bnd-m-"+directManagement)
			w.exec(t, "UPDATE scope_links SET upper_task_id='01mgmtNext' WHERE link_id=?", "lnk-m-"+directManagement+"-"+storeRecipientInitiative)
		} else {
			w.exec(t, "UPDATE scope_bindings SET task_id='supervisor2' WHERE binding_id='b-supervisor'")
			w.exec(t, "UPDATE scope_links SET upper_task_id='supervisor2' WHERE link_id='lnk-project'")
		}
		w.expectResolution(t, false)
	default:
		t.Fatalf("no change %q in this world yet", change)
	}
}

// expectResolution checks that a change moved only the sender (sameRecipient) or the recipient.
func (w *holdWorld) expectResolution(t *testing.T, sameRecipient bool) {
	t.Helper()
	relation := "rel-1"
	if w.kind == "direct" {
		relation = "initiative:" + storeRecipientInitiative
	}
	r, err := w.c.Resolve(w.ctx, relation)
	row, rowErr := w.c.Get(w.ctx, w.id)
	if err != nil || rowErr != nil || (r.Recipient == row.RecipientTaskID) != sameRecipient || (sameRecipient && r.Sender == row.SenderTaskID) {
		t.Fatalf("the change did not move what it names: %+v %v (row %s -> %s)", r, err, row.SenderTaskID, row.RecipientTaskID)
	}
}

func (w *holdWorld) autoSend(t *testing.T, c *Channel) {
	t.Helper()
	w.now += 60
	if _, err := c.AutoSend(w.ctx, w.host, w.now, 10, 10, "", "", ""); err != nil {
		t.Fatal(err)
	}
}

// enter runs a row's entry path.
func (w *holdWorld) enter(t *testing.T, entry string) {
	t.Helper()
	w.answer = ""
	switch entry {
	case "acknowledge":
		w.acknowledge(t)
	case "stage":
		o := w.f.obligation(t)
		if _, err := w.c.Stage(w.ctx, o, "", "2023-11-14T22:16:20.000000+00:00"); err != nil {
			t.Fatal(err)
		}
	case "direct_stage":
		w.stageDirect(t)
	case "notice_stage":
		w.stageNotice(t)
	case "stage_unsent":
		// The project's unsent messages staged again by hand (supervisor-stage --project).
		w.stagePass(t, "2023-11-14T22:16:20.000000+00:00")
	case "daemon_pass":
		w.autoSend(t, w.c)
	case "restart_pass":
		// A daemon started again holds nothing in memory: a fresh channel over the same store.
		fresh := &Channel{Store: w.s, Linkage: StoreLinkage{w.s}, Program: w.c.Program, Settings: &delivery.TaskSettings{}}
		w.autoSend(t, fresh)
	case "send_by_id":
		w.now += 60
		if _, err := w.c.Attempt(w.ctx, w.id, w.host, w.now); err != nil {
			t.Fatal(err)
		}
	case "notice_park":
		if err := w.notice.channel.Park(w.ctx, w.id, "superseded elsewhere"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no entry %q in this world yet", entry)
	}
}

// pass is the daemon supervisor pass that follows every entry. A fault notice is sent by its own
// deliverer once its notification is reserved, not by this pass, so a notice row reads the hold its
// entry left and no pass follows.
func (w *holdWorld) pass(t *testing.T) {
	t.Helper()
	if w.kind == "notice" {
		return
	}
	w.autoSend(t, w.c)
}

func (w *holdWorld) read(t *testing.T) (held, turn bool) {
	t.Helper()
	row, err := w.c.Get(w.ctx, w.id)
	if err != nil {
		t.Fatal(err)
	}
	var sent int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT count(*) FROM supervisor_attempts WHERE message_id=? AND send_attempted<>'no'", w.id).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	return row.HoldReason.String == store.SupervisorHoldSuperseded, sent > 0
}
