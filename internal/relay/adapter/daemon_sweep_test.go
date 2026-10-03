package adapter

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-304: the fault sweep the daemon runs files report_omitted for a settled managed turn only while the
// turn still owes its report. The sweep is the one daemonFactory builds, over a store and a marker tree in a
// temporary directory. A turn that is already past (a later turn was admitted to the generation, or the turn
// has its own final receipt) owes nothing; the same turn with neither is an omission.

// daemonSweepWorld seeds one attached managed child whose turn-9 ended without a report (the Stop hook
// witnessed the end), then adds what makes the turn past: a later admitted turn (past is
// "later_turn_admitted"), a final receipt of its own ("own_receipt"), or nothing (""). It builds the daemon
// over that store with daemonFactory, as the relay service does, and returns the sweeper and fault ledger
// the daemon would sweep and record with, and the store. State, markers and workspace live in a temporary
// directory, and the process environment is pointed there, so no real relay state is read.
func daemonSweepWorld(t *testing.T, ctx context.Context, past string) (*faults.Sweeper, *faults.Ledger, *store.Store) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state, root, work := filepath.Join(dir, "S"), filepath.Join(dir, "markers"), filepath.Join(dir, "work")
	for name, value := range map[string]string{"CODEX_SESSION_RELAY_STATE": state, "HOME": dir + "/home", "XDG_STATE_HOME": dir + "/xs", "XDG_CONFIG_HOME": dir + "/xc", "CODEX_HOME": dir + "/ch"} {
		t.Setenv(name, value)
	}
	if err = os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seed := []string{
		"INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('rel','ISSUE','active','parent','host','child','host','" + work + "',1,'[]','[]','stamp','stamp')",
		"INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at) VALUES('rel',1,'dispatch-1','bound','turn-9','stamp')",
		"INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,child_task_id,standby_turn_id,relationship_id,execution_generation,receipt_status,created_at,updated_at) VALUES('req-1','ISSUE','fp','v1','" + work + "','" + root + "','sock','create-1','dispatch-1','attached',3,'child','standby-1','rel',1,'accepted','stamp','stamp')",
		"INSERT INTO assignment_settlements VALUES('rel','child','standby-1','completed','stamp')",
		"INSERT INTO assignment_settlements VALUES('rel','child','turn-9','completed','stamp')",
	}
	switch past {
	case "later_turn_admitted":
		seed = append(seed, "INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('rel',1,'turn-10','explicit_admission_bound:turn-9','child','admitted','stamp')")
	case "own_receipt":
		seed = append(seed, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES('ev-1','rel',1,'x','ready_for_review','child','child','turn-9','completed','{}','final','stamp','stamp')")
	}
	for _, query := range seed {
		if _, err = s.Q(ctx).ExecContext(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	assignment := delivery.AssignmentID("dispatch-1")
	marker, err := delivery.AssignmentDir(root, work, assignment)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]map[string]any{
		"intent.json":              {"dispatchRequestIdHash": assignment, "workspace": work, "dbPath": filepath.Join(state, "relay.sqlite3"), "issueKey": "ISSUE"},
		"claims/child/claim.json":  {"sessionId": "child", "dispatchRequestId": "dispatch-1"},
		"bound.json":               {"sessionId": "child", "taskId": "child"},
		"relationship.json":        {"relationshipId": "rel", "executionGeneration": 1},
		"hook/child/turn-9/1.json": {"sessionId": "child", "turnId": "turn-9", "observation": "undeclared_turn_end", "decisionState": "unresolved_handoff", "at": "1970-01-02T03:46:40+00:00"},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(marker, name)
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := daemonFactory(ctx, dispatch.Services{Selection: store.StateSelection{Path: state}, SocketPath: filepath.Join(dir, "app.sock")}, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if host, ok := d.Host.(io.Closer); ok {
			_ = host.Close()
		}
	})
	return d.Sweeper, d.Faults, s
}

// recordingObserver passes every request to the observer the daemon installed and keeps what it answered,
// so a test can say that a turn was judged, and how, and not only that nothing was filed for it.
type recordingObserver struct {
	faults.ManagedReadingObserver
	readings []map[string]any
}

func (r *recordingObserver) Observe(ctx context.Context, request faults.ManagedReadingRequest) (any, error) {
	reading, err := r.ManagedReadingObserver.Observe(ctx, request)
	if object, ok := reading.(map[string]any); ok {
		r.readings = append(r.readings, object)
	}
	return reading, err
}

func TestDaemonSweepFilesAnOmissionOnlyForATurnThatStillOwesItsReport(t *testing.T) {
	for _, c := range []struct {
		name, past string
		owedReason string
		owed       bool
	}{
		{"a turn that still owes its report", "", "terminal_without_report", true},
		{"a later turn was admitted", "later_turn_admitted", "later_turn_admitted", false},
		{"the turn has its own final receipt", "own_receipt", "turn_receipted", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			sweeper, ledger, s := daemonSweepWorld(t, ctx, c.past)
			// The daemon reads its settled managed turns through delivery's omission judgment.
			if _, ok := sweeper.ManagedObserver.(supervisor.OmissionObserver); !ok {
				t.Fatalf("the daemon's sweeper reads managed turns through %T, want supervisor.OmissionObserver", sweeper.ManagedObserver)
			}
			recorder := &recordingObserver{ManagedReadingObserver: sweeper.ManagedObserver}
			sweeper.ManagedObserver = recorder
			batch, err := sweeper.Sweep(ctx, "crw")
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Gaps) != 0 {
				t.Fatalf("the sweep reports gaps: %v", batch.Gaps)
			}
			// Turn-9 is the one settled turn the sweep reads (the standby turn is skipped), and it was judged: it
			// ended without a report, and delivery says whether a report is still owed for it.
			if len(recorder.readings) != 1 {
				t.Fatalf("the sweep read %d managed turns, want 1: %v", len(recorder.readings), recorder.readings)
			}
			reading := recorder.readings[0]
			if selectors, _ := reading["selectors"].(map[string]any); selectors["turn"] != "turn-9" || reading["reportingState"] != "unreported" || reading["owed"] != c.owed || reading["owedReason"] != c.owedReason {
				t.Fatalf("turn-9 reads %v / owed %v (%v), want unreported / owed %v (%s)", reading["reportingState"], reading["owed"], reading["owedReason"], c.owed, c.owedReason)
			}
			want := 0
			if c.owed {
				want = 1
			}
			filed := 0
			for _, o := range batch.Observations {
				if o.Signature["turn"] != "turn-9" || o.Cleared {
					continue
				}
				switch o.FaultClass {
				case "report_omitted":
					filed++
				case "observation_unmeasured":
					t.Fatalf("the turn was left unmeasured: %s", o.Detail)
				}
			}
			if filed != want {
				t.Fatalf("the sweep files %d report_omitted observations for turn-9, want %d", filed, want)
			}
			if _, err = sweeper.RecordAll(ctx, ledger, batch); err != nil {
				t.Fatal(err)
			}
			rows, err := s.All(ctx, "SELECT fault_id FROM fault_ledger WHERE fault_class='report_omitted'")
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != want {
				t.Fatalf("the fault ledger holds %d report_omitted faults, want %d", len(rows), want)
			}
		})
	}
}
