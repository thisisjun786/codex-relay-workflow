package faults

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func faultTables(t *testing.T, l *Ledger) string {
	t.Helper()
	encoded, err := json.Marshal(testsupport.TableRows(t, l.Store.DB, "name LIKE 'fault_%'"))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func refusalText(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "transaction body: ")
}

// A fault command that refuses because it was given an id, class or owner the ledger does not
// know names the value as Go quotes it, and writes nothing: the refusal comes before the first
// write or rolls the command's transaction back, which is why the text is only returned.
func TestFaultRefusalsNameTheirValueWithGoQuotesAndWriteNothing(t *testing.T) {
	missingPublication := `fault_unknown: no publication "missing"`
	missingFault := `fault_unknown: no fault "missing"`
	cases := []struct {
		name string
		run  func(context.Context, *Ledger) error
		want string
	}{
		{"fault-show", func(ctx context.Context, l *Ledger) error {
			_, err := executeC(ctx, l, "fault-show", map[string]string{"--publication": "missing"})
			return err
		}, missingPublication},
		{"fault-cancel", func(ctx context.Context, l *Ledger) error {
			_, err := executeC(ctx, l, "fault-cancel", map[string]string{"--publication": "missing", "--reason": "r"})
			return err
		}, missingPublication},
		{"fault-claim", func(ctx context.Context, l *Ledger) error {
			_, err := executeF1(ctx, l, "fault-claim", map[string]string{"--publication": "missing", "--owner": "operator"})
			return err
		}, missingPublication},
		{"fault-operation", func(ctx context.Context, l *Ledger) error {
			_, err := executeF1(ctx, l, "fault-operation", map[string]string{"--publication": "missing", "--claim-token": "bad"})
			return err
		}, missingPublication},
		{"fault-reconcile", func(ctx context.Context, l *Ledger) error {
			_, err := executeF1(ctx, l, "fault-reconcile", map[string]string{"--publication": "missing"})
			return err
		}, missingPublication},
		{"fault-complete", func(ctx context.Context, l *Ledger) error {
			_, err := executeF1(ctx, l, "fault-complete", map[string]string{"--publication": "missing"})
			return err
		}, missingPublication},
		{"fault-fail", func(ctx context.Context, l *Ledger) error {
			_, err := executeF2(ctx, l, "fault-fail", map[string]string{"--publication": "missing", "--claim-token": "token", "--error": "failed"})
			return err
		}, missingPublication},
		{"fault-move", func(ctx context.Context, l *Ledger) error {
			_, err := executeF2(ctx, l, "fault-move", map[string]string{"--fault": "missing", "--scope": `{"projectKey":"P"}`})
			return err
		}, missingFault},
		{"fault-notification-raise", func(ctx context.Context, l *Ledger) error {
			_, err := executeD(ctx, l, "fault-notification-raise", map[string]string{"--fault": "missing", "--reason": "test"})
			return err
		}, missingFault},
		{"fault-policy", func(ctx context.Context, l *Ledger) error {
			_, err := executeD(ctx, l, "fault-policy", map[string]string{"--product": "crw", "--fault-class": "not_registered", "--severity": "degraded", "--reason": "unknown"})
			return err
		}, `fault_class_unregistered: "not_registered" is not a registered fault class, so nothing declares what would clear it`},
		{"record", func(ctx context.Context, l *Ledger) error {
			_, err := l.Record(ctx, Observation{Product: "crw", FaultClass: "nope", Severity: Broken, Signature: map[string]any{"turn": "t"}, OccurrenceKey: "k"})
			return err
		}, `fault_class_unregistered: "nope" is not a registered fault class, so nothing declares what would clear it`},
		{"fault-notification-reserve", func(ctx context.Context, l *Ledger) error {
			_, err := executeD(ctx, l, "fault-notification-reserve", map[string]string{"--owner": "relay-daemon"})
			return err
		}, `fault_observation_malformed: owner "relay-daemon" is the relay daemon's notification deliverer's: its reservations are settled from the supervisor channel's records, so a reserver with a transport of its own names itself`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, ctx := testLedger(t)
			// An unrelated fault and its queued publication are already there, so a refusal that
			// touched any existing row would show in the snapshot too.
			if recorded, err := l.Record(ctx, testObservation("seed", "report_omitted", Broken)); err != nil || !recorded {
				t.Fatalf("seed: %v %v", recorded, err)
			}
			before := faultTables(t, l)
			err := c.run(ctx, l)
			if got := refusalText(err); got != c.want {
				t.Fatalf("refusal %q, want %q", got, c.want)
			}
			if after := faultTables(t, l); after != before {
				t.Fatalf("a refused command wrote to the fault tables:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// A publication block that repeats a header reports the repeat with Go quotes; the answer that
// carries it is returned by fault-reconcile and, in fault-complete, wrapped in a refusal before any
// write.
func TestDuplicateHeaderProblemNamesTheHeaderWithGoQuotes(t *testing.T) {
	block := "<!-- relay-fault:p1 -->\nissue: A\nissue: B\n<!-- /relay-fault:p1 -->"
	found := f1ReadBlock(block, "p1")
	if len(found.problems) == 0 || found.problems[0] != `duplicate header "issue"` {
		t.Fatalf("problems %v", found.problems)
	}
}

// The adoption and scope conflicts of a fault are caught by routing, which stores their text as
// the route's reason and incident_routes.detail (routing/intake.go and routing/reconcile.go match
// the reason words and keep the whole text). They are not changed by the Python-spelling cleanup,
// and this pins that: a later sweep that rewrites them must first decide what the stored rows hold.
func TestStoredFaultConflictsKeepTheirPythonSpelling(t *testing.T) {
	// A fault that files (broken) takes the adopted issue as its own at once.
	t.Run("owned issue", func(t *testing.T) {
		l, ctx := testLedger(t)
		o := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "f4-owned"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}
		if recorded, err := l.Record(ctx, o); err != nil || !recorded {
			t.Fatalf("record: %v %v", recorded, err)
		}
		id := FaultID("crw", "report_omitted", o.Signature)
		if _, err := executeF2(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": "CRW-1", "--scope": `{"projectKey":"P"}`}); err != nil {
			t.Fatal(err)
		}
		_, err := executeF2(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": "CRW-2", "--scope": `{"projectKey":"P"}`})
		if want := "fault_adopt_conflict: this fault already owns 'CRW-1'"; refusalText(err) != want {
			t.Fatalf("refusal %q, want %q", refusalText(err), want)
		}
	})
	// A fault that never files (notice) only records the adoption, still pending, so the second
	// issue meets the stored adoption and not an owned issue.
	t.Run("pending adoption", func(t *testing.T) {
		l, ctx := testLedger(t)
		o := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Notice, Signature: map[string]any{"turn": "f4-pending"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}
		if recorded, err := l.Record(ctx, o); err != nil || !recorded {
			t.Fatalf("record: %v %v", recorded, err)
		}
		id := FaultID("crw", "report_omitted", o.Signature)
		if _, err := executeF2(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": "CRW-1", "--scope": `{"projectKey":"P"}`}); err != nil {
			t.Fatal(err)
		}
		stored, err := l.Store.One(ctx, "SELECT state FROM fault_adoptions WHERE fault_id = ?", id)
		if err != nil || stored == nil || stored.Text("state") != "pending" {
			t.Fatalf("the adoption of a notice stays pending: %v %v", stored, err)
		}
		_, err = executeF2(ctx, l, "fault-adopt", map[string]string{"--fault": id, "--external-ref": "CRW-2", "--scope": `{"projectKey":"P"}`})
		if want := "fault_adopt_conflict: this fault already adopts 'CRW-1'"; refusalText(err) != want {
			t.Fatalf("refusal %q, want %q", refusalText(err), want)
		}
	})
	t.Run("workspace", func(t *testing.T) {
		l, ctx := testLedger(t)
		signature := map[string]any{"turn": "f3"}
		for _, o := range []Observation{
			{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: signature, OccurrenceKey: "a", Scope: map[string]any{"workspace": "w1"}},
			{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: signature, OccurrenceKey: "b", Scope: map[string]any{"workspace": "unassigned"}},
		} {
			if recorded, err := l.Record(ctx, o); err != nil || !recorded {
				t.Fatalf("record: %v %v", recorded, err)
			}
		}
		unassigned := FaultIDInWorkspace("crw", "report_omitted", signature, "unassigned")
		_, err := executeF2(ctx, l, "fault-move", map[string]string{"--fault": unassigned, "--scope": `{"workspace":"w1"}`})
		if !regexp.MustCompile(`^fault_scope_conflict: workspace 'w1' already records this failure as fault [0-9a-f]{32};`).MatchString(refusalText(err)) {
			t.Fatalf("refusal %q", refusalText(err))
		}
	})
}
