package capacity

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// editregion.py: revision marks, reaffirmation and follow-ups.

// checkRestater is EditRegions._check_restater.
func (e *EditRegions) checkRestater(ctx context.Context, repository, actor string) error {
	rows, err := e.all(ctx, "SELECT DISTINCT left_project AS project FROM edit_agreements WHERE repository = ?"+
		"  UNION SELECT DISTINCT right_project FROM edit_agreements WHERE repository = ?", repository, repository)
	if err != nil {
		return err
	}
	for _, r := range rows {
		parents, err := owners(ctx, e.Store, scopeProject, text(r, "project"), roleParent)
		if err != nil {
			return err
		}
		if len(parents) == 1 && parents[0] == actor {
			return nil
		}
	}
	if len(rows) == 0 {
		registered, err := e.one(ctx, "SELECT task_id FROM scope_bindings"+
			"  WHERE task_id = ? AND role = 'parent'"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL", actor)
		if err != nil || registered != nil {
			return err
		}
	}
	return refuse(contract.RefusalScopeRoleMismatch, "task "+repr(actor)+" is the registered parent of no project holding an"+
		" agreement in "+repr(repository)+", so it cannot restate that repository's revision and reopen everybody's agreements")
}

// unrelatedStart is EditRegions._unrelated_start.
func (e *EditRegions) unrelatedStart(ctx context.Context, repository, revision string) (*refusal, error) {
	baseRows, err := e.all(ctx, "SELECT DISTINCT base_revision FROM edit_agreements"+
		"  WHERE repository = ? AND state IN ('proposed','agreed','reopened')"+
		"    AND superseded_by IS NULL", repository)
	if err != nil {
		return nil, err
	}
	marks, err := e.all(ctx, marksQuery, repository)
	if err != nil {
		return nil, err
	}
	successors := successorMap(marks)
	var bases []string
	for _, r := range baseRows {
		bases = append(bases, text(r, "base_revision"))
	}
	if len(bases) == 0 && len(successors) == 0 {
		return nil, nil
	}
	if slices.Contains(bases, revision) {
		return nil, nil
	}
	for _, to := range successors {
		if to == revision {
			return nil, nil
		}
	}
	endSet := map[string]bool{}
	for _, base := range bases {
		chain := walk(successors, base)
		if len(chain) == 0 {
			endSet[base] = true
		} else {
			endSet[chain[len(chain)-1]] = true
		}
	}
	for _, to := range successors {
		if _, marked := successors[to]; !marked {
			endSet[to] = true
		}
	}
	var ends []string
	for end := range endSet {
		ends = append(ends, end)
	}
	slices.Sort(ends)
	quoted := make([]string, len(ends))
	for i, end := range ends {
		quoted[i] = repr(end)
	}
	return &refusal{contract.RefusalAgreementRevisionStale, "no live agreement in " + repr(repository) + " stands on " + repr(revision) +
		" and no recorded move reaches it, so a move from it would start a chain no agreement follows. A first move starts at a live" +
		" agreement's revision and a later one at the end of its recorded chain; the chains here end at [" + strings.Join(quoted, ", ") + "]",
		domainEditRegion, repository, strings.Join(ends, ","), revision}, nil
}

// RestateRevision is EditRegions.restate_revision: record that agreements stand on a newer tree.
func (e *EditRegions) RestateRevision(ctx context.Context, repository, from, to, actor string) (contract.OrderedObject, error) {
	for _, f := range [][2]string{{repository, "a repository"}, {from, "a base revision"}, {to, "a base revision"}} {
		if err := exact(f[0], f[1]); err != nil {
			return nil, err
		}
	}
	if from == to {
		return nil, refuse(contract.RefusalLinkNotActive, "a revision is not its own successor")
	}
	if err := e.checkRestater(ctx, repository, actor); err != nil {
		return nil, err
	}
	now := e.Now()
	var decided *refusal
	moved, reached := []any{}, []string{}
	var existing row
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		if existing, err = e.one(ctx, "SELECT * FROM edit_revision_marks  WHERE repository = ? AND from_revision = ?", repository, from); err != nil {
			return err
		}
		if existing != nil && text(existing, "to_revision") != to {
			chain, err := e.chainFrom(ctx, repository, from)
			if err != nil {
				return err
			}
			if !slices.Contains(chain, to) {
				end := chain[len(chain)-1]
				decided = &refusal{contract.RefusalAgreementRevisionStale, repr(from) + " was already restated to " +
					repr(text(existing, "to_revision")) + " by " + repr(text(existing, "actor")) +
					"; one revision has one successor and a second would leave two chains nobody can order. A later move is recorded from" +
					" the end of the recorded chain, which is " + repr(end) + ": " + commandLine("region-restate-revision", "--repository",
					repository, "--from-revision", end, "--to-revision", to, "--actor", actor),
					domainEditRegion, repository, text(existing, "to_revision"), to}
				return recordIn(ctx, e.Store, decided, now)
			}
		} else if existing == nil {
			if decided, err = e.unrelatedStart(ctx, repository, from); err != nil {
				return err
			}
			if reached, err = e.chainFrom(ctx, repository, to); err != nil {
				return err
			}
			if decided == nil && slices.Contains(reached, from) {
				quoted := make([]string, len(reached))
				for i, r := range reached {
					quoted[i] = repr(r)
				}
				decided = &refusal{contract.RefusalAgreementRevisionStale, repr(to) + " already reaches " + repr(from) +
					" through [" + strings.Join(quoted, ", ") + "], so this mark would close a cycle" +
					" and leave no current revision for anything to stand on", domainEditRegion, repository, to, from}
			}
			if decided != nil {
				return recordIn(ctx, e.Store, decided, now)
			}
			if err := e.Store.RecordEditRevisionMark(ctx, store.EditRevisionMarksRow{MarkID: MarkID(repository, from, to),
				Repository: repository, FromRevision: from, ToRevision: to, Actor: actor, RecordedAt: now}); err != nil {
				return err
			}
		}
		rows, err := e.all(ctx, "SELECT agreement_id FROM edit_agreements"+
			"  WHERE repository = ? AND base_revision = ?"+
			"    AND state IN ('proposed','agreed') AND superseded_by IS NULL", repository, from)
		if err != nil {
			return err
		}
		for _, r := range rows {
			moved = append(moved, text(r, "agreement_id"))
		}
		if err := e.Store.ReopenEditAgreements(ctx, repository, from, stateReopened, now); err != nil {
			return err
		}
		if reached, err = e.chainFrom(ctx, repository, from); err != nil {
			return err
		}
		return journal(ctx, e.Store, "edit_revision_restated", repository, contract.OrderedObject{
			{Key: "from", Value: from}, {Key: "to", Value: to}, {Key: "reopened", Value: moved},
			{Key: "alreadyRecorded", Value: existing != nil}}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	return contract.OrderedObject{
		{Key: "repository", Value: repository}, {Key: "fromRevision", Value: from}, {Key: "toRevision", Value: to},
		{Key: "reopened", Value: moved}, {Key: "alreadyRecorded", Value: existing != nil},
		{Key: "currentRevision", Value: reached[len(reached)-1]},
	}, nil
}

// Reaffirm is EditRegions.reaffirm: carry an agreement onto the revision its chain reaches.
func (e *EditRegions) Reaffirm(ctx context.Context, identifier, actor, revision string, condition sql.NullString) (contract.OrderedObject, error) {
	now := e.Now()
	var decided *refusal
	var carried row
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, err := e.one(ctx, placeQuery, identifier)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse(contract.RefusalUnregisteredScope, "no agreement "+repr(identifier))
		}
		repository := text(r, "repository")
		_, acting, err := e.actingSide(ctx, r, actor, repository)
		if err != nil {
			return err
		}
		decided = acting
		if state := text(r, "state"); decided == nil && !slices.Contains(liveStates, state) {
			decided = &refusal{contract.RefusalAgreementNotOpen, "agreement " + repr(identifier) + " is " + state +
				"; a closed agreement is not carried forward, it is proposed again", domainEditRegion, repository, state, actor}
		}
		if decided == nil && revision == text(r, "base_revision") {
			decided = unmovedRefusal(identifier, text(r, "base_revision"), repository, actor)
		}
		if decided == nil {
			chain, err := e.chainFrom(ctx, repository, text(r, "base_revision"))
			if err != nil {
				return err
			}
			terminal := text(r, "base_revision")
			if len(chain) > 0 {
				terminal = chain[len(chain)-1]
			}
			if revision != terminal {
				decided = &refusal{contract.RefusalAgreementRevisionStale, "this agreement stands on " + repr(text(r, "base_revision")) +
					", whose recorded chain reaches " + repr(terminal) + ", not " + repr(revision) +
					". Restate the revision first, or name the one the chain reaches", domainEditRegion, repository, terminal, revision}
			}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		successorID, err := RegionID(repository, revision, text(r, "path"), text(r, "region_kind"), text(r, "region_key"))
		if err != nil {
			return err
		}
		successor := regionPlace{id: successorID, repository: repository, baseRevision: revision, path: text(r, "path"),
			kind: text(r, "region_kind"), key: text(r, "region_key"), class: text(r, "region_class")}
		low, high := sortedPair(text(r, "left_project"), text(r, "right_project"))
		if decided, err = e.overlapRefusal(ctx, successor, low, high, actor); err != nil {
			return err
		}
		if decided == nil {
			if decided, err = e.peerRefusal(ctx, low, high, text(r, "peer_link_id"), actor, repository); err != nil {
				return err
			}
		}
		if decided == nil {
			standing, err := e.one(ctx, "SELECT agreement_id FROM edit_agreements"+
				"  WHERE region_id = ? AND left_project = ? AND right_project = ?"+
				"    AND state IN ('proposed','agreed','reopened')"+
				"    AND superseded_by IS NULL", successorID, low, high)
			if err != nil {
				return err
			}
			if standing != nil {
				decided = standingRefusal(text(standing, "agreement_id"), identifier, revision, repository, actor)
			}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		carried = r
		return nil
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	if e.beforeCarry != nil {
		e.beforeCarry()
	}
	return e.Propose(ctx, Proposal{Repository: text(carried, "repository"), BaseRevision: revision, Path: text(carried, "path"),
		RegionKind: text(carried, "region_kind"), RegionKey: text(carried, "region_key"), RegionClass: text(carried, "region_class"),
		RegenerateFrom: nullText(carried, "regenerate_from"), LeftProject: text(carried, "left_project"),
		RightProject: text(carried, "right_project"), PeerLinkID: text(carried, "peer_link_id"), ProposerTaskID: actor,
		ConstraintText: text(carried, "constraint_text"), IssueKey: nullText(carried, "issue_key"), NextOwner: nullText(carried, "next_owner"),
		Supersedes: valid(identifier), Carry: &Carry{Predecessor: identifier, Restated: condition}})
}

// Followup is EditRegions.followup's keyword arguments.
type Followup struct {
	Agreement, Trigger, Acceptance, RecordedBy string
	IssueRef, AssigneeTask, AssigneeProject    sql.NullString
}

func (e *EditRegions) followupAnswer(ctx context.Context, identifier string) (contract.OrderedObject, error) {
	r, err := e.one(ctx, "SELECT * FROM edit_followups WHERE followup_id = ?", identifier)
	if err != nil {
		return nil, err
	}
	return followupRecord(r), nil
}

// Followup records what the two sides agreed should happen next.
func (e *EditRegions) Followup(ctx context.Context, in Followup) (contract.OrderedObject, error) {
	if err := exact(in.Trigger, "a follow-up trigger"); err != nil {
		return nil, err
	}
	if err := exact(in.Acceptance, "a follow-up acceptance criterion"); err != nil {
		return nil, err
	}
	pair, err := e.one(ctx, "SELECT left_project, right_project FROM edit_agreements  WHERE agreement_id = ?", in.Agreement)
	if err != nil {
		return nil, err
	}
	if pair != nil {
		owned, err := e.realOwnedSide(ctx, text(pair, "left_project"), text(pair, "right_project"), in.RecordedBy)
		if err != nil {
			return nil, err
		}
		if owned == "" {
			return nil, refuse(contract.RefusalScopeRoleMismatch, "task "+repr(in.RecordedBy)+" owns neither side of agreement "+
				repr(in.Agreement)+", so it cannot append work to it")
		}
	}
	if in.AssigneeTask.String != "" && in.AssigneeTask.String != in.RecordedBy {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a follow-up records its own author as the assignee or nobody; "+
			repr(in.RecordedBy)+" cannot accept it for "+repr(in.AssigneeTask.String)+", who accepts it themselves")
	}
	identifier := FollowupID(in.Agreement, in.Trigger)
	now := e.Now()
	err = e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		found, err := e.one(ctx, "SELECT 1 FROM edit_agreements WHERE agreement_id = ?", in.Agreement)
		if err != nil {
			return err
		}
		if found == nil {
			return refuse(contract.RefusalUnregisteredScope, "no agreement "+repr(in.Agreement))
		}
		taken := in.AssigneeTask.String != ""
		state := followupOpen
		if taken {
			state = followupAccepted
		}
		return e.Store.RecordEditFollowup(ctx, store.EditFollowupsRow{FollowupID: identifier, AgreementID: in.Agreement,
			TriggerText: in.Trigger, AcceptanceText: in.Acceptance, IssueRef: in.IssueRef, AssigneeTaskID: in.AssigneeTask,
			AssigneeProject: in.AssigneeProject, AcceptedAt: sql.NullString{String: now, Valid: taken}, State: state,
			RecordedBy: in.RecordedBy, RecordedAt: now, UpdatedAt: now})
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	return e.followupAnswer(ctx, identifier)
}

// followupContext is the follow-up row, its agreement and the acting side, read in one body.
func (e *EditRegions) followupContext(ctx context.Context, identifier, actor string) (row, row, string, *refusal, error) {
	item, err := e.one(ctx, "SELECT * FROM edit_followups WHERE followup_id = ?", identifier)
	if err != nil {
		return nil, nil, "", nil, err
	}
	if item == nil {
		return nil, nil, "", nil, refuse(contract.RefusalUnregisteredScope, "no follow-up "+repr(identifier))
	}
	agreement, err := e.one(ctx, "SELECT * FROM edit_agreements WHERE agreement_id = ?", text(item, "agreement_id"))
	if err != nil {
		return nil, nil, "", nil, err
	}
	side, decided, err := e.actingSide(ctx, agreement, actor, text(agreement, "repository"))
	return item, agreement, side, decided, err
}

// AcceptFollowup is EditRegions.accept_followup: somebody takes it.
func (e *EditRegions) AcceptFollowup(ctx context.Context, identifier, actor, project string) (contract.OrderedObject, error) {
	now := e.Now()
	var decided *refusal
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		item, agreement, side, acting, err := e.followupContext(ctx, identifier, actor)
		if err != nil {
			return err
		}
		decided = acting
		repository, state, assignee := text(agreement, "repository"), text(item, "state"), text(item, "assignee_task_id")
		if decided == nil && project != text(agreement, side) {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "task " + repr(actor) + " owns " + repr(text(agreement, side)) +
				", so its acceptance is recorded under that project, not " + repr(project), domainEditRegion, repository, text(agreement, side), project}
		}
		if decided == nil && (state == followupDone || state == followupDropped) {
			decided = &refusal{contract.RefusalAgreementNotOpen, "follow-up " + repr(identifier) + " is " + state +
				", which is terminal; a new follow-up records new work", domainEditRegion, repository, state, actor}
		}
		if decided == nil && assignee != "" && assignee != actor {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "follow-up " + repr(identifier) + " was already accepted by " +
				repr(assignee) + "; taking it from them is not an acceptance", domainEditRegion, repository, assignee, actor}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		if err := e.Store.AcceptEditFollowup(ctx, identifier, actor, project, followupAccepted, now); err != nil {
			return err
		}
		return journal(ctx, e.Store, "edit_followup_accepted", identifier, contract.OrderedObject{{Key: "actor", Value: actor}}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	return e.followupAnswer(ctx, identifier)
}

// SettleFollowup is EditRegions.settle_followup: finish it or drop it.
func (e *EditRegions) SettleFollowup(ctx context.Context, identifier, actor, disposition string, reason sql.NullString) (contract.OrderedObject, error) {
	if !slices.Contains(followupDispositions, disposition) {
		return nil, refuse(contract.RefusalLinkNotActive, "a follow-up disposition is "+strings.Join(followupDispositions, " or ")+", not "+repr(disposition))
	}
	now := e.Now()
	var decided *refusal
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		item, agreement, _, acting, err := e.followupContext(ctx, identifier, actor)
		if err != nil {
			return err
		}
		decided = acting
		repository, state, assignee := text(agreement, "repository"), text(item, "state"), text(item, "assignee_task_id")
		if decided == nil && (state == followupDone || state == followupDropped) && state != disposition {
			decided = &refusal{contract.RefusalAgreementNotOpen, "follow-up " + repr(identifier) + " was already settled as " +
				state + "; that decision stands", domainEditRegion, repository, state, disposition}
		}
		if decided == nil && disposition == followupDone && assignee == "" {
			decided = &refusal{contract.RefusalFollowupUnassigned, "follow-up " + repr(identifier) + " was never accepted by anybody, so" +
				" nobody can report it done. Accept it first, or drop it", domainEditRegion, repository, "", actor}
		}
		if decided == nil && disposition == followupDone && assignee != actor {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "follow-up " + repr(identifier) + " was accepted by " +
				repr(assignee) + ", so only they can report it done", domainEditRegion, repository, assignee, actor}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		if err := e.Store.SettleEditFollowup(ctx, identifier, disposition, reason, now); err != nil {
			return err
		}
		return journal(ctx, e.Store, "edit_followup_settled", identifier, contract.OrderedObject{
			{Key: "disposition", Value: disposition}, {Key: "actor", Value: actor}}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	return e.followupAnswer(ctx, identifier)
}
