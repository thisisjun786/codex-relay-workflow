package managed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// managedTree is the tree a scenario runs in: a fresh directory holding the workspace the request
// names as its cwd.
func managedTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// checkManaged compares a scenario's value with its golden, with the scenario's tree as <root>.
func checkManaged(t *testing.T, key string, value any, root string) {
	t.Helper()
	golden.CheckJSON(t, key, value, golden.Substitute(root, "<root>"))
}

// managedTables are the tables a managed start writes, each compared whole with its golden.
var managedTables = []string{"managed_start_requests", "relationships", "generations", "canonical_criteria", "verification_mode", "authorized_settings", "generation_turns"}

// ledgerDevice and ledgerInode are the physical identity of the operations ledger the fake host
// reports; a replaced ledger is the same file with inode ledgerInode+1.
const (
	ledgerDevice = 4242
	ledgerInode  = 777000
)

// ledgerIdentity is the operations ledger the fake host reports: a file in the workspace.
func ledgerIdentity(root string, inode int64) map[string]any {
	path := filepath.Join(root, "workspace", "test-operations-ledger")
	return map[string]any{"path": path, "realPath": path, "device": int64(ledgerDevice), "inode": inode}
}

// rootDeviceToken stands in a golden for the device of the tree the scenario ran in.
const rootDeviceToken = "<root device>"

// maskRootDevice replaces each device or logDevice that is the device of the scenario's tree
// (device, in decimal) with rootDeviceToken.
func maskRootDevice(v any, device string) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, value := range x {
			if f, ok := value.(float64); ok && (k == "device" || k == "logDevice") && strconv.FormatInt(int64(f), 10) == device {
				out[k] = rootDeviceToken
				continue
			}
			out[k] = maskRootDevice(value, device)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = maskRootDevice(item, device)
		}
		return out
	default:
		return v
	}
}

func deviceOf(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("no device for %s", path)
	}
	return strconv.FormatUint(uint64(stat.Dev), 10), nil
}

// scenarioRequest is the managed-start request every managed scenario starts from.
func scenarioRequest(root string) []byte {
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

// MEX-1..3: the one instructed execution-cli sequence covers them all: the instructed
// sequence, the restart from a marker (receipt 8) and the duplicate child (receipts 9 and 10).
func Test27_MEX_1_PythonInstructedSequenceWholeOutput(t *testing.T) {
	compareExecutionCLI(t)
}

// compareExecutionCLI runs the instructed sequence through the real CLI on a real store and
// compares every step's output with the golden, normalized by normalizedExecution; the device of
// the tree the store lives in is <root device> and the marker directory's workspace key, a digest
// of the workspace's path, is <execution workspace key>.
func compareExecutionCLI(t *testing.T) {
	root := managedTree(t)
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
	// doctor's ownership.runtime_build names the answering runtime (decisions.md 31): Go's own
	// build, never the fence build. normalizedExecution then compares it as answeringBuild.
	for _, i := range []int{0, 6} {
		goBuild := obj(obj(obj(got[i])["stdout"])["ownership"])["runtime_build"]
		if goBuild != goRuntimeBuild() || goBuild == ownership.CompatibilityBuild {
			t.Errorf("step %d doctor runtime_build: %v (want %q)", i, goBuild, goRuntimeBuild())
		}
	}
	steps := make([]any, len(got))
	for i, step := range got {
		steps[i] = normalizedExecution(t, testsupport.Go, step, goRuntimeBuild())
	}
	device, err := deviceOf(root)
	if err != nil {
		t.Fatal(err)
	}
	key, err := delivery.WorkspaceKey(workspace)
	if err != nil {
		t.Fatal(err)
	}
	golden.CheckJSON(t, "steps", maskRootDevice(steps, device), golden.Substitute(key, "<execution workspace key>"), golden.Substitute(root, "<root>"))
	goBefore := obj(obj(got[0])["stdout"])
	goIssue := obj(goBefore["issue"])
	if goIssue["readable"] != false || goIssue["holds"] != nil || obj(got[1])["storeExists"] != false {
		t.Fatal("pre-registration diagnosis created a store", goBefore)
	}
	goAfter := obj(obj(got[6])["stdout"])
	goOwner := obj(goAfter["issue"])
	goFound := obj(obj(got[7])["stdout"])
	if goOwner["holds"] != true || goOwner["responsibleChild"] != "child" || goOwner["storeAgreement"] != "same" || goOwner["storeId"] != obj(goAfter["store"])["storeId"] || goFound["responsibleRelationship"] != goOwner["responsibleRelationship"] || len(goFound["assignments"].([]any)) != 1 {
		t.Fatalf("assignment mismatch: %v %v", goOwner, goFound)
	}
	goMarkerBody := obj(obj(got[8])["stdout"])
	if !strings.Contains(fmt.Sprint(goMarkerBody), str(goOwner["responsibleRelationship"])) || !strings.Contains(fmt.Sprint(goMarkerBody), filepath.Join(root, "goexecution", "relay.sqlite3")) {
		t.Fatalf("marker restart: %v", goMarkerBody)
	}
	goDuplicate := obj(obj(got[9])["stdout"])
	if obj(got[9])["exit"] != float64(2) || goDuplicate["reason"] != "duplicate_assignment" || len(obj(obj(got[10])["stdout"])["assignments"].([]any)) != 1 {
		t.Fatalf("duplicate assignment: %v", goDuplicate)
	}
}

var executionTime = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|\+00:00)`)

// goRuntimeBuild is the build doctor's ownership.runtime_build names for the Go runtime: the
// holder-identity build, else the version (cli doctor.go runtimeBuild).
func goRuntimeBuild() string {
	if cli.Build != "" {
		return cli.Build
	}
	return cli.Version
}

// answeringBuild stands for doctor's ownership.runtime_build when it names the runtime that
// answered, as decisions.md 31 documents: the Python fence build from Python, Go's own build
// from Go. Any other value is kept, so a runtime naming the wrong build still differs.
const answeringBuild = "<answering runtime build>"

// normalizedExecution normalizes the step output of writer, the runtime that produced it; build
// is the runtime_build that runtime's doctor names.
func normalizedExecution(t *testing.T, writer testsupport.Runtime, v any, build string) any {
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
			if block, ok := value.(map[string]any); ok && k == "ownership" {
				// doctor's ownership block is the store's schema_meta ownership rows as read.
				// Each runtime diagnoses the store it created and owns, so the owner row is the
				// one runtime-identity difference and must be that runtime's own; every other
				// key is compared as it is. Before any store exists (step 0) the block reads no
				// stamp, owner null among the rest, and names no runtime: compared as it is.
				// runtime_build names the answering runtime (decisions.md 31), not the store.
				rows := map[string]any{}
				for key, row := range block {
					if block["owner"] != nil {
						row = testsupport.OwnerNeutral(t, writer, key, row)
					}
					rows[key] = row
				}
				if rows["runtime_build"] == build {
					rows["runtime_build"] = answeringBuild
				}
				value = rows
			}
			out[k] = normalizedExecution(t, writer, value, build)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizedExecution(t, writer, item, build)
		}
		return out
	case string:
		x = strings.ReplaceAll(strings.ReplaceAll(x, "goexecution-markers", "execution-markers"), "goexecution", "execution")
		return executionTime.ReplaceAllString(x, "<time>")
	default:
		return x
	}
}
func Test27_MST_1_PythonFakeWholeReceiptAndRows(t *testing.T) { compareManaged(t, "happy", 2) }
func Test27_MST_10_PythonShowAfterStartWholeOutputAndRows(t *testing.T) {
	compareManaged(t, "show-after-start", 2)
}
func Test27_MST_10_PythonShowAbsentWholeOutputAndRows(t *testing.T) {
	compareManaged(t, "show-absent", 2)
}
func Test27_MST_9_PythonUnknownInputWholeOutputAndRows(t *testing.T) {
	compareManaged(t, "cli-unknown-input", 2)
}
func Test27_MST_9_PythonMissingWorkerWholeOutputAndRows(t *testing.T) {
	compareManaged(t, "cli-missing-worker", 2)
}
func Test27_MST_9_PythonMissingSelectorsWholeOutputAndRows(t *testing.T) {
	compareManaged(t, "cli-missing-selectors", 4)
}
func Test27_MRS_1_PythonReservationReplayWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-replay")
}
func Test27_MRS_1_PythonCheckpointReplayWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-checkpoint")
}
func Test27_MRS_2_PythonReservationContentionWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-contention")
}
func Test27_MRS_2_PythonActiveOwnerWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-active-owner")
}
func Test27_MRS_2_PythonRawRegisterWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-raw-register")
}
func Test27_MRS_2_PythonUniqueIndexWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-index")
}
func Test27_MRS_2_PythonResumeBlockedWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-resume")
}
func Test27_MRS_2_PythonTwoConnectionsWholeOutputAndRows(t *testing.T) {
	root := managedTree(t)
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
	if len(got) != 2 {
		t.Fatalf("two racers, outcomes %v", got)
	}
	winner := str(obj(obj(got[0])["ok"])["request_id"])
	if winner != "req-a" && winner != "req-b" {
		t.Fatalf("no winner: %v", got)
	}
	golden.CheckJSON(t, "outcomes", raceWinnerFirst(t, got, winner))
	goLoser := obj(got[1])
	goDetail := str(goLoser["detail"])
	if goLoser["error"] != "RegistrationError" || !strings.HasPrefix(goDetail, "duplicate_assignment: issue 'REL-1' is already held by request") {
		t.Fatalf("unexpected Go refusal: %v", goLoser)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM managed_start_requests").Scan(&count); err != nil || count != 1 {
		t.Fatalf("Go pending=%d err=%v", count, err)
	}
}

// raceWinnerFirst is the two racers' outcomes, the winner's first, with the racers' names
// exchanged when req-b won, so the golden reads the same whichever thread reserved first.
func raceWinnerFirst(t *testing.T, outcomes []any, winner string) any {
	t.Helper()
	raw, err := json.Marshal(outcomes)
	if err != nil {
		t.Fatal(err)
	}
	if winner == "req-b" {
		raw = bytes.ReplaceAll(raw, []byte("req-a"), []byte("req-?"))
		raw = bytes.ReplaceAll(raw, []byte("req-b"), []byte("req-a"))
		raw = bytes.ReplaceAll(raw, []byte("req-?"), []byte("req-b"))
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func Test27_MRS_3_PythonReservationReceiptsWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-receipts")
}
func Test27_MRS_3_PythonReservationAttachWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-attach")
}
func Test27_MRS_3_PythonAttachConflictWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-attach-conflict")
}
func Test27_MRS_4_PythonReservationReleaseWholeOutputAndRows(t *testing.T) {
	compareReservation(t, "reservation-release")
}
func Test27_MRS_4_PythonArmReleaseRaceWholeOutputAndRows(t *testing.T) {
	root := managedTree(t)
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
	if wins != 1 || losses != 1 || row.Revision != 1 {
		t.Fatalf("Go wins=%d losses=%d row=%v", wins, losses, row)
	}
	if winner.action == "arm" && row.State != "create_armed" || winner.action == "release" && (row.State != "released" || row.ReleaseReason.String != "operator") {
		t.Fatalf("winner=%v row=%v", winner.action, row)
	}
	gotRow := obj(orderedValue(t, reservationRecord(row)))
	if gotRow["state"] != "create_armed" && gotRow["state"] != "released" {
		t.Fatalf("Go retained invalid race row: %v", gotRow)
	}
	golden.CheckJSON(t, "row", raceRow(gotRow))
	if !strings.Contains(loser.err.Error(), "relationship_conflict") {
		t.Fatalf("Go loser=%v", loser.err)
	}
}

// raceRow is a reservation row the arm/release race left, with the two columns whose value
// depends on which racer won masked; the test checks them against the winner itself.
func raceRow(row map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range row {
		if key == "state" || key == "release_reason" {
			value = "<the winner's>"
		}
		out[key] = value
	}
	return out
}
func Test27_MRS_5_PythonEnsureSettingsWholeOutputAndRows(t *testing.T) {
	root := managedTree(t)
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(root, "gostate", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req, err := ParseRequest(scenarioRequest(root))
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
	checkManaged(t, "receipts", got, root)
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
	checkManaged(t, "table authorized_settings", actual, root)
}

// compareReservation runs a reservation scenario on a fresh store and compares its receipts and
// every managed table with the golden.
func compareReservation(t *testing.T, scenario string) {
	root := managedTree(t)
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
		checkReservation(t, s, got)
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
		checkReservation(t, s, got)
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
	checkReservation(t, s, got)
}
func checkReservation(t *testing.T, s *store.Store, got []any) {
	t.Helper()
	ctx := context.Background()
	golden.CheckJSON(t, "receipts", got)
	for _, table := range managedTables {
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
			golden.CheckJSON(t, "table "+table, actual)
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
		golden.CheckJSON(t, "table managed_start_requests", actual)
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
	compareManaged(t, "naming", 2)
}
func Test27_MST_2_PythonRegisteredShellRecoveryWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "crash-criteria", 2)
}
func Test27_MST_2_PythonBusinessReceiptReplayWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "crash-business", 2)
}
func Test27_MST_3_PythonFakeUncertainFirstTurnWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "uncertain-turn", 2)
}
func Test27_MST_3_PythonFakeUnknownRecoveryWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "unknown-recovery", 2)
}
func Test27_MST_3_PythonFakePausedPartialWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "paused-partial", 2)
}
func Test27_MST_3_PythonFakeUnknownWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "unknown", 2)
}
func Test27_MST_3_PythonFakeStandbyWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "standby", 2)
}
func Test27_MST_4_PythonFakeUnknownLedgerWholeErrorAndRows(t *testing.T) {
	compareManaged(t, "unknown-ledger", 1)
}
func Test27_MST_4_PythonFakeMissingWorkerWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "missing-worker", 1)
}
func Test27_MST_4_PythonFakeLedgerReplacementWholeErrorAndRows(t *testing.T) {
	compareManaged(t, "ledger-replaced", 1)
}
func Test27_MST_4_PythonFakeLedgerRetryWholeErrorAndRows(t *testing.T) {
	compareManaged(t, "ledger-retry", 2)
}
func Test27_MST_4_PythonFakeUnsupportedPolicyWholeErrorAndRows(t *testing.T) {
	compareManaged(t, "unsupported-policy", 1)
}
func Test27_MST_6_PythonFakeScopeDirectionAndRecipients(t *testing.T) {
	compareManaged(t, "recipient-scope", 1)
}
func Test27_MST_6_PythonFakePredeclaredRecipientWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "recipient-predeclared", 1)
}
func Test27_MST_5_PythonFakeAllChangedInputsWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "input-changes", 6)
}
func Test27_MST_5_PythonFakeOriginalSelectorSpellingsWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "selector-spellings", 4)
}
func Test27_MST_5_PythonFakePromptChangeWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "prompt-changed", 2)
}
func Test27_MST_7_PythonFakeSettingsDriftWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "settings-drift", 2)
}
func Test27_MST_7_PythonFakeRegistrationDriftWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "relationship-paused", 2)
}
func Test27_MST_7_PythonFakeApprovalDriftWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "approval-drift", 1)
}
func Test27_MST_7_PythonFakeApprovalDriftPartialWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "approval-drift-partial", 1)
}
func Test27_MST_7_PythonFakeEnvironmentWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "environment", 1)
}
func Test27_MST_7_PythonFakePolicyRestorationWholeReceiptAndRows(t *testing.T) {
	compareManaged(t, "policy-disappears", 2)
}
func Test27_MST_7_PythonFakePauseWholeReceiptAndRows(t *testing.T) { compareManaged(t, "paused", 1) }
func Test27_MST_8_PythonHostReadyRefusalsWholeOutput(t *testing.T) {
	for _, code := range []string{"recipient_archived", "lifecycle_unknown", "recipient_paused", "recipient_cannot_accept_input", "recipient_not_idle"} {
		t.Run(code, func(t *testing.T) {
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
			golden.CheckJSON(t, "guard", got)
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
				golden.CheckJSON(t, "rpcBudget", host.guardBudget)
				return
			}
			if scenario == "guard-pause" {
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
					t.Fatalf("Go worker guard %v sent=%d", got, host.sent)
				}
			} else {
				change := map[string][2]string{"guard-drift-parent": {"parent_task_id", "replacement-parent"}, "guard-drift-host": {"child_host_id", "other-host"}, "guard-drift-roots": {"artifact_roots", `["/other-scope"]`}, "guard-drift-recipients": {"allowed_recipients", `["unrelated"]`}}[scenario]
				checkManagedScopeDrift(t, change[0], change[1])
			}
		})
	}
}

// compareManaged runs a managed-start scenario of the given number of steps against the fake host
// and compares its receipts, the host's effects and every managed table with the golden.
func compareManaged(t *testing.T, scenario string, steps int) {
	root := managedTree(t)
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
	raw := scenarioRequest(root)
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
		checkManaged(t, "receipts", []any{got}, root)
		for _, table := range managedTables {
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
	host := &managedFake{operations: map[string]map[string]any{}, settings: obj(obj(req["child"])["settings"]), ledger: ledgerIdentity(root, ledgerInode), standby: "completed"}
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
				host.ledger = ledgerIdentity(root, ledgerInode+1)
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
	for index := range steps {
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
			host.ledger["inode"] = int64(ledgerInode + 1)
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
		// Each receipt as emitted, key order included; the request fingerprint, a digest of the
		// physical request, is <fingerprint>.
		ordered := make([]string, len(orderedReceipts))
		for i, raw := range orderedReceipts {
			var value any
			if e := json.Unmarshal([]byte(raw), &value); e != nil {
				t.Fatal(e)
			}
			raw = strings.ReplaceAll(raw, str(obj(value)["requestFingerprint"]), "<fingerprint>")
			ordered[i] = strings.ReplaceAll(strings.ReplaceAll(raw, "gomarkers", "markers"), "gostate", "state")
		}
		checkManaged(t, "ordered receipts", ordered, root)
	}
	checkManaged(t, "receipts", normalizedManaged(got), root)
	golden.CheckJSON(t, "effects", []int{host.created, host.sent})
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
				relationship := obj(got[0])["relationshipId"]
				answer["detail"] = fmt.Sprintf("recipient_not_authorized: a revision_request for '%s' goes to its own child '%s', not to '%s'", relationship, child, step.recipient)
			}
			outcomes = append(outcomes, answer)
		}
		golden.CheckJSON(t, "scope", outcomes)
	}
	for _, table := range managedTables {
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
		checkManaged(t, "table "+table, normalizedManaged(actual), root)
	}
}
