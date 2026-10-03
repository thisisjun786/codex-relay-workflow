package capacity

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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
		parents, err := owners(ctx, e.Store, scopeProject, r.Text("project"), roleParent)
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
	return refuse(contract.RefusalScopeRoleMismatch, "task "+strconv.Quote(actor)+" is the registered parent of no project holding an"+
		" agreement in "+strconv.Quote(repository)+", so it cannot restate that repository's revision and reopen everybody's agreements")
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
		bases = append(bases, r.Text("base_revision"))
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
		quoted[i] = pyvalue.StrRepr(end)
	}
	return &refusal{contract.RefusalAgreementRevisionStale, "no live agreement in " + pyvalue.StrRepr(repository) + " stands on " + pyvalue.StrRepr(revision) +
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
		if existing != nil && existing.Text("to_revision") != to {
			chain, err := e.chainFrom(ctx, repository, from)
			if err != nil {
				return err
			}
			if !slices.Contains(chain, to) {
				end := chain[len(chain)-1]
				decided = &refusal{contract.RefusalAgreementRevisionStale, pyvalue.StrRepr(from) + " was already restated to " +
					pyvalue.StrRepr(existing.Text("to_revision")) + " by " + pyvalue.StrRepr(existing.Text("actor")) +
					"; one revision has one successor and a second would leave two chains nobody can order. A later move is recorded from" +
					" the end of the recorded chain, which is " + pyvalue.StrRepr(end) + ": " + commandLine("region-restate-revision", "--repository",
					repository, "--from-revision", end, "--to-revision", to, "--actor", actor),
					domainEditRegion, repository, existing.Text("to_revision"), to}
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
					quoted[i] = pyvalue.StrRepr(r)
				}
				decided = &refusal{contract.RefusalAgreementRevisionStale, pyvalue.StrRepr(to) + " already reaches " + pyvalue.StrRepr(from) +
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
			moved = append(moved, r.Text("agreement_id"))
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
			return refuse(contract.RefusalUnregisteredScope, "no agreement "+strconv.Quote(identifier))
		}
		repository := r.Text("repository")
		_, acting, err := e.actingSide(ctx, r, actor, repository)
		if err != nil {
			return err
		}
		decided = acting
		if state := r.Text("state"); decided == nil && !slices.Contains(liveStates, state) {
			decided = &refusal{contract.RefusalAgreementNotOpen, "agreement " + pyvalue.StrRepr(identifier) + " is " + state +
				"; a closed agreement is not carried forward, it is proposed again", domainEditRegion, repository, state, actor}
		}
		if decided == nil && revision == r.Text("base_revision") {
			decided = unmovedRefusal(identifier, r.Text("base_revision"), repository, actor)
		}
		if decided == nil {
			chain, err := e.chainFrom(ctx, repository, r.Text("base_revision"))
			if err != nil {
				return err
			}
			terminal := r.Text("base_revision")
			if len(chain) > 0 {
				terminal = chain[len(chain)-1]
			}
			if revision != terminal {
				decided = &refusal{contract.RefusalAgreementRevisionStale, "this agreement stands on " + pyvalue.StrRepr(r.Text("base_revision")) +
					", whose recorded chain reaches " + pyvalue.StrRepr(terminal) + ", not " + pyvalue.StrRepr(revision) +
					". Restate the revision first, or name the one the chain reaches", domainEditRegion, repository, terminal, revision}
			}
		}
		if decided != nil {
			return recordIn(ctx, e.Store, decided, now)
		}
		successorID, err := RegionID(repository, revision, r.Text("path"), r.Text("region_kind"), r.Text("region_key"))
		if err != nil {
			return err
		}
		successor := regionPlace{id: successorID, repository: repository, baseRevision: revision, path: r.Text("path"),
			kind: r.Text("region_kind"), key: r.Text("region_key"), class: r.Text("region_class")}
		low, high := sortedPair(r.Text("left_project"), r.Text("right_project"))
		if decided, err = e.overlapRefusal(ctx, successor, low, high, actor); err != nil {
			return err
		}
		if decided == nil {
			if decided, err = e.peerRefusal(ctx, low, high, r.Text("peer_link_id"), actor, repository); err != nil {
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
				decided = standingRefusal(standing.Text("agreement_id"), identifier, revision, repository, actor)
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
	return e.Propose(ctx, Proposal{Repository: carried.Text("repository"), BaseRevision: revision, Path: carried.Text("path"),
		RegionKind: carried.Text("region_kind"), RegionKey: carried.Text("region_key"), RegionClass: carried.Text("region_class"),
		RegenerateFrom: nullText(carried, "regenerate_from"), LeftProject: carried.Text("left_project"),
		RightProject: carried.Text("right_project"), PeerLinkID: carried.Text("peer_link_id"), ProposerTaskID: actor,
		ConstraintText: carried.Text("constraint_text"), IssueKey: nullText(carried, "issue_key"), NextOwner: nullText(carried, "next_owner"),
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
		owned, err := e.realOwnedSide(ctx, pair.Text("left_project"), pair.Text("right_project"), in.RecordedBy)
		if err != nil {
			return nil, err
		}
		if owned == "" {
			return nil, refuse(contract.RefusalScopeRoleMismatch, "task "+strconv.Quote(in.RecordedBy)+" owns neither side of agreement "+
				strconv.Quote(in.Agreement)+", so it cannot append work to it")
		}
	}
	if in.AssigneeTask.String != "" && in.AssigneeTask.String != in.RecordedBy {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a follow-up records its own author as the assignee or nobody; "+
			strconv.Quote(in.RecordedBy)+" cannot accept it for "+strconv.Quote(in.AssigneeTask.String)+", who accepts it themselves")
	}
	// followup_id hashes the trigger first, and str.encode("utf-8") raises for one holding a
	// surrogate escape (an argv byte that is not UTF-8).
	if err := store.EncodeUTF8(in.Trigger); err != nil {
		return nil, err
	}
	identifier := FollowupID(in.Agreement, in.Trigger)
	now := e.Now()
	err = e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		found, err := e.one(ctx, "SELECT 1 FROM edit_agreements WHERE agreement_id = ?", in.Agreement)
		if err != nil {
			return err
		}
		if found == nil {
			return refuse(contract.RefusalUnregisteredScope, "no agreement "+strconv.Quote(in.Agreement))
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
		return nil, nil, "", nil, refuse(contract.RefusalUnregisteredScope, "no follow-up "+strconv.Quote(identifier))
	}
	agreement, err := e.one(ctx, "SELECT * FROM edit_agreements WHERE agreement_id = ?", item.Text("agreement_id"))
	if err != nil {
		return nil, nil, "", nil, err
	}
	side, decided, err := e.actingSide(ctx, agreement, actor, agreement.Text("repository"))
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
		repository, state, assignee := agreement.Text("repository"), item.Text("state"), item.Text("assignee_task_id")
		if decided == nil && project != agreement.Text(side) {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "task " + pyvalue.StrRepr(actor) + " owns " + pyvalue.StrRepr(agreement.Text(side)) +
				", so its acceptance is recorded under that project, not " + pyvalue.StrRepr(project), domainEditRegion, repository, agreement.Text(side), project}
		}
		if decided == nil && (state == followupDone || state == followupDropped) {
			decided = &refusal{contract.RefusalAgreementNotOpen, "follow-up " + pyvalue.StrRepr(identifier) + " is " + state +
				", which is terminal; a new follow-up records new work", domainEditRegion, repository, state, actor}
		}
		if decided == nil && assignee != "" && assignee != actor {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "follow-up " + pyvalue.StrRepr(identifier) + " was already accepted by " +
				pyvalue.StrRepr(assignee) + "; taking it from them is not an acceptance", domainEditRegion, repository, assignee, actor}
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
		return nil, refuse(contract.RefusalLinkNotActive, "a follow-up disposition is "+strings.Join(followupDispositions, " or ")+", not "+strconv.Quote(disposition))
	}
	now := e.Now()
	var decided *refusal
	err := e.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		item, agreement, _, acting, err := e.followupContext(ctx, identifier, actor)
		if err != nil {
			return err
		}
		decided = acting
		repository, state, assignee := agreement.Text("repository"), item.Text("state"), item.Text("assignee_task_id")
		if decided == nil && (state == followupDone || state == followupDropped) && state != disposition {
			decided = &refusal{contract.RefusalAgreementNotOpen, "follow-up " + pyvalue.StrRepr(identifier) + " was already settled as " +
				state + "; that decision stands", domainEditRegion, repository, state, disposition}
		}
		if decided == nil && disposition == followupDone && assignee == "" {
			decided = &refusal{contract.RefusalFollowupUnassigned, "follow-up " + pyvalue.StrRepr(identifier) + " was never accepted by anybody, so" +
				" nobody can report it done. Accept it first, or drop it", domainEditRegion, repository, "", actor}
		}
		if decided == nil && disposition == followupDone && assignee != actor {
			decided = &refusal{contract.RefusalScopeRoleMismatch, "follow-up " + pyvalue.StrRepr(identifier) + " was accepted by " +
				pyvalue.StrRepr(assignee) + ", so only they can report it done", domainEditRegion, repository, assignee, actor}
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
