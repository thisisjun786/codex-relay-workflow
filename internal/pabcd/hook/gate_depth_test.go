package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1075. JSON.parse reads a document at any nesting depth, so a field no gate reads can nest as
// deep as the input bound allows without changing the verdict (goal-gate.ts:81,113,136,201,
// automation-ownership-gate.ts:71). encoding/json refuses a container past 10,000 levels, which
// turned a valid payload into "no payload" and flipped the verdicts. These cases run the real
// `crw hook pre-tool-use --leg <leg>` entry (harness.Hook) and compare the bytes with the same call
// without the deep field; the oracle's cli.js was run on the same payloads for the expected side.

const gateDeepJustOver = 10001 // the issue's reproduction: one level past what encoding/json reads

func gateNested(depth int) string { return strings.Repeat("[", depth) + strings.Repeat("]", depth) }

type gateHome struct{ cwd, sqliteHome string }

// gateEnv points every home the legs read into temporary directories, with the goals database
// unreadable (a file that is not a database).
func gateEnv(t *testing.T) gateHome {
	t.Helper()
	dir := t.TempDir()
	for key, sub := range map[string]string{"HOME": "home", "CODEX_HOME": "codex", "CRW_HOME": "crw"} {
		path := filepath.Join(dir, sub)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	t.Setenv("PLUGIN_ROOT", "") // no hook record
	h := gateHome{cwd: filepath.Join(dir, "work"), sqliteHome: filepath.Join(dir, "sqlite")}
	for _, path := range []string{h.cwd, h.sqliteHome} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.sqliteHome, "goals_1.sqlite"), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SQLITE_HOME", h.sqliteHome)
	return h
}

// gateRun is `crw hook pre-tool-use --leg <leg>` with the payload on stdin: stdout and the exit status.
func gateRun(t *testing.T, leg, raw string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := harness.Hook(context.Background(), []string{"pre-tool-use", "--leg", leg}, strings.NewReader(raw), &stdout, &stderr, os.LookupEnv, harness.Legs())
	return stdout.String(), code
}

// gatePayload splices the already encoded fields of tool_input into the PreToolUse envelope.
func gatePayload(t *testing.T, cwd, session, tool, toolInput string) string {
	t.Helper()
	cwdJSON, _ := json.Marshal(cwd)
	return `{"hook_event_name":"PreToolUse","session_id":"` + session + `","cwd":` + string(cwdJSON) + `,"tool_name":"` + tool + `","tool_input":` + toolInput + `}`
}

func TestGateVerdictsDoNotDependOnTheDepthOfAFieldTheyNeverRead(t *testing.T) {
	h := gateEnv(t)
	// A session state that exists but is not a state document: update_goal complete is denied.
	badState := state.StatePath(h.cwd, "gate-bad")
	if err := os.MkdirAll(filepath.Dir(badState), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badState, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, leg, session, tool, input string // input holds %s where the ignored field goes
		want                            string // a substring of the shallow answer ("" is silence)
	}{
		{"request_user_input while the goals database is unreadable", "pre-tool-use-guarding-interview-in-goal", "gate-2", "request_user_input", `{"questions":[]%s}`, "goal-active=unreadable"},
		{"update_goal complete over an unreadable session state", "pre-tool-use-guarding-goal-complete", "gate-bad", "update_goal", `{"status":"complete"%s}`, "state is unreadable"},
		{"automation_update view", "pre-tool-use-guarding-automation-ownership", "gate-4", "automation_update", `{"mode":"view"%s}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shallow, code := gateRun(t, c.leg, gatePayload(t, h.cwd, c.session, c.tool, strings.Replace(c.input, "%s", "", 1)))
			if code != 0 || (c.want == "") != (shallow == "") || !strings.Contains(shallow, c.want) {
				t.Fatalf("the shallow call answered %q (exit %d), want %q", shallow, code, c.want)
			}
			// 9,997 levels is what encoding/json still reads; 10,001 is the first it refused; 400,000 is
			// far past it and still inside the input bound (about 800 KB).
			for _, depth := range []int{9997, gateDeepJustOver, 400000} {
				deep := strings.Replace(c.input, "%s", `,"ignored":`+gateNested(depth), 1)
				got, code := gateRun(t, c.leg, gatePayload(t, h.cwd, c.session, c.tool, deep))
				if code != 0 || got != shallow {
					t.Errorf("depth %d: exit %d, %q; the call without the field answered %q", depth, code, got, shallow)
				}
			}
		})
	}
}

// Documents that are not JSON, or not an object, are still handled as they were.
func TestGateDepthDoesNotChangeHowBrokenJSONIsHandled(t *testing.T) {
	h := gateEnv(t)
	deep := gateNested(gateDeepJustOver)
	cases := []struct{ name, leg, raw, want string }{
		{"goal gate: broken JSON passes", "pre-tool-use-guarding-goal-budget", `{"hook_event_name":"PreToolUse","tool_input":` + deep + `,`, ""},
		{"goal gate: deep top-level array passes", "pre-tool-use-guarding-goal-budget", deep, ""},
		{"automation gate: broken JSON cannot be verified", "pre-tool-use-guarding-automation-ownership", `{"tool_name":"automation_update","x":` + deep + `,`, "Cannot verify automation ownership"},
		// The reader accepts only the escapes JSON has: \x0065 is not \u0065, so this payload is as broken as it is for
		// JSON.parse and for the json.Valid check this reader replaced.
		{"automation gate: an unknown escape followed by four hex digits is broken JSON", "pre-tool-use-guarding-automation-ownership", `{"hook_event_name":"PreToolUse","tool_name":"automation_update","tool_input":{"mode":"vi\x0065w"}}`, "Cannot verify automation ownership"},
		{"goal gate: an unknown escape followed by four hex digits is not a payload", "pre-tool-use-guarding-interview-in-goal", `{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/","tool_name":"request_user_input","tool_input":{"questions":[],"note":"a\x0065"}}`, ""},
		{"automation gate: a deep array is not a payload", "pre-tool-use-guarding-automation-ownership", deep, "Malformed native hook payload."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, code := gateRun(t, c.leg, c.raw)
			if code != 0 || (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Errorf("exit %d, %q, want %q", code, got, c.want)
			}
		})
	}
	_ = h
}

// CRW-1075 (verification fix). A Go stack overflow is a fatal error no recover answers, so
// the gates must meet a document inside the input bound whose nesting runs to millions of levels
// without recursing once per opener. These cases run the hook in a child process (this test binary,
// re-executed) and compare exit code and stdout with the oracle's: the CXC v0.2.40 cli.js answers a
// broken document of 3,000,000 "[" with no output and exit 0, and reads a valid one 2,000,000 deep.
const gateChildEnv = "CRW_GATE_DEPTH_CHILD_LEG"

func TestGateDepthChildProcess(t *testing.T) {
	leg := os.Getenv(gateChildEnv)
	if leg == "" {
		t.Skip("child process of TestGateBoundsNestingByTheInputItReads")
	}
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(70)
	}
	// A hook runs in a memory-limited process. A reader that recurses once per container needs a
	// stack in proportion to the nesting of its input, so the child gets a stack no recursion of
	// that depth fits: the fatal "stack exceeds limit" error is what a real overflow looks like.
	debug.SetMaxStack(32 << 20)
	code := harness.Hook(context.Background(), []string{"pre-tool-use", "--leg", leg}, bytes.NewReader(raw), os.Stdout, os.Stderr, os.LookupEnv, harness.Legs())
	os.Exit(code)
}

func gateChild(t *testing.T, leg, raw string) (string, int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestGateDepthChildProcess$")
	cmd.Env = append(os.Environ(), gateChildEnv+"="+leg)
	cmd.Stdin = strings.NewReader(raw)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	tail := stderr.String()
	if len(tail) > 300 {
		tail = tail[:300]
	}
	return stdout.String(), code, tail
}

func TestGateBoundsNestingByTheInputItReads(t *testing.T) {
	if testing.Short() {
		t.Skip("reads megabytes of nesting")
	}
	h := gateEnv(t)
	const leg = "pre-tool-use-guarding-interview-in-goal"
	valid := func(depth int) string {
		return gatePayload(t, h.cwd, "gate-1", "request_user_input", `{"questions":[],"ignored":`+gateNested(depth)+`}`)
	}
	unclosed := gatePayload(t, h.cwd, "gate-1", "request_user_input", `{"questions":[],"ignored":`)
	unclosed = unclosed[:len(unclosed)-1] + strings.Repeat("[", 2_500_000)
	shallow, code := gateRun(t, leg, valid(1))
	if code != 0 || !strings.Contains(shallow, "goal-active=unreadable") {
		t.Fatalf("the shallow call answered %q (exit %d)", shallow, code)
	}
	cases := []struct {
		name, raw, want string
	}{
		{"3,000,000 unclosed arrays are not a payload", strings.Repeat("[", 3_000_000), ""},
		{"4 MiB of unclosed arrays are not a payload", strings.Repeat("[", 4*1024*1024), ""},
		{"an envelope that never closes its 2,500,000-deep field is not a payload", unclosed, ""},
		// ReadStdin bounds the encoded bytes; each 0xff of the input decodes to the three bytes of U+FFFD, so the
		// nesting the decoded text allows is more than the input bound holds in openers alone.
		{"3 MiB of unclosed arrays padded with invalid UTF-8 to the input bound are not a payload", strings.Repeat("[", 3*1024*1024) + strings.Repeat("\xff", 1024*1024), ""},
		{"1,500,000 closed arrays and trailing text are not a payload", strings.Repeat("[", 1_500_000) + strings.Repeat("]", 1_500_000) + "x", ""},
		{"600,000 closed objects and trailing text are not a payload", strings.Repeat(`{"a":`, 600_000) + "1" + strings.Repeat("}", 600_000) + "x", ""},
		{"800,000 unclosed objects are not a payload", strings.Repeat(`{"a":`, 800_000), ""},
		{"a valid payload 1,900,000 deep keeps its verdict", valid(1_900_000), shallow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, code, stderr := gateChild(t, leg, c.raw)
			if code != 0 || got != c.want {
				t.Errorf("exit %d, stdout %q, want exit 0 and %q; stderr %q", code, got, c.want, stderr)
			}
		})
	}
}

// CRW-1075 (verification fix round 2). The gates read their payload at any depth, so the entry that
// runs them must too: cli.ts:415-426 reads agent_id/agent_type (parse.ts isSubagentHookPayload) and
// payload.cwd with JSON.parse before a gate runs. A child's deep payload is exempt, and a deep
// payload whose cwd turns PABCD off is silent, as the oracle's cli.js answers both (exit 0, no
// output); an entry that read them with encoding/json's depth limit ran the gate instead.
func TestGateEntryReadsTheSubagentStampAndCwdAtAnyDepth(t *testing.T) {
	h := gateEnv(t)
	t.Setenv("CRW_PABCD", "")
	off := t.TempDir()
	if err := os.WriteFile(filepath.Join(off, "crw.json"), []byte(`{"pabcd":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // the process cwd has no crw.json: PABCD is on there
	child := func(depth int) string {
		raw := gatePayload(t, h.cwd, "gate-1", "request_user_input", `{"questions":[],"ignored":`+gateNested(depth)+`}`)
		return raw[:len(raw)-1] + `,"agent_id":"child-1","agent_type":"worker"}`
	}
	disabled := func(depth int) string {
		return gatePayload(t, off, "gate-2", "request_user_input", `{"questions":[],"ignored":`+gateNested(depth)+`}`)
	}
	cases := []struct {
		name, leg string
		raw       func(int) string
	}{
		{"a child's request_user_input in goal mode is exempt", "pre-tool-use-guarding-interview-in-goal", child},
		{"request_user_input in a project with PABCD off is silent", "pre-tool-use-guarding-interview-in-goal", disabled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, depth := range []int{1, 9997, gateDeepJustOver, 400000} {
				if got, code := gateRun(t, c.leg, c.raw(depth)); code != 0 || got != "" {
					t.Errorf("depth %d: exit %d, %q; want exit 0 and no output", depth, code, got)
				}
			}
		})
	}
}
