package decisions

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// CRW-718 part A: the raise record derived from a blocked_needs_input receipt or from an UNSAFE or
// NEEDS_HUMAN work report. The observations are fixtures; the supervisor is not imported.

const deriveObservedAt = "2026-10-10T01:00:00Z"

// deriveReceipt is a child's blocked_needs_input receipt with no work report.
func deriveReceipt() Observation {
	return Observation{
		DecisionID: "ud-718", ObligationID: "0123456789abcdef0123456789abcdef", EventID: "ev-blocked",
		RelationshipID: "rel-0123456789abcdef", Generation: 2, Issue: "CRW-718", Project: "PRJ-A",
		Outcome: OutcomeBlockedNeeds, Summary: "Which store should hold the table?", ObservedAt: deriveObservedAt,
	}
}

// deriveUnsafeReport is the same receipt carrying an UNSAFE work report.
func deriveUnsafeReport() Observation {
	obs := deriveReceipt()
	obs.ReportStatus, obs.Reason, obs.Summary = ReportStatusUnsafe, "destructive step", "The migration would drop a live table."
	return obs
}

func mustDerive(t *testing.T, obs Observation, existing *Record) Record {
	t.Helper()
	record, err := Derive(obs, existing)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return record
}

func TestDeriveReceiptPathMakesOneLinkedRecord(t *testing.T) {
	obs := deriveReceipt()
	record := mustDerive(t, obs, nil)
	if err := ValidateRaise(record); err != nil {
		t.Fatalf("a derived record must pass ValidateRaise: %v", err)
	}
	if record.State != StateRaised || record.DecisionID != "ud-718" || record.Kind != KindBlockedEscalation || record.RaisedAt != deriveObservedAt || record.RaisedVia != RaisedViaObligation {
		t.Fatalf("record = %+v", record)
	}
	// A relationship-blocking question is applied by a reply that answers this receipt: decision-apply
	// reads that receipt from an "event" source, so the receipt is the source (CRW-718 d1).
	if record.Source != (Source{Kind: SourceKindEvent, Ref: obs.EventID}) {
		t.Fatalf("a receipt question's source is the receipt: %+v", record.Source)
	}
	if !strings.Contains(record.Seen[0].Note, obs.ObligationID) || !strings.Contains(record.Context, obs.ObligationID) {
		t.Fatalf("the record must stay linked to the obligation: seen %+v context %q", record.Seen, record.Context)
	}
	if want := []Blocking{{Kind: BlockingRelationship, Ref: obs.RelationshipID}}; !reflect.DeepEqual(record.Blocking, want) {
		t.Fatalf("blocking = %+v", record.Blocking)
	}
	if len(record.Options) != 2 || record.Options[0].Reply != ReplyAnswer || record.Options[1].Reply != ReplyStop {
		t.Fatalf("a relationship-blocking option names its reply: %+v", record.Options)
	}
	if want := []Seen{{At: deriveObservedAt, Source: "event:ev-blocked", Note: "receipt blocked_needs_input, obligation 0123456789abcdef0123456789abcdef"}}; !reflect.DeepEqual(record.Seen, want) {
		t.Fatalf("seen = %+v", record.Seen)
	}
	if record.Origin != (Origin{Issue: "CRW-718", Project: "PRJ-A"}) || record.Authority.Kind != AuthorityUser {
		t.Fatalf("origin/authority = %+v %+v", record.Origin, record.Authority)
	}
	if record.Fingerprint != Fingerprint(record.Context, record.Blocking, record.Options) {
		t.Fatal("the fingerprint is not the content's")
	}
	if !strings.Contains(record.Context, "Which store should hold the table?") || !strings.Contains(record.Context, "rel-0123456789abcdef") {
		t.Fatalf("context = %q", record.Context)
	}
}

func TestDeriveWorkReportPathUnsafeAndNeedsHuman(t *testing.T) {
	// UNSAFE on a blocked_needs_input receipt: one record linked to the obligation, replies named.
	unsafe := mustDerive(t, deriveUnsafeReport(), nil)
	if err := ValidateRaise(unsafe); err != nil {
		t.Fatal(err)
	}
	if unsafe.Source != (Source{Kind: SourceKindEvent, Ref: "ev-blocked"}) || unsafe.Seen[0].Note != "work report UNSAFE, obligation 0123456789abcdef0123456789abcdef" || unsafe.Blocking[0].Kind != BlockingRelationship {
		t.Fatalf("unsafe = %+v", unsafe)
	}
	if !strings.Contains(unsafe.Context, "UNSAFE") || !strings.Contains(unsafe.Context, "destructive step") || !strings.Contains(unsafe.Context, "drop a live table") {
		t.Fatalf("context = %q", unsafe.Context)
	}
	if unsafe.Fingerprint == mustDerive(t, deriveReceipt(), nil).Fingerprint {
		t.Fatal("a work report and a bare receipt are different questions")
	}
	// NEEDS_HUMAN on a receipt that did not end blocked: no reply to give, so the issue is blocked.
	obs := deriveUnsafeReport()
	obs.ReportStatus, obs.Outcome = ReportStatusNeedsHuman, "ready_for_review"
	human := mustDerive(t, obs, nil)
	if want := []Blocking{{Kind: BlockingIssue, Ref: "CRW-718"}}; !reflect.DeepEqual(human.Blocking, want) {
		t.Fatalf("blocking = %+v", human.Blocking)
	}
	// No receipt waits for a reply, so the link to the obligation is the source.
	if human.Source != (Source{Kind: SourceKindObligation, Ref: obs.ObligationID}) {
		t.Fatalf("source = %+v", human.Source)
	}
	for _, option := range human.Options {
		if option.Reply != "" {
			t.Fatalf("an issue-blocking option names no reply: %+v", option)
		}
	}
	// Without an issue key the project is what waits.
	obs.Issue = ""
	if got := mustDerive(t, obs, nil).Blocking; !reflect.DeepEqual(got, []Blocking{{Kind: BlockingProject, Ref: "PRJ-A"}}) {
		t.Fatalf("blocking = %+v", got)
	}
}

func TestDeriveRepeatedStatementMergesIntoSeen(t *testing.T) {
	for name, obs := range map[string]Observation{"receipt": deriveReceipt(), "work report": deriveUnsafeReport()} {
		first := mustDerive(t, obs, nil)
		again := obs
		again.DecisionID, again.EventID, again.ObservedAt = "ud-other", "ev-resend", "2026-10-10T02:00:00Z"
		merged := mustDerive(t, again, &first)
		if merged.DecisionID != first.DecisionID || merged.Fingerprint != first.Fingerprint || merged.State != StateRaised || merged.RaisedAt != first.RaisedAt {
			t.Fatalf("%s: a repeat keeps the record: %+v", name, merged)
		}
		if len(merged.Seen) != 2 || merged.Seen[0] != first.Seen[0] || merged.Seen[1].Source != "event:ev-resend" || merged.Seen[1].At != again.ObservedAt {
			t.Fatalf("%s: seen = %+v", name, merged.Seen)
		}
		if !reflect.DeepEqual(merged.Options, first.Options) || !reflect.DeepEqual(merged.Blocking, first.Blocking) || merged.Context != first.Context {
			t.Fatalf("%s: a repeat changes nothing but seen", name)
		}
		if len(first.Seen) != 1 {
			t.Fatalf("%s: the stored record was changed in place", name)
		}
	}
}

func TestDeriveRepeatOfAnsweredAndAppliedKeepsAnswerAndApply(t *testing.T) {
	obs := deriveReceipt()
	stored := mustDerive(t, obs, nil)
	stored.State = StateAnswered
	stored.AnsweredAt, stored.AnsweredBy, stored.AnsweredVia, stored.AnswerText = "2026-10-10T01:30:00Z", "jun", "file-import", "use sqlite"
	stored.Authority = Authority{Kind: AuthorityParent, Ref: "parent-1"}
	for _, state := range []State{StateAnswered, StateApplied} {
		stored.State = state
		if state == StateApplied {
			stored.AppliedAt, stored.AppliedEvent, stored.AppliedGeneration = "2026-10-10T01:40:00Z", "ev-reply", 3
		}
		again := obs
		again.ObservedAt = "2026-10-10T03:00:00Z"
		merged := mustDerive(t, again, &stored)
		want := stored
		want.Seen = append(append([]Seen{}, stored.Seen...), Seen{At: again.ObservedAt, Source: "event:ev-blocked", Note: "receipt blocked_needs_input, obligation " + obs.ObligationID})
		if !reflect.DeepEqual(merged, want) {
			t.Fatalf("%s: a repeat of an answered record changes only seen:\n got %+v\nwant %+v", state, merged, want)
		}
	}
}

func TestDeriveDifferentQuestionsAreDifferentRecords(t *testing.T) {
	base := mustDerive(t, deriveReceipt(), nil)
	for name, change := range map[string]func(*Observation){
		"generation":   func(o *Observation) { o.Generation = 3 },
		"statement":    func(o *Observation) { o.Summary = "Another question." },
		"relationship": func(o *Observation) { o.RelationshipID = "rel-other" },
	} {
		obs := deriveReceipt()
		change(&obs)
		if got := mustDerive(t, obs, nil); got.Fingerprint == base.Fingerprint {
			t.Fatalf("%s: shares the fingerprint of another question", name)
		}
		if _, err := Derive(obs, &base); !errors.Is(err, ErrFingerprintMismatch) {
			t.Fatalf("%s: merging into another question = %v, want ErrFingerprintMismatch", name, err)
		}
	}
}

func TestDeriveKeepsMergeConflictChecks(t *testing.T) {
	// A stored option whose reply differs from the one this observation names is refused, not folded.
	stored := mustDerive(t, deriveReceipt(), nil)
	stored.Options = append([]Option{}, stored.Options...)
	stored.Options[0].Reply = ReplyScopeChange
	if _, err := Derive(deriveReceipt(), &stored); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("a differing stored reply = %v, want ErrMergeConflict", err)
	}
	// A stored record whose fingerprint is not its content's is refused.
	broken := mustDerive(t, deriveReceipt(), nil)
	broken.Context += " edited"
	if _, err := Derive(deriveReceipt(), &broken); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("a stored record out of step with its fingerprint = %v", err)
	}
}

func TestDeriveRepeatLeavesAnOldRecordWithoutRepliesAsItIs(t *testing.T) {
	stored := mustDerive(t, deriveReceipt(), nil)
	stored.Options = []Option{{ID: ReplyAnswer, Label: "Answer the child"}, {ID: ReplyStop, Label: "Stop the child"}}
	merged := mustDerive(t, deriveReceipt(), &stored)
	if !reflect.DeepEqual(merged.Options, stored.Options) {
		t.Fatalf("a repeat does not fill a reply into an old record: %+v", merged.Options)
	}
}

func TestDeriveRefusals(t *testing.T) {
	ordinary := deriveReceipt()
	ordinary.Outcome = "ready_for_review"
	if _, err := Derive(ordinary, nil); !errors.Is(err, ErrNotDecisionRequest) {
		t.Fatalf("an ordinary outcome = %v", err)
	}
	blocked := deriveReceipt()
	blocked.Outcome, blocked.ReportStatus = "failed", "BLOCKED"
	if _, err := Derive(blocked, nil); !errors.Is(err, ErrNotDecisionRequest) {
		t.Fatalf("a BLOCKED work report asks for no decision: %v", err)
	}
	for name, change := range map[string]func(*Observation){
		"decision id":     func(o *Observation) { o.DecisionID = "" },
		"obligation id":   func(o *Observation) { o.ObligationID = "" },
		"event id":        func(o *Observation) { o.EventID = "" },
		"relationship id": func(o *Observation) { o.RelationshipID = " " },
		"project":         func(o *Observation) { o.Project = "" },
		"observed at":     func(o *Observation) { o.ObservedAt = "" },
		"bad obligation":  func(o *Observation) { o.ObligationID = "a b" },
	} {
		obs := deriveReceipt()
		change(&obs)
		if _, err := Derive(obs, nil); !errors.Is(err, ErrObservationField) {
			t.Fatalf("%s missing = %v", name, err)
		}
	}
	late := deriveReceipt()
	late.ObservedAt = "yesterday"
	if _, err := Derive(late, nil); !errors.Is(err, ErrBadRaisedAt) {
		t.Fatalf("a bad observation time = %v", err)
	}
	late = deriveReceipt()
	late.NeededBy = "soon"
	if _, err := Derive(late, nil); !errors.Is(err, ErrBadNeededBy) {
		t.Fatalf("a bad needed_by = %v", err)
	}
	colon := deriveReceipt()
	colon.RelationshipID = "rel:1"
	if _, err := Derive(colon, nil); !errors.Is(err, ErrAmbiguousField) {
		t.Fatalf("a relationship id the format cannot carry = %v", err)
	}
	// A repeat needs no decision id: the stored record names it.
	stored := mustDerive(t, deriveReceipt(), nil)
	again := deriveReceipt()
	again.DecisionID = ""
	mustDerive(t, again, &stored)
}

func TestDeriveContextIsMadeValid(t *testing.T) {
	obs := deriveReceipt()
	obs.Summary = "Pick a|b\nsecond line\twith tab\x07bell"
	obs.NeededBy = "2026-10-11T00:00:00Z"
	obs.Authority = Authority{Kind: AuthorityDelegatedManagement}
	record := mustDerive(t, obs, nil)
	if strings.ContainsAny(record.Context, "|\x07") || !strings.Contains(record.Context, "a/b\nsecond line\twith tab bell") {
		t.Fatalf("context = %q", record.Context)
	}
	if record.NeededBy != obs.NeededBy || record.Authority.Kind != AuthorityDelegatedManagement {
		t.Fatalf("record = %+v", record)
	}
	blank := deriveReceipt()
	blank.Summary = ""
	if got := mustDerive(t, blank, nil).Context; !strings.Contains(got, "the turn ended blocked_needs_input") {
		t.Fatalf("context = %q", got)
	}
}

// Two obligations whose statements differ only in a character the format cannot carry are different
// questions: the lossy display form must not make them one record (CRW-718 d2).
func TestDeriveObligationsThatReadAlikeAreDifferentQuestions(t *testing.T) {
	a, b := deriveUnsafeReport(), deriveUnsafeReport()
	a.ObligationID, a.Summary = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Run a|b?"
	b.ObligationID, b.Summary = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "Run a/b?"
	first := mustDerive(t, a, nil)
	if second := mustDerive(t, b, nil); second.Fingerprint == first.Fingerprint {
		t.Fatal("two obligations share a fingerprint")
	}
	if _, err := Derive(b, &first); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("a different obligation merged into a held question = %v", err)
	}
	first.State, first.AnsweredAt, first.AnsweredBy, first.AnsweredVia, first.AnswerText = StateAnswered, "2026-10-10T01:30:00Z", "jun", "file-import", "answer"
	if _, err := Derive(b, &first); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("a different obligation took an answered question's answer = %v", err)
	}
	// The same obligation still merges.
	again := a
	again.EventID, again.ObservedAt = "ev-again", "2026-10-10T02:00:00Z"
	if merged := mustDerive(t, again, &first); len(merged.Seen) != 2 {
		t.Fatalf("seen = %+v", merged.Seen)
	}
}

// The supervisor's obligation id for a work report does not include the receipt outcome, so a later
// event of the same obligation with another outcome is the same question (CRW-718 d3).
func TestDeriveSameObligationWithAnotherOutcomeMerges(t *testing.T) {
	first := deriveUnsafeReport()
	first.Outcome, first.Reason, first.Summary = "failed", "needs approval", ""
	stored := mustDerive(t, first, nil)
	if strings.Contains(stored.Context, "failed") {
		t.Fatalf("the outcome is not part of the question: %q", stored.Context)
	}
	for _, outcome := range []string{"ready_for_review", "failed", "cancelled"} {
		next := first
		next.EventID, next.Outcome, next.ObservedAt = "ev-"+outcome, outcome, "2026-10-10T02:00:00Z"
		merged := mustDerive(t, next, &stored)
		if merged.Fingerprint != stored.Fingerprint || len(merged.Seen) != 2 {
			t.Fatalf("%s: %+v", outcome, merged)
		}
	}
	// A receipt that waits for a reply blocks the relationship instead of the issue: another question.
	waiting := first
	waiting.Outcome = OutcomeBlockedNeeds
	if _, err := Derive(waiting, &stored); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("a blocked receipt of the same report = %v", err)
	}
}
