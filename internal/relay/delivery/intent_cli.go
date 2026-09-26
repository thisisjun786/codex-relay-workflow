package delivery

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The eight marker commands of `codex-session-relay` (cli.py:3470-3760, parser :4960-5032):
// intent-declare/attempt/bind/register/claim/disposition/resolve/show, plus the store records
// intent-claim and intent-disposition mirror beside their marker facts (declarations.py).
// guard-evaluate, the ninth marker command, is todo 33's.

func markerFlags(extra ...flagSpec) []flagSpec {
	return append([]flagSpec{{"--marker-root", "store", false, nil, nil}, {"--workspace", "store", true, nil, nil}}, extra...)
}

var intentCommands = map[string]commandSpec{
	"intent-declare": {flags: markerFlags(flagSpec{"--dispatch-request-id", "store", true, nil, nil}, flagSpec{"--issue", "store", true, nil, nil}, flagSpec{"--declared-at", "store", false, nil, nil},
		flagSpec{"--criteria-source", "store", false, nil, nil}, flagSpec{"--baseline-revision", "store", false, nil, nil}, flagSpec{"--settings", "store", false, nil, nil}, flagSpec{"--no-db-path", "true", false, nil, false}),
		run: cmdIntentDeclare, exempt: func(a map[string]any) bool { return a["--no-db-path"] == true }},
	"intent-attempt": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--outcome", "store", true, AttemptOutcomes, nil}, flagSpec{"--task-id", "store", false, nil, nil}),
		run: cmdIntentAttempt, exempt: always},
	"intent-bind": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--session", "store", true, nil, nil}, flagSpec{"--task-id", "store", true, nil, nil}),
		run: cmdIntentBind, exempt: always},
	"intent-register": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--relationship", "store", true, nil, nil}, flagSpec{"--dispatch-request-id", "store", true, nil, nil}, flagSpec{"--db-path", "store", false, nil, nil}),
		run: cmdIntentRegister, exempt: func(a map[string]any) bool { return truthy(a["--db-path"]) }},
	"intent-claim": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--session", "store", true, nil, nil}, flagSpec{"--dispatch-request-id", "store", true, nil, nil}, flagSpec{"--first-turn", "store", false, nil, nil}),
		run: cmdIntentClaim, exempt: always},
	"intent-disposition": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--session", "store", true, nil, nil}, flagSpec{"--turn", "store", true, nil, nil}, flagSpec{"--outcome", "store", true, DispositionOutcomes, nil}),
		run: cmdIntentDisposition, exempt: always},
	"intent-resolve": {flags: markerFlags(flagSpec{"--assignment", "store", true, nil, nil}, flagSpec{"--chosen-task", "store", true, nil, nil}, flagSpec{"--chosen-session", "store", true, nil, nil}, flagSpec{"--reason", "store", true, nil, nil}, flagSpec{"--adjudicate", "append", true, nil, nil}),
		run: cmdIntentResolve, exempt: always},
	"intent-show": {flags: markerFlags(flagSpec{"--assignment", "store", false, nil, nil}, flagSpec{"--session", "store", false, nil, nil}, flagSpec{"--now", "store", false, nil, nil}),
		run: cmdIntentShow, exempt: always},
}

// intentCommandNames is cli.py's add_parser order for the marker commands this package serves.
var intentCommandNames = []string{"intent-declare", "intent-attempt", "intent-bind", "intent-register", "intent-claim", "intent-disposition", "intent-resolve", "intent-show"}

func init() {
	for name, spec := range intentCommands {
		deliveryCommands[name] = spec
	}
}

func always(map[string]any) bool { return true }

// payloadExit is cli.PayloadExit: a whole answer printed with its own exit code.
type payloadExit struct {
	payload Obj
	code    int
}

func (p *payloadExit) Error() string                              { return pyStr(fieldOf(p.payload, "detail")) }
func (p *payloadExit) ExitPayload() (contract.OrderedObject, int) { return p.payload, p.code }

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
			return nil, &hostError{"JSONDecodeError", err.Error()}
		}
	}
	var db any
	if c.args["--no-db-path"] != true {
		db = c.dbPath()
	}
	return DeclareIntent(root, IntentDeclaration{Workspace: c.s("--workspace"), DispatchRequestID: c.s("--dispatch-request-id"), IssueKey: c.s("--issue"), DeclaredAt: declaredAt,
		CriteriaSource: c.opt("--criteria-source"), BaselineRevision: c.opt("--baseline-revision"), AuthorizedSettings: settings, DBPath: db})
}

func cmdIntentAttempt(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	return RecordAttempt(root, c.s("--workspace"), c.s("--assignment"), c.s("--outcome"), c.clock.ISO(), c.opt("--task-id"))
}

func cmdIntentBind(c *cliRun) (any, error) {
	root, err := c.markerRoot()
	if err != nil {
		return nil, err
	}
	return BindIdentity(root, c.s("--workspace"), c.s("--assignment"), c.s("--session"), c.s("--task-id"), c.clock.ISO())
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
			return nil, &usageError{"--adjudicate takes factId=digest, not " + store.PyRepr(value), contract.ExitUsage}
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
	return PublishResolution(root, c.s("--workspace"), c.s("--assignment"), c.s("--chosen-task"), c.s("--chosen-session"), c.s("--reason"), c.clock.ISO(), entries)
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
		facts, unreadable = ReadAssignment(directory)
		if _, has := get(facts, "intent"); len(unreadable) == 0 && !has {
			return Obj{{Key: "markerRoot", Value: root}, {Key: "workspace", Value: workspace}, {Key: "managed", Value: false}, {Key: "assignmentId", Value: filepath.Base(directory)},
				{Key: "assignmentDir", Value: directory}, {Key: "unreadable", Value: []any{}}, {Key: "detail", Value: "no intent is published for this assignment"}}, nil
		}
	} else {
		if directory, facts, unreadable, err = SelectAssignment(root, workspace, c.opt("--session")); err != nil {
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
	published, err := PublishClaim(root, workspace, assignment, c.s("--session"), dispatch, c.opt("--first-turn"), c.clock.ISO())
	if err != nil {
		return nil, err
	}
	directory, err := AssignmentDir(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	facts, unreadable := ReadAssignment(directory)
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
		record = recordClaim(c.ctx, storeOf(facts), store.ReportingSessionsRow{AssignmentID: assignment, SessionID: pyStr(session), DispatchRequestID: dispatch,
			MarkerRoot: markerRoot, Workspace: workspaceResolved, IssueKey: nullString(fieldOf(intentFact, "issueKey")), Capability: declarationsCapability, RecordedAt: c.clock.ISO()})
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
		before, unreadableBefore = ReadAssignment(directory)
	} else {
		directory = ""
	}
	var published, record Obj
	body := func(ctx context.Context, held *store.Store, heldPath string, problem Obj) error {
		var err error
		published, err = PublishDisposition(root, workspace, assignment, c.s("--session"), c.s("--turn"), c.s("--outcome"), c.clock.ISO())
		if err != nil {
			return err
		}
		if directory == "" {
			if directory, err = AssignmentDir(root, workspace, assignment); err != nil {
				return err
			}
		}
		facts, unreadable := ReadAssignment(directory)
		standingValue, readable := ReadDisposition(directory, fieldOf(published, "sessionId"), fieldOf(published, "turnId"))
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
	held, heldPath, problem := openDeclarationStore(c.ctx, storeOf(before))
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
		if err := body(c.ctx, nil, "", declFailure("store_locked", store.PythonSQLiteError(txErr), heldPath)); err != nil {
			return nil, err
		}
	case bodyErr != nil:
		return nil, bodyErr
	case txErr != nil && str(record, "state") == declRecorded:
		// Held.settled: a commit that failed undoes the record.
		record = declFailure("store_write_failed", store.PythonSQLiteError(txErr), fieldOf(record, "store"))
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
		return nil, &payloadExit{append(payload, F{Key: "detail", Value: "the marker fact was published and the relay store record was not: " + pyStr(fieldOf(record, "detail"))}), contract.ExitRefused}
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
	if !ok || strings.TrimSpace(path) == "" {
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

// openDeclarationStore is declarations._open: the store to record in and its path, or nil and
// the answer saying why there is none.
func openDeclarationStore(ctx context.Context, dbPath any) (*store.Store, string, Obj) {
	if dbPath == nil {
		return nil, "", declNotRecorded("no_store_recorded", "the assignment's intent names no relay store, so there is none to record this in; the relay derives nothing from a store for this assignment", nil)
	}
	expanded, err := store.ExpandUser(dbPath.(string))
	if err != nil {
		return nil, "", declFailure("store_unreadable", store.PythonOSError(err), dbPath)
	}
	path := pathlibString(expanded)
	metadata, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, path, declNotRecorded("store_absent", "the relay store the intent names does not exist, so nothing can be derived from it either; nothing was created", path)
	case err != nil:
		return nil, path, declFailure("store_unreadable", store.PythonOSError(err), path)
	case !metadata.Mode().IsRegular():
		return nil, path, declFailure("store_not_a_file", "the path the intent names is not a regular file", path)
	}
	s, err := store.Open(ctx, path, "")
	if err != nil {
		return nil, path, declFailure("store_unopenable", store.PythonSQLiteError(err), path)
	}
	return s, path, nil
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
func recordClaim(ctx context.Context, dbPath any, row store.ReportingSessionsRow) Obj {
	s, path, problem := openDeclarationStore(ctx, dbPath)
	if s == nil {
		return problem
	}
	defer func() { _ = s.Close() }()
	var answer Obj
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
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
		return declFailure("store_write_failed", store.PythonSQLiteError(err), path)
	}
	return answer
}

// recordDisposition is Held.disposition: the declared outcome, create-once, on the held write.
func recordDisposition(ctx context.Context, held *store.Store, path string, row store.TurnDeclarationsRow) Obj {
	existing, err := held.TurnDeclaration(ctx, row.AssignmentID, row.SessionID, row.TurnID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := held.RecordTurnDeclaration(ctx, row); err != nil {
			return declFailure("store_write_failed", store.PythonSQLiteError(err), path)
		}
		return declRecordedAnswer(path)
	case err != nil:
		return declFailure("store_write_failed", store.PythonSQLiteError(err), path)
	case existing.Outcome == row.Outcome:
		return declUnchangedAnswer(path)
	}
	return declConflictAnswer(path, Obj{{Key: "outcome", Value: existing.Outcome}})
}
