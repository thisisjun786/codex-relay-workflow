package delivery

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_ack_reconcile.py ACR-21..ACR-23 (see ackreconcile_a_test.go).

func Test21_ACR21_recovery_reports_awaiting_acks_and_never_a_correction(t *testing.T) {
	t.Run("dispatched completion", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_a_dispatched_delivery_awaiting_acknowledgement_is_reported_not_resent", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			h.attemptOn(event, h.host, nil)
			report := h.recoverOnStart()
			h.eq(field(report, "awaitingAck"))
			h.eq(field(report, "resent"))
		})
	})
	t.Run("dispatched correction", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_a_dispatched_correction_is_not_reported_as_awaiting_acknowledgement", func(h *hl) {
			first := h.verdictAcknowledged([]string{parent, child})
			_, err := h.ack.RecordVerdict(h.ctx, first, "needs_changes", "v1", nil, []any{Obj{{Key: "id", Value: "tie"}, {Key: "verdict", Value: "needs_changes"}, {Key: "note", Value: "equal timestamps"}}}, nil, nil)
			mustDo(t, err)
			correction := h.one("SELECT event_id FROM deliveries WHERE kind = ?", Revision).S("event_id")
			h.attemptOn(correction, h.host, nil)

			h.clock.Advance(5)
			r, err := LoadRelationship(h.ctx, h.store, h.rid)
			mustDo(t, err)
			_, err = BindAnchor(h.ctx, h.store, h.clock, h.rid, r.Generation, "revision-turn")
			mustDo(t, err)
			turn := turnRef{child, "revision-turn", "completed"}
			payload := h.readyPayload(h.rid, r.Generation, []string{h.artifact("revised.txt", "the corrected deliverable")}, 1, turn)
			_, err = h.accept(payload, storeAcceptNone)
			mustDo(t, err)
			_, err = h.delivery.Enqueue(h.ctx, str(payload, "eventId"), "", "")
			mustDo(t, err)
			h.attemptOn(str(payload, "eventId"), h.host, nil)

			report := h.recoverOnStart()
			h.eq(field(report, "awaitingAck"))
			h.eq(listed(report, "awaitingAck", correction))
			h.eq(field(report, "resent"))
			h.eq(h.row(correction).S("state"))
			h.eq(h.row(str(payload, "eventId")).S("state"))
			h.eq(len(h.host.sends))
		})
	})
}

// counts is VerdictAtomicity._counts.
func (h *hl) counts() []any {
	return []any{
		h.count("SELECT COUNT(*) AS c FROM generations"),
		h.count("SELECT COUNT(*) AS c FROM verdicts"),
		h.count("SELECT COUNT(*) AS c FROM deliveries WHERE kind = 'revision_request'"),
	}
}

// failQueueing makes the correction's delivery insert fail inside the verdict transaction: the
// storage failure the Python test injects by replacing delivery.enqueue_in. A TEMP trigger lives
// on the store's one connection only and never touches the file's schema.
func (h *hl) failQueueing() (restore func()) {
	h.t.Helper()
	_, err := h.store.DB.ExecContext(h.ctx, "CREATE TEMP TRIGGER fail_queueing BEFORE INSERT ON main.deliveries WHEN NEW.kind = 'revision_request' BEGIN SELECT RAISE(ABORT, 'storage failed while queueing the correction'); END")
	mustDo(h.t, err)
	return func() {
		_, err := h.store.DB.ExecContext(h.ctx, "DROP TRIGGER temp.fail_queueing")
		mustDo(h.t, err)
	}
}

// failAtCommit makes COMMIT itself fail (store.fault_hook raising): the verdict insert leaves a
// dangling deferred foreign key that SQLite checks only at COMMIT.
func (h *hl) failAtCommit() (restore func()) {
	h.t.Helper()
	for _, stmt := range []string{
		"CREATE TEMP TABLE commit_fault_parent (id INTEGER PRIMARY KEY)",
		"CREATE TEMP TABLE commit_fault_child (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES commit_fault_parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TEMP TRIGGER fail_at_commit AFTER INSERT ON main.verdicts BEGIN INSERT INTO commit_fault_child (parent) VALUES (-1); END",
	} {
		_, err := h.store.DB.ExecContext(h.ctx, stmt)
		mustDo(h.t, err)
	}
	return func() {
		for _, stmt := range []string{"DROP TRIGGER temp.fail_at_commit", "DROP TABLE temp.commit_fault_child", "DROP TABLE temp.commit_fault_parent"} {
			_, err := h.store.DB.ExecContext(h.ctx, stmt)
			mustDo(h.t, err)
		}
	}
}

func (h *hl) mustFail(_ any, err error) {
	h.t.Helper()
	if err == nil {
		h.t.Fatal("the injected storage fault did not fail the verdict")
	}
}

func Test21_ACR22_a_needs_changes_verdict_is_atomic(t *testing.T) {
	t.Run("normal path", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_the_normal_path_produces_a_generation_a_verdict_and_a_revision", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			_, err := h.verdict(event, "needs_changes", "v1")
			mustDo(t, err)
			h.eq(h.counts())
		})
	})
	t.Run("fault while queueing", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_a_storage_failure_while_queueing_the_correction_rolls_everything_back", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			before := h.counts()
			restore := h.failQueueing()
			h.mustFail(h.verdict(event, "needs_changes", "v1"))
			restore()
			h.eq(h.counts())
			h.eq(before[0])
		})
	})
	t.Run("fault then retry", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_a_storage_failure_leaves_the_verdict_retryable", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			restore := h.failQueueing()
			h.mustFail(h.verdict(event, "needs_changes", "v1"))
			restore()
			record, err := h.verdict(event, "needs_changes", "v1")
			mustDo(t, err)
			h.eq(field(record, "nextExecutionGeneration"))
			h.eq(h.counts())
		})
	})
	t.Run("fault at commit", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_a_fault_at_commit_time_also_rolls_back", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			restore := h.failAtCommit()
			h.mustFail(h.verdict(event, "needs_changes", "v1"))
			restore()
			h.eq(h.counts())
		})
	})
}

// contractOnly is the test's _contract: the verdict without the relay's internal markers.
func contractOnly(record Obj) Obj {
	out := Obj{}
	for _, f := range record {
		if len(f.Key) == 0 || f.Key[0] != '_' {
			out = append(out, f)
		}
	}
	return out
}

func Test21_ACR23_a_replayed_verdict_allocates_nothing_further(t *testing.T) {
	t.Run("sequential replay", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_a_replayed_verdict_allocates_nothing_further", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			first, err := h.verdict(event, "needs_changes", "v1")
			mustDo(t, err)
			again, err := h.verdict(event, "needs_changes", "v1")
			mustDo(t, err)
			requireSameJSON(t, "replayed contract", contractOnly(again), jsonable(contractOnly(first)))
			h.eq(contractOnly(first))
			h.eq(truthy(field(first, "_replay")))
			h.eq(truthy(field(again, "_replay")))
			h.eq(h.counts())
		})
	})
	t.Run("two concurrent writers", func(t *testing.T) {
		mirror(t, acr, "VerdictAtomicity.test_concurrent_replay_allocates_one_generation_and_one_revision", func(h *hl) {
			event := h.verdictAcknowledged([]string{parent, child})
			var (
				mu      sync.Mutex
				results []Obj
				errs    = []any{}
				ready   sync.WaitGroup
				start   = make(chan struct{})
				done    sync.WaitGroup
			)
			for i := 0; i < 2; i++ {
				ready.Add(1)
				done.Add(1)
				go func() {
					defer done.Done()
					s, err := store.Open(context.Background(), h.store.Path, "")
					if err != nil {
						ready.Done()
						mu.Lock()
						errs = append(errs, err.Error())
						mu.Unlock()
						return
					}
					defer func() { _ = s.Close() }()
					ack := NewAck(NewService(s, h.clock))
					ready.Done()
					<-start
					record, err := ack.RecordVerdict(context.Background(), event, "needs_changes", "v1", nil, nil, nil, nil)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err.Error())
						return
					}
					results = append(results, record)
				}()
			}
			ready.Wait()
			close(start)
			done.Wait()
			h.eq(errs)
			h.eq(len(results))
			if len(results) != 2 {
				t.Fatalf("both writers should succeed: %v", errs)
			}
			requireSameJSON(t, "the two contract records", contractOnly(results[1]), jsonable(contractOnly(results[0])))
			h.eq(contractOnly(results[0]))
			replays := []bool{truthy(field(results[0], "_replay")), truthy(field(results[1], "_replay"))}
			sort.Slice(replays, func(i, j int) bool { return !replays[i] && replays[j] })
			h.eq([]any{replays[0], replays[1]})
			h.eq(h.counts())
		})
	})
}
