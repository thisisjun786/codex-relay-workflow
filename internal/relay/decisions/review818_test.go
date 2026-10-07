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
	if err := Validate(withoutReplies); !errors.Is(err, ErrOptionReplyRequired) {
		t.Fatalf("a relationship-blocking record with no option replies: %v", err)
	}
	// One option carrying a reply and one without is still refused.
	halfAnswered := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ""))
	if err := Validate(halfAnswered); !errors.Is(err, ErrOptionReplyRequired) {
		t.Fatalf("a half-answered option set: %v", err)
	}
	// Every option carrying a reply validates.
	if err := Validate(review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))); err != nil {
		t.Fatalf("a relationship-blocking record with a reply on every option: %v", err)
	}
	// A record that blocks no relationship needs no reply.
	plain := review818Record(Authority{Kind: AuthorityUser}, nil, review818Options("", ""))
	if err := Validate(plain); err != nil {
		t.Fatalf("a record that blocks no relationship: %v", err)
	}
	// A reply outside the vocabulary is refused wherever it appears.
	plain.Options = review818Options("carry-on", "")
	if err := Validate(plain); !errors.Is(err, ErrUnknownReply) {
		t.Fatalf("an option reply outside the vocabulary: %v", err)
	}
	// The reply is not part of the question's identity: two records that differ only in reply are
	// one question.
	withReplies := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options(ReplyStop, ReplyAnswer))
	withoutReply := review818Record(Authority{Kind: AuthorityUser}, review818Relationship(), review818Options("", ""))
	if withReplies.Fingerprint != withoutReply.Fingerprint {
		t.Fatalf("the reply changed the fingerprint: %s and %s", withReplies.Fingerprint, withoutReply.Fingerprint)
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
