package managed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type managedFake struct {
	operations                 map[string]map[string]any
	created, sent              int
	settings                   map[string]any
	ledger                     map[string]any
	standby                    string
	creationStatus             string
	partial, attemptedTurn     bool
	paused                     bool
	guardBudget                int
	beforeStartWorker          bool
	beforeSend                 func(SendRequest)
	sendStatus                 string
	creationEnvironmentChanged bool
	onCreate                   func()
	onGetOperation             func(string)
	onSend                     func(SendRequest)
	creationReceipt            map[string]any
	hostScenario               string
	hostCalls                  []string
}

func (f *managedFake) RequireLedger(_ context.Context, expected map[string]any) error {
	if !jsonSame(f.ledger, expected) {
		return refusal("relationship_conflict", "ledger replaced")
	}
	return nil
}
func (f *managedFake) LedgerIdentityRecord(context.Context) (map[string]any, error) {
	return f.ledger, nil
}
func (f *managedFake) GetOperation(_ context.Context, id string) (map[string]any, error) {
	if f.onGetOperation != nil {
		f.onGetOperation(id)
	}
	return f.operations[id], nil
}
func (f *managedFake) CreateThread(_ context.Context, in CreateThreadRequest) (map[string]any, error) {
	f.created++
	if f.onCreate != nil {
		f.onCreate()
	}
	created := map[string]any{}
	for k, v := range f.settings {
		created[k] = v
	}
	environments := created["environments"]
	if f.creationEnvironmentChanged {
		environments = []any{map[string]any{"environmentId": "local", "cwd": created["cwd"], "runtimeWorkspaceRoots": created["runtimeWorkspaceRoots"]}}
	}
	created["thread"] = map[string]any{"id": "child-new", "environments": environments}
	delete(created, "environments")
	receipt := map[string]any{"status": "accepted", "threadId": "child-new", "turnId": "standby", "creation": created}
	if f.creationStatus != "" {
		receipt["status"] = f.creationStatus
		delete(receipt, "turnId")
	}
	if f.partial {
		receipt["status"] = "failed"
		receipt["attemptedEffects"] = []any{"thread/start", "thread/name/set"}
		delete(receipt, "turnId")
		if f.attemptedTurn {
			receipt["attemptedEffects"] = append(receipt["attemptedEffects"].([]any), "turn/start")
		}
	}
	f.operations[in.RequestID] = receipt
	f.creationReceipt = receipt
	return receipt, nil
}
func (f *managedFake) SendMessage(_ context.Context, in SendRequest) (map[string]any, error) {
	if f.paused {
		return map[string]any{"status": "failed", "reason": "recipient_paused"}, nil
	}
	f.guardBudget = in.GuardRPCRequests
	if f.beforeSend != nil {
		f.beforeSend(in)
	}
	if in.BeforeStart != nil {
		var withhold map[string]any
		var err error
		if f.beforeStartWorker {
			var worker sync.WaitGroup
			worker.Add(1)
			go func() { defer worker.Done(); withhold, err = in.BeforeStart(context.Background()) }()
			worker.Wait()
		} else {
			withhold, err = in.BeforeStart(context.Background())
		}
		if err != nil {
			return nil, err
		}
		if withhold != nil {
			return map[string]any{"status": "failed", "reason": withhold["code"]}, nil
		}
	}
	f.sent++
	if f.onSend != nil {
		f.onSend(in)
	}
	if f.sendStatus != "" {
		receipt := map[string]any{"status": f.sendStatus, "threadId": in.ThreadID}
		f.operations[in.RequestID] = receipt
		return receipt, nil
	}
	turn := "business"
	if len(in.RequestID) > 16 && in.RequestID[:16] == "managed-standby-" {
		turn = "recovered-standby"
	}
	receipt := map[string]any{"status": "accepted", "threadId": in.ThreadID, "turnId": turn, "requestId": in.RequestID}
	f.operations[in.RequestID] = receipt
	return receipt, nil
}
func (f *managedFake) ReadTurn(_ context.Context, task, turn string) (*Turn, error) {
	return &Turn{ID: turn, Status: f.standby}, nil
}
func (f *managedFake) Lifecycle(_ context.Context, _, _ string) (bool, string, error) {
	if f.paused {
		return false, "recipient_paused", nil
	}
	return true, "", nil
}
func Test27_MST_7_WorkerPolicyDisappearsAfterCreation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	ready := true
	host.onCreate = func() { ready = false }
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) {
		if !ready {
			return "worker_policy_unconfigured", nil
		}
		return "", nil
	}}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["reason"] != "worker_policy_unconfigured" || host.created != 1 || host.sent != 0 {
		t.Fatalf("policy disappearance: %v effects %d/%d", got, host.created, host.sent)
	}
	ready = true
	result, err = start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["state"] != "admitted" || host.created != 1 || host.sent != 1 {
		t.Fatalf("policy restoration: %v effects %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_7_CreationEnvironmentDriftRefusesRegistration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, creationEnvironmentChanged: true}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM relationships").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if got["reason"] != "creation_settings_unverified" || count != 0 || host.sent != 0 {
		t.Fatalf("creation drift: %v relation count=%d sent=%d", got, count, host.sent)
	}
}

func Test27_MST_3_PausedPartialShellNoRecoverySend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, partial: true, paused: true}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["state"] != "incomplete" || host.created != 1 || host.sent != 0 {
		t.Fatalf("paused partial shell replaced: %v %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_7_PausedChildPreservesShell(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed", paused: true}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["reason"] != "recipient_paused" || host.created != 1 || host.sent != 0 {
		t.Fatalf("paused shell: %v effects %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_6_CreatedChildIsAuthorizedOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	var recipients string
	if err := s.DB.QueryRowContext(ctx, "SELECT allowed_recipients FROM relationships WHERE relationship_id=?", got["relationshipId"]).Scan(&recipients); err != nil {
		t.Fatal(err)
	}
	var people []string
	if err := json.Unmarshal([]byte(recipients), &people); err != nil {
		t.Fatal(err)
	}
	if !jsonSame(people, []string{"parent", "child-new"}) {
		t.Fatalf("created recipient missing or repeated: %v", people)
	}
	if !jsonSame(req["allowedRecipients"], []any{"parent"}) {
		t.Fatal("caller request mutated")
	}
}

func Test27_MST_3_UnknownRecoverySendIsNotRetried(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, partial: true, sendStatus: "outcome_unknown"}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	for range 2 {
		result, e := start.Run(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		got := map[string]any{}
		for _, f := range result {
			got[f.Key] = f.Value
		}
		if got["state"] != "incomplete" {
			t.Fatal(got)
		}
	}
	if host.created != 1 || host.sent != 1 {
		t.Fatalf("unknown standby retry: %d/%d", host.created, host.sent)
	}
}

func Test27_MST_8_FinalGuardRefusesScopeDrift(t *testing.T) {
	for _, change := range []struct{ name, column, value string }{
		{"parent", "parent_task_id", "other-parent"},
		{"host", "child_host_id", "other-host"},
		{"roots", "artifact_roots", `["/different"]`},
		{"recipients", "allowed_recipients", `["unrelated"]`},
	} {
		t.Run(change.name, func(t *testing.T) { checkManagedScopeDrift(t, change.column, change.value) })
	}
}

func checkManagedScopeDrift(t *testing.T, column, value string) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	var observed string
	host.beforeSend = func(in SendRequest) {
		if in.GuardRPCRequests != 10 {
			t.Fatalf("ready-check budget %d, want 10", in.GuardRPCRequests)
		}
		if in.BeforeStart == nil {
			t.Fatal("missing final guard")
		}
		_, err := s.DB.ExecContext(ctx, "UPDATE relationships SET "+column+"=? WHERE issue_key=?", value, req["issueKey"])
		if err != nil {
			t.Fatal(err)
		}
		verdict, err := in.BeforeStart(ctx)
		if err != nil {
			t.Fatal(err)
		}
		observed = str(verdict["code"])
	}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	for _, f := range result {
		fields[f.Key] = f.Value
	}
	if observed != "managed_scope_changed" || fields["state"] == "admitted" || host.sent != 0 {
		t.Fatalf("scope drift escaped: verdict=%s receipt=%v sent=%d", observed, fields, host.sent)
	}
}

func Test27_MST_3_UncertainFirstTurnNeverRecovers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, partial: true, attemptedTurn: true}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	for range 2 {
		receipt, e := start.Run(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		got := map[string]any{}
		for _, f := range receipt {
			got[f.Key] = f.Value
		}
		if got["state"] != "incomplete" || got["reason"] != "creation_failed" {
			t.Fatal(got)
		}
	}
	if host.created != 1 || host.sent != 0 {
		t.Fatalf("uncertain first turn replaced: %d/%d", host.created, host.sent)
	}
}

func Test27_MST_4_LedgerReplacementBeforeCreationWithholdsEffect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}}
	host.onGetOperation = func(id string) {
		if strings.HasPrefix(id, "managed-create-") {
			host.ledger = map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 3}
		}
	}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	_, err = start.Run(ctx, raw)
	reasonIs(t, err, "relationship_conflict")
	if host.created != 0 || host.sent != 0 {
		t.Fatalf("replaced ledger caused host effect: %d/%d", host.created, host.sent)
	}
}

func Test27_MST_4_UnknownLedgerNeverReserves(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	host := &managedFake{ledger: nil, operations: map[string]map[string]any{}}
	start := &Start{Store: s, Adapter: host, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir}
	_, err = start.Run(ctx, raw)
	if err == nil || !strings.Contains(err.Error(), "observed bridge ledger identity") {
		t.Fatalf("unknown ledger: %v", err)
	}
	_, err = s.ManagedStartRequest(ctx, "managed-1")
	if err != sql.ErrNoRows {
		t.Fatalf("unknown ledger reserved: %v", err)
	}
	if host.created != 0 || host.sent != 0 {
		t.Fatal("unknown ledger caused host effect")
	}
}

func Test27_MST_4_PreflightRefusesBeforeHostEffects(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "worker_policy_unconfigured", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	for _, f := range result {
		fields[f.Key] = f.Value
	}
	if fields["state"] != "refused" || fields["reason"] != "worker_policy_unconfigured" || host.created != 0 || host.sent != 0 {
		t.Fatalf("wrong preflight: %v, effects %d/%d", fields, host.created, host.sent)
	}
	_, err = s.ManagedStartRequest(ctx, "managed-1")
	if err != sql.ErrNoRows {
		t.Fatalf("preflight reserved a request: %v", err)
	}
}

func Test27_MST_2_RegisteredShellCompletesCriteriaAfterCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	host.onCreate = func() {
		_, e := s.DB.ExecContext(ctx, "CREATE TRIGGER fail_criteria BEFORE INSERT ON canonical_criteria BEGIN SELECT RAISE(ABORT,'caller died'); END")
		if e != nil {
			t.Fatal(e)
		}
	}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	if _, err = start.Run(ctx, raw); err == nil || !strings.Contains(err.Error(), "caller died") {
		t.Fatalf("expected criteria crash: %v", err)
	}
	if host.created != 1 || host.sent != 0 {
		t.Fatalf("effects before criteria: %d/%d", host.created, host.sent)
	}
	if _, err := s.DB.ExecContext(ctx, "DROP TRIGGER fail_criteria"); err != nil {
		t.Fatal(err)
	}
	host.onCreate = nil
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["state"] != "admitted" || host.created != 1 || host.sent != 1 {
		t.Fatalf("resume after criteria crash: %v %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_2_BusinessReceiptReplayedAfterAdmissionCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	host.onSend = func(in SendRequest) {
		if strings.HasPrefix(in.RequestID, "managed-business-") {
			_, e := s.DB.ExecContext(ctx, "CREATE TRIGGER fail_admission BEFORE INSERT ON generation_turns BEGIN SELECT RAISE(ABORT,'caller died'); END")
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	if _, err = start.Run(ctx, raw); err == nil || !strings.Contains(err.Error(), "caller died") {
		t.Fatalf("admission failure not observed: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, "DROP TRIGGER fail_admission"); err != nil {
		t.Fatal(err)
	}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["state"] != "admitted" || got["businessTurnId"] != "business" || host.created != 1 || host.sent != 1 {
		t.Fatalf("crash replay: %v %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_2_StandbyRecoveryUsesRetainedShell(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed", partial: true}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	for range 2 {
		result, e := start.Run(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		fields := map[string]any{}
		for _, f := range result {
			fields[f.Key] = f.Value
		}
		if fields["state"] != "admitted" || fields["standbyTurnId"] != "recovered-standby" {
			t.Fatalf("wrong recovery: %v", fields)
		}
	}
	if host.created != 1 || host.sent != 2 {
		t.Fatalf("recovery repeated host effect: %d/%d", host.created, host.sent)
	}
	original := host.operations["managed-create-72a8e50ded92d4aa42328b9181bd188470f5be13faf25bdb8cf3ffa221c92020"]
	if original["status"] != "failed" || original["turnId"] != nil {
		t.Fatalf("original receipt changed: %v", original)
	}
}

func Test27_MST_3_StandbyIncompleteRetainsChildUntilRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "inProgress"}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["reason"] != "standby_incomplete" || got["childTaskId"] != "child-new" || host.sent != 0 {
		t.Fatalf("standby incomplete: %v sent=%d", got, host.sent)
	}
	host.standby = "completed"
	result, err = start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	if got["state"] != "admitted" || host.created != 1 || host.sent != 1 {
		t.Fatalf("standby retry: %v %d/%d", got, host.created, host.sent)
	}
}

func Test27_MST_3_UnknownCreationNeverCreatesReplacement(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, creationStatus: "outcome_unknown"}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	for range 2 {
		receipt, e := start.Run(ctx, raw)
		if e != nil {
			t.Fatal(e)
		}
		got := map[string]any{}
		for _, f := range receipt {
			got[f.Key] = f.Value
		}
		if got["state"] != "incomplete" || got["reason"] != "creation_unknown" {
			t.Fatal(fmt.Sprint(got))
		}
	}
	if host.created != 1 || host.sent != 0 {
		t.Fatalf("uncertain creation retried or dispatched: %d/%d", host.created, host.sent)
	}
}

func Test27_MST_1_FakeHostStartReplay(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	child := obj(obj(req["child"])["settings"])
	host := &managedFake{operations: map[string]map[string]any{}, settings: child, ledger: map[string]any{"realPath": filepath.Join(dir, "operations"), "device": 1, "inode": 2}, standby: "completed"}
	start := &Start{Store: s, Adapter: host, Now: func() string { return "2026-09-26T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	result, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("missing result")
	}
	found := map[string]any{}
	for _, item := range result {
		found[item.Key] = item.Value
	}
	if found["state"] != "admitted" || found["businessTurnId"] != "business" || host.created != 1 || host.sent != 1 {
		t.Fatalf("first start: %v; effects %d/%d", found, host.created, host.sent)
	}
	replay, err := start.Run(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	replayed := map[string]any{}
	for _, item := range replay {
		replayed[item.Key] = item.Value
	}
	if replayed["businessTurnId"] != "business" || host.created != 1 || host.sent != 1 {
		t.Fatalf("replay: %v; effects %d/%d", replayed, host.created, host.sent)
	}
	observation, err := LastObservation(ctx, s, "managed-1")
	if err != nil {
		t.Fatal(err)
	}
	journal := observation.(map[string]any)
	if journal["businessTurnId"] != "business" || journal["childTaskId"] != "child-new" {
		t.Fatalf("managed-show observation lost ids: %v", journal)
	}
	var output, stderr bytes.Buffer
	code := cli.ExecuteAs(ctx, "codex-session-relay", []string{"--state", dir, "managed-show", "--request-id", "managed-1"}, &output, &stderr)
	var shown map[string]any
	if err := json.Unmarshal(output.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	request := shown["request"].(map[string]any)
	last := shown["lastObservation"].(map[string]any)
	if code != 0 || request["child_task_id"] != "child-new" || last["businessTurnId"] != "business" {
		t.Fatalf("managed-show code=%d stdout=%s stderr=%s", code, &output, &stderr)
	}
	changed := map[string]any{}
	if err := json.Unmarshal(raw, &changed); err != nil {
		t.Fatal(err)
	}
	changed["prompt"] = "different assignment"
	rawChanged, _ := json.Marshal(changed)
	_, err = start.Run(ctx, rawChanged)
	reasonIs(t, err, "relationship_conflict")
	if host.created != 1 || host.sent != 1 {
		t.Fatal("fingerprint mismatch performed another host effect")
	}
}

func (f *managedFake) HostCall(_ context.Context, method string, params map[string]any) (map[string]any, error) {
	f.hostCalls = append(f.hostCalls, method)
	switch method {
	case "thread/list":
		if f.hostScenario == "lifecycle_unknown" {
			return map[string]any{"data": []any{}}, nil
		}
		if f.hostScenario == "recipient_archived" && params["archived"] == true || f.hostScenario != "recipient_archived" && params["archived"] == false {
			return map[string]any{"data": []any{map[string]any{"id": "child-new"}}}, nil
		}
		return map[string]any{"data": []any{}}, nil
	case "thread/goal/get":
		if f.hostScenario == "recipient_paused" {
			return map[string]any{"goal": map[string]any{"status": "paused"}}, nil
		}
		return map[string]any{"goal": nil}, nil
	case "thread/read":
		accept := f.hostScenario != "recipient_cannot_accept_input"
		status := "idle"
		if f.hostScenario == "recipient_not_idle" {
			status = "busy"
		}
		return map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}, "canAcceptDirectInput": accept}}, nil
	}
	return nil, fmt.Errorf("unexpected host call %s", method)
}
