package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-300: what the daemon's tick costs for relationships whose turn declared ready_for_review.
//
// The world is the standing world of PR #319 (a project, a supervisor above it, one relationship per
// issue) with what omission derivation reads added to each relationship: a generation opened by a
// dispatch request, the claim of a reporting session, the turn's declaration (ready_for_review), the
// relay's completed settlement of the turn, and a receipt event over a real artifact with a frozen copy.
// Two states of that receipt event are built:
//
//   - receipted: the receipt is FINAL (the daemon promotes it with the settlement). The turn has
//     reported, so nothing is owed through an omission, and the completion the event raises has gone up.
//   - owed: the receipt is STAGED after the settlement and the live artifact changed since it was
//     written, with no usable frozen copy. This is the state an offline emit leaves when it arrives after
//     the daemon settled the turn (delivery/cli_emit.go takes an inProgress claim; store/receipt_intake.go
//     stores it staged without looking at the settlement) until the daemon next reads the turn. The turn
//     has no final child event, so the omission is owed, and judging it needs the receipt.
//
// omissionNow is far after the settlements, so the grace has passed.

const omissionNow = 1_800_000_000.0

type omissionWorld struct {
	*standingWorld
	artifacts []string
	frozen    []string
	owed      map[string]bool
}

// newOmissionWorld seeds relationships relationships, the first owed of them in the owed state, and
// stages what the project owes (see prepare).
func newOmissionWorld(tb testing.TB, relationships, owed int) *omissionWorld {
	tb.Helper()
	w := seedOmissionWorld(tb, relationships, owed)
	w.prepare()
	return w
}

// seedOmissionWorld is newOmissionWorld without the first visit: nothing is staged.
func seedOmissionWorld(tb testing.TB, relationships, owed int) *omissionWorld {
	tb.Helper()
	base := newStandingWorld(tb, relationships, 1)
	w := &omissionWorld{standingWorld: base, owed: map[string]bool{}}
	root, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	const settled = "2026-09-30T00:00:00.000000+00:00"
	for i, rid := range w.rels {
		work := filepath.Join(root, "work", rid)
		if err := os.MkdirAll(work, 0o700); err != nil {
			tb.Fatal(err)
		}
		artifact := filepath.Join(work, "deliver.txt")
		if err := os.WriteFile(artifact, []byte(strings.Repeat("the delivered bytes of "+rid+"\n", 200)), 0o600); err != nil {
			tb.Fatal(err)
		}
		entries, err := store.BuildManifest([]string{artifact}, []string{work})
		if err != nil {
			tb.Fatal(err)
		}
		revision, err := store.ManifestRevision(entries)
		if err != nil {
			tb.Fatal(err)
		}
		frozen := filepath.Join(root, "frozen", rid)
		if err := store.FreezeManifest(entries, frozen); err != nil {
			tb.Fatal(err)
		}
		receipt, err := json.Marshal(map[string]any{
			"manifest":     []map[string]any{{"path": entries[0].Path, "sha256": entries[0].SHA256, "bytes": *entries[0].Bytes}},
			"revisionHash": revision,
		})
		if err != nil {
			tb.Fatal(err)
		}
		roots, _ := json.Marshal([]string{work})
		dispatch, turn, child, event, issue := fmt.Sprintf("dispatch-%04d", i), fmt.Sprintf("turn-%04d", i), fmt.Sprintf("child-%04d", i), fmt.Sprintf("ev-%04d-000", i), fmt.Sprintf("ISS-%04d", i)
		assignment := delivery.AssignmentID(dispatch)
		w.exec("UPDATE relationships SET child_cwd=?, artifact_roots=? WHERE relationship_id=?", work, string(roots), rid)
		w.exec("INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,1,?,'bound',?,NULL,?,?)", rid, dispatch, turn, settled, settled)
		w.exec("INSERT INTO reporting_sessions (assignment_id,session_id,dispatch_request_id,marker_root,workspace,issue_key,capability,recorded_at) VALUES (?,?,?,?,?,?,'declarations/1',?)", assignment, child, dispatch, filepath.Join(root, "markers"), work, issue, settled)
		w.exec("INSERT INTO turn_declarations (assignment_id,session_id,turn_id,outcome,declared_at,recorded_at) VALUES (?,?,?,'ready_for_review',?,?)", assignment, child, turn, settled, settled)
		w.exec("INSERT INTO assignment_settlements (relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES (?,?,?,'completed',?)", rid, child, turn, settled)
		w.exec("UPDATE events SET turn_thread_id=?, turn_id=?, revision_hash=?, receipt=?, manifest_ref=? WHERE event_id=?", child, turn, revision, string(receipt), frozen, event)
		w.exec("UPDATE work_reports SET revision_hash=? WHERE event_id=?", revision, event)
		w.artifacts = append(w.artifacts, artifact)
		w.frozen = append(w.frozen, frozen)
		if i < owed {
			w.owed[rid] = true
			w.exec("UPDATE events SET stage='staged', staged_at=?, turn_status='inProgress', manifest_ref=NULL WHERE event_id=?", settled, event)
			if err := os.WriteFile(artifact, []byte("the artifact changed after its receipt\n"), 0o600); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return w
}

// prepare runs the first visit, which stages what the project owes (the completion of every receipted
// relationship and the omission of every owed one), and marks every message dispatched: the state of a
// project whose reports have gone up, which every later tick only reads.
func (w *omissionWorld) prepare() {
	w.tb.Helper()
	answer, err := w.c.StageUnsent(w.ctx, worldProject, delivery.ISOOf(omissionNow), 300)
	if err != nil {
		w.tb.Fatal(err)
	}
	if staged := answer["staged"].([]any); len(staged) != len(w.rels) {
		w.tb.Fatalf("staged %d of %d reports: refused %v", len(staged), len(w.rels), answer["refused"])
	}
	w.exec("UPDATE supervisor_messages SET state='dispatched'")
}

// tick is one daemon tick of the supervisor pass.
func (w *omissionWorld) tick(ctx context.Context) AutoSendResult {
	w.tb.Helper()
	result, err := w.c.AutoSend(ctx, &sendHost{status: "idle"}, omissionNow, 4, 2, "", "", "")
	if err != nil {
		w.tb.Fatal(err)
	}
	return result
}

// dump is every row of the tables a tick could write (and of the ones it reads), in rowid order.
func (w *omissionWorld) dump() string {
	w.tb.Helper()
	var out strings.Builder
	for _, table := range []string{"supervisor_messages", "supervisor_attempts", "supervisor_readbacks", "journal", "events", "relationships", "generations", "work_reports", "assignment_settlements", "turn_declarations", "reporting_sessions"} {
		rows, err := w.s.DB.QueryContext(w.ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			w.tb.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				w.tb.Fatal(err)
			}
			fmt.Fprintf(&out, "%s %q\n", table, values)
		}
		if err := rows.Err(); err != nil {
			w.tb.Fatal(err)
		}
		_ = rows.Close()
	}
	return out.String()
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// normalizedJSON is a JSON document as plain values, so two spellings of one document compare equal.
func normalizedJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// A tick over relationships whose turn already reported has nothing to judge about a receipt: the
// state (a final child receipt of the turn) says nothing is owed, so no artifact is read.
func TestCRW300ReceiptedTickReadsNoArtifact(t *testing.T) {
	const relationships = 6
	w := newOmissionWorld(t, relationships, 0)
	ctx, reads := store.WithArtifactReads(w.ctx)
	w.tick(ctx)
	if got := reads.Count(); got != 0 {
		t.Errorf("a tick over %d relationships whose ready_for_review turn already reported read %d artifacts, want 0", relationships, got)
	}
}

// The watch the tick test leans on sees a second open of the database file through the read-only path,
// through the driver registered as "sqlite", and through a reconnect of the store's own pool.
func TestCRW300DatabaseOpensAreSeen(t *testing.T) {
	w := newOmissionWorld(t, 1, 0)
	opens, watching := watchDatabaseOpens(t, filepath.Dir(w.s.Path))
	if !watching {
		t.Skip("no inotify to watch the database file with")
	}
	if got := opens(); got != 0 {
		t.Fatalf("%d opens were seen before anything opened the database", got)
	}
	ro, err := store.OpenReadOnly(w.ctx, w.s.Path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	if opens() == 0 {
		t.Error("store.OpenReadOnly (the path LookupStoredReceiptAt takes) was not seen")
	}
	other, err := sql.Open("sqlite", "file:"+w.s.Path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Ping(); err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	if opens() == 0 {
		t.Error("a connection opened with sql.Open(\"sqlite\") was not seen")
	}
	w.s.DB.SetMaxIdleConns(0)
	for i := 0; i < 2; i++ {
		if _, err := w.s.DB.ExecContext(w.ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	if opens() == 0 {
		t.Error("a reconnect of the store's pool was not seen")
	}
}

// The artifact read counter counts one read for one artifact, wherever the test runs.
func TestCRW300ArtifactReadsAreCounted(t *testing.T) {
	w := newOmissionWorld(t, 1, 0)
	ctx, reads := store.WithArtifactReads(w.ctx)
	if _, _, _, err := store.HashArtifactContext(ctx, w.artifacts[0], []string{filepath.Dir(w.artifacts[0])}, false); err != nil {
		t.Fatal(err)
	}
	if reads.Count() != 1 {
		t.Errorf("one artifact read was counted as %d", reads.Count())
	}
}

// The tick reads on the store's own connection, and a tick over a settled project writes nothing.
func TestCRW300TickOpensNoConnectionAndChangesNoRow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owed  int
		total int
	}{{"receipted", 0, 6}, {"mixed", 2, 6}} {
		t.Run(tc.name, func(t *testing.T) {
			w := newOmissionWorld(t, tc.total, tc.owed)
			before := w.dump()
			opens, watching := watchDatabaseOpens(t, filepath.Dir(w.s.Path))
			result := w.tick(w.ctx)
			if watching {
				if got := opens(); got != 0 {
					t.Errorf("the tick opened the database file %d times: it reads on the store's own connection", got)
				}
			}
			if result.SupervisorStaged != 0 || result.SupervisorSent != 0 || result.Deferred != 0 || result.Attempts != 0 {
				t.Errorf("the tick over a settled project did work: %+v", result)
			}
			if after := w.dump(); after != before {
				t.Errorf("the tick changed rows:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A relationship whose omission is owed is still judged against its receipt: the lookup is what finds
// the changed artifact, so the state-first skip must leave it in place. Every message the first visit
// staged is an omission whose frozen reading carries the receipt's answer, and each tick reads the
// artifact of each owed relationship once.
func TestCRW300OwedOmissionStillReadsItsReceipt(t *testing.T) {
	const relationships = 3
	w := newOmissionWorld(t, relationships, relationships)
	rows, err := w.s.DB.QueryContext(w.ctx, "SELECT obligation_kind, reading FROM supervisor_messages ORDER BY message_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var kind string
		var reading sql.NullString
		if err := rows.Scan(&kind, &reading); err != nil {
			t.Fatal(err)
		}
		count++
		if kind != "unreported" || !strings.Contains(reading.String, "artifacts_changed_since_receipt") {
			t.Errorf("staged %q with reading %s, want an omission whose receipt answer is artifacts_changed_since_receipt", kind, reading.String)
		}
	}
	if count != relationships {
		t.Errorf("staged %d messages, want %d", count, relationships)
	}
	// The reading frozen on each message is the whole derivation's, field for field.
	for _, rid := range w.rels {
		var frozen string
		if err := w.s.DB.QueryRowContext(w.ctx, "SELECT reading FROM supervisor_messages WHERE relationship_id=? AND obligation_kind='unreported'", rid).Scan(&frozen); err != nil {
			t.Fatal(err)
		}
		whole := orderedMap(delivery.DeriveOmission(w.ctx, w.s, store.PathlibParent(w.s.Path), rid, "", delivery.ISOOf(omissionNow), 300))
		if staged, derived := normalizedJSON(t, []byte(frozen)), normalizedJSON(t, mustMarshal(t, whole)); !reflect.DeepEqual(staged, derived) {
			t.Errorf("%s: the staged reading is\n%v\nthe whole derivation reads\n%v", rid, staged, derived)
		}
	}
	ctx, reads := store.WithArtifactReads(w.ctx)
	w.tick(ctx)
	if got := reads.Count(); got != relationships {
		t.Errorf("a tick over %d owed relationships read %d artifacts, want one each", relationships, got)
	}
}

// BenchmarkOmissionTick is one daemon tick over a project of relationships ready_for_review relationships:
// receipted (every turn has reported) and mixed (one in eight is in the owed state). It reports the
// statements sent and the artifacts read per tick besides the time, because the time on a shared host
// is noisy and the counts are not.
//
//	go test ./internal/relay/supervisor -run '^$' -bench OmissionTick -benchtime 5x -count 3
func BenchmarkOmissionTick(b *testing.B) {
	for _, relationships := range []int{8, 32, 128} {
		for _, scenario := range []struct {
			name string
			owed int
		}{{"receipted", 0}, {"mixed", relationships / 8}} {
			w := newOmissionWorld(b, relationships, scenario.owed)
			b.Run(fmt.Sprintf("rel=%d/%s", relationships, scenario.name), func(b *testing.B) {
				counter := countStatements(b, w.s)
				var artifactReads int64
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					ctx, reads := store.WithArtifactReads(w.ctx)
					w.tick(ctx)
					artifactReads += reads.Count()
				}
				b.StopTimer()
				b.ReportMetric(float64(counter.count())/float64(b.N), "stmts/op")
				b.ReportMetric(float64(artifactReads)/float64(b.N), "reads/op")
			})
		}
	}
}
