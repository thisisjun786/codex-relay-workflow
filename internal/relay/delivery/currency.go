package delivery

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Lineage evidence and currency reasons (currency.py).
//
// CRW-827: the evidence words have one declaration, in registry, where the one head judgment lives.
// These names stay exported so the callers that read them - dagsched's accept path, the CLI, the
// tests - do not change. The three reasons below are this package's own and are not head evidence.
const (
	Sole               = registry.EvidenceSole
	Chain              = registry.EvidenceChain
	NoRevision         = registry.EvidenceNone
	Fork               = registry.EvidenceFork
	Cycle              = registry.EvidenceCycle
	UnknownPredecessor = registry.EvidenceUnknownPredecessor
	Disconnected       = registry.EvidenceDisconnected

	StaleGeneration    = "stale_generation"
	SupersededRevision = "superseded_revision"
	RevisionAmbiguous  = "revision_ambiguous"
)

// ambiguousEvidence is currency.AMBIGUOUS, as the one judgment declares it.
var ambiguousEvidence = registry.AmbiguousEvidence

// HeadRevision is currency.head_revision: the one revision this generation stands on, or why not.
// The judgment is registry's (CRW-827); this prints its answer as the dict this package has always
// printed, so every caller of delivery.HeadRevision is unchanged.
func HeadRevision(ctx context.Context, s *store.Store, rid string, generation int64) (Obj, error) {
	return HeadRevisionFrom(ctx, s.Q(ctx), rid, generation)
}

// HeadRevisionFrom reads through the caller's snapshot, including a read-only guard connection.
func HeadRevisionFrom(ctx context.Context, q store.Querier, rid string, generation int64) (Obj, error) {
	head, err := registry.HeadRevisionFrom(ctx, q, rid, generation)
	if err != nil {
		return nil, err
	}
	return head.Record(), nil
}

// RefuseNewFork is emit's judgment inside the intake transaction (CRW-826): a ready_for_review
// receipt that would leave its generation with no single head is refused the existing
// revision_ambiguous, and nothing is written. The reading is the head judgment's own
// (registry.ReadRevisions, registry.ReadThroughSuppressed, registry.JudgeHead), run once over the
// revisions the store holds and once over those revisions with this receipt's revision added, so
// what the generation reads WITHOUT the receipt decides whether the receipt may be refused at all:
//
//   - a generation that holds no revision is the first receipt's: there is no head to fork from;
//   - a generation that already reads no single head (fork, cycle, unknown predecessor,
//     disconnected) is left as it is: this judgment never makes an ambiguous generation worse,
//     and the recovery route (a generation opened by hand) is what repairs it;
//   - a revision that declares itself (--supersedes-revision naming its own revision) is left to
//     the lineage check, which refuses it precisely (revision_lineage_invalid).
//
// Otherwise a receipt whose addition turns the one head into an ambiguous reading is refused,
// and the detail names the revision the generation reads now: the child knows whether it named a
// predecessor, so nothing here refuses what it could not have known (CRW-470).
func RefuseNewFork(ctx context.Context, q store.Querier, rid string, generation int64, eventID, revisionHash string, supersedes *string) error {
	declared := ""
	if supersedes != nil {
		declared = strings.TrimSpace(*supersedes)
	}
	if declared != "" && declared == revisionHash {
		// the lineage check refuses a self-supersede with its own reason, after this judgment
		return nil
	}
	revisions, suppressed, err := registry.ReadRevisions(ctx, q, rid, generation)
	if err != nil {
		return err
	}
	if len(revisions) == 0 {
		return nil
	}
	anchors, err := registry.RequestedPredecessors(ctx, q, rid, generation)
	if err != nil {
		return err
	}
	without := registry.JudgeHead(registry.ReadThroughSuppressed(revisions, suppressed, anchors), anchors)
	if slices.Contains(ambiguousEvidence, without.Evidence) {
		return nil
	}
	// the revisions the judgment read are not written to: append to a copy, so the without-reading
	// stays what it was (ReadThroughSuppressed returns its input when no receipt is suppressed).
	candidate := registry.Revision{ID: eventID, Hash: revisionHash, Declared: declared}
	with := registry.JudgeHead(registry.ReadThroughSuppressed(append(slices.Clone(revisions), candidate), suppressed, anchors), anchors)
	if !slices.Contains(ambiguousEvidence, with.Evidence) {
		return nil
	}
	head := without.RevisionHash
	return &store.RefusedError{Reason: RevisionAmbiguous, Detail: fmt.Sprintf(
		"this receipt would leave generation %d of %s with no single head (%s): the revision the generation reads now is %s, so name it with --supersedes-revision %s",
		generation, rid, with.Evidence, head, head)}
}

// Currency is currency.currency_of: is this event the thing the assignment stands on now?
func Currency(ctx context.Context, s *store.Store, relationship, event Row) (Obj, error) {
	rid := event.S("relationship_id")
	if relationship.S("status") != "active" || truthy(relationship.Opt("superseded_by")) {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RelationshipNotActive}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("relationship %q is %q", rid, relationship.S("status"))}}, nil
	}
	generation := relationship.I("execution_generation")
	if event.I("execution_generation") != generation {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: StaleGeneration}, {Key: "evidence", Value: nil}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: fmt.Sprintf("this event is generation %d and the assignment is on generation %d", event.I("execution_generation"), generation)}}, nil
	}
	head, err := HeadRevision(ctx, s, rid, generation)
	if err != nil {
		return nil, err
	}
	if slices.Contains(ambiguousEvidence, pyjson.Text(head.Get("evidence"))) {
		competitors, _ := head.Lookup("competitors")
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: RevisionAmbiguous}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: nil}, {Key: "headRevisionHash", Value: nil}, {Key: "detail", Value: pyjson.Text(head.Get("detail"))}, {Key: "competitors", Value: competitors}}, nil
	}
	headID, _ := head.Lookup("eventId")
	headHash, _ := head.Lookup("revisionHash")
	if headID != event.S("event_id") {
		return Obj{{Key: "current", Value: false}, {Key: "reason", Value: SupersededRevision}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: fmt.Sprintf("the current revision of generation %d is %v", generation, headID)}}, nil
	}
	return Obj{{Key: "current", Value: true}, {Key: "reason", Value: nil}, {Key: "evidence", Value: pyjson.Text(head.Get("evidence"))}, {Key: "headEventId", Value: headID}, {Key: "headRevisionHash", Value: headHash}, {Key: "detail", Value: ""}}, nil
}
