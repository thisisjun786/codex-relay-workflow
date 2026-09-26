package registry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type contractObject = contract.OrderedObject

var stamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}\+00:00`)

type cliStep struct {
	Argv  []string          `json:"argv"`
	Files map[string]string `json:"files"`
	SQL   string            `json:"sql"`
}

type cliResult struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
}

// runCLICase replays one case of testdata/cli_cases.json through Execute, exactly as
// gen_cli.py drives the Python CLI, and returns the results plus the case's state directory.
func runCLICase(t *testing.T, name string) ([]cliResult, string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/cli_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string][]cliStep
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	steps, ok := cases[name]
	if !ok {
		t.Fatalf("no case %q", name)
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	var results []cliResult
	for _, step := range steps {
		for file, text := range step.Files {
			if err := os.WriteFile(filepath.Join(home, file), []byte(strings.ReplaceAll(text, "${HOME}", home)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if step.SQL != "" {
			s, err := store.Open(ctx(), filepath.Join(state, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx(), step.SQL); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			continue
		}
		argv := []string{"--state", state}
		for _, a := range step.Argv {
			argv = append(argv, strings.ReplaceAll(a, "${HOME}", home))
		}
		var stdout, stderr bytes.Buffer
		code := Execute(ctx(), argv, &stdout, &stderr)
		text := strings.ReplaceAll(stamp.ReplaceAllString(stdout.String(), "<T>"), home, "<HOME>")
		results = append(results, cliResult{code, text})
	}
	return results, state
}

// sameCLIAsPython compares every step's exit code and whole stdout bytes with Python's.
func sameCLIAsPython(t *testing.T, name string) ([]cliResult, string) {
	t.Helper()
	got, state := runCLICase(t, name)
	raw, err := os.ReadFile("testdata/python_cli.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string][]cliResult
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	want := all[name]
	if len(got) != len(want) {
		t.Fatalf("%s: %d steps, python %d", name, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s step %d differs from Python\n go (exit %d):\n%s\n py (exit %d):\n%s", name, i, got[i].Exit, got[i].Stdout, want[i].Exit, want[i].Stdout)
		}
	}
	return got, state
}

func stdoutJSON(t *testing.T, r cliResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		t.Fatalf("stdout is not JSON: %q", r.Stdout)
	}
	return out
}

func count(t *testing.T, state, query string) int {
	t.Helper()
	s, err := store.Open(ctx(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.DB.QueryRowContext(ctx(), query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func Test25_CLI14_register_records_both_endpoints_settings_and_settings_show_reads_them(t *testing.T) {
	got, _ := sameCLIAsPython(t, "register_with_settings")
	if payload := stdoutJSON(t, got[0]); payload["authorizedSettings"].(map[string]any)["01parent-task"] != "recorded" {
		t.Fatal(payload)
	}
	got, _ = sameCLIAsPython(t, "register_without_settings")
	if stdoutJSON(t, got[0])["authorizedSettings"] != nil || stdoutJSON(t, got[1])["usable"] != false {
		t.Fatal(got)
	}
	got, _ = sameCLIAsPython(t, "settings_record_file")
	if stdoutJSON(t, got[0])["source"] != "creation_result" || stdoutJSON(t, got[1])["usable"] != true {
		t.Fatal(got)
	}
}

func Test25_CLI15_settings_are_validated_where_written_and_nothing_is_stored(t *testing.T) {
	for name, reason := range map[string]string{"settings_incomplete": SettingsIncomplete, "settings_mistyped": SettingsMistyped, "settings_untrusted": UnsupportedApprovalPolicy} {
		got, _ := sameCLIAsPython(t, name)
		if got[0].Exit != 2 || stdoutJSON(t, got[0])["reason"] != reason {
			t.Fatal(name, got[0])
		}
	}
}

func Test25_CLI16_settings_show_separates_complete_from_deliverable(t *testing.T) {
	for name, code := range map[string]string{"show_cwd_int": SettingsMistyped, "show_bare_sandbox": UnsupportedSandboxType} {
		got, _ := sameCLIAsPython(t, name)
		shown := stdoutJSON(t, got[len(got)-1])
		if shown["usable"] != true || shown["deliverable"] != false || shown["recordFinding"].(map[string]any)["code"] != code {
			t.Fatal(name, shown)
		}
	}
}

func Test25_CLI11_pause_refuses_and_resume_requires_the_restated_scope(t *testing.T) {
	got, _ := sameCLIAsPython(t, "pause_resume")
	if got[2].Exit != 2 || stdoutJSON(t, got[2])["reason"] != "relationship_not_active" || stdoutJSON(t, got[3])["status"] != "active" {
		t.Fatal(got)
	}
}

// The generation, anchor, admission and status commands answer exactly as Python on success and
// refusal (Domain/cli-shape for generation-open, generation-bind, admit-turn, relationship-status).
func Test25_CLI_generation_anchor_admission_and_status_commands_match_python(t *testing.T) {
	sameCLIAsPython(t, "generations_cli")
	sameCLIAsPython(t, "register_refusals")
	sameCLIAsPython(t, "argparse_stray_positional")
	got, _ := sameCLIAsPython(t, "clear_and_cite")
	if got[0].Exit != 4 {
		t.Fatal(got)
	}
}

func Test25_SPR8_registration_refuses_an_invalid_row_rather_than_storing_it(t *testing.T) {
	got, state := sameCLIAsPython(t, "settings_sandbox_list")
	if stdoutJSON(t, got[0])["reason"] != UnsupportedSandboxType || stdoutJSON(t, got[1])["settings"] != nil {
		t.Fatal(got)
	}
	if n := count(t, state, "SELECT COUNT(*) FROM authorized_settings"); n != 0 {
		t.Fatalf("%d rows stored", n)
	}
}

func Test25_SPR13_settings_round_trip_and_a_re_record_replaces(t *testing.T) {
	got, state := sameCLIAsPython(t, "re_record_replaces")
	if stdoutJSON(t, got[2])["settings"].(map[string]any)["cwd"] != "/b" {
		t.Fatal(got[2])
	}
	if n := count(t, state, "SELECT COUNT(*) FROM authorized_settings"); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// RAT-2: a registration refused by a linkage contest rolls back entirely, and the contest is
// recorded exactly once per refusal (one conflict row, one journal line each time).
func Test25_RAT2_a_contest_outlives_the_rolled_back_registration_exactly_once(t *testing.T) {
	_, state := sameCLIAsPython(t, "contested_project")
	for query, want := range map[string]int{
		"SELECT COUNT(*) FROM relationships":                          0,
		"SELECT COUNT(*) FROM generations":                            0,
		"SELECT COUNT(*) FROM authorized_settings":                    0,
		"SELECT COUNT(*) FROM linkage_conflicts":                      1,
		"SELECT COUNT(*) FROM journal WHERE kind = 'linkage_refused'": 2,
	} {
		if n := count(t, state, query); n != want {
			t.Errorf("%s = %d, want %d", query, n, want)
		}
	}
	// Uncomposed: registry.register's own transaction commits the contest; nothing writes it again.
	r := newRegistry(t)
	if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b','parent','project','PROJ-1','01parent-two','host-a','active',1,'t','t')"); err != nil {
		t.Fatal(err)
	}
	in := fixture()
	in.ProjectKey = "PROJ-1"
	_, err := r.Register(ctx(), in)
	mustReason(t, err, "foreign_scope")
	var conflicts, journalled int
	if err := r.Store.DB.QueryRowContext(ctx(), "SELECT (SELECT COUNT(*) FROM linkage_conflicts), (SELECT COUNT(*) FROM journal WHERE kind='linkage_refused')").Scan(&conflicts, &journalled); err != nil {
		t.Fatal(err)
	}
	if conflicts != 1 || journalled != 1 {
		t.Fatalf("uncomposed: %d conflicts, %d journal lines", conflicts, journalled)
	}
}

// A registration with --project binds the child and scopes the issue; the lifecycle moves the
// binding; the creation role is not rewritten later (role_binding_mismatch).
func Test25_CLI_project_registration_binds_and_keeps_the_creation_role(t *testing.T) {
	sameCLIAsPython(t, "project_attach")
}

func settingsFixture(cwd string) contractObject {
	raw := `{"sandbox":{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false},"approvalPolicy":"never","cwd":"` + cwd + `","runtimeWorkspaceRoots":["` + cwd + `"],"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh","environments":[{"environmentId":"local","cwd":"` + cwd + `","runtimeWorkspaceRoots":["` + cwd + `"]}]}`
	v, err := decodeJSON([]byte(raw))
	if err != nil {
		panic(err)
	}
	return v.(contractObject)
}

func registerWithBothSettings(r *Registry) error {
	_, err := r.RegisterWithSettings(ctx(), fixture(), []settingsWrite{
		{task: parent, values: settingsFixture("/parent")},
		{task: child, values: settingsFixture(root)},
	})
	return err
}

// RAT-1: register is one transaction over relationships, generations and authorized_settings.
// A full registration commits exactly once; a process killed at that commit leaves none of the
// three tables with a row.
func Test25_RAT1_register_is_one_transaction_and_a_kill_before_commit_leaves_nothing(t *testing.T) {
	if path := os.Getenv("CRW_RAT1_DB"); path != "" {
		s, err := store.Open(ctx(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		s.SetFaultHook(func() {
			fmt.Println("written")
			_, _ = io.Copy(io.Discard, os.Stdin)
		})
		t.Fatalf("reached COMMIT: %v", registerWithBothSettings(&Registry{Store: s, Now: func() string { return fakeISO }}))
	}
	commits := 0
	r := newRegistry(t)
	r.Store.SetFaultHook(func() { commits++ })
	if err := registerWithBothSettings(r); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Fatalf("%d commits, want 1: the registration is not one transaction", commits)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seed, err := store.Open(ctx(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "CRW_RAT1_DB="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := bufio.NewScanner(out)
	if !line.Scan() || line.Text() != "written" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child did not reach the commit: %q", line.Text())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // killed on purpose
	for _, table := range []string{"relationships", "generations", "authorized_settings"} {
		if n := count(t, dir, "SELECT COUNT(*) FROM "+table); n != 0 {
			t.Errorf("%s holds %d rows after the kill", table, n)
		}
	}
}

// Host errors keep Python's envelope: exit 3, {"error": "host", "detail": "<Class>: <message>"}.
func Test25_CLI_host_errors_carry_pythons_class_and_message(t *testing.T) {
	sameCLIAsPython(t, "host_errors")
}

// The JSONDecodeError text a malformed --settings answers with is Python's, message and position.
func Test25_CLI_settings_json_errors_read_like_pythons(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_jsonerr.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]string
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := store.PythonJSONError(c[0]); got != c[1] {
			t.Errorf("%q: go %q, python %q", c[0], got, c[1])
		}
	}
}

// CLI-6: a refusal exits 2 with {"error": "refused", "reason": <machine-readable reason>,
// "detail"}, byte-identical to Python (exercised through this package's commands; the property's
// own example, emit scope_escape, is todo 21's command).
func Test25_CLI6_a_refusal_exits_two_with_a_machine_readable_reason(t *testing.T) {
	got, _ := sameCLIAsPython(t, "register_refusals")
	for _, step := range got[1:] {
		payload := stdoutJSON(t, step)
		if step.Exit != 2 || payload["error"] != "refused" || payload["reason"] == nil || len(payload) != 3 {
			t.Fatal(step)
		}
	}
}

// CLI-8 (value half): the ACK proof a caller computes is sha256("E|T") hex, the value
// 'codex-session-relay ack-proof' prints. The ack-proof and ack commands are todo 21's surface.
func Test25_CLI8_the_ack_proof_is_sha256_of_event_and_turn(t *testing.T) {
	got, err := store.AckProof("0123456789abcdef0123456789abcdef", "turn-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "351770a8e0241f932e68981652c6a760e693229f152b334c14123470337a7e41" {
		t.Fatalf("ack proof %s", got)
	}
}
