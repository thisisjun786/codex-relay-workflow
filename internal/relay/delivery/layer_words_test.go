package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// CRW-1443: the text delivery renders for a session names the layer (부모, 자식), never the relay
// role id written as a bare word. Each message and refusal below is built through the package's own
// builders, and layername.Find (the matcher the ci ratchet shares) must find no bare role word in it.

func requireNoRoleWords(t *testing.T, what, text string) {
	t.Helper()
	if text == "" {
		t.Fatalf("%s: nothing rendered", what)
	}
	if hits := layername.Find(text); len(hits) > 0 {
		var words []string
		for _, h := range hits {
			start, end := max(0, h.Offset-40), min(len(text), h.Offset+len(h.Word)+40)
			words = append(words, h.Word+" in "+`"`+text[start:end]+`"`)
		}
		t.Errorf("%s names a role id where a person reads a layer (write 부모 / 자식): %s", what, strings.Join(words, "; "))
	}
}

func TestRenderedTextNamesLayersNotRoleIds(t *testing.T) {
	t.Run("messages", func(t *testing.T) {
		row := Row{"event_id": "evt-1", "relationship_id": "rel-0000000000000001", "recipient_task_id": parent}
		completion := Obj{{Key: "executionGeneration", Value: int64(1)}, {Key: "attempt", Value: int64(1)}, {Key: "outcome", Value: "ready_for_review"}, {Key: "revisionHash", Value: "rev-1"},
			{Key: "criteria", Value: []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}}}}}
		requireNoRoleWords(t, "the verification request", renderCompletion(row, completion, "req-1"))

		// A revision with an undecided finding and no note, and no generation recorded: every
		// "not recorded" line and every line about the findings without a disposition is shown.
		undecided := Obj{{Key: "criteria", Value: []any{Obj{{Key: "id", Value: "c1"}}, Obj{{Key: "id", Value: "c2"}}}}}
		requireNoRoleWords(t, "the revision request with undecided findings", renderRevision(row, undecided, "req-1"))
		requireNoRoleWords(t, "the revision request with no findings", renderRevision(row, Obj{}, "req-1"))
		verified := Obj{{Key: "executionGeneration", Value: int64(2)}, {Key: "criteria", Value: []any{Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}}}}}
		requireNoRoleWords(t, "the revision request with every finding verified", renderRevision(row, verified, "req-1"))

		for _, decision := range []string{DecisionAnswer, DecisionStop} {
			record := Obj{{Key: "decision", Value: decision}, {Key: "answersEvent", Value: "evt-0"}, {Key: "executionGeneration", Value: int64(1)}, {Key: "note", Value: "use the shorter table"},
				{Key: "childTaskId", Value: child}, {Key: "anchorTurnId", Value: dispatchTurn}}
			requireNoRoleWords(t, "the "+decision+" decision", renderDecision(row, record, "req-1"))
		}

		grant := Obj{{Key: "turnId", Value: "mt-1"}, {Key: "grantId", Value: "g-1"}, {Key: "repository", Value: "owner/repo"}, {Key: "baseRef", Value: "dev"}, {Key: "candidateHead", Value: "abc"},
			{Key: "grantedFrom", Value: "queue"}, {Key: "requestId", Value: "rr-1"}, {Key: "requestedBy", Value: "01other-parent"}, {Key: "evidence", Value: "asked"}}
		requireNoRoleWords(t, "the merge-turn grant", renderGrant("evt-1", grant, "req-1", evidence.RequiredReading{Reason: "no reading"}))
		requireNoRoleWords(t, "the merge-turn return request", renderReturnRequest("evt-1", grant, "req-1"))
	})

	t.Run("work reports", func(t *testing.T) {
		completion, revision := kinds(reportComposeStore(t))
		// followUpOwner is the child's own free text naming who owns a follow-up (task-packet.md,
		// merge-readiness handoff), echoed as written, so the input here names owners by task id.
		done := richCompletion()
		done.hand("thread_dispositions", `[{"threadId":"t1","disposition":"accepted","addressedBy":"#12","followUpOwner":"01parent-task","reopenTrigger":"a new finding"},{"threadId":"t2","disposition":"fixed"},{"threadId":"t3","disposition":"accepted","addressedBy":"#13","followUpOwner":"01child-task","reopenTrigger":"a red check"}]`)
		for _, c := range []struct {
			name string
			k    kind
			c    *composed
		}{{"the DONE work report", completion, done}, {"the sparse work report", completion, sparseCompletion()}, {"the revision work report", revision, richRevision()}, {"the plain revision work report", revision, plainRevision()}} {
			text, err := c.k.compose(c.c, 1<<20)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			requireNoRoleWords(t, c.name, text)
		}
		undecided := plainRevision()
		undecided.rec("criteria", `[{"id":"c1"},{"id":"c2","verdict":"verified"}]`)
		text, err := revision.compose(undecided, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		requireNoRoleWords(t, "the revision work report with an undecided finding", text)
	})

	t.Run("refusals", func(t *testing.T) {
		tree := parityTree(t)
		f, ack, _, revision, _, _ := revisionFixture(t, tree)
		_, err := ack.Evaluate(f.ctx, revision)
		requireReason(t, err, WrongDeliveryKind)
		requireNoRoleWords(t, "the evaluate refusal for a revision request", Detail(err))
		_, err = ack.Acknowledge(f.ctx, revision, "child-turn", AckProof(revision, "child-turn"), true, nil, f.host)
		requireReason(t, err, WrongDeliveryKind)
		requireNoRoleWords(t, "the ack refusal for a revision request", Detail(err))

		bad := "not-a-head"
		err = (&PreparedVerdict{req: VerdictRequest{VerifiedHead: &bad}}).checkHead()
		if err == nil {
			t.Fatal("a malformed --verified-head was accepted")
		}
		requireNoRoleWords(t, "the --verified-head usage error", err.Error())
	})

	t.Run("recipient direction", func(t *testing.T) {
		r := Relationship{ID: "rel-1", Parent: Endpoint{TaskID: "01parent-task"}, Child: Endpoint{TaskID: "01child-task"}, AllowedRecipients: []string{"01parent-task", "01child-task"}}
		for _, kind := range []string{Completion, MergeTurnGrant} {
			err := assertAssignmentDelivery(r, kind, "01other-task", nil, nil, nil)
			requireReason(t, err, RecipientNotAuthorized)
			want := "a " + kind + " for 'rel-1' goes to its own 자식 '01parent-task', not to '01other-task'"
			if Detail(err) != want {
				t.Errorf("%s to the wrong recipient: %q, want %q", kind, Detail(err), want)
			}
			requireNoRoleWords(t, "the "+kind+" recipient refusal", Detail(err))
		}
		// A revision request goes to the relay child; that sentence is relay child text and stays.
		err := assertAssignmentDelivery(r, Revision, "01other-task", nil, nil, nil)
		requireReason(t, err, RecipientNotAuthorized)
		if want := "a revision_request for 'rel-1' goes to its own child '01child-task', not to '01other-task'"; Detail(err) != want {
			t.Errorf("revision to the wrong recipient: %q, want %q", Detail(err), want)
		}
	})
}
