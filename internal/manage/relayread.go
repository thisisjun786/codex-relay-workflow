package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// crw manage relay-read: one read-only projection of the relay store. The GUI calls
// RelayReadState instead of opening the store itself, so the store's shape is read in one place
// and every field comes from the relay's own readers rather than a second implementation of them.

// The store this command reads, the document it prints, and the words its read marks use.
const (
	relayReadStoreFile   = "relay.sqlite3"
	relayReadSchema      = "crw-relay-read/1"
	relayReadBusyMillis  = 5000
	relayReadReadOK      = "ok"
	relayReadReadUnknown = "unknown"
)

// The four sections of the projection, as the failures list names them.
const (
	relayReadSectionBindings      = "bindings"
	relayReadSectionRelationships = "relationships"
	relayReadSectionPlans         = "plans"
	relayReadSectionMergeTurns    = "mergeTurns"
)

// The command's exit statuses; usageExit (2) is the one this package shares.
const (
	relayReadUnknownExit = 1
	relayReadStoreExit   = 3
)

// The named errors a caller filters with errors.Is. A store that is absent or cannot be opened is
// one of these and never an empty projection, because a blank projection reads as a relay with
// nothing in it.
var (
	ErrRelayStateUnconfigured = errors.New("relay_read_state_unconfigured")
	ErrRelayStoreAbsent       = errors.New("relay_read_store_absent")
	ErrRelayStoreUnreadable   = errors.New("relay_read_store_unreadable")
)

// RelayReadOptions chooses what the projection covers. An empty Plans or Projects means every one
// the store carries; IncludeClosed false keeps only live relationships, active or paused bindings
// and merge turns that have not closed.
//
// A project selector narrows every section to that project: its relationships, its project-scope
// bindings, its plans and its merge turns. A plan selector narrows the sections the plan owns: its
// progress, the relationships that execute its nodes (the link the relay keeps in
// dag_node_executions) and those relationships' merge turns. Both selectors may be given; a plan
// the caller named that the selection leaves out is still reported, with an unknown read mark, so a
// named target never vanishes without one.
type RelayReadOptions struct {
	Plans         []string
	Projects      []string
	IncludeClosed bool
}

// RelayReadMark is how one item's own source read went. A section that could not be read at all is
// null with a line in RelayProjection.Failures instead.
type RelayReadMark struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// RelayBinding is one scope binding, as the registry's binding record spells it.
type RelayBinding struct {
	BindingID any           `json:"bindingId"`
	Role      any           `json:"role"`
	ScopeKind any           `json:"scopeKind"`
	ScopeKey  any           `json:"scopeKey"`
	TaskID    any           `json:"taskId"`
	HostID    any           `json:"hostId"`
	Cwd       any           `json:"cwd"`
	Status    any           `json:"status"`
	Revision  any           `json:"revision"`
	Read      RelayReadMark `json:"read"`
}

// RelayHead is the reviewable head the assignment view decided on: the relay's own reading, its
// ambiguity included and never resolved here.
type RelayHead struct {
	EventID      any `json:"eventId"`
	RevisionHash any `json:"revisionHash"`
	Evidence     any `json:"evidence"`
	Competitors  any `json:"competitors"`
	Detail       any `json:"detail"`
}

// RelayVerdict is the four fields of a last verdict the projection carries; the criteria memo and
// the coverage stay in the store.
type RelayVerdict struct {
	Verdict             any `json:"verdict"`
	EventID             any `json:"eventId"`
	ExecutionGeneration any `json:"executionGeneration"`
	DecidedAt           any `json:"decidedAt"`
}

// RelayRelationship is one assignment's state, as the assignment view reports it.
type RelayRelationship struct {
	RelationshipID      any           `json:"relationshipId"`
	IssueKey            any           `json:"issueKey"`
	ParentTaskID        any           `json:"parentTaskId"`
	ChildTaskID         any           `json:"childTaskId"`
	RelationshipStatus  any           `json:"relationshipStatus"`
	ExecutionGeneration any           `json:"executionGeneration"`
	State               any           `json:"state"`
	Head                *RelayHead    `json:"head"`
	LastVerdict         *RelayVerdict `json:"lastVerdict"`
	NextExpectedAction  any           `json:"nextExpectedAction"`
	Read                RelayReadMark `json:"read"`
}

// RelayPlan is one DAG plan's progress: the stage counts under the scheduler's own stage names,
// the blocked overlay's size and the denominator those counts are of.
type RelayPlan struct {
	PlanID      any            `json:"planId"`
	ProjectKey  any            `json:"projectKey"`
	Revision    any            `json:"revision"`
	Stages      map[string]int `json:"stages"`
	Blocked     any            `json:"blocked"`
	Denominator any            `json:"denominator"`
	Read        RelayReadMark  `json:"read"`
}

// RelayMergeTurn is one merge turn's own record, as the merge turn service reports it.
type RelayMergeTurn struct {
	TurnID       any           `json:"turnId"`
	Repository   any           `json:"repository"`
	PRNumber     any           `json:"prNumber"`
	HolderTaskID any           `json:"holderTaskId"`
	State        any           `json:"state"`
	RequestedAt  any           `json:"requestedAt"`
	UpdatedAt    any           `json:"updatedAt"`
	Read         RelayReadMark `json:"read"`
}

// RelayFailure is one section that could not be read: its value is null and this is why.
type RelayFailure struct {
	Section string `json:"section"`
	Item    string `json:"item"`
	Reason  string `json:"reason"`
}

// RelayProjection is the whole document. A section is null when its source could not be read and an
// array otherwise, and a null is never turned into zero, an empty array or a success.
type RelayProjection struct {
	Schema        string              `json:"schema"`
	StateDir      string              `json:"stateDir"`
	ReadAt        string              `json:"readAt"`
	Bindings      []RelayBinding      `json:"bindings"`
	Relationships []RelayRelationship `json:"relationships"`
	Plans         []RelayPlan         `json:"plans"`
	MergeTurns    []RelayMergeTurn    `json:"mergeTurns"`
	Failures      []RelayFailure      `json:"failures"`
}

// relayReadRecord is one of the relay's ordered records, read by key: the shape both the registry
// and the merge turn service answer with.
type relayReadRecord interface{ Get(string) any }

// relayReadStorePath is the store this command reads: the named file below the relay state
// directory, exactly as the relay's own readers spell it.
func relayReadStorePath(stateDir string) string {
	return filepath.Join(stateDir, relayReadStoreFile)
}

// relayReadOpenStore opens the relay store read-only under the store's own no-sidecar rule, so a
// read creates neither the write-ahead log nor the shared-memory index beside a store whose log
// holds no frame. store.OpenInPlace resolves the path the way SQLite does and then makes SQLite
// name that same file as the connection's main database. It takes the state directory and returns
// the read-only handle, so a sibling store reading in this package can reuse it; the read itself is
// the caller's ReadSnapshot.
func relayReadOpenStore(ctx context.Context, stateDir string) (*store.ReadOnly, error) {
	if stateDir == "" {
		return nil, ErrRelayStateUnconfigured
	}
	path := relayReadStorePath(stateDir)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no relay store at %s", ErrRelayStoreAbsent, path)
		}
		return nil, fmt.Errorf("%w: %v", ErrRelayStoreUnreadable, err)
	}
	handle, err := store.OpenInPlace(ctx, path, relayReadBusyMillis*time.Millisecond)
	if err != nil {
		// A context that ended is that context's error, not an unreadable store.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: no relay store at %s", ErrRelayStoreAbsent, path)
		}
		return nil, fmt.Errorf("%w: %v", ErrRelayStoreUnreadable, err)
	}
	return handle, nil
}

// RelayReadState is the projection of the relay store below stateDir. Every section is read inside
// one snapshot, through the relay's own readers, and a source that cannot be read is reported
// rather than replaced with an empty answer.
func RelayReadState(ctx context.Context, stateDir string, opts RelayReadOptions) (RelayProjection, error) {
	handle, err := relayReadOpenStore(ctx, stateDir)
	if err != nil {
		return RelayProjection{}, err
	}
	defer handle.Close()
	projection := RelayProjection{
		Schema: relayReadSchema, StateDir: stateDir, ReadAt: time.Now().UTC().Format(time.RFC3339),
		Failures: []RelayFailure{},
	}
	if err := handle.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		// ReadSnapshot builds a bare Store; the path it was opened from is what the assignment
		// view's store_directory reads, so a reader that needs the store's own directory sees it
		// rather than failing on an empty path.
		st.Path = relayReadStorePath(stateDir)
		relayReadSections(ctx, st, opts, &projection)
		return nil
	}); err != nil {
		// A context that ended is its own error: it is not a store this command could not read.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RelayProjection{}, ctxErr
		}
		return RelayProjection{}, fmt.Errorf("%w: %v", ErrRelayStoreUnreadable, err)
	}
	return projection, nil
}

// RelayRead is RelayReadState over the state directory the configuration names. An empty
// cfg.Relay.State is the one the relay's own doctor answer selects, asked through the same helper
// the other manage commands use.
func RelayRead(ctx context.Context, e *Env, cfg *Config, opts RelayReadOptions) (RelayProjection, error) {
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return RelayProjection{}, err
	}
	return RelayReadState(ctx, state, opts)
}

// relayReadSections reads every section into the projection. A section whose list query fails is
// left null with one failure line; an item whose own source fails carries an unknown read mark.
func relayReadSections(ctx context.Context, st *store.Store, opts RelayReadOptions, out *RelayProjection) {
	out.Bindings = relayReadBindings(ctx, st, opts, out)
	out.Relationships = relayReadRelationships(ctx, st, opts, out)
	out.Plans = relayReadPlans(ctx, st, opts, out)
	out.MergeTurns = relayReadMergeTurns(ctx, st, opts, out)
}

// relayReadFail records a section that could not be read; its value stays null.
func relayReadFail(out *RelayProjection, section, item string, err error) {
	out.Failures = append(out.Failures, RelayFailure{Section: section, Item: item, Reason: err.Error()})
}

// relayReadPlaceholders is n "?"s for an IN list, the form the store's own readings use.
func relayReadPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// relayReadArgs turns a string list into query arguments.
func relayReadArgs(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

// relayReadIDs runs one list query. It chooses which items to read and decides nothing about them;
// a table the store does not carry is the section's failure.
func relayReadIDs(ctx context.Context, st *store.Store, query string, args []any) ([]string, error) {
	rows, err := st.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// The live relationship predicate and the open merge turn predicate, the rules the registry's own
// readers and the merge turn service use.
const (
	relayReadLiveRelationshipFilter = "status IN ('active', 'paused') AND superseded_by IS NULL"
	relayReadOpenTurnFilter         = "state IN ('waiting', 'holding', 'merging', 'unknown')"
)

// relayReadRelationships reads the assignment state of each chosen relationship.
func relayReadRelationships(ctx context.Context, st *store.Store, opts RelayReadOptions, out *RelayProjection) []RelayRelationship {
	query := "SELECT r.relationship_id FROM relationships r"
	var args []any
	var conditions []string
	if len(opts.Projects) > 0 {
		query += " JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
		conditions = append(conditions, "s.project_key IN ("+relayReadPlaceholders(len(opts.Projects))+")")
		args = append(args, relayReadArgs(opts.Projects)...)
	}
	if len(opts.Plans) > 0 {
		// A plan selector narrows the relationships to the ones that execute a node of the plan, the
		// same link the merge turns are narrowed by, so the two sections agree on which lanes a plan
		// owns.
		conditions = append(conditions, "EXISTS (SELECT 1 FROM dag_node_executions e WHERE e.relationship_id = r.relationship_id"+
			" AND e.plan_id IN ("+relayReadPlaceholders(len(opts.Plans))+"))")
		args = append(args, relayReadArgs(opts.Plans)...)
	}
	if !opts.IncludeClosed {
		conditions = append(conditions, "r."+relayReadLiveRelationshipFilter)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY r.created_at, r.relationship_id"
	ids, err := relayReadIDs(ctx, st, query, args)
	if err != nil {
		relayReadFail(out, relayReadSectionRelationships, "", err)
		return nil
	}
	view := registry.NewAssignmentView(&registry.Registry{Store: st})
	items := make([]RelayRelationship, 0, len(ids))
	for _, rid := range ids {
		item := RelayRelationship{RelationshipID: rid, Read: RelayReadMark{State: relayReadReadOK}}
		state, err := view.State(ctx, rid)
		if err != nil {
			item.Read = RelayReadMark{State: relayReadReadUnknown, Reason: err.Error()}
			items = append(items, item)
			continue
		}
		item.IssueKey = state.Get("issueKey")
		item.ParentTaskID = state.Get("parentTaskId")
		item.ChildTaskID = state.Get("childTaskId")
		item.RelationshipStatus = state.Get("relationshipStatus")
		item.ExecutionGeneration = state.Get("executionGeneration")
		item.State = state.Get("state")
		item.NextExpectedAction = state.Get("nextExpectedAction")
		if head, ok := state.Get("head").(relayReadRecord); ok && head != nil {
			item.Head = &RelayHead{EventID: head.Get("eventId"), RevisionHash: head.Get("revisionHash"),
				Evidence: head.Get("evidence"), Competitors: head.Get("competitors"), Detail: head.Get("detail")}
		}
		if verdict, ok := state.Get("lastVerdict").(relayReadRecord); ok && verdict != nil {
			item.LastVerdict = &RelayVerdict{Verdict: verdict.Get("verdict"), EventID: verdict.Get("eventId"),
				ExecutionGeneration: verdict.Get("executionGeneration"), DecidedAt: verdict.Get("decidedAt")}
		}
		items = append(items, item)
	}
	return items
}

// relayReadPlans reads the progress of each chosen plan under the scheduler's own stage names.
func relayReadPlans(ctx context.Context, st *store.Store, opts RelayReadOptions, out *RelayProjection) []RelayPlan {
	query := "SELECT plan_id FROM dag_plans"
	var args []any
	var conditions []string
	if len(opts.Plans) > 0 {
		conditions = append(conditions, "plan_id IN ("+relayReadPlaceholders(len(opts.Plans))+")")
		args = append(args, relayReadArgs(opts.Plans)...)
	}
	if len(opts.Projects) > 0 {
		conditions = append(conditions, "project_key IN ("+relayReadPlaceholders(len(opts.Projects))+")")
		args = append(args, relayReadArgs(opts.Projects)...)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY plan_id"
	ids, err := relayReadIDs(ctx, st, query, args)
	if err != nil {
		relayReadFail(out, relayReadSectionPlans, "", err)
		return nil
	}
	// A plan the caller named is kept even when a selector left it out, so a named target never
	// vanishes from the projection without a mark. The scheduler refuses one the store does not
	// carry, and one the project selector excluded is refused here, so both read as an item whose
	// source could not be read rather than as a shorter, successful list.
	excluded, err := relayReadExcludedPlans(ctx, st, opts, ids)
	if err != nil {
		relayReadFail(out, relayReadSectionPlans, "", err)
		return nil
	}
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	for _, wanted := range opts.Plans {
		if !selected[wanted] {
			ids = append(ids, wanted)
			selected[wanted] = true
		}
	}
	sort.Strings(ids)
	scheduler := &dagsched.Scheduler{Store: st}
	items := make([]RelayPlan, 0, len(ids))
	for _, plan := range ids {
		item := RelayPlan{PlanID: plan, Read: RelayReadMark{State: relayReadReadOK}}
		if reason, ok := excluded[plan]; ok {
			item.Read = RelayReadMark{State: relayReadReadUnknown, Reason: reason}
			items = append(items, item)
			continue
		}
		progress, err := scheduler.Progress(ctx, st.Q(ctx), plan)
		if err != nil {
			item.Read = RelayReadMark{State: relayReadReadUnknown, Reason: err.Error()}
			items = append(items, item)
			continue
		}
		item.ProjectKey = progress.ProjectKey
		item.Revision = progress.Denominator.Revision
		item.Blocked = progress.Blocked.Nodes
		item.Denominator = progress.Denominator.Nodes
		item.Stages = map[string]int{}
		for _, stage := range progress.Stages {
			item.Stages[stage.Stage] = stage.Nodes
		}
		items = append(items, item)
	}
	return items
}

// relayReadExcludedPlans is the named plans the store carries but the selection left out, with the
// reason each is unread: the existence check is made without the project predicate, so a plan the
// store holds and a selector excluded is reported rather than dropped, while one the store does not
// hold at all is left to the scheduler's own refusal.
func relayReadExcludedPlans(ctx context.Context, st *store.Store, opts RelayReadOptions, selected []string) (map[string]string, error) {
	excluded := map[string]string{}
	if len(opts.Plans) == 0 || len(opts.Projects) == 0 {
		return excluded, nil
	}
	chosen := map[string]bool{}
	for _, id := range selected {
		chosen[id] = true
	}
	var wanted []string
	for _, id := range opts.Plans {
		if !chosen[id] {
			wanted = append(wanted, id)
		}
	}
	if len(wanted) == 0 {
		return excluded, nil
	}
	known, err := relayReadIDs(ctx, st,
		"SELECT plan_id FROM dag_plans WHERE plan_id IN ("+relayReadPlaceholders(len(wanted))+")",
		relayReadArgs(wanted))
	if err != nil {
		return nil, err
	}
	for _, id := range known {
		excluded[id] = "the plan belongs to a project outside the selection"
	}
	return excluded, nil
}

// relayReadMergeTurns reads the record of each chosen merge turn.
func relayReadMergeTurns(ctx context.Context, st *store.Store, opts RelayReadOptions, out *RelayProjection) []RelayMergeTurn {
	query := "SELECT turn_id FROM merge_turns"
	var conditions []string
	var args []any
	if !opts.IncludeClosed {
		conditions = append(conditions, relayReadOpenTurnFilter)
	}
	if len(opts.Plans) > 0 {
		// A plan selector narrows the turns to the ones whose relationship executes a node of that
		// plan, the link the relay itself keeps in dag_node_executions.
		conditions = append(conditions, "EXISTS (SELECT 1 FROM dag_node_executions e WHERE e.relationship_id = merge_turns.relationship_id"+
			" AND e.plan_id IN ("+relayReadPlaceholders(len(opts.Plans))+"))")
		args = append(args, relayReadArgs(opts.Plans)...)
	}
	if len(opts.Projects) > 0 {
		conditions = append(conditions, "project_key IN ("+relayReadPlaceholders(len(opts.Projects))+")")
		args = append(args, relayReadArgs(opts.Projects)...)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY turn_id"
	ids, err := relayReadIDs(ctx, st, query, args)
	if err != nil {
		relayReadFail(out, relayReadSectionMergeTurns, "", err)
		return nil
	}
	service := &mergeturn.Service{Store: st}
	items := make([]RelayMergeTurn, 0, len(ids))
	for _, turn := range ids {
		item := RelayMergeTurn{TurnID: turn, Read: RelayReadMark{State: relayReadReadOK}}
		record, err := service.Turn(ctx, turn)
		if err != nil {
			item.Read = RelayReadMark{State: relayReadReadUnknown, Reason: err.Error()}
			items = append(items, item)
			continue
		}
		if record == nil {
			item.Read = RelayReadMark{State: relayReadReadUnknown, Reason: "the store no longer holds this merge turn"}
			items = append(items, item)
			continue
		}
		item.Repository = record["repository"]
		item.PRNumber = record["prNumber"]
		item.HolderTaskID = record["holderTaskId"]
		item.State = record["state"]
		item.RequestedAt = record["requestedAt"]
		item.UpdatedAt = record["updatedAt"]
		items = append(items, item)
	}
	return items
}

// relayReadScope is one scope binding group: the kind and key the owners are read by.
type relayReadScope struct{ kind, key string }

// relayReadBindings reads every live owner of every chosen scope through the registry's own
// Owners, and every binding of the scope when the caller asked for closed ones.
func relayReadBindings(ctx context.Context, st *store.Store, opts RelayReadOptions, out *RelayProjection) []RelayBinding {
	query := "SELECT DISTINCT scope_kind, scope_key FROM scope_bindings"
	var args []any
	if len(opts.Projects) > 0 {
		// A project selector narrows the scope list to project-scope bindings of that project, which
		// is what naming a project asks for; a caller that wants every scope kind leaves the
		// selector off. The rows themselves are still read through the registry's Owners.
		query += " WHERE scope_kind = 'project' AND scope_key IN (" + relayReadPlaceholders(len(opts.Projects)) + ")"
		args = relayReadArgs(opts.Projects)
	}
	query += " ORDER BY scope_kind, scope_key"
	scopes, err := relayReadScopes(ctx, st, query, args)
	if err != nil {
		relayReadFail(out, relayReadSectionBindings, "", err)
		return nil
	}
	registryOf := &registry.Registry{Store: st}
	items := []RelayBinding{}
	for _, scope := range scopes {
		if !opts.IncludeClosed {
			owners, err := registryOf.Owners(ctx, scope.kind, scope.key)
			if err != nil {
				relayReadFail(out, relayReadSectionBindings, scope.kind+"="+scope.key, err)
				return nil
			}
			for _, owner := range owners {
				items = append(items, relayReadBindingRecord(owner.Get("bindingId"), owner))
			}
			continue
		}
		ids, err := relayReadIDs(ctx, st,
			"SELECT binding_id FROM scope_bindings WHERE scope_kind = ? AND scope_key = ? ORDER BY revision DESC, binding_id",
			[]any{scope.kind, scope.key})
		if err != nil {
			relayReadFail(out, relayReadSectionBindings, scope.kind+"="+scope.key, err)
			return nil
		}
		for _, id := range ids {
			record, err := registryOf.Binding(ctx, id)
			if err != nil {
				items = append(items, RelayBinding{BindingID: id, Read: RelayReadMark{State: relayReadReadUnknown, Reason: err.Error()}})
				continue
			}
			if record == nil {
				// A row the store no longer carries is reported like every other unread source
				// rather than dropped, so an item never vanishes from the projection unnoticed.
				items = append(items, RelayBinding{BindingID: id, Read: RelayReadMark{State: relayReadReadUnknown,
					Reason: "the store no longer holds this binding"}})
				continue
			}
			items = append(items, relayReadBindingRecord(id, record))
		}
	}
	return items
}

// relayReadScopes reads the scope list the bindings are grouped by.
func relayReadScopes(ctx context.Context, st *store.Store, query string, args []any) ([]relayReadScope, error) {
	rows, err := st.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scopes []relayReadScope
	for rows.Next() {
		var scope relayReadScope
		if err := rows.Scan(&scope.kind, &scope.key); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// relayReadBindingRecord is one binding record as the projection carries it.
func relayReadBindingRecord(id any, record relayReadRecord) RelayBinding {
	return RelayBinding{
		BindingID: id, Role: record.Get("role"), ScopeKind: record.Get("scopeKind"), ScopeKey: record.Get("scopeKey"),
		TaskID: record.Get("taskId"), HostID: record.Get("hostId"), Cwd: record.Get("cwd"),
		Status: record.Get("status"), Revision: record.Get("revision"), Read: RelayReadMark{State: relayReadReadOK},
	}
}

// relayReadCommand is crw manage relay-read.
var relayReadCommand = Command{Name: "relay-read", Summary: "print a read-only projection of the relay store", Run: relayReadRun}

func init() { Register(relayReadCommand) }

// relayReadUsage is the one line the command prints.
const relayReadUsage = "usage: crw manage relay-read [-h] [--state DIR] [--plan P] [--project K] [--all]"

// relayReadRun is crw manage relay-read. Exit 0 means every section and item was read, 1 that at
// least one was not (the document is printed either way), 2 a command line this command cannot use,
// and 3 that the store could not be read or the document could not be written.
func relayReadRun(ctx context.Context, e *Env, args []string) int {
	opts := RelayReadOptions{}
	state := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprintln(e.Stdout, relayReadUsage)
			return 0
		case arg == "--all":
			opts.IncludeClosed = true
		case arg == "--state", arg == "--plan", arg == "--project":
			// The next token is the value unless it is missing or is itself an option: an option
			// token here is a missing value, not a value that happens to start with "--".
			if i+1 >= len(args) || relayReadIsOptionToken(args[i+1]) {
				return relayReadUsageError(e, arg+" needs a value")
			}
			i++
			relayReadApplyOption(&opts, &state, arg, args[i])
		case strings.HasPrefix(arg, "--state="), strings.HasPrefix(arg, "--plan="), strings.HasPrefix(arg, "--project="):
			name, value, _ := strings.Cut(arg, "=")
			if value == "" {
				return relayReadUsageError(e, name+" needs a value")
			}
			relayReadApplyOption(&opts, &state, name, value)
		default:
			return relayReadUsageError(e, fmt.Sprintf("unexpected argument %q", arg))
		}
	}
	cfg := coreDefaults(e)
	if state != "" {
		cfg.Relay.State = state
	}
	projection, err := RelayRead(ctx, e, cfg, opts)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage relay-read: error: %v\n", err)
		return relayReadStoreExit
	}
	data, err := json.Marshal(projection)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage relay-read: error: %v\n", err)
		return relayReadStoreExit
	}
	if _, err := fmt.Fprintf(e.Stdout, "%s\n", data); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage relay-read: error: %v\n", err)
		return relayReadStoreExit
	}
	if relayReadHasUnknown(projection) {
		return relayReadUnknownExit
	}
	return 0
}

// relayReadIsOptionToken reports whether a token is one of this command's own options rather than
// a value: a value a caller means literally is written with the --name=value form.
func relayReadIsOptionToken(token string) bool {
	switch token {
	case "-h", "--help", "--all", "--state", "--plan", "--project":
		return true
	}
	return false
}

// relayReadApplyOption records one option's value.
func relayReadApplyOption(opts *RelayReadOptions, state *string, name, value string) {
	switch name {
	case "--state":
		*state = value
	case "--plan":
		opts.Plans = append(opts.Plans, value)
	case "--project":
		opts.Projects = append(opts.Projects, value)
	}
}

// relayReadUsageError prints the usage line and the reason and reports a usage error.
func relayReadUsageError(e *Env, reason string) int {
	fmt.Fprintln(e.Stderr, relayReadUsage)
	fmt.Fprintf(e.Stderr, "crw manage relay-read: error: %s\n", reason)
	return usageExit
}

// relayReadHasUnknown reports whether any section was null or any item unread.
func relayReadHasUnknown(projection RelayProjection) bool {
	if projection.Bindings == nil || projection.Relationships == nil || projection.Plans == nil || projection.MergeTurns == nil {
		return true
	}
	for _, item := range projection.Bindings {
		if item.Read.State != relayReadReadOK {
			return true
		}
	}
	for _, item := range projection.Relationships {
		if item.Read.State != relayReadReadOK {
			return true
		}
	}
	for _, item := range projection.Plans {
		if item.Read.State != relayReadReadOK {
			return true
		}
	}
	for _, item := range projection.MergeTurns {
		if item.Read.State != relayReadReadOK {
			return true
		}
	}
	return false
}
