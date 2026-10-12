package trace

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Options are what Export reads.
type Options struct {
	// Path is the store copy (the relay.sqlite3 file of a copy made in a temporary directory).
	Path string
	// Run limits the trace to one run; "" exports every run of the copy.
	Run string
}

// openTimeout bounds the wait of the read-only opener.
const openTimeout = 10 * time.Second

// reader is the read surface of the opened copy: queries only.
type reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Export writes nothing: it reads the journal, flow and DAG-zone rows and the delivery rows of the store copy at opts.Path and
// returns the trace. The copy is opened in place and read-only (store.OpenInPlace: mode=ro or immutable, no lock, no schema
// script, no sidecar created), after RefuseLive has judged the path; the SHA-256 of the copy's file is taken before and after
// and the export fails if they differ, so a trace names exactly the bytes it was read from. A copy whose write-ahead log holds
// frames without their index is refused (store.ErrWALWithoutIndex): make the copy with SQLite's backup, not with cp.
func Export(ctx context.Context, opts Options) (*Trace, error) {
	if err := RefuseLive(opts.Path); err != nil {
		return nil, err
	}
	before, size, err := fileDigest(opts.Path)
	if err != nil {
		return nil, err
	}
	ro, err := store.OpenInPlace(ctx, opts.Path, openTimeout)
	if err != nil {
		return nil, fmt.Errorf("open the store copy %s: %w", opts.Path, err)
	}
	t, err := build(ctx, ro, opts)
	closeErr := ro.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	after, _, err := fileDigest(opts.Path)
	if err != nil {
		return nil, err
	}
	if after != before {
		return nil, fmt.Errorf("the store copy %s changed while it was read (%s, then %s)", opts.Path, before, after)
	}
	t.Source.Digest, t.Source.Bytes = before, size
	return t, nil
}

// fileDigest is the SHA-256 of the file at path and its size.
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func build(ctx context.Context, r reader, opts Options) (*Trace, error) {
	b := &builder{r: r, ctx: ctx, tables: map[string]bool{}}
	rows, err := r.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("the file is not a readable relay store: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return nil, err
		}
		b.tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	if !b.tables["schema_meta"] {
		return nil, errors.New("the file is not a relay store: it has no schema_meta table")
	}
	t := &Trace{Schema: Schema, Runs: []Run{}, Journal: []JournalRow{}, Deliveries: []Delivery{}, Acceptances: []Acceptance{}, Integration: []Batch{}}
	_ = r.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = 'store_id'`).Scan(&t.Source.StoreID)
	if t.Acceptances, err = b.acceptances(); err != nil {
		return nil, err
	}
	if t.Integration, err = b.batches(); err != nil {
		return nil, err
	}
	if t.Journal, err = b.journal(); err != nil {
		return nil, err
	}
	if t.Deliveries, err = b.deliveries(); err != nil {
		return nil, err
	}
	if t.Aux, err = b.aux(); err != nil {
		return nil, err
	}
	runs, err := b.runs(opts.Run)
	if err != nil {
		return nil, err
	}
	if opts.Run != "" && len(runs) == 0 {
		return nil, fmt.Errorf("the store copy holds no run %q", opts.Run)
	}
	for i := range runs {
		if err := b.fill(&runs[i], t); err != nil {
			return nil, err
		}
	}
	t.Runs = runs
	if opts.Run != "" {
		// the selection is the one Select makes of the whole export, so --run on the export and --run on the reader agree
		return t.Select(opts.Run)
	}
	return t, nil
}

type builder struct {
	r      reader
	ctx    context.Context
	tables map[string]bool
}

// query runs q when every table it names exists, and returns no rows when one is missing: a store from before a zone table
// has none of its rows.
func (b *builder) query(tables []string, q string, args ...any) (*sql.Rows, error) {
	for _, t := range tables {
		if !b.tables[t] {
			return nil, nil
		}
	}
	return b.r.QueryContext(b.ctx, q, args...)
}

func (b *builder) runs(only string) ([]Run, error) {
	q, args := `SELECT run_id, workflow, script_digest, args_digest, plan_id, plan_revision, resume_from, started_at FROM flow_runs ORDER BY rowid`, []any(nil)
	if only != "" {
		q, args = `SELECT run_id, workflow, script_digest, args_digest, plan_id, plan_revision, resume_from, started_at FROM flow_runs WHERE run_id = ? ORDER BY rowid`, []any{only}
	}
	rows, err := b.query([]string{"flow_runs"}, q, args...)
	if err != nil || rows == nil {
		return []Run{}, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.RunID, &r.Workflow, &r.ScriptDigest, &r.ArgsDigest, &r.PlanID, &r.PlanRevision, &r.ResumeFrom, &r.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// splitMeta separates the reserved underscore members of a stored row from its data (internal/flow/journal stores them so).
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

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
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

// events reads the journal rows of a run, the call rows and the flow rows, in the order the journal appended them.
func (b *builder) events(runID string) ([]Event, error) {
	var out []Event
	rows, err := b.query([]string{"flow_calls"}, `SELECT event_seq, call_id, path, position, kind, event, result_bytes, detail_json, recorded_at FROM flow_calls WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			var e Event
			var kind, detail string
			var result []byte
			if err := rows.Scan(&e.Seq, &e.CallID, &e.Path, &e.Position, &kind, &e.Event, &result, &detail, &e.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			meta, data, err := splitMeta(detail)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("the row %s of call %s: %w", e.Event, e.CallID, err)
			}
			e.Kind, e.Op = engine.KindCall, kind
			if _, has := meta["_op"]; has {
				e.Op = str(meta, "_op")
			}
			e.Label = str(meta, "_label")
			if result != nil {
				if data == nil {
					data = map[string]any{}
				}
				data["result"] = string(result)
			}
			e.Data = data
			out = append(out, e)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	rows, err = b.query([]string{"flow_events"}, `SELECT seq, kind, call_id, payload_json, recorded_at FROM flow_events WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			var e Event
			var payload string
			if err := rows.Scan(&e.Seq, &e.Event, &e.CallID, &payload, &e.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			meta, data, err := splitMeta(payload)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("the row %s of run %s: %w", e.Event, runID, err)
			}
			e.Kind, e.Path, e.Op, e.Label = str(meta, "_kind"), str(meta, "_path"), str(meta, "_op"), str(meta, "_label")
			e.Position = intOf(meta["_position"])
			e.Data = data
			out = append(out, e)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].Seq < out[k].Seq })
	if out == nil {
		out = []Event{}
	}
	return out, nil
}

// fill reads one run: its rows, calls, nodes and recorded decisions.
func (b *builder) fill(run *Run, t *Trace) error {
	events, err := b.events(run.RunID)
	if err != nil {
		return err
	}
	run.Events = events
	run.Calls = calls(run.RunID, events)
	execs, err := b.executions(run)
	if err != nil {
		return err
	}
	if run.Decisions, err = b.decisions(run, execs); err != nil {
		return err
	}
	run.Nodes = nodes(run, execs, t)
	return nil
}

// calls restores every agent call of a run from its rows alone.
func calls(runID string, events []Event) []Call {
	order := []string{}
	rowsOf := map[string][]engine.Event{}
	unmeasured := map[string]bool{}
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == "tokens_unmeasured" && e.CallID != "" {
			unmeasured[e.CallID] = true
		}
		if e.Kind != engine.KindCall || e.Op != "agent" {
			continue
		}
		if _, ok := rowsOf[e.CallID]; !ok {
			order = append(order, e.CallID)
		}
		rowsOf[e.CallID] = append(rowsOf[e.CallID], e.Engine(runID))
	}
	at := map[int]string{}
	for _, e := range events {
		at[e.Seq] = e.At
	}
	out := []Call{}
	for _, id := range order {
		rows := rowsOf[id]
		var prep engine.Event
		for _, ev := range rows {
			if ev.Event == engine.EvPrepared {
				prep = ev
			}
		}
		if prep.Seq == 0 {
			continue
		}
		d := prep.Data
		c := Call{
			CallID: id, Path: prep.Path, Position: prep.Position, Label: prep.Label, Role: str(d, "role"), Family: str(d, "family"), Node: str(d, "node"),
			Model: str(d, "model"), Effort: str(d, "effort"), ResumeOf: str(d, "resume_of"), StepDecision: str(d, "step_decision"), SchemaDigest: str(d, "schema_digest"),
			Status: "open", PreparedSeq: prep.Seq, PreparedAt: at[prep.Seq], TokensUnmeasured: unmeasured[id],
		}
		if res, ok := engine.RestoreCallResult(runID, id, rows, false); ok {
			c.Status, c.Reason, c.ThreadID = res.Status, res.Reason, res.Meta.ThreadID
			c.Tokens = Tokens{Input: res.Meta.Tokens.Input, Output: res.Meta.Tokens.Output, Total: res.Meta.Tokens.Total}
			c.RepairUsed, c.Turns = res.Meta.RepairUsed, len(res.Meta.TurnIDs)
			c.TerminalSeq, c.TerminalAt = res.Meta.EndSeq, at[res.Meta.EndSeq]
			if res.Status == engine.StatusCompleted {
				c.Result = string(res.Output)
			}
		}
		out = append(out, c)
	}
	return out
}

// execution is a dag_agent_executions row of the run.
type execution struct {
	id, node, kind, role, tier, at string
}

func (b *builder) executions(run *Run) (map[string]execution, error) {
	rows, err := b.query([]string{"dag_agent_executions"}, `SELECT execution_id, node_id, kind, role, tier, decided_at FROM dag_agent_executions WHERE run_id = ? OR (run_id = '' AND plan_id = ?) ORDER BY rowid`, run.RunID, run.PlanID)
	if err != nil || rows == nil {
		return map[string]execution{}, err
	}
	defer rows.Close()
	out := map[string]execution{}
	for rows.Next() {
		var x execution
		if err := rows.Scan(&x.id, &x.node, &x.kind, &x.role, &x.tier, &x.at); err != nil {
			return nil, err
		}
		out[x.id] = x
	}
	return out, rows.Err()
}

func (b *builder) decisions(run *Run, execs map[string]execution) ([]Decision, error) {
	out := []Decision{}
	rows, err := b.query([]string{"flow_spawn_decisions"}, `SELECT decision_id, kind, call_id, node_id, rule_id, decision, reason, requested_model, requested_effort, applied_model, applied_effort, decided_at FROM flow_spawn_decisions WHERE run_id = ? ORDER BY rowid`, run.RunID)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			d := Decision{Source: "flow_spawn_decisions"}
			if err := rows.Scan(&d.ID, &d.Kind, &d.CallID, &d.Node, &d.Rule, &d.Decision, &d.Reason, &d.RequestedModel, &d.RequestedEffort, &d.AppliedModel, &d.AppliedEffort, &d.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, d)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	rows, err = b.query([]string{"flow_step_decisions"}, `SELECT step_id, node_id, from_tier, to_tier, rule_id, decided_at FROM flow_step_decisions WHERE run_id = ? ORDER BY rowid`, run.RunID)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			d := Decision{Source: "flow_step_decisions", Kind: "tier"}
			if err := rows.Scan(&d.ID, &d.Node, &d.FromTier, &d.ToTier, &d.Rule, &d.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, d)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// the store has one connection: the acceptance map is read before the closures' cursor is opened
	accNode, err := b.acceptanceNodes()
	if err != nil {
		return nil, err
	}
	rows, err = b.query([]string{"dag_agent_closures"}, `SELECT subject_kind, subject_id, reason, closed_at FROM dag_agent_closures WHERE state = 'escalated' ORDER BY closed_at, rowid`)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			var kind, id, reason, at string
			if err := rows.Scan(&kind, &id, &reason, &at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			node := ""
			if kind == "execution" {
				x, ok := execs[id]
				if !ok {
					continue
				}
				node = x.node
			} else {
				n, ok := accNode[id]
				if !ok || !nodeOfRun(run, execs, n) {
					continue
				}
				node = n.node
			}
			out = append(out, Decision{Source: "dag_agent_closures", ID: kind + ":" + id, Kind: "escalation", Node: node, Reason: reason, At: at})
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].At != out[k].At {
			return out[i].At < out[k].At
		}
		if out[i].Source != out[k].Source {
			return out[i].Source < out[k].Source
		}
		return out[i].ID < out[k].ID
	})
	return out, nil
}

type accRef struct{ node, execution string }

func nodeOfRun(_ *Run, execs map[string]execution, a accRef) bool {
	_, ok := execs[a.execution]
	return ok
}

// acceptanceNodes maps an agent acceptance to its node and execution.
func (b *builder) acceptanceNodes() (map[string]accRef, error) {
	rows, err := b.query([]string{"dag_agent_acceptances"}, `SELECT acceptance_id, node_id, execution_id FROM dag_agent_acceptances`)
	out := map[string]accRef{}
	if err != nil || rows == nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var a accRef
		if err := rows.Scan(&id, &a.node, &a.execution); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

func (b *builder) acceptances() ([]Acceptance, error) {
	out := []Acceptance{}
	rows, err := b.query([]string{"dag_agent_acceptances"}, `SELECT acceptance_id, plan_id, node_id, execution_id, reviewer_call_id, accepted_at FROM dag_agent_acceptances ORDER BY accepted_at, acceptance_id`)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			a := Acceptance{Kind: "agent"}
			if err := rows.Scan(&a.AcceptanceID, &a.PlanID, &a.NodeID, &a.ExecutionID, &a.ReviewerCallID, &a.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, a)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	rows, err = b.query([]string{"dag_acceptances"}, `SELECT acceptance_id, plan_id, node_id, accepted_at FROM dag_acceptances ORDER BY accepted_at, acceptance_id`)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			a := Acceptance{Kind: "relationship"}
			if err := rows.Scan(&a.AcceptanceID, &a.PlanID, &a.NodeID, &a.At); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, a)
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].At != out[k].At {
			return out[i].At < out[k].At
		}
		return out[i].AcceptanceID < out[k].AcceptanceID
	})
	return out, nil
}

// batches reads the integration batches: one per batch id any stage table or the batch table names.
func (b *builder) batches() ([]Batch, error) {
	type stage struct{ batch, plan, stage, node, acceptance, at string }
	var stages []stage
	read := func(table, q string) error {
		rows, err := b.query([]string{table}, q)
		if err != nil || rows == nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s stage
			if err := rows.Scan(&s.batch, &s.plan, &s.stage, &s.node, &s.acceptance, &s.at); err != nil {
				return err
			}
			stages = append(stages, s)
		}
		return rows.Err()
	}
	if err := read("dag_integration_stages", `SELECT batch_id, plan_id, stage, node_id, acceptance_id, recorded_at FROM dag_integration_stages ORDER BY recorded_at, rowid`); err != nil {
		return nil, err
	}
	if err := read("dag_agent_integration_stages", `SELECT batch_id, plan_id, stage, node_id, acceptance_id, recorded_at FROM dag_agent_integration_stages ORDER BY recorded_at, rowid`); err != nil {
		return nil, err
	}
	type batchRow struct{ plan, ref, at string }
	rowsOf := map[string]batchRow{}
	rows, err := b.query([]string{"dag_integration_batches"}, `SELECT batch_id, plan_id, integration_ref, recorded_at FROM dag_integration_batches`)
	if err != nil {
		return nil, err
	}
	if rows != nil {
		for rows.Next() {
			var id string
			var r batchRow
			if err := rows.Scan(&id, &r.plan, &r.ref, &r.at); err != nil {
				_ = rows.Close()
				return nil, err
			}
			rowsOf[id] = r
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	byID := map[string]*Batch{}
	var order []string
	get := func(id, plan string) *Batch {
		if x, ok := byID[id]; ok {
			return x
		}
		x := &Batch{BatchID: id, PlanID: plan, Nodes: []BatchNode{}}
		byID[id] = x
		order = append(order, id)
		return x
	}
	nodeAt := map[string]int{}
	for _, s := range stages {
		bt := get(s.batch, s.plan)
		if bt.IntentAt == "" || s.at < bt.IntentAt {
			bt.IntentAt = s.at
		}
		if s.stage == "ref_moved" && s.node == "" && bt.MovedAt == "" {
			bt.MovedAt = s.at
		}
		if s.node == "" {
			continue
		}
		key := s.batch + "\x00" + s.node + "\x00" + s.acceptance
		i, ok := nodeAt[key]
		if !ok {
			bt.Nodes = append(bt.Nodes, BatchNode{NodeID: s.node, AcceptanceID: s.acceptance})
			i = len(bt.Nodes) - 1
			nodeAt[key] = i
		}
		n := &bt.Nodes[i]
		switch s.stage {
		case "intent":
			if n.IntentAt == "" {
				n.IntentAt = s.at
			}
		case "marked":
			n.MarkedAt, n.MarkPending = s.at, false
		case "mark_pending":
			if n.MarkedAt == "" {
				n.MarkPending = true
			}
		}
	}
	for id, r := range rowsOf {
		bt := get(id, r.plan)
		bt.Ref = r.ref
		if bt.IntentAt == "" || r.at < bt.IntentAt {
			bt.IntentAt = r.at
		}
		if bt.MovedAt == "" {
			bt.MovedAt = r.at
		}
	}
	out := []Batch{}
	for _, id := range order {
		bt := byID[id]
		bt.Landed = bt.MovedAt != ""
		for _, n := range bt.Nodes {
			bt.Landed = bt.Landed || n.MarkedAt != ""
		}
		out = append(out, *bt)
	}
	sort.SliceStable(out, func(i, k int) bool {
		if out[i].IntentAt != out[k].IntentAt {
			return out[i].IntentAt < out[k].IntentAt
		}
		return out[i].BatchID < out[k].BatchID
	})
	return out, nil
}

// journalKinds are the relay journal rows the trace carries: the kinds that begin with one of these prefixes, and the listed ones.
var (
	journalPrefixes = []string{"delivery\\_", "dag\\_", "flow\\_"}
	journalListed   = []string{"event_accepted", "acknowledged", "ack_verified", "verdict_recorded", "verdict_superseded", "sync_enqueued", "reconciled"}
)

// journal reads the relay journal rows of the lifecycle the trace follows, by seq.
func (b *builder) journal() ([]JournalRow, error) {
	out := []JournalRow{}
	var where []string
	var args []any
	for _, p := range journalPrefixes {
		where = append(where, `kind LIKE ? ESCAPE '\'`)
		args = append(args, p+"%")
	}
	where = append(where, `kind IN (`+strings.TrimSuffix(strings.Repeat("?,", len(journalListed)), ",")+`)`)
	for _, k := range journalListed {
		args = append(args, k)
	}
	rows, err := b.query([]string{"journal"}, `SELECT seq, at, kind, COALESCE(subject, '') FROM journal WHERE `+strings.Join(where, " OR ")+` ORDER BY seq`, args...)
	if err != nil || rows == nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var j JournalRow
		if err := rows.Scan(&j.Seq, &j.At, &j.Kind, &j.Subject); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// busyDeferred and the counted states are the delivery policy's own (store.RelationshipSendsSQL): an attempt that settled as
// withheld before the send or deferred busy is not a send.
func (b *builder) deliveries() ([]Delivery, error) {
	out := []Delivery{}
	hasJournal := b.tables["journal"]
	busy := `0`
	if hasJournal {
		busy = `(SELECT COUNT(*) FROM journal j WHERE j.kind = 'delivery_deferred_busy' AND j.subject = d.event_id)`
	}
	hasAttempts := b.tables["attempts"]
	first, sends, deferred := `NULL`, `0`, `0`
	if hasAttempts {
		counted := `a.event_id = d.event_id AND a.sent_at IS NOT NULL AND (a.state IS NULL OR a.state NOT IN ('withheld_pre_send', 'deferred_busy'))`
		first = `(SELECT MIN(a.sent_at) FROM attempts a WHERE ` + counted + `)`
		sends = `(SELECT COUNT(*) FROM attempts a WHERE ` + counted + `)`
		deferred = `(SELECT COUNT(*) FROM attempts a WHERE a.event_id = d.event_id AND a.state = 'deferred_busy')`
	}
	rows, err := b.query([]string{"deliveries"}, `SELECT d.event_id, d.relationship_id, d.recipient_task_id, d.kind, d.state, COALESCE(d.hold_reason, ''), d.created_at, COALESCE(`+first+`, ''), `+sends+`, `+busy+` + `+deferred+` FROM deliveries d ORDER BY d.created_at, d.event_id`)
	if err != nil || rows == nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.EventID, &d.RelationshipID, &d.RecipientTaskID, &d.Kind, &d.State, &d.HoldReason, &d.CreatedAt, &d.FirstSentAt, &d.Sends, &d.BusyAnswers); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (b *builder) aux() (Aux, error) {
	a := Aux{SyncOutbox: SyncOutbox{ByState: map[string]int{}}}
	rows, err := b.query([]string{"sync_outbox"}, `SELECT state, COUNT(*) FROM sync_outbox GROUP BY state`)
	if err != nil {
		return a, err
	}
	if rows != nil {
		for rows.Next() {
			var state string
			var n int
			if err := rows.Scan(&state, &n); err != nil {
				_ = rows.Close()
				return a, err
			}
			a.SyncOutbox.ByState[state] = n
			a.SyncOutbox.Rows += n
		}
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return a, err
		}
	}
	count := func(table string, into *int) error {
		if !b.tables[table] {
			return nil
		}
		return b.r.QueryRowContext(b.ctx, `SELECT COUNT(*) FROM `+table).Scan(into)
	}
	if err := count("merge_trains", &a.MergeTrains.Trains); err != nil {
		return a, err
	}
	if err := count("merge_train_members", &a.MergeTrains.Members); err != nil {
		return a, err
	}
	if err := count("merge_train_events", &a.MergeTrains.Events); err != nil {
		return a, err
	}
	return a, nil
}

// stageOf is the stage a call of a role starts on its node.
func stageOf(role string) string {
	switch role {
	case "executor", "executor-t1":
		return "build"
	case "reviewer":
		return "review"
	case "reviewer-final":
		return "final_review"
	case "architect":
		return "plan"
	case "explorer":
		return "explore"
	}
	return role
}

// nodes builds the stage transitions of every node the run touched, oldest first.
func nodes(run *Run, execs map[string]execution, t *Trace) []Node {
	byNode := map[string]*Node{}
	var order []string
	add := func(node string, tr Transition) {
		if node == "" {
			return
		}
		n, ok := byNode[node]
		if !ok {
			n = &Node{NodeID: node}
			byNode[node] = n
			order = append(order, node)
		}
		n.Transitions = append(n.Transitions, tr)
	}
	ids := make([]string, 0, len(execs))
	for id := range execs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		x := execs[id]
		add(x.node, Transition{Stage: "released", At: x.at, Source: "dag_agent_executions", Detail: x.kind})
	}
	for _, c := range run.Calls {
		add(c.Node, Transition{Stage: stageOf(c.Role), At: c.PreparedAt, Seq: c.PreparedSeq, CallID: c.CallID, Source: "flow_calls", Detail: c.Role})
	}
	accNode := map[string]string{}
	for _, a := range t.Acceptances {
		if a.Kind == "agent" {
			if _, ok := execs[a.ExecutionID]; ok {
				accNode[a.AcceptanceID] = a.NodeID
				add(a.NodeID, Transition{Stage: "accepted", At: a.At, Source: "dag_agent_acceptances", Detail: a.AcceptanceID})
			}
		}
	}
	for _, batch := range t.Integration {
		for _, n := range batch.Nodes {
			if _, ok := accNode[n.AcceptanceID]; ok && n.MarkedAt != "" {
				add(n.NodeID, Transition{Stage: "integrated", At: n.MarkedAt, Source: "integration", Detail: batch.BatchID})
			}
		}
	}
	for _, d := range run.Decisions {
		if d.Source == "dag_agent_closures" {
			add(d.Node, Transition{Stage: "escalated", At: d.At, Source: d.Source, Detail: d.Reason})
		}
	}
	out := []Node{}
	sort.Strings(order)
	for _, id := range order {
		n := byNode[id]
		sort.SliceStable(n.Transitions, func(i, k int) bool { return n.Transitions[i].At < n.Transitions[k].At })
		out = append(out, *n)
	}
	return out
}
