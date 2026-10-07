package decisions

import (
	"errors"
	"testing"
)

// CRW-903: the findings the reviewer raised on the CRW-737 bundle pull request, at the record
// level. A decision that blocks a relationship carries the reply the relay makes for each of its
// options, and an answer never rewrites the authority of the record it answers.

// review818Record is the relationship-blocking question the cases below raise, with the options,
// the blocking subjects and the authority the case names.
func review818Record(authority Authority, blocking []Blocking, options []Option) Record {
	record := Record{
		Schema:     Schema,
		DecisionID: "ud-903",
		Kind:       KindMergeApproval,
		Context:    "Hold the merge until the retention decision?",
		Options:    options,
		Blocking:   blocking,
		Origin:     Origin{Project: "PRJ-A"},
		Source:     Source{Kind: "receipt", Ref: "ev-blocked"},
		Authority:  authority,
		State:      StateRaised,
		RaisedAt:   "2026-10-06T00:00:00Z",
		Seen:       []Seen{{At: "2026-10-06T00:00:00Z", Source: "project:PRJ-A"}},
	}
	record.Fingerprint = Fingerprint(record.Context, record.Blocking, record.Options)
	return record
}

// review818Relationship is the one blocking subject these cases use.
func review818Relationship() []Blocking {
	return []Blocking{{Kind: BlockingRelationship, Ref: "rel-903"}}
}

// review818Options is the two-option set: the hold option replies stop and the merge option
// replies answer, unless the case leaves a reply out.
func review818Options(holdReply, mergeReply string) []Option {
	return []Option{
		{ID: "hold", Label: "hold", Effect: "hold the merge", Reply: holdReply},
		{ID: "merge", Label: "merge", Effect: "merge now", Reply: mergeReply},
	}
}

// A decision that blocks a relationship must carry a reply on every option: the relay applies it
// by comparing the chosen option's reply with the reply event's decision, so a raise that leaves
// one out can never be applied.
func TestReview818RaiseNeedsRepliesForARelationship(t *testing.T) {
	withoutReplies := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options("", ""))
	if err := ValidateRaise(withoutReplies); !errors.Is(err, ErrOptionReplyRequired) {
		t.Fatalf("a relationship-blocking record with no option replies: %v", err)
	}
	// One option carrying a reply and one without is still refused.
	halfAnswered := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ""))
	if err := ValidateRaise(halfAnswered); !errors.Is(err, ErrOptionReplyRequired) {
		t.Fatalf("a half-answered option set: %v", err)
	}
	// Every option carrying a reply validates.
	if err := ValidateRaise(review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))); err != nil {
		t.Fatalf("a relationship-blocking record with a reply on every option: %v", err)
	}
	// A record that blocks no relationship needs no reply.
	plain := review818Record(Authority{Kind: AuthorityUser}, nil, review818Options("", ""))
	if err := ValidateRaise(plain); err != nil {
		t.Fatalf("a record that blocks no relationship: %v", err)
	}
	// A reply outside the vocabulary is refused wherever it appears.
	plain.Options = review818Options("carry-on", "")
	if err := Validate(plain); !errors.Is(err, ErrUnknownReply) {
		t.Fatalf("an option reply outside the vocabulary: %v", err)
	}
	// A record written before the reply field existed carries relationship-blocking options with
	// no reply: the reader must still accept it, so it can be listed, answered and withdrawn. It
	// can never be applied, because its options name no reply to match the reply event against.
	if err := Validate(withoutReplies); err != nil {
		t.Fatalf("a stored relationship-blocking record without option replies: %v", err)
	}
	// The reply is not part of the question's identity: two records that differ only in reply are
	// one question.
	withReplies := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))
	withoutReply := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options("", ""))
	if withReplies.Fingerprint != withoutReply.Fingerprint {
		t.Fatalf("the reply changed the fingerprint: %s and %s", withReplies.Fingerprint, withoutReply.Fingerprint)
	}
}

// A question whose options name no reply has nothing for the fold to reconcile, so a repeat raise
// of it still folds and appends its observation. Two stored ids the fingerprint calls one identity
// (Validate refuses only an exact duplicate) must not turn that repeat raise into a refusal: there
// is no reply to place, so the stored option set stands.
func TestReview818ARepeatRaiseWithoutRepliesStillFolds(t *testing.T) {
	stored := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), []Option{
		{ID: "HOLD", Label: "hold", Effect: "hold the merge"},
		{ID: "hold", Label: "hold again", Effect: "hold the merge"},
	})
	incoming := stored
	incoming.Seen = append([]Seen{}, stored.Seen...)
	merged, err := Merge(stored, incoming)
	if err != nil {
		t.Fatalf("a repeat raise that names no reply: %v", err)
	}
	if len(merged.Options) != len(stored.Options) {
		t.Fatalf("the fold changed the option set: %v", merged.Options)
	}
	for i, option := range merged.Options {
		if option.ID != stored.Options[i].ID || option.Reply != stored.Options[i].Reply {
			t.Fatalf("the fold changed option %d: %+v", i, option)
		}
	}
}

// The stored option set may hold two ids the fingerprint calls one identity (Validate refuses only
// an exact duplicate), each with its own reply. An incoming raise that names the same ids is the
// same statement of the same question, so it folds: each reply is compared with the reply of the
// option that carries the same id, not with every option in the normalized bucket.
func TestReview818AnIdenticalReraiseWithOneIdentityFolds(t *testing.T) {
	stored := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), []Option{
		{ID: "HOLD", Label: "hold", Effect: "hold the merge", Reply: ReplyStop},
		{ID: "hold", Label: "hold again", Effect: "hold the merge", Reply: ReplyAnswer},
	})
	incoming := stored
	incoming.Seen = append([]Seen{}, stored.Seen...)
	merged, err := Merge(stored, incoming)
	if err != nil {
		t.Fatalf("an identical repeat raise: %v", err)
	}
	if len(merged.Seen) != 2 {
		t.Fatalf("the fold left %d observations, want the appended second one", len(merged.Seen))
	}
	for i, option := range merged.Options {
		if option.ID != stored.Options[i].ID || option.Reply != stored.Options[i].Reply {
			t.Fatalf("the fold changed option %d: %+v", i, option)
		}
	}
}

// The conflict is per option id: a raise that changes HOLD's reply while hold keeps its own is
// refused, and the option whose reply it did not change is not what refuses it.
func TestReview818AConflictIsJudgedPerOptionID(t *testing.T) {
	stored := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), []Option{
		{ID: "HOLD", Label: "hold", Effect: "hold the merge", Reply: ReplyStop},
		{ID: "hold", Label: "hold again", Effect: "hold the merge", Reply: ReplyAnswer},
	})
	incoming := stored
	incoming.Options = []Option{
		{ID: "HOLD", Label: "hold", Effect: "hold the merge", Reply: ReplyAnswer},
		{ID: "hold", Label: "hold again", Effect: "hold the merge", Reply: ReplyAnswer},
	}
	if _, err := Merge(stored, incoming); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("changing one option's reply = %v, want ErrMergeConflict", err)
	}
}

// An option reply the stored set cannot place is refused. The stored ids here are HOLD and HoLd,
// which the fingerprint calls one identity and whose exact spellings the incoming raise does not
// repeat, so a named reply has no single stored option to belong to.
func TestReview818AReplyThatNamesNoSingleStoredOptionIsRefused(t *testing.T) {
	stored := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), []Option{
		{ID: "HOLD", Label: "hold", Effect: "hold the merge"},
		{ID: "HoLd", Label: "hold again", Effect: "hold the merge"},
	})
	incoming := stored
	incoming.Options = []Option{
		{ID: "hold", Label: "hold", Effect: "hold the merge", Reply: ReplyStop},
		{ID: "HOLD", Label: "hold again", Effect: "hold the merge"},
	}
	if _, err := Merge(stored, incoming); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("an incoming reply that names no single stored option = %v, want ErrMergeConflict", err)
	}
}

// The reply is not part of the question's identity, so two raises of one question that name
// different replies for the same option are the same question and cannot become two records. The
// fold is refused instead: applying the record compares the chosen option's reply with the reply
// the relay recorded, and a silent fold would apply the later raise's answer against a mapping it
// never offered.
func TestReview818AFoldRefusesConflictingOptionReplies(t *testing.T) {
	first := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))
	second := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyAnswer, ReplyStop))
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("the two statements are not one question: %s and %s", first.Fingerprint, second.Fingerprint)
	}
	if _, err := Merge(first, second); !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("folding a conflicting reply = %v, want ErrMergeConflict", err)
	}
}

// The fold fills a reply the stored record does not carry, which is the shape a record written
// before the reply field existed has, and an incoming raise that names no reply keeps the stored
// one. The stored record's options are the ones the merged record carries.
func TestReview818AFoldFillsAndKeepsOptionReplies(t *testing.T) {
	legacy := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options("", ""))
	named := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))
	filled, err := Merge(legacy, named)
	if err != nil {
		t.Fatalf("filling a stored empty reply: %v", err)
	}
	for _, option := range filled.Options {
		want := ReplyStop
		if option.ID == "merge" {
			want = ReplyAnswer
		}
		if option.Reply != want {
			t.Fatalf("option %q carries reply %q after the fold, want the filled %q", option.ID, option.Reply, want)
		}
	}
	// An incoming raise that names no reply keeps the stored one.
	silent := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options("", ""))
	kept, err := Merge(named, silent)
	if err != nil {
		t.Fatalf("a raise that names no reply: %v", err)
	}
	for _, option := range kept.Options {
		want := ReplyStop
		if option.ID == "merge" {
			want = ReplyAnswer
		}
		if option.Reply != want {
			t.Fatalf("option %q carries reply %q after the fold, want the stored %q", option.ID, option.Reply, want)
		}
	}
}

// A relationship-blocking decision is answered with an option, never with free text alone: the
// relay cannot read a reply action out of prose.
func TestReview818RelationshipDecisionNeedsAnOption(t *testing.T) {
	record := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))
	if _, err := ValidateAnswer(record, Answer{Text: "hold it", By: "task-a", Via: ViaDots, Authority: Authority{Kind: AuthorityUser}}); !errors.Is(err, ErrAnswerNeedsOption) {
		t.Fatalf("a text-only answer to a relationship-blocking decision: %v", err)
	}
	if _, err := ValidateAnswer(record, Answer{Option: "hold", By: "task-a", Via: ViaDots, Authority: Authority{Kind: AuthorityUser}}); err != nil {
		t.Fatalf("an option answer to a relationship-blocking decision: %v", err)
	}
}

// The authority an answer cites never rewrites the record's: the cited class must be the record's
// own class, and when the record carries a ref the cited ref must be that same ref.
func TestReview818AuthorityIsKept(t *testing.T) {
	record := answerRecord(StateRaised, Authority{Kind: AuthorityParent, Ref: "plan-1"})
	if _, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: AuthorityParent, Ref: "plan-2"}}); !errors.Is(err, ErrAuthorityGrade) {
		t.Fatalf("an answer citing another ref: %v", err)
	}
	answered, err := ValidateAnswer(record, Answer{Option: "now", By: "task-a", Via: ViaDirectAsk, Authority: Authority{Kind: AuthorityParent, Ref: "plan-1"}})
	if err != nil {
		t.Fatalf("an answer citing the record's own ref: %v", err)
	}
	if answered.Authority != (Authority{Kind: AuthorityParent, Ref: "plan-1"}) {
		t.Fatalf("the answer changed the record's authority: %+v", answered.Authority)
	}
}
