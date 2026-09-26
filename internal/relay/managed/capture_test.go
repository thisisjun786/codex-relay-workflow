package managed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type managedCapture struct {
	Receipts        []map[string]any            `json:"receipts"`
	OrderedReceipts []string                    `json:"orderedReceipts"`
	Effects         []int                       `json:"effects"`
	Tables          map[string][]map[string]any `json:"tables"`
	Scope           []map[string]any            `json:"scope"`
	Ledger          struct {
		Path   string `json:"path"`
		Device uint64 `json:"device"`
		Inode  uint64 `json:"inode"`
	} `json:"ledger"`
}

func capturePythonManaged(t *testing.T, scenario string) (string, managedCapture) {
	t.Helper()
	root := t.TempDir()
	home, err := os.MkdirTemp("/dev/shm", "crw-managed-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/managed/testdata/capture.py"), root, scenario)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_DATA_HOME="+home, "CODEX_HOME="+home, "TMPDIR=/dev/shm", "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-thread-bridge/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python fake %s: %v\n%s", scenario, err, output)
	}
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture managedCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	return root, capture
}
func pythonFixture(root string) []byte {
	workspace := filepath.Join(root, "workspace")
	settings := func() map[string]any {
		return map[string]any{"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}, "approvalPolicy": "never", "cwd": workspace, "runtimeWorkspaceRoots": []any{workspace}, "model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh", "environments": []any{}}
	}
	request := map[string]any{"schema": Schema, "requestId": "managed-1", "issueKey": "REL-MANAGED", "parent": map[string]any{"taskId": "01parent-task", "hostId": "host-a", "settings": settings()}, "child": map[string]any{"hostId": "host-a", "title": "REL-MANAGED Verify admission", "settings": settings()}, "artifactRoots": []any{workspace}, "allowedRecipients": []any{"01parent-task"}, "criteria": []any{map[string]any{"id": "c1", "title": "preserve replay identity", "required": true}}, "criteriaSource": "issue:REL-MANAGED", "baselineRevision": "baseline", "scopeRef": "issue:REL-MANAGED", "prompt": "business-secret: implement the scoped fix"}
	raw, _ := json.Marshal(request)
	return raw
}
func normalizedManaged(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			if k == "requestFingerprint" || k == "request_fingerprint" {
				out[k] = "<physical fingerprint>"
				continue
			}
			out[k] = normalizedManaged(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = normalizedManaged(value)
		}
		return out
	case string:
		return strings.ReplaceAll(strings.ReplaceAll(x, "gomarkers", "markers"), "gostate", "state")
	default:
		return v
	}
}
func Test27_MEX_1_PythonInstructedSequenceWholeOutput(t *testing.T) {
	comparePythonExecutionCLI(t)
}
func Test27_MEX_2_PythonMarkerRestartWholeOutput(t *testing.T) {
	comparePythonExecutionCLI(t)
}
func Test27_MEX_3_PythonDuplicateChildWholeOutput(t *testing.T) {
	comparePythonExecutionCLI(t)
}
func comparePythonExecutionCLI(t *testing.T) {
	root, want := capturePythonManaged(t, "execution-cli")
	state := filepath.Join(root, "goexecution")
	marker := filepath.Join(root, "goexecution-markers")
	workspace := filepath.Join(root, "execution-workspace")
	var got []any
	command := func(args ...string) map[string]any {
		t.Helper()
		argv := append([]string{"--state", state}, args...)
		var output, stderr bytes.Buffer
		code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &output, &stderr)
		var payload map[string]any
		if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
			t.Fatalf("%v: %v stdout=%s stderr=%s", argv, err, &output, &stderr)
		}
		got = append(got, map[string]any{"exit": float64(code), "stdout": payload})
		return payload
	}
	command("doctor", "--issue", "REL-EXECUTION")
	_, err := os.Stat(filepath.Join(state, "relay.sqlite3"))
	got = append(got, map[string]any{"storeExists": !os.IsNotExist(err)})
	declared := command("intent-declare", "--dispatch-request-id", "dispatch-1", "--issue", "REL-EXECUTION", "--marker-root", marker, "--workspace", workspace)
	assignment := str(declared["assignmentId"])
	relation := command("register", "--parent-task", "parent", "--parent-host", "host", "--child-task", "child", "--child-host", "host", "--issue", "REL-EXECUTION", "--artifact-root", workspace, "--allowed-recipient", "parent", "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", "standby")
	rid := str(relation["relationshipId"])
	command("intent-register", "--assignment", assignment, "--relationship", rid, "--dispatch-request-id", "dispatch-1", "--db-path", filepath.Join(state, "relay.sqlite3"), "--marker-root", marker, "--workspace", workspace)
	command("criteria-register", "--relationship", rid, "--criterion", "c1=the deliverable behaves")
	command("doctor", "--issue", "REL-EXECUTION")
	command("assignment-find", "--issue", "REL-EXECUTION")
	command("intent-show", "--assignment", assignment, "--marker-root", marker, "--workspace", workspace)
	command("register", "--parent-task", "parent", "--parent-host", "host", "--child-task", "other-child", "--child-host", "host", "--issue", "REL-EXECUTION", "--artifact-root", workspace, "--allowed-recipient", "parent", "--dispatch-request-id", "dispatch-2", "--dispatch-turn-id", "standby")
	command("assignment-find", "--issue", "REL-EXECUTION")
	if len(got) != len(want.Receipts) {
		t.Fatalf("Go steps=%d Python=%d", len(got), len(want.Receipts))
	}
	for i, expected := range want.Receipts {
		g, p := normalizedExecution(got[i]), normalizedExecution(expected)
		if !reflect.DeepEqual(g, p) {
			t.Errorf("step %d Go=%v Python=%v", i, g, p)
		}
	}
	before := obj(obj(want.Receipts[0])["stdout"])
	issue := obj(before["issue"])
	goBefore := obj(obj(got[0])["stdout"])
	goIssue := obj(goBefore["issue"])
	if issue["readable"] != false || issue["holds"] != nil || obj(want.Receipts[1])["storeExists"] != false || goIssue["readable"] != false || goIssue["holds"] != nil || obj(got[1])["storeExists"] != false {
		t.Fatal("pre-registration diagnosis created a store", before, goBefore)
	}
	after := obj(obj(want.Receipts[6])["stdout"])
	owner := obj(after["issue"])
	if owner["holds"] != true || owner["responsibleChild"] != "child" || owner["storeAgreement"] != "same" || owner["storeId"] != obj(after["store"])["storeId"] {
		t.Fatalf("Python diagnosis: %v", owner)
	}
	found := obj(obj(want.Receipts[7])["stdout"])
	goAfter := obj(obj(got[6])["stdout"])
	goOwner := obj(goAfter["issue"])
	goFound := obj(obj(got[7])["stdout"])
	if found["responsibleRelationship"] != owner["responsibleRelationship"] || len(found["assignments"].([]any)) != 1 || goOwner["holds"] != true || goOwner["responsibleChild"] != "child" || goOwner["storeAgreement"] != "same" || goOwner["storeId"] != obj(goAfter["store"])["storeId"] || goFound["responsibleRelationship"] != goOwner["responsibleRelationship"] || len(goFound["assignments"].([]any)) != 1 {
		t.Fatalf("assignment mismatch Go=%v Python=%v", goFound, found)
	}
	markerBody := obj(obj(want.Receipts[8])["stdout"])
	goMarkerBody := obj(obj(got[8])["stdout"])
	if !strings.Contains(fmt.Sprint(markerBody), str(owner["responsibleRelationship"])) || !strings.Contains(fmt.Sprint(markerBody), filepath.Join(root, "execution", "relay.sqlite3")) || !strings.Contains(fmt.Sprint(goMarkerBody), str(goOwner["responsibleRelationship"])) || !strings.Contains(fmt.Sprint(goMarkerBody), filepath.Join(root, "goexecution", "relay.sqlite3")) {
		t.Fatalf("marker restart Go=%v Python=%v", goMarkerBody, markerBody)
	}
	duplicate := obj(obj(want.Receipts[9])["stdout"])
	goDuplicate := obj(obj(got[9])["stdout"])
	if obj(want.Receipts[9])["exit"] != float64(2) || duplicate["reason"] != "duplicate_assignment" || len(obj(obj(want.Receipts[10])["stdout"])["assignments"].([]any)) != 1 || obj(got[9])["exit"] != float64(2) || goDuplicate["reason"] != "duplicate_assignment" || len(obj(obj(got[10])["stdout"])["assignments"].([]any)) != 1 {
		t.Fatalf("duplicate assignment Go=%v Python=%v", goDuplicate, duplicate)
	}
}

var executionTime = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|\+00:00)`)

func normalizedExecution(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			if k == "runtime" {
				continue
			}
			if k == "createdAt" {
				out[k] = "<independent creation time>"
				continue
			}
			if k == "storeId" {
				out[k] = "<independent store>"
				continue
			}
			if k == "inode" || k == "logInode" {
				out[k] = "<physical inode>"
				continue
			}
			out[k] = normalizedExecution(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizedExecution(item)
		}
		return out
	case string:
		x = strings.ReplaceAll(strings.ReplaceAll(x, "goexecution-markers", "execution-markers"), "goexecution", "execution")
		return executionTime.ReplaceAllString(x, "<time>")
	default:
		return x
	}
}
func Test27_MST_1_PythonFakeWholeReceiptAndRows(t *testing.T) { comparePythonManaged(t, "happy") }
func Test27_MST_10_PythonShowAfterStartWholeOutputAndRows(t *testing.T) {
	comparePythonManaged(t, "show-after-start")
}
func Test27_MST_10_PythonShowAbsentWholeOutputAndRows(t *testing.T) {
	comparePythonManaged(t, "show-absent")
}
func Test27_MST_9_PythonUnknownInputWholeOutputAndRows(t *testing.T) {
	comparePythonManaged(t, "cli-unknown-input")
}
func Test27_MST_9_PythonMissingWorkerWholeOutputAndRows(t *testing.T) {
	comparePythonManaged(t, "cli-missing-worker")
}
func Test27_MST_9_PythonMissingSelectorsWholeOutputAndRows(t *testing.T) {
	comparePythonManaged(t, "cli-missing-selectors")
}
func Test27_MRS_1_PythonReservationReplayWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-replay")
}
func Test27_MRS_1_PythonCheckpointReplayWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-checkpoint")
}
func Test27_MRS_2_PythonReservationContentionWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-contention")
}
func Test27_MRS_2_PythonActiveOwnerWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-active-owner")
}
func Test27_MRS_2_PythonRawRegisterWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-raw-register")
}
func Test27_MRS_2_PythonUniqueIndexWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-index")
}
func Test27_MRS_2_PythonResumeBlockedWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-resume")
}
func Test27_MRS_2_PythonTwoConnectionsWholeOutputAndRows(t *testing.T) {
	root, want := capturePythonManaged(t, "reservation-threads")
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := Identity{IssueKey: "REL-1", Fingerprint: "fp-1", Version: Schema, Workspace: "/tmp/work", MarkerRoot: "/tmp/markers", SocketIdentity: "/tmp/socket", CreateRequestID: "create-1", DispatchRequestID: "business-1"}
	start := make(chan struct{})
	outcomes := make(chan any, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"req-a", "req-b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			connection, e := store.Open(ctx, s.Path, "")
			if e != nil {
				outcomes <- e
				return
			}
			defer connection.Close()
			in := id
			in.RequestID = name
			<-start
			row, e := (Reservation{Store: connection, Now: delivery.NewFakeClock().ISO}).Reserve(ctx, in)
			if e != nil {
				outcomes <- map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")}
				return
			}
			outcomes <- map[string]any{"ok": orderedValue(t, reservationRecord(row))}
		}(name)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	got := []any{}
	for outcome := range outcomes {
		got = append(got, outcome)
	}
	sort.Slice(got, func(i, j int) bool { return obj(got[i])["ok"] != nil })
	if len(got) != len(want.Receipts) {
		t.Fatalf("Go=%v Python=%v", got, want.Receipts)
	}
	winner := str(obj(obj(got[0])["ok"])["request_id"])
	pyWinner := str(obj(obj(want.Receipts[0])["ok"])["request_id"])
	if winner != "req-a" && winner != "req-b" || pyWinner != "req-a" && pyWinner != "req-b" {
		t.Fatalf("no winner Go=%v Python=%v", got, want.Receipts)
	}
	goWon := obj(obj(got[0])["ok"])
	pyWon := obj(obj(want.Receipts[0])["ok"])
	if !reflect.DeepEqual(goWon, normalizeRaceWinner(pyWon, winner)) {
		t.Errorf("winner Go=%v Python=%v", goWon, pyWon)
	}
	goLoser, pyLoser := obj(got[1]), obj(want.Receipts[1])
	if goLoser["error"] != pyLoser["error"] {
		t.Fatalf("loser class Go=%v Python=%v", goLoser, pyLoser)
	}
	goDetail := str(goLoser["detail"])
	pyDetail := str(pyLoser["detail"])
	if goDetail != strings.ReplaceAll(pyDetail, pyWinner, winner) {
		t.Errorf("loser Go=%v Python=%v", goLoser, pyLoser)
	}
	if goLoser["error"] != "RegistrationError" || !strings.HasPrefix(goDetail, "duplicate_assignment: issue 'REL-1' is already held by request") {
		t.Fatalf("unexpected Go refusal: %v", goLoser)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM managed_start_requests").Scan(&count); err != nil || count != 1 {
		t.Fatalf("Go pending=%d err=%v", count, err)
	}
}
func normalizeRaceWinner(row map[string]any, newID string) map[string]any {
	out := map[string]any{}
	for key, value := range row {
		if key == "request_id" {
			value = newID
		}
		out[key] = value
	}
	return out
}
func Test27_MRS_3_PythonReservationReceiptsWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-receipts")
}
func Test27_MRS_3_PythonReservationAttachWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-attach")
}
func Test27_MRS_3_PythonAttachConflictWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-attach-conflict")
}
func Test27_MRS_4_PythonReservationReleaseWholeOutputAndRows(t *testing.T) {
	comparePythonReservation(t, "reservation-release")
}
func Test27_MRS_4_PythonArmReleaseRaceWholeOutputAndRows(t *testing.T) {
	root, want := capturePythonManaged(t, "reservation-arm-release")
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := Reservation{Store: s, Now: delivery.NewFakeClock().ISO}
	id := Identity{RequestID: "req-1", IssueKey: "REL-1", Fingerprint: "fp-1", Version: Schema, Workspace: "/tmp/work", MarkerRoot: "/tmp/markers", SocketIdentity: "/tmp/socket", CreateRequestID: "create-1", DispatchRequestID: "business-1"}
	if _, err := r.Reserve(ctx, id); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type raceResult struct {
		action string
		row    store.ManagedStartRequestsRow
		err    error
	}
	results := make(chan raceResult, 2)
	var wg sync.WaitGroup
	for _, action := range []string{"arm", "release"} {
		wg.Add(1)
		go func(action string) {
			defer wg.Done()
			conn, e := store.Open(ctx, s.Path, "")
			if e != nil {
				results <- raceResult{action: action, err: e}
				return
			}
			defer conn.Close()
			reservation := Reservation{Store: conn, Now: delivery.NewFakeClock().ISO}
			<-start
			var row store.ManagedStartRequestsRow
			if action == "arm" {
				row, e = reservation.Arm(ctx, id.RequestID, id.Fingerprint, 0)
			} else {
				row, e = reservation.Release(ctx, id.RequestID, id.Fingerprint, 0, "operator")
			}
			results <- raceResult{action: action, row: row, err: e}
		}(action)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, losses := 0, 0
	var winner, loser raceResult
	for result := range results {
		if result.err == nil {
			wins++
			winner = result
		} else {
			losses++
			loser = result
			var refused *store.RefusedError
			if !errors.As(result.err, &refused) || refused.Reason != "relationship_conflict" {
				t.Fatalf("unexpected loser: %v", result.err)
			}
		}
	}
	row, e := s.ManagedStartRequest(ctx, id.RequestID)
	if e != nil {
		t.Fatal(e)
	}
	if wins != 1 || losses != 1 || row.Revision != 1 || len(want.Receipts) != 4 {
		t.Fatalf("Go wins=%d losses=%d row=%v Python=%v", wins, losses, row, want.Receipts)
	}
	if winner.action == "arm" && row.State != "create_armed" || winner.action == "release" && (row.State != "released" || row.ReleaseReason.String != "operator") {
		t.Fatalf("winner=%v row=%v", winner.action, row)
	}
	pythonRow := obj(want.Receipts[3])
	if pythonRow["revision"] != float64(1) || (pythonRow["state"] != "create_armed" && pythonRow["state"] != "released") {
		t.Fatalf("Python retained invalid race row: %v", pythonRow)
	}
	gotRow := obj(orderedValue(t, reservationRecord(row)))
	if gotRow["state"] != "create_armed" && gotRow["state"] != "released" {
		t.Fatalf("Go retained invalid race row: %v", gotRow)
	}
	pythonWinner := obj(obj(want.Receipts[1])["ok"])
	pythonLoser := obj(want.Receipts[2])
	if pythonWinner["state"] != pythonRow["state"] || pythonLoser["error"] != "RegistrationError" {
		t.Fatalf("Python race output: %v", want.Receipts)
	}
	if gotRow["state"] != pythonRow["state"] {
		for key, value := range pythonRow {
			if key == "state" || key == "release_reason" {
				continue
			}
			if gotRow[key] != value {
				t.Errorf("race row field %s Go=%v Python=%v", key, gotRow[key], value)
			}
		}
	} else if !reflect.DeepEqual(gotRow, pythonRow) {
		t.Errorf("race row Go=%v Python=%v", gotRow, pythonRow)
	}
	if !strings.Contains(loser.err.Error(), "relationship_conflict") {
		t.Fatalf("Go loser=%v Python=%v", loser.err, pythonLoser)
	}
}
func Test27_MRS_5_PythonEnsureSettingsWholeOutputAndRows(t *testing.T) {
	root, want := capturePythonManaged(t, "reservation-settings")
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req, err := ParseRequest(pythonFixture(root))
	if err != nil {
		t.Fatal(err)
	}
	settings := obj(obj(req["parent"])["settings"])
	clock := delivery.NewFakeClock()
	got := []any{}
	for _, action := range []struct{ task, source string }{{"parent", "creation_result"}, {"parent", "recovery"}} {
		source, e := EnsureSettings(ctx, s, action.task, settings, action.source, clock.ISO())
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, map[string]any{"taskId": action.task, "source": source, "settings": settings})
	}
	changed := map[string]any{}
	for key, value := range settings {
		changed[key] = value
	}
	changed["model"] = "other-model"
	_, err = EnsureSettings(ctx, s, "parent", changed, "recovery", clock.ISO())
	if err == nil {
		t.Fatal("settings drift accepted")
	}
	got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(err.Error(), "transaction body: ")})
	source, err := EnsureSettings(ctx, s, "task-new", settings, "recovery", clock.ISO())
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, map[string]any{"taskId": "task-new", "source": source, "settings": settings})
	for i, expected := range want.Receipts {
		if !reflect.DeepEqual(got[i], expected) {
			t.Errorf("receipt %d Go=%v Python=%v", i, got[i], expected)
		}
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT task_id,settings,source,recorded_at FROM authorized_settings ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	var actual []map[string]any
	for rows.Next() {
		var task, encoded, source, at string
		if err := rows.Scan(&task, &encoded, &source, &at); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		actual = append(actual, map[string]any{"task_id": task, "settings": encoded, "source": source, "recorded_at": at})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(actual, want.Tables["authorized_settings"]) {
		t.Errorf("settings rows Go=%v Python=%v", actual, want.Tables["authorized_settings"])
	}
}
func comparePythonReservation(t *testing.T, scenario string) {
	root, want := capturePythonManaged(t, scenario)
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := Reservation{Store: s, Now: delivery.NewFakeClock().ISO}
	id := Identity{RequestID: "req-1", IssueKey: "REL-1", Fingerprint: "fp-1", Version: Schema, Workspace: "/tmp/work", MarkerRoot: "/tmp/markers", SocketIdentity: "/tmp/socket", CreateRequestID: "create-1", DispatchRequestID: "business-1"}
	got := []any{}
	if scenario == "reservation-resume" {
		first, e := r.Reserve(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(first)))
		if _, e := s.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state='released',revision=1,release_reason='make-room' WHERE request_id='req-1'"); e != nil {
			t.Fatal(e)
		}
		reg := &registry.Registry{Store: s, Now: delivery.NewFakeClock().ISO}
		relation, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host", Cwd: sql.NullString{String: "/parent", Valid: true}}, Child: registry.Endpoint{TaskID: "child", HostID: "host", Cwd: sql.NullString{String: "/tmp/work", Valid: true}}, IssueKey: "REL-1", ArtifactRoots: []string{"/tmp/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "live"})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := reg.SetStatus(ctx, relation.ID, "paused", "operator"); e != nil {
			t.Fatal(e)
		}
		if _, e := s.DB.ExecContext(ctx, "UPDATE managed_start_requests SET state='reserved',revision=0,release_reason=NULL WHERE request_id='req-1'"); e != nil {
			t.Fatal(e)
		}
		got = append(got, map[string]any{"relationshipId": relation.ID})
		_, e = reg.Resume(ctx, relation.ID, 1, []string{"/tmp/work"}, []string{"parent"}, "operator")
		if e == nil {
			t.Fatal("pending reservation bypassed on resume")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		row, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
		compareReservationCapture(t, s, want, got)
		return
	}
	if scenario == "reservation-active-owner" {
		reg := &registry.Registry{Store: s, Now: delivery.NewFakeClock().ISO}
		relation, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host", Cwd: sql.NullString{String: "/parent", Valid: true}}, Child: registry.Endpoint{TaskID: "child", HostID: "host", Cwd: sql.NullString{String: "/tmp/work", Valid: true}}, IssueKey: "REL-1", ArtifactRoots: []string{"/tmp/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "live"})
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, map[string]any{"relationshipId": relation.ID})
		for _, status := range []string{"active", "paused"} {
			if status == "paused" {
				if _, e := reg.SetStatus(ctx, relation.ID, status, "operator"); e != nil {
					t.Fatal(e)
				}
			}
			_, e := r.Reserve(ctx, id)
			if e == nil {
				t.Fatal("active owner admitted reservation")
			}
			got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		}
		compareReservationCapture(t, s, want, got)
		return
	}
	reserveCount := 2
	if scenario != "reservation-replay" {
		reserveCount = 1
	}
	for range reserveCount {
		row, e := r.Reserve(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
	}
	if scenario == "reservation-index" {
		_, e := s.DB.ExecContext(ctx, `INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,'reserved',0,?,?)`, "req-direct", "REL-1", "fp-1", Schema, "/tmp/work", "/tmp/markers", "/tmp/socket", "create-x", "business-x", delivery.NewFakeClock().ISO(), delivery.NewFakeClock().ISO())
		if e == nil {
			t.Fatal("unique pending index accepted duplicate")
		}
		detail := strings.TrimPrefix(e.Error(), "constraint failed: ")
		detail = strings.TrimSuffix(detail, " (2067)")
		got = append(got, map[string]any{"error": "IntegrityError", "detail": detail})
		row, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
	} else if scenario == "reservation-raw-register" {
		reg := &registry.Registry{Store: s, Now: delivery.NewFakeClock().ISO}
		_, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host", Cwd: sql.NullString{String: "/parent", Valid: true}}, Child: registry.Endpoint{TaskID: "other", HostID: "host", Cwd: sql.NullString{String: "/tmp/work", Valid: true}}, IssueKey: "REL-1", ArtifactRoots: []string{"/tmp/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "raw"})
		if e == nil {
			t.Fatal("raw registration bypassed pending request")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		row, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
	} else if scenario == "reservation-release" {
		released, e := r.Release(ctx, id.RequestID, id.Fingerprint, 0, "stopped")
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(released)))
		_, e = r.Reserve(ctx, id)
		if e == nil {
			t.Fatal("released request reused")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		row, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
		armedID := id
		armedID.RequestID = "req-armed"
		armedID.IssueKey = "REL-ARMED"
		if _, e := r.Reserve(ctx, armedID); e != nil {
			t.Fatal(e)
		}
		if _, e := r.Arm(ctx, armedID.RequestID, armedID.Fingerprint, 0); e != nil {
			t.Fatal(e)
		}
		_, e = r.Release(ctx, armedID.RequestID, armedID.Fingerprint, 1, "stopped")
		if e == nil {
			t.Fatal("armed request released")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		armedRow, e := s.ManagedStartRequest(ctx, armedID.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(armedRow)))
	} else if scenario == "reservation-attach-conflict" {
		armed, e := r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(armed)))
		recorded, e := r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(recorded)))
		reg := &registry.Registry{Store: s, Now: delivery.NewFakeClock().ISO}
		_, e = reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host", Cwd: sql.NullString{String: "/parent", Valid: true}}, Child: registry.Endpoint{TaskID: "other", HostID: "host", Cwd: sql.NullString{String: "/tmp/work", Valid: true}}, IssueKey: "REL-1", ArtifactRoots: []string{"/tmp/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "business-1", DispatchTurnID: sql.NullString{String: "standby", Valid: true}, ManagedRequestID: "req-1"})
		if e == nil {
			t.Fatal("unpublished child attached")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(e.Error(), "transaction body: ")})
		row, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
	} else if scenario == "reservation-attach" {
		armed, e := r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(armed)))
		recorded, e := r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(recorded)))
		reg := &registry.Registry{Store: s, Now: delivery.NewFakeClock().ISO}
		relation, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: "parent", HostID: "host", Cwd: sql.NullString{String: "/parent", Valid: true}}, Child: registry.Endpoint{TaskID: "child", HostID: "host", Cwd: sql.NullString{String: "/tmp/work", Valid: true}}, IssueKey: "REL-1", ArtifactRoots: []string{"/tmp/work"}, AllowedRecipients: []string{"parent"}, DispatchRequestID: "business-1", DispatchTurnID: sql.NullString{String: "standby", Valid: true}, ManagedRequestID: "req-1"})
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, map[string]any{"relationshipId": relation.ID})
		attached, e := s.ManagedStartRequest(ctx, id.RequestID)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(attached)))
	} else if scenario == "reservation-checkpoint" {
		for range 2 {
			armed, e := r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
			if e != nil {
				t.Fatal(e)
			}
			got = append(got, orderedValue(t, reservationRecord(armed)))
		}
		for range 2 {
			recorded, e := r.Receipt(ctx, id.RequestID, id.Fingerprint, map[string]any{"status": "accepted", "threadId": "child", "turnId": "standby"})
			if e != nil {
				t.Fatal(e)
			}
			got = append(got, orderedValue(t, reservationRecord(recorded)))
		}
		replayed, e := r.Reserve(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(replayed)))
	} else if scenario == "reservation-receipts" {
		armed, e := r.Arm(ctx, id.RequestID, id.Fingerprint, 0)
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(armed)))
		for _, receipt := range []map[string]any{{"status": "unknown", "threadId": "child"}, {"status": "accepted", "threadId": "child"}, {"status": "accepted", "threadId": "child", "turnId": "standby"}} {
			row, e := r.Receipt(ctx, id.RequestID, id.Fingerprint, receipt)
			if e != nil {
				t.Fatal(e)
			}
			got = append(got, orderedValue(t, reservationRecord(row)))
		}
	} else {
		if scenario == "reservation-contention" {
			id.RequestID = "req-2"
		} else {
			id.Fingerprint = "different"
		}
		_, err = r.Reserve(ctx, id)
		if err == nil {
			t.Fatal("changed identity accepted")
		}
		got = append(got, map[string]any{"error": "RegistrationError", "detail": strings.TrimPrefix(err.Error(), "transaction body: ")})
		row, e := s.ManagedStartRequest(ctx, "req-1")
		if e != nil {
			t.Fatal(e)
		}
		got = append(got, orderedValue(t, reservationRecord(row)))
	}
	compareReservationCapture(t, s, want, got)
}
func compareReservationCapture(t *testing.T, s *store.Store, want managedCapture, got []any) {
	t.Helper()
	ctx := context.Background()
	if len(got) != len(want.Receipts) {
		t.Fatalf("Go receipts %d Python %d", len(got), len(want.Receipts))
	}
	for i, expected := range want.Receipts {
		if !reflect.DeepEqual(got[i], expected) {
			t.Errorf("receipt %d Go=%v Python=%v", i, got[i], expected)
		}
	}
	for table, expected := range want.Tables {
		if table != "managed_start_requests" {
			rows, err := s.DB.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			actual := []any{}
			for rows.Next() {
				values := make([]any, len(columns))
				pointers := make([]any, len(columns))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := rows.Scan(pointers...); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				record := map[string]any{}
				for i, key := range columns {
					switch v := values[i].(type) {
					case []byte:
						record[key] = string(v)
					case int64:
						record[key] = float64(v)
					default:
						record[key] = v
					}
				}
				actual = append(actual, record)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			rows.Close()
			wantRows := []any{}
			for _, row := range expected {
				wantRows = append(wantRows, row)
			}
			if !reflect.DeepEqual(actual, wantRows) {
				t.Errorf("table %s Go=%v Python=%v", table, actual, wantRows)
			}
			continue
		}
		rows, err := s.DB.QueryContext(ctx, "SELECT request_id FROM managed_start_requests ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		actual := []any{}
		for _, id := range ids {
			row, err := s.ManagedStartRequest(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			actual = append(actual, orderedValue(t, reservationRecord(row)))
		}
		wantRows := []any{}
		for _, row := range expected {
			wantRows = append(wantRows, row)
		}
		if !reflect.DeepEqual(actual, wantRows) {
			t.Errorf("managed rows Go=%v Python=%v", actual, wantRows)
		}
	}
}
func orderedValue(t *testing.T, value contract.OrderedObject) any {
	t.Helper()
	var output bytes.Buffer
	if err := contract.Emit(&output, value); err != nil {
		t.Fatal(err)
	}
	var result any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func Test27_MST_2_PythonFakeNamingRecoveryWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "naming")
}
func Test27_MST_2_PythonRegisteredShellRecoveryWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "crash-criteria")
}
func Test27_MST_2_PythonBusinessReceiptReplayWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "crash-business")
}
func Test27_MST_3_PythonFakeUncertainFirstTurnWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "uncertain-turn")
}
func Test27_MST_3_PythonFakeUnknownRecoveryWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "unknown-recovery")
}
func Test27_MST_3_PythonFakePausedPartialWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "paused-partial")
}
func Test27_MST_3_PythonFakeUnknownWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "unknown")
}
func Test27_MST_3_PythonFakeStandbyWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "standby")
}
func Test27_MST_4_PythonFakeUnknownLedgerWholeErrorAndRows(t *testing.T) {
	comparePythonManaged(t, "unknown-ledger")
}
func Test27_MST_4_PythonFakeMissingWorkerWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "missing-worker")
}
func Test27_MST_4_PythonFakeLedgerReplacementWholeErrorAndRows(t *testing.T) {
	comparePythonManaged(t, "ledger-replaced")
}
func Test27_MST_4_PythonFakeLedgerRetryWholeErrorAndRows(t *testing.T) {
	comparePythonManaged(t, "ledger-retry")
}
func Test27_MST_4_PythonFakeUnsupportedPolicyWholeErrorAndRows(t *testing.T) {
	comparePythonManaged(t, "unsupported-policy")
}
func Test27_MST_6_PythonFakeScopeDirectionAndRecipients(t *testing.T) {
	comparePythonManaged(t, "recipient-scope")
}
func Test27_MST_6_PythonFakePredeclaredRecipientWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "recipient-predeclared")
}
func Test27_MST_5_PythonFakeAllChangedInputsWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "input-changes")
}
func Test27_MST_5_PythonFakeOriginalSelectorSpellingsWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "selector-spellings")
}
func Test27_MST_5_PythonFakePromptChangeWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "prompt-changed")
}
func Test27_MST_7_PythonFakeSettingsDriftWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "settings-drift")
}
func Test27_MST_7_PythonFakeRegistrationDriftWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "relationship-paused")
}
func Test27_MST_7_PythonFakeApprovalDriftWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "approval-drift")
}
func Test27_MST_7_PythonFakeApprovalDriftPartialWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "approval-drift-partial")
}
func Test27_MST_7_PythonFakeEnvironmentWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "environment")
}
func Test27_MST_7_PythonFakePolicyRestorationWholeReceiptAndRows(t *testing.T) {
	comparePythonManaged(t, "policy-disappears")
}
func Test27_MST_7_PythonFakePauseWholeReceiptAndRows(t *testing.T) { comparePythonManaged(t, "paused") }
func Test27_MST_8_PythonHostReadyRefusalsWholeOutput(t *testing.T) {
	for _, code := range []string{"recipient_archived", "lifecycle_unknown", "recipient_paused", "recipient_cannot_accept_input", "recipient_not_idle"} {
		t.Run(code, func(t *testing.T) {
			_, want := capturePythonManaged(t, "host-ready-"+code)
			host := &managedFake{hostScenario: code}
			gotCode, err := hostReady(context.Background(), host, "child-new")
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]any{"guard": map[string]any{"code": gotCode, "message": "Managed turn withheld: " + gotCode}, "calls": func() []any {
				calls := make([]any, len(host.hostCalls))
				for i, v := range host.hostCalls {
					calls[i] = v
				}
				return calls
			}(), "rpcBudget": float64(10)}
			if !reflect.DeepEqual(got, want.Receipts[1]) {
				t.Fatalf("Go=%v Python=%v", got, want.Receipts[1])
			}
			root := t.TempDir()
			ctx := context.Background()
			s, err := store.Open(ctx, filepath.Join(root, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			raw := requestFixture(t)
			req, err := ParseRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			worker := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(root, "ledger"), "device": 1, "inode": 2}, standby: "completed", hostScenario: code}
			start := &Start{Store: s, Adapter: worker, Socket: filepath.Join(root, "socket"), MarkerRoot: filepath.Join(root, "markers"), StateSelector: root, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
			receipt, err := start.Run(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if orderedValue(t, receipt).(map[string]any)["reason"] != "business_failed" || worker.sent != 0 || len(worker.hostCalls) != len(host.hostCalls) {
				t.Fatalf("host refusal not applied before business send: receipt=%v sent=%d calls=%v", receipt, worker.sent, worker.hostCalls)
			}
		})
	}
}
func Test27_MST_8_PythonGuardCaptureAndGoScopeGuard(t *testing.T) {
	for _, scenario := range []string{"guard-pause", "guard-budget", "guard-drift-parent", "guard-drift-host", "guard-drift-roots", "guard-drift-recipients"} {
		t.Run(scenario, func(t *testing.T) {
			_, want := capturePythonManaged(t, scenario)
			if len(want.Receipts) != 2 {
				t.Fatalf("Python guard capture: %v", want.Receipts)
			}
			outcome := want.Receipts[1]
			guard := obj(outcome["guard"])
			if scenario == "guard-budget" {
				ctx := context.Background()
				state := filepath.Join(t.TempDir(), "state")
				s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				raw := requestFixture(t)
				req, err := ParseRequest(raw)
				if err != nil {
					t.Fatal(err)
				}
				host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(state, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
				start := &Start{Store: s, Adapter: host, Socket: filepath.Join(state, "socket"), MarkerRoot: filepath.Join(state, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
				if _, err := start.Run(ctx, raw); err != nil {
					t.Fatal(err)
				}
				if float64(host.guardBudget) != outcome["rpcBudget"] {
					t.Fatalf("Go guard budget=%d Python=%v", host.guardBudget, outcome["rpcBudget"])
				}
				calls := outcome["calls"].([]any)
				if outcome["rpcBudget"] != float64(10) || len(calls) != 10 || guard != nil {
					t.Fatal(outcome)
				}
				for i, call := range calls {
					if i < 8 && call != "thread/list" || i == 8 && call != "thread/goal/get" || i == 9 && call != "thread/read" {
						t.Fatalf("host RPC order: %v", calls)
					}
				}
				return
			}
			if scenario == "guard-pause" {
				if guard["code"] != "recipient_paused" || outcome["workerFinished"] != true || outcome["workerError"] != nil {
					t.Fatal(outcome)
				}
				ctx := context.Background()
				state := filepath.Join(t.TempDir(), "state")
				s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				raw := requestFixture(t)
				req, err := ParseRequest(raw)
				if err != nil {
					t.Fatal(err)
				}
				host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(state, "ledger"), "device": 1, "inode": 2}, standby: "completed", beforeStartWorker: true}
				ready := true
				host.beforeSend = func(SendRequest) { ready = false }
				start := &Start{Store: s, Adapter: host, Socket: filepath.Join(state, "socket"), MarkerRoot: filepath.Join(state, "markers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) {
					if !ready {
						return "recipient_paused", nil
					}
					return "", nil
				}}
				receipt, err := start.Run(ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]any{}
				for _, item := range receipt {
					got[item.Key] = item.Value
				}
				if got["state"] == "admitted" || got["reason"] != "business_failed" || host.sent != 0 {
					t.Fatalf("Go worker guard %v sent=%d Python=%v", got, host.sent, outcome)
				}
			} else {
				if guard["code"] != "managed_scope_changed" {
					t.Fatal(outcome)
				}
				change := map[string][2]string{"guard-drift-parent": {"parent_task_id", "replacement-parent"}, "guard-drift-host": {"child_host_id", "other-host"}, "guard-drift-roots": {"artifact_roots", `["/other-scope"]`}, "guard-drift-recipients": {"allowed_recipients", `["unrelated"]`}}[scenario]
				checkManagedScopeDrift(t, change[0], change[1])
			}
		})
	}
}
func comparePythonManaged(t *testing.T, scenario string) {
	root, want := capturePythonManaged(t, scenario)
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "CODEX_HOME"} {
		t.Setenv(key, root)
	}
	ctx := context.Background()
	state := filepath.Join(root, "gostate")
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := pythonFixture(root)
	if scenario == "unsupported-policy" {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		obj(obj(request["child"])["settings"])["sandbox"] = map[string]any{"type": "readOnly", "networkAccess": true}
		raw, _ = json.Marshal(request)
	}
	if scenario == "recipient-predeclared" {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		request["allowedRecipients"] = []any{"01parent-task", "child-new"}
		raw, _ = json.Marshal(request)
	}
	req, err := ParseRequest(raw)
	if scenario == "unsupported-policy" && err != nil {
		got := map[string]any{"error": "UntransmittableSetting", "detail": err.Error()}
		if !reflect.DeepEqual(got, want.Receipts[0]) {
			t.Fatalf("Go error=%v Python=%v", got, want.Receipts[0])
		}
		for table, rows := range want.Tables {
			if len(rows) != 0 {
				t.Fatalf("Python wrote %s: %v", table, rows)
			}
			var count int
			if e := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&count); e != nil || count != 0 {
				t.Fatalf("Go wrote %s: count=%d err=%v", table, count, e)
			}
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: map[string]any{"path": want.Ledger.Path, "realPath": want.Ledger.Path, "device": int64(want.Ledger.Device), "inode": int64(want.Ledger.Inode)}, standby: "completed"}
	if strings.HasPrefix(scenario, "host-ready-") {
		host.hostScenario = strings.TrimPrefix(scenario, "host-ready-")
	}
	if scenario == "unknown-ledger" {
		host.ledger = nil
	}
	clock := delivery.NewFakeClock()
	ready := true
	start := &Start{Store: s, Adapter: host, Now: clock.ISO, Socket: filepath.Join(root, "socket"), MarkerRoot: filepath.Join(root, "gomarkers"), StateSelector: state, Readiness: func(context.Context, map[string]any) (string, error) {
		if !ready {
			return "worker_policy_unconfigured", nil
		}
		return "", nil
	}}
	if scenario == "policy-disappears" {
		host.onCreate = func() { ready = false }
	}
	if scenario == "missing-worker" {
		ready = false
	}
	if scenario == "ledger-replaced" {
		host.onGetOperation = func(id string) {
			if strings.HasPrefix(id, "managed-create-") {
				host.ledger = map[string]any{"path": want.Ledger.Path, "realPath": want.Ledger.Path, "device": int64(want.Ledger.Device), "inode": int64(want.Ledger.Inode) + 1}
			}
		}
	}
	var got []any
	var orderedReceipts []string
	if scenario == "unknown" || scenario == "ledger-retry" {
		host.creationStatus = "outcome_unknown"
	}
	if scenario == "naming" || scenario == "uncertain-turn" || scenario == "unknown-recovery" || scenario == "paused-partial" {
		host.partial = true
	}
	if scenario == "uncertain-turn" {
		host.attemptedTurn = true
	}
	if scenario == "unknown-recovery" {
		host.sendStatus = "outcome_unknown"
	}
	if scenario == "paused-partial" {
		host.paused = true
	}
	if strings.HasPrefix(scenario, "guard-") || scenario == "crash-criteria" || scenario == "standby" || scenario == "relationship-paused" || scenario == "settings-drift" || scenario == "prompt-changed" || scenario == "input-changes" || scenario == "selector-spellings" {
		host.standby = "inProgress"
	}
	if scenario == "paused" {
		host.paused = true
	}
	if scenario == "environment" {
		host.creationEnvironmentChanged = true
	}
	if scenario == "approval-drift" || scenario == "approval-drift-partial" {
		host.settings = map[string]any{}
		for k, v := range obj(obj(req["child"])["settings"]) {
			host.settings[k] = v
		}
		host.settings["approvalPolicy"] = "on-request"
		if scenario == "approval-drift-partial" {
			host.partial = true
		}
	}
	baseStart := start
	for index := range len(want.Receipts) {
		if scenario == "cli-missing-selectors" && index > 0 {
			var selectors []string
			switch index {
			case 2:
				selectors = []string{"--state", filepath.Join(root, "absent")}
			case 3:
				selectors = []string{"--socket", filepath.Join(root, "socket")}
			}
			argv := append(selectors, "managed-start", "--request", "{}", "--marker-root", filepath.Join(root, "gomarkers"))
			var out, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", argv, &out, &stderr)
			var result any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			got = append(got, map[string]any{"exit": float64(code), "stdout": result})
			continue
		}
		if scenario == "cli-missing-worker" && index == 1 {
			absent := filepath.Join(root, "absent")
			var out, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", []string{"--state", absent, "--socket", filepath.Join(root, "socket"), "managed-start", "--request", string(raw), "--marker-root", filepath.Join(root, "gomarkers")}, &out, &stderr)
			var result any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(absent, "relay.sqlite3"))
			got = append(got, map[string]any{"exit": float64(code), "stdout": result, "storeExists": !os.IsNotExist(statErr)})
			continue
		}
		if scenario == "cli-unknown-input" && index == 1 {
			var changed map[string]any
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			changed["overridePermissions"] = true
			invalid, _ := json.Marshal(changed)
			absent := filepath.Join(root, "absent")
			var out, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", []string{"--state", absent, "--socket", filepath.Join(root, "socket"), "managed-start", "--request", string(invalid), "--marker-root", filepath.Join(root, "gomarkers")}, &out, &stderr)
			var result any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(absent, "relay.sqlite3"))
			got = append(got, map[string]any{"exit": float64(code), "stdout": result, "storeExists": !os.IsNotExist(statErr)})
			continue
		}
		if scenario == "show-absent" && index == 1 {
			absent := filepath.Join(root, "absent")
			var out, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", []string{"--state", absent, "managed-show", "--request-id", "missing"}, &out, &stderr)
			var result any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(absent, "relay.sqlite3"))
			got = append(got, map[string]any{"exit": float64(code), "stdout": result, "storeExists": !os.IsNotExist(statErr)})
			continue
		}
		if scenario == "show-after-start" && index == 1 {
			var out, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", []string{"--state", state, "managed-show", "--request-id", "managed-1"}, &out, &stderr)
			var result any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			got = append(got, map[string]any{"exit": float64(code), "stdout": result})
			continue
		}
		start = baseStart
		currentRaw := raw
		if scenario == "input-changes" && index > 0 {
			var changed map[string]any
			if e := json.Unmarshal(raw, &changed); e != nil {
				t.Fatal(e)
			}
			switch index {
			case 1:
				changed["criteriaSource"] = "new-source"
			case 2:
				changed["scopeRef"] = "new-scope"
			case 3:
				changed["baselineRevision"] = "new-base"
			case 4:
				changed["prompt"] = "different"
			case 5:
				changed["criteria"].([]any)[0].(map[string]any)["required"] = false
			}
			currentRaw, _ = json.Marshal(changed)
		}
		if scenario == "selector-spellings" && index > 0 {
			alias := filepath.Join(root, "spelling")
			if e := os.MkdirAll(alias, 0700); e != nil {
				t.Fatal(e)
			}
			changed := *baseStart
			switch index {
			case 1:
				changed.Socket = alias + "/../socket"
			case 2:
				changed.MarkerRoot = alias + "/../gomarkers"
			case 3:
				changed.StateSelector = alias + "/.."
			}
			start = &changed
		}
		if (scenario == "standby" || scenario == "crash-criteria") && index == 1 {
			host.standby = "completed"
		}
		if scenario == "ledger-retry" && index == 1 {
			host.ledger["inode"] = int64(want.Ledger.Inode) + 1
			host.operations = map[string]map[string]any{}
		}
		if scenario == "policy-disappears" && index == 1 {
			ready = true
		}
		if scenario == "relationship-paused" && index == 1 {
			_, e := s.DB.ExecContext(ctx, "UPDATE relationships SET status='paused' WHERE issue_key=?", "REL-MANAGED")
			if e != nil {
				t.Fatal(e)
			}
		}
		if scenario == "prompt-changed" && index == 1 {
			var changed map[string]any
			if e := json.Unmarshal(raw, &changed); e != nil {
				t.Fatal(e)
			}
			changed["prompt"] = "different assignment"
			currentRaw, _ = json.Marshal(changed)
		}
		if scenario == "settings-drift" && index == 1 {
			var stored string
			if e := s.DB.QueryRowContext(ctx, "SELECT settings FROM authorized_settings WHERE task_id='child-new'").Scan(&stored); e != nil {
				t.Fatal(e)
			}
			var changed map[string]any
			if e := json.Unmarshal([]byte(stored), &changed); e != nil {
				t.Fatal(e)
			}
			sandbox := changed["sandbox"].(map[string]any)
			sandbox["networkAccess"] = true
			encoded, e := settingsJSON(changed)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.DB.ExecContext(ctx, "UPDATE authorized_settings SET settings=?, source='user_transition' WHERE task_id='child-new'", encoded); e != nil {
				t.Fatal(e)
			}
		}
		receipt, e := start.Run(ctx, currentRaw)
		if e != nil {
			if scenario == "ledger-retry" && index == 1 {
				if e.Error() != "relationship_conflict: managed request id belongs to different input or selectors" {
					t.Fatalf("wrong Go retry error: %v", e)
				}
				got = append(got, map[string]any{"error": "RegistrationError", "detail": e.Error()})
				continue
			}
			if (scenario == "unknown-ledger" || scenario == "ledger-replaced") && index == 0 {
				if scenario == "ledger-replaced" {
					if e.Error() != "relationship_conflict: ledger replaced" {
						t.Fatalf("wrong Go ledger error: %v", e)
					}
					got = append(got, map[string]any{"error": "HostUnavailable", "detail": "ledger replaced"})
					continue
				}
				got = append(got, map[string]any{"error": "ValueError", "detail": e.Error()})
				continue
			}
			if (scenario == "settings-drift" || scenario == "prompt-changed" || scenario == "input-changes" || scenario == "selector-spellings") && index > 0 {
				detail := strings.TrimPrefix(e.Error(), "transaction body: ")
				got = append(got, map[string]any{"error": "RegistrationError", "detail": detail})
				continue
			}
			t.Fatal(e)
		}
		var buf bytes.Buffer
		if e := contract.Emit(&buf, receipt); e != nil {
			t.Fatal(e)
		}
		var value any
		if e := json.Unmarshal(buf.Bytes(), &value); e != nil {
			t.Fatal(e)
		}
		got = append(got, value)
		var compact bytes.Buffer
		if e := json.Compact(&compact, buf.Bytes()); e != nil {
			t.Fatal(e)
		}
		orderedReceipts = append(orderedReceipts, compact.String())
	}
	if scenario == "happy" {
		if len(want.OrderedReceipts) != len(got) {
			t.Fatalf("ordered receipts missing")
		}
		for i, raw := range orderedReceipts {
			python := want.OrderedReceipts[i]
			// Only independently generated physical values differ; retain every JSON key
			// and its position while normalizing those strings.
			var goValue, pyValue any
			if e := json.Unmarshal([]byte(raw), &goValue); e != nil {
				t.Fatal(e)
			}
			if e := json.Unmarshal([]byte(python), &pyValue); e != nil {
				t.Fatal(e)
			}
			goFingerprint := str(obj(goValue)["requestFingerprint"])
			pyFingerprint := str(obj(pyValue)["requestFingerprint"])
			raw = strings.ReplaceAll(raw, goFingerprint, "<fingerprint>")
			python = strings.ReplaceAll(python, pyFingerprint, "<fingerprint>")
			raw = strings.ReplaceAll(strings.ReplaceAll(raw, "gomarkers", "markers"), "gostate", "state")
			if raw != python {
				t.Errorf("ordered receipt %d Go=%s Python=%s", i, raw, python)
			}
		}
	}
	var expected []any
	for _, v := range want.Receipts {
		expected = append(expected, v)
	}
	if !reflect.DeepEqual(normalizedManaged(got), normalizedManaged(expected)) {
		a, _ := json.MarshalIndent(normalizedManaged(got), "", "  ")
		b, _ := json.MarshalIndent(normalizedManaged(expected), "", "  ")
		t.Errorf("complete receipts differ\nGo: %s\nPython: %s", a, b)
	}
	if host.created != want.Effects[0] || host.sent != want.Effects[1] {
		t.Errorf("host effects Go=%d/%d Python=%v", host.created, host.sent, want.Effects)
	}
	if scenario == "recipient-scope" {
		var child, parent, recipients string
		if e := s.DB.QueryRowContext(ctx, "SELECT child_task_id,parent_task_id,allowed_recipients FROM relationships WHERE issue_key='REL-MANAGED'").Scan(&child, &parent, &recipients); e != nil {
			t.Fatal(e)
		}
		var people []string
		if e := json.Unmarshal([]byte(recipients), &people); e != nil {
			t.Fatal(e)
		}
		outcomes := []any{}
		for _, step := range []struct{ kind, recipient string }{{"revision_request", "child-new"}, {"completion_event", "01parent-task"}, {"revision_request", "unrelated"}} {
			answer := map[string]any{"kind": step.kind, "recipient": step.recipient}
			eligible := (step.kind == "revision_request" && step.recipient == child) || (step.kind == "completion_event" && step.recipient == parent)
			allowed := false
			for _, who := range people {
				allowed = allowed || who == step.recipient
			}
			if eligible && allowed {
				answer["result"] = "allowed"
			} else {
				answer["error"] = "ScopeError"
				relationship := want.Receipts[0]["relationshipId"]
				answer["detail"] = fmt.Sprintf("recipient_not_authorized: a revision_request for '%s' goes to its own child '%s', not to '%s'", relationship, child, step.recipient)
			}
			outcomes = append(outcomes, answer)
		}
		python := []any{}
		for _, entry := range want.Scope {
			python = append(python, entry)
		}
		if !reflect.DeepEqual(outcomes, python) {
			t.Errorf("scope outcomes Go=%v Python=%v", outcomes, python)
		}
	}
	for _, table := range []string{"managed_start_requests", "relationships", "generations", "canonical_criteria", "verification_mode", "authorized_settings", "generation_turns"} {
		rows, err := s.DB.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		actual := []any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			record := map[string]any{}
			for i, key := range columns {
				if text, ok := values[i].([]byte); ok {
					values[i] = string(text)
				}
				if number, ok := values[i].(int64); ok {
					record[key] = float64(number)
				} else {
					record[key] = values[i]
				}
			}
			actual = append(actual, record)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		expected := []any{}
		for _, v := range want.Tables[table] {
			expected = append(expected, v)
		}
		if !reflect.DeepEqual(normalizedManaged(actual), normalizedManaged(expected)) {
			a, _ := json.MarshalIndent(normalizedManaged(actual), "", "  ")
			b, _ := json.MarshalIndent(normalizedManaged(expected), "", "  ")
			t.Errorf("table %s differs\nGo: %s\nPython: %s", table, a, b)
		}
	}
}
