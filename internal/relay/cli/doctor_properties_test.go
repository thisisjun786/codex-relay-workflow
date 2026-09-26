package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// plain renders an ordered answer the way the CLI prints it and decodes it back.
func plain(v any) any {
	var b bytes.Buffer
	if err := contract.Emit(&b, v); err != nil {
		panic(err)
	}
	var out any
	_ = json.Unmarshal(b.Bytes(), &out)
	return out
}

// test_cli.py properties of todo 25 part A that read doctor and status (CLI-1..4, 10, 12,
// 17..20, 24..34, 36, 37). Each drives the real Python relay CLI and this build's CLI on the
// same isolated state, compares the whole stdout (doctor minus the documented runtime block)
// and exit code, then asserts the property's own fields on Python's answer.

const (
	parentTask = "01parent-task"
	childTask  = "01child-task"
	hostID     = "host-a"
	issueKey   = "REL-1"
)

var timestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

// both runs argv through Python and Go and requires the same exit and stdout. Doctor's runtime
// block (Go only, documented) is removed; timestamps written by the commands themselves are
// normalised, because the two runs happen at different instants.
func both(t *testing.T, dir string, argv ...string) map[string]any {
	t.Helper()
	py, got := python(t, dir, argv...), golang(t, dir, argv...)
	goOut := got.stdout
	if strings.Contains(goOut, "\n  \"runtime\": {") {
		goOut = withoutKey(t, goOut, "runtime")
	}
	pyOut := timestamp.ReplaceAllString(py.stdout, "<T>")
	goOut = timestamp.ReplaceAllString(goOut, "<T>")
	if py.code != got.code || pyOut != goOut {
		t.Fatalf("%v\nexit python=%d go=%d\npython:\n%s\ngo:\n%s\ngo stderr: %s", argv, py.code, got.code, pyOut, goOut, got.stderr)
	}
	if py.stdout == "" {
		return nil
	}
	return decode(t, py.stdout)
}

func exitOf(t *testing.T, dir string, argv ...string) int {
	t.Helper()
	return python(t, dir, argv...).code
}

func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }

func settingsJSON(cwd string, sandbox string) string {
	if sandbox == "" {
		sandbox = `{"type": "workspaceWrite", "writableRoots": ["` + cwd + `"], "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}`
	}
	return `{"sandbox": ` + sandbox + `, "approvalPolicy": "never", "cwd": "` + cwd + `", "runtimeWorkspaceRoots": ["` + cwd + `"], "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "environments": [{"environmentId": "local", "cwd": "` + cwd + `", "runtimeWorkspaceRoots": ["` + cwd + `"]}]}`
}

// register records an assignment through the Python CLI (the state both sides then read).
func register(t *testing.T, home, state string, extra ...string) map[string]any {
	t.Helper()
	root := filepath.Join(home, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"--state", state, "register", "--parent-task", parentTask, "--parent-host", hostID,
		"--child-task", childTask, "--child-host", hostID, "--issue", issueKey, "--artifact-root", root,
		"--allowed-recipient", parentTask, "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", "turn-dispatch-1"}, extra...)
	done := python(t, home, argv...)
	if done.code != 0 {
		t.Fatalf("register: %+v", done)
	}
	return decode(t, done.stdout)
}

func sqlite(t *testing.T, home, db, statement string) {
	t.Helper()
	script := "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.executescript(sys.argv[2]); c.commit(); c.close()"
	runPython(t, home, script, db, statement)
}

// runPython runs a Python snippet in the relay's environment (for staging rows by hand, as the
// Python tests do with sqlite3 or Store()).
func runPython(t *testing.T, dir string, script string, args ...string) string {
	t.Helper()
	out, err := pythonSnippet(dir, script, args...)
	if err != nil {
		t.Fatalf("python snippet: %v\n%s", err, out)
	}
	return out
}

func pythonJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// CLI-1: doctor --issue against an absent store answers without creating it: readable false,
// holds null, responsibleRelationship null, detail "not readable".
func Test25_CLI1_doctor_issue_on_an_absent_store_answers_null_and_creates_nothing(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	report := both(t, home, "--state", state, "doctor", "--issue", issueKey)
	issue := obj(report["issue"])
	if issue["readable"] != false || issue["holds"] != nil || issue["responsibleRelationship"] != nil || !strings.Contains(issue["detail"].(string), "not readable") {
		t.Fatal(issue)
	}
	if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
		t.Fatal("doctor --issue created the store")
	}
}

// CLI-2: ownership: registered -> holds true with the child; another issue -> readable, holds
// false (not null); paused still holds; superseded (active row with a successor) is no owner.
func Test25_CLI2_doctor_issue_names_the_live_owner(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	relationship := register(t, home, state)
	rid := relationship["relationshipId"].(string)
	issue := obj(both(t, home, "--state", state, "doctor", "--issue", issueKey)["issue"])
	if issue["holds"] != true || issue["responsibleChild"] != childTask || issue["responsibleRelationship"] != rid {
		t.Fatal(issue)
	}
	other := obj(both(t, home, "--state", state, "doctor", "--issue", "SOME-OTHER-ISSUE")["issue"])
	if other["readable"] != true || other["holds"] != false || other["responsibleChild"] != nil {
		t.Fatal(other)
	}
	both(t, home, "--state", state, "relationship-status", "--relationship", rid, "--status", "paused", "--actor", parentTask)
	if obj(both(t, home, "--state", state, "doctor", "--issue", issueKey)["issue"])["holds"] != true {
		t.Fatal("a paused owner stopped holding")
	}
	sqlite(t, home, filepath.Join(state, "relay.sqlite3"), "UPDATE relationships SET superseded_by = 'rel-0000000000000001'")
	superseded := obj(both(t, home, "--state", state, "doctor", "--issue", issueKey)["issue"])
	if superseded["holds"] != false || superseded["responsibleRelationship"] != nil {
		t.Fatal(superseded)
	}
}

// CLI-3: the issue rows and the store identity come from the same read.
func Test25_CLI3_the_issue_rows_and_the_identity_come_from_one_read(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	register(t, home, state)
	report := both(t, home, "--state", state, "doctor", "--issue", issueKey)
	issue := obj(report["issue"])
	if issue["storeAgreement"] != "same" || issue["storeId"] != obj(report["store"])["storeId"] {
		t.Fatal(issue)
	}
}

// CLI-4: doctor without --issue has no issue key.
func Test25_CLI4_doctor_without_the_flag_has_no_issue_key(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	register(t, home, state)
	if _, found := both(t, home, "--state", state, "doctor")["issue"]; found {
		t.Fatal("issue key present")
	}
}

// CLI-10: doctor reports the environment: procAvailable true, the read-only adapter line.
func Test25_CLI10_doctor_reports_the_environment(t *testing.T) {
	home := pythonHome(t)
	report := both(t, home, "--state", filepath.Join(home, "state"), "doctor")
	if report["procAvailable"] != true || report["adapter"] != "none (read-only, no --socket)" {
		t.Fatal(report["procAvailable"], report["adapter"])
	}
}

// CLI-17: doctor never creates or adopts a store: an absent directory stays absent; an empty,
// unrelated relay.sqlite3 stays byte-unchanged with no sidecars and contents unavailable.
func Test25_CLI17_doctor_never_creates_or_adopts_a_store(t *testing.T) {
	home := pythonHome(t)
	absent := filepath.Join(home, "absent")
	report := both(t, home, "--state", absent, "doctor")
	if obj(report["stateSelection"])["source"] != "flag" || obj(report["access"])["directoryExists"] != false || obj(report["contents"])["available"] != false {
		t.Fatal(report)
	}
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("doctor created the directory")
	}
	borrowed := filepath.Join(home, "borrowed")
	if err := os.MkdirAll(borrowed, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(borrowed, "relay.sqlite3")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report = both(t, home, "--state", borrowed, "doctor")
	info, err := os.Stat(target)
	if err != nil || info.Size() != 0 {
		t.Fatal("doctor wrote into the unrelated file")
	}
	entries, _ := os.ReadDir(borrowed)
	if len(entries) != 1 {
		t.Fatalf("sidecars: %v", entries)
	}
	if obj(report["access"])["dbExists"] != true || obj(report["contents"])["available"] != false || obj(report["contents"])["detail"] == nil {
		t.Fatal(report["contents"])
	}
}

// CLI-18: stateSelection names the rule and path: env for CODEX_SESSION_RELAY_STATE, flag for
// --state even when the variable is also set.
func Test25_CLI18_doctor_names_the_rule_that_chose_the_directory(t *testing.T) {
	home := pythonHome(t)
	chosen := filepath.Join(home, "chosen")
	t.Setenv("CODEX_SESSION_RELAY_STATE", chosen)
	byEnv := obj(both(t, home, "doctor")["stateSelection"])
	if byEnv["source"] != "env" || byEnv["path"] != chosen {
		t.Fatal(byEnv)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(home, "ignored"))
	byFlag := obj(both(t, home, "--state", chosen, "doctor")["stateSelection"])
	if byFlag["source"] != "flag" || byFlag["path"] != chosen {
		t.Fatal(byFlag)
	}
}

// challenge writes a store challenge through Python only: nonces are random, so the two
// implementations cannot be compared on this write, and the doctor comparison reads it back.
func challenge(t *testing.T, home, state string) string {
	t.Helper()
	done := python(t, home, "--state", state, "store-challenge", "--write", "--actor", "parent")
	if done.code != 0 {
		t.Fatal(done)
	}
	return decode(t, done.stdout)["nonce"].(string)
}

func identity(t *testing.T, home, state string) map[string]any {
	t.Helper()
	return obj(both(t, home, "--state", state, "store-identity")["store"])
}

func number(v any) string {
	if f, ok := v.(float64); ok {
		return strings.TrimSuffix(strings.TrimSuffix(jsonNumber(f), ".0"), ".00")
	}
	return ""
}

func jsonNumber(f float64) string {
	raw, _ := json.Marshal(f)
	return string(raw)
}

// CLI-19: same-store proof. Another store -> exit 2 mismatch with the diagnosis kept; all four
// -> proven; nonce + store only -> unproven naming --expect-inode/--expect-log; a copy made
// before the nonce -> mismatch; a hardlinked second name -> unproven naming "names" (and
// "write-ahead log" with --expect-log), links 2; an empty or unusable value is never proven.
func Test25_CLI19_same_store_proof_needs_the_nonce_and_the_physical_identity(t *testing.T) {
	home := pythonHome(t)
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	mine := identity(t, home, a)
	theirs := identity(t, home, b)
	if mine["storeId"] == theirs["storeId"] {
		t.Fatal("same id")
	}
	refused := both(t, home, "--state", b, "doctor", "--expect-store", mine["storeId"].(string))
	if refused["sameStore"] != "unproven" && refused["sameStore"] != "mismatch" {
		t.Fatal(refused["sameStore"])
	}
	if _, ok := refused["stateSelection"]; !ok {
		t.Fatal("diagnosis dropped")
	}
	nonce := challenge(t, home, a)
	pair := number(mine["device"]) + ":" + number(mine["inode"])
	log := number(mine["logDevice"]) + ":" + number(mine["logInode"]) + ":" + mine["logName"].(string)
	if got := both(t, home, "--state", a, "doctor", "--expect-store", mine["storeId"].(string), "--expect-inode", pair, "--expect-log", log, "--expect-nonce", nonce)["sameStore"]; got != "proven" {
		t.Fatal(got)
	}
	alone := both(t, home, "--state", a, "doctor", "--expect-store", mine["storeId"].(string), "--expect-nonce", nonce)
	if alone["sameStore"] != "unproven" || !strings.Contains(alone["detail"].(string), "--expect-inode") || !strings.Contains(alone["detail"].(string), "--expect-log") {
		t.Fatal(alone["detail"])
	}
	// A copy made before the nonce.
	c := filepath.Join(home, "c")
	fresh := identity(t, home, c)
	copyDir := filepath.Join(home, "copy")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if raw, err := os.ReadFile(filepath.Join(c, "relay.sqlite3"+suffix)); err == nil {
			if err := os.WriteFile(filepath.Join(copyDir, "relay.sqlite3"+suffix), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cNonce := challenge(t, home, c)
	cPair := number(fresh["device"]) + ":" + number(fresh["inode"])
	cLog := number(fresh["logDevice"]) + ":" + number(fresh["logInode"]) + ":" + fresh["logName"].(string)
	if got := both(t, home, "--state", copyDir, "doctor", "--expect-store", fresh["storeId"].(string), "--expect-inode", cPair, "--expect-log", cLog, "--expect-nonce", cNonce)["sameStore"]; got != "mismatch" {
		t.Fatal("copy:", got)
	}
	// A hardlinked second name.
	two := filepath.Join(home, "two")
	if err := os.MkdirAll(two, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(a, "relay.sqlite3"), filepath.Join(two, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	counted := both(t, home, "--state", two, "doctor", "--expect-store", mine["storeId"].(string), "--expect-inode", pair, "--expect-nonce", nonce)
	if counted["sameStore"] != "unproven" || !strings.Contains(counted["detail"].(string), "names") {
		t.Fatal(counted["detail"])
	}
	graded := both(t, home, "--state", two, "doctor", "--expect-store", mine["storeId"].(string), "--expect-inode", pair, "--expect-log", log, "--expect-nonce", nonce)
	if graded["sameStore"] != "unproven" || !strings.Contains(graded["detail"].(string), "write-ahead log") || obj(graded["store"])["links"] != float64(2) {
		t.Fatal(graded["detail"], graded["store"])
	}
	for _, flagValue := range [][2]string{{"--expect-log", ""}, {"--expect-log", "1:2"}, {"--expect-log", "nonsense"}, {"--expect-store", ""}, {"--expect-inode", ""}, {"--expect-nonce", ""}} {
		answer := both(t, home, "--state", a, "doctor", flagValue[0], flagValue[1])
		if answer["sameStore"] == "proven" || exitOf(t, home, "--state", a, "doctor", flagValue[0], flagValue[1]) != 2 {
			t.Fatal(flagValue, answer["sameStore"])
		}
	}
}

// CLI-20: ledger.configured and ledger.split: true when --state moved the store away from the
// env-resolved ledger, false when they agree.
func Test25_CLI20_doctor_reports_that_state_and_the_ledger_have_split(t *testing.T) {
	home := pythonHome(t)
	state, socket := filepath.Join(home, "state"), filepath.Join(home, "app.sock")
	split := obj(both(t, home, "--state", state, "--socket", socket, "doctor")["ledger"])
	if split["configured"] != true || split["split"] != true {
		t.Fatal(split)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	if together := obj(both(t, home, "--state", state, "--socket", socket, "doctor")["ledger"]); together["split"] != false {
		t.Fatal(together)
	}
}

// staged registers an assignment named name through Python and stages one ready_for_review
// claim for it (emit), returning the relationship and event ids.
func staged(t *testing.T, home, state, name string) (string, string) {
	t.Helper()
	parent, child := "01parent-"+name, "01child-"+name
	root := filepath.Join(home, "work", name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := python(t, home, "--state", state, "register", "--parent-task", parent, "--parent-host", hostID, "--child-task", child, "--child-host", hostID,
		"--issue", "REL-"+name, "--artifact-root", root, "--allowed-recipient", parent, "--dispatch-request-id", "dispatch-"+name, "--dispatch-turn-id", "turn-"+name)
	if reg.code != 0 {
		t.Fatal(reg)
	}
	rid := decode(t, reg.stdout)["relationshipId"].(string)
	path := filepath.Join(root, "out.txt")
	if err := os.WriteFile(path, []byte(name+" still going"), 0o644); err != nil {
		t.Fatal(err)
	}
	emitted := python(t, home, "--state", state, "emit", "--relationship", rid, "--generation", "1", "--outcome", "ready_for_review",
		"--turn-thread", child, "--turn-id", "turn-"+name, "--turn-status", "completed", "--artifact", path)
	if emitted.code != 0 {
		t.Fatal(emitted)
	}
	return rid, obj(decode(t, emitted.stdout)["receipt"])["eventId"].(string)
}

var ages = regexp.MustCompile(`("(?:ageSeconds|oldestStagedAgeSeconds)": )[0-9.]+`)

// statusBoth compares status whole; the ages are the one clock-dependent value (the two runs
// read the wall clock at different instants) and are normalised, never dropped.
func statusBoth(t *testing.T, home string, argv ...string) map[string]any {
	t.Helper()
	py, got := python(t, home, argv...), golang(t, home, argv...)
	norm := func(s string) string { return ages.ReplaceAllString(timestamp.ReplaceAllString(s, "<T>"), "${1}<AGE>") }
	if py.code != got.code || norm(py.stdout) != norm(got.stdout) {
		t.Fatalf("%v\nexit python=%d go=%d\npython:\n%s\ngo:\n%s", argv, py.code, got.code, py.stdout, got.stdout)
	}
	return decode(t, py.stdout)
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// CLI-12: status --relationship scopes stagedEvents, anchors and backlog to that assignment;
// an unscoped status reports every assignment.
func Test25_CLI12_a_scoped_status_reports_only_its_assignment(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	mine, myEvent := staged(t, home, state, "a")
	theirs, theirEvent := staged(t, home, state, "b")
	scoped := obj(statusBoth(t, home, "--state", state, "status", "--relationship", mine)["observation"])
	events := scoped["stagedEvents"].([]any)
	if len(events) != 1 || obj(events[0])["eventId"] != myEvent || len(obj(scoped["anchors"])) != 1 || obj(scoped["anchors"])[mine] == nil ||
		len(obj(scoped["backlog"])) != 1 || obj(scoped["backlog"])[theirs] != nil {
		t.Fatal(scoped)
	}
	all := obj(statusBoth(t, home, "--state", state, "status")["observation"])
	seen := map[any]bool{}
	for _, e := range all["stagedEvents"].([]any) {
		seen[obj(e)["eventId"]] = true
	}
	if !seen[myEvent] || !seen[theirEvent] || len(obj(all["anchors"])) != 2 || len(obj(all["backlog"])) != 2 {
		t.Fatal(keys(all))
	}
}

const requirement = `[{"role": "parent", "model": "gpt-5.5", "reasoningEffort": "xhigh"}]`

// CLI-24: --require-worker-policy: unparseable JSON and an unreadable @file are usage (exit 4);
// @file goes through the same parser; wrong shapes are exit 2 with workerReadiness {ready false,
// reason} naming each shape's own reason.
func Test25_CLI24_worker_policy_requirements_are_parsed_then_judged(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	bad := both(t, home, "--state", state, "doctor", "--require-worker-policy", "{not json")
	if bad["error"] != "usage" || !strings.Contains(bad["detail"].(string), "worker policy requirements") || exitOf(t, home, "--state", state, "doctor", "--require-worker-policy", "{not json") != 4 {
		t.Fatal(bad)
	}
	missing := both(t, home, "--state", state, "doctor", "--require-worker-policy", "@"+filepath.Join(home, "absent.json"))
	if missing["error"] != "usage" || !strings.Contains(missing["detail"].(string), "No such file") {
		t.Fatal(missing)
	}
	file := filepath.Join(home, "requirements.json")
	if err := os.WriteFile(file, []byte(requirement), 0o600); err != nil {
		t.Fatal(err)
	}
	if reason := obj(both(t, home, "--state", state, "doctor", "--require-worker-policy", "@"+file)["workerReadiness"])["reason"]; reason != "worker_policy_unreadable" {
		t.Fatal(reason)
	}
	for raw, reason := range map[string]string{
		`{"role": "parent"}`: "worker_policy_requirements_invalid",
		`[{"role": "supervisor", "model": "m", "reasoningEffort": "max"}]`: "worker_policy_role_unsupported",
		`[{"role": "parent", "model": false, "reasoningEffort": "max"}]`:   "worker_policy_requirements_invalid",
		`[]`: "worker_policy_requirements_invalid",
	} {
		readiness := obj(both(t, home, "--state", state, "doctor", "--require-worker-policy", raw)["workerReadiness"])
		if readiness["ready"] != false || readiness["reason"] != reason || exitOf(t, home, "--state", state, "doctor", "--require-worker-policy", raw) != 2 {
			t.Fatal(raw, readiness)
		}
	}
}

// CLI-25: doctor without the flag reports the worker and gates nothing.
func Test25_CLI25_doctor_without_the_flag_reports_the_worker_and_gates_nothing(t *testing.T) {
	home := pythonHome(t)
	report := both(t, home, "--state", filepath.Join(home, "state"), "doctor")
	if obj(report["workerPolicy"])["observed"] != false || report["callerWorkerAgreement"] != "unknown" {
		t.Fatal(report["workerPolicy"], report["callerWorkerAgreement"])
	}
	if _, found := report["workerReadiness"]; found {
		t.Fatal("workerReadiness present")
	}
}

// CLI-26: a readiness refusal (exit 2) keeps the whole diagnosis and the other answers of the
// same invocation (proven store, nonce found, issue readable).
func Test25_CLI26_a_readiness_refusal_keeps_the_whole_diagnosis(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	mine := identity(t, home, state)
	nonce := challenge(t, home, state)
	report := both(t, home, "--state", state, "doctor", "--require-worker-policy", requirement,
		"--expect-store", mine["storeId"].(string), "--expect-inode", number(mine["device"])+":"+number(mine["inode"]),
		"--expect-log", number(mine["logDevice"])+":"+number(mine["logInode"])+":"+mine["logName"].(string),
		"--expect-nonce", nonce, "--issue", issueKey)
	if obj(report["workerReadiness"])["reason"] != "worker_policy_unreadable" || report["sameStore"] != "proven" ||
		obj(report["nonce"])["found"] != true || obj(report["issue"])["readable"] != true || obj(report["issue"])["holds"] != false {
		t.Fatal(report["workerReadiness"], report["sameStore"], report["nonce"], report["issue"])
	}
	for _, key := range []string{"stateSelection", "access", "rolePolicy"} {
		if _, ok := report[key]; !ok {
			t.Fatal("dropped", key)
		}
	}
}

// pythonStore creates a Python store at dir recording socket ("" for none), as the Python tests
// do with Store(path, socket_path=...).close().
func pythonStore(t *testing.T, home, dir, socket string) {
	t.Helper()
	arg := "None"
	if socket != "" {
		arg = "sys.argv[2]"
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	runPython(t, home, "import sys; from pathlib import Path; from codex_session_relay.store import Store; Store(Path(sys.argv[1]) / 'relay.sqlite3', socket_path="+arg+").close()", dir, socket)
}

// recoverBoth runs argv in env through both implementations and compares the whole refusal.
// The first word of every recovery command is each implementation's own running program
// (CLI-29: a pasted line reaches the relay that printed it), so it is checked per side and then
// normalised to <PROG> before the byte comparison.
func recoverBoth(t *testing.T, dir string, argv ...string) map[string]any {
	t.Helper()
	py, got := python(t, dir, argv...), golang(t, dir, argv...)
	pyProgram := filepath.Join(repositoryRoot(t), ".venv", "bin", "codex-session-relay")
	goProgram := "codex-session-relay"
	norm := func(text, program string) string {
		text = strings.ReplaceAll(text, `"`+program+` `, `"<PROG> `)
		return strings.ReplaceAll(text, `"env -u CODEX_SESSION_RELAY_STATE `+program+` `, `"env -u CODEX_SESSION_RELAY_STATE <PROG> `)
	}
	realPy, _ := filepath.EvalSymlinks(filepath.Dir(pyProgram))
	pyOut := norm(norm(py.stdout, pyProgram), filepath.Join(realPy, "codex-session-relay"))
	goOut := norm(got.stdout, goProgram)
	if py.code != got.code || pyOut != goOut {
		t.Fatalf("%v\nexit python=%d go=%d\npython:\n%s\ngo:\n%s", argv, py.code, got.code, pyOut, goOut)
	}
	if strings.Contains(pyOut, "codex-session-relay --") || strings.Contains(goOut, "codex-session-relay --") {
		t.Fatalf("a recovery line was not rendered with the running program:\n%s\n%s", py.stdout, got.stdout)
	}
	return decode(t, py.stdout)
}

func commands(recover []any) []string {
	var out []string
	for _, line := range recover {
		if s := line.(string); !strings.HasPrefix(s, "  ") {
			out = append(out, s)
		}
	}
	return out
}

// contested makes a home whose default discovery sees two stores recording one socket.
func contested(t *testing.T, name string) (home, root, socket string) {
	t.Helper()
	home = filepath.Join(os.Getenv("HOME"), name)
	root = filepath.Join(home, ".local", "state", "codex-session-relay")
	socket = filepath.Join(os.Getenv("HOME"), name+".sock")
	for _, d := range []string{"aaaa444444444444", "bbbb444444444444"} {
		pythonStore(t, home, filepath.Join(root, d), socket)
	}
	return home, root, socket
}

// withHome points HOME at home (default discovery's root) with no XDG_STATE_HOME or pin, as
// the Python tests' run_in_home does, for the rest of the test.
func withHome(t *testing.T, home string) {
	t.Helper()
	if !strings.HasPrefix(home, os.TempDir()) {
		t.Fatalf("%s is not a test directory", home)
	}
	// This HOME is a t.TempDir, and its default state root is exactly what these properties
	// are about, so the store's live-state guard is lifted for it and nothing else.
	t.Setenv("CRW_ALLOW_LIVE_STATE", "1")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("XDG_STATE_HOME")
}

// CLI-27: a socket claimed by two directories: an ordinary command refuses
// ambiguous_state_directory with 2 candidates and creates nothing; doctor still describes it;
// an explicit --state resolves the contest.
func Test25_CLI27_a_contested_socket_refuses_instead_of_creating_a_third_store(t *testing.T) {
	pythonHome(t)
	home, root, socket := contested(t, "contested")
	withHome(t, home)
	refused := recoverBoth(t, home, "--socket", socket, "status")
	if refused["reason"] != "ambiguous_state_directory" || len(refused["candidates"].([]any)) != 2 {
		t.Fatal(refused)
	}
	if _, err := os.Stat(refused["wouldHaveCreated"].(string)); !os.IsNotExist(err) {
		t.Fatal("a third store was created")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatal(entries)
	}
	siblings := obj(both(t, home, "--socket", socket, "doctor")["siblingStores"])
	if siblings["ambiguous"] != true || len(siblings["claimingThisSocket"].([]any)) != 2 {
		t.Fatal(siblings)
	}
	chosen := filepath.Join(root, "aaaa444444444444")
	if deliveries := both(t, home, "--state", chosen, "--socket", socket, "status")["deliveries"]; len(deliveries.([]any)) != 0 {
		t.Fatal(deliveries)
	}
}

// CLI-28: a directory recording another socket is refused state_directory_serves_another_socket
// naming recordedSocket; a store recording none is refused unidentified_state_directory before
// one is created.
func Test25_CLI28_a_store_for_another_socket_or_none_is_refused(t *testing.T) {
	home := pythonHome(t)
	state, first, second := filepath.Join(home, "reused"), filepath.Join(home, "first.sock"), filepath.Join(home, "second.sock")
	pythonStore(t, home, state, first)
	refused := recoverBoth(t, home, "--state", state, "--socket", second, "status")
	if refused["reason"] != "state_directory_serves_another_socket" || refused["recordedSocket"] != first {
		t.Fatal(refused)
	}
	unlabelled := filepath.Join(home, "unlabelled-home")
	root := filepath.Join(unlabelled, ".local", "state", "codex-session-relay")
	pythonStore(t, home, filepath.Join(root, "0123456789abcdef"), "")
	withHome(t, unlabelled)
	refused = recoverBoth(t, unlabelled, "--socket", filepath.Join(home, "unlabelled.sock"), "status")
	if refused["reason"] != "unidentified_state_directory" {
		t.Fatal(refused)
	}
	if _, err := os.Stat(refused["wouldHaveCreated"].(string)); !os.IsNotExist(err) {
		t.Fatal("created")
	}
}

// optionValues is option_values: a shell round trip with --opt=value split back apart.
func optionValues(t *testing.T, command string) []string {
	t.Helper()
	out := runPython(t, os.Getenv("HOME"), "import json,shlex,sys; print(json.dumps(shlex.split(sys.argv[1])))", command)
	var words []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &words); err != nil {
		t.Fatal(err, out)
	}
	for i, w := range words {
		if head, tail, ok := strings.Cut(w, "="); ok && strings.HasPrefix(head, "--") {
			words[i] = tail
		}
	}
	return words
}

func contains(words []string, want string) bool {
	for _, w := range words {
		if w == want {
			return true
		}
	}
	return false
}

// CLI-29: recovery lines are runnable and pasteable: each names --socket= (attached), one
// --state= per candidate, doctor and service status lines, no retired adoption; paths are
// shell-quoted ($(...) survives a round trip); a dash-leading socket path still parses; the first
// word is the running program.
func Test25_CLI29_recovery_lines_are_runnable_and_safe_to_paste(t *testing.T) {
	pythonHome(t)
	home, _, socket := contested(t, "recovery")
	withHome(t, home)
	refused := recoverBoth(t, home, "--socket", socket, "status")
	lines := commands(refused["recover"].([]any))
	joined := strings.Join(lines, "\n")
	for _, line := range lines {
		if !strings.Contains(line, "--socket="+socket) {
			t.Fatal("dropped the socket:", line)
		}
	}
	for _, candidate := range refused["candidates"].([]any) {
		if !strings.Contains(joined, "--state="+candidate.(string)) {
			t.Fatal("candidate not offered:", candidate)
		}
	}
	if !strings.Contains(joined, "doctor") || !strings.Contains(joined, "service status") || strings.Contains(strings.Join(anyStrings(refused["recover"]), " "), "the directory above") {
		t.Fatal(lines)
	}
	// $(...) in a socket path survives a shell round trip whole.
	pythonHome(t)
	qhome := filepath.Join(os.Getenv("HOME"), "quoted-home")
	qroot := filepath.Join(qhome, ".local", "state", "codex-session-relay")
	qsocket := filepath.Join(os.Getenv("HOME"), "sock$(touch /tmp/pwned);x.sock")
	for _, d := range []string{"aaaa555555555555", "bbbb555555555555"} {
		pythonStore(t, qhome, filepath.Join(qroot, d), qsocket)
	}
	withHome(t, qhome)
	quoted := recoverBoth(t, qhome, "--socket", qsocket, "status")
	for _, line := range commands(quoted["recover"].([]any)) {
		if !contains(optionValues(t, line), qsocket) {
			t.Fatal("socket did not survive:", line)
		}
	}
	// The wrong-socket refusal quotes too.
	pythonHome(t)
	base := os.Getenv("HOME")
	state, first := filepath.Join(base, "quoted state"), filepath.Join(base, "first$(id).sock")
	pythonStore(t, base, state, first)
	wrong := recoverBoth(t, base, "--state", state, "--socket", filepath.Join(base, "second.sock"), "status")
	var words []string
	for _, line := range commands(wrong["recover"].([]any)) {
		words = append(words, optionValues(t, line)...)
	}
	if !contains(words, first) || !contains(words, state) {
		t.Fatal(words)
	}
	// A dash-leading relative socket path still produces runnable commands.
	pythonHome(t)
	dhome := filepath.Join(os.Getenv("HOME"), "dash-home")
	droot := filepath.Join(dhome, ".local", "state", "codex-session-relay")
	for _, d := range []string{"aaaa666666666666", "bbbb666666666666"} {
		pythonStore(t, os.Getenv("HOME"), filepath.Join(droot, d), filepath.Join(os.Getenv("HOME"), "-odd.sock"))
	}
	withHome(t, dhome)
	dashed := recoverBoth(t, filepath.Dir(dhome), "--socket=-odd.sock", "status")
	for _, line := range commands(dashed["recover"].([]any)) {
		if !contains(optionValues(t, line), "-odd.sock") {
			t.Fatal(line)
		}
		if strings.HasSuffix(line, "service status") {
			continue // service is todo 29's command; the doctor lines are replayed here
		}
		replay := golang(t, filepath.Dir(dhome), optionArgv(t, line)...)
		if strings.Contains(replay.stderr, "expected one argument") || !strings.HasPrefix(strings.TrimSpace(replay.stdout), "{") {
			t.Fatal("unusable:", line, replay.stderr)
		}
	}
}

// optionArgv is the argv a printed line runs, minus its program word(s).
func optionArgv(t *testing.T, line string) []string {
	t.Helper()
	out := runPython(t, os.Getenv("HOME"), "import json,shlex,sys; print(json.dumps(shlex.split(sys.argv[1])))", line)
	var words []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &words); err != nil {
		t.Fatal(err)
	}
	for i, w := range words {
		if strings.HasPrefix(w, "--") {
			return words[i:]
		}
	}
	return nil
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// CLI-30: the wrong-socket refusal offers a matching pair and a note that a store "does not
// rewrite" the socket it recorded.
func Test25_CLI30_the_wrong_socket_refusal_offers_a_matching_pair(t *testing.T) {
	home := pythonHome(t)
	state, first, second := filepath.Join(home, "pair"), filepath.Join(home, "pair-first.sock"), filepath.Join(home, "pair-second.sock")
	pythonStore(t, home, state, first)
	refused := recoverBoth(t, home, "--state", state, "--socket", second, "status")
	joined := strings.Join(anyStrings(refused["recover"]), " ")
	if !strings.Contains(joined, first) || !strings.Contains(joined, second) || !strings.Contains(refused["note"].(string), "does not rewrite") {
		t.Fatal(refused)
	}
}

// CLI-31: recovery vs a CODEX_SESSION_RELAY_STATE pin: the socket-first line runs unpinned (env
// -u), also when the pin names the same store; a flag-caused refusal offers the pinned store as
// its own candidate; a pin spelling the same directory differently is not offered; an
// unexpandable pin keeps the refusal (exit 2, 2 commands, "does not resolve").
func Test25_CLI31_recovery_lines_handle_a_state_pin(t *testing.T) {
	home := pythonHome(t)
	flagged, other, wanted := filepath.Join(home, "flagged"), filepath.Join(home, "other.sock"), filepath.Join(home, "wanted.sock")
	pythonStore(t, home, flagged, other)
	t.Setenv("CODEX_SESSION_RELAY_STATE", flagged)
	same := recoverBoth(t, home, "--state", flagged, "--socket", wanted, "status")
	unpinned := 0
	for _, line := range commands(same["recover"].([]any)) {
		if strings.Contains(line, wanted) && strings.HasPrefix(line, "env -u CODEX_SESSION_RELAY_STATE ") {
			unpinned++
		}
	}
	if unpinned != 1 {
		t.Fatal(same["recover"])
	}
	pinned := filepath.Join(home, "pinned")
	pythonStore(t, home, pinned, wanted)
	t.Setenv("CODEX_SESSION_RELAY_STATE", pinned)
	flagCaused := recoverBoth(t, home, "--state", flagged, "--socket", wanted, "status")
	offered := 0
	for _, line := range commands(flagCaused["recover"].([]any)) {
		if strings.Contains(line, "--state="+pinned) {
			offered++
		}
	}
	if offered != 1 {
		t.Fatal(flagCaused["recover"])
	}
	for _, spelling := range []string{flagged + "/../flagged", "~/flagged"} {
		t.Setenv("CODEX_SESSION_RELAY_STATE", spelling)
		answer := recoverBoth(t, home, "--state", flagged, "--socket", wanted, "status")
		states := 0
		for _, line := range commands(answer["recover"].([]any)) {
			if strings.Contains(line, "--state=") {
				states++
			}
			if strings.Contains(line, "--state="+spelling) {
				t.Fatal("the same directory came back:", line)
			}
		}
		if states != 1 {
			t.Fatal(answer["recover"])
		}
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "~no-such-user-for-this-test/store")
	bad := recoverBoth(t, home, "--state="+flagged, "--socket="+wanted, "status")
	if bad["reason"] != "state_directory_serves_another_socket" || len(commands(bad["recover"].([]any))) != 2 ||
		!strings.Contains(strings.Join(anyStrings(bad["recover"]), " "), "does not resolve") {
		t.Fatal(bad)
	}
}

// seededReceipts registers an assignment with both participants' settings recorded (through
// Python's register, the path a creation result takes) and returns the state directory.
func seededReceipts(t *testing.T, home string, parentSandbox, childSandbox string) string {
	t.Helper()
	state := filepath.Join(home, "state")
	parentCwd, childCwd := filepath.Join(home, "parent"), filepath.Join(home, "work")
	for _, d := range []string{parentCwd, childCwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	register(t, home, state, "--parent-settings", settingsJSON(parentCwd, parentSandbox), "--child-settings", settingsJSON(childCwd, childSandbox))
	return state
}

func receipt(report map[string]any) map[string]any { return obj(report["accessReceipt"]) }

// CLI-32: participants reaching one store by flag, env and discovery report the same storeId and
// device/inode, observed read and write, and their own selectedBy; a participant on another
// store gets another storeId and sameStore is not proven.
func Test25_CLI32_participants_reach_one_store_by_three_routes(t *testing.T) {
	home := pythonHome(t)
	state := seededReceipts(t, home, "", "")
	byFlag := receipt(both(t, home, "--state", state, "doctor"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	byEnv := receipt(both(t, home, "doctor"))
	os.Unsetenv("CODEX_SESSION_RELAY_STATE")
	for name, r := range map[string]map[string]any{"flag": byFlag, "env": byEnv} {
		if obj(r["selectedBy"])["source"] != name || r["storeId"] != byFlag["storeId"] || r["inode"] != byFlag["inode"] || r["device"] != byFlag["device"] {
			t.Fatal(name, r)
		}
		access := obj(r["observedAccess"])
		if access["read"] != true || access["write"] != true || access["detail"] != nil {
			t.Fatal(access)
		}
	}
	other := filepath.Join(home, "other")
	theirs := receipt(both(t, home, "--state", other, "store-identity"))
	_ = theirs
	elsewhere := both(t, home, "--state", other, "doctor", "--expect-store", byFlag["storeId"].(string))
	if receipt(elsewhere)["storeId"] == byFlag["storeId"] || elsewhere["sameStore"] == "proven" {
		t.Fatal(elsewhere["sameStore"])
	}
}

// CLI-33: recordedSandbox carries each participant's effective sandbox (readable, mode,
// writableRoots, networkAccess false, recordedFrom creation_result); omitted defaults are
// filled; a deliverable participant has refusedBy null and resumeMode workspace-write.
func Test25_CLI33_each_receipt_carries_the_effective_sandbox(t *testing.T) {
	home := pythonHome(t)
	state := seededReceipts(t, home, `{"type": "workspaceWrite"}`, "")
	recorded := obj(receipt(both(t, home, "--state", state, "doctor"))["recordedSandbox"])
	if recorded["available"] != true {
		t.Fatal(recorded)
	}
	participants := obj(recorded["participants"])
	for _, task := range []string{parentTask, childTask} {
		p := obj(participants[task])
		if p["readable"] != true || p["mode"] != "workspaceWrite" || p["networkAccess"] != false || p["recordedFrom"] != "creation_result" ||
			p["refusedBy"] != nil || p["resumeMode"] != "workspace-write" {
			t.Fatal(task, p)
		}
	}
	defaults := obj(participants[parentTask])
	if roots, ok := defaults["writableRoots"].([]any); !ok || len(roots) != 0 || defaults["excludeTmpdirEnvVar"] != false || defaults["excludeSlashTmp"] != false {
		t.Fatal(defaults)
	}
	if obj(participants[childTask])["cwd"] != filepath.Join(home, "work") {
		t.Fatal(participants[childTask])
	}
}

// CLI-34: a readable record delivery cannot carry is deliverable false with delivery's reason in
// refusedBy, resumeMode null and a detail naming the refusal.
func Test25_CLI34_a_record_delivery_cannot_carry_is_not_reported_as_one_it_would(t *testing.T) {
	home := pythonHome(t)
	state := seededReceipts(t, home, "", "")
	db := filepath.Join(state, "relay.sqlite3")
	child := filepath.Join(home, "work")
	cases := []struct{ settings, reason, says string }{
		{strings.Replace(settingsJSON(child, `{"type": "externalSandbox"}`), "", "", 1), "unsupported_sandbox_type", "externalSandbox"},
		{strings.Replace(settingsJSON(child, ""), `"cwd": "`+child+`", `, "", 1), "settings_incomplete", "cwd"},
		{strings.Replace(settingsJSON(child, ""), `"cwd": "`+child+`"`, `"cwd": 7`, 1), "settings_mistyped", "cwd is int, not str"},
		{strings.Replace(settingsJSON(child, ""), `"never"`, `"untrusted"`, 1), "unsupported_approval_policy", "'untrusted'"},
		{strings.Replace(settingsJSON(child, ""), `"runtimeWorkspaceRoots": ["`+child+`"], "model"`, `"runtimeWorkspaceRoots": 7, "model"`, 1), "settings_mistyped", "runtimeWorkspaceRoots is int, not a list"},
	}
	for _, c := range cases {
		sqlite(t, home, db, "UPDATE authorized_settings SET settings = '"+strings.ReplaceAll(c.settings, "'", "''")+"' WHERE task_id = '"+childTask+"'")
		participants := obj(obj(receipt(both(t, home, "--state", state, "doctor"))["recordedSandbox"])["participants"])
		p := obj(participants[childTask])
		if p["deliverable"] != false || p["refusedBy"] != c.reason || p["resumeMode"] != nil || !strings.Contains(p["detail"].(string), c.says) || p["readable"] != true {
			t.Fatal(c.reason, p)
		}
		if obj(participants[parentTask])["deliverable"] != true {
			t.Fatal("parent lost")
		}
	}
}

// CLI-37: one unreadable participant row is reported (readable false, "not an object") while the
// rest of the receipt survives.
func Test25_CLI37_one_unreadable_participant_does_not_take_the_diagnosis_with_it(t *testing.T) {
	home := pythonHome(t)
	state := seededReceipts(t, home, "", "")
	sqlite(t, home, filepath.Join(state, "relay.sqlite3"), "UPDATE authorized_settings SET settings = '[]' WHERE task_id = '"+childTask+"'")
	r := receipt(both(t, home, "--state", state, "doctor"))
	participants := obj(obj(r["recordedSandbox"])["participants"])
	if obj(participants[childTask])["readable"] != false || !strings.Contains(obj(participants[childTask])["detail"].(string), "not an object") ||
		obj(participants[parentTask])["readable"] != true || r["storeId"] == nil || obj(r["observedAccess"])["read"] != true {
		t.Fatal(r)
	}
}

// CLI-36: the receipt never pairs one store's identity with another's participants: a store
// replaced by a copy (same storeId, other inode) after the probe, or a probe naming another
// identity, is reported unavailable with no participants and a detail naming it. The details are
// compared with Python's _access_receipt driven the same way.
func Test25_CLI36_a_store_changed_under_the_read_is_reported_not_served(t *testing.T) {
	home := pythonHome(t)
	state := seededReceipts(t, home, "", "")
	selection, err := store.ResolveStateDir(state, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	honest := cli.AccessReceipt(ctx, state, store.Probe(ctx, selection))
	if recordedOf(honest)["available"] != true {
		t.Fatal(honest)
	}
	// A probe that names another identity.
	moved := store.Probe(ctx, selection)
	moved.Store.StoreID = "another-store-entirely"
	swapped := recordedOf(cli.AccessReceipt(ctx, state, moved))
	pySwapped := pythonJSON(t, runPython(t, home, `
import json, sys
from codex_session_relay.cli import _access_receipt
from codex_session_relay.store import probe, resolve_state_dir
class S: pass
s = S(); s.selection = resolve_state_dir(sys.argv[1])
report = probe(s.selection)
moved = dict(report, store=dict(report["store"], storeId="another-store-entirely"))
print(json.dumps(_access_receipt(s, moved)["recordedSandbox"]))`, state))
	if swapped["available"] != false || len(obj(swapped["participants"])) != 0 || swapped["detail"] != pySwapped["detail"] ||
		!strings.Contains(swapped["detail"].(string), "changed under this command") {
		t.Fatalf("go %v\npython %v", swapped, pySwapped)
	}
	// A copy replacing the file after the probe.
	measured := store.Probe(ctx, selection)
	database := filepath.Join(state, "relay.sqlite3")
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(state, "replacement.sqlite3")
	if err := os.WriteFile(replacement, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, database); err != nil {
		t.Fatal(err)
	}
	copied := recordedOf(cli.AccessReceipt(ctx, state, measured))
	if copied["available"] != false || len(obj(copied["participants"])) != 0 || !strings.Contains(copied["detail"].(string), "inode") {
		t.Fatal(copied)
	}
	want := "the store changed under this command: device:inode " + strconv.FormatUint(measured.Store.Device, 10) + ":" + strconv.FormatUint(measured.Store.Inode, 10) + " was measured, rows were read from "
	if !strings.HasPrefix(copied["detail"].(string), want) {
		t.Fatalf("detail %q, want prefix %q (cli.py:2708)", copied["detail"], want)
	}
}

func recordedOf(receipt any) map[string]any { return obj(obj(plain(receipt))["recordedSandbox"]) }
