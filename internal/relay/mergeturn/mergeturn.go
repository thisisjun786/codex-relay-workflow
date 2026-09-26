// Package mergeturn coordinates exclusive claims on repository base branches.
package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	Waiting = "waiting"
	Holding = "holding"
	Merging = "merging"
	Unknown = "unknown"
)

type Service struct {
	Store    *store.Store
	Registry *registry.Registry
	Now      func() string
	// Delivery is MergeTurn's delivery: absent, a grant carries no wake key at all, as in
	// Python; the relay CLI always supplies one.
	Delivery Delivery
}

// Delivery is what a promotion hands its grant to (delivery.py grant_channel_in and
// queue_grant_in). Channel only reads; Queue writes inside the promotion's transaction.
type Delivery interface {
	// Channel answers ("", eventID) when the grant can reach its recipient, or (why, "").
	Channel(ctx context.Context, relationship sql.NullString, recipient, grant, project string) (refused, eventID string, err error)
	Queue(ctx context.Context, eventID, relationship, recipient, receipt, grant, at string) error
}

func (s *Service) now() string {
	if s.Now != nil {
		return s.Now()
	}
	return registry.SystemISO()
}
func key(parts ...string) string { return registry.CoordinationID(parts[0], parts[1:]...) }
func TargetKey(repository, base string) (string, error) {
	for _, p := range []struct{ v, n string }{{repository, "a repository"}, {base, "a base ref"}} {
		if _, err := registry.CoordinationExact(p.v, p.n); err != nil {
			return "", err
		}
	}
	return key("tgt", repository, base), nil
}
func TurnID(target, holder string, tenure int64) string {
	return key("mtn", target, holder, fmt.Sprint(tenure))
}
func GrantID(turn string, tenure, sequence int64) string {
	return key("mtg", turn, fmt.Sprint(tenure), fmt.Sprint(sequence))
}
func ledgerID(turn, identity string) string { return key("mte", turn, identity) }
func nullable(v string) sql.NullString      { return sql.NullString{String: v, Valid: v != ""} }
func value(v sql.NullString) any {
	if v.Valid {
		return v.String
	}
	return nil
}
func (s *Service) ledger(ctx context.Context, id, kind, from, to, evidence, actor, text, identity, at string) error {
	wanted := store.MergeTurnLedgerRow{EntryID: ledgerID(id, identity), TurnID: id, Kind: kind, FromState: nullable(from), ToState: nullable(to), EvidenceKind: evidence, ActorTaskID: actor, Evidence: text, IdempotencyKey: identity, RecordedAt: at}
	if err := s.Store.WriteMergeLedger(ctx, wanted); err != nil {
		return err
	}
	if !engineLedgerKind(evidence) {
		return nil
	}
	seen, err := s.Store.MergeLedgerEntry(ctx, id, identity)
	if err != nil {
		return err
	}
	if seen.Kind != wanted.Kind || seen.FromState != wanted.FromState || seen.ToState != wanted.ToState || seen.EvidenceKind != wanted.EvidenceKind || seen.ActorTaskID != wanted.ActorTaskID || seen.Evidence != wanted.Evidence {
		return &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "turn " + pyRepr(id) + " already holds " + pyRepr(identity) + " as " + pyRepr(seen.EvidenceKind) + " by " + pyRepr(seen.ActorTaskID) + ", so recording " + pyRepr(evidence) + " under it would be discarded without anything saying so; this ledger is inconsistent and the operation is rolled back rather than half written"}
	}
	return nil
}
func engineLedgerKind(kind string) bool {
	for _, name := range []string{"claim", "close", "candidate_head_changed", "took_free_target", "promoted", "currency_confirmed", "outcome_unknown", "grant", "grant_acknowledged", "readiness_declared", "readiness_withdrawn", "landing_base_restated"} {
		if kind == name {
			return true
		}
	}
	return false
}
func (s *Service) grant(ctx context.Context, r store.MergeTurnsRow, source, at string) error {
	entries, err := s.Store.MergeLedger(ctx, r.TurnID)
	if err != nil {
		return err
	}
	sequence := int64(1)
	for _, e := range entries {
		if e.EvidenceKind == "grant" {
			sequence++
		}
	}
	id := GrantID(r.TurnID, r.Tenure, sequence)
	envelope := map[string]any{"kind": "merge_turn_grant", "grantId": id, "turnId": r.TurnID, "tenure": r.Tenure, "sequence": sequence, "targetKey": r.TargetKey, "repository": r.Repository, "baseRef": r.BaseRef, "recipientTaskId": r.HolderTaskID, "candidateHead": r.CandidateHead, "grantedFrom": source}
	// Only a promotion wakes its recipient (_grant_in wake=True), and the address is decided
	// before the row is written, so the envelope is never amended afterwards.
	queued := ""
	if source == "promotion" && s.Delivery != nil {
		refused, eventID, err := s.Delivery.Channel(ctx, r.RelationshipID, r.HolderTaskID, id, r.ProjectKey)
		if err != nil {
			return err
		}
		if eventID != "" {
			envelope["wake"] = map[string]any{"eventId": eventID}
			queued = eventID
		} else {
			envelope["wake"] = map[string]any{"refused": refused}
			if err := s.journalOutcome(ctx, "merge_turn_wake_unaddressed", r.TurnID, contract.OrderedObject{{Key: "grantId", Value: id}, {Key: "recipientTaskId", Value: r.HolderTaskID}, {Key: "reason", Value: refused}}, at); err != nil {
				return err
			}
		}
	}
	evidence := canonicalJSON(envelope)
	if err := s.ledger(ctx, r.TurnID, "attestation", r.State, "", "grant", r.HolderTaskID, evidence, "grant:"+id, at); err != nil {
		return err
	}
	if queued == "" {
		return nil
	}
	if err := s.Delivery.Queue(ctx, queued, r.RelationshipID.String, r.HolderTaskID, evidence, id, at); err != nil {
		return err
	}
	return s.journalOutcome(ctx, "merge_turn_wake_queued", r.TurnID, contract.OrderedObject{{Key: "grantId", Value: id}, {Key: "eventId", Value: queued}, {Key: "recipientTaskId", Value: r.HolderTaskID}}, at)
}
func (s *Service) ownership(ctx context.Context, project, subject, actor string) (string, *registry.CoordinationRefusal, error) {
	owners, err := s.Registry.Owners(ctx, "project", project)
	if err != nil {
		return "", nil, err
	}
	var parents []string
	for _, o := range owners {
		var role, task string
		for _, field := range o {
			if field.Key == "role" {
				role, _ = field.Value.(string)
			}
			if field.Key == "taskId" {
				task, _ = field.Value.(string)
			}
		}
		if role == "parent" {
			parents = append(parents, task)
		}
	}
	if len(parents) == 0 {
		return "", &registry.CoordinationRefusal{Reason: contract.RefusalUnregisteredScope, Detail: "project " + pyRepr(project) + " has no registered parent, so nobody can claim a merge turn for it", Domain: registry.DomainMergeTarget, Subject: subject, Challenger: actor}, nil
	}
	if len(parents) > 1 {
		slices.Sort(parents)
		quoted := make([]string, len(parents))
		for i, task := range parents {
			quoted[i] = pyRepr(task)
		}
		return "", &registry.CoordinationRefusal{Reason: contract.RefusalDuplicateScopeOwner, Detail: "project " + pyRepr(project) + " has more than one live parent (" + strings.Join(quoted, ", ") + "), so there is no owner to hold a merge turn under; repair the store rather than letting one of them win", Domain: registry.DomainMergeTarget, Subject: subject, Incumbent: parents[0], Challenger: actor}, nil
	}
	return parents[0], nil, nil
}

type ClaimOptions struct {
	PR           sql.NullInt64
	Relationship sql.NullString
}

func (s *Service) Request(ctx context.Context, repository, base, project, holder, host, head string, ready bool, options ...ClaimOptions) (map[string]any, error) {
	target, err := TargetKey(repository, base)
	if err != nil {
		return nil, err
	}
	for _, f := range []struct{ v, n string }{{project, "a project key"}, {holder, "a task id"}, {host, "a host id"}, {head, "a candidate head"}} {
		if _, err = registry.CoordinationExact(f.v, f.n); err != nil {
			return nil, err
		}
	}
	at := s.now()
	var id string
	replayed := false
	var refusal *registry.CoordinationRefusal
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		owner, r, e := s.ownership(tx, project, target, holder)
		if e != nil {
			return e
		}
		refusal = r
		if refusal == nil && owner != holder {
			refusal = &registry.CoordinationRefusal{Reason: contract.RefusalScopeRoleMismatch, Detail: "task " + pyRepr(holder) + " is not the registered parent of project " + pyRepr(project) + ", which is held by " + pyRepr(owner) + "; holding a merge turn is the project parent's, and saying it is your turn is not being its parent", Domain: registry.DomainMergeTarget, Subject: target, Incumbent: owner, Challenger: holder}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		live, e := s.Store.LiveMergeClaim(tx, target, holder)
		if e == nil {
			id = live.TurnID
			replayed = true
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		occupied, e := s.Store.MergeTargetOccupant(tx, target)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		tenure, e := s.Store.HighestMergeTenure(tx, target, holder)
		if e != nil {
			return e
		}
		tenure++
		id = TurnID(target, holder, tenure)
		state := Holding
		held := nullable(at)
		owners, e := s.Registry.Owners(tx, "project", project)
		if e != nil {
			return e
		}
		for _, o := range owners {
			for _, f := range o {
				if f.Key == "status" && f.Value != "active" {
					state = Waiting
				}
			}
		}
		if occupied.TurnID != "" {
			state = Waiting
		}
		if state == Waiting {
			held = nullable("")
		}
		flag := int64(0)
		if ready {
			flag = 1
		}
		rrow := store.MergeTurnsRow{TurnID: id, TargetKey: target, Repository: repository, BaseRef: base, ProjectKey: project, HolderTaskID: holder, HolderHostID: host, CandidateHead: head, DeclaredReady: flag, State: state, Tenure: tenure, RequestedAt: at, HeldAt: held, UpdatedAt: at}
		if len(options) > 0 {
			rrow.PRNumber = options[0].PR
			rrow.RelationshipID = options[0].Relationship
		}
		if e = s.Store.InsertMergeTurn(tx, rrow); e != nil {
			return e
		}
		if e = s.ledger(tx, id, "transition", "", state, "claim", holder, "requested "+repository+" "+base, fmt.Sprintf("request:%d", tenure), at); e != nil {
			return e
		}
		if state == Holding {
			if e = s.grant(tx, rrow, "claim", at); e != nil {
				return e
			}
		}
		_, e = s.Store.Querier(tx).ExecContext(tx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)", at, "merge_turn_requested", id, pythonJSON(contract.OrderedObject{{Key: "targetKey", Value: target}, {Key: "state", Value: state}, {Key: "holder", Value: holder}}))
		return e
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	answer, err := s.Turn(ctx, id)
	if err != nil {
		return nil, err
	}
	if replayed {
		answer["alreadyClaimed"] = true
	}
	return answer, nil
}
func record(r store.MergeTurnsRow) map[string]any {
	return map[string]any{"turnId": r.TurnID, "targetKey": r.TargetKey, "repository": r.Repository, "baseRef": r.BaseRef, "projectKey": r.ProjectKey, "holderTaskId": r.HolderTaskID, "holderHostId": r.HolderHostID, "relationshipId": value(r.RelationshipID), "prNumber": func() any {
		if r.PRNumber.Valid {
			return r.PRNumber.Int64
		}
		return nil
	}(), "candidateHead": r.CandidateHead, "declaredReady": r.DeclaredReady == 1, "state": r.State, "tenure": r.Tenure, "landedSha": value(r.LandedSHA), "observedBaseSha": value(r.ObservedBaseSHA), "checkedBaseSha": value(r.CheckedBaseSHA), "closeReason": value(r.CloseReason), "requestedAt": r.RequestedAt, "heldAt": value(r.HeldAt), "mergingAt": value(r.MergingAt), "closedAt": value(r.ClosedAt), "updatedAt": r.UpdatedAt}
}

// recognizedGrant is mergeturn.grant_envelope: one ledger entry read as a grant this engine
// wrote for this turn and tenure, or nil. Integers stay json.Number, as json.loads keeps ints.
func recognizedGrant(entry store.MergeTurnLedgerRow, turn string, tenure int64) map[string]any {
	if entry.EvidenceKind != "grant" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(entry.Evidence))
	decoder.UseNumber()
	var g map[string]any
	if decoder.Decode(&g) != nil || g == nil || decoder.More() {
		return nil
	}
	number, ok := g["sequence"].(json.Number)
	if !ok || strings.ContainsAny(string(number), ".eE") {
		return nil
	}
	seq, err := number.Int64()
	if err != nil {
		return nil
	}
	stated, ok := g["tenure"].(json.Number)
	if !ok || g["turnId"] != turn {
		return nil
	}
	if value, err := stated.Float64(); err != nil || value != float64(tenure) {
		return nil
	}
	id := GrantID(turn, tenure, seq)
	if g["grantId"] != id || entry.IdempotencyKey != "grant:"+id {
		return nil
	}
	recipient, ok := g["recipientTaskId"].(string)
	if !ok || recipient == "" {
		return nil
	}
	head, ok := g["candidateHead"].(string)
	if !ok || head == "" {
		return nil
	}
	return g
}

// grantSequence is a recognised grant's sequence.
func grantSequence(g map[string]any) int64 {
	n, _ := g["sequence"].(json.Number).Int64()
	return n
}

func (s *Service) Turn(ctx context.Context, id string) (map[string]any, error) {
	r, err := s.Store.MergeTurn(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := record(r)
	entries, err := s.Store.MergeLedger(ctx, id)
	if err != nil {
		return nil, err
	}
	ledger := make([]any, 0, len(entries))
	var grant map[string]any
	unreadable := []string{}
	for _, e := range entries {
		ledger = append(ledger, map[string]any{"entryId": e.EntryID, "kind": e.Kind, "fromState": value(e.FromState), "toState": value(e.ToState), "evidenceKind": e.EvidenceKind, "actorTaskId": e.ActorTaskID, "evidence": e.Evidence, "idempotencyKey": e.IdempotencyKey, "recordedAt": e.RecordedAt})
		if e.EvidenceKind == "grant" {
			g := recognizedGrant(e, id, r.Tenure)
			if g != nil {
				g["recordedAt"] = e.RecordedAt
				g["acknowledgedAt"] = nil
				g["acknowledgedBy"] = nil
				if grant == nil || grantSequence(g) > grantSequence(grant) {
					grant = g
				}
			} else {
				unreadable = append(unreadable, e.IdempotencyKey)
			}
		}
	}
	if grant != nil {
		for _, e := range entries {
			if e.IdempotencyKey == "grant_acknowledged:"+grant["grantId"].(string) {
				grant["acknowledgedAt"] = e.RecordedAt
				grant["acknowledgedBy"] = e.ActorTaskID
			}
		}
	}
	out["ledger"] = ledger
	if grant == nil {
		out["grant"] = nil
	} else {
		out["grant"] = grant
	}
	out["unreadableGrants"] = unreadable
	restatements := make([]any, 0)
	unreadRestatements := make([]any, 0)
	for _, entry := range entries {
		if entry.EvidenceKind != "landing_base_restated" {
			continue
		}
		envelope := restatementEnvelope(entry, id)
		if envelope == nil {
			unreadRestatements = append(unreadRestatements, entry.IdempotencyKey)
			continue
		}
		envelope["actorTaskId"] = entry.ActorTaskID
		envelope["recordedAt"] = entry.RecordedAt
		restatements = append(restatements, envelope)
	}
	slices.SortStableFunc(restatements, func(a, b any) int {
		x, y := string(a.(map[string]any)["sequence"].(json.Number)), string(b.(map[string]any)["sequence"].(json.Number))
		if len(x) != len(y) {
			return len(x) - len(y)
		}
		return strings.Compare(x, y)
	})
	out["baseRestatements"] = restatements
	out["unreadableRestatements"] = unreadRestatements
	return out, nil
}

// Outstanding returns a parent's live claims in request order, including the current grant.
func (s *Service) Outstanding(ctx context.Context, task string) ([]map[string]any, error) {
	if _, err := registry.CoordinationExact(task, "a task id"); err != nil {
		return nil, err
	}
	claims, err := s.Store.OutstandingMergeClaims(ctx, task)
	if err != nil {
		return nil, err
	}
	answer := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		record, err := s.Turn(ctx, claim.TurnID)
		if err != nil {
			return nil, err
		}
		why := "already " + claim.State
		if claim.State == Waiting {
			why = ""
			occupant, err := s.Store.MergeTargetOccupant(ctx, claim.TargetKey)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if occupant.TurnID != "" {
				why = "target_occupied"
			} else if why, err = s.withheld(ctx, record["projectKey"].(string), task); err != nil {
				return nil, err
			}
		}
		record["targetFree"] = why == ""
		if why == "" {
			record["heldBackBy"] = nil
		} else {
			record["heldBackBy"] = why
		}
		answer = append(answer, record)
	}
	return answer, nil
}

// Target reports occupancy without interpreting a waiter's claim as a grant.
func (s *Service) Target(ctx context.Context, repository, base string) (map[string]any, error) {
	key, err := TargetKey(repository, base)
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.MergeTurnsForTarget(ctx, key)
	if err != nil {
		return nil, err
	}
	waiters := make([]any, 0)
	var holder any
	for _, row := range rows {
		if row.State == Waiting {
			waiters = append(waiters, record(row))
		}
		if row.State == Holding || row.State == Merging || row.State == Unknown {
			h, err := s.Turn(ctx, row.TurnID)
			if err != nil {
				return nil, err
			}
			holder = h
		}
	}
	conflicts, err := s.Registry.CoordinationConflicts(ctx, registry.DomainMergeTarget, key)
	if err != nil {
		return nil, err
	}
	var next any
	readyPeers := make([]any, 0)
	withheldPeers := make([]any, 0)
	for _, item := range waiters {
		waiter := item.(map[string]any)
		if !waiter["declaredReady"].(bool) {
			continue
		}
		peer := map[string]any{"turnId": waiter["turnId"], "holderTaskId": waiter["holderTaskId"], "candidateHead": waiter["candidateHead"], "prNumber": waiter["prNumber"]}
		why, err := s.withheld(ctx, waiter["projectKey"].(string), waiter["holderTaskId"].(string))
		if err != nil {
			return nil, err
		}
		if why != "" {
			peer["reason"] = why
			withheldPeers = append(withheldPeers, peer)
		} else {
			readyPeers = append(readyPeers, peer)
			if next == nil {
				next = waiter
			}
		}
	}
	var blocked any
	contests := make([]any, len(conflicts))
	for i, c := range conflicts {
		contests[i] = c
	}
	answer := map[string]any{"targetKey": key, "repository": repository, "baseRef": base, "holder": holder, "waiters": waiters, "nextReady": next, "occupied": holder != nil, "conflicts": contests}
	if holder != nil {
		h := holder.(map[string]any)
		entries, err := s.Store.MergeLedger(ctx, h["turnId"].(string))
		if err != nil {
			return nil, err
		}
		var requested, accepted any
		for _, e := range entries {
			if e.EvidenceKind == "return_requested" && requested == nil {
				requested = e.RecordedAt
			}
			if e.EvidenceKind == "transport_accepted" && accepted == nil {
				accepted = e.RecordedAt
			}
		}
		answer["returnRequestedAt"] = requested
		answer["transportAcceptedAt"] = accepted
		answer["releasedAt"] = h["closedAt"]
		checks, err := s.Store.MergeChecks(ctx, h["turnId"].(string))
		if err != nil {
			return nil, err
		}
		current := make([]store.MergeTurnChecksRow, 0)
		moved := make([]store.MergeTurnChecksRow, 0)
		for _, c := range checks {
			if c.HeadSHA == h["candidateHead"] {
				current = append(current, c)
			}
			if c.RefusalReason.String == "merge_candidate_moved" {
				moved = append(moved, c)
			}
		}
		considered := current
		if len(considered) == 0 {
			considered = moved
		}
		var latest *store.MergeTurnChecksRow
		tied := make([]any, 0)
		if len(considered) > 0 {
			first := considered[0]
			latest = &first
			for _, c := range considered[1:] {
				if c.RecordedAt != first.RecordedAt {
					break
				}
				if c.Result != first.Result || c.RefusalReason.String != first.RefusalReason.String {
					latest = nil
				}
				tied = append(tied, c.CheckID)
			}
			if latest == nil {
				tied = append(tied, first.CheckID)
				slices.SortFunc(tied, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
			} else {
				tied = tied[:0]
			}
		}
		var lastResult, lastRefusal, lastHead any
		if latest != nil {
			lastResult = latest.Result
			lastHead = latest.HeadSHA
			lastRefusal = value(latest.RefusalReason)
		}
		holderWhy, err := s.withheld(ctx, h["projectKey"].(string), h["holderTaskId"].(string))
		if err != nil {
			return nil, err
		}
		var cause string
		switch {
		case h["state"] == Merging:
			cause = "merge_in_flight"
		case h["state"] == Unknown:
			cause = "outcome_unknown"
		case holderWhy == "not_the_project_owner":
			cause = "holder_no_longer_owns_the_project"
		case holderWhy == "owner_paused":
			cause = "holder_paused"
		case !h["declaredReady"].(bool):
			cause = "candidate_not_ready"
		case len(tied) > 0:
			cause = "restatements_disagree"
		case latest != nil && latest.Result == "refused":
			cause = map[string]string{"merge_currency_stale": "required_evidence_not_current", "merge_review_incomplete": "review_not_finished", "merge_candidate_moved": "candidate_moved", "merge_target_unreadable": "target_unreadable"}[latest.RefusalReason.String]
			if cause == "" {
				cause = "restatement_refused"
			}
		default:
			cause = "candidate_not_restated"
		}
		blocked = map[string]any{"cause": cause, "turnId": h["turnId"], "holderTaskId": h["holderTaskId"], "candidateHead": h["candidateHead"], "prNumber": h["prNumber"], "checkSnapshots": len(current), "lastResult": lastResult, "lastRefusal": lastRefusal, "lastCheckedHead": lastHead, "ambiguousRestatements": tied, "readyPeers": readyPeers, "withheldPeers": withheldPeers}
		delete(h, "ledger")
		delete(h, "unreadableGrants")
		delete(h, "baseRestatements")
		delete(h, "unreadableRestatements")
	}
	answer["blocked"] = blocked
	return answer, nil
}

// Attest records a transport fact without moving a turn. Engine-owned ledger names are reserved.
func (s *Service) Attest(ctx context.Context, turn, kind, identity, actor, evidence string) (map[string]any, error) {
	squatted := ""
	if engineLedgerKind(kind) {
		squatted = "evidence kind " + pyRepr(kind)
	} else {
		for _, prefix := range []string{"request:", "close:", "head:", "take:", "promote:", "merging:", "unknown:", "ready:", "grant:", "grant_acknowledged:", "restate-base:"} {
			if strings.HasPrefix(identity, prefix) {
				squatted = "idempotency key " + pyRepr(identity) + ", which is in the " + pyRepr(prefix) + " namespace"
				break
			}
		}
	}
	if squatted != "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: squatted + " belongs to the merge turn itself. Attesting is for facts that reached a turn from outside it, such as a transport accepting a message; the turn's own transitions, grants and acknowledgements are written by the operations that cause them"}
	}
	at := s.now()
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		row, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		if _, err = registry.CoordinationExact(kind, "an evidence kind"); err != nil {
			return err
		}
		return s.ledger(tx, turn, "attestation", row.State, "", kind, actor, evidence, identity, at)
	})
	if err != nil {
		return nil, err
	}
	return s.Turn(ctx, turn)
}

// Acknowledge is MergeTurn.acknowledge_grant: a parent acting on a grant it read, checked
// against what is true now.
func (s *Service) Acknowledge(ctx context.Context, turn, actor, grant, evidence string) (map[string]any, error) {
	if strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "acknowledging a grant states what was read; a bare acknowledgement is exactly the remembered message this replaces"}
	}
	if _, err := registry.CoordinationExact(grant, "a grant id"); err != nil {
		return nil, err
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		current, err := s.currentGrant(tx, turn, r.Tenure)
		if err != nil {
			return err
		}
		switch {
		case r.HolderTaskID != actor:
			refusal = notHolder(r, actor, "acknowledge a grant on")
		case r.State != Holding:
			refusal = wrongState(r, actor, "acknowledging a grant")
		case current == "":
			refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyRepr(turn)+" records no grant, so there is nothing here to acknowledge; a claim made before grants were recorded has none and needs none", r.HolderTaskID, actor)
		case grant != current:
			refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "grant "+pyRepr(grant)+" is not the grant for tenure "+fmt.Sprint(r.Tenure)+" of turn "+pyRepr(turn)+", which is "+pyRepr(current)+"; the grant you read was returned before you acted on it", current, grant)
		}
		if refusal == nil {
			owner, other, err := s.ownership(tx, r.ProjectKey, r.TargetKey, actor)
			if err != nil {
				return err
			}
			refusal = other
			if refusal == nil && owner != actor {
				refusal = staleOwner(r, owner, true, actor)
			}
		}
		if refusal == nil {
			status, err := s.ownerStatus(tx, r.ProjectKey)
			if err != nil {
				return err
			}
			if status != "active" {
				refusal = paused(r, actor, "act on a grant")
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		return s.ledger(tx, turn, "attestation", r.State, "", "grant_acknowledged", actor, evidence, "grant_acknowledged:"+current, at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	return s.Turn(ctx, turn)
}

// restatementEnvelope is mergeturn.restatement_envelope: one ledger entry read as a
// restatement restate_base wrote, or nil. The sequence stays an exact integer.
func restatementEnvelope(entry store.MergeTurnLedgerRow, turn string) map[string]any {
	if entry.EvidenceKind != "landing_base_restated" || entry.Kind != "transition" || entry.FromState.String != "landed" || entry.ToState.String != "landed" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(entry.Evidence))
	decoder.UseNumber()
	var envelope map[string]any
	if decoder.Decode(&envelope) != nil || envelope == nil || decoder.More() || envelope["turnId"] != turn {
		return nil
	}
	sequence, ok := envelope["sequence"].(json.Number)
	if !ok || strings.ContainsAny(string(sequence), ".eE") || entry.IdempotencyKey != "restate-base:"+string(sequence) {
		return nil
	}
	for _, name := range []string{"from", "to"} {
		if text, ok := envelope[name].(string); !ok || text == "" {
			return nil
		}
	}
	return map[string]any{"sequence": sequence, "from": envelope["from"], "to": envelope["to"], "evidence": envelope["evidence"], "source": envelope["source"]}
}

// withheld is MergeTurn._withheld: why a claim would not be promoted, or "" when it would.
func (s *Service) withheld(ctx context.Context, project, holder string) (string, error) {
	held, err := s.parents(ctx, project)
	if err != nil {
		return "", err
	}
	if len(held) != 1 || held[0] != holder {
		return "not_the_project_owner", nil
	}
	status, err := s.ownerStatus(ctx, project)
	if err != nil {
		return "", err
	}
	if status != "active" {
		return "owner_paused", nil
	}
	return "", nil
}
