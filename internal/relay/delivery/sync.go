package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The verdict's sync obligation (sync.py enqueue_verdict_in) and the claim fence the outbox's
// completion rests on (claim, _fenced). The rest of the outbox - next, complete's readback
// validation, fail, retry - is todo 23's.
const (
	SyncNotClaimable     = "sync_not_claimable"
	coordinationDocument = "coordination_document"
	syncLeaseSeconds     = 300.0
)

func syncCanonical(target, targetRef, kind, rid string, event, generation, revision, verdict, digest, ruling any) string {
	render := func(v any) string {
		if v == nil {
			return "null"
		}
		return pyStr(v)
	}
	parts := []string{target, targetRef, kind, rid, render(event), render(generation), render(revision), render(verdict)}
	payload := strings.Join(parts, "|")
	if digest != nil {
		payload += "|criteria=" + render(digest)
	}
	if ruling != nil {
		payload += "|ruling=" + render(ruling)
	}
	return payload
}

func renderVerdictSummary(r Relationship, event Row, verdict string, findings []any, record Obj, digest any, ruling int64) string {
	lines := []string{
		fmt.Sprintf("%s · %s · %s", r.IssueKey, r.Child.TaskID, verdict),
		fmt.Sprintf("generation %d, revision %s", event.I("execution_generation"), event.S("revision_hash")[:12]),
	}
	if d, ok := digest.(string); ok && d != "" {
		line := "criteria set " + d[:12]
		if ruling > 1 {
			line += fmt.Sprintf(", ruling %d", ruling)
		} else if ruling == 1 {
			line += ", ruling 1"
		}
		lines = append(lines, line)
	}
	if next, _ := get(record, "nextExecutionGeneration"); truthy(next) {
		lines = append(lines, fmt.Sprintf("a revision request was queued to the same child under generation %s", pyStr(next)))
	}
	if len(findings) > 0 {
		lines = append(lines, "findings:")
		for _, f := range findings {
			o := f.(Obj)
			line := "  " + str(o, "id") + ": " + str(o, "verdict")
			if note := str(o, "note"); note != "" {
				line += " — " + note
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// VerdictSync is the SyncHook record_verdict calls inside its transaction.
func VerdictSync(s *store.Store, clock Clock) SyncHook {
	return func(ctx context.Context, r Relationship, event Row, verdict string, findings []any, record Obj, digest any, ruling int64) error {
		target, err := one(ctx, s, "SELECT * FROM sync_targets WHERE relationship_id = ? AND target = ?", r.ID, coordinationDocument)
		if err != nil || target == nil {
			return err
		}
		var identityRuling any
		if ruling > 1 {
			identityRuling = ruling
		}
		summaryRuling := ruling
		if digest == nil {
			summaryRuling = 0
		}
		canonical := syncCanonical(coordinationDocument, target.S("target_ref"), "verdict", r.ID, event.S("event_id"), event.I("execution_generation"), event.S("revision_hash"), verdict, digest, identityRuling)
		sum := sha256.Sum256([]byte(canonical))
		full := hex.EncodeToString(sum[:])
		id := full[:32]
		now := clock.ISO()
		inserted, err := execSQL(ctx, s, "INSERT OR IGNORE INTO sync_outbox (sync_id, relationship_id, issue_key, target, target_ref, subject_kind, event_id, execution_generation, revision_hash, verdict, identity_digest, summary, state, attempts, next_attempt_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?)",
			id, r.ID, r.IssueKey, coordinationDocument, target.S("target_ref"), "verdict", event.S("event_id"), event.I("execution_generation"), event.S("revision_hash"), verdict, full, renderVerdictSummary(r, event, verdict, findings, record, digest, summaryRuling), "pending", now, now)
		if err != nil {
			return err
		}
		return journal(ctx, s, "sync_enqueued", id, Obj{{Key: "subjectKind", Value: "verdict"}, {Key: "eventId", Value: event.S("event_id")}, {Key: "criteriaDigest", Value: digest}, {Key: "ruling", Value: identityRuling}, {Key: "inserted", Value: inserted == 1}}, now)
	}
}

// SyncClaim is sync.claim: a lease and a per-claim token, which fences completion.
func SyncClaim(ctx context.Context, s *store.Store, clock Clock, id, owner string, now float64) (Obj, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := one(ctx, s, "SELECT * FROM sync_outbox WHERE sync_id = ?", id)
		if err != nil {
			return err
		}
		switch {
		case row == nil:
			return refuse(SyncNotClaimable, "no synchronisation job %s", store.PyRepr(id))
		case row.S("state") == "confirmed":
			return refuse(SyncNotClaimable, "%s is already confirmed", store.PyRepr(id))
		case row.S("state") == "failed":
			return refuse(SyncNotClaimable, "%s is failed after %d attempts; call retry to resume it deliberately", store.PyRepr(id), row.I("attempts"))
		case !row.N("next_attempt_at") && row.F("next_attempt_at") > now:
			return refuse(SyncNotClaimable, "%s is backing off until %s", store.PyRepr(id), pyStr(row.Opt("next_attempt_at")))
		case !row.N("lease_until") && row.F("lease_until") > now:
			return refuse(SyncNotClaimable, "%s is leased by %s until %s", store.PyRepr(id), pyReprValue(row.Opt("lease_owner")), pyStr(row.Opt("lease_until")))
		}
		_, err = execSQL(ctx, s, "UPDATE sync_outbox SET state = ?, lease_owner = ?, lease_until = ?, claim_token = ?, updated_at = ? WHERE sync_id = ?", "claimed", owner, now+syncLeaseSeconds, token, clock.ISO(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "syncId", Value: id}, {Key: "claimToken", Value: token}, {Key: "owner", Value: owner}, {Key: "leaseUntil", Value: now + syncLeaseSeconds}}, nil
}

// SyncFenced is sync._fenced: only the claim currently held may act on a job.
func SyncFenced(ctx context.Context, s *store.Store, id, token string) (Row, error) {
	row, err := one(ctx, s, "SELECT * FROM sync_outbox WHERE sync_id = ?", id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(SyncNotClaimable, "no synchronisation job %s", store.PyRepr(id))
	}
	if row.S("claim_token") != token {
		return nil, refuse(SyncNotClaimable, "this claim token is not the one currently held for %s", store.PyRepr(id))
	}
	return row, nil
}
