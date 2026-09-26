package capacity

import (
	"context"
	"database/sql"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// editregion.py: what two peer parents agreed about a shared edit region, and what that does
// not mean. The unit is a place (repository, base revision, path, optional symbol or data key),
// never a file name; an agreement confers nothing, and nothing here writes an authorization.

const (
	peerKind = "peer"

	kindTree   = "tree"
	kindFile   = "file"
	kindSymbol = "symbol"
	kindData   = "data"

	classSource    = "source"
	classGenerated = "generated"

	stateProposed  = "proposed"
	stateAgreed    = "agreed"
	stateReopened  = "reopened"
	stateDeclined  = "declined"
	stateWithdrawn = "withdrawn"
	stateReleased  = "released"

	followupOpen     = "open"
	followupAccepted = "accepted"
	followupDone     = "done"
	followupDropped  = "dropped"

	overlapTotal    = "total"
	overlapPartial  = "partial"
	overlapDisjoint = "disjoint"

	acceptanceOnPriorRevision = "acceptance_on_prior_revision"
	notYetAccepted            = "not_yet_accepted"

	marksQuery   = "SELECT from_revision, to_revision FROM edit_revision_marks  WHERE repository = ?"
	lineageQuery = "SELECT agreement_id, supersedes, constraint_text, base_revision, proposer_task_id," +
		" left_condition, right_condition FROM edit_agreements WHERE repository = ?"
	carriesQuery = "SELECT c.* FROM edit_reaffirmations c" +
		"  JOIN edit_agreements a ON a.agreement_id = c.agreement_id WHERE a.repository = ?"
	placeQuery = "SELECT a.*, r.path AS path, r.region_kind AS region_kind," +
		"       r.region_key AS region_key, r.region_class AS region_class," +
		"       r.regenerate_from AS regenerate_from" +
		"  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id" +
		" WHERE a.agreement_id = ?"
)

var (
	regionKinds          = []string{kindTree, kindFile, kindSymbol, kindData}
	keyedKinds           = []string{kindSymbol, kindData}
	regionClasses        = []string{classSource, classGenerated}
	liveStates           = []string{stateProposed, stateAgreed, stateReopened}
	dispositions         = []string{"accepted", "declined", "withdrawn", "released"}
	followupDispositions = []string{followupDone, followupDropped}
	carriedPlace         = []string{"repository", "path", "region_kind", "region_key", "region_class",
		"regenerate_from", "left_project", "right_project", "peer_link_id"}
)

// canonicalPath is editregion.canonical_path: one spelling per place, or "" when refused.
func canonicalPath(p string) (string, bool) {
	if strings.TrimSpace(p) == "" || strings.Contains(p, "\x00") || strings.Contains(p, "|") {
		return "", false
	}
	if strings.HasPrefix(p, "/") || p == "." || p == ".." || p != path.Clean(p) {
		return "", false
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return "", false
	}
	return p, true
}

// RegionID is editregion.region_id.
func RegionID(repository, baseRevision, p, kind, key string) (string, error) {
	if err := exact(repository, "a repository"); err != nil {
		return "", err
	}
	if err := exact(baseRevision, "a base revision"); err != nil {
		return "", err
	}
	return derive("rgn", repository, baseRevision, p, kind, key), nil
}

// AgreementID is editregion.agreement_id: sorted, so either side converges on one record.
func AgreementID(region, left, right string, tenure int64) string {
	low, high := sortedPair(left, right)
	return derive("agr", region, low, high, strconv.FormatInt(tenure, 10))
}

// FollowupID is editregion.followup_id.
func FollowupID(agreement, trigger string) string {
	return derive("fup", agreement, sha256Hex(trigger))
}

// MarkID is editregion.mark_id.
func MarkID(repository, from, to string) string { return derive("rvm", repository, from, to) }

func sortedPair(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

// shellQuote is shlex.quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("%+,-./:=@_", r))) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// commandLine is editregion.command_line: a command quoted so that it runs as printed.
func commandLine(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

// isWithin is scope.is_within: containment by path component.
func isWithin(root, p string) bool {
	root, p = path.Clean(root), path.Clean(p)
	if root == "/" {
		return strings.HasPrefix(p, "/")
	}
	return p == root || strings.HasPrefix(p, root+"/")
}

// place is the part of a region overlap compares.
type place struct{ regionID, repository, path, kind, key string }

// overlap is editregion.overlap: total, partial or disjoint, decided on the place.
func overlap(left, right place) string {
	if left.regionID == right.regionID {
		return overlapTotal
	}
	if left.repository != right.repository {
		return overlapDisjoint
	}
	for _, pair := range [][2]place{{left, right}, {right, left}} {
		if pair[0].kind == kindTree && isWithin(pair[0].path, pair[1].path) {
			return overlapPartial
		}
	}
	if left.path != right.path {
		return overlapDisjoint
	}
	if left.kind == kindFile || right.kind == kindFile {
		return overlapPartial
	}
	if slices.Contains(keyedKinds, left.kind) && slices.Contains(keyedKinds, right.kind) {
		if left.key == right.key {
			return overlapPartial
		}
	}
	return overlapDisjoint
}

// EditRegions is editregion.EditRegions over one store and one clock.
type EditRegions struct {
	Store *store.Store
	Now   func() string
	// ownedSide replaces _owned_side when set (tests only: the ownership-lost race).
	ownedSide func(ctx context.Context, low, high, actor string) (string, error)
	// beforeCarry runs between reaffirm's validation and propose's write (tests only: the
	// window test_edit_regions' racing() reaches by wrapping propose).
	beforeCarry func()
}

// row is a sqlite3.Row: the columns of one read, by name.
type row = store.Row

func text(r row, name string) string {
	switch v := r.Get(name).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// value is r[name] as a JSON value: None stays nil.
func value(r row, name string) any {
	switch v := r.Get(name).(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

func (e *EditRegions) one(ctx context.Context, query string, args ...any) (row, error) {
	return e.Store.One(ctx, query, args...)
}

func (e *EditRegions) all(ctx context.Context, query string, args ...any) ([]row, error) {
	return e.Store.All(ctx, query, args...)
}

func byID(rows []row) map[string]row {
	out := map[string]row{}
	for _, r := range rows {
		out[text(r, "agreement_id")] = r
	}
	return out
}

func successorMap(rows []row) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		out[text(r, "from_revision")] = text(r, "to_revision")
	}
	return out
}

// walk is EditRegions._walk: every revision reachable by successor marks, bounded.
func walk(marks map[string]string, revision string) []string {
	var reached []string
	cursor := revision
	for range len(marks) + 1 {
		next, ok := marks[cursor]
		if !ok || slices.Contains(reached, next) {
			break
		}
		cursor = next
		reached = append(reached, cursor)
	}
	return reached
}

func (e *EditRegions) chainFrom(ctx context.Context, repository, revision string) ([]string, error) {
	marks, err := e.all(ctx, marksQuery, repository)
	if err != nil {
		return nil, err
	}
	return walk(successorMap(marks), revision), nil
}

// Agreement is EditRegions.agreement: the described record, or nil.
func (e *EditRegions) Agreement(ctx context.Context, identifier string) (contract.OrderedObject, error) {
	r, err := e.one(ctx, "SELECT * FROM edit_agreements WHERE agreement_id = ?", identifier)
	if err != nil || r == nil {
		return nil, err
	}
	return e.described(ctx, agreementRecord(r), nil, nil, nil)
}

// CurrentRevision is EditRegions.current_revision: whether nothing supersedes this revision.
func (e *EditRegions) CurrentRevision(ctx context.Context, repository, revision string) (bool, error) {
	r, err := e.one(ctx, "SELECT * FROM edit_revision_marks  WHERE repository = ? AND from_revision = ?", repository, revision)
	return r == nil, err
}

// ShowFilter is EditRegions.show's keyword filters; invalid is None.
type ShowFilter struct{ BaseRevision, Project, Path sql.NullString }

// Show is EditRegions.show.
func (e *EditRegions) Show(ctx context.Context, repository string, f ShowFilter) (contract.OrderedObject, error) {
	rows, err := e.all(ctx, "SELECT a.*, r.path AS region_path, r.region_kind AS region_kind,"+
		"       r.region_key AS region_key, r.region_class AS region_class,"+
		"       r.regenerate_from AS regenerate_from"+
		"  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"+
		" WHERE a.repository = ? ORDER BY a.proposed_at, a.agreement_id", repository)
	if err != nil {
		return nil, err
	}
	marks, err := e.all(ctx, marksQuery, repository)
	if err != nil {
		return nil, err
	}
	carryRows, err := e.all(ctx, carriesQuery, repository)
	if err != nil {
		return nil, err
	}
	successors, carries, lineage := successorMap(marks), byID(carryRows), byID(rows)
	exclusive, regenerate := []any{}, []any{}
	var ids []string
	for _, r := range rows {
		if f.BaseRevision.Valid && text(r, "base_revision") != f.BaseRevision.String {
			continue
		}
		if f.Project.Valid && f.Project.String != text(r, "left_project") && f.Project.String != text(r, "right_project") {
			continue
		}
		if f.Path.Valid && text(r, "region_path") != f.Path.String {
			continue
		}
		record, err := e.described(ctx, agreementRecord(r), successors, carries, lineage)
		if err != nil {
			return nil, err
		}
		generated := text(r, "region_class") == classGenerated
		record = append(record, contract.Field{Key: "region", Value: contract.OrderedObject{
			{Key: "path", Value: value(r, "region_path")}, {Key: "regionKind", Value: value(r, "region_kind")},
			{Key: "regionKey", Value: orNone(text(r, "region_key"))}, {Key: "regionClass", Value: value(r, "region_class")},
			{Key: "regenerateFrom", Value: value(r, "regenerate_from")},
		}})
		resolution := "exclusive"
		if generated {
			resolution = "rederive"
		}
		record = append(record, contract.Field{Key: "resolution", Value: resolution})
		if generated {
			regenerate = append(regenerate, record)
		} else {
			exclusive = append(exclusive, record)
		}
	}
	for _, list := range [][]any{exclusive, regenerate} {
		for _, record := range list {
			ids = append(ids, record.(contract.OrderedObject)[0].Value.(string))
		}
	}
	followups, err := e.followupsFor(ctx, ids)
	if err != nil {
		return nil, err
	}
	conflicts, err := Conflicts(ctx, e.Store, domainEditRegion, repository)
	if err != nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "repository", Value: repository}, {Key: "baseRevision", Value: nullable(f.BaseRevision)},
		{Key: "exclusive", Value: exclusive}, {Key: "regenerate", Value: regenerate},
		{Key: "followups", Value: followups}, {Key: "conflicts", Value: conflicts},
	}, nil
}

func (e *EditRegions) followupsFor(ctx context.Context, agreements []string) (contract.OrderedObject, error) {
	accepted, unassigned, closed := []any{}, []any{}, []any{}
	for _, identifier := range agreements {
		rows, err := e.all(ctx, "SELECT * FROM edit_followups  WHERE agreement_id = ? ORDER BY recorded_at, followup_id", identifier)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			record := followupRecord(r)
			switch state := text(r, "state"); {
			case state == followupDone || state == followupDropped:
				closed = append(closed, record)
			case text(r, "assignee_task_id") != "":
				accepted = append(accepted, record)
			default:
				unassigned = append(unassigned, record)
			}
		}
	}
	return contract.OrderedObject{{Key: "accepted", Value: accepted}, {Key: "unassigned", Value: unassigned}, {Key: "closed", Value: closed}}, nil
}

func agreementRecord(r row) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "agreementId", Value: value(r, "agreement_id")}, {Key: "regionId", Value: value(r, "region_id")},
		{Key: "repository", Value: value(r, "repository")}, {Key: "baseRevision", Value: value(r, "base_revision")},
		{Key: "leftProject", Value: value(r, "left_project")}, {Key: "rightProject", Value: value(r, "right_project")},
		{Key: "peerLinkId", Value: value(r, "peer_link_id")}, {Key: "proposerTaskId", Value: value(r, "proposer_task_id")},
		{Key: "issueKey", Value: value(r, "issue_key")}, {Key: "constraintText", Value: value(r, "constraint_text")},
		{Key: "leftCondition", Value: value(r, "left_condition")}, {Key: "rightCondition", Value: value(r, "right_condition")},
		{Key: "leftAcceptedAt", Value: value(r, "left_accepted_at")},
		{Key: "rightAcceptedAt", Value: value(r, "right_accepted_at")},
		{Key: "nextOwner", Value: value(r, "next_owner")}, {Key: "state", Value: value(r, "state")}, {Key: "tenure", Value: value(r, "tenure")},
		{Key: "supersedes", Value: value(r, "supersedes")}, {Key: "supersededBy", Value: value(r, "superseded_by")},
		{Key: "closeReason", Value: value(r, "close_reason")}, {Key: "proposedAt", Value: value(r, "proposed_at")},
		{Key: "updatedAt", Value: value(r, "updated_at")}, {Key: "closedAt", Value: value(r, "closed_at")},
		{Key: "authorizes", Value: []any{}}, {Key: "grantsMergePermission", Value: false},
	}
}

func followupRecord(r row) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "followupId", Value: value(r, "followup_id")}, {Key: "agreementId", Value: value(r, "agreement_id")},
		{Key: "trigger", Value: value(r, "trigger_text")}, {Key: "acceptance", Value: value(r, "acceptance_text")},
		{Key: "issueRef", Value: value(r, "issue_ref")}, {Key: "assigneeTaskId", Value: value(r, "assignee_task_id")},
		{Key: "assigneeProject", Value: value(r, "assignee_project")}, {Key: "acceptedAt", Value: value(r, "accepted_at")},
		{Key: "state", Value: value(r, "state")}, {Key: "closeReason", Value: value(r, "close_reason")},
		{Key: "recordedBy", Value: value(r, "recorded_by")}, {Key: "recordedAt", Value: value(r, "recorded_at")},
	}
}

func get(record contract.OrderedObject, key string) any {
	for _, f := range record {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func set(record contract.OrderedObject, key string, v any) contract.OrderedObject {
	for i := range record {
		if record[i].Key == key {
			record[i].Value = v
			return record
		}
	}
	return append(record, contract.Field{Key: key, Value: v})
}

func str(v any) string { s, _ := v.(string); return s }

// described is EditRegions._described: what a record cannot say from its own columns.
func (e *EditRegions) described(ctx context.Context, record contract.OrderedObject, successors map[string]string, carries, lineage map[string]row) (contract.OrderedObject, error) {
	repository, identifier := str(get(record, "repository")), str(get(record, "agreementId"))
	if successors == nil {
		marks, err := e.all(ctx, marksQuery, repository)
		if err != nil {
			return nil, err
		}
		successors = successorMap(marks)
	}
	if carries == nil {
		r, err := e.one(ctx, "SELECT * FROM edit_reaffirmations WHERE agreement_id = ?", identifier)
		if err != nil {
			return nil, err
		}
		carries = map[string]row{}
		if r != nil {
			carries[identifier] = r
		}
	}
	carry, carried := carries[identifier]
	base := str(get(record, "baseRevision"))
	var legacy any
	var stated contract.OrderedObject
	if !carried {
		var constraint any = base
		if str(get(record, "supersedes")) != "" {
			if lineage == nil {
				lineageRows, err := e.all(ctx, lineageQuery, repository)
				if err != nil {
					return nil, err
				}
				carryRows, err := e.all(ctx, carriesQuery, repository)
				if err != nil {
					return nil, err
				}
				lineage, carries = byID(lineageRows), byID(carryRows)
			}
			constraint = writtenOn(identifier, lineage, carries)
			if origin := originOf(identifier, lineage, carries); origin != nil && text(origin, "agreement_id") != identifier {
				legacy = contract.OrderedObject{
					{Key: "origin", Value: value(origin, "agreement_id")}, {Key: "proposerTaskId", Value: value(origin, "proposer_task_id")},
					{Key: "leftCondition", Value: value(origin, "left_condition")}, {Key: "rightCondition", Value: value(origin, "right_condition")},
				}
			}
		}
		stateOf := func(key string) any {
			if get(record, key) != nil {
				return base
			}
			return nil
		}
		stated = contract.OrderedObject{{Key: "constraint", Value: constraint}, {Key: "leftCondition", Value: stateOf("leftCondition")},
			{Key: "rightCondition", Value: stateOf("rightCondition")}}
	} else {
		stated = contract.OrderedObject{{Key: "constraint", Value: value(carry, "constraint_revision")},
			{Key: "leftCondition", Value: value(carry, "left_condition_revision")},
			{Key: "rightCondition", Value: value(carry, "right_condition_revision")}}
	}
	record = set(record, "statedOn", stated)
	record = set(record, "legacyCarry", legacy)
	earlier := []any{}
	for _, f := range stated {
		if f.Value != nil && f.Value != base {
			earlier = append(earlier, f.Key)
		}
	}
	record = set(record, "textFromEarlierRevision", earlier)
	current := base
	if chain := walk(successors, base); len(chain) > 0 {
		current = chain[len(chain)-1]
	}
	record = set(record, "currentRevision", current)
	var reaffirmation any
	if carried {
		awaiting, err := e.awaiting(ctx, record, carry)
		if err != nil {
			return nil, err
		}
		var waiting any
		if awaiting != nil {
			record = set(record, "nextOwner", get(awaiting, "task"))
			waiting = awaiting
		}
		reaffirmation = contract.OrderedObject{
			{Key: "predecessor", Value: value(carry, "predecessor_id")}, {Key: "actor", Value: value(carry, "actor")},
			{Key: "actorProject", Value: value(carry, "actor_project")}, {Key: "fromRevision", Value: value(carry, "from_revision")},
			{Key: "toRevision", Value: value(carry, "to_revision")}, {Key: "recordedAt", Value: value(carry, "recorded_at")},
			{Key: "awaitingAcceptance", Value: waiting},
		}
	}
	return set(record, "reaffirmation", reaffirmation), nil
}

// awaiting is EditRegions._awaiting: the side a carried agreement still waits for.
func (e *EditRegions) awaiting(ctx context.Context, record contract.OrderedObject, carry row) (contract.OrderedObject, error) {
	if get(record, "state") != stateProposed {
		return nil, nil
	}
	for _, side := range [][3]string{{"leftAcceptedAt", "leftProject", "left_accepted_at"}, {"rightAcceptedAt", "rightProject", "right_accepted_at"}} {
		if str(get(record, side[0])) != "" {
			continue
		}
		predecessor, err := e.one(ctx, "SELECT * FROM edit_agreements WHERE agreement_id = ?", text(carry, "predecessor_id"))
		if err != nil {
			return nil, err
		}
		var prior any
		if predecessor != nil {
			prior = value(predecessor, side[2])
		}
		key := str(get(record, side[1]))
		parent, err := e.soleParent(ctx, key)
		if err != nil {
			return nil, err
		}
		identifier := str(get(record, "agreementId"))
		var task, command, precondition any
		if parent != "" {
			task = parent
			command = commandLine("region-settle", "--agreement", identifier, "--actor", parent, "--disposition", "accepted")
		} else {
			precondition = repr(key) + " has no single registered parent; the parent that takes it runs " +
				commandLine("region-settle", "--agreement", identifier) + " --actor <that task> --disposition accepted"
		}
		reason := notYetAccepted
		if prior != nil && prior != "" {
			reason = acceptanceOnPriorRevision
		}
		return contract.OrderedObject{
			{Key: "project", Value: key}, {Key: "task", Value: task}, {Key: "reason", Value: reason},
			{Key: "priorAcceptedAt", Value: prior}, {Key: "priorRevision", Value: value(carry, "from_revision")},
			{Key: "command", Value: command}, {Key: "precondition", Value: precondition},
		}, nil
	}
	return nil, nil
}

// soleParent is EditRegions._sole_parent: "" when there is not exactly one.
func (e *EditRegions) soleParent(ctx context.Context, project string) (string, error) {
	parents, err := owners(ctx, e.Store, scopeProject, project, roleParent)
	if err != nil || len(parents) != 1 {
		return "", err
	}
	return parents[0], nil
}

// writtenOn is EditRegions._written_on: the revision an agreement's constraint was written on.
func writtenOn(identifier string, lineage, carries map[string]row) any {
	cursor, ok := lineage[identifier]
	if !ok {
		return nil
	}
	for range len(lineage) + 1 {
		if carry, ok := carries[text(cursor, "agreement_id")]; ok {
			return value(carry, "constraint_revision")
		}
		before, ok := lineage[text(cursor, "supersedes")]
		if !ok || text(before, "constraint_text") != text(cursor, "constraint_text") {
			break
		}
		cursor = before
	}
	return value(cursor, "base_revision")
}

// originOf is EditRegions._origin.
func originOf(identifier string, lineage, carries map[string]row) row {
	cursor, ok := lineage[identifier]
	for range len(lineage) + 1 {
		if !ok {
			return nil
		}
		if _, carried := carries[text(cursor, "agreement_id")]; carried || text(cursor, "supersedes") == "" {
			return cursor
		}
		before, found := lineage[text(cursor, "supersedes")]
		if !found {
			return cursor
		}
		cursor = before
	}
	return cursor
}
