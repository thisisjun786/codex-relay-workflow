// Package journal persists the flow engine's journal in the relay store's flow zone (flow_runs, flow_calls, flow_events;
// R04). It is the engine.Journal the engine writes to in a run that must survive its process: the same events in the same
// order as the in-memory journal, with the sequence numbers the engine reads.
//
// Every dispatch intent (prepared, thread_requested, fork_requested, turn_requested, interrupt_requested, and a session call's
// step_published) is committed in a transaction of its own before Append returns, and so before the request it announces is sent. Every other row (a thread
// or turn that started, a terminal row, a flow row) is written by this one process in a batch transaction: the rows wait in
// memory until the next intent, the end of a call, the end of the run or Flush, and are then committed together in order. A
// crash loses only result rows that a later intent would have followed, and the reconcile pass finds those on the host from
// the intent that was committed.
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Identity is what the run record (flow_runs) holds besides what the engine knows: the plan the run pinned, the actor and
// the coordinator epoch it holds, and the run it resumes.
type Identity struct {
	PlanID       string
	PlanRevision int64
	// ActorTaskID is the task that runs the flow (required to start a run).
	ActorTaskID string
	EpochNonce  string
	ResumeFrom  string
}

// Options configure a journal.
type Options struct {
	// Now is the clock of the rows' recorded_at; nil is time.Now.
	Now func() time.Time
	// Immediate commits every row as it is appended, instead of batching the rows that are not intents.
	Immediate bool
}

// Journal is the engine.Journal over the store.
type Journal struct {
	st    *store.Store
	runID string
	id    Identity
	opts  Options
	ctx   context.Context

	mu      sync.Mutex
	events  []engine.Event
	seq     int
	batch   []stored
	err     error
	hasRun  bool
	context map[string]callContext
	// nodeExecution is the execution the latest call of a node was associated with: a call of the node that carries none of its
	// own (one a resumed process reaches for the first time) belongs to the same execution.
	nodeExecution map[string]string
	// loading is set while the rows the store already holds are read: they inherit nothing that was not recorded with them.
	loading bool
	// onCommit is told, outside mu, after a batch was committed (engine.CommitNotifier); committed is set under mu by the commit.
	onCommit  func()
	committed bool
}

// stored is a row that waits for its transaction, with what it inherits from the earlier rows of its call as of the moment it
// was appended (a later row of the call must not change it).
type stored struct {
	e  engine.Event
	cc callContext
}

var _ engine.FailingJournal = (*Journal)(nil)

// callContext is what the rows of a call inherit from its earlier rows: the columns the relay reads without opening
// detail_json (the role, the node, the head a reviewer reads, the execution, and the thread and last turn a terminal row
// names).
type callContext struct {
	role, node, head, execution, thread, turn string
}

// Open returns the journal of run runID in the store, with the rows the run already holds. ctx bounds the opening only;
// rows are written under a context that ctx does not cancel, because the terminal rows of a run that was aborted still
// have to be stored.
func Open(ctx context.Context, st *store.Store, runID string, id Identity, opts Options) (*Journal, error) {
	if st == nil || runID == "" {
		return nil, errors.New("journal: a store and a run id are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	j := &Journal{st: st, runID: runID, id: id, opts: opts, ctx: context.WithoutCancel(ctx), context: map[string]callContext{}, nodeExecution: map[string]string{}}
	events, hasRun, err := Load(ctx, st, runID)
	if err != nil {
		return nil, err
	}
	j.hasRun = hasRun
	if hasRun {
		if err := j.loadIdentity(ctx); err != nil {
			return nil, err
		}
		if err := j.loadNodeExecutions(ctx); err != nil {
			return nil, err
		}
	}
	j.loading = true
	for _, e := range events {
		j.noteContext(e)
		// the rows a command wrote into the run (engine.ForeignSeqBase) are not the run's own numbers
		if int64(e.Seq) < engine.ForeignSeqBase {
			j.seq = max(j.seq, e.Seq)
		}
	}
	j.loading = false
	j.events = events
	return j, nil
}

// loadIdentity fills what the caller left out of the Identity from the run record the first process wrote: a run that is opened
// again (a resume, a recovery) keeps the plan its rows are associated with, so the rows it appends are found by the same
// plan and node as the earlier ones.
func (j *Journal) loadIdentity(ctx context.Context) error {
	var plan, actor, epoch, from string
	var revision int64
	err := j.st.DB.QueryRowContext(ctx, `SELECT COALESCE(plan_id, ''), COALESCE(plan_revision, 0), COALESCE(actor_task_id, ''), COALESCE(epoch_nonce, ''), COALESCE(resume_from, '') FROM flow_runs WHERE run_id = ?`,
		j.runID).Scan(&plan, &revision, &actor, &epoch, &from)
	if err != nil {
		return fmt.Errorf("journal: the run record: %w", err)
	}
	j.id.PlanID = orElse(j.id.PlanID, plan)
	if j.id.PlanRevision == 0 {
		j.id.PlanRevision = revision
	}
	j.id.ActorTaskID = orElse(j.id.ActorTaskID, actor)
	j.id.EpochNonce = orElse(j.id.EpochNonce, epoch)
	j.id.ResumeFrom = orElse(j.id.ResumeFrom, from)
	return nil
}

// loadNodeExecutions reads the execution each node of the run was last associated with, from the rows already stored.
func (j *Journal) loadNodeExecutions(ctx context.Context) error {
	rows, err := j.st.DB.QueryContext(ctx, `SELECT node_id, execution_id FROM flow_calls WHERE run_id = ? AND COALESCE(node_id, '') != '' AND COALESCE(execution_id, '') != '' ORDER BY event_seq`, j.runID)
	if err != nil {
		return fmt.Errorf("journal: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var node, execution string
		if err := rows.Scan(&node, &execution); err != nil {
			return fmt.Errorf("journal: %w", err)
		}
		j.nodeExecution[node] = execution
	}
	return rows.Err()
}

// intents are the events committed before the request they announce is sent.
var intents = map[string]bool{
	engine.EvPrepared: true, engine.EvThreadRequested: true, engine.EvForkRequested: true, engine.EvTurnRequested: true, engine.EvInterruptRequested: true,
	// the step a session call publishes is its intent: the step file and the packet are written only after it is stored
	engine.EvStepPublished: true,
}

// IsIntent reports whether an event is a dispatch intent.
func IsIntent(kind, event string) bool { return kind == engine.KindCall && intents[event] }

// Append stores e, assigning its sequence number, and returns it. An intent is committed before Append returns; if it cannot
// be, Err reports why and the engine sends nothing for it. After a failure rows are kept in memory only.
func (j *Journal) Append(e engine.Event) engine.Event {
	out := j.appendRow(e)
	j.notifyCommit()
	return out
}

// SetOnCommit installs the function that is called after each committed batch, outside the journal's lock (engine.CommitNotifier).
func (j *Journal) SetOnCommit(notify func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.onCommit = notify
}

// notifyCommit tells the commit notifier that a batch was committed since the last notice.
func (j *Journal) notifyCommit() {
	j.mu.Lock()
	fire, fn := j.committed, j.onCommit
	j.committed = false
	j.mu.Unlock()
	if fire && fn != nil {
		fn()
	}
}

var _ engine.CommitNotifier = (*Journal)(nil)

func (j *Journal) appendRow(e engine.Event) engine.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.RunID != j.runID && j.err == nil {
		j.err = fmt.Errorf("journal: a row of run %q was appended to the journal of run %q", e.RunID, j.runID)
	}
	data, err := normalize(e.Data)
	if err != nil && j.err == nil {
		j.err = fmt.Errorf("journal: the row %s of call %s cannot be stored: %w", e.Event, e.CallID, err)
	}
	e.Data = data
	j.seq++
	e.Seq = j.seq
	j.events = append(j.events, e)
	if j.err != nil {
		return e
	}
	// a row inherits what its call knew when it was appended; the prepared row of a call also inherits the execution of its node
	cc := j.context[e.CallID]
	j.noteContext(e)
	if e.Kind == engine.KindCall && e.Event == engine.EvPrepared {
		cc = j.context[e.CallID]
	}
	j.batch = append(j.batch, stored{e: e, cc: cc})
	switch {
	case IsIntent(e.Kind, e.Event):
		// the rows before it first, then the intent in a transaction of its own
		j.flushLocked(len(j.batch) - 1)
		if j.err == nil {
			j.flushLocked(len(j.batch))
		}
	case j.opts.Immediate || e.Kind == engine.KindRun || (e.Kind == engine.KindCall && isTerminal(e.Event)):
		j.flushLocked(len(j.batch))
	}
	return e
}

func isTerminal(event string) bool {
	switch event {
	case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
		return true
	}
	return false
}

// Events returns every row in append order.
func (j *Journal) Events() []engine.Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]engine.Event(nil), j.events...)
}

// Err is the first failure to store a row, nil while every row is stored.
func (j *Journal) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Flush commits the rows that wait for a batch.
func (j *Journal) Flush() error {
	j.mu.Lock()
	j.flushLocked(len(j.batch))
	err := j.err
	j.mu.Unlock()
	j.notifyCommit()
	return err
}

// Close commits what waits and reports the first failure. It does not close the store.
func (j *Journal) Close() error { return j.Flush() }

// flushLocked commits the first n rows of the batch in one transaction.
func (j *Journal) flushLocked(n int) {
	if n <= 0 || j.err != nil {
		return
	}
	rows := j.batch[:n]
	err := j.st.Transaction(j.ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, r := range rows {
			if err := j.insert(ctx, conn, r.e, r.cc); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		j.err = fmt.Errorf("journal: %w", err)
		return
	}
	j.batch = slices.Clone(j.batch[n:])
	j.committed = true
}

// noteContext keeps what the later rows of a call inherit from this one.
func (j *Journal) noteContext(e engine.Event) {
	if e.Kind != engine.KindCall {
		return
	}
	cc := j.context[e.CallID]
	switch e.Event {
	case engine.EvPrepared:
		node, execution := str(e.Data, "node"), str(e.Data, "execution_id")
		if execution == "" && node != "" && !j.loading {
			// a call the resumed process reaches for the first time belongs to the execution its node's earlier calls do
			execution = j.nodeExecution[node]
		}
		if execution != "" && node != "" {
			j.nodeExecution[node] = execution
		}
		cc = callContext{role: str(e.Data, "role"), node: node, head: str(e.Data, "head"), execution: execution, thread: str(e.Data, "thread_id")}
	case engine.EvThreadStarted, engine.EvForkStarted:
		cc.thread = str(e.Data, "thread_id")
	case engine.EvTurnStarted:
		cc.turn = str(e.Data, "turn_id")
	default:
		return
	}
	j.context[e.CallID] = cc
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// normalize is the row's Data as the store returns it: the JSON round trip, with an empty map as nil. The journal keeps its
// rows in this form from the moment they are appended, so a row reads the same live and after a restart.
func normalize(data map[string]any) (map[string]any, error) {
	if data == nil {
		return nil, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func (j *Journal) stamp() string { return j.opts.Now().UTC().Format(time.RFC3339Nano) }

// insert writes one row in the open transaction.
func (j *Journal) insert(ctx context.Context, conn *sql.Conn, e engine.Event, cc callContext) error {
	if e.Kind == engine.KindRun && e.Event == "started" && !j.hasRun {
		if err := j.insertRun(ctx, conn, e); err != nil {
			return err
		}
		j.hasRun = true
	}
	if e.Kind == engine.KindCall {
		return j.insertCall(ctx, conn, e, cc)
	}
	return j.insertEvent(ctx, conn, e)
}

func (j *Journal) insertRun(ctx context.Context, conn *sql.Conn, e engine.Event) error {
	if j.id.ActorTaskID == "" {
		return errors.New("a run record needs the actor task id (Identity.ActorTaskID)")
	}
	helpers := "[]"
	if raw, err := json.Marshal(e.Data["helpers"]); err == nil && e.Data["helpers"] != nil {
		helpers = string(raw)
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?)`, j.runID, str(e.Data, "workflow"), str(e.Data, "script_digest"), helpers, str(e.Data, "args_digest"),
		j.id.PlanID, j.id.PlanRevision, j.id.ActorTaskID, j.id.EpochNonce, j.id.ResumeFrom, j.stamp())
	return err
}

// The reserved members a row's JSON carries besides its Data: what has no column of its own. A member of Data never starts
// with an underscore.
const (
	keyLabel    = "_label"
	keyKind     = "_kind"
	keyPath     = "_path"
	keyPosition = "_position"
	keyOp       = "_op"
)

func (j *Journal) insertEvent(ctx context.Context, conn *sql.Conn, e engine.Event) error {
	payload := map[string]any{keyKind: e.Kind}
	for k, v := range e.Data {
		payload[k] = v
	}
	if e.Path != "" {
		payload[keyPath] = e.Path
	}
	if e.Position != 0 {
		payload[keyPosition] = e.Position
	}
	if e.Op != "" {
		payload[keyOp] = e.Op
	}
	if e.Label != "" {
		payload[keyLabel] = e.Label
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at) VALUES (?,?,?,?,?,?,?,?)`,
		j.runID, e.Seq, e.Event, e.CallID, str(e.Data, "node"), e.Label, string(raw), j.stamp())
	return err
}

func (j *Journal) insertCall(ctx context.Context, conn *sql.Conn, e engine.Event, cc callContext) error {
	d := e.Data
	detail := map[string]any{}
	for k, v := range d {
		if k != "result" {
			detail[k] = v
		}
	}
	if e.Label != "" {
		detail[keyLabel] = e.Label
	}
	if e.Op == "" {
		detail[keyOp] = ""
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	detailJSON := ""
	if len(detail) > 0 {
		detailJSON = string(raw)
	}
	var result []byte
	if r, ok := d["result"].(string); ok {
		result = []byte(r)
	}
	alloc, _ := d["allocation"].(map[string]any)
	kindOf := e.Op
	if kindOf == "" {
		kindOf = "call"
	}
	role := orElse(str(d, "role"), cc.role)
	node := orElse(str(d, "node"), cc.node)
	commit := orElse(str(d, "commit"), orElse(str(d, "head"), cc.head))
	worktreeKind := str(alloc, "kind")
	if worktreeKind != "fresh" && worktreeKind != "restore" {
		worktreeKind = ""
	}
	inputState := str(d, "input_state")
	if inputState != "delivered" && inputState != "not_delivered" {
		inputState = ""
	}
	pending := orElse(str(d, "pending_tree"), str(alloc, "pending"))
	// the thread and turn a call ended on are named by its terminal row; the rows before it name what they are about
	thread, turn := str(d, "thread_id"), str(d, "turn_id")
	if isTerminal(e.Event) {
		thread, turn = orElse(thread, cc.thread), orElse(turn, cc.turn)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, input_digest, input_snapshot, event, operation, result_digest, result_bytes, result_artifact,
 commit_sha, commit_parent, tree_digest, verification_digest, acceptance_id, batch_id, thread_id, turn_id, plan_id, node_id, attempt, execution_id, worktree_kind, worktree_path, start_commit,
 pending_tree, snapshot_digest, input_state, held_tree, tokens, detail_json, recorded_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.CallID, e.Seq, j.runID, e.Path, e.Position, kindOf, role, str(d, "input_digest"), str(d, "input_snapshot"), e.Event, str(d, "op"), str(d, "result_digest"), result, "",
		commit, str(d, "parent"), str(d, "tree"), str(d, "digest"), str(d, "acceptance_id"), str(d, "batch_commit"), thread, turn,
		orElse(str(d, "plan_id"), j.id.PlanID), node, intOf(alloc["attempt"]), orElse(str(d, "execution_id"), cc.execution), worktreeKind, str(alloc, "path"), str(alloc, "start_commit"),
		pending, str(alloc, "snapshot_digest"), inputState, str(d, "p_held"), tokensOf(d), detailJSON, j.stamp())
	return err
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// tokensOf is the token total a row carries (the usage of a completed call).
func tokensOf(d map[string]any) int64 {
	t, ok := d["tokens"].(map[string]any)
	if !ok {
		return 0
	}
	if n, ok := t["total"].(float64); ok && n > 0 {
		return int64(n)
	}
	return 0
}

// Load reads the rows of a run from the store, in the order they were appended. hasRun is false for a run the store does not
// know.
func Load(ctx context.Context, st *store.Store, runID string) (events []engine.Event, hasRun bool, err error) {
	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_runs WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("journal: %w", err)
	}
	if n == 0 {
		return nil, false, nil
	}
	calls, err := readCalls(ctx, st.DB, `WHERE run_id = ?`, runID)
	if err != nil {
		return nil, true, err
	}
	flows, err := readEvents(ctx, st.DB, runID)
	if err != nil {
		return nil, true, err
	}
	events = append(calls, flows...)
	sort.SliceStable(events, func(i, k int) bool { return events[i].Seq < events[k].Seq })
	return events, true, nil
}

func readCalls(ctx context.Context, st queryer, where string, args ...any) ([]engine.Event, error) {
	return readCallsLimited(ctx, st, where, 0, false, args...)
}

// readCallsLimited reads the rows of flow_calls that where selects, in order, at most limit of them (0: all). Lean leaves the
// result bytes out: the rows then carry no "result" member, which a reader of the shape of a run (not of what its calls returned)
// does not need and which can be large.
func readCallsLimited(ctx context.Context, st queryer, where string, limit int, lean bool, args ...any) ([]engine.Event, error) {
	order := ` ORDER BY event_seq`
	if limit > 0 {
		order += ` LIMIT ` + strconv.Itoa(limit)
	}
	result := `result_bytes`
	if lean {
		result = `NULL`
	}
	rows, err := st.QueryContext(ctx, `SELECT run_id, call_id, event_seq, path, position, kind, event, `+result+`, detail_json FROM flow_calls `+where+order, args...)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer rows.Close()
	var out []engine.Event
	for rows.Next() {
		var e engine.Event
		var kind, detail string
		var result []byte
		if err := rows.Scan(&e.RunID, &e.CallID, &e.Seq, &e.Path, &e.Position, &kind, &e.Event, &result, &detail); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
		e.Kind, e.Op = engine.KindCall, kind
		meta, data, err := splitMeta(detail)
		if err != nil {
			return nil, fmt.Errorf("journal: the row %s of call %s: %w", e.Event, e.CallID, err)
		}
		if _, has := meta[keyOp]; has {
			e.Op = str(meta, keyOp)
		}
		e.Label = str(meta, keyLabel)
		if result != nil {
			if data == nil {
				data = map[string]any{}
			}
			data["result"] = string(result)
		}
		e.Data = data
		out = append(out, e)
	}
	return out, rows.Err()
}

func readEvents(ctx context.Context, st queryer, runID string) ([]engine.Event, error) {
	return readEventsAfter(ctx, st, runID, 0, 0, false)
}

// readEventsAfter reads the flow rows of a run after the sequence number after, in order, at most limit of them (0: all). own
// leaves out the rows a command wrote into the run (engine.ForeignSeqBase and above).
func readEventsAfter(ctx context.Context, st queryer, runID string, after, limit int, own bool) ([]engine.Event, error) {
	order := ` ORDER BY seq`
	if limit > 0 {
		order += ` LIMIT ` + strconv.Itoa(limit)
	}
	where := ` WHERE run_id = ? AND seq > ?`
	args := []any{runID, after}
	if own {
		where += ` AND seq < ?`
		args = append(args, engine.ForeignSeqBase)
	}
	rows, err := st.QueryContext(ctx, `SELECT run_id, seq, kind, call_id, payload_json FROM flow_events`+where+order, args...)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}
	defer rows.Close()
	var out []engine.Event
	for rows.Next() {
		var e engine.Event
		var payload string
		if err := rows.Scan(&e.RunID, &e.Seq, &e.Event, &e.CallID, &payload); err != nil {
			return nil, fmt.Errorf("journal: %w", err)
		}
		meta, data, err := splitMeta(payload)
		if err != nil {
			return nil, fmt.Errorf("journal: the row %s of run %s: %w", e.Event, e.RunID, err)
		}
		e.Kind, e.Path, e.Op, e.Label = str(meta, keyKind), str(meta, keyPath), str(meta, keyOp), str(meta, keyLabel)
		e.Position = intOf(meta[keyPosition])
		e.Data = data
		out = append(out, e)
	}
	return out, rows.Err()
}

// splitMeta separates the reserved members of a stored row from its Data. A row with no Data reads back with a nil Data.
func splitMeta(raw string) (meta, data map[string]any, err error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil, nil
	}
	var all map[string]any
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil, nil, err
	}
	meta = map[string]any{}
	for k, v := range all {
		if strings.HasPrefix(k, "_") {
			meta[k] = v
			delete(all, k)
		}
	}
	if len(all) == 0 {
		all = nil
	}
	return meta, all, nil
}

// ReadCallResult restores the CallResult of one call from its flow_calls rows and from nothing else: the output is the
// canonical result bytes the completed row stored, the status and reason are those of the call's last terminal event (held
// included, which has no output), and the meta is the references the rows carry. The host is not asked and no other table is
// read. ok is false when the call has no prepared row, or its newest attempt (the rows from the last prepared row on) has no
// terminal row: a call that is open again has no result, and the terminal row of an earlier attempt is not its result.
func ReadCallResult(ctx context.Context, st *store.Store, runID, callID string, omitSeq bool) (res engine.CallResult, ok bool, err error) {
	rows, err := readCalls(ctx, st.DB, `WHERE run_id = ? AND call_id = ?`, runID, callID)
	if err != nil {
		return engine.CallResult{}, false, err
	}
	res, ok = engine.RestoreCallResult(runID, callID, rows, omitSeq)
	return res, ok, nil
}

// RunOf is the run that holds a call, "" when no run does.
func RunOf(ctx context.Context, st *store.Store, callID string) (string, error) {
	var run sql.NullString
	err := st.DB.QueryRowContext(ctx, `SELECT run_id FROM flow_calls WHERE call_id = ? ORDER BY rowid DESC LIMIT 1`, callID).Scan(&run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("journal: %w", err)
	}
	return run.String, nil
}

// queryer is what a read statement runs on: the store's database, or a transaction of it that holds one snapshot.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// testAfterCallsRead and testAfterPageRead run between the statements of a read (tests only): after the call rows of a page, and
// after the page, before the rows the state is read from.
var testAfterCallsRead, testAfterPageRead func()

// PageRead is what ReadPage answers: the page and, when it was asked for, every row of the run, both from one snapshot.
type PageRead struct {
	// HasRun is false for a store that holds no run runID; the rest is then empty.
	HasRun bool
	// Events are the rows after the cursor in order, at most the limit of them, and More says that a row beyond them exists.
	Events []engine.Event
	More   bool
	// Lean is every row of the run without call results (LoadLean) when ReadPage was asked for it.
	Lean []engine.Event
}

// ReadPage reads the rows of run runID whose sequence number is above after, in order, at most limit of them (limit 1 or more),
// and, with lean, every row of the run without the results of its calls, all in one read transaction: a writer that commits
// while the page is being read is seen completely or not at all, so the cursor (the seq of the last row) never passes a
// committed row, and a state summed from Lean is the state at the page's own moment. A reader that holds no transaction between
// two pages (a wait) reads each page this way again.
//
// A page holds the run's own rows only. The rows a command wrote into the run (CRW-1204: crw flow report-up, report-ack,
// report-delivered, and crw flow apply before CRW-1416) are numbered from engine.ForeignSeqBase while the run, alive, goes on
// numbering its own below it: a cursor that took one of their numbers would pass every row the run writes after. `crw flow
// reports` lists them; Lean holds them.
func ReadPage(ctx context.Context, st *store.Store, runID string, after, limit int, lean bool) (PageRead, error) {
	if limit < 1 {
		return PageRead{}, errors.New("journal: a page holds at least one row")
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return PageRead{}, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_runs WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return PageRead{}, fmt.Errorf("journal: %w", err)
	}
	if n == 0 {
		return PageRead{}, nil
	}
	out := PageRead{HasRun: true}
	calls, err := readCallsLimited(ctx, tx, `WHERE run_id = ? AND event_seq > ?`, limit+1, true, runID, after)
	if err != nil {
		return PageRead{}, err
	}
	if testAfterCallsRead != nil {
		testAfterCallsRead()
	}
	flows, err := readEventsAfter(ctx, tx, runID, after, limit+1, true)
	if err != nil {
		return PageRead{}, err
	}
	events := append(calls, flows...)
	sort.SliceStable(events, func(i, k int) bool { return events[i].Seq < events[k].Seq })
	if len(events) > limit {
		events, out.More = events[:limit], true
	}
	out.Events = events
	if testAfterPageRead != nil {
		testAfterPageRead()
	}
	if lean {
		if out.Lean, err = loadLean(ctx, tx, runID); err != nil {
			return PageRead{}, err
		}
	}
	return out, nil
}

// LoadAfter is ReadPage without the rows of the whole run: the read of `crw flow watch` and of the GUI's event stream. The number
// of the last row is the cursor of the next read, as `crw relay journal-tail` keeps the relay journal's. A store that holds no run
// runID answers hasRun false.
//
// The rows are lean: they carry no call result (the "result" member of Data), so a follower of a run pays for the shape of the
// run and not for what its calls returned.
func LoadAfter(ctx context.Context, st *store.Store, runID string, after, limit int) (events []engine.Event, more, hasRun bool, err error) {
	p, err := ReadPage(ctx, st, runID, after, limit, false)
	return p.Events, p.More, p.HasRun, err
}

// LoadLean is Load without the results of the calls: the rows of a run as `crw flow ps`, `show` and the GUI read them, from one
// snapshot.
func LoadLean(ctx context.Context, st *store.Store, runID string) (events []engine.Event, hasRun bool, err error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("journal: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_runs WHERE run_id = ?`, runID).Scan(&n); err != nil {
		return nil, false, fmt.Errorf("journal: %w", err)
	}
	if n == 0 {
		return nil, false, nil
	}
	events, err = loadLean(ctx, tx, runID)
	return events, true, err
}

func loadLean(ctx context.Context, q queryer, runID string) ([]engine.Event, error) {
	calls, err := readCallsLimited(ctx, q, `WHERE run_id = ?`, 0, true, runID)
	if err != nil {
		return nil, err
	}
	flows, err := readEvents(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	events := append(calls, flows...)
	sort.SliceStable(events, func(i, k int) bool { return events[i].Seq < events[k].Seq })
	return events, nil
}
