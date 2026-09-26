package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyRepr(turn)+" is not the landing the currency check reads; it records no base", "", actor)
			} else if other, _ := latest.Get("turn_id").(string); other != turn {
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyRepr(turn)+" is not the landing the currency check reads; that is "+pyRepr(other)+", so restate that one", other, actor)
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
		entries, e := s.Store.MergeLedger(tx, turn)
		if e != nil {
			return e
		}
		keys := map[string]bool{}
		for _, entry := range entries {
			keys[entry.IdempotencyKey] = true
		}
		sequence := nextRestatement(keys)
		from := r.ObservedBaseSHA.String
		body := `{"evidence":` + canonicalJSON(evidence) + `,"from":` + canonicalJSON(from) + `,"sequence":` + sequence + `,"source":` + canonicalJSON(tip.Source) + `,"to":` + canonicalJSON(tip.SHA) + `,"turnId":` + canonicalJSON(turn) + `}`
		if e = s.ledger(tx, turn, "transition", "landed", "landed", "landing_base_restated", actor, body, "restate-base:"+sequence, at); e != nil {
			return e
		}
		if _, e = s.Store.Querier(tx).ExecContext(tx, "UPDATE merge_turns SET observed_base_sha = ?, updated_at = ? WHERE turn_id = ?", tip.SHA, at, turn); e != nil {
			return e
		}
		var previous any
		if r.ObservedBaseSHA.Valid {
			previous = r.ObservedBaseSHA.String
		}
		return s.journalOutcome(tx, "merge_turn_base_restated", turn, contract.OrderedObject{{Key: "from", Value: previous}, {Key: "to", Value: tip.SHA}, {Key: "source", Value: tip.Source}, {Key: "actor", Value: actor}}, at)
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
