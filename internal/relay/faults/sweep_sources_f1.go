package faults

import (
	"context"
	"fmt"
	"strings"
)

func (sw *Sweeper) syncFaults(ctx context.Context, product string, cursor any) (page, error) {
	after, until, e := sw.rotation(ctx, cursor, "SELECT MAX(sync_id) FROM sync_outbox", false)
	if e != nil || until == nil {
		return page{complete: after == nil}, e
	}
	at, _ := after.(string)
	rows, e := sw.Store.All(ctx, "SELECT sync_id,relationship_id,issue_key,target,target_ref,attempts,last_error FROM sync_outbox WHERE state='failed' AND sync_id>? AND sync_id<=? ORDER BY sync_id LIMIT ?", at, until, sweepLimit)
	if e != nil {
		return page{}, e
	}
	cache := map[string]map[string]any{}
	obs := []Observation{}
	for _, r := range rows {
		scope, e := sw.scopeOf(ctx, r.Get("relationship_id"), cache)
		if e != nil {
			return page{}, e
		}
		scope["issueKey"] = r.Get("issue_key")
		target := r.Text("target")
		count := integer(r, "attempts")
		subject := map[string]any{"relationship": r.Get("relationship_id")}
		obs = append(obs, Observation{Product: product, FaultClass: "record_sync_failed", Severity: Broken, Signature: map[string]any{"target": r.Get("target"), "targetRef": r.Get("target_ref")}, OccurrenceKey: fmt.Sprintf("sync:%s:%d", r.Text("sync_id"), count), Scope: scope, Detail: "a " + target + " write exhausted its attempts", Evidence: []any{evidence("row", "sync_outbox:"+r.Text("sync_id"), map[string]any{"attempts": r.Get("attempts"), "lastError": r.Get("last_error"), "relationship": r.Get("relationship_id"), "targetRef": r.Get("target_ref")}), sw.facts("the "+target+" carries this write", fmt.Sprintf("the write gave up after %d attempts: %s", count, pyStr(r.Get("last_error"))), "the "+target+" is behind what the relay recorded", []any{"only the last error of the job is kept"}, subject)}})
	}
	return pageOf(obs, rows, "sync_id", after, until), nil
}
func (sw *Sweeper) observationFaults(ctx context.Context, product string, cursor any) (page, error) {
	key := "(g.relationship_id || ':' || printf('%020d',g.execution_generation))"
	after, until, e := sw.rotation(ctx, cursor, "SELECT MAX("+key+") FROM generations g", false)
	if e != nil || until == nil {
		return page{complete: after == nil}, e
	}
	at, _ := after.(string)
	rows, e := sw.Store.All(ctx, "SELECT g.relationship_id,g.execution_generation,g.dispatch_turn_id,p.turn_id,p.last_polled_at,p.last_attempt_at,p.last_error,p.last_status FROM generations g JOIN relationships r ON r.relationship_id=g.relationship_id AND r.status='active' AND r.superseded_by IS NULL AND r.execution_generation=g.execution_generation LEFT JOIN poll_observations p ON p.relationship_id=g.relationship_id AND p.execution_generation=g.execution_generation AND p.turn_id=g.dispatch_turn_id WHERE g.anchor_state='bound' AND g.dispatch_turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM assignment_settlements s WHERE s.relationship_id=g.relationship_id AND s.turn_id=COALESCE(p.turn_id,g.dispatch_turn_id)) AND p.turn_id IS NOT NULL AND p.last_attempt_at IS NOT NULL AND (p.last_polled_at IS NULL OR p.last_error IS NOT NULL) AND "+key+">? AND "+key+"<=? ORDER BY "+key+" LIMIT ?", at, until, sweepLimit)
	if e != nil {
		return page{}, e
	}
	obs := []Observation{}
	cache := map[string]map[string]any{}
	for _, r := range rows {
		scope, e := sw.scopeOf(ctx, r.Get("relationship_id"), cache)
		if e != nil {
			return page{}, e
		}
		never := r.Get("last_polled_at") == nil
		severity, detail, actual := Degraded, "the most recent poll of this anchor failed", "the most recent poll failed: "+pyStr(r.Get("last_error"))
		if never {
			severity = Broken
			detail = "this anchor has never been successfully polled"
			actual = "no poll of this anchor has succeeded"
		}
		turn := r.Get("turn_id")
		if turn == nil {
			turn = r.Get("dispatch_turn_id")
		}
		rel := r.Text("relationship_id")
		gen := integer(r, "execution_generation")
		obs = append(obs, Observation{Product: product, FaultClass: "observation_stalled", Severity: severity, Signature: map[string]any{"relationship": rel, "generation": gen}, OccurrenceKey: fmt.Sprintf("poll:%s:%d:%s:%s", rel, gen, turn, r.Get("last_attempt_at")), Scope: scope, Detail: detail, Evidence: []any{evidence("row", "poll_observations:"+rel, map[string]any{"turn": turn, "lastPolledAt": r.Get("last_polled_at"), "lastAttemptAt": r.Get("last_attempt_at"), "lastError": r.Get("last_error"), "lastStatus": r.Get("last_status")}), sw.facts("the scheduler reads this anchor successfully", actual, "a turn ending on this anchor is not observed, so its outcome is not delivered", []any{"a poll row keeps only its latest attempt, so earlier failures are not counted"}, map[string]any{"relationship": rel, "generation": gen, "turn": turn})}})
	}
	result := pageOf(obs, rows, "relationship_id", after, until)
	if result.cursor != nil {
		result.cursor = map[string]any{"at": anchorCursor(rows[len(rows)-1].Text("relationship_id"), integer(rows[len(rows)-1], "execution_generation")), "until": until}
	}
	return result, nil
}
func (sw *Sweeper) refusalFaults(ctx context.Context, product string, cursor any) (page, error) {
	after, until, e := sw.rotation(ctx, cursor, "SELECT MAX(seq) FROM journal", true)
	if e != nil || until == nil {
		return page{complete: after == nil}, e
	}
	reason := "(CASE WHEN json_valid(j.detail) THEN json_extract(j.detail, '$.reason') END)"
	streak := "NOT EXISTS (SELECT 1 FROM journal n WHERE n.subject=j.subject AND n.seq>j.seq AND (n.kind IN ('delivery_attempted','delivery_withheld_inactive') OR (n.kind='delivery_withheld' AND COALESCE((CASE WHEN json_valid(n.detail) THEN json_extract(n.detail,'$.reason') END),'')!=COALESCE(" + reason + ",''))))"
	reasons := []string{"environments_unknown", "role_binding_mismatch", "role_policy_unconfigured", "setting_unobservable", "settings_incomplete", "settings_mistyped", "settings_not_preserved", "settings_record_stale_for_role", "settings_unavailable", "unsupported_approval_policy", "unsupported_sandbox_type", "unverifiable_permission_profile"}
	at, _ := after.(int64)
	args := []any{at, until}
	args = append(args, settledDelivery...)
	for _, v := range reasons {
		args = append(args, v)
	}
	args = append(args, sweepLimit)
	rows, e := sw.Store.All(ctx, "SELECT j.seq,j.subject AS event_id,j.at,"+reason+" AS reason,CASE WHEN json_valid(j.detail) THEN json_extract(j.detail,'$.detail') END AS refusal_detail,d.relationship_id,d.recipient_task_id,d.state,e.execution_generation AS generation,e.turn_id AS turn FROM journal j JOIN deliveries d ON d.event_id=j.subject LEFT JOIN events e ON e.event_id=j.subject WHERE j.kind='delivery_withheld' AND j.seq>? AND j.seq<=? AND d.state NOT IN (?,?,?) AND "+notSuperseded+" AND "+reason+" IN ("+strings.TrimRight(strings.Repeat("?,", len(reasons)), ",")+") AND "+streak+" ORDER BY j.seq LIMIT ?", args...)
	if e != nil {
		return page{}, e
	}
	obs := []Observation{}
	scopes := map[string]map[string]any{}
	current := map[string]bool{}
	for _, r := range rows {
		ok, e := sw.current(ctx, r.Text("event_id"), current)
		if e != nil {
			return page{}, e
		}
		if !ok {
			continue
		}
		scope, e := sw.scopeOf(ctx, r.Get("relationship_id"), scopes)
		if e != nil {
			return page{}, e
		}
		refusal := r.Text("reason")
		rel := r.Text("relationship_id")
		event := r.Text("event_id")
		recipient := r.Text("recipient_task_id")
		seq := integer(r, "seq")
		obs = append(obs, Observation{Product: product, FaultClass: "delivery_refused", Severity: Degraded, Signature: map[string]any{"relationship": rel, "errorCode": refusal}, OccurrenceKey: fmt.Sprintf("refused:%d", seq), Scope: scope, Detail: fmt.Sprintf("a delivery to %s was refused before sending: %s", recipient, refusal), Evidence: []any{evidence("row", fmt.Sprintf("journal:%d", seq), map[string]any{"event": event, "reason": refusal, "detail": r.Get("refusal_detail"), "deliveryState": r.Get("state"), "at": r.Get("at")}), sw.facts("the recorded settings pass the check made before sending", "refused before any transport call: "+refusal, "the delivery is withheld and the recipient is not given it", []any{"only the delivery's current refusal streak is counted"}, map[string]any{"event": event, "relationship": rel, "generation": r.Get("generation"), "turn": r.Get("turn")})}})
	}
	return pageOf(obs, rows, "seq", after, until), nil
}

func (sw *Sweeper) derivedStillPresent(ctx context.Context, class string, signature map[string]any) (bool, error) {
	var query string
	var args []any
	switch class {
	case "record_sync_failed":
		query = "SELECT 1 FROM sync_outbox WHERE state='failed' AND target=? AND target_ref=? LIMIT 1"
		args = []any{signature["target"], signature["targetRef"]}
	case "observation_stalled":
		query = "SELECT 1 FROM generations g JOIN relationships r ON r.relationship_id=g.relationship_id AND r.status='active' AND r.superseded_by IS NULL AND r.execution_generation=g.execution_generation LEFT JOIN poll_observations p ON p.relationship_id=g.relationship_id AND p.execution_generation=g.execution_generation AND p.turn_id=g.dispatch_turn_id WHERE g.relationship_id=? AND g.execution_generation=? AND g.anchor_state='bound' AND g.dispatch_turn_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM assignment_settlements s WHERE s.relationship_id=g.relationship_id AND s.turn_id=COALESCE(p.turn_id,g.dispatch_turn_id)) AND p.turn_id IS NOT NULL AND p.last_attempt_at IS NOT NULL AND (p.last_polled_at IS NULL OR p.last_error IS NOT NULL) LIMIT 1"
		args = []any{signature["relationship"], signature["generation"]}
	case "delivery_refused":
		reason := "(CASE WHEN json_valid(j.detail) THEN json_extract(j.detail,'$.reason') END)"
		query = "SELECT d.event_id FROM deliveries d WHERE d.relationship_id=? AND d.state NOT IN (?,?,?) AND " + notSuperseded + " AND EXISTS(SELECT 1 FROM journal j WHERE j.subject=d.event_id AND j.kind='delivery_withheld' AND " + reason + "=? AND NOT EXISTS(SELECT 1 FROM journal n WHERE n.subject=j.subject AND n.seq>j.seq AND (n.kind IN ('delivery_attempted','delivery_withheld_inactive') OR (n.kind='delivery_withheld' AND COALESCE((CASE WHEN json_valid(n.detail) THEN json_extract(n.detail,'$.reason') END),'')!=COALESCE(" + reason + ",''))))) LIMIT 1"
		args = append([]any{signature["relationship"]}, settledDelivery...)
		args = append(args, signature["errorCode"])
	}
	if class == "delivery_refused" {
		query = strings.TrimSuffix(query, " LIMIT 1") + " AND NOT EXISTS(SELECT 1 FROM fault_overtaken_deliveries o WHERE o.event_id=d.event_id) AND d.event_id>? ORDER BY d.event_id LIMIT ?"
		after := ""
		checked := 0
		memo := []string{}
		for {
			parameters := append(append([]any(nil), args...), after, sweepLimit)
			rows, e := sw.Store.All(ctx, query, parameters...)
			if e != nil {
				return false, e
			}
			for _, r := range rows {
				if checked >= presentChecks {
					return true, sw.noteOvertaken(ctx, memo)
				}
				checked++
				if sw.SupersessionReason == nil {
					return true, sw.noteOvertaken(ctx, memo)
				}
				verdict, e := sw.SupersessionReason(ctx, r.Text("event_id"))
				if e != nil {
					return false, e
				}
				if verdict == "" {
					return true, sw.noteOvertaken(ctx, memo)
				}
				memo = append(memo, r.Text("event_id")+"\x00"+verdict)
			}
			if len(rows) < sweepLimit {
				return false, sw.noteOvertaken(ctx, memo)
			}
			after = rows[len(rows)-1].Text("event_id")
		}
	}
	row, e := sw.Store.One(ctx, query, args...)
	return row != nil, e
}
