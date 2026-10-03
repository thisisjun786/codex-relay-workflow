package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Outcomes of a merge check (dag_merge_checks.outcome).
const (
	OutcomeEligible      = "eligible"
	OutcomeRetrySameSHA  = "retry_same_sha"
	OutcomeEvicted       = "evicted"
	OutcomeChecksPending = "checks_pending"
	OutcomeStaleHead     = "stale_head"
	OutcomeStaleBase     = "stale_base"
	OutcomeStaleCriteria = "stale_criteria"
	OutcomePredecessor   = "predecessor_not_landed"
)

// mergeCheck is one observation of an accepted pull request at merge time. The table is a history: a row is appended only when the observation differs from the latest
// one, so a repeat of the same question is a replay and a flaky required check's retry round, a stale head and an eviction are all readable afterwards.
type mergeCheck struct {
	Acceptance Acceptance
	Observed   PullRequest
	BaseTip    string
	ChecksBase string // the base the checks ran against, when known
	Failed     []failure
	Round      int
	Outcome    string
	Reason     string
}

// atStand is the acceptance as a judgement of its pull request holds it: its head is the head it stands on, its own or, after a recorded base refresh (baserefresh.go), the one the record names. A
// merge check records that head as head_sha and the head the pull request showed as observed_head_sha; only the checks read the copy, never an identity, a digest or a criteria rule.
func atStand(a Acceptance, st acceptanceStand) Acceptance {
	a.HeadSHA = st.Head
	return a
}

// staleHeadReason says why a pull request at another head than the one the acceptance stands on is stale (E-10); the words are the same as before a base refresh existed when there is no record.
func staleHeadReason(observed, accepted string, st acceptanceStand) string {
	if st.RefreshID == "" {
		return "the pull request head is " + observed + " and the accepted head is " + accepted
	}
	return "the pull request head is " + observed + " and the head that base refresh " + st.RefreshID + " of the accepted head " + accepted + " names is " + st.Head
}

// appendMergeCheck appends the observation unless it equals the latest row's (outcome, head the acceptance stood on, observed head, failed required checks, round, evidence digest). It runs inside the
// caller's transaction or on its own; its reads and its insert use the connection ctx carries.
func (s *Scheduler) appendMergeCheck(ctx context.Context, q store.Querier, m mergeCheck) (seq int64, appended bool, err error) {
	body := EvidenceBodyOf(m.Observed)
	digest := EvidenceDigest(body)
	failed := failuresJSON(m.Failed)
	var lastSeq int64
	var outcome, observed, lastDigest, lastFailed, lastTip, lastHead string
	var round int
	found, err := queryOne(ctx, q, "SELECT check_seq, outcome, observed_head_sha, checks_digest, failed_required_json, round_no, base_tip_sha, head_sha FROM dag_merge_checks WHERE acceptance_id = ? ORDER BY check_seq DESC LIMIT 1",
		[]any{m.Acceptance.AcceptanceID}, &lastSeq, &outcome, &observed, &lastDigest, &lastFailed, &round, &lastTip, &lastHead)
	if err != nil {
		return 0, false, err
	}
	if found && outcome == m.Outcome && observed == m.Observed.HeadSHA && lastDigest == digest && lastFailed == failed && round == m.Round && lastTip == m.BaseTip && lastHead == m.Acceptance.HeadSHA {
		return lastSeq, false, nil
	}
	seq = lastSeq + 1
	var checksBase any
	if m.ChecksBase != "" {
		checksBase = m.ChecksBase
	}
	sum := sha256.Sum256([]byte(m.Acceptance.AcceptanceID + "|" + strconv.FormatInt(seq, 10)))
	_, err = q.ExecContext(ctx, "INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_base_sha, checks_digest, evidence_json,"+
		" failed_required_json, round_no, outcome, reason, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"dmc-"+hex.EncodeToString(sum[:])[:32], m.Acceptance.AcceptanceID, seq, m.Acceptance.HeadSHA, m.Observed.HeadSHA, m.BaseTip, checksBase, digest, body.JSON(),
		failed, m.Round, m.Outcome, m.Reason, s.now())
	return seq, err == nil, err
}

// pinnedPredecessor is an incoming code-pinned edge's predecessor as release freshness reads it: its active acceptance and the forge identity the relay recorded.
type pinnedPredecessor struct {
	Acceptance Acceptance
	Forge      string
	Number     int64
	Stand      acceptanceStand // what the acceptance stands on when it is read: the head the pull request is compared with
}

// pinnedPredecessors are the predecessors of a node's code-pinned incoming edges that have an active acceptance with a forge row, in edge order. A pinned edge whose
// acceptance has no forge row is not here: the reading already says blocked:acceptance_incomplete for it, so it can never drop out of freshness checking silently.
func (s *Scheduler) pinnedPredecessors(ctx context.Context, q store.Querier, plan string, edges []dag.SnapEdge) ([]pinnedPredecessor, error) {
	var out []pinnedPredecessor
	seen := map[string]bool{}
	for _, e := range edges {
		if !e.PinsCodeHead {
			continue
		}
		a, found, err := loadActiveAcceptance(ctx, q, plan, e.FromNodeID)
		if err != nil {
			return nil, err
		}
		if !found || seen[a.AcceptanceID] {
			continue
		}
		var forge string
		var number int64
		has, err := queryOne(ctx, q, "SELECT forge_repository, pr_number FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{a.AcceptanceID}, &forge, &number)
		if err != nil {
			return nil, err
		}
		if has {
			seen[a.AcceptanceID] = true
			stand, err := s.standOf(ctx, q, a)
			if err != nil {
				return nil, err
			}
			out = append(out, pinnedPredecessor{Acceptance: a, Forge: forge, Number: number, Stand: stand})
		}
	}
	return out, nil
}

// freshness is E-10 for the pull requests a release builds on: the relay reads each pinned predecessor's pull request itself and refuses the release when its head is
// no longer the one the acceptance stands on (the accepted head, or after a recorded base refresh the head the record names), after recording that observation (a stale_head row), so no release
// follows a push (contract E-10). It applies the fail-closed rule in its
// release form: a reader error or a verdict of unknown is the host's failure and nothing is written (a merged pull request is read by the rule in ClassifyPullRequest: its verdict is unknown by
// construction, and it is readable beside what merging explains), a verdict of stale is refused, a closed or merged pull request is
// allowed (a predecessor that landed before its successor got capacity keeps its immutable accepted head) and only the head is compared. A failure to read is retryable;
// a moved head is not. A record that lands while the forge is read moves what the acceptance stands on: the observation is then not written (it would be made under a head the acceptance no longer stands on)
// and the release is refused as a moved candidate, to be repeated.
func (s *Scheduler) freshness(ctx context.Context, plan, actor string, preds []pinnedPredecessor) error {
	if len(preds) == 0 {
		return nil
	}
	if s.PRs == nil {
		return fmt.Errorf("this scheduler has no pull request reader, so it cannot confirm that %d pinned predecessor head(s) are current", len(preds))
	}
	for _, p := range preds {
		pr, err := s.PRs(ctx, p.Forge, p.Number)
		if err != nil {
			return err
		}
		if err := ClassifyPullRequest(pr); err != nil {
			return err
		}
		if pr.HeadSHA == p.Stand.Head {
			continue
		}
		if err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
			// the stale_head row is a write of the release that read the forge, and a release of a session that lost the plan's epoch while the forge was read writes nothing
			if err := s.fence(txCtx, s.Store.Q(txCtx), plan, actor); err != nil {
				return err
			}
			if now, err := s.standOf(txCtx, s.Store.Q(txCtx), p.Acceptance); err != nil {
				return err
			} else if now != p.Stand {
				return refuseCandidateMoved("a base refresh of %s was recorded while its pull request %s#%d was being read, so that reading is older than the history: repeat the release so the current head is checked", p.Acceptance.NodeID, p.Forge, p.Number)
			}
			_, _, err := s.appendMergeCheck(txCtx, s.Store.Q(txCtx), mergeCheck{Acceptance: atStand(p.Acceptance, p.Stand), Observed: pr, BaseTip: pr.BaseSHA, Round: 1, Outcome: OutcomeStaleHead,
				Reason: staleHeadReason(pr.HeadSHA, p.Acceptance.HeadSHA, p.Stand)})
			return err
		}); err != nil {
			return err
		}
		if p.Stand.RefreshID != "" {
			return refuseCandidateMoved("the pull request %s#%d is at %s and the head of %s that base refresh %s names is %s", p.Forge, p.Number, pr.HeadSHA, p.Acceptance.NodeID, p.Stand.RefreshID, p.Stand.Head)
		}
		return refuseCandidateMoved("the pull request %s#%d is at %s and the accepted head of %s is %s", p.Forge, p.Number, pr.HeadSHA, p.Acceptance.NodeID, p.Acceptance.HeadSHA)
	}
	return nil
}
