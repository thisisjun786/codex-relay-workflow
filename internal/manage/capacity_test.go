package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The judgement is driven through injected signals only: a fake relay executable, replaced gh and
// status-page seams, and a temporary relay store.

var capacityTestNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

var capacityTestClearStatus = []byte(`{"incidents": []}`)

type capacityFixture struct {
	env      *Env
	cfg      *Config
	section  map[string]any
	stateDir string
	relayDir string
}

func capacityTestPlan(plan, project, parent, family string) map[string]any {
	out := map[string]any{"plan": plan, "project": project, "parent": parent}
	if family != "" {
		out["family"] = family
	}
	return out
}

// capacityTestFixture wires one scenario: a relay store, the environment, and the configuration.
func capacityTestFixture(t *testing.T, plans []map[string]any) *capacityFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	f := &capacityFixture{relayDir: filepath.Join(dir, "relay"), stateDir: filepath.Join(dir, "state"),
		section: map[string]any{"plans": plans}}
	if err := os.MkdirAll(f.relayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	capacityTestStore(t, filepath.Join(f.relayDir, "relay.sqlite3"))
	f.env = &Env{Getenv: os.Getenv, Now: func() time.Time { return capacityTestNow }, Executable: filepath.Join(dir, "crw")}
	capacityTestLoad(t, f)
	return f
}

func capacityTestLoad(t *testing.T, f *capacityFixture) {
	t.Helper()
	raw, err := json.Marshal(f.section)
	if err != nil {
		t.Fatal(err)
	}
	cfg := coreDefaults(f.env)
	cfg.Repository, cfg.StateDir, cfg.Relay.State = "owner/repo", f.stateDir, f.relayDir
	cfg.raw = map[string]json.RawMessage{"capacity": raw}
	f.cfg = cfg
}

// capacityTestSection replaces one key of the section and reloads the configuration.
func capacityTestSection(t *testing.T, f *capacityFixture, key string, value any) {
	t.Helper()
	f.section[key] = value
	capacityTestLoad(t, f)
}

func capacityTestState(t *testing.T, f *capacityFixture, plan string, since time.Time, waiting, alerted []string, alertedAt time.Time) {
	t.Helper()
	entry := map[string]any{"since": float64(since.Unix()), "waiting": waiting, "alerted": alerted}
	if !alertedAt.IsZero() {
		entry["alerted_at"] = float64(alertedAt.Unix())
	}
	data, err := json.Marshal(map[string]any{"plans": map[string]any{plan: entry}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stateDir, capacityStateFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func capacityTestRun(t *testing.T, f *capacityFixture, dry bool) CapacityReport {
	report, err := Capacity(context.Background(), f.env, f.cfg, dry)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if len(report.Plans) == 0 {
		t.Fatal("Capacity returned no plan")
	}
	return report
}

// capacityTestPlans is the one plan the single-plan scenarios configure.
var capacityTestPlans = []map[string]any{capacityTestPlan("p-crw-129", "P-CRW-129", "parent-1", "P-CRW-129")}

// capacityTestReady wires the plans with every signal clear: free lane, host within, quick receipt.
func capacityTestReady(t *testing.T, plans []map[string]any) *capacityFixture {
	f := capacityTestFixture(t, plans)
	capacityTestRelay(t, f, capacityTestReading([]string{"CRW-1", "CRW-2"}, nil, 12, 12, "within"))
	capacityTestSeams(t, 0, capacityTestClearStatus, nil)
	capacityTestReceipt(t, f, "e1", "parent-1", 1, false)
	capacityTestState(t, f, "p-crw-129", capacityTestNow.Add(-60*time.Minute), []string{"CRW-1", "CRW-2"}, nil, time.Time{})
	return f
}

// C1: with every signal clear the plan is an expansion candidate and it alerts.
func TestCapacityExpandCandidateWhenEverySignalIsClear(t *testing.T) {
	plan := capacityTestRun(t, capacityTestReady(t, capacityTestPlans), false).Plans[0]
	if plan.Verdict != capacityExpand || len(plan.Reasons) != 0 || !plan.Alert {
		t.Fatalf("plan = %+v, want an alerting expand_candidate with no reason", plan)
	}
	if plan.WaitingMinutes != 60 || len(plan.Waiting) != 2 || plan.Held != 12 || plan.Ceiling != 12 || plan.HostMemory != capacityWithin {
		t.Errorf("the plan's evidence is wrong: %+v", plan)
	}
	if plan.ReceiptWait.MedianMinutes == nil || plan.ReceiptWait.Count != 1 {
		t.Errorf("receipt wait = %+v, want one receipt with a median", plan.ReceiptWait)
	}
	if plan.ParentsInFamily != 1 || plan.Family != "P-CRW-129" || plan.Project != "P-CRW-129" || plan.Parent != "parent-1" {
		t.Errorf("the plan's identity is wrong: %+v", plan)
	}
}

// C1: the eight hold reasons, each from one injected signal on the clear scenario.
func TestCapacityHoldsForEachReason(t *testing.T) {
	cases := []struct {
		name    string
		reason  string
		perturb func(t *testing.T, f *capacityFixture)
	}{
		{"fewer waiting nodes than the floor", "not_persistent", func(t *testing.T, f *capacityFixture) {
			capacityTestRelay(t, f, capacityTestReading([]string{"CRW-1"}, nil, 12, 12, "within"))
		}},
		{"the same waiting set is too young", "not_persistent", func(t *testing.T, f *capacityFixture) {
			capacityTestState(t, f, "p-crw-129", capacityTestNow.Add(-5*time.Minute), []string{"CRW-1", "CRW-2"}, nil, time.Time{})
		}},
		{"the host is not within", "host_memory", func(t *testing.T, f *capacityFixture) {
			capacityTestRelay(t, f, capacityTestReading([]string{"CRW-1", "CRW-2"}, nil, 12, 12, "deferring"))
		}},
		{"a child model returned 429", "child_429", func(t *testing.T, f *capacityFixture) {
			capacityTestUsageLog(t, f, 429, "deepseek-v4.1-flash", capacityTestNow.Add(-time.Minute))
		}},
		{"an unresolved Actions incident", "actions_incident", func(t *testing.T, f *capacityFixture) {
			capacityTestSeams(t, 0, []byte(`{"incidents":[{"name":"Incident with Actions","components":[{"name":"Actions"}]}]}`), nil)
		}},
		{"the parent holds fewer than its ceiling", "release_lag", func(t *testing.T, f *capacityFixture) {
			capacityTestRelay(t, f, capacityTestReading([]string{"CRW-1", "CRW-2"}, nil, 10, 12, "within"))
		}},
		{"the merge lane is saturated", "lane_saturated", func(t *testing.T, f *capacityFixture) {
			capacityTestSeams(t, 9, capacityTestClearStatus, nil)
		}},
		{"the parent is busy on receipts", "parent_busy", func(t *testing.T, f *capacityFixture) {
			capacityTestReceipt(t, f, "e2", "parent-1", 20, false)
			capacityTestReceipt(t, f, "e3", "parent-1", 30, false)
		}},
		{"the family already holds the cap", "family_parent_cap", func(t *testing.T, f *capacityFixture) {
			capacityTestSection(t, f, "plans", []map[string]any{
				capacityTestPlan("p-crw-129", "P-CRW-129", "parent-1", "P-CRW-129"),
				capacityTestPlan("p-crw-130", "P-CRW-130", "parent-2", "P-CRW-129"),
				capacityTestPlan("p-crw-131", "P-CRW-131", "parent-3", "P-CRW-129"),
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := capacityTestReady(t, capacityTestPlans)
			tc.perturb(t, f)
			plan := capacityTestRun(t, f, false).Plans[0]
			if plan.Verdict != capacityHold || len(plan.Reasons) == 0 || plan.Reasons[0] != tc.reason {
				t.Fatalf("plan = %+v, want a hold whose first reason is %q", plan, tc.reason)
			}
		})
	}
}

// C2: three parents in one family hold and two pass; an absent family falls back to the project.
func TestCapacityFamilyParentCapThreeHoldsTwoPass(t *testing.T) {
	two := []map[string]any{
		capacityTestPlan("p-crw-129", "P-CRW-129", "parent-1", "P-CRW-129"),
		capacityTestPlan("p-crw-130", "P-CRW-130", "parent-2", "P-CRW-129"),
	}
	three := append(append([]map[string]any(nil), two...), capacityTestPlan("p-crw-131", "P-CRW-131", "parent-3", "P-CRW-129"))
	for _, tc := range []struct {
		name    string
		plans   []map[string]any
		parents int
		verdict string
	}{{"two parents pass", two, 2, capacityExpand}, {"three parents hold", three, 3, capacityHold}} {
		t.Run(tc.name, func(t *testing.T) {
			plan := capacityTestRun(t, capacityTestReady(t, tc.plans), false).Plans[0]
			if plan.Verdict != tc.verdict || plan.ParentsInFamily != tc.parents {
				t.Fatalf("plan = %+v, want %s with %d parents", plan, tc.verdict, tc.parents)
			}
			if tc.verdict == capacityHold && plan.Reasons[0] != "family_parent_cap" {
				t.Fatalf("reasons = %q, want family_parent_cap", plan.Reasons)
			}
		})
	}

	f := capacityTestReady(t, []map[string]any{
		capacityTestPlan("p1", "P-CRW-129", "parent-1", ""),
		capacityTestPlan("p2", "P-CRW-129", "parent-2", ""),
		capacityTestPlan("p3", "P-CRW-129", "parent-3", ""),
	})
	if plan := capacityTestRun(t, f, false).Plans[0]; plan.Family != "P-CRW-129" || plan.ParentsInFamily != 3 || plan.Verdict != capacityHold {
		t.Fatalf("plan = %+v, want family P-CRW-129 with 3 parents and a hold", plan)
	}
}

// C4: the same waiting set is not alerted again inside the window, and is alerted after it.
func TestCapacityRealertSuppression(t *testing.T) {
	for _, tc := range []struct {
		name      string
		alertedAt time.Time
		alert     bool
	}{{"inside the window", capacityTestNow.Add(-30 * time.Minute), false}, {"after the window", capacityTestNow.Add(-4 * time.Hour), true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := capacityTestReady(t, capacityTestPlans)
			capacityTestState(t, f, "p-crw-129", capacityTestNow.Add(-60*time.Minute),
				[]string{"CRW-1", "CRW-2"}, []string{"CRW-1", "CRW-2"}, tc.alertedAt)
			if plan := capacityTestRun(t, f, false).Plans[0]; plan.Verdict != capacityExpand || plan.Alert != tc.alert {
				t.Fatalf("plan = %+v, want expand_candidate with alert %t", plan, tc.alert)
			}
		})
	}
}

// C4: a dry run writes nothing, a real run writes the state the next judgement reads.
func TestCapacityStateIsWrittenOnlyByARealRun(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	path := filepath.Join(f.stateDir, capacityStateFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if plan := capacityTestRun(t, f, true).Plans[0]; plan.Verdict != capacityExpand || !plan.Alert {
		t.Fatalf("the dry run did not judge: %+v", plan)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("a dry run wrote state: %v %s, want %s", err, after, before)
	}
	if err := os.RemoveAll(f.stateDir); err != nil {
		t.Fatal(err)
	}
	capacityTestRun(t, f, true)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a dry run created state: %v", err)
	}

	capacityTestState(t, f, "p-crw-129", capacityTestNow.Add(-60*time.Minute), []string{"CRW-1", "CRW-2"}, nil, time.Time{})
	if first := capacityTestRun(t, f, false).Plans[0]; !first.Alert {
		t.Fatalf("the first real run did not alert: %+v", first)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(saved, &doc); err != nil {
		t.Fatal(err)
	}
	state, _ := doc["plans"].(map[string]any)["p-crw-129"].(map[string]any)
	waiting, _ := state["waiting"].([]any)
	alerted, _ := state["alerted"].([]any)
	since, _ := state["since"].(float64)
	alertedAt, _ := state["alerted_at"].(float64)
	if len(waiting) != 2 || len(alerted) != 2 || since == 0 || alertedAt == 0 {
		t.Fatalf("state = %v, want the waiting and alerted sets with their times", state)
	}
	if second := capacityTestRun(t, f, false).Plans[0]; second.Alert {
		t.Fatalf("the second run alerted again: %+v", second)
	}
}

// The document carries the issue's keys and thresholds, one text line per plan, and its exits.
func TestCapacityDocumentTextThresholdsAndExitStatuses(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	var out, errOut strings.Builder
	f.env.Stdout, f.env.Stderr = &out, &errOut
	if code := capacityCommand.Run(context.Background(), f.env, nil); code != 0 {
		t.Fatalf("the JSON form: exit %d %s", code, errOut.String())
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("the judgement is not JSON: %v", err)
	}
	for _, key := range []string{"at", "limits", "lane", "actions", "child_429", "plans"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the document carries no %q: %v", key, doc)
		}
	}
	if limits, _ := doc["limits"].(map[string]any); limits["min_waiting"] != float64(2) || limits["persist_minutes"] != float64(30) ||
		limits["receipt_max_minutes"] != float64(15) || limits["realert_hours"] != float64(3) ||
		limits["lane_max"] != float64(9) || limits["max_parents_per_family"] != float64(3) {
		t.Errorf("limits = %v, want the defaults", limits)
	}

	out.Reset()
	if code := capacityCommand.Run(context.Background(), f.env, []string{"--text"}); code != 0 {
		t.Fatalf("--text: exit %d %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("--text printed %d lines, want 1: %q", len(lines), out.String())
	}
	for _, want := range []string{"p-crw-129", "parent-1", capacityExpand} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the line does not carry %q: %q", want, lines[0])
		}
	}
	for _, r := range lines[0] {
		if r > 127 {
			t.Fatalf("the text form is not English: %q", lines[0])
		}
	}

	capacityTestSeams(t, 5, capacityTestClearStatus, nil)
	for key, value := range map[string]any{"min_waiting": 1, "lane_max": 1, "max_parents_per_family": 9,
		"persist_minutes": 1, "receipt_max_minutes": 99, "realert_hours": 1} {
		capacityTestSection(t, f, key, value)
	}
	report := capacityTestRun(t, f, false)
	if report.Limits.MinWaiting != 1 || report.Limits.LaneMax != 1 || report.Limits.MaxParentsPerFamily != 9 ||
		report.Limits.PersistMinutes != 1 || report.Limits.ReceiptMaxMinutes != 99 || report.Limits.RealertHours != 1 {
		t.Fatalf("limits = %+v, want the section's values", report.Limits)
	}
	if report.Plans[0].Verdict != capacityHold || report.Plans[0].Reasons[0] != "lane_saturated" {
		t.Fatalf("plan = %+v, want a hold for lane_saturated with lane_max 1", report.Plans[0])
	}

	if code := capacityCommand.Run(context.Background(), f.env, []string{"--nope"}); code != usageExit {
		t.Fatalf("an unknown argument: exit %d, want %d", code, usageExit)
	}
	if err := os.WriteFile(f.env.Executable, []byte("#!/bin/sh\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	if code := capacityCommand.Run(context.Background(), f.env, nil); code != capacityReadFailureExit {
		t.Fatalf("a failed relay read: exit %d, want %d (%s)", code, capacityReadFailureExit, errOut.String())
	}
	if errOut.Len() == 0 {
		t.Error("a failed relay read said nothing")
	}
}
