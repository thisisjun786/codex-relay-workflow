package capacity

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The writing half of editregion.py. The write protocol is coordination.py's: open one
// transaction, read and validate completely, then record the conflict alone or write the whole
// operation; the refusal is raised after the transaction closes, so the evidence survives.

// ownedSideOf is EditRegions._owned_side: the project the actor is the registered parent of.
func (e *EditRegions) ownedSideOf(ctx context.Context, low, high, actor string) (string, error) {
	if e.ownedSide != nil {
		return e.ownedSide(ctx, low, high, actor)
	}
	return e.realOwnedSide(ctx, low, high, actor)
}

func (e *EditRegions) realOwnedSide(ctx context.Context, low, high, actor string) (string, error) {
	for _, project := range []string{low, high} {
		parents, err := owners(ctx, e.Store, scopeProject, project, roleParent)
		if err != nil {
			return "", err
		}
		if len(parents) == 1 && parents[0] == actor {
			return project, nil
		}
	}
	return "", nil
}

// actingSide is EditRegions._acting_side: "left_project" or "right_project", or a refusal.
func (e *EditRegions) actingSide(ctx context.Context, r row, actor, subject string) (string, *refusal, error) {
	for _, side := range []string{"left_project", "right_project"} {
		parents, err := owners(ctx, e.Store, scopeProject, text(r, side), roleParent)
		if err != nil {
			return "", nil, err
		}
		if len(parents) != 1 || parents[0] != actor {
			continue
		}
		link, err := e.one(ctx, "SELECT * FROM scope_links WHERE link_id = ?", text(r, "peer_link_id"))
		if err != nil {
			return "", nil, err
		}
		if link == nil || !isLive(text(link, "status")) || text(link, "superseded_by") != "" {
			return "", &refusal{contract.RefusalUnregisteredScope, "peer link " + repr(text(r, "peer_link_id")) +
				" is not live, so these two projects are not registered peers", domainEditRegion, subject, "", actor}, nil
		}
		return side, nil, nil
	}
	return "", &refusal{contract.RefusalScopeRoleMismatch, "task " + repr(actor) + " is the registered parent of neither " +
		repr(text(r, "left_project")) + " nor " + repr(text(r, "right_project")), domainEditRegion, subject, "", actor}, nil
}

// Proposal is EditRegions.propose's keyword arguments; invalid NullStrings are None.
type Proposal struct {
	Repository, BaseRevision, Path, RegionKind, LeftProject, RightProject, PeerLinkID string
	ProposerTaskID, ConstraintText, RegionKey                                         string
	RegionClass                                                                       string // "" is SOURCE
	RegenerateFrom, Condition, IssueKey, NextOwner, Supersedes                        sql.NullString
	Carry                                                                             *Carry
}

// Carry is propose's carry={"predecessor": id, "restated": text or None}; reaffirm's only.
type Carry struct {
	Predecessor string
	Restated    sql.NullString
}

// regionPlace is the region a proposal decides against: persisted or about to be.
type regionPlace struct {
	id, repository, baseRevision, path, kind, key, class string
	regenerateFrom                                       sql.NullString
}

func (p regionPlace) place() place {
	return place{regionID: p.id, repository: p.repository, path: p.path, kind: p.kind, key: p.key}
}

// Propose claims a place, not a file.
func (e *EditRegions) Propose(ctx context.Context, in Proposal) (contract.OrderedObject, error) {
	if in.RegionClass == "" {
		in.RegionClass = classSource
	}
	clean, ok := canonicalPath(in.Path)
	if problem := shapeRefusal(clean, ok, in); problem != nil {
		return nil, problem.err()
	}
	identifier, err := RegionID(in.Repository, in.BaseRevision, clean, in.RegionKind, in.RegionKey)
	if err != nil {
		return nil, err
	}
	if err := exact(in.LeftProject, "a project key"); err != nil {
		return nil, err
	}
	if err := exact(in.RightProject, "a project key"); err != nil {
		return nil, err
	}
	low, high := sortedPair(in.LeftProject, in.RightProject)
	if low == high {
		return nil, refuse(contract.RefusalScopeCycle, "a project is not its own peer")
	}
	if slices.Contains(keyedKinds, in.RegionKind) != (in.RegionKey != "") {
		what := "covers the whole path and takes no key"
		if slices.Contains(keyedKinds, in.RegionKind) {
			what = "names the symbol or data key it covers"
		}
		return nil, refuse(contract.RefusalRegionTooBroad, "a "+in.RegionKind+" region "+what)
	}
	if in.Carry == nil {
		owned, err := e.ownedSideOf(ctx, low, high, in.ProposerTaskID)
		if err != nil {
			return nil, err
		}
		if owned == "" {
			return nil, refuse(contract.RefusalScopeRoleMismatch, "task "+repr(in.ProposerTaskID)+" is the registered parent of neither "+
				repr(low)+" nor "+repr(high)+", and a proposal pre-accepts its own side, so a stranger could forge one and block an overlapping region")
		}
	}
	now := e.Now()
	var decided *refusal
	var replay row
	var agreement string
	err = e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		region := regionPlace{id: identifier, repository: in.Repository, baseRevision: in.BaseRevision, path: clean,
			kind: in.RegionKind, key: in.RegionKey, class: in.RegionClass, regenerateFrom: in.RegenerateFrom}
		stored, err := e.one(ctx, "SELECT * FROM edit_regions WHERE region_id = ?", identifier)
		if err != nil {
			return err
		}
		if stored != nil {
			region = persisted(stored)
		}
		var predecessor row
		if in.Carry != nil {
			destination := map[string]string{"repository": in.Repository, "path": clean, "region_kind": in.RegionKind,
				"region_key": in.RegionKey, "region_class": in.RegionClass, "regenerate_from": in.RegenerateFrom.String,
				"left_project": low, "right_project": high, "peer_link_id": in.PeerLinkID}
			if predecessor, decided, err = e.carryCheck(ctx, in.Carry, destination, in.Supersedes, in.BaseRevision, in.ProposerTaskID); err != nil {
				return err
			}
		}
		if decided == nil && (region.class != in.RegionClass || region.regenerateFrom.String != in.RegenerateFrom.String) {
			decided = &refusal{contract.RefusalRegionOverlap, "region " + repr(identifier) + " is already recorded as " +
				repr(region.class) + " derived from " + reprNullable(region.regenerateFrom) + "; a place is classified once, and a" +
				" different classification is a different claim about it", domainEditRegion, in.Repository, identifier, in.RegionClass}
		}
		var previous row
		if decided == nil {
			if previous, err = e.one(ctx, "SELECT * FROM edit_agreements"+
				"  WHERE region_id = ? AND left_project = ? AND right_project = ?"+
				"    AND state IN ('proposed','agreed','reopened') AND superseded_by IS NULL", identifier, low, high); err != nil {
				return err
			}
		}
		switch {
		case decided != nil:
		case previous != nil && predecessor != nil:
			decided = standingRefusal(text(previous, "agreement_id"), text(predecessor, "agreement_id"), in.BaseRevision, in.Repository, in.ProposerTaskID)
		case previous != nil:
			replay = previous
		default:
			if decided, err = e.overlapRefusal(ctx, region, low, high, in.ProposerTaskID); err != nil {
				return err
			}
			if decided == nil {
				if decided, err = e.peerRefusal(ctx, low, high, in.PeerLinkID, in.ProposerTaskID, identifier); err != nil {
					return err
				}
			}
		}
		var owned string
		if replay == nil && decided == nil {
			if owned, err = e.ownedSideOf(ctx, low, high, in.ProposerTaskID); err != nil {
				return err
			}
			if owned == "" {
				decided = &refusal{contract.RefusalScopeRoleMismatch, "task " + repr(in.ProposerTaskID) + " no longer owns either " +
					repr(low) + " or " + repr(high) + "; the project changed hands while this proposal was being decided",
					domainEditRegion, in.Repository, "", in.ProposerTaskID}
			}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		if replay != nil {
			return nil
		}
		if err := e.Store.RecordEditRegion(ctx, store.EditRegionsRow{RegionID: identifier, Repository: in.Repository,
			BaseRevision: in.BaseRevision, Path: clean, RegionKind: in.RegionKind, RegionKey: in.RegionKey,
			RegionClass: in.RegionClass, RegenerateFrom: in.RegenerateFrom, RecordedAt: now}); err != nil {
			return err
		}
		stored, err = e.one(ctx, "SELECT * FROM edit_regions WHERE region_id = ?", identifier)
		if err != nil {
			return err
		}
		region = persisted(stored)
		top, err := e.Store.HighestAgreementTenure(ctx, identifier, low, high)
		if err != nil {
			return err
		}
		tenure := top + 1
		agreement = AgreementID(identifier, low, high, tenure)
		terms := carriedTerms{proposer: in.ProposerTaskID, constraint: in.ConstraintText, issue: in.IssueKey,
			nextOwner: in.NextOwner, conditions: map[string]sql.NullString{}}
		terms.conditions[owned] = in.Condition
		if predecessor != nil {
			if terms, err = e.carriedTerms(ctx, predecessor, owned, low, high, in.Carry.Restated, region.baseRevision); err != nil {
				return err
			}
		}
		accepted := func(side string) sql.NullString { return sql.NullString{String: now, Valid: owned == side} }
		if err := e.Store.InsertEditAgreement(ctx, store.EditAgreementsRow{AgreementID: agreement, RegionID: identifier,
			Repository: region.repository, BaseRevision: region.baseRevision, LeftProject: low, RightProject: high,
			PeerLinkID: in.PeerLinkID, ProposerTaskID: terms.proposer, IssueKey: terms.issue, ConstraintText: terms.constraint,
			LeftCondition: terms.conditions[low], RightCondition: terms.conditions[high], LeftAcceptedAt: accepted(low),
			RightAcceptedAt: accepted(high), NextOwner: terms.nextOwner, State: stateProposed, Tenure: tenure,
			Supersedes: in.Supersedes, ProposedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := journal(ctx, e.Store, "edit_region_proposed", agreement, contract.OrderedObject{
			{Key: "regionId", Value: identifier}, {Key: "path", Value: clean}, {Key: "pair", Value: []any{low, high}},
		}, now); err != nil {
			return err
		}
		if predecessor != nil {
			return e.retireCarried(ctx, predecessor, agreement, in.ProposerTaskID, owned, terms, region.baseRevision, now)
		}
		return nil
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	if replay != nil {
		answer, err := e.described(ctx, agreementRecord(replay), nil, nil, nil)
		if err != nil {
			return nil, err
		}
		return append(answer, contract.Field{Key: "alreadyProposed", Value: true}), nil
	}
	answer, err := e.Agreement(ctx, agreement)
	if err != nil {
		return nil, err
	}
	return append(answer, contract.Field{Key: "alreadyProposed", Value: false}), nil
}

func persisted(r row) regionPlace {
	from := sql.NullString{}
	if v, ok := r.Get("regenerate_from").(string); ok {
		from = sql.NullString{String: v, Valid: true}
	}
	return regionPlace{id: text(r, "region_id"), repository: text(r, "repository"), baseRevision: text(r, "base_revision"),
		path: text(r, "path"), kind: text(r, "region_kind"), key: text(r, "region_key"), class: text(r, "region_class"),
		regenerateFrom: from}
}

// shapeRefusal is EditRegions._shape_refusal.
func shapeRefusal(clean string, canonical bool, in Proposal) *refusal {
	broad := func(detail string) *refusal {
		return &refusal{contract.RefusalRegionTooBroad, detail, domainEditRegion, in.Repository, in.Path, ""}
	}
	switch {
	case !slices.Contains(regionKinds, in.RegionKind):
		return broad("a region kind is one of " + strings.Join(regionKinds, ", ") + ", not " + repr(in.RegionKind))
	case !slices.Contains(regionClasses, in.RegionClass):
		return broad("a region class is one of " + strings.Join(regionClasses, ", ") + ", not " + repr(in.RegionClass))
	case !canonical:
		return broad("a region path is repository-relative and canonical: " + repr(in.Path) +
			" has a leading slash, a '..' component, a redundant separator or a trailing one. Two spellings of one place would derive two regions while" +
			" containment treated them as the same place")
	case in.RegionKind == kindTree && (clean == "" || clean == "."):
		return broad("a tree region at the repository root claims everything, which is the 'one name blocks the project' failure at its largest")
	case in.RegionClass == classGenerated && strings.TrimSpace(in.RegenerateFrom.String) == "":
		return broad("a generated region names what it is re-derived from; without that a reader" +
			" cannot tell an artefact to regenerate from one to hand-merge")
	}
	return nil
}

// overlapRefusal is EditRegions._overlap_refusal: another pair's live overlapping agreement.
func (e *EditRegions) overlapRefusal(ctx context.Context, region regionPlace, low, high, actor string) (*refusal, error) {
	if region.class == classGenerated {
		return nil, nil
	}
	rows, err := e.all(ctx, "SELECT a.*, r.path AS path, r.region_kind AS region_kind,"+
		"       r.region_key AS region_key, r.region_class AS region_class,"+
		"       r.repository AS repository, r.region_id AS region_id"+
		"  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"+
		" WHERE a.repository = ? AND a.base_revision = ?"+
		"   AND a.state IN ('proposed','agreed','reopened') AND a.superseded_by IS NULL", region.repository, region.baseRevision)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if text(r, "region_class") == classGenerated {
			continue
		}
		if text(r, "left_project") == low && text(r, "right_project") == high {
			continue
		}
		other := place{regionID: text(r, "region_id"), repository: text(r, "repository"), path: text(r, "path"),
			kind: text(r, "region_kind"), key: text(r, "region_key")}
		if overlap(region.place(), other) == overlapDisjoint {
			continue
		}
		return &refusal{contract.RefusalRegionOverlap, "agreement " + repr(text(r, "agreement_id")) + " between " +
			repr(text(r, "left_project")) + " and " + repr(text(r, "right_project")) + " already covers " + text(r, "region_kind") +
			" " + repr(text(r, "path")) + ", which overlaps this region", domainEditRegion, region.repository, text(r, "agreement_id"), actor}, nil
	}
	return nil, nil
}

// peerRefusal is EditRegions._peer_refusal.
func (e *EditRegions) peerRefusal(ctx context.Context, low, high, link, actor, subject string) (*refusal, error) {
	r, err := e.one(ctx, "SELECT * FROM scope_links WHERE link_id = ?", link)
	if err != nil {
		return nil, err
	}
	if r == nil || text(r, "link_kind") != peerKind || !isLive(text(r, "status")) || text(r, "superseded_by") != "" {
		return &refusal{contract.RefusalUnregisteredScope, "link " + repr(link) + " is not a live peer link, and an agreement" +
			" joins two projects that registered as peers", domainEditRegion, subject, "", actor}, nil
	}
	a, b := sortedPair(text(r, "upper_key"), text(r, "lower_key"))
	if a != low || b != high {
		return &refusal{contract.RefusalUnregisteredScope, "peer link " + repr(link) + " joins [" + repr(a) + ", " + repr(b) +
			"], not [" + repr(low) + ", " + repr(high) + "]", domainEditRegion, subject, "", actor}, nil
	}
	return nil, nil
}

// carryCheck is EditRegions._carry_check: whether a carry may retire its predecessor here.
func (e *EditRegions) carryCheck(ctx context.Context, carry *Carry, destination map[string]string, supersedes sql.NullString, revision, actor string) (row, *refusal, error) {
	subject := destination["repository"]
	predecessor, err := e.one(ctx, placeQuery, carry.Predecessor)
	if err != nil {
		return nil, nil, err
	}
	if predecessor == nil {
		return nil, &refusal{contract.RefusalUnregisteredScope, "no agreement " + repr(carry.Predecessor) + " to carry",
			domainEditRegion, subject, carry.Predecessor, actor}, nil
	}
	identifier := text(predecessor, "agreement_id")
	for _, field := range carriedPlace {
		if destination[field] != text(predecessor, field) {
			return nil, &refusal{contract.RefusalUnregisteredScope, "a carry of " + repr(identifier) + " lands on its own place, pair and" +
				" link; " + field + " " + reprText(destination[field], field == "regenerate_from") + " is not its " +
				reprValue(predecessor.Get(field)), domainEditRegion, subject, identifier, actor}, nil
		}
	}
	if supersedes.String != identifier {
		return nil, &refusal{contract.RefusalUnregisteredScope, "a carry of " + repr(identifier) + " supersedes it, not " +
			reprNullable(supersedes), domainEditRegion, subject, identifier, actor}, nil
	}
	state := text(predecessor, "state")
	if !slices.Contains(liveStates, state) || text(predecessor, "superseded_by") != "" {
		by := ""
		if s := text(predecessor, "superseded_by"); s != "" {
			by = ", superseded by " + repr(s)
		}
		return nil, &refusal{contract.RefusalAgreementNotOpen, "agreement " + repr(identifier) + " is " + state + by +
			"; it was settled or carried while this carry was being decided, so there is nothing left to carry",
			domainEditRegion, subject, state, actor}, nil
	}
	chain, err := e.chainFrom(ctx, subject, text(predecessor, "base_revision"))
	if err != nil {
		return nil, nil, err
	}
	if len(chain) == 0 {
		return nil, unmovedRefusal(identifier, text(predecessor, "base_revision"), subject, actor), nil
	}
	if end := chain[len(chain)-1]; end != revision {
		return nil, &refusal{contract.RefusalAgreementRevisionStale, "the recorded chain from " + repr(text(predecessor, "base_revision")) +
			" now ends at " + repr(end) + ", not " + repr(revision) + ": the base moved again while this was being carried. Reaffirm it onto " +
			repr(end), domainEditRegion, subject, end, revision}, nil
	}
	return predecessor, nil, nil
}

// reprText is repr() of a destination field; regenerate_from may be None, spelled "" here.
func reprText(v string, nullable bool) string {
	if nullable && v == "" {
		return "None"
	}
	return repr(v)
}

func reprValue(v any) string {
	if s, ok := v.(string); ok {
		return repr(s)
	}
	return "None"
}

func unmovedRefusal(identifier, revision, subject, actor string) *refusal {
	return &refusal{contract.RefusalLinkNotActive, "agreement " + repr(identifier) + " already stands on " + repr(revision) +
		", which no recorded move has superseded. A revision is not its own successor," +
		" and proposing it again in place would only clear the other side's acceptance", domainEditRegion, subject, revision, actor}
}

func standingRefusal(standing, identifier, revision, subject, actor string) *refusal {
	return &refusal{contract.RefusalRegionOverlap, "agreement " + repr(standing) + " between these two projects already stands on this" +
		" place at " + repr(revision) + " with terms of its own. Carrying " + repr(identifier) +
		" onto it would hand it a lineage and terms it never had; agree on " + repr(standing) + " instead",
		domainEditRegion, subject, standing, actor}
}

type carriedTerms struct {
	proposer, constraint  string
	issue, nextOwner      sql.NullString
	conditions, revisions map[string]sql.NullString
	constraintRevision    sql.NullString
}

func nullText(r row, name string) sql.NullString {
	switch v := r.Get(name).(type) {
	case string:
		return sql.NullString{String: v, Valid: true}
	case []byte:
		return sql.NullString{String: string(v), Valid: true}
	}
	return sql.NullString{}
}

func valid(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

// carriedTerms is EditRegions._carried_terms: what a successor inherits, with revisions.
func (e *EditRegions) carriedTerms(ctx context.Context, predecessor row, owned, low, high string, restated sql.NullString, revision string) (carriedTerms, error) {
	earlier, err := e.one(ctx, "SELECT * FROM edit_reaffirmations WHERE agreement_id = ?", text(predecessor, "agreement_id"))
	if err != nil {
		return carriedTerms{}, err
	}
	before := text(predecessor, "base_revision")
	conditions := map[string]sql.NullString{low: nullText(predecessor, "left_condition"), high: nullText(predecessor, "right_condition")}
	revisions := map[string]sql.NullString{}
	proposer := text(predecessor, "proposer_task_id")
	var constraintRevision sql.NullString
	switch {
	case earlier != nil:
		revisions[low], revisions[high] = nullText(earlier, "left_condition_revision"), nullText(earlier, "right_condition_revision")
		constraintRevision = nullText(earlier, "constraint_revision")
	case text(predecessor, "supersedes") != "":
		lineageRows, err := e.all(ctx, lineageQuery, text(predecessor, "repository"))
		if err != nil {
			return carriedTerms{}, err
		}
		carryRows, err := e.all(ctx, carriesQuery, text(predecessor, "repository"))
		if err != nil {
			return carriedTerms{}, err
		}
		lineage, carries := byID(lineageRows), byID(carryRows)
		origin := originOf(text(predecessor, "agreement_id"), lineage, carries)
		source, sourced := carries[text(origin, "agreement_id")]
		for _, side := range [][2]string{{low, "left_condition"}, {high, "right_condition"}} {
			if conditions[side[0]].Valid {
				revisions[side[0]] = valid(before)
				continue
			}
			conditions[side[0]] = nullText(origin, side[1])
			switch {
			case sourced:
				revisions[side[0]] = nullText(source, side[1]+"_revision")
			case conditions[side[0]].Valid:
				revisions[side[0]] = nullText(origin, "base_revision")
			}
		}
		proposer = text(origin, "proposer_task_id")
		if v, ok := writtenOn(text(predecessor, "agreement_id"), lineage, carries).(string); ok {
			constraintRevision = valid(v)
		}
	default:
		for _, side := range []string{low, high} {
			if conditions[side].Valid {
				revisions[side] = valid(before)
			}
		}
		constraintRevision = valid(before)
	}
	if restated.Valid {
		conditions[owned], revisions[owned] = restated, valid(revision)
	}
	other := low
	if owned == low {
		other = high
	}
	next, err := e.soleParent(ctx, other)
	if err != nil {
		return carriedTerms{}, err
	}
	return carriedTerms{proposer: proposer, constraint: text(predecessor, "constraint_text"), issue: nullText(predecessor, "issue_key"),
		nextOwner: sql.NullString{String: next, Valid: next != ""}, conditions: conditions, revisions: revisions,
		constraintRevision: constraintRevision}, nil
}

// retireCarried is EditRegions._retire_carried.
func (e *EditRegions) retireCarried(ctx context.Context, predecessor row, successor, actor, owned string, terms carriedTerms, revision, now string) error {
	identifier := text(predecessor, "agreement_id")
	if err := e.Store.SupersedeEditAgreement(ctx, identifier, stateReleased, "reaffirmed onto "+revision, successor, now); err != nil {
		return err
	}
	low, high := text(predecessor, "left_project"), text(predecessor, "right_project")
	if err := e.Store.RecordEditReaffirmation(ctx, store.EditReaffirmationsRow{AgreementID: successor, PredecessorID: identifier,
		Actor: actor, ActorProject: owned, FromRevision: text(predecessor, "base_revision"), ToRevision: revision,
		ConstraintRevision: terms.constraintRevision.String, LeftConditionRevision: terms.revisions[low],
		RightConditionRevision: terms.revisions[high], RecordedAt: now}); err != nil {
		return err
	}
	return journal(ctx, e.Store, "edit_region_reaffirmed", successor, contract.OrderedObject{
		{Key: "predecessor", Value: identifier}, {Key: "actor", Value: actor},
		{Key: "from", Value: text(predecessor, "base_revision")}, {Key: "to", Value: revision},
	}, now)
}

// Settle is EditRegions.settle: accept, decline, withdraw or release.
func (e *EditRegions) Settle(ctx context.Context, identifier, actor, disposition string, condition, reason sql.NullString) (contract.OrderedObject, error) {
	if !slices.Contains(dispositions, disposition) {
		return nil, refuse(contract.RefusalLinkNotActive, "a disposition is one of "+strings.Join(dispositions, ", ")+", not "+repr(disposition))
	}
	if disposition == "accepted" && condition.Valid {
		return nil, refuse(contract.RefusalLinkNotActive, "an acceptance takes no condition, and one given here would be dropped. A side"+
			" states its condition when it proposes, restates it with region-reaffirm"+
			" --condition after a base move, or declines with the condition it would accept")
	}
	now := e.Now()
	var decided *refusal
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, err := e.one(ctx, "SELECT * FROM edit_agreements WHERE agreement_id = ?", identifier)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse(contract.RefusalUnregisteredScope, "no agreement "+repr(identifier))
		}
		repository, state := text(r, "repository"), text(r, "state")
		side, acting, err := e.actingSide(ctx, r, actor, repository)
		if err != nil {
			return err
		}
		decided = acting
		if decided == nil && !slices.Contains(liveStates, state) {
			decided = &refusal{contract.RefusalAgreementNotOpen, "agreement " + repr(identifier) + " is " + state +
				", which admits no further settlement; a new proposal supersedes it", domainEditRegion, repository, state, actor}
		}
		if decided == nil {
			superseded, err := e.one(ctx, "SELECT * FROM edit_revision_marks  WHERE repository = ? AND from_revision = ?", repository, text(r, "base_revision"))
			if err != nil {
				return err
			}
			if superseded != nil {
				chain, err := e.chainFrom(ctx, repository, text(r, "base_revision"))
				if err != nil {
					return err
				}
				end := text(superseded, "to_revision")
				if len(chain) > 0 {
					end = chain[len(chain)-1]
				}
				decided = &refusal{contract.RefusalAgreementRevisionStale, "this agreement stands on " + repr(text(r, "base_revision")) +
					", which was restated to " + repr(text(superseded, "to_revision")) + "; the recorded chain from it ends at " + repr(end) +
					". Reaffirm it on the current revision before settling it: " + commandLine("region-reaffirm", "--agreement", identifier,
					"--actor", actor, "--revision", end), domainEditRegion, repository, text(r, "base_revision"), actor}
			}
		}
		if decided == nil && disposition == stateWithdrawn && text(r, "proposer_task_id") != actor {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "only " + repr(text(r, "proposer_task_id")) +
				" can withdraw its own proposal; the other side declines instead", domainEditRegion, repository, text(r, "proposer_task_id"), actor}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		return e.applySettlement(ctx, r, side, disposition, condition, reason, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	return e.Agreement(ctx, identifier)
}

func (e *EditRegions) applySettlement(ctx context.Context, r row, side, disposition string, condition, reason sql.NullString, now string) error {
	identifier := text(r, "agreement_id")
	left := side == "left_project"
	switch disposition {
	case "accepted":
		column := "right"
		if left {
			column = "left"
		}
		if err := e.Store.AcceptEditAgreementSide(ctx, identifier, column, now); err != nil {
			return err
		}
		both, err := e.one(ctx, "SELECT left_accepted_at, right_accepted_at FROM edit_agreements  WHERE agreement_id = ?", identifier)
		if err != nil {
			return err
		}
		if text(both, "left_accepted_at") != "" && text(both, "right_accepted_at") != "" {
			if err := e.Store.SetEditAgreementState(ctx, identifier, stateAgreed, now); err != nil {
				return err
			}
		}
		return journal(ctx, e.Store, "edit_region_accepted", identifier, contract.OrderedObject{{Key: "side", Value: side}}, now)
	case "declined":
		column := "right_condition"
		if left {
			column = "left_condition"
		}
		q := e.Store.Querier(ctx)
		if _, err := q.ExecContext(ctx, "UPDATE edit_agreements SET "+column+" = ?, state = ?, close_reason = ?,"+
			" closed_at = ?, updated_at = ? WHERE agreement_id = ?", condition, stateDeclined, reason, now, now, identifier); err != nil {
			return err
		}
		stated := sql.NullString{}
		if condition.Valid {
			stated = valid(text(r, "base_revision"))
		}
		if _, err := q.ExecContext(ctx, "UPDATE edit_reaffirmations SET "+column+"_revision = ?"+
			" WHERE agreement_id = ?", stated, identifier); err != nil {
			return err
		}
		return journal(ctx, e.Store, "edit_region_declined", identifier, contract.OrderedObject{
			{Key: "side", Value: side}, {Key: "condition", Value: nullable(condition)}}, now)
	}
	closed := stateReleased
	if disposition == stateWithdrawn {
		closed = stateWithdrawn
	}
	if _, err := e.Store.Querier(ctx).ExecContext(ctx, "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"+
		" updated_at = ? WHERE agreement_id = ?", closed, reason, now, now, identifier); err != nil {
		return err
	}
	return journal(ctx, e.Store, "edit_region_closed", identifier, contract.OrderedObject{{Key: "state", Value: closed}}, now)
}
