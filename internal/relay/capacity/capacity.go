// Package capacity ports capacity.py: execution slots counted from held rows, declared
// ceilings, and what each of them can honestly answer. A run count is derived from the slots
// this store holds; any other dimension is observed or it is unmeasured, never inferred.
package capacity

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	scopeStore = "store"

	held     = "held"
	released = "released"

	runs = "runs"

	derivedFromSlots = "derived_from_slots"
	observed         = "observed"
	unmeasured       = "unmeasured"

	within      = "within"
	overCeiling = "over_ceiling"

	// notAMeasurement is the headroom note every non-runs dimension carries.
	notAMeasurement = "a slot count is not a measurement of this dimension"
)

// scopes is capacity.SCOPES, in its order: the words a refusal lists.
var scopes = []string{scopeInitiative, scopeProject, scopeStore}

// SlotID is capacity.slot_id: one subject holds one slot, and a later tenure is a different one.
func SlotID(subjectKind, subjectKey string, tenure int64) (string, error) {
	if err := exact(subjectKind, "a subject kind"); err != nil {
		return "", err
	}
	if err := exact(subjectKey, "a subject key"); err != nil {
		return "", err
	}
	return derive("slt", subjectKind, subjectKey, strconv.FormatInt(tenure, 10)), nil
}

// LimitID is capacity.limit_id: one ceiling per dimension per scope.
func LimitID(scopeKind, scopeKey, dimension string) (string, error) {
	for _, f := range [][2]string{{scopeKind, "a scope kind"}, {scopeKey, "a scope key"}, {dimension, "a dimension"}} {
		if err := exact(f[0], f[1]); err != nil {
			return "", err
		}
	}
	return derive("lim", scopeKind, scopeKey, dimension), nil
}

// Capacity is capacity.Capacity over one store and one clock.
type Capacity struct {
	Store *store.Store
	// Now is clock.iso().
	Now func() string
}

// Slot is Capacity.slot: the newest tenure of a subject, or nil.
func (c *Capacity) Slot(ctx context.Context, subjectKind, subjectKey string) (contract.OrderedObject, error) {
	row, err := c.Store.ExecutionSlot(ctx, subjectKind, subjectKey)
	if noRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return slotRecord(row), nil
}

// ReportFilter is Capacity.report's keyword filters; an invalid NullString is Python's None.
type ReportFilter struct{ Project, ParentTask, Initiative sql.NullString }

// Report is Capacity.report: held slots, counted per parent and in total, plus any that
// outlived their scope.
func (c *Capacity) Report(ctx context.Context, filter ReportFilter) (contract.OrderedObject, error) {
	rows, err := c.Store.ExecutionSlotsInState(ctx, held)
	if err != nil {
		return nil, err
	}
	keep := func(want sql.NullString, got sql.NullString) bool {
		return !want.Valid || (got.Valid && got.String == want.String)
	}
	list := []any{}
	var kept []store.ExecutionSlotsRow
	perParent := contract.OrderedObject{}
	counts := map[string]int{}
	for _, row := range rows {
		if !keep(filter.Project, sql.NullString{String: row.ProjectKey, Valid: true}) ||
			!keep(filter.ParentTask, sql.NullString{String: row.ParentTaskID, Valid: true}) ||
			!keep(filter.Initiative, row.InitiativeKey) {
			continue
		}
		kept = append(kept, row)
		list = append(list, slotRecord(row))
		if _, seen := counts[row.ParentTaskID]; !seen {
			perParent = append(perParent, contract.Field{Key: row.ParentTaskID})
		}
		counts[row.ParentTaskID]++
	}
	for i := range perParent {
		perParent[i].Value = counts[perParent[i].Key]
	}
	stale := []any{}
	for i, row := range kept {
		parents, err := owners(ctx, c.Store, scopeProject, row.ProjectKey, roleParent)
		if err != nil {
			return nil, err
		}
		if len(parents) != 1 || parents[0] != row.ParentTaskID {
			stale = append(stale, list[i])
		}
	}
	return contract.OrderedObject{
		{Key: "held", Value: list}, {Key: "total", Value: len(list)}, {Key: "perParent", Value: perParent},
		{Key: "staleSlots", Value: stale},
	}, nil
}

// Headroom is Capacity.headroom: per dimension, what is used, against what was declared, and
// how that use is known.
func (c *Capacity) Headroom(ctx context.Context, scopeKind, scopeKey string) (contract.OrderedObject, error) {
	rows, err := c.Store.ExecutionLimits(ctx, scopeKind, scopeKey)
	if err != nil {
		return nil, err
	}
	dimensions := []any{}
	for _, row := range rows {
		entry, err := c.dimension(ctx, row)
		if err != nil {
			return nil, err
		}
		dimensions = append(dimensions, entry)
	}
	return contract.OrderedObject{{Key: "scopeKind", Value: scopeKind}, {Key: "scopeKey", Value: scopeKey}, {Key: "dimensions", Value: dimensions}}, nil
}

func (c *Capacity) dimension(ctx context.Context, row store.ExecutionLimitsRow) (contract.OrderedObject, error) {
	var used any
	var usedNumber float64
	measured := true
	proof := derivedFromSlots
	if row.Dimension == runs {
		count, err := c.Store.RunsIn(ctx, row.ScopeKind, row.ScopeKey, held)
		if err != nil {
			return nil, err
		}
		used, usedNumber = count, float64(count)
	} else {
		seen, err := c.Store.ExecutionUsage(ctx, row.ScopeKind, row.ScopeKey, row.Dimension)
		switch {
		case noRows(err):
			measured, proof = false, unmeasured
		case err != nil:
			return nil, err
		default:
			used, usedNumber, proof = seen.Observed, seen.Observed, observed
		}
	}
	state := within
	var overBy any = 0
	switch {
	case !measured:
		state = unmeasured
	case usedNumber > row.Ceiling:
		state = overCeiling
		// used - ceiling: an int minus a float is a float in Python.
		overBy = usedNumber - row.Ceiling
	}
	var note any = notAMeasurement
	if row.Dimension == runs {
		note = nil
	}
	return contract.OrderedObject{
		{Key: "dimension", Value: row.Dimension}, {Key: "unit", Value: row.Unit}, {Key: "ceiling", Value: row.Ceiling},
		{Key: "used", Value: used}, {Key: "proof", Value: proof}, {Key: "state", Value: state},
		{Key: "enforce", Value: row.Enforce == 1}, {Key: "revision", Value: row.Revision},
		{Key: "declaredBy", Value: row.DeclaredBy}, {Key: "source", Value: row.Source},
		{Key: "overBy", Value: overBy}, {Key: "note", Value: note},
	}, nil
}

func slotRecord(row store.ExecutionSlotsRow) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "slotId", Value: row.SlotID}, {Key: "subjectKind", Value: row.SubjectKind},
		{Key: "subjectKey", Value: row.SubjectKey}, {Key: "parentTaskId", Value: row.ParentTaskID},
		{Key: "projectKey", Value: row.ProjectKey}, {Key: "initiativeKey", Value: nullable(row.InitiativeKey)},
		{Key: "tenure", Value: row.Tenure}, {Key: "state", Value: row.State},
		{Key: "reservedBy", Value: row.ReservedBy}, {Key: "reservedAt", Value: row.ReservedAt},
		{Key: "releasedAt", Value: nullable(row.ReleasedAt)}, {Key: "releasedBy", Value: nullable(row.ReleasedBy)},
		{Key: "releaseReason", Value: nullable(row.ReleaseReason)}, {Key: "detail", Value: nullable(row.Detail)},
	}
}

// finite is Capacity._finite: a bound and a measurement are real, finite and not negative.
func finite(value float64, what string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return refuse(contract.RefusalLinkNotActive, what+" is a finite number of zero or more, not "+pyFloat(value))
	}
	return nil
}

// checkScope is Capacity._check_scope: the store scope has exactly one key.
func checkScope(scopeKind, scopeKey string) error {
	if scopeKind == scopeStore && scopeKey != scopeStore {
		return refuse(contract.RefusalLinkNotActive, "the store scope has one key, "+repr(scopeStore)+", not "+repr(scopeKey)+
			"; enforcement reads that key and a ceiling under any other would be recorded and never applied")
	}
	return nil
}

func checkScopeKind(scopeKind, what string) error {
	for _, known := range scopes {
		if known == scopeKind {
			return nil
		}
	}
	return refuse(contract.RefusalLinkNotActive, "a "+what+" scope is one of "+strings.Join(scopes, ", ")+", not "+repr(scopeKind))
}

// checkDeclarer is Capacity._check_declarer: the declarer owns the scope it speaks for; the
// store scope takes any live supervisor binding, as recorded.
func (c *Capacity) checkDeclarer(ctx context.Context, scopeKind, scopeKey, actor string) error {
	if scopeKind == scopeStore {
		var task string
		err := c.Store.Querier(ctx).QueryRowContext(ctx, "SELECT task_id FROM scope_bindings"+
			"  WHERE task_id = ? AND role = 'supervisor'"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL", actor).Scan(&task)
		if noRows(err) {
			return refuse(contract.RefusalScopeRoleMismatch, "task "+repr(actor)+" holds no live supervisor binding, and the store"+
				" scope has no owner of its own to speak for it")
		}
		return err
	}
	role := roleSupervisor
	if scopeKind == scopeProject {
		role = roleParent
	}
	held, err := owners(ctx, c.Store, scopeKind, scopeKey, role)
	if err != nil {
		return err
	}
	if len(held) == 1 && held[0] == actor {
		return nil
	}
	which := ", which has " + strconv.Itoa(len(held)) + " live owners"
	if len(held) == 1 {
		which = ", which is held by " + repr(held[0])
	}
	return refuse(contract.RefusalScopeRoleMismatch, "task "+repr(actor)+" is not the registered "+role+" of "+
		scopeKind+" "+repr(scopeKey)+which+", so it cannot state a bound for it")
}

// Reservation is Capacity.reserve's keyword arguments; Detail invalid is None.
type Reservation struct {
	SubjectKind, SubjectKey, ParentTask, Project, ReservedBy string
	Detail                                                   sql.NullString
}

// Reserve is Capacity.reserve: the count, the decision and the insert share one BEGIN
// IMMEDIATE, so two interleaved callers cannot both pass a ceiling of one.
func (c *Capacity) Reserve(ctx context.Context, in Reservation) (contract.OrderedObject, error) {
	if err := exact(in.Project, "a project key"); err != nil {
		return nil, err
	}
	if err := exact(in.ParentTask, "a task id"); err != nil {
		return nil, err
	}
	now := c.Now()
	var decided *refusal
	var existing *store.ExecutionSlotsRow
	err := c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		initiative, err := above(ctx, c.Store, in.Project)
		if err != nil {
			return err
		}
		parents, err := owners(ctx, c.Store, scopeProject, in.Project, roleParent)
		if err != nil {
			return err
		}
		switch {
		case len(parents) > 1:
			sorted := append([]string(nil), parents...)
			sortStrings(sorted)
			quoted := make([]string, len(sorted))
			for i, t := range sorted {
				quoted[i] = repr(t)
			}
			decided = &refusal{contract.RefusalDuplicateScopeOwner, "project " + repr(in.Project) + " has more than one live parent (" +
				strings.Join(quoted, ", ") + "), so there is no owner to reserve under", domainExecution, in.SubjectKey, sorted[0], in.ParentTask}
		case len(parents) == 0:
			decided = &refusal{contract.RefusalUnregisteredScope, "project " + repr(in.Project) + " has no registered parent",
				domainExecution, in.SubjectKey, "", in.ParentTask}
		case parents[0] != in.ParentTask:
			decided = &refusal{contract.RefusalScopeRoleMismatch, "task " + repr(in.ParentTask) + " is not the registered parent of project" +
				" " + repr(in.Project) + ", which is held by " + repr(parents[0]), domainExecution, in.SubjectKey, parents[0], in.ParentTask}
		}
		if decided == nil {
			row, err := c.Store.HeldExecutionSlot(ctx, in.SubjectKind, in.SubjectKey, held)
			switch {
			case noRows(err):
			case err != nil:
				return err
			case row.ParentTaskID != in.ParentTask || row.ProjectKey != in.Project:
				decided = &refusal{contract.RefusalDispositionConflict, repr(in.SubjectKey) + " is already held by " +
					repr(row.ParentTaskID) + " for project " + repr(row.ProjectKey) + ", not by " + repr(in.ParentTask) +
					" for " + repr(in.Project), domainExecution, in.SubjectKey, row.ParentTaskID, in.ParentTask}
			default:
				existing = &row
			}
		}
		var initiativeKey sql.NullString
		if initiative != nil {
			initiativeKey = sql.NullString{String: initiative.key, Valid: true}
		}
		if decided == nil && existing == nil {
			if decided, err = c.ceilingRefusal(ctx, in.Project, initiativeKey, in.ParentTask, in.SubjectKey); err != nil {
				return err
			}
		}
		if decided != nil {
			return recordIn(ctx, c.Store, decided, now)
		}
		if existing != nil {
			return nil
		}
		highest, err := c.Store.HighestSlotTenure(ctx, in.SubjectKind, in.SubjectKey)
		if err != nil {
			return err
		}
		tenure := highest + 1
		identifier, err := SlotID(in.SubjectKind, in.SubjectKey, tenure)
		if err != nil {
			return err
		}
		if err := c.Store.InsertExecutionSlot(ctx, store.ExecutionSlotsRow{
			SlotID: identifier, SubjectKind: in.SubjectKind, SubjectKey: in.SubjectKey, ParentTaskID: in.ParentTask,
			ProjectKey: in.Project, InitiativeKey: initiativeKey, Tenure: tenure, State: held, ReservedBy: in.ReservedBy,
			ReservedAt: now, Detail: in.Detail,
		}); err != nil {
			return err
		}
		return journal(ctx, c.Store, "slot_reserved", identifier, contract.OrderedObject{
			{Key: "subjectKey", Value: in.SubjectKey}, {Key: "parentTaskId", Value: in.ParentTask}, {Key: "tenure", Value: tenure},
		}, now)
	})
	if err != nil {
		return nil, err
	}
	if decided != nil {
		return nil, decided.err()
	}
	if existing != nil {
		return append(slotRecord(*existing), contract.Field{Key: "alreadyHeld", Value: true}), nil
	}
	answer, err := c.Slot(ctx, in.SubjectKind, in.SubjectKey)
	if err != nil {
		return nil, err
	}
	return append(answer, contract.Field{Key: "alreadyHeld", Value: false}), nil
}

// ceilingRefusal is Capacity._ceiling_refusal: every enforced ceiling in scope, runs counted
// and everything else observed.
func (c *Capacity) ceilingRefusal(ctx context.Context, project string, initiative sql.NullString, parent, subject string) (*refusal, error) {
	type scope struct{ kind, key string }
	checked := []scope{{scopeProject, project}, {scopeStore, scopeStore}}
	if initiative.Valid {
		checked = append([]scope{{scopeInitiative, initiative.String}}, checked...)
	}
	for _, sc := range checked {
		limits, err := c.Store.EnforcedExecutionLimits(ctx, sc.kind, sc.key)
		if err != nil {
			return nil, err
		}
		for _, row := range limits {
			if row.Dimension == runs {
				used, err := c.Store.RunsIn(ctx, sc.kind, sc.key, held)
				if err != nil {
					return nil, err
				}
				if float64(used) >= row.Ceiling {
					return &refusal{contract.RefusalCapacityExhausted, sc.kind + " " + repr(sc.key) + " already holds " +
						strconv.FormatInt(used, 10) + " of " + pyFloat(row.Ceiling) + " runs", domainExecution, subject, sc.key, parent}, nil
				}
				continue
			}
			seen, err := c.Store.ExecutionUsage(ctx, sc.kind, sc.key, row.Dimension)
			if noRows(err) {
				return &refusal{contract.RefusalCapacityUnmeasured, "an enforced ceiling of " + pyFloat(row.Ceiling) + " " +
					row.Unit + " is declared for " + repr(row.Dimension) + " on " + sc.kind + " " + repr(sc.key) +
					" and nothing has measured it. A count of running tasks is not a measurement of this dimension, so" +
					" there is no basis to say whether the bound holds", domainExecution, subject, row.Dimension, parent}, nil
			}
			if err != nil {
				return nil, err
			}
			if seen.Observed >= row.Ceiling {
				return &refusal{contract.RefusalCapacityExhausted, repr(row.Dimension) + " was observed at " + pyFloat(seen.Observed) +
					" " + row.Unit + " against a ceiling of " + pyFloat(row.Ceiling), domainExecution, subject, row.Dimension, parent}, nil
			}
		}
	}
	return nil, nil
}

// Release is Capacity.release's keyword arguments; an invalid Tenure is None.
type Release struct {
	SubjectKind, SubjectKey, ReleasedBy, Reason string
	Tenure                                      sql.NullInt64
}

// Release gives the slot back. Idempotent on the subject; a changed reason is refused and the
// contest retained.
func (c *Capacity) Release(ctx context.Context, in Release) (contract.OrderedObject, error) {
	if err := exact(in.Reason, "a release reason"); err != nil {
		return nil, err
	}
	now := c.Now()
	var decided *refusal
	var already *store.ExecutionSlotsRow
	err := c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		tenures, err := c.Store.ExecutionSlotTenures(ctx, in.SubjectKind, in.SubjectKey)
		if err != nil {
			return err
		}
		var row *store.ExecutionSlotsRow
		switch {
		case in.Tenure.Valid:
			for i := range tenures {
				if tenures[i].Tenure == in.Tenure.Int64 {
					row = &tenures[i]
					break
				}
			}
		case len(tenures) > 1:
			numbers := make([]any, len(tenures))
			for i, t := range tenures {
				numbers[i] = t.Tenure
			}
			return refuse(contract.RefusalDispositionConflict, in.SubjectKind+" "+repr(in.SubjectKey)+" has tenures "+
				reprInts(numbers)+"; name the one this release settles, because the newest is not necessarily the one a"+
				" delayed notification is about")
		case len(tenures) == 1:
			row = &tenures[0]
		}
		if row == nil {
			return refuse(contract.RefusalSlotUnknown, "no slot was ever reserved for "+in.SubjectKind+" "+repr(in.SubjectKey))
		}
		if row.ParentTaskID != in.ReleasedBy {
			supervisor, err := above(ctx, c.Store, row.ProjectKey)
			if err != nil {
				return err
			}
			if supervisor == nil || !supervisor.owned || supervisor.owner != in.ReleasedBy {
				return refuse(contract.RefusalScopeRoleMismatch, "slot "+repr(row.SlotID)+" is held by "+
					repr(row.ParentTaskID)+", so "+repr(in.ReleasedBy)+" cannot release it")
			}
		}
		if row.State == released {
			if row.ReleaseReason.String != in.Reason {
				decided = &refusal{contract.RefusalDispositionConflict, "slot " + repr(row.SlotID) + " was released as " +
					reprNullable(row.ReleaseReason) + " by " + reprNullable(row.ReleasedBy) +
					"; a later notification restates that reason rather than replacing it",
					domainExecution, in.SubjectKey, row.ReleaseReason.String, in.Reason}
				return recordIn(ctx, c.Store, decided, now)
			}
			already = row
			return nil
		}
		if _, err := c.Store.ReleaseExecutionSlot(ctx, row.SlotID, released, now, in.ReleasedBy, in.Reason, held); err != nil {
			return err
		}
		return journal(ctx, c.Store, "slot_released", row.SlotID, contract.OrderedObject{
			{Key: "subjectKey", Value: in.SubjectKey}, {Key: "reason", Value: in.Reason},
		}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if decided != nil {
		return nil, decided.err()
	}
	var answer contract.OrderedObject
	if already != nil {
		answer = slotRecord(*already)
	} else if answer, err = c.Slot(ctx, in.SubjectKind, in.SubjectKey); err != nil {
		return nil, err
	}
	return append(answer, contract.Field{Key: "alreadyReleased", Value: already != nil}), nil
}

// Limit is Capacity.declare_limit's keyword arguments.
type Limit struct {
	ScopeKind, ScopeKey, Dimension, Unit string
	Ceiling                              float64
	DeclaredBy, Source                   string
	Enforce                              bool
}

// DeclareLimit is Capacity.declare_limit: lowering a ceiling below current use revokes nothing
// and says so.
func (c *Capacity) DeclareLimit(ctx context.Context, in Limit) (contract.OrderedObject, error) {
	if err := checkScopeKind(in.ScopeKind, "limit"); err != nil {
		return nil, err
	}
	if err := checkScope(in.ScopeKind, in.ScopeKey); err != nil {
		return nil, err
	}
	if err := c.checkDeclarer(ctx, in.ScopeKind, in.ScopeKey, in.DeclaredBy); err != nil {
		return nil, err
	}
	if err := finite(in.Ceiling, "a ceiling"); err != nil {
		return nil, err
	}
	identifier, err := LimitID(in.ScopeKind, in.ScopeKey, in.Dimension)
	if err != nil {
		return nil, err
	}
	now := c.Now()
	err = c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		previous, err := c.Store.ExecutionLimit(ctx, identifier)
		var revision int64 = 1
		var previousCeiling any
		switch {
		case noRows(err):
		case err != nil:
			return err
		default:
			revision, previousCeiling = previous.Revision+1, previous.Ceiling
		}
		var enforce int64
		if in.Enforce {
			enforce = 1
		}
		if err := c.Store.DeclareExecutionLimit(ctx, store.ExecutionLimitsRow{
			LimitID: identifier, ScopeKind: in.ScopeKind, ScopeKey: in.ScopeKey, Dimension: in.Dimension, Unit: in.Unit,
			Ceiling: in.Ceiling, Enforce: enforce, DeclaredBy: in.DeclaredBy, Source: in.Source, Revision: revision,
			DeclaredAt: now, UpdatedAt: now,
		}); err != nil {
			return err
		}
		return journal(ctx, c.Store, "execution_limit_declared", identifier, contract.OrderedObject{
			{Key: "dimension", Value: in.Dimension}, {Key: "ceiling", Value: in.Ceiling},
			{Key: "previousCeiling", Value: previousCeiling}, {Key: "revision", Value: revision},
		}, now)
	})
	if err != nil {
		return nil, err
	}
	room, err := c.Headroom(ctx, in.ScopeKind, in.ScopeKey)
	if err != nil {
		return nil, err
	}
	for _, entry := range room[2].Value.([]any) {
		if object := entry.(contract.OrderedObject); object[0].Value == in.Dimension {
			return append(object, contract.Field{Key: "limitId", Value: identifier}), nil
		}
	}
	return nil, sql.ErrNoRows
}

// Observation is Capacity.observe's keyword arguments.
type Observation struct {
	ScopeKind, ScopeKey, Dimension string
	Observed                       float64
	ObservedBy, Method             string
}

// Observe records a measurement: the only way a non-runs dimension gets a current value.
func (c *Capacity) Observe(ctx context.Context, in Observation) (contract.OrderedObject, error) {
	if err := exact(in.Method, "a measurement method"); err != nil {
		return nil, err
	}
	if err := checkScopeKind(in.ScopeKind, "usage"); err != nil {
		return nil, err
	}
	if err := exact(in.Dimension, "a dimension"); err != nil {
		return nil, err
	}
	if in.Dimension == runs {
		return nil, refuse(contract.RefusalLinkNotActive, "runs is derived from held slots and is never observed; recording one would"+
			" put a second, disagreeing figure beside the counted one")
	}
	if err := checkScope(in.ScopeKind, in.ScopeKey); err != nil {
		return nil, err
	}
	if err := c.checkDeclarer(ctx, in.ScopeKind, in.ScopeKey, in.ObservedBy); err != nil {
		return nil, err
	}
	if err := finite(in.Observed, "an observation"); err != nil {
		return nil, err
	}
	now := c.Now()
	err := c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return c.Store.ObserveExecutionUsage(ctx, store.ExecutionUsageRow{
			ScopeKind: in.ScopeKind, ScopeKey: in.ScopeKey, Dimension: in.Dimension, Observed: in.Observed,
			ObservedBy: in.ObservedBy, Method: in.Method, ObservedAt: now,
		})
	})
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "scopeKind", Value: in.ScopeKind}, {Key: "scopeKey", Value: in.ScopeKey}, {Key: "dimension", Value: in.Dimension},
		{Key: "observed", Value: in.Observed}, {Key: "observedBy", Value: in.ObservedBy}, {Key: "method", Value: in.Method},
		{Key: "observedAt", Value: now},
	}, nil
}

func reprInts(values []any) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.FormatInt(v.(int64), 10)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func reprNullable(v sql.NullString) string {
	if !v.Valid {
		return "None"
	}
	return repr(v.String)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
