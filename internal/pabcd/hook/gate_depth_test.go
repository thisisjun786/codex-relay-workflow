package hook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
		{"create_goal with a token budget", "pre-tool-use-guarding-goal-budget", "gate-1", "create_goal", `{"objective":"x","token_budget":1%s}`, "Use create_goal with objective only"},
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
