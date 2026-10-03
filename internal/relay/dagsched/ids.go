package dagsched

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The ids of the rows the scheduler writes are registry.CoordinationID over the fields that identify the row, so the same observation, check or record is found again under the id it was
// made with. One constructor per kind of row names the prefix and the fields in the order they are joined; TestScheduledIDsKeepTheirBytes pins the bytes, and a row's id never depends on the
// time or on a counter other than the sequence it states. (refreshDigest in baserefresh.go and landingRef in integration.go are the constructors of the base refresh id and of the landing set
// reference.)

// mergeCheckID is the id of the seq-th merge check of an acceptance.
func mergeCheckID(acceptance string, seq int64) string {
	return registry.CoordinationID("dmc", acceptance, itoa64(seq))
}

// revalidationID is the id of the seq-th re-validation of an acceptance.
func revalidationID(acceptance string, seq int64) string {
	return registry.CoordinationID("drv", acceptance, itoa64(seq))
}

// integrationObservationID is the id of the seq-th observation of an acceptance's head against one target branch.
func integrationObservationID(acceptance, repository, baseRef string, seq int64) string {
	return registry.CoordinationID("dio", acceptance, repository, baseRef, itoa64(seq))
}

// pairObservationID is the id of the measurement of two live heads against their merge base.
func pairObservationID(plan, left, right, leftHead, rightHead, base string) string {
	return registry.CoordinationID("dco", plan, left, right, leftHead, rightHead, base)
}

// tipObservationID is the id of the measurement of one live head against the tip of its target branch.
func tipObservationID(plan, node, head, tip, base string) string {
	return registry.CoordinationID("dto", plan, node, head, tip, base)
}

// decisionID is the id of a recorded decision: the canonical text of everything it records except who recorded it and when, as one field.
func decisionID(plan, subject, digest, disposition, authorityKind, authorityRef string, revision int64) string {
	return registry.CoordinationID("dec", dag.Canonical(map[string]any{"plan_id": plan, "subject": subject, "digest": digest, "disposition": disposition,
		"authority_kind": authorityKind, "authority_ref": authorityRef, "revision": revision}))
}

// summaryID is the id of an outbox entry: the document, the plan revision, the progress digest it states and its sequence number, with the plan.
func summaryID(plan, document string, planRevision int64, subjectDigest string, seq int64) string {
	return registry.CoordinationID("sum", dag.Canonical(map[string]any{"plan_id": plan, "document": document, "plan_revision": planRevision, "subject_digest": subjectDigest, "seq": seq}))
}
