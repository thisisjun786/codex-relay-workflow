package gui

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/policystore"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// The status tests drive GET /api/status through the real guard over the package registry,
// with the manage seam replaced, so no test runs a manage command against a real host. The
// seam is manage-shaped, so the argument list each source uses is asserted from the fake.

// The exact argument list every source must use. The two commands that can write state are
// called only in their non-writing form.
const (
	statusArgRelay    = "relay-read"
	statusArgCapacity = "capacity --dry-run"
	statusArgDag      = "dag-review --no-state"
	statusArgHostRead = "host-read --method thread/loaded/list"
)

// The fake documents each source answers with. They are small but carry the values the screen
// renders, so a pass-through is observable.
const (
	fakeRelayDocument = `{"schema":"crw-relay-read/1","stateDir":"/tmp/relay","readAt":"2026-10-07T00:00:00Z",` +
		`"bindings":[{"bindingId":"b-1","role":"parent","read":{"state":"ok"}}],` +
		`"relationships":[{"relationshipId":"rel-1","issueKey":"CRW-1","executionGeneration":1,` +
		`"head":{"revisionHash":"deadbeef"},"nextExpectedAction":"await_receipt","read":{"state":"ok"}}],` +
		`"plans":[{"planId":"p-1","revision":4,"stages":{"released":2,"integrated":1},"blocked":0,"denominator":5,"read":{"state":"ok"}}],` +
		`"mergeTurns":[{"turnId":"t-1","prNumber":7,"holderTaskId":"task-1","state":"holding","read":{"state":"ok"}}],` +
		`"failures":[]}`
	fakeCapacityDocument = `{"at":"2026-10-07T00:00:00Z","limits":{"min_waiting":2,"lane_max":9},` +
		`"lane":{"merges_last_hour":3},"actions":{"state":"within"},"child_429":{"state":"within","count":null},` +
		`"plans":[{"plan":"p-1","verdict":"hold","waiting":[],"held":2,"ceiling":9,"reasons":[]}]}`
	fakeDagDocument = `{"plans":[{"plan":"p-1","revision":4,"nodes":5,"edges":6,"released":2,"accepted":1,"integrated":1}],` +
		`"anomalies":[{"kind":"landed_not_observed","plan":"p-1","node":"n-1","issue":"CRW-9","detail":"landed 20 minutes ago"}],` +
		`"checks":[]}`
	fakeHostReadDocument = `{"ok":true,"method":"thread/loaded/list","result":{"data":["thread-1"]}}`
)

// statusResult is one fake source's answer.
type statusResult struct {
	code   int
	stdout string
	stderr string
}

// statusFake is the replaced manage seam: it records every argument list it was called with
// and answers from byArgs.
type statusFake struct {
	calls  []string
	byArgs map[string]statusResult
}

// fakeStatusManage replaces the manage seam for one test. An argument list the test did not
// plan for answers exit 3, so an unexpected call fails the assertion that reads the result
// rather than passing silently.
func fakeStatusManage(t *testing.T, byArgs map[string]statusResult) *statusFake {
	t.Helper()
	previous := statusManage
	fake := &statusFake{byArgs: byArgs}
	statusManage = func(_ context.Context, args []string) (int, string, string) {
		key := strings.Join(args, " ")
		fake.calls = append(fake.calls, key)
		result, ok := fake.byArgs[key]
		if !ok {
			return 3, "", "crw manage: this argument list was not expected: " + key
		}
		return result.code, result.stdout, result.stderr
	}
	t.Cleanup(func() { statusManage = previous })
	return fake
}

// okStatusSources is every source answering with its own document.
func okStatusSources() map[string]statusResult {
	return map[string]statusResult{
		statusArgRelay:    {code: 0, stdout: fakeRelayDocument},
		statusArgCapacity: {code: 0, stdout: fakeCapacityDocument},
		statusArgDag:      {code: 0, stdout: fakeDagDocument},
		statusArgHostRead: {code: 0, stdout: fakeHostReadDocument},
	}
}

// statusServer builds a Server over the package registry, so the real routes are exercised.
func statusServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{Port: guardPort, Token: guardToken, Version: "test-version"})
	if err != nil {
		t.Fatalf("New over the package registry: %v", err)
	}
	return server
}

// decodeStatus drives GET /api/status and decodes the body.
func decodeStatus(t *testing.T, server *Server) map[string]any {
	t.Helper()
	recorder := request(server, http.MethodGet, "/api/status", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/status: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("status body %q: %v", recorder.Body.String(), err)
	}
	return body
}

// object reads one nested object from a decoded body.
func object(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is %#v, want an object", key, parent[key])
	}
	return value
}

// bar reads the status bar from a decoded body.
func bar(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	return object(t, body, "bar")
}

// statusList reads one nested list from a decoded body.
func statusList(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	list, ok := parent[key].([]any)
	if !ok {
		t.Fatalf("%s is %#v, want a list", key, parent[key])
	}
	return list
}

// mark reads one bar reading's state and reason.
func mark(t *testing.T, parent map[string]any, key string) (string, string) {
	t.Helper()
	reading := object(t, parent, key)
	state, _ := reading["state"].(string)
	reason, _ := reading["reason"].(string)
	return state, reason
}

// TestStatusKeepsTheThreeReadingsApart is C1: each bar reading has its own state, so one
// source failing never changes the other two.
func TestStatusKeepsTheThreeReadingsApart(t *testing.T) {
	// The policy reading is ok only with a wiring record and a readable policy file.
	policyHost(t, policyText, true)
	cases := []struct {
		name    string
		results map[string]statusResult
		want    map[string]string
	}{
		{
			name: "the relay store cannot be read",
			results: func() map[string]statusResult {
				results := okStatusSources()
				results[statusArgRelay] = statusResult{code: 3, stderr: "crw manage relay-read: error: relay_read_store_absent: no relay store\n"}
				return results
			}(),
			want: map[string]string{"relayStore": "unknown", "appServer": "ok", "executionPolicy": "ok"},
		},
		{
			name: "the App Server cannot be reached",
			results: func() map[string]statusResult {
				results := okStatusSources()
				results[statusArgHostRead] = statusResult{code: 3, stdout: `{"ok":false,"reason":"host_unreachable","detail":"connect: no such file"}`}
				return results
			}(),
			want: map[string]string{"relayStore": "ok", "appServer": "unknown", "executionPolicy": "ok"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fakeStatusManage(t, testCase.results)
			readings := bar(t, decodeStatus(t, statusServer(t)))
			for key, want := range testCase.want {
				state, _ := mark(t, readings, key)
				if state != want {
					t.Fatalf("%s state = %q, want %q (bar %#v)", key, state, want, readings)
				}
			}
		})
	}
}

// TestStatusUnknownReadingCarriesItsReason pins that an unread source is unknown with the
// reason the command itself gave, and never an empty reason.
func TestStatusUnknownReadingCarriesItsReason(t *testing.T) {
	policyHost(t, policyText, true)
	results := okStatusSources()
	results[statusArgRelay] = statusResult{code: 3, stderr: "crw manage relay-read: error: relay_read_store_unreadable: disk\n"}
	fakeStatusManage(t, results)
	state, reason := mark(t, bar(t, decodeStatus(t, statusServer(t))), "relayStore")
	if state != "unknown" {
		t.Fatalf("relayStore state = %q, want unknown", state)
	}
	if !strings.Contains(reason, "relay_read_store_unreadable") {
		t.Fatalf("relayStore reason = %q, want the command own stderr", reason)
	}
}

// TestStatusUsesTheNonWritingArgumentLists is C2 and C3: the exact argument list of every
// source, with the two commands that can write state in their non-writing form.
func TestStatusUsesTheNonWritingArgumentLists(t *testing.T) {
	policyHost(t, policyText, true)
	fake := fakeStatusManage(t, okStatusSources())
	decodeStatus(t, statusServer(t))
	want := []string{statusArgRelay, statusArgCapacity, statusArgDag, statusArgHostRead}
	if len(fake.calls) != len(want) {
		t.Fatalf("manage calls = %q, want %q", fake.calls, want)
	}
	for i, args := range want {
		if fake.calls[i] != args {
			t.Fatalf("manage call %d = %q, want %q (all %q)", i, fake.calls[i], args, fake.calls)
		}
	}
}

// TestStatusPassesTheManageDocumentsThroughVerbatim is C2: the relay, capacity and DAG
// values are the command's own document, not a value this package derived.
func TestStatusPassesTheManageDocumentsThroughVerbatim(t *testing.T) {
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	body := decodeStatus(t, statusServer(t))
	for _, testCase := range []struct{ section, document string }{
		{"relay", fakeRelayDocument},
		{"capacity", fakeCapacityDocument},
		{"dag", fakeDagDocument},
	} {
		section := object(t, body, testCase.section)
		if state, _ := section["state"].(string); state != "ok" {
			t.Fatalf("%s state = %q, want ok (%v)", testCase.section, state, section["reason"])
		}
		var want, got any
		if err := json.Unmarshal([]byte(testCase.document), &want); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(section["data"])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s data is not JSON: %v", testCase.section, err)
		}
		wantJSON, _ := json.Marshal(want)
		gotJSON, _ := json.Marshal(got)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("%s data = %s, want %s", testCase.section, gotJSON, wantJSON)
		}
	}
}

// TestStatusRelayPartialReadIsUnknown pins the decided answer that a relay read which could
// not read part of the store shows that part as unknown, while the document it did print is
// still carried.
func TestStatusRelayPartialReadIsUnknown(t *testing.T) {
	policyHost(t, policyText, true)
	results := okStatusSources()
	results[statusArgRelay] = statusResult{code: 1, stdout: `{"schema":"crw-relay-read/1","bindings":[],` +
		`"relationships":[{"relationshipId":"rel-1","read":{"state":"unknown","reason":"the store no longer holds this relationship"}}],` +
		`"plans":[],"mergeTurns":[],"failures":[{"section":"plans","item":"","reason":"no such table: dag_plans"}]}`}
	fakeStatusManage(t, results)
	body := decodeStatus(t, statusServer(t))
	state, reason := mark(t, bar(t, body), "relayStore")
	if state != "unknown" {
		t.Fatalf("relayStore state = %q, want unknown", state)
	}
	if reason != "no such table: dag_plans" {
		t.Fatalf("relayStore reason = %q, want the failures reason", reason)
	}
	section := object(t, body, "relay")
	if section["data"] == nil {
		t.Fatalf("the relay document was dropped: %#v", section)
	}
}

// TestStatusRelayPartialReadWithoutAFailureUsesTheItemReason covers the case where only an
// item's own source failed: the document has no failures entry to name it.
func TestStatusRelayPartialReadWithoutAFailureUsesTheItemReason(t *testing.T) {
	policyHost(t, policyText, true)
	results := okStatusSources()
	results[statusArgRelay] = statusResult{code: 1, stdout: `{"schema":"crw-relay-read/1","bindings":[],` +
		`"relationships":[{"relationshipId":"rel-1","read":{"state":"unknown","reason":"the store no longer holds this relationship"}}],` +
		`"plans":[],"mergeTurns":[],"failures":[]}`}
	fakeStatusManage(t, results)
	state, reason := mark(t, bar(t, decodeStatus(t, statusServer(t))), "relayStore")
	if state != "unknown" {
		t.Fatalf("relayStore state = %q, want unknown", state)
	}
	if reason != "the store no longer holds this relationship" {
		t.Fatalf("relayStore reason = %q, want the item own reason", reason)
	}
}

// TestStatusDagAnomalyExitIsACompleteRead pins that dag-review exit 1 (anomalies found) is
// the command answering, not a read failure.
func TestStatusDagAnomalyExitIsACompleteRead(t *testing.T) {
	policyHost(t, policyText, true)
	results := okStatusSources()
	results[statusArgDag] = statusResult{code: 1, stdout: fakeDagDocument}
	fakeStatusManage(t, results)
	body := decodeStatus(t, statusServer(t))
	section := object(t, body, "dag")
	if state, _ := section["state"].(string); state != "ok" {
		t.Fatalf("dag state = %q, want ok (%v)", state, section["reason"])
	}
	data := object(t, section, "data")
	anomalies, ok := data["anomalies"].([]any)
	if !ok || len(anomalies) != 1 {
		t.Fatalf("dag anomalies = %#v, want the one the command reported", data["anomalies"])
	}
}

// TestStatusUnparseableOutputIsUnknownWithADecodeReason pins the terminal fallback: a
// truncated document with no stderr line still carries a reason.
func TestStatusUnparseableOutputIsUnknownWithADecodeReason(t *testing.T) {
	policyHost(t, policyText, true)
	results := okStatusSources()
	results[statusArgCapacity] = statusResult{code: 0, stdout: `{"at":`}
	fakeStatusManage(t, results)
	body := decodeStatus(t, statusServer(t))
	section := object(t, body, "capacity")
	if state, _ := section["state"].(string); state != "unknown" {
		t.Fatalf("capacity state = %q, want unknown", state)
	}
	if reason, _ := section["reason"].(string); reason == "" {
		t.Fatalf("capacity reason is blank: %#v", section)
	}
	if section["data"] != nil {
		t.Fatalf("capacity data = %#v, want null", section["data"])
	}
}

// TestStatusPolicyReadsTheThreeDigestsApart is C1 for the policy reading: the file, the
// registered and the running digest are separate values, each with its own reason.
func TestStatusPolicyReadsTheThreeDigestsApart(t *testing.T) {
	file := policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	readings := bar(t, decodeStatus(t, statusServer(t)))
	policy := object(t, readings, "executionPolicy")
	if state, _ := policy["state"].(string); state != "ok" {
		t.Fatalf("executionPolicy state = %q (%v)", state, policy["reason"])
	}
	if policy["policyState"] != "registered" {
		t.Fatalf("policyState = %v", policy["policyState"])
	}
	if policy["path"] != file {
		t.Fatalf("path = %v, want %q", policy["path"], file)
	}
	if policy["fileDigest"] != digestOf(policyText) || policy["registeredDigest"] != digestOf(policyText) {
		t.Fatalf("digests = %#v", policy)
	}
	// The running digest could not be read on this isolated host, so it is null with its own
	// reason rather than a blank or a zero.
	if policy["runningDigest"] != nil {
		t.Fatalf("runningDigest = %#v, want null", policy["runningDigest"])
	}
	if reason, _ := policy["runningReason"].(string); reason == "" {
		t.Fatalf("runningReason is blank: %#v", policy)
	}
}

// TestStatusPolicyWithoutARecordIsUnknown pins that a host with no wiring record reads as
// unknown with policystore's reason, never as ok.
func TestStatusPolicyWithoutARecordIsUnknown(t *testing.T) {
	policyHost(t, policyText, false)
	fakeStatusManage(t, okStatusSources())
	policy := object(t, bar(t, decodeStatus(t, statusServer(t))), "executionPolicy")
	if state, _ := policy["state"].(string); state != "unknown" {
		t.Fatalf("executionPolicy state = %q, want unknown", state)
	}
	if reason, _ := policy["reason"].(string); reason == "" {
		t.Fatalf("executionPolicy reason is blank: %#v", policy)
	}
	if policy["policyState"] != "not_registered" {
		t.Fatalf("policyState = %v", policy["policyState"])
	}
}

// TestStatusHasNoWritePath is C4: the screen's route is registered for GET only.
func TestStatusHasNoWritePath(t *testing.T) {
	for _, route := range Routes() {
		if route.Path == "/api/status" && route.Method != http.MethodGet {
			t.Fatalf("/api/status is registered for %s", route.Method)
		}
	}
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	recorder := request(statusServer(t), http.MethodPost, "/api/status", guardHost, "{}", writeHeaders())
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("POST /api/status: %d %s, want 404", recorder.Code, recorder.Body.String())
	}
}

// statusTree is the recursive listing of a directory: every path with its size and the SHA-256 of
// its content, sorted, so a file created, removed, grown or rewritten in place (even with a value of
// the same length) by one request is visible.
func statusTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			entries = append(entries, "dir "+relative)
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, fmt.Sprintf("file %s %d %x", relative, info.Size(), sha256.Sum256(content)))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(entries)
	return entries
}

// TestStatusTreeSeesSameLengthRewrite pins the oracle of the no-write test: a file rewritten in
// place with a value of the same length keeps its size, so the listing must carry the content too.
func TestStatusTreeSeesSameLengthRewrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "crw-config.json")
	if err := os.WriteFile(path, []byte(`{"mode":"aaaa"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := statusTree(t, root)
	if err := os.WriteFile(path, []byte(`{"mode":"bbbb"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after := statusTree(t, root)
	if strings.Join(before, "\n") == strings.Join(after, "\n") {
		t.Fatalf("a same-length rewrite is invisible to the tree listing: %v", after)
	}
}

// The fake relay the no-write test reads: one plan with one node, one live relationship and one
// capacity plan that names the plan.
const (
	statusFakePlan    = "plan-status"
	statusFakeProject = "project-status"
	statusFakeParent  = "task-parent"
	statusFakeStamp   = "2026-10-07T00:00:00.000000+00:00"
)

// statusSeedFakeRelay writes the fake relay store below relayState through the product's own store
// and repository, then closes it. socketPath is the App Server socket the store records: the one the
// command resolves from the test's configuration, since a store that records another socket is
// refused. The close comes before the request is made, so the store is a settled file with no
// write-ahead frames: a read that creates a sidecar is then visible to the tree comparison rather
// than masked by a log the open already had to write.
func statusSeedFakeRelay(t *testing.T, relayState, socketPath string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(relayState, "relay.sqlite3"), socketPath)
	if err != nil {
		t.Fatalf("create the fake relay store: %v", err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			t.Fatalf("close the fake relay store: %v", err)
		}
	}()
	raw, err := json.Marshal(map[string]any{
		"schema": dag.SchemaRevision, "plan_id": statusFakePlan, "project_key": statusFakeProject,
		"request_id": statusFakePlan + "-r1", "expected_parent_revision": 0, "author_task_id": statusFakeParent,
		"changes": []any{map[string]any{"op": dag.OpAddNode, "node": map[string]any{
			"node_id": "A", "issue_key": "CRW-1", "kind": dag.NodeImplementation,
			"criteria_set_digest": testsupport.Dig("criteria A")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatalf("decode the fake plan revision: %v", err)
	}
	if _, err := (&dag.Repo{Store: st, Now: func() string { return statusFakeStamp }}).Put(ctx, revision); err != nil {
		t.Fatalf("put the fake plan revision: %v", err)
	}
	if err := storeseed.RecordRelationshipScope(ctx, st, "rel-CRW-1", statusFakeProject, statusFakeStamp); err != nil {
		t.Fatalf("scope the fake relationship: %v", err)
	}
	if err := storeseed.RecordRelationship(ctx, st, store.Relationship{
		ID: "rel-CRW-1", IssueKey: "CRW-1", Status: "active", ParentTaskID: statusFakeParent, ChildTaskID: "child-CRW-1",
		Generation: 1, ArtifactRoots: "[]", AllowedRecipients: `["parent"]`, CreatedAt: statusFakeStamp, UpdatedAt: statusFakeStamp,
	}, store.Generation{RelationshipID: "rel-CRW-1", Number: 1, DispatchRequestID: "dispatch-rel-CRW-1",
		AnchorState: store.AnchorBound, DispatchTurnID: sql.NullString{String: "turn-dispatch", Valid: true},
		OpenedAt: statusFakeStamp, BoundAt: sql.NullString{String: statusFakeStamp, Valid: true}}, "host", "host"); err != nil {
		t.Fatalf("fake relationship: %v", err)
	}
}

// TestStatusReadsWriteNothing is C3: the real manage path is invoked, over a fake relay store and a
// capacity plan that the reads succeed on, and neither the configuration directory nor the state
// directory gains a file.
func TestStatusReadsWriteNothing(t *testing.T) {
	// The capacity reading runs the relay command through this test binary (see TestMain), so the
	// crw binary is built first. testsupport caches it for the whole test process and TestMain
	// removes it when the process ends, so this test never deletes it (a second run in the same
	// process, as with -count=2, reuses it).
	testsupport.CRW(t)
	root := t.TempDir()
	// The real manage path runs below, so the argument list is recorded around it rather than
	// faked: this test must discriminate the non-writing form of each command. The fake relay store
	// and capacity plan let relay-read, capacity and dag-review succeed, so each one reaches the
	// reads that could write (the offset file is the one dag-review writes unless --no-state) instead
	// of stopping on a missing store before it.
	previous := statusManage
	var calls []string
	statusManage = func(ctx context.Context, args []string) (int, string, string) {
		calls = append(calls, strings.Join(args, " "))
		return statusRunManage(ctx, args)
	}
	t.Cleanup(func() { statusManage = previous })
	codexHome := filepath.Join(root, "codex-home")
	stateRoot := filepath.Join(root, "xdg-state")
	configHome := filepath.Join(root, "xdg-config")
	manageState := filepath.Join(root, "manage-state")
	relayState := filepath.Join(root, "relay-state")
	for _, dir := range []string{codexHome, stateRoot, configHome, manageState, relayState} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The capacity signal reads the GitHub status page by default; this test points it at a
	// local server, so the run makes no external request.
	actions := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"incidents":[]}`)
	}))
	defer actions.Close()
	statusSeedFakeRelay(t, relayState, filepath.Join(codexHome, "app-server-control", "app-server-control.sock"))
	config, err := json.Marshal(map[string]any{"manage": map[string]any{
		"relay":     map[string]any{"state": relayState},
		"state_dir": manageState,
		"capacity": map[string]any{"actions_status_url": actions.URL,
			"plans": []map[string]any{{"plan": statusFakePlan, "project": statusFakeProject, "parent": statusFakeParent}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "crw-config.json")
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "xdg-cache"))
	t.Setenv("CRW_CONFIG", configPath)

	before := statusTree(t, root)
	recorder := request(statusServer(t), http.MethodGet, "/api/status", guardHost, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/status: %d %s", recorder.Code, recorder.Body.String())
	}
	after := statusTree(t, root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("the request changed the tree:\nbefore: %v\nafter:  %v", before, after)
	}
	// HTTP 200 comes back when every source failed, so the test also requires each named read to
	// have succeeded: a pass over an empty fixture would say nothing about the read paths.
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("status body %q: %v", recorder.Body.String(), err)
	}
	for _, source := range []string{"relay", "capacity", "dag"} {
		if state, reason := mark(t, body, source); state != statusOK {
			t.Fatalf("the %s read is %q (%s), want ok", source, state, reason)
		}
	}
	// The reads succeeded on the fake store and plan, so the values they carry came from it: the
	// relationship is listed with the pull request the relay exports (null here, the relationship has
	// none), and the capacity reading judges the configured plan.
	relayData := object(t, object(t, body, "relay"), "data")
	relationships := statusList(t, relayData, "relationships")
	if len(relationships) != 1 {
		t.Fatalf("relay relationships = %d, want the one the fake store holds", len(relationships))
	}
	if _, ok := relationships[0].(map[string]any)["pullRequest"]; !ok {
		t.Fatalf("the relay relationship carries no pullRequest field: %#v", relationships[0])
	}
	if plans := statusList(t, object(t, object(t, body, "capacity"), "data"), "plans"); len(plans) != 1 {
		t.Fatalf("capacity plans = %d, want the one the configuration names", len(plans))
	}
	// The same run proves the argument lists: the file comparison alone cannot tell the two
	// commands' non-writing forms apart when a fixture stops them before the write.
	want := []string{statusArgRelay, statusArgCapacity, statusArgDag, statusArgHostRead}
	if len(calls) != len(want) {
		t.Fatalf("manage calls = %q, want %q", calls, want)
	}
	for i, args := range want {
		if calls[i] != args {
			t.Fatalf("manage call %d = %q, want %q (all %q)", i, calls[i], args, calls)
		}
	}
}

// TestStatusCancelledRequestDoesNotQueue pins that a request whose context ended while another
// read held the gate returns at once instead of waiting for a read it can no longer use. Without
// this, a cancelled poll would sit behind a slow read and hold the endpoint open.
func TestStatusCancelledRequestDoesNotQueue(t *testing.T) {
	// Hold the gate as a read in flight does.
	statusManageGate <- struct{}{}
	defer func() { <-statusManageGate }()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan statusResult, 1)
	go func() {
		code, stdout, stderr := statusManageCall(cancelled, statusArgRelay)
		done <- statusResult{code: code, stdout: stdout, stderr: stderr}
	}()
	select {
	case result := <-done:
		if result.stderr == "" {
			t.Fatalf("a call that did not start carries no reason: %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled call waited for the gate instead of returning")
	}
}

// TestStatusCancelledRequestIsUnknown pins the same property through the handler: a cancelled
// request produces unknown readings with a reason, never a document that reads as ok.
func TestStatusCancelledRequestIsUnknown(t *testing.T) {
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	server := statusServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil).WithContext(ctx)
	req.Host = guardHost
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/status: %d %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("status body %q: %v", recorder.Body.String(), err)
	}
	readings := bar(t, body)
	for _, key := range []string{"relayStore", "appServer"} {
		state, reason := mark(t, readings, key)
		if state != "unknown" {
			t.Fatalf("%s state = %q, want unknown", key, state)
		}
		if reason == "" {
			t.Fatalf("%s reason is blank", key)
		}
	}
}

// The plan promises a time bound on every source, the policy read included. This pins it: the
// read runs under its own deadline, and a policy read that does not finish inside the bound is
// unknown with a reason rather than left to hold the request open.
func TestStatusPolicyReadIsBounded(t *testing.T) {
	policyHost(t, policyText, true)
	previous := statusPolicyReader
	var gotDeadline bool
	statusPolicyReader = func(ctx context.Context) statusPolicyReading {
		_, gotDeadline = ctx.Deadline()
		return statusPolicyReadProduction(ctx)
	}
	t.Cleanup(func() { statusPolicyReader = previous })
	fakeStatusManage(t, okStatusSources())
	decodeStatus(t, statusServer(t))
	if !gotDeadline {
		t.Fatal("the policy read ran without a deadline")
	}
}

// TestStatusPolicyReadThatOutlivesItsBoundIsUnknown pins the other half: a read that does not
// finish inside the bound is unknown with a reason, never a blank or an ok.
func TestStatusPolicyReadThatOutlivesItsBoundIsUnknown(t *testing.T) {
	// A short bound keeps the test quick while it still exercises the real timeout path.
	previousTimeout := statusPolicyTimeout
	statusPolicyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusPolicyTimeout = previousTimeout })
	previous := statusPolicyReader
	statusPolicyReader = func(ctx context.Context) statusPolicyReading {
		// Stand in for a read that reports ok from what it already had after its deadline
		// passed. That is the case the timeout branch exists to catch: the ok must not survive,
		// or the endpoint would report a policy it never finished reading as read.
		<-ctx.Done()
		return statusPolicyReading{
			State: statusOK, PolicyState: "registered", Path: "/tmp/execution-policy.json",
			Applied: policystore.AppliedApplied,
		}
	}
	t.Cleanup(func() { statusPolicyReader = previous })
	fakeStatusManage(t, okStatusSources())
	policy := object(t, bar(t, decodeStatus(t, statusServer(t))), "executionPolicy")
	if state, _ := policy["state"].(string); state != "unknown" {
		t.Fatalf("executionPolicy state = %q, want unknown", state)
	}
	if reason, _ := policy["reason"].(string); reason == "" {
		t.Fatalf("executionPolicy reason is blank: %#v", policy)
	}
	if policy["applied"] != policystore.AppliedUnverifiable {
		t.Fatalf("applied = %v, want %q", policy["applied"], policystore.AppliedUnverifiable)
	}
}

// A read that never answers is bounded: the request returns on the source's own deadline, and
// the source reads unknown with the reason, instead of holding the response open.
func TestStatusBlockingReadIsBoundedAndUnknown(t *testing.T) {
	previousTimeout := statusSourceTimeout
	statusSourceTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusSourceTimeout = previousTimeout })

	release := make(chan struct{})
	previousManage := statusManage
	statusManage = func(_ context.Context, _ []string) (int, string, string) {
		<-release
		return 0, "", ""
	}
	t.Cleanup(func() { statusManage = previousManage })
	t.Cleanup(func() { close(release) })

	previousPolicy := statusPolicyReader
	statusPolicyReader = func(_ context.Context) statusPolicyReading {
		return statusPolicyReading{State: statusOK, PolicyState: policystore.Registered}
	}
	t.Cleanup(func() { statusPolicyReader = previousPolicy })

	start := time.Now()
	body := decodeStatus(t, statusServer(t))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("GET /api/status held the response for %s past the source bound", elapsed)
	}
	relay := object(t, body, "relay")
	if relay["state"] != statusUnknown {
		t.Fatalf("a read that did not finish must be unknown, got %v", relay["state"])
	}
	if reason, _ := relay["reason"].(string); !strings.Contains(reason, "did not finish") {
		t.Fatalf("the unknown reading must name the unfinished read, got %q", reason)
	}
}

// TestStatusPolicyKeepsFileDigestsWhenRunningTimesOut pins that a running-digest read that does
// not finish leaves the file and registered digests the policy read already had, and reports only
// the running digest unknown.
func TestStatusPolicyKeepsFileDigestsWhenRunningTimesOut(t *testing.T) {
	previousTimeout := statusPolicyTimeout
	statusPolicyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { statusPolicyTimeout = previousTimeout })
	previousRunning := statusRunningReader
	statusRunningReader = func(ctx context.Context) policystore.Running {
		<-ctx.Done()
		return policystore.Running{State: policystore.RunningUnavailable, Reason: "held"}
	}
	t.Cleanup(func() { statusRunningReader = previousRunning })
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	policy := object(t, bar(t, decodeStatus(t, statusServer(t))), "executionPolicy")
	if policy["fileDigest"] != digestOf(policyText) || policy["registeredDigest"] != digestOf(policyText) {
		t.Fatalf("file and registered digests were dropped when the running read timed out: %#v", policy)
	}
	if policy["runningDigest"] != nil {
		t.Fatalf("runningDigest = %#v, want null", policy["runningDigest"])
	}
	if reason, _ := policy["runningReason"].(string); reason == "" {
		t.Fatalf("runningReason is blank: %#v", policy)
	}
}

// TestManageReadsNeverOverlapAcrossStatusAndPolicy pins that a policy read waits behind a status
// read holding the manage gate, and that both reach the running digest through the same gate.
func TestManageReadsNeverOverlapAcrossStatusAndPolicy(t *testing.T) {
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	var mu sync.Mutex
	active, maxActive, calls := 0, 0, 0
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	previous := manageRunningDigest
	manageRunningDigest = func(ctx context.Context, env policystore.LookupEnv) policystore.Running {
		mu.Lock()
		active++
		calls++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		return policystore.Running{State: policystore.RunningUnavailable, Reason: "stub"}
	}
	t.Cleanup(func() { manageRunningDigest = previous })

	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		decodeStatus(t, statusServer(t))
	}()
	<-entered

	policyDone := make(chan Response, 1)
	go func() {
		response, _ := policyHandler(nil, httptest.NewRequest(http.MethodGet, "/api/policy", nil))
		policyDone <- response
	}()
	select {
	case <-entered:
		t.Fatal("the policy read started its running digest while the status read held the manage gate")
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	<-statusDone
	<-policyDone

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("running digest calls overlapped: at most %d active", maxActive)
	}
	if calls != 2 {
		t.Fatalf("running digest calls = %d, want 2 (one status, one policy) through the gate", calls)
	}
}

// TestPolicyWaitingForTheGateStartsNoRunningRead pins that a policy request whose context ends while
// a status read holds the gate returns without starting its running digest read, on both the GET
// route and the write route's running read.
func TestPolicyWaitingForTheGateStartsNoRunningRead(t *testing.T) {
	policyHost(t, policyText, true)
	fakeStatusManage(t, okStatusSources())
	var mu sync.Mutex
	calls := 0
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	previous := manageRunningDigest
	manageRunningDigest = func(ctx context.Context, env policystore.LookupEnv) policystore.Running {
		mu.Lock()
		calls++
		mu.Unlock()
		entered <- struct{}{}
		<-release
		return policystore.Running{State: policystore.RunningUnavailable, Reason: "stub"}
	}
	t.Cleanup(func() { manageRunningDigest = previous })

	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		decodeStatus(t, statusServer(t))
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/policy", nil).WithContext(ctx)
	if _, err := policyHandler(nil, request); err != nil {
		t.Fatal(err)
	}
	running := policyWriteOptions().Running(ctx, envLookup)
	if running.State == policystore.RunningObserved {
		t.Fatal("a cancelled write-route read reported an observed digest")
	}

	close(release)
	<-statusDone
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("running digest calls = %d, want 1: the cancelled policy reads must not start", calls)
	}
}
