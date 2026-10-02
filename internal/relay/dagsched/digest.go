package dagsched

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// SchemaAcceptance names the acceptance record (contract 4.3).
const SchemaAcceptance = "dag-acceptance/1"

// Acceptance is a dag_acceptances row. Optional columns are the empty string or zero when NULL.
type Acceptance struct {
	AcceptanceID, PlanID, NodeID, ManifestDigest   string
	RelationshipID                                 string
	ExecutionGeneration                            int64
	EventID, RevisionHash, CriteriaSetDigest       string
	Verdict                                        string
	HeadSHA, Repository                            string
	PRNumber                                       int64
	OutputManifestRef, EvidenceDigest, AckTier     string
	VerdictTurnID, RuleVersionJSON, AcceptedByTask string
	CoordinatorEpoch                               int64
	AcceptedAt, SupersedesAcceptanceID, State      string
}

// AcceptanceDigest is the digest contract 4.3 marks as the identity of an acceptance: canonical JSON of the included fields,
// an absent optional field omitted (null, empty and missing are one thing, 4.1). The plan id is included because node ids
// are plan-local (dag_zone.go): two plans may name a node alike, and acceptance_id is a store-wide key. The evidence digest,
// the rule versions, the ack tier, the verdict turn, the author, the epoch, the time, the supersession and the state are
// recorded and left out, so a re-run of a flaky check or a re-verification never changes what was accepted.
func AcceptanceDigest(a Acceptance) string {
	m := map[string]any{
		"schema": SchemaAcceptance, "plan_id": a.PlanID, "node_id": a.NodeID, "manifest_digest": a.ManifestDigest,
		"relationship_id": a.RelationshipID, "execution_generation": a.ExecutionGeneration, "event_id": a.EventID,
		"revision_hash": a.RevisionHash, "criteria_set_digest": a.CriteriaSetDigest, "verdict": a.Verdict,
	}
	if a.HeadSHA != "" {
		m["head_sha"] = a.HeadSHA
	}
	if a.Repository != "" {
		m["repository"] = a.Repository
	}
	if a.PRNumber > 0 {
		m["pr_number"] = a.PRNumber
	}
	return digestOf(m)
}

func digestOf(v any) string {
	h := sha256.Sum256([]byte(dag.Canonical(v)))
	return hex.EncodeToString(h[:])
}

// CheckRow is one check of an EvidenceBody.
type CheckRow struct {
	Name, RunID, HeadSHA, Conclusion string
	// Provider is part of the evidence only when the forge named one, so a body without providers digests as it always did.
	Provider string
	Attempt  int64
}

// EvidenceBody is the one serialization of what the relay observed of a pull request: the checks of the exact head, the
// required list and the review digest. Its digest is the acceptance's evidence_digest and a merge check's checks_digest, and
// the body itself is a merge check's evidence_json, so the digest can be recomputed from the row (B-13).
type EvidenceBody struct {
	Checks       []CheckRow
	Required     []string
	ReviewDigest string
}

// EvidenceBodyOf is the conversion from a pull request snapshot: the checks reported for the pull request's current head only
// (a check of an earlier push that a latest-filtered listing still returns is not evidence about this head), the declared
// required names and the review digest.
func EvidenceBodyOf(pr PullRequest) EvidenceBody {
	b := EvidenceBody{Required: append([]string(nil), pr.RequiredDeclared...), ReviewDigest: pr.ReviewDigest}
	for _, c := range pr.Checks {
		if c.HeadSHA == pr.HeadSHA {
			b.Checks = append(b.Checks, CheckRow{Name: c.Name, RunID: c.RunID, HeadSHA: c.HeadSHA, Conclusion: c.Conclusion, Provider: c.Provider, Attempt: c.Attempt})
		}
	}
	return b
}

// object is the canonical form: checks sorted by name, run id and attempt, required sorted.
func (b EvidenceBody) object() map[string]any {
	checks := append([]CheckRow(nil), b.Checks...)
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].Name != checks[j].Name {
			return checks[i].Name < checks[j].Name
		}
		if checks[i].RunID != checks[j].RunID {
			return checks[i].RunID < checks[j].RunID
		}
		return checks[i].Attempt < checks[j].Attempt
	})
	rows := make([]any, 0, len(checks))
	for _, c := range checks {
		row := map[string]any{"name": c.Name, "run_id": c.RunID, "head_sha": c.HeadSHA, "conclusion": c.Conclusion, "attempt": c.Attempt}
		if c.Provider != "" {
			row["provider"] = c.Provider
		}
		rows = append(rows, row)
	}
	required := append([]string(nil), b.Required...)
	sort.Strings(required)
	req := make([]any, 0, len(required))
	for _, r := range required {
		req = append(req, r)
	}
	return map[string]any{"checks": rows, "required": req, "review_digest": b.ReviewDigest}
}

// JSON is the canonical text of the body (a merge check's evidence_json).
func (b EvidenceBody) JSON() string { return dag.Canonical(b.object()) }

// EvidenceDigest is the sha256 of the canonical body.
func EvidenceDigest(b EvidenceBody) string { return digestOf(b.object()) }

// RecomputeEvidenceDigest parses a stored evidence_json and digests it again, so a row whose digest or body was altered is found.
func RecomputeEvidenceDigest(evidenceJSON string) (string, error) {
	var wire struct {
		Checks []struct {
			Name       string `json:"name"`
			RunID      string `json:"run_id"`
			HeadSHA    string `json:"head_sha"`
			Conclusion string `json:"conclusion"`
			Provider   string `json:"provider"`
			Attempt    int64  `json:"attempt"`
		} `json:"checks"`
		Required     []string `json:"required"`
		ReviewDigest string   `json:"review_digest"`
	}
	if err := json.Unmarshal([]byte(evidenceJSON), &wire); err != nil {
		return "", err
	}
	b := EvidenceBody{Required: wire.Required, ReviewDigest: wire.ReviewDigest}
	for _, c := range wire.Checks {
		b.Checks = append(b.Checks, CheckRow{Name: c.Name, RunID: c.RunID, HeadSHA: c.HeadSHA, Conclusion: c.Conclusion, Provider: c.Provider, Attempt: c.Attempt})
	}
	return EvidenceDigest(b), nil
}

// ReleaseRequestID is the managed-start request id of one release: derived from the node and its manifest digest only (contract
// 2.6, E-17, E-23), within the engine's 128 characters. It carries no attempt counter: a replay is the same request.
func ReleaseRequestID(nodeID, manifestDigest string) string {
	h := sha256.Sum256([]byte(nodeID + "|" + manifestDigest))
	return "dag-" + hex.EncodeToString(h[:])[:40]
}
