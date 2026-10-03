package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// restateKey is mergeturn._RESTATE_KEY, \Arestate-base:([0-9]{1,4000})\Z; RE2 caps a repeat at
// 1000, so the length bound is checked beside it.
var restateKey = regexp.MustCompile(`\Arestate-base:([0-9]+)\z`)

// nextRestatement is the sequence restate_base picks: one past every restate-base:<n> key the
// turn holds, then the first number whose key no row uses. Numbers are compared as decimal
// strings, because Python's int has no width.
func nextRestatement(keys map[string]bool) string {
	best := "0"
	for key := range keys {
		m := restateKey.FindStringSubmatch(key)
		if m == nil || len(m[1]) > 4000 {
			continue
		}
		n := strings.TrimLeft(m[1], "0")
		if n == "" {
			n = "0"
		}
		if len(n) > len(best) || len(n) == len(best) && n > best {
			best = n
		}
	}
	candidate := increment(best)
	for range len(keys) + 1 {
		if !keys["restate-base:"+candidate] {
			break
		}
		candidate = increment(candidate)
	}
	return candidate
}

// increment adds one to a non-negative decimal string.
func increment(n string) string {
	digits := []byte(n)
	for i := len(digits) - 1; i >= 0; i-- {
		if digits[i] < '9' {
			digits[i]++
			return string(digits)
		}
		digits[i] = '0'
	}
	return "1" + string(digits)
}

// RestateBase is MergeTurn.restate_base: re-read the base a landing left behind, and record
// it beside the value it replaces.
func (s *Service) RestateBase(ctx context.Context, turn, actor, stated, evidence string, reader Reader) (map[string]any, error) {
	if strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "a restatement states why the recorded base is being read again"}
	}
	if stated != "" {
		if _, err := registry.CoordinationExact(stated, "an observed base sha"); err != nil {
			return nil, err
		}
	}
	early, err := s.Store.MergeTurn(ctx, turn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	tip := Tip{}
	why := "the turn was not a landing this caller may restate when the call began, and changed during it; call again"
	if err == nil && early.State == "landed" {
		if refusal, e := s.authority(ctx, early, actor, "restate its base"); e != nil {
			return nil, e
		} else if refusal == nil {
			tip, why = readTarget(ctx, reader, early.Repository, early.BaseRef)
		}
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	answered := false
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		if refusal, e = s.authority(tx, r, actor, "restate its base"); e != nil {
			return e
		}
		if refusal == nil && r.State != "landed" {
			refusal = wrongState(r, actor, "restating a landing's base")
		}
		if refusal == nil {
			latest, e := s.latestLanding(tx, r.TargetKey)
			if e != nil {
				return e
			}
			if latest == nil {
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyvalue.StrRepr(turn)+" is not the landing the currency check reads; it records no base", "", actor)
			} else if other, _ := latest.Get("turn_id").(string); other != turn {
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyvalue.StrRepr(turn)+" is not the landing the currency check reads; that is "+pyvalue.StrRepr(other)+", so restate that one", other, actor)
			}
		}
		if refusal == nil {
			flight, e := s.Store.One(tx, "SELECT turn_id FROM merge_turns WHERE target_key = ? AND state IN (?,?)  ORDER BY turn_id LIMIT 1", r.TargetKey, Merging, Unknown)
			if e != nil {
				return e
			}
			if flight != nil {
				inFlight, e := s.Store.MergeTurn(tx, flight.Get("turn_id").(string))
				if e != nil {
					return e
				}
				refusal = unresolved(inFlight, actor)
			}
		}
		if refusal == nil && tip.SHA == "" {
			refusal = unreadableTarget(r, actor, why, "the recorded base cannot be read again")
		}
		if refusal == nil && stated != "" && !SameCommit(stated, tip.SHA) {
			refusal = mismatch(r, actor, stated, tip, "--observed-base-sha", true)
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if r.ObservedBaseSHA.Valid && tip.SHA == r.ObservedBaseSHA.String {
			answered = true
			return nil
		}
		_, e = s.writeRestatement(tx, r, tip, actor, func(sequence string) string {
			return `{"evidence":` + canonicalJSON(evidence) + `,"from":` + canonicalJSON(r.ObservedBaseSHA.String) + `,"sequence":` + sequence + `,"source":` + canonicalJSON(tip.Source) + `,"to":` + canonicalJSON(tip.SHA) + `,"turnId":` + canonicalJSON(turn) + `}`
		}, nil, at)
		return e
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	answer, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	answer["baseObservation"] = observation(tip)
	answer["restated"] = !answered
	return answer, nil
}

// writeRestatement records tip as the base a landing leaves behind, beside the value it
// replaces, inside the caller's transaction: one restate-base ledger entry (its body made from the
// sequence it takes), the landing's observed_base_sha and the journal row. extra is appended to the
// journal row's fields. It is the one writer of a restatement, for the command and for a check.
func (s *Service) writeRestatement(ctx context.Context, r store.MergeTurnsRow, tip Tip, actor string, body func(sequence string) string, extra contract.OrderedObject, at string) (string, error) {
	entries, err := s.Store.MergeLedger(ctx, r.TurnID)
	if err != nil {
		return "", err
	}
	keys := map[string]bool{}
	for _, entry := range entries {
		keys[entry.IdempotencyKey] = true
	}
	sequence := nextRestatement(keys)
	if err = s.ledger(ctx, r.TurnID, "transition", "landed", "landed", "landing_base_restated", actor, body(sequence), "restate-base:"+sequence, at); err != nil {
		return "", err
	}
	if _, err = s.Store.Querier(ctx).ExecContext(ctx, "UPDATE merge_turns SET observed_base_sha = ?, updated_at = ? WHERE turn_id = ?", tip.SHA, at, r.TurnID); err != nil {
		return "", err
	}
	var previous any
	if r.ObservedBaseSHA.Valid {
		previous = r.ObservedBaseSHA.String
	}
	detail := contract.OrderedObject{{Key: "from", Value: previous}, {Key: "to", Value: tip.SHA}, {Key: "source", Value: tip.Source}, {Key: "actor", Value: actor}}
	return sequence, s.journalOutcome(ctx, "merge_turn_base_restated", r.TurnID, append(detail, extra...), at)
}

// restateAfterOutOfLaneMerge is what a check does inside its transaction when the base the last
// landing recorded is not the base the candidate states, which the tip read for this check equals.
// When the reading taken before the transaction confirms the move as merge commits on the first-parent
// line that no landing on the target records, it records the base again, as merge-turn-restate-base
// would, and returns what it wrote. Otherwise it returns the refusal detail: nothing is written
// unless the cause is confirmed.
func (s *Service) restateAfterOutOfLaneMerge(ctx context.Context, row store.MergeTurnsRow, landing store.Row, actor, base string, tip Tip, moved *moveReading, checkID, at string) (map[string]any, string, error) {
	landed := landing.Text("turn_id")
	recorded := landing.Text("observed_base_sha")
	unconfirmed := func(why string) (map[string]any, string, error) {
		return nil, staleLandingDetail(row, landing, base, tip.SHA, why), nil
	}
	if moved == nil || moved.landing != landed || !SameCommit(moved.from, recorded) || !SameCommit(moved.to, tip.SHA) {
		return unconfirmed("the landing record changed while the branch was being read; call again")
	}
	if moved.why != "" {
		return unconfirmed(moved.why)
	}
	flight, err := s.Store.One(ctx, "SELECT turn_id FROM merge_turns WHERE target_key = ? AND state IN (?,?)  ORDER BY turn_id LIMIT 1", row.TargetKey, Merging, Unknown)
	if err != nil {
		return nil, "", err
	}
	if flight != nil {
		return unconfirmed("turn " + pyvalue.StrRepr(flight.Text("turn_id")) + " is merging or unknown on this target, so its landing is not yet known")
	}
	landings, err := s.Store.All(ctx, "SELECT turn_id, landed_sha FROM merge_turns WHERE target_key = ? AND state = 'landed' AND landed_sha IS NOT NULL", row.TargetKey)
	if err != nil {
		return nil, "", err
	}
	landedShas := map[string]string{}
	for _, l := range landings {
		landedShas[strings.ToLower(l.Text("landed_sha"))] = l.Text("turn_id")
	}
	merges, why := outsideMerges(moved.movement, recorded, tip.SHA, landedShas)
	if why != "" {
		return unconfirmed(why)
	}
	landingRow, err := s.row(ctx, landed)
	if err != nil {
		return nil, "", err
	}
	described := make([]string, len(merges))
	commits := make([]any, len(merges))
	for i, step := range merges {
		subject := cleanSubject(step.Subject)
		described[i] = step.SHA + " (" + subject + ")"
		commits[i] = contract.OrderedObject{{Key: "sha", Value: step.SHA}, {Key: "subject", Value: subject}}
	}
	evidence := "automatic: merge-turn-check of turn " + row.TurnID + " by " + actor + " read that " + row.BaseRef + " of " + row.Repository + " moved from " + recorded + " to " + tip.SHA +
		" through " + strconv.Itoa(len(merges)) + " merge commit(s) on its first-parent line that no landing on this target records (read from " + tip.Source + "): " + strings.Join(described, "; ")
	sequence, err := s.writeRestatement(ctx, landingRow, tip, actor, func(sequence string) string {
		return canonicalJSON(contract.OrderedObject{
			{Key: "automatic", Value: true}, {Key: "checkId", Value: checkID}, {Key: "checkTurnId", Value: row.TurnID},
			{Key: "evidence", Value: evidence}, {Key: "from", Value: recorded}, {Key: "mergeCommits", Value: commits},
			{Key: "sequence", Value: json.Number(sequence)}, {Key: "source", Value: tip.Source}, {Key: "to", Value: tip.SHA}, {Key: "turnId", Value: landed},
		})
	}, contract.OrderedObject{{Key: "automatic", Value: true}, {Key: "checkTurnId", Value: row.TurnID}}, at)
	if err != nil {
		return nil, "", err
	}
	return map[string]any{"turnId": landed, "from": recorded, "to": tip.SHA, "sequence": json.Number(sequence), "source": tip.Source, "mergeCommits": commits}, "", nil
}
