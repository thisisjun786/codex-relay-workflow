package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type testClock struct{ now float64 }

func (c *testClock) Now() float64 { return c.now }
func (c *testClock) ISO() string {
	return time.UnixMicro(int64(c.now * 1e6)).UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
func testLedger(t *testing.T) (*Ledger, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Ledger{Store: s, Clock: &testClock{now: 100000}}, ctx
}
func testObservation(key, class, severity string) Observation {
	return Observation{Product: "crw", FaultClass: class, Severity: severity, Signature: map[string]any{"relationship": "rel-1", "turn": "turn-7"}, OccurrenceKey: key, Scope: map[string]any{"projectKey": "CRW", "issueKey": "CRW-205"}, Detail: "an admitted turn settled without a report", Evidence: []any{map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"rows": 0}}}}
}
func count(t *testing.T, l *Ledger, ctx context.Context, table string) int64 {
	t.Helper()
	r, e := l.Store.One(ctx, "SELECT COUNT(*) AS n FROM "+table)
	if e != nil {
		t.Fatal(e)
	}
	return integer(r, "n")
}
func state(t *testing.T, l *Ledger, ctx context.Context, id string) string {
	t.Helper()
	r, e := l.Store.One(ctx, "SELECT state FROM fault_ledger WHERE fault_id = ?", id)
	if e != nil {
		t.Fatal(e)
	}
	return text(r, "state")
}
func record(t *testing.T, l *Ledger, ctx context.Context, o Observation) bool {
	t.Helper()
	b, e := l.Record(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func Test22_FLT_1_IdentityConverges(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	if !record(t, l, c, o) || record(t, l, c, o) {
		t.Fatal("duplicate recorded")
	}
	if count(t, l, c, "fault_ledger") != 1 || count(t, l, c, "fault_occurrences") != 1 {
		t.Fatal("duplicate rows")
	}
	o.OccurrenceKey = "b"
	record(t, l, c, o)
	if count(t, l, c, "fault_occurrences") != 2 {
		t.Fatal("second delivery missing")
	}
	if FaultID("crw", "report_omitted", map[string]any{"a": 1, "b": 2}) != FaultID("crw", "report_omitted", map[string]any{"b": 2, "a": 1}) {
		t.Fatal("signature order changed identity")
	}
	second, e := store.Open(c, l.Store.Path, "")
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	again := &Ledger{Store: second, Clock: l.Clock}
	if record(t, again, c, o) {
		t.Fatal("restart duplicated")
	}
}
func Test22_FLT_2_Registry(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "invented", Broken)
	if _, e := l.Record(c, o); e == nil || e.Error()[:24] != "fault_class_unregistered" {
		t.Fatalf("wrong refusal: %v", e)
	}
	raw, e := os.ReadFile(filepath.Join("..", "..", "..", "contract", "schema", "fault-kinds.json"))
	if e != nil {
		t.Fatal(e)
	}
	var manifest struct {
		Classes map[string]struct{ Component, Clears string }
		Kinds   map[string]struct {
			Name          string  `json:"name"`
			Creates       bool    `json:"creates"`
			RequiresIssue bool    `json:"requires_issue"`
			Target        *string `json:"target"`
			Evidence      string  `json:"evidence"`
		}
	}
	if e = json.Unmarshal(raw, &manifest); e != nil {
		t.Fatal(e)
	}
	if len(classes) != len(manifest.Classes) {
		t.Fatalf("registered %d of %d classes", len(classes), len(manifest.Classes))
	}
	for name, policy := range manifest.Classes {
		got, ok := classes[name]
		if !ok || got.component != policy.Component || got.clears != policy.Clears || got.clears == "" {
			t.Fatalf("class %s: %+v", name, got)
		}
	}
	if len(kinds) != len(manifest.Kinds) {
		t.Fatalf("registered %d of %d kinds", len(kinds), len(manifest.Kinds))
	}
	for name, policy := range manifest.Kinds {
		got, ok := kinds[name]
		target := ""
		if policy.Target != nil {
			target = *policy.Target
		}
		if !ok || policy.Name != name || got.Creates != policy.Creates || got.RequiresIssue != policy.RequiresIssue || got.Target != target || got.Evidence != policy.Evidence || got.Creates && got.Evidence != "block" {
			t.Fatalf("kind %s: %+v", name, got)
		}
	}
}
func Test22_FLT_13_WorkspaceIdentity(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	o.Scope["workspace"] = "/one"
	record(t, l, c, o)
	one := FaultIDInWorkspace("crw", o.FaultClass, o.Signature, "/one")
	if state(t, l, c, one) != Open {
		t.Fatal("workspace id not recorded")
	}
	o.Scope["workspace"] = "/two"
	record(t, l, c, o)
	two := FaultIDInWorkspace("crw", o.FaultClass, o.Signature, "/two")
	if two == one || count(t, l, c, "fault_ledger") != 2 {
		t.Fatal("workspaces merged")
	}
	if got := scopeKeyFor("crw", map[string]any{"workspace": "/one", "projectKey": "CRW"}); got != "ws|crw|/one|CRW" {
		t.Fatalf("scope key %s", got)
	}
}
func Test22_FLT_19_OccurrencesOncePerFact(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("delivery:one", "delivery_stalled", Degraded)
	if !record(t, l, c, o) || record(t, l, c, o) {
		t.Fatal("same attempt counted twice")
	}
	o.OccurrenceKey = "delivery:two"
	record(t, l, c, o)
	if count(t, l, c, "fault_ledger") != 1 || count(t, l, c, "fault_occurrences") != 2 {
		t.Fatal("two deliveries did not converge")
	}
	o.Cleared = true
	o.OccurrenceKey = "clear"
	record(t, l, c, o)
	if record(t, l, c, o) {
		t.Fatal("duplicate clear reopened episode")
	}
	id := FaultID("crw", o.FaultClass, o.Signature)
	r, e := l.Store.One(c, "SELECT occurrence_count FROM fault_ledger WHERE fault_id = ?", id)
	if e != nil || integer(r, "occurrence_count") != 2 {
		t.Fatalf("clear changed count: %v %+v", e, r)
	}
}
func Test22_FLT_23_ResolutionRequiresCurrentPassingCheck(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	record(t, l, c, o)
	id := FaultID("crw", o.FaultClass, o.Signature)
	if _, _, err := l.Remediate(c, id, "fix", "PR #1", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Resolve(c, id); err == nil || !strings.Contains(err.Error(), "fault_unverified") {
		t.Fatalf("fix alone resolved: %v", err)
	}
	if _, _, err := l.Remediate(c, id, "reverification", "check", "suite", "failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Resolve(c, id); err == nil || !strings.Contains(err.Error(), "fault_unverified") {
		t.Fatalf("failed check resolved: %v", err)
	}
	if _, _, err := l.Remediate(c, id, "reverification", "check", "suite", "passed"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Remediate(c, id, "reverification", "check", "suite", "invalid"); err == nil || !strings.Contains(err.Error(), "fault_observation_malformed") {
		t.Fatalf("invalid outcome accepted: %v", err)
	}
	o.OccurrenceKey = "b"
	record(t, l, c, o)
	if _, err := l.Resolve(c, id); err == nil || !strings.Contains(err.Error(), "fault_state_conflict") {
		t.Fatalf("recurrence resolved: %v", err)
	}
}
func Test22_FLT_24_WithdrawnFaultReopensInSameRecord(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("raise", "observation_unmeasured", Notice)
	id := FaultID("crw", o.FaultClass, o.Signature)
	record(t, l, c, o)
	o.Cleared = true
	o.OccurrenceKey = "clear"
	record(t, l, c, o)
	if state(t, l, c, id) != Withdrawn {
		t.Fatal("clear did not withdraw")
	}
	o.Cleared = false
	o.OccurrenceKey = "raise"
	if !record(t, l, c, o) || state(t, l, c, id) != Observed || count(t, l, c, "fault_ledger") != 1 {
		t.Fatal("recurrence did not reopen original fault")
	}
	r, e := l.Store.One(c, "SELECT cycle,reopen_count,occurrence_count FROM fault_ledger WHERE fault_id = ?", id)
	if e != nil {
		t.Fatal(e)
	}
	if integer(r, "occurrence_count") != 2 {
		t.Fatal("recurrence not counted once")
	}
}
func Test22_FLT_3_EvidenceBoundedByValue(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	original := map[string]any{"kind": "row", "ref": "events", "observed": map[string]any{"state": "failed"}}
	o.Evidence = []any{original}
	record(t, l, c, o)
	original["observed"].(map[string]any)["state"] = "changed"
	r, e := l.Store.One(c, "SELECT evidence,evidence_digest FROM fault_occurrences LIMIT 1")
	if e != nil {
		t.Fatal(e)
	}
	items := loadsMap("{\"items\":" + text(r, "evidence") + "}")["items"].([]any)
	if items[0].(map[string]any)["observed"].(map[string]any)["state"] != "failed" || evidenceDigest(items) != text(r, "evidence_digest") {
		t.Fatal("evidence not stored by value with its digest")
	}
	o.OccurrenceKey = "b"
	o.Evidence = make([]any, 13)
	for i := range o.Evidence {
		o.Evidence[i] = map[string]any{"ref": "e"}
	}
	record(t, l, c, o)
	r, e = l.Store.One(c, "SELECT evidence,truncated FROM fault_occurrences ORDER BY rowid DESC LIMIT 1")
	if e != nil {
		t.Fatal(e)
	}
	items = loadsMap("{\"items\":" + text(r, "evidence") + "}")["items"].([]any)
	if len(items) != 8 || integer(r, "truncated") != 1 {
		t.Fatal("evidence bound failed")
	}
}
func Test22_FLT_4_Suppression(t *testing.T) {
	l, c := testLedger(t)
	broken := testObservation("a", "report_omitted", Broken)
	record(t, l, c, broken)
	id := FaultID("crw", broken.FaultClass, broken.Signature)
	if state(t, l, c, id) != Open || count(t, l, c, "fault_publications") != 1 {
		t.Fatal("broken not filed once")
	}
	d := testObservation("1", "delivery_stalled", Degraded)
	for i, key := range []string{"1", "2", "3", "4"} {
		d.OccurrenceKey = key
		record(t, l, c, d)
		want := Observed
		if i >= 2 {
			want = Open
		}
		if state(t, l, c, FaultID("crw", d.FaultClass, d.Signature)) != want {
			t.Fatalf("threshold at %d", i)
		}
	}
	if count(t, l, c, "fault_publications") != 2 {
		t.Fatal("duplicate filing")
	}
	n := testObservation("notice", "observation_unmeasured", Notice)
	record(t, l, c, n)
	if state(t, l, c, FaultID("crw", n.FaultClass, n.Signature)) != Observed {
		t.Fatal("notice filed")
	}
}
func Test22_FLT_5_FixDoesNotSuppressFiling(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("1", "delivery_stalled", Degraded)
	id := FaultID("crw", o.FaultClass, o.Signature)
	record(t, l, c, o)
	if _, _, err := l.Remediate(c, id, "fix", "PR #1", "", ""); err != nil {
		t.Fatal(err)
	}
	o.OccurrenceKey = "2"
	record(t, l, c, o)
	o.OccurrenceKey = "3"
	record(t, l, c, o)
	if state(t, l, c, id) != Open || count(t, l, c, "fault_publications") != 1 {
		t.Fatal("fix prevented filing")
	}
	n := testObservation("notice", "observation_unmeasured", Notice)
	record(t, l, c, n)
	nid := FaultID("crw", n.FaultClass, n.Signature)
	if _, _, err := l.Remediate(c, nid, "fix", "PR #1", "", ""); err != nil {
		t.Fatal(err)
	}
	if count(t, l, c, "fault_publications") != 1 {
		t.Fatal("notice fix created publication")
	}
}
func Test22_FLT_6_PruningDoesNotChangeDecision(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("1", "delivery_stalled", Degraded)
	id := FaultID("crw", o.FaultClass, o.Signature)
	record(t, l, c, o)
	o.OccurrenceKey = "2"
	record(t, l, c, o)
	removed, err := l.Prune(c, id, 1)
	if err != nil || removed != 1 {
		t.Fatalf("prune: %d %v", removed, err)
	}
	if record(t, l, c, o) {
		t.Fatal("pruned occurrence counted again")
	}
	o.OccurrenceKey = "3"
	record(t, l, c, o)
	if state(t, l, c, id) != Open || count(t, l, c, "fault_occurrences") != 2 {
		t.Fatal("prune changed suppression")
	}
}
func Test22_FLT_7_RemediationExecutions(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "report_omitted", Broken)
	record(t, l, c, o)
	id := FaultID("crw", o.FaultClass, o.Signature)
	fix, ok, err := l.Remediate(c, id, "fix", "PR #1", "", "")
	if err != nil || !ok || fix == "" {
		t.Fatalf("fix: %s %v %v", fix, ok, err)
	}
	first, ok, err := l.Remediate(c, id, "reverification", "check", "suite", "passed")
	if err != nil || !ok {
		t.Fatalf("check: %v", err)
	}
	same, ok, err := l.Remediate(c, id, "reverification", "check", "suite", "passed")
	if err != nil || ok || same != first {
		t.Fatalf("duplicate check: %s %v %v", same, ok, err)
	}
	_, ok, err = l.Remediate(c, id, "fix", "PR #2", "", "")
	if err != nil || !ok {
		t.Fatalf("second fix: %v", err)
	}
	second, ok, err := l.Remediate(c, id, "reverification", "check", "suite", "passed")
	if err != nil || !ok || second == first {
		t.Fatalf("second execution: %s %v %v", second, ok, err)
	}
	failed, ok, err := l.Remediate(c, id, "reverification", "check", "suite", "failed")
	if err != nil || !ok || failed == second {
		t.Fatalf("failed execution: %s %v %v", failed, ok, err)
	}
	if _, err = l.Resolve(c, id); err == nil || !strings.Contains(err.Error(), "fault_unverified") {
		t.Fatalf("failed check resolved: %v", err)
	}
	if _, ok, err = l.Remediate(c, id, "reverification", "check", "suite", "passed"); err != nil || !ok {
		t.Fatalf("repeat after failure: %v", err)
	}
	if done, err := l.Resolve(c, id); err != nil || !done {
		t.Fatalf("passing check did not resolve: %v", err)
	}
}
func Test22_FLT_8_ClearByState(t *testing.T) {
	l, c := testLedger(t)
	o := testObservation("a", "observation_unmeasured", Notice)
	record(t, l, c, o)
	o.Cleared = true
	o.OccurrenceKey = "cleared"
	record(t, l, c, o)
	if state(t, l, c, FaultID("crw", o.FaultClass, o.Signature)) != Withdrawn {
		t.Fatal("unfiled fault not withdrawn")
	}
	b := testObservation("b", "report_omitted", Broken)
	record(t, l, c, b)
	b.Cleared = true
	b.OccurrenceKey = "cleared"
	record(t, l, c, b)
	if state(t, l, c, FaultID("crw", b.FaultClass, b.Signature)) != Withdrawn {
		t.Fatal("unsent publication not withdrawn")
	}
}
func Test22_FLT_9_SweepRotation(t *testing.T) {
	l, c := testLedger(t)
	sw := &Sweeper{Store: l.Store}
	o := testObservation("seed", "delivery_stalled", Degraded)
	for i := 0; i < 70; i++ {
		o.OccurrenceKey = fmt.Sprintf("event-%03d", i)
		record(t, l, c, o)
	}
	var cursor any
	seen := map[int64]bool{}
	for i := 0; i < 3; i++ {
		at, until, err := sw.rotation(c, cursor, "SELECT MAX(rowid) FROM fault_timeline", true)
		if err != nil {
			t.Fatal(err)
		}
		start := int64(0)
		if at != nil {
			start = at.(int64)
		}
		rows, err := l.Store.All(c, "SELECT rowid AS seq FROM fault_timeline WHERE rowid > ? AND rowid <= ? ORDER BY rowid LIMIT ?", start, until, sweepLimit)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen[integer(r, "seq")] = true
		}
		p := pageOf(nil, rows, "seq", at, until)
		cursor = p.cursor
		if cursor != nil {
			cursor = dumps(cursor, false)
		}
		if i < 2 && cursor == nil {
			t.Fatalf("wrapped early at page %d", i)
		}
		if i == 2 && cursor != nil {
			t.Fatal("short page failed to wrap")
		}
	}
	if len(seen) != 70 {
		t.Fatalf("rotation reached %d of 70", len(seen))
	}
	if cursor != nil {
		t.Fatal("rotation did not reset")
	}
}
func Test22_FLT_10_NumericCursor(t *testing.T) {
	nine := anchorCursor("rel-x", 9)
	ten := anchorCursor("rel-x", 10)
	if nine != "rel-x:00000000000000000009" || ten <= nine {
		t.Fatalf("cursor order: %q >= %q", nine, ten)
	}
}
