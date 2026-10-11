// Package tracetest builds synthetic store copies for the tests of internal/flow/trace and internal/flow/shadow. A Builder
// opens a relay store in a temporary directory, runs workflow scripts on the engine's fake backend through the flow journal (so
// the flow_* rows are the ones the journal writes), inserts the delivery and integration rows a scenario needs, and Copy
// returns a standalone copy of the file made with SQLite's own VACUUM INTO. No operational database is ever opened.
package tracetest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Header is the meta statement every script body starts with.
const Header = "export const meta = { name: 'fixture' }\n"

// Epoch is the first instant of the builder's clock.
var Epoch = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// Builder is a synthetic store.
type Builder struct {
	T    testing.TB
	Dir  string
	Path string
	St   *store.Store

	mu  sync.Mutex
	now time.Time
	n   int
}

// New opens a store in a temporary directory with foreign keys off, so a scenario can insert the rows it needs without the
// parents the rest of the system would have written.
func New(t testing.TB) *Builder {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "src", "relay.sqlite3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := &Builder{T: t, Dir: dir, Path: path, St: st, now: Epoch}
	b.Exec(`PRAGMA foreign_keys = OFF`)
	return b
}

// Now is the builder's clock: every reading is one second after the one before, so rows written in order are recorded in order.
func (b *Builder) Now() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = b.now.Add(time.Second)
	return b.now
}

// Stamp is a reading of the clock as the stores spell it.
func (b *Builder) Stamp() string { return b.Now().UTC().Format(time.RFC3339Nano) }

// At is the stamp of Epoch plus s seconds.
func At(s int) string { return Epoch.Add(time.Duration(s) * time.Second).Format(time.RFC3339Nano) }

// Exec runs a statement on the store.
func (b *Builder) Exec(q string, args ...any) {
	b.T.Helper()
	if _, err := b.St.DB.ExecContext(context.Background(), q, args...); err != nil {
		b.T.Fatalf("%v\n%s", err, q)
	}
}

// ID is a fresh id with a prefix.
func (b *Builder) ID(prefix string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	return fmt.Sprintf("%s-%d", prefix, b.n)
}

// Run runs a script (its whole text, header included) as run runID through the flow journal, on backend (a fresh fake when
// nil), and returns the result.
func (b *Builder) Run(runID, planID, source string, backend engine.Backend, policy *engine.Policy) *engine.Result {
	b.T.Helper()
	ctx := context.Background()
	j, err := journal.Open(ctx, b.St, runID, journal.Identity{PlanID: planID, PlanRevision: 1, ActorTaskID: "task-orchestrator", EpochNonce: "nonce"}, journal.Options{Now: b.Now})
	if err != nil {
		b.T.Fatal(err)
	}
	prog, err := script.Load(script.Source{Name: "fixture.js", Text: []byte(source)})
	if err != nil {
		b.T.Fatal(err)
	}
	if backend == nil {
		backend = engine.NewFake()
	}
	if policy == nil {
		policy = engine.FakePolicy()
	}
	res, err := engine.Run(ctx, prog, engine.Config{RunID: runID, Backend: backend, Policy: policy, Journal: j, Now: b.Now})
	if err != nil {
		b.T.Fatal(err)
	}
	if err := j.Close(); err != nil {
		b.T.Fatal(err)
	}
	return res
}

// Copy is a standalone copy of the store as it is now, in a directory of its own, made with VACUUM INTO: no write-ahead log
// beside it and nothing of the source's identity but its content.
func (b *Builder) Copy() string {
	b.T.Helper()
	dir := filepath.Join(b.Dir, fmt.Sprintf("copy-%d", b.nextCopy()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.T.Fatal(err)
	}
	path := filepath.Join(dir, "relay.sqlite3")
	b.Exec(`VACUUM INTO ?`, path)
	return path
}

func (b *Builder) nextCopy() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	return b.n
}

// Execution inserts a dag_agent_executions row.
func (b *Builder) Execution(id, plan, node, kind, role, tier, run, at string) {
	b.T.Helper()
	supersedes := any(nil)
	if kind != "initial" {
		supersedes = id + "-prev"
	}
	b.Exec(`INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, author_model, author_effort, thread_id, worktree, base_sha, run_id, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id)
 VALUES (?, ?, ?, 1, ?, 'manifest', ?, ?, '', '', '', '', '', ?, 'task-orchestrator', 1, ?, ?)`, id, plan, node, kind, role, tier, run, at, supersedes)
}

// Acceptance inserts a dag_agent_acceptances row that stands on reviewerCall.
func (b *Builder) Acceptance(id, plan, node, execution, reviewerCall, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, head, base, repository, premerge_digest, premerge_json, premerge_evaluated_head, verification_digest, verification_json, tree_sha, criteria_set_digest, reviewer_call_id, reviewer_model, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at)
 VALUES (?, ?, ?, 'manifest', ?, 'head', 'base', 'repo', 'pd', '{}', 'head', 'vd', '{}', 'tree', 'criteria', ?, 'gpt-fake-sol', 'pairs', 'task-orchestrator', 1, ?)`, id, plan, node, execution, reviewerCall, at)
}

// Closure inserts a dag_agent_closures row.
func (b *Builder) Closure(kind, id, state, reason, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES (?, ?, ?, 1, 'task-orchestrator', 1, ?, ?)`, kind, id, state, reason, at)
}

// SpawnDecision inserts a flow_spawn_decisions row.
func (b *Builder) SpawnDecision(run, kind, node, call, rule, decision, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO flow_spawn_decisions (decision_id, run_id, kind, call_id, node_id, rule_id, decision, evidence_json, decided_at) VALUES (?, ?, ?, ?, ?, ?, ?, '{}', ?)`, b.ID("sd"), run, kind, call, node, rule, decision, at)
}

// StepDecision inserts a flow_step_decisions row.
func (b *Builder) StepDecision(run, node, from, to, rule, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO flow_step_decisions (step_id, run_id, node_id, from_tier, to_tier, rule_id, evidence_json, decided_at) VALUES (?, ?, ?, ?, ?, ?, '{}', ?)`, b.ID("st"), run, node, from, to, rule, at)
}

// Delivery inserts a delivery of an event with its attempts: sent are the start stamps of attempts that sent, busy the number
// of attempts that were deferred busy, and journalBusy the number of delivery_deferred_busy journal rows.
func (b *Builder) Delivery(event, relationship, recipient, state, hold, created string, sent []string, busy, journalBusy int) {
	b.T.Helper()
	b.Exec(`INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, hold_reason, created_at, updated_at) VALUES (?, ?, 'verdict', ?, 'thread', ?, ?, ?, ?)`,
		event, relationship, recipient, state, nullable(hold), created, created)
	no := 0
	for _, at := range sent {
		no++
		b.Exec(`INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?, ?, ?, 'dispatch', 'settled', 'dispatched', ?, ?)`, b.ID("rq"), event, no, at, at)
	}
	for i := 0; i < busy; i++ {
		no++
		b.Exec(`INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?, ?, ?, 'dispatch', 'settled', 'deferred_busy', ?, ?)`, b.ID("rq"), event, no, created, created)
	}
	for i := 0; i < journalBusy; i++ {
		b.Exec(`INSERT INTO journal (at, kind, subject, detail) VALUES (?, 'delivery_deferred_busy', ?, '')`, created, event)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Journal inserts a row of the relay's journal table.
func (b *Builder) Journal(kind, subject, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO journal (at, kind, subject, detail) VALUES (?, ?, ?, '')`, at, kind, subject)
}

// IntegrationStage inserts a dag_integration_stages row (node "" is the batch-level row).
func (b *Builder) IntegrationStage(batch, plan, stage, node, acceptance, detail, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)
 VALUES (?, ?, ?, ?, ?, ?, '', '', 0, '', ?, 'task-orchestrator', ?)`, b.ID("is"), batch, plan, stage, node, acceptance, detail, at)
}

// AgentIntegrationStage inserts a dag_agent_integration_stages row.
func (b *Builder) AgentIntegrationStage(batch, plan, stage, node, acceptance, execution, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO dag_agent_integration_stages (stage_id, batch_id, plan_id, node_id, acceptance_id, execution_id, stage, head, detail, recorded_by, recorded_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 'head', '', 'task-orchestrator', ?)`, b.ID("ais"), batch, plan, node, acceptance, execution, stage, at)
}

// IntegrationBatch inserts a dag_integration_batches row (a batch that moved the ref).
func (b *Builder) IntegrationBatch(batch, plan, ref, at string) {
	b.T.Helper()
	b.Exec(`INSERT INTO dag_integration_batches (batch_id, plan_id, repository, integration_ref, base_ref, old_head, new_head, merged_json, split_json, verification_json, recorded_by, coordinator_epoch, recorded_at)
 VALUES (?, ?, 'repo', ?, 'refs/heads/main', 'old', 'new', '[]', '[]', '{}', 'task-orchestrator', 1, ?)`, batch, plan, ref, at)
}

// SyncOutbox inserts a sync_outbox row in state.
func (b *Builder) SyncOutbox(state string) {
	b.T.Helper()
	b.Exec(`INSERT INTO sync_outbox (sync_id, relationship_id, issue_key, target, target_ref, subject_kind, identity_digest, summary, state, created_at, updated_at) VALUES (?, 'rel', 'CRW-1', 'linear', 'ref', 'verdict', ?, 's', ?, ?, ?)`, b.ID("sy"), b.ID("dg"), state, At(0), At(0))
}

// MergeTrain inserts a merge train with one member and one event.
func (b *Builder) MergeTrain() {
	b.T.Helper()
	id := b.ID("train")
	b.Exec(`INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES (?, 'target', 'repo', 'main', 'sha', 'leader', ?)`, id, At(0))
	b.Exec(`INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES (?, 1, 'turn', 1, 'rel', 'head')`, id)
	b.Exec(`INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES (?, 1, 'opened', 'leader', '{}', ?)`, id, At(0))
}

// Snapshot is the state of every file under dir: its size, modification time and SHA-256 by relative path. Two snapshots are
// equal when nothing under dir was written, created, removed or touched.
func Snapshot(t testing.TB, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel+"/"] = info.ModTime().UTC().Format(time.RFC3339Nano)
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = fmt.Sprintf("%d %s %x", info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), sum)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Context is a context that ends with the test, bounded so a stuck read fails the test instead of hanging it.
func Context(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
