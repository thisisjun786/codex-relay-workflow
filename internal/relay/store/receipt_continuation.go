package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ReasonRevisionLineageInvalid is errors.RefusalReason.REVISION_LINEAGE_INVALID.
const ReasonRevisionLineageInvalid = "revision_lineage_invalid"

// continuationClaim is admission.ContinuationClaim: what a child states when it completes on a
// turn other than the anchor.
type continuationClaim struct {
	anchor, actor, reason string
}

// parseContinuation is ContinuationClaim.from_record; a malformed claim is a malformed receipt.
func parseContinuation(raw []byte) (*continuationClaim, error) {
	if raw == nil {
		return nil, nil
	}
	value, err := decodeOrdered(raw)
	if err != nil || value.isNull() {
		if err == nil {
			return nil, nil
		}
		return nil, refuse(ReasonMalformedReceipt, "a continuation claim is an object")
	}
	if value.kind != jsonObject {
		return nil, refuse(ReasonMalformedReceipt, "a continuation claim is an object")
	}
	var missing []string
	for _, field := range []string{"anchorTurnId", "actor", "reason"} {
		if _, ok := value.field(field); !ok {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		quoted := make([]string, len(missing))
		for i, m := range missing {
			quoted[i] = PyRepr(m)
		}
		return nil, refuse(ReasonMalformedReceipt, "a continuation claim needs [%s]", strings.Join(quoted, ", "))
	}
	claim := &continuationClaim{}
	for _, field := range []string{"anchorTurnId", "actor", "reason"} {
		v, _ := value.field(field)
		text, ok := v.text()
		if !ok || strings.TrimSpace(text) == "" {
			return nil, refuse(ReasonMalformedReceipt, "continuation.%s must be a non-empty string", field)
		}
		switch field {
		case "anchorTurnId":
			claim.anchor = text
		case "actor":
			claim.actor = text
		default:
			claim.reason = text
		}
	}
	return claim, nil
}

// checkTurnIdentity is _check_turn_identity with the AnchorOrExplicit strategy and no adapter:
// the thread must be the registered child, and the turn the anchor, a turn already explicitly
// admitted against this anchor, or one the claim admits now (recorded before the rest is
// checked, as Python's record_admission is).
func (in ReceiptIntake) checkTurnIdentity(ctx context.Context, relationship Relationship, generation Generation, turn TurnReference, claim *continuationClaim) error {
	s := in.Store
	if turn.ThreadID != relationship.ChildTaskID {
		return refuse(ReasonUnassignedTurn, "turnRef names thread %s, but the registered child of this relationship is %s", PyRepr(turn.ThreadID), PyRepr(relationship.ChildTaskID))
	}
	anchor := generation.DispatchTurnID
	if anchor.Valid && turn.TurnID == anchor.String {
		return nil
	}
	var evidence string
	err := s.q(ctx).QueryRowContext(ctx, `SELECT t.evidence FROM generation_turns t JOIN generations g ON g.relationship_id=t.relationship_id AND g.execution_generation=t.execution_generation WHERE t.relationship_id=? AND t.execution_generation=? AND t.turn_id=? AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id <> '' AND t.evidence = (?||g.dispatch_turn_id)`, relationship.ID, generation.Number, turn.TurnID, boundExplicitPrefix).Scan(&evidence)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("admitted turn: %w", err)
	}
	anchorRepr := "None"
	if anchor.Valid {
		anchorRepr = PyRepr(anchor.String)
	}
	unadmitted := func(detail string) error {
		return refuse(ReasonUnassignedTurn, "turn %s is not admitted to generation %d (anchor %s): %s", PyRepr(turn.TurnID), generation.Number, anchorRepr, detail)
	}
	if claim == nil {
		return unadmitted("a turn other than the anchor needs an explicit continuation admission naming the generation, its anchor, an actor and a reason")
	}
	if !anchor.Valid || claim.anchor != anchor.String {
		return unadmitted(fmt.Sprintf("the continuation claims anchor %s, but generation %d is anchored to %s", PyRepr(claim.anchor), generation.Number, anchorRepr))
	}
	detail := claim.actor + ": " + claim.reason + " | corroboration=not_corroborated"
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var bound sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT dispatch_turn_id FROM generations WHERE relationship_id=? AND execution_generation=?`, relationship.ID, generation.Number).Scan(&bound); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return refuse(ReasonUnknownGeneration, "admission needs an existing generation")
			}
			return err
		}
		if !bound.Valid || strings.TrimSpace(bound.String) == "" {
			return refuse(ReasonUnboundGeneration, "admission needs a bound generation")
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO generation_turns (relationship_id, execution_generation, turn_id, evidence, actor, detail, admitted_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(relationship_id, execution_generation, turn_id) DO UPDATE SET evidence=excluded.evidence, actor=excluded.actor, detail=excluded.detail, admitted_at=excluded.admitted_at WHERE generation_turns.evidence <> excluded.evidence`,
			relationship.ID, generation.Number, turn.TurnID, boundExplicitPrefix+bound.String, "child", detail, in.Now())
		return err
	})
}

// recordLineage is currency.record_lineage: only a revision declaring itself is refused here;
// forks and unknown predecessors are recorded and surface when the head is read.
func recordLineage(ctx context.Context, conn *sql.Conn, relationshipID string, generation int64, eventID, revision string, supersedes *string, now string) error {
	var declared any
	declaredBy := "undeclared"
	if supersedes != nil && *supersedes != "" {
		text := strings.TrimSpace(*supersedes)
		if text != "" {
			declared = text
			declaredBy = "child_declared"
		}
		if text == revision {
			return refuse(ReasonRevisionLineageInvalid, "a revision cannot supersede itself")
		}
	}
	_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, supersedes_hash, declared_by, recorded_at) VALUES (?,?,?,?,?,?,?)`, relationshipID, generation, eventID, revision, declared, declaredBy, now)
	return err
}

// PyRepr is Python's repr() of a str.
func PyRepr(text string) string {
	quote := "'"
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}
