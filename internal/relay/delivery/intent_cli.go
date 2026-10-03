package delivery

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The eight marker commands of `codex-session-relay` (cli.py:3470-3760, parser :4960-5032):
// intent-declare/attempt/bind/register/claim/disposition/resolve/show, plus the store records
// intent-claim and intent-disposition mirror beside their marker facts (declarations.py). The
// ninth, guard-evaluate, is gone: the Stop is judged in the hook or by the owner over control.sock
// (internal/relay/hook).

// intentCommands are the marker commands, in cli.py's add_parser order. They open their own
// admitted connection (none is admitted at dispatch), and every form but intent-declare's and
// intent-register's with the selected store answers without it (_reads_no_selected_store), so
// only those two forms meet check_start and the selection refusal.
var intentCommands = []commandSpec{
	{marker("intent-declare", func(args dispatch.Args) bool { return args.Bool("no-db-path") }), cmdIntentDeclare},
	{marker("intent-attempt", always), cmdIntentAttempt},
	{marker("intent-bind", always), cmdIntentBind},
	{marker("intent-register", func(args dispatch.Args) bool { return args.Text("db-path") != "" }), cmdIntentRegister},
	{marker("intent-claim", always), cmdIntentClaim},
	{marker("intent-disposition", always), cmdIntentDisposition},
	{marker("intent-resolve", always), cmdIntentResolve},
	{marker("intent-show", always), cmdIntentShow},
}

func marker(name string, selectsNoStore func(dispatch.Args) bool) dispatch.Command {
	return dispatch.Command{Name: name, OwnAdmission: true, ReadOnly: name == "intent-show", SelectsNoStore: selectsNoStore}
}

func always(dispatch.Args) bool { return true }

// markerRoot is _marker_root: resolve_marker_root(args.marker_root).path.
func (c *cliRun) markerRoot() (string, error) {
	selection, err := ResolveMarkerRoot(c.s("--marker-root"))
	return selection.Path, err
}

func (c *cliRun) dbPath() string { return c.state + "/relay.sqlite3" }

func cmdIntentDeclare(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	declaredAt := c.s("--declared-at")
	if declaredAt == "" {
		declaredAt = c.clock.ISO()
	}
	var settings any
	if raw := c.s("--settings"); raw != "" {
		if settings, err = loads(raw); err != nil {
			return nil, dispatch.Host("--settings is not JSON: " + err.Error())
		}
	}
	var db any
	if c.args["--no-db-path"] != true {
		db = c.dbPath()
	}
	return DeclareIntent(c.ctx, root, IntentDeclaration{Workspace: c.s("--workspace"), DispatchRequestID: c.s("--dispatch-request-id"), IssueKey: c.s("--issue"), DeclaredAt: declaredAt,
		CriteriaSource: c.opt("--criteria-source"), BaselineRevision: c.opt("--baseline-revision"), AuthorizedSettings: settings, DBPath: db})
}

func cmdIntentAttempt(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	return RecordAttempt(c.ctx, root, c.s("--workspace"), c.s("--assignment"), c.s("--outcome"), c.clock.ISO(), c.opt("--task-id"))
}

func cmdIntentBind(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	return BindIdentity(c.ctx, root, c.s("--workspace"), c.s("--assignment"), c.s("--session"), c.s("--task-id"), c.clock.ISO())
}

func cmdIntentRegister(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	db := c.s("--db-path")
	if db == "" {
		db = c.dbPath()
	}
	return RegisterRelationship(c.ctx, root, c.s("--workspace"), c.s("--assignment"), c.s("--relationship"), c.s("--dispatch-request-id"), c.clock.ISO(), db)
}

// adjudicated is _adjudicated: every --adjudicate factId=digest, or a usage error.
func adjudicated(values []string) ([]Obj, error) {
	entries := []Obj{}
	for _, value := range values {
		factID, digest, _ := strings.Cut(value, "=")
		if factID == "" || digest == "" {
			return nil, &dispatch.UsageError{Detail: "--adjudicate takes factId=digest, not " + strconv.Quote(value), Code: contract.ExitUsage}
		}
		entries = append(entries, Obj{{Key: "factId", Value: factID}, {Key: "digest", Value: digest}})
	}
	return entries, nil
}

func cmdIntentResolve(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	entries, err := adjudicated(c.list("--adjudicate"))
	if err != nil {
		return nil, err
	}
	return PublishResolution(c.ctx, root, c.s("--workspace"), c.s("--assignment"), c.s("--chosen-task"), c.s("--chosen-session"), c.s("--reason"), c.clock.ISO(), entries)
}

// orEmptyList is `value or []`.
func orEmptyList(v any) any {
	if !truthy(v) {
		return []any{}
	}
	return v
}

func stringsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func cmdIntentShow(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	workspace := c.s("--workspace")
	var directory string
	var facts Obj
	var unreadable []string
	if assignment := c.s("--assignment"); assignment != "" {
		if directory, err = AssignmentDir(root, workspace, assignment); err != nil {
			return nil, err
		}
		facts, unreadable = ReadAssignment(c.ctx, directory)
		if _, has := get(facts, "intent"); len(unreadable) == 0 && !has {
			return Obj{{Key: "markerRoot", Value: root}, {Key: "workspace", Value: workspace}, {Key: "managed", Value: false}, {Key: "assignmentId", Value: filepath.Base(directory)},
				{Key: "assignmentDir", Value: directory}, {Key: "unreadable", Value: []any{}}, {Key: "detail", Value: "no intent is published for this assignment"}}, nil
		}
	} else {
		if directory, facts, unreadable, err = SelectAssignment(c.ctx, root, workspace, c.opt("--session")); err != nil {
			return nil, err
		}
		if directory == "" {
			return Obj{{Key: "markerRoot", Value: root}, {Key: "workspace", Value: workspace}, {Key: "managed", Value: false}, {Key: "unreadable", Value: stringsAny(unreadable)}}, nil
		}
	}
	var malformed any
	if len(facts) > 0 {
		if m := Malformed(facts); m != "" {
			malformed = m
		}
	}
	payload := Obj{{Key: "markerRoot", Value: root}, {Key: "workspace", Value: workspace}, {Key: "managed", Value: true}, {Key: "assignmentId", Value: filepath.Base(directory)},
		{Key: "assignmentDir", Value: directory}, {Key: "unreadable", Value: stringsAny(unreadable)}, {Key: "malformed", Value: malformed}}
	if malformed != nil || len(unreadable) > 0 {
		return payload, nil
	}
	now := c.opt("--now")
	if !truthy(now) {
		now = c.clock.ISO()
	}
	return append(payload,
		F{Key: "assignmentState", Value: DeriveAssignmentState(facts, now)},
		F{Key: "identityContested", Value: IdentityContested(facts)},
		F{Key: "intent", Value: fieldOf(facts, "intent")},
		F{Key: "bound", Value: fieldOf(facts, "bound")},
		F{Key: "relationship", Value: fieldOf(facts, "relationship")},
		F{Key: "attempts", Value: orEmptyList(fieldOf(facts, "attempts"))},
		F{Key: "claims", Value: orEmptyList(fieldOf(facts, "claims"))},
		F{Key: "conflicts", Value: orEmptyList(fieldOf(facts, "conflicts"))},
		F{Key: "resolutions", Value: orEmptyList(fieldOf(facts, "resolutions"))}), nil
}

func cmdIntentClaim(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	workspace, assignment, dispatch := c.s("--workspace"), c.s("--assignment"), c.s("--dispatch-request-id")
	// cmd_intent_claim: the store the intent names fences this claim before its marker is
	// published (check_start), so a store another runtime owns refuses with nothing written. A
	// marker this cannot read names no store here.
	var before Obj
	if directory, err := AssignmentDir(root, workspace, assignment); err == nil {
		before, _ = ReadAssignment(c.ctx, directory)
	}
	if target := storeOf(before); target != nil {
		path, err := expandedStore(target)
		if err != nil {
			return nil, err
		}
		if err := intentStoreFence(c.ctx, path); err != nil {
			return nil, err
		}
	}
	published, err := PublishClaim(c.ctx, root, workspace, assignment, c.s("--session"), dispatch, c.opt("--first-turn"), c.clock.ISO())
	if err != nil {
		return nil, err
	}
	directory, err := AssignmentDir(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	facts, unreadable := ReadAssignment(c.ctx, directory)
	session := fieldOf(published, "sessionId")
	var standing Obj
	claims, _ := fieldOf(facts, "claims").([]any)
	for _, item := range claims {
		if claim, ok := item.(Obj); ok && Claimant(claim) == session {
			standing = claim
			break
		}
	}
	intentFact, _ := fieldOf(facts, "intent").(Obj)
	var record Obj
	switch {
	case len(unreadable) > 0:
		sorted := slices.Clone(unreadable)
		slices.Sort(sorted)
		record = declNotRecorded("marker_unreadable", "the marker could not be read whole after the claim ("+strings.Join(sorted, ", ")+"), so this session records nothing about how it reports; running the claim again once the marker reads records it", nil)
	case Malformed(facts) != "":
		record = declNotRecorded("marker_malformed", "the marker's "+Malformed(facts)+" is not the shape a fact must be, so this session records nothing about how it reports", nil)
	case fieldOf(published, "outcome") == Conflict || standing == nil || !pyEqual(fieldOf(standing, "dispatchRequestId"), dispatch):
		record = declNotRecorded("claim_not_standing", "the marker does not stand on this claim, so this session records nothing about how it reports", nil)
	default:
		declared, _ := fieldOf(intentFact, "workspace").(string)
		same := false
		if Correlated(facts, session, assignment) && declared != "" {
			left, errLeft := resolved(declared)
			right, errRight := resolved(workspace)
			if errLeft != nil || errRight != nil {
				return nil, errors.Join(errLeft, errRight)
			}
			same = left == right
		}
		if !same {
			record = declNotRecorded("claim_uncorrelated", "the claim does not correlate with the intent declared for this workspace, so the store derives nothing for this session", nil)
			break
		}
		markerRoot, err := resolved(root)
		if err != nil {
			return nil, err
		}
		workspaceResolved, err := resolved(workspace)
		if err != nil {
			return nil, err
		}
		record, err = recordClaim(c.ctx, storeOf(facts), store.ReportingSessionsRow{AssignmentID: assignment, SessionID: pyStr(session), DispatchRequestID: dispatch,
			MarkerRoot: markerRoot, Workspace: workspaceResolved, IssueKey: nullString(fieldOf(intentFact, "issueKey")), Capability: declarationsCapability, RecordedAt: c.clock.ISO()})
		if err != nil {
			return nil, err
		}
	}
	return withStoreRecord(published, record)
}

func cmdIntentDisposition(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	workspace, assignment := c.s("--workspace"), c.s("--assignment")
	var before Obj
	var unreadableBefore []string
	directory, err := AssignmentDir(root, workspace, assignment)
	if err == nil {
		before, unreadableBefore = ReadAssignment(c.ctx, directory)
	} else {
		directory = ""
	}
	var published, record Obj
	body := func(ctx context.Context, held *store.Store, heldPath string, problem Obj) error {
		var err error
		published, err = PublishDisposition(c.ctx, root, workspace, assignment, c.s("--session"), c.s("--turn"), c.s("--outcome"), c.clock.ISO())
		if err != nil {
			return err
		}
		if directory == "" {
			if directory, err = AssignmentDir(root, workspace, assignment); err != nil {
				return err
			}
		}
		facts, unreadable := ReadAssignment(c.ctx, directory)
		standingValue, readable := ReadDisposition(c.ctx, directory, fieldOf(published, "sessionId"), fieldOf(published, "turnId"))
		standing, _ := standingValue.(Obj)
		switch {
		case slices.Contains(unreadable, "intent") || slices.Contains(unreadableBefore, "intent") || !readable || !truthy(standingValue) || malformedDisposition(standingValue) != "":
			record = declFailure("marker_unreadable", "the intent or the disposition could not be read back from the marker after it was published, so what the marker stands on is unknown", nil)
		case !pyEqual(storeOf(facts), storeOf(before)):
			record = declFailure("store_changed", "the intent named another store while this was being recorded, so the record was not written to either", nil)
		case held == nil:
			record = problem
		default:
			declaredAt := fieldOf(standing, "at")
			if !truthy(declaredAt) {
				declaredAt = ""
			}
			record = recordDisposition(ctx, held, heldPath, store.TurnDeclarationsRow{AssignmentID: assignment, SessionID: pyStr(fieldOf(published, "sessionId")), TurnID: pyStr(fieldOf(published, "turnId")),
				Outcome: pyStr(fieldOf(standing, "outcome")), DeclaredAt: pyStr(declaredAt), RecordedAt: c.clock.ISO()})
		}
		return nil
	}
	// declarations.Held: the store is opened before anything is published, so an ownership
	// refusal (or an unexpandable path) is raised with nothing written.
	held, heldPath, problem, err := openDeclarationStore(c.ctx, storeOf(before))
	if err != nil {
		return nil, err
	}
	if held == nil {
		if err := body(c.ctx, nil, "", problem); err != nil {
			return nil, err
		}
		return withStoreRecord(published, record)
	}
	defer func() { _ = held.Close() }()
	began := false
	var bodyErr error
	txErr := held.Transaction(c.ctx, func(ctx context.Context, _ *sql.Conn) error {
		began = true
		bodyErr = body(ctx, held, heldPath, nil)
		return bodyErr
	})
	switch {
	case !began:
		// Held: a store that cannot be locked is only the answer, and the publication goes ahead.
		if err := body(c.ctx, nil, "", declFailure("store_locked", store.StoredSQLiteError(txErr), heldPath)); err != nil {
			return nil, err
		}
	case bodyErr != nil:
		return nil, bodyErr
	case txErr != nil && str(record, "state") == declRecorded:
		// Held.settled: a commit that failed undoes the record.
		record = declFailure("store_write_failed", store.StoredSQLiteError(txErr), fieldOf(record, "store"))
	}
	return withStoreRecord(published, record)
}

// malformedDisposition is intent.malformed_disposition.
func malformedDisposition(record any) string {
	if record == nil {
		return ""
	}
	o, ok := record.(Obj)
	if !ok {
		return "disposition"
	}
	for _, field := range []string{"sessionId", "turnId", "outcome"} {
		if v, present := get(o, field); present {
			if _, isString := v.(string); !isString {
				return "disposition." + field
			}
		}
	}
	return ""
}

// withStoreRecord is _with_store_record: the marker answer with the store record beside it; a
// failed record fails the command with the whole answer.
func withStoreRecord(published, record Obj) (any, error) {
	payload := append(slices.Clone(published), F{Key: "storeRecord", Value: record})
	if str(record, "state") == declFailed {
		return nil, &dispatch.PayloadExit{Payload: append(payload, F{Key: "detail", Value: "the marker fact was published and the relay store record was not: " + pyStr(fieldOf(record, "detail"))}), Code: contract.ExitRefused}
	}
	return payload, nil
}

// ---------------------------------------------------------------- declarations.py

const (
	declarationsCapability = "declarations/1"
	declRecorded           = "recorded"
	declNotRecordedState   = "not_recorded"
	declFailed             = "failed"
)

// storeOf is declarations.store_of: the store the coordinator recorded, or nil.
func storeOf(facts Obj) any {
	declared, ok := fieldOf(facts, "intent").(Obj)
	if !ok {
		return nil
	}
	path, ok := fieldOf(declared, "dbPath").(string)
	if !ok || pyvalue.Strip(path) == "" {
		return nil
	}
	return path
}

func declNotRecorded(reason, detail string, dbPath any) Obj {
	return Obj{{Key: "recorded", Value: false}, {Key: "state", Value: declNotRecordedState}, {Key: "reason", Value: reason}, {Key: "store", Value: dbPath}, {Key: "detail", Value: detail}}
}

func declFailure(reason, detail string, dbPath any) Obj {
	return Obj{{Key: "recorded", Value: false}, {Key: "state", Value: declFailed}, {Key: "reason", Value: reason}, {Key: "store", Value: dbPath},
		{Key: "detail", Value: detail + ". The marker fact stands; running the same command again retries this record and changes nothing else"}}
}

// pathlibString is str(Path(value)): repeated and trailing separators and "." parts dropped,
// ".." kept.
func pathlibString(value string) string {
	if value == "" {
		return "."
	}
	parts := []string{}
	for _, part := range strings.Split(value, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	joined := strings.Join(parts, "/")
	if strings.HasPrefix(value, "/") {
		return "/" + joined
	}
	if joined == "" {
		return "."
	}
	return joined
}

// expandedStore is the store an intent names with its ~ expanded; an unknown ~user is a host
// error out of the command.
func expandedStore(dbPath any) (string, error) {
	expanded, err := store.ExpandUser(dbPath.(string))
	if errors.Is(err, store.ErrNoHome) {
		return "", dispatch.Host("the relay store the intent names cannot be resolved: " + dispatch.Detail(err))
	}
	if err != nil {
		return "", err
	}
	return pathlibString(expanded), nil
}

// intentStoreFence is the ownership check the fence makes on the store an intent names before
// a claim or a disposition is published (cmd_intent_claim's check_start, declarations.Held's
// Store()): only an ownership refusal refuses. An OS or SQLite failure refuses nothing here; it
// is the record the command answers with after its marker write.
func intentStoreFence(ctx context.Context, path string) error {
	err := store.CheckStartLikeFence(ctx, path)
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused
	}
	return nil
}

// openDeclarationStore is declarations._open: the store to record in and its path, or nil and
// the answer saying why there is none. What Store() raises rather than answers - an ownership
// refusal, like the RuntimeError of an unexpandable path - is the error.
func openDeclarationStore(ctx context.Context, dbPath any) (*store.Store, string, Obj, error) {
	if dbPath == nil {
		return nil, "", declNotRecorded("no_store_recorded", "the assignment's intent names no relay store, so there is none to record this in; the relay derives nothing from a store for this assignment", nil), nil
	}
	path, err := expandedStore(dbPath)
	if err != nil {
		return nil, "", nil, err
	}
	metadata, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, path, declNotRecorded("store_absent", "the relay store the intent names does not exist, so nothing can be derived from it either; nothing was created", path), nil
	case err != nil:
		return nil, path, declFailure("store_unreadable", store.StoredOSError(err), path), nil
	case !metadata.Mode().IsRegular():
		return nil, path, declFailure("store_not_a_file", "the path the intent names is not a regular file", path), nil
	}
	if err := intentStoreFence(ctx, path); err != nil {
		return nil, path, nil, err
	}
	s, err := store.Open(ctx, path, "")
	if err != nil {
		return nil, path, declFailure("store_unopenable", store.StoredSQLiteError(err), path), nil
	}
	return s, path, nil, nil
}

func declRecordedAnswer(path string) Obj {
	return Obj{{Key: "recorded", Value: true}, {Key: "state", Value: declRecorded}, {Key: "reason", Value: nil}, {Key: "store", Value: path}, {Key: "detail", Value: nil}}
}

func declUnchangedAnswer(path string) Obj {
	return Obj{{Key: "recorded", Value: false}, {Key: "state", Value: Unchanged}, {Key: "reason", Value: nil}, {Key: "store", Value: path}, {Key: "detail", Value: "already recorded, identically"}}
}

func declConflictAnswer(path string, existing Obj) Obj {
	return Obj{{Key: "recorded", Value: false}, {Key: "state", Value: Conflict}, {Key: "reason", Value: "store_disagrees"}, {Key: "store", Value: path},
		{Key: "detail", Value: "this store already holds a different record for it, and the first one stands, as it does in the marker: " + pyReprValue(existing)}}
}

func nullString(v any) sql.NullString {
	s, ok := v.(string)
	return sql.NullString{String: s, Valid: ok}
}

func nullValue(n sql.NullString) any {
	if !n.Valid {
		return nil
	}
	return n.String
}

// recordClaim is declarations.record_claim: create-once in a write of its own.
func recordClaim(ctx context.Context, dbPath any, row store.ReportingSessionsRow) (Obj, error) {
	s, path, problem, err := openDeclarationStore(ctx, dbPath)
	if s == nil {
		return problem, err
	}
	defer func() { _ = s.Close() }()
	var answer Obj
	err = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		existing, err := s.ReportingSession(ctx, row.AssignmentID, row.SessionID)
		if errors.Is(err, sql.ErrNoRows) {
			if err := s.RecordReportingSession(ctx, row); err != nil {
				return err
			}
			answer = declRecordedAnswer(path)
			return nil
		}
		if err != nil {
			return err
		}
		if existing.DispatchRequestID == row.DispatchRequestID && existing.MarkerRoot == row.MarkerRoot && existing.Workspace == row.Workspace && existing.IssueKey == row.IssueKey && existing.Capability == row.Capability {
			answer = declUnchangedAnswer(path)
			return nil
		}
		answer = declConflictAnswer(path, Obj{{Key: "dispatch_request_id", Value: existing.DispatchRequestID}, {Key: "marker_root", Value: existing.MarkerRoot},
			{Key: "workspace", Value: existing.Workspace}, {Key: "issue_key", Value: nullValue(existing.IssueKey)}, {Key: "capability", Value: existing.Capability}})
		return nil
	})
	if err != nil {
		return declFailure("store_write_failed", store.StoredSQLiteError(err), path), nil
	}
	return answer, nil
}

// recordDisposition is Held.disposition: the declared outcome, create-once, on the held write.
func recordDisposition(ctx context.Context, held *store.Store, path string, row store.TurnDeclarationsRow) Obj {
	existing, err := held.TurnDeclaration(ctx, row.AssignmentID, row.SessionID, row.TurnID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := held.RecordTurnDeclaration(ctx, row); err != nil {
			return declFailure("store_write_failed", store.StoredSQLiteError(err), path)
		}
		return declRecordedAnswer(path)
	case err != nil:
		return declFailure("store_write_failed", store.StoredSQLiteError(err), path)
	case existing.Outcome == row.Outcome:
		return declUnchangedAnswer(path)
	}
	return declConflictAnswer(path, Obj{{Key: "outcome", Value: existing.Outcome}})
}
