//go:build dev

package ci

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// CRW-1579 (D8): every place the program decides a temporary location goes through internal/tmploc,
// the one place. The check greps every tracked file for the direct deciders: os.TempDir(), a literal
// /tmp, /var/tmp or /dev/shm, MkdirTemp or CreateTemp with an empty parent (which is TMPDIR, decided
// inside os), and the direct reads of TMPDIR. Every hit must be an exception with its reason, and every
// exception entry must still match a hit, so a stale entry fails too.
//
// The pattern is written with bracket expressions so that its own text is no hit. This file's
// tables quote the lines they allow (E7): tmpMetadataLines recognizes complete string-only
// exception entries as metadata rather than new location decisions.
const tmpDecisionPattern = `os[.]TempDir[(][)]|"/[t]mp|"/var/[t]mp|"/dev/[s]hm|MkdirTemp[(]""|CreateTemp[(]""|[(]"TMPDIR"[)]|"TMPDIR", "TM[P]"`

// tmpParentPattern finds the os.MkdirTemp and os.CreateTemp calls whose parent the variable-parent
// rule judges.
const tmpParentPattern = `os[.](MkdirTemp|CreateTemp)[(]`

// tmpHit is one line a pattern found: the file, the line number and the line's text, trimmed.
type tmpHit struct {
	file string
	line int
	text string
}

func (h tmpHit) String() string { return fmt.Sprintf("%s:%d: %s", h.file, h.line, h.text) }

// tmpEntry allows one line: the file and the line's exact text (trimmed), with the reason it is
// allowed.
type tmpEntry struct {
	file, text, why string
}

func (e tmpEntry) matches(h tmpHit) bool { return e.file == h.file && e.text == h.text }

// tmpGlob is an exception that covers whole files, with its reason.
type tmpGlob struct {
	name   string
	reason string
	covers func(file string) bool
}

// E1: internal/tmploc (non-test) is the one place: it holds DefaultDir, SocketRoot and the read of
// TMPDIR. It is matched by file and exact line text, so a new hit in tmploc still fails.
const tmpE1 = "E1 internal/tmploc is the one place: DefaultDir, SocketRoot and the one read of TMPDIR"

var tmpTmplocLines = []tmpEntry{
	{"internal/tmploc/dir.go", `const DefaultDir = "/tmp"`, tmpE1},
	{"internal/tmploc/dir.go", `if v := getenv("TMPDIR"); v != "" {`, tmpE1},
	{"internal/tmploc/dir.go", `return os.Getenv("TMPDIR")`, tmpE1},
	{"internal/tmploc/doc.go", `// ("/tmp"), reported with source "default" (G-D1 = A, Q63ANS1722). The default is a location only:`, tmpE1},
	{"internal/tmploc/socket.go", `const SocketRoot = "/tmp"`, tmpE1},
	{"internal/tmploc/tmploctest/tmploctest.go", `return "/dev/shm"`, "E1 test-only memory-backed harness candidate; the test helper centralizes this location"},
	{"internal/tmploc/tmploctest/tmploctest.go", `for _, dir := range []string{tmploc.DefaultDir, "/var/tmp"} {`, "E1 test-only lexical fixture-path fallback candidates; the test helper centralizes their selection"},
}

// E4: the recorded hook payload text in contracttest/hook.go, the same as contract/fixtures/hook.
// It is matched by file and exact line text.
var tmpRecordedPayload = tmpEntry{
	"internal/contracttest/hook.go",
	`payload = map[string]any{"cwd": "/tmp/workspace", "hook_event_name": "Stop", "last_assistant_message": "I finished the task.", "model": "test-model", "permission_mode": "default", "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688", "stop_hook_active": false, "transcript_path": "/tmp/transcript.jsonl", "turn_id": "turn-1"}`,
	"E4 recorded hook payload text, the same as contract/fixtures/hook: not a location this program chooses",
}

// E5: the cxccorpus RecordedScratch line, the recorded oracle condition of the path_length_dependent
// fixtures: their case root <RecordedScratch>/cxc-rec-<16 hex> is the recorded 33 bytes, and the
// recorder's default and the long cases' replay both use it (a replay that cannot use it fails with
// the reason). It is matched by file and exact line text.
var tmpRecordedScratch = tmpEntry{
	"internal/dev/cxccorpus/engine.go",
	`const RecordedScratch = "/var/tmp"`,
	"E5 the recorded case-root parent of the path_length_dependent fixtures (33 bytes): the recorded oracle condition, not a location this program chooses",
}

// E6: port history prose and port records that quote code or Python literals. These are records of
// what was, not locations; a new file with a hit fails. The visualizer record quotes the original
// diagram-to-html.sh output directory line that its recorded edit replaces.
var tmpPortProse = []string{
	"port/cxc/records/crw-dev-visualizer.json",
	"docs/port-cxc/known-defects.md",
	"docs/port-cxc/known-defects/CRW-1393.md",
	"docs/port/refactor-backlog.d/11-real-use-run-4/_section.md",
	"docs/port/refactor-backlog.d/18-closed-by-the-real-use-run-4-record/_section.md",
	"docs/port/refactor-backlog.md",
}

// tmpRecordedData is E3's file rule: testdata and contract/fixtures.
func tmpRecordedData(file string) bool {
	return strings.Contains("/"+file, "/testdata/") || strings.HasPrefix(file, "contract/fixtures/")
}

// tmpGlobs are the exceptions that cover whole files.
var tmpGlobs = []tmpGlob{
	{"E3", "recorded data and the oracle's own programs (testdata, contract/fixtures): not locations this program chooses", tmpRecordedData},
	{"E6", "port history prose and port records that quote code or Python literals", func(file string) bool {
		return slices.Contains(tmpPortProse, file)
	}},
	{"CRW-1580 후속: CRW-1591/1423 착지 뒤: internal/dev/ci/local_home_test.go", "CRW-1591(A) is changing the same local code; excluded from the first CRW-1580 commit", func(file string) bool {
		return file == "internal/dev/ci/local_home_test.go"
	}},
	{"CRW-1580 후속: CRW-1591/1423 착지 뒤: internal/dev/ci/local_seal_test.go", "CRW-1591(A) is changing the same local code; excluded from the first CRW-1580 commit", func(file string) bool {
		return file == "internal/dev/ci/local_seal_test.go"
	}},
	{"CRW-1580 후속: CRW-1591/1423 착지 뒤: internal/dev/ci/local_test.go", "CRW-1591(A) is changing the same local code; excluded from the first CRW-1580 commit", func(file string) bool {
		return file == "internal/dev/ci/local_test.go"
	}},
	{"CRW-1580 후속: CRW-1591/1423 착지 뒤: internal/flow/pullrequest/cli_test.go", "CRW-1423(H) holds internal/flow/pullrequest/**; excluded from the first CRW-1580 commit", func(file string) bool {
		return file == "internal/flow/pullrequest/cli_test.go"
	}},
	{"CRW-1580 후속: CRW-1591/1423 착지 뒤: internal/flow/pullrequest/remote_test.go", "CRW-1423(H) holds internal/flow/pullrequest/**; excluded from the first CRW-1580 commit", func(file string) bool {
		return file == "internal/flow/pullrequest/remote_test.go"
	}},
}

// tmpTestLines are the named test-source lines the location guard allows (CRW-1580):
// fixture inputs, policy assertions and recorded text, each with its reason. Entries match the
// file and exact trimmed source text, independently of line numbers.
var tmpTestLines = []tmpEntry{
	{"cmd/crw/component_spawn_test.go", "{\"a subagent spawn is denied before anything else\", `{\"hook_event_name\":\"PreToolUse\",\"session_id\":\"s\",\"cwd\":\"/tmp\",\"tool_name\":\"spawn_agent\",\"agent_id\":\"child-1\",\"agent_type\":\"explorer\",\"tool_input\":{\"message\":\"spawn a helper\"},\"tool_use_id\":\"call-1\"}`, spawnLegWarned(spawnLegRecursionDeny) + \"}}\\n\"},", "E7 hook payload fixtures for recursion refusal and input size; cwd is recorded input, not an allocated directory"},
	{"cmd/crw/component_spawn_test.go", "{\"an input over 4 MiB is released\", `{\"hook_event_name\":\"PreToolUse\",\"session_id\":\"s\",\"cwd\":\"/tmp\",\"tool_name\":\"spawn_agent\",\"tool_input\":{\"message\":\"big\",\"agent_type\":\"explorer\"}}` + strings.Repeat(\" \", 4194305), spawnLegReleased},", "E7 hook payload fixtures for recursion refusal and input size; cwd is recorded input, not an allocated directory"},
	{"internal/bridge/execution/authorize_test.go", "p := withException(t, doc{\"model\": unapproved, \"reasoningEffort\": \"high\", \"cwd\": []any{\"/tmp\"}, \"role\": \"parent\"})", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/authorize_test.go", "got := authorize(t, p, Input{Model: unapproved, Effort: \"high\", CWD: \"/tmp\", Exception: \"one-task\", Role: \"parent\"})", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/authorize_test.go", "entry := doc{\"model\": unapproved, \"reasoningEffort\": \"high\", \"cwd\": []any{\"/tmp\"}}", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/authorize_test.go", "r := refused(t, second(withException(t, entry).Authorize(Input{Model: unapproved, Effort: \"high\", CWD: \"/tmp\", Exception: \"one-task\", Role: row.cited})))", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/authorize_test.go", "p := withException(t, doc{\"model\": unapproved, \"reasoningEffort\": \"high\", \"cwd\": []any{\"/tmp\"}})", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/authorize_test.go", "if authorize(t, p, Input{Model: unapproved, Effort: \"high\", CWD: \"/tmp\", Exception: \"one-task\"}).Model != unapproved {", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/execution_test.go", "e := doc{\"model\": model, \"reasoningEffort\": effort, \"cwd\": []any{\"/tmp\"}}", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/bridge/execution/execution_test.go", "{doc{\"exceptions\": doc{\"e\": doc{\"model\": model, \"cwd\": []any{\"/tmp\"}}}}, \"missing\"},", "E7 policy authorization fixture cwd values; these tests compare policy inputs and do not allocate those paths"},
	{"internal/contracttest/hook_native_test.go", "stopTranscript = \"/tmp/transcript.jsonl\"", "E7 the frozen native hook payload's transcript and cwd; identical to the recorded contract fixture"},
	{"internal/contracttest/hook_native_test.go", "stopCWD        = \"/tmp/workspace\"", "E7 the frozen native hook payload's transcript and cwd; identical to the recorded contract fixture"},
	{"internal/crwconfig/hostpaths_test.go", "for _, line := range []string{\"/home/jun/code\", \"/state/crw/mgmt\", \"/scratch/crw/fuzz\", \"/var/tmp/crw-12\", \"/var/tmp/cxc-v0.2.40/plugins\", \"/dev/nvme0n1p2\", \"/dev/sda1\", \"nvme1n1\", \"/mnt/disk2\", \"lina-os\"} {", "E7 path strings whose presence or absence in public configuration documentation is the assertion"},
	{"internal/crwconfig/hostpaths_test.go", "for _, line := range []string{\"~/.local/state/crw\", \"scratch_root\", \"/tmp/crw\", \"$XDG_STATE_HOME/crw\", \"/mnt/user-data/outputs\", \"cxc-v0.2.40/plugins/codexclaw\", \"/var/tmp/cxc-v0.2.40-rec\"} {", "E7 path strings whose presence or absence in public configuration documentation is the assertion"},
	{"internal/crwconfig/paths_test.go", "RootTemp:     filepath.Join(\"/tmp\", \"crw\"),", "E7 expected default temp_root with TMPDIR unset or empty; the location rule is the subject"},
	{"internal/crwconfig/paths_test.go", "{\"TMPDIR unset\", rootsEnv(\"HOME\", home), nil, true, \"/tmp/crw\", SourceDefault, \"\"},", "E7 expected default temp_root with TMPDIR unset or empty; the location rule is the subject"},
	{"internal/crwconfig/paths_test.go", "{\"TMPDIR empty\", rootsEnv(\"HOME\", home, \"TMPDIR\", \"\"), nil, true, \"/tmp/crw\", SourceDefault, \"\"},", "E7 expected default temp_root with TMPDIR unset or empty; the location rule is the subject"},
	{"internal/crwconfig/verifier_probe_test.go", "for _, place := range []string{\"/tmp\", \"/var/tmp\"} {", "E7 explicit former temporary-root exclusions under test: usable toolchains below both roots must be discovered"},
	{"internal/crwconfig/verifier_probe_test.go", "t.Setenv(\"TMPDIR\", \"/tmp\")", "E7 explicit former temporary-root exclusions under test: usable toolchains below both roots must be discovered"},
	{"internal/crwconfig/verifier_probe_test.go", "if !strings.HasPrefix(home, \"/tmp/\") {", "E7 explicit former temporary-root exclusions under test: usable toolchains below both roots must be discovered"},
	{"internal/dev/ci/local_parallel_test.go", "\"tmpdir\":   os.Getenv(\"TMPDIR\"),", "E7 the helper observes the private TMPDIR assigned by the verifier and asserts its initial emptiness"},
	{"internal/dev/ci/local_parallel_test.go", "entries, err := os.ReadDir(os.Getenv(\"TMPDIR\"))", "E7 the helper observes the private TMPDIR assigned by the verifier and asserts its initial emptiness"},
	{"internal/dev/ci/tmp_decisions_test.go", "// the one place. The check greps every tracked file for the direct deciders: os.TempDir(), a literal", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "{\"internal/tmploc/dir.go\", `const DefaultDir = \"/tmp\"`, tmpE1},", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "{\"internal/tmploc/dir.go\", `if v := getenv(\"TMPDIR\"); v != \"\" {`, tmpE1},", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "{\"internal/tmploc/dir.go\", `return os.Getenv(\"TMPDIR\")`, tmpE1},", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "{\"internal/tmploc/doc.go\", `// (\"/tmp\"), reported with source \"default\" (G-D1 = A, Q63ANS1722). The default is a location only:`, tmpE1},", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "{\"internal/tmploc/socket.go\", `const SocketRoot = \"/tmp\"`, tmpE1},", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "`payload = map[string]any{\"cwd\": \"/tmp/workspace\", \"hook_event_name\": \"Stop\", \"last_assistant_message\": \"I finished the task.\", \"model\": \"test-model\", \"permission_mode\": \"default\", \"session_id\": \"01a0b109-1ea5-7fb3-9adc-87f45ed83688\", \"stop_hook_active\": false, \"transcript_path\": \"/tmp/transcript.jsonl\", \"turn_id\": \"turn-1\"}`,", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_decisions_test.go", "`const RecordedScratch = \"/var/tmp\"`,", "E7 guard documentation and literal source text of the named E1/E4/E5 exceptions, not an executed location choice"},
	{"internal/dev/ci/tmp_shell_sites_test.go", "if got.code != 0 || !strings.HasPrefix(out, \"/tmp/codex-diagrams/\") {", "E7 expected shell default output paths when TMPDIR is unset; the root selection is under test"},
	{"internal/dev/ci/tmp_shell_sites_test.go", "if len(calls) == 0 || !strings.HasPrefix(calls[0], \"/tmp/scaffold-audit.\") {", "E7 expected shell default output paths when TMPDIR is unset; the root selection is under test"},
	{"internal/dev/cxccorpus/recorded_scratch_test.go", "// records (\"/var/tmp/cxc-rec-<16 hex>, the recorded 33 bytes\").", "E7 documentation of the frozen oracle root and its recorded 33-byte case path"},
	{"internal/dev/cxcfuzz/cases_test.go", "if info, err := os.Stat(\"/tmp\"); err != nil || !info.IsDir() {", "E7 explicitly compares a short default root with a deep root to verify case paths are independent of host placement"},
	{"internal/dev/cxcfuzz/cases_test.go", "t.Skip(\"/tmp is not a directory on this host\")", "E7 explicitly compares a short default root with a deep root to verify case paths are independent of host placement"},
	{"internal/dev/cxcfuzz/cases_test.go", "for _, row := range []struct{ name, base string }{{\"below-tmp\", \"/tmp\"}, {\"below-a-deep-directory\", deep}} {", "E7 explicitly compares a short default root with a deep root to verify case paths are independent of host placement"},
	{"internal/dev/cxcfuzz/homeguard_test.go", "// refused with tmploc's reason; os.MkdirTemp(\"\") would make the root below the working directory.", "E7 comment describing the old relative-TMPDIR behavior that the regression refuses"},
	{"internal/dev/laneparity/review_test.go", "\"version absolute\": {\"version\": \"/tmp\"},", "E7 an absolute version string is malformed input to the pinned-review configuration decoder"},
	{"internal/flow/cli/cilocal_integration_test.go", "if p, err := filepath.EvalSymlinks(os.Getenv(\"TMPDIR\")); err == nil {", "E7 observes the verifier's assigned TMPDIR and tests its unset/default case; it does not choose scratch for other tests"},
	{"internal/flow/cli/cilocal_integration_test.go", "{name: \"no TMPDIR: the default\", unset: true, parent: \"/tmp\"},", "E7 observes the verifier's assigned TMPDIR and tests its unset/default case; it does not choose scratch for other tests"},
	{"internal/flow/cli/cilocal_integration_test.go", "if err := os.Unsetenv(\"TMPDIR\"); err != nil {", "E7 observes the verifier's assigned TMPDIR and tests its unset/default case; it does not choose scratch for other tests"},
	{"internal/flow/cli/integrate_env_test.go", "if err := os.Unsetenv(\"TMPDIR\"); err != nil {", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/integrate_env_test.go", "if v, ok := os.LookupEnv(\"TMPDIR\"); (caller == \"B\" && v != b) || (caller == \"unset\" && ok) {", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_apply_test.go", "if err := os.Unsetenv(\"TMPDIR\"); err != nil {", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_apply_test.go", "if v, ok := os.LookupEnv(\"TMPDIR\"); !ok || v != a || os.TempDir() != a {", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_apply_test.go", "t.Errorf(\"TMPDIR set %v: applied it is %q (%v), os.TempDir %q, want %s\", set, v, ok, os.TempDir(), a)", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_apply_test.go", "v, ok := os.LookupEnv(\"TMPDIR\")", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_apply_test.go", "if v := os.Getenv(\"TMPDIR\"); v != b {", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/runenv_launch_test.go", "_ = os.Unsetenv(\"TMPDIR\")", "E7 direct environment observations and unset inputs test exact TMPDIR application, isolation and restoration"},
	{"internal/flow/cli/tempguard_test.go", "var tempDecision = regexp.MustCompile(`os\\.TempDir\\(\\)|\"/tmp|\"/var/tmp|\"/dev/shm|MkdirTemp\\(\"\"|CreateTemp\\(\"\"|\\(\"TMPDIR\"\\)|\"TMPDIR\", \"TMP\"`)", "E7 the architecture guard's regular expression is scanner input, not a filesystem location"},
	{"internal/flow/ops/finalgate_test.go", "return engine.FakeReply{Text: jsonOf(map[string]any{\"captures\": []any{map[string]any{\"path\": \"/tmp/qa/shot.png\", \"kind\": \"screenshot\", \"shows\": \"the home screen\"}},", "E7 synthetic screenshot evidence path and its expected packet spelling"},
	{"internal/flow/ops/finalgate_test.go", "if len(packets) != 2 || !strings.Contains(packets[0], \"/tmp/qa/shot.png\") || !strings.Contains(packets[1], \"/tmp/qa/shot.png\") {", "E7 synthetic screenshot evidence path and its expected packet spelling"},
	{"internal/flow/ops/schemas_test.go", "for _, region := range []string{\"../outside.go\", \"/tmp/outside.go\", \"..\", \"pkg/../../outside.go\"} {", "E7 absolute and escaping region paths are refusal inputs to the schema validator"},
	{"internal/flow/ops/tmploc_site_test.go", "// relative TMPDIR. os.MkdirTemp(\"\") would have made the directory below the working directory.", "E7 comment describing the relative-TMPDIR regression; no directory is chosen here"},
	{"internal/flow/scenario/lab_test.go", "return map[string]any{\"captures\": []any{map[string]any{\"path\": \"/tmp/qa/shot-1.png\", \"kind\": \"screenshot\", \"shows\": \"the home screen at 1280px\"}},", "E7 synthetic screenshot evidence returned by the scenario agent"},
	{"internal/gui/policy_api_test.go", "\"    \\\"legacy\\\": {\\\"role\\\": \\\"parent\\\", \\\"model\\\": \\\"devin/swe-2\\\", \\\"reasoningEffort\\\": \\\"max\\\", \\\"cwd\\\": [\\\"/tmp/project\\\"]}\\n\" +", "E7 policy document and mutation payload fixtures; cwd is the stored policy input"},
	{"internal/gui/policy_api_test.go", "\"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"extra\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"anthropic/opus\\\",\\\"reasoningEffort\\\":\\\"xhigh\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\",", "E7 policy document and mutation payload fixtures; cwd is the stored policy input"},
	{"internal/gui/policy_api_test.go", "const recordBackup = \"/tmp/does-not-need-to-exist/crw-bridge-mcp.json.crw-20261007T000000Z.bak\"", "E7 a recorded installer backup path displayed from a receipt; the test explicitly needs no file at that path"},
	{"internal/gui/policy_fixes_test.go", "payload := \"{\\\"expectedDigest\\\":\\\"\" + digestOf(policyWritableText) + \"\\\",\\\"change\\\":{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"extra\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"anthropic/opus\\\",\\\"effort\\\":\\\"xhigh\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}}\"", "E7 policy document and mutation payload fixtures; cwd is the stored policy input"},
	{"internal/gui/policy_fixes_test.go", "payload := \"{\\\"expectedDigest\\\":\\\"\" + digestOf(policyWritableText) + \"\\\",\\\"change\\\":{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"extra\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"anthropic/opus\\\",\\\"reasoningEffort\\\":\\\"xhigh\\\",\\\"effort\\\":\\\"max\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}}\"", "E7 policy document and mutation payload fixtures; cwd is the stored policy input"},
	{"internal/gui/status_api_test.go", "fakeRelayDocument = `{\"schema\":\"crw-relay-read/1\",\"stateDir\":\"/tmp/relay\",\"readAt\":\"2026-10-07T00:00:00Z\",` +", "E7 synthetic relay-read and execution-policy records used as UI input"},
	{"internal/gui/status_api_test.go", "State: statusOK, PolicyState: \"registered\", Path: \"/tmp/execution-policy.json\",", "E7 synthetic relay-read and execution-policy records used as UI input"},
	{"internal/harness/hook_test.go", "for args, want := range map[string]bool{\"stop\": true, \"session-start --leg x\": true, \"--plugin-launch\": false, \"/tmp/settings.json\": false, \"nope\": false, \"\": false} {", "E7 a settings pathname is deliberately invalid hook argv in the parser table"},
	{"internal/harness/payload_test.go", "got, ok := ParseSessionStart(doc(t, obj{\"hook_event_name\": \"SessionStart\", \"session_id\": \"session-1\", \"cwd\": \"/tmp/workspace\"}))", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "if want := (SessionStart{SessionID: \"session-1\", Cwd: \"/tmp/workspace\"}); !ok || got != want {", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "doc(t, obj{\"session_id\": \"s1\", \"cwd\": \"/tmp/x\"}),", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "doc(t, obj{\"hook_event_name\": \"Stop\", \"session_id\": \"s1\", \"cwd\": \"/tmp/x\"}),", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "doc(t, obj{\"hook_event_name\": \"SessionStart\", \"cwd\": \"/tmp/x\"}),", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "{\"session_id\": \"\", \"cwd\": \"/tmp/x\"},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "{\"session_id\": \" \\t\\n\", \"cwd\": \"/tmp/x\"},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "{\"session_id\": \"  session-1  \", \"cwd\": \"/tmp/x\"},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "{\"session_id\": \"../session-1\", \"cwd\": \"/tmp/x\"},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "{\"session_id\": \"세션-1\", \"cwd\": \"/tmp/x\"},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "root := obj{\"hook_event_name\": \"UserPromptSubmit\", \"session_id\": \"s1\", \"cwd\": \"/tmp/x\", \"prompt\": \"interview me\", \"turn_id\": \"t1\"}", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/harness/payload_test.go", "\"SubagentStop is detected too\": {doc(t, obj{\"hook_event_name\": \"SubagentStop\", \"session_id\": \"s1\", \"cwd\": \"/tmp/x\", \"agent_type\": \"worker\", \"agent_id\": \"a-1\"}), true},", "E7 hook payload cwd strings and decoded expectations; no filesystem operation uses these fixture paths"},
	{"internal/manage/audit_test.go", "roots := []string{\"/home/\", \"/state/\", \"/scratch/\", \"/var/tmp\"}", "E7 private host path spellings that the published audit must not contain"},
	{"internal/manage/childresume_test.go", "\"  *doctor*) printf '%s\\\\n' '{\\\"stateSelection\\\":{\\\"path\\\":\\\"/tmp/relay-store\\\"}}' ;;\\n\" +", "E7 the fake relay's doctor reply and configured state spelling; calls are recorded by the fake command"},
	{"internal/manage/childresume_test.go", "cfg.Relay.State = \"/tmp/relay-store\"", "E7 the fake relay's doctor reply and configured state spelling; calls are recorded by the fake command"},
	{"internal/manage/core_tmpfs_cases_test.go", "if n := len(\"/dev/shm/\") + len(tmpfsRootPrefix) + len(strconv.Itoa(1<<22)); n > 31 {", "E7 asserts the maximum length of the tmpfs fixture root; the path geometry is the test subject"},
	{"internal/manage/core_tmpfs_cases_test.go", "record(role + \" \" + os.Getenv(\"TMPDIR\"))", "E7 the helper records the TMPDIR it inherited to test tmpfs-root ownership and reuse"},
	{"internal/manage/pump_signals_plan_test.go", "{\"/tmp/s6r dag-accept --plan p1\", nil},", "E7 a command transcript fixture for plan signal recognition, not an executed program path"},
	{"internal/manage/relayread_test.go", "f.relayReadBinding(\"bnd-1\", \"parent\", \"project\", \"project-1\", \"parent-1\", \"host-1\", \"/tmp/work\", \"active\", 1)", "E7 recorded binding workspace paths in synthetic relay-read responses"},
	{"internal/manage/relayread_test.go", "f.relayReadBinding(\"bnd-\"+project, \"parent\", \"project\", project, \"parent-1\", \"host-1\", \"/tmp/work\", \"active\", 1)", "E7 recorded binding workspace paths in synthetic relay-read responses"},
	{"internal/manage/relayread_test.go", "f.relayReadBinding(\"bnd-superseded\", \"parent\", \"project\", \"project-1\", \"parent-old\", \"host-1\", \"/tmp/old\", \"superseded\", 1)", "E7 recorded binding workspace paths in synthetic relay-read responses"},
	{"internal/pabcd/hook/githubpost_scriptplace_test.go", "{[]string{\"diff\", \"--no-index\", \"--src-prefix\", \"--\", \"--output=/tmp/b.md\", \"/dev/null\", \"/tmp/s.txt\"}, false},", "E7 git argv path operands used to test script/output-option recognition"},
	{"internal/pabcd/hook/githubpost_scriptplace_test.go", "{[]string{\"diff\", \"--src-prefix\", \"--\", \"--output\", \"/tmp/b.md\"}, false},", "E7 git argv path operands used to test script/output-option recognition"},
	{"internal/pabcd/hook/githubpost_test.go", "for _, tmp := range []struct{ name, value string }{{\"TMPDIR=/tmp\", \"/tmp\"}, {\"TMPDIR unset\", \"\"}, {\"TMPDIR=t.TempDir()\", t.TempDir()}} {", "E7 explicit default, unset and custom TMPDIR cases test that readable body files are accepted regardless of placement"},
	{"internal/pabcd/hook/githubpost_test.go", "if err := os.Unsetenv(\"TMPDIR\"); err != nil {", "E7 explicit default, unset and custom TMPDIR cases test that readable body files are accepted regardless of placement"},
	{"internal/pabcd/hook/goalgate_test.go", "raw := goalGateTestPayload(t, \"/tmp/x\", \"s1\", \"create_goal\", map[string]any{\"objective\": \"y\", \"token_budget\": 9})", "E7 goal-gate payload cwd and decoded expectations, not scratch allocation"},
	{"internal/pabcd/hook/goalgate_test.go", "if !ok || p.ToolName != \"create_goal\" || p.SessionID != \"s1\" || p.Cwd != \"/tmp/x\" {", "E7 goal-gate payload cwd and decoded expectations, not scratch allocation"},
	{"internal/pabcd/hook/goalgate_test.go", "goalGateTestPayload(t, \"/tmp/x\", \"s1\", \"request_user_input\", map[string]any{}): true,", "E7 goal-gate payload cwd and decoded expectations, not scratch allocation"},
	{"internal/pabcd/hook/goalgate_test.go", "goalGateTestPayload(t, \"/tmp/x\", \"s1\", \"shell\", map[string]any{}):              false,", "E7 goal-gate payload cwd and decoded expectations, not scratch allocation"},
	{"internal/pabcd/hook/leaf_recovery_layer_test.go", "text := memoryGateReasonFor(MemoryWriteAttempt{Surface: \"tool\", Cause: \"authorization-state\", Target: \"note.md\"}, \"s1\", \"/tmp\", leaf)", "E7 synthetic cwd in the rendered memory-gate refusal reason"},
	{"internal/pabcd/hook/shellwrite_brace_test.go", "\"{cp,\\\"/tmp/x\\\"} \" + root + \"/a\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_escape_test.go", "{command: path(\"Path('/tmp/b', '/m/a').write_text('x')\"), has: []string{\"/m/a\"}, lacks: []string{\"/tmp/b\"}},", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_escape_test.go", "{command: path(\"Path('/m/a', '/tmp/b',).write_text('x')\"), has: []string{\"/tmp/b\"}, lacks: []string{\"/m/a\"}},", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_escape_test.go", "{command: `python3 -c 'from pathlib import Path; Path(\"/tmp\", \"\"\"review\"\"\", \"memories\", \"a\").write_text(\"x\")'`, has: []string{\"/tmp/review/memories/a\"}},", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_runner_test.go", "\"printf '%s' \" + \"/tmp/x\" + \" | entr cp /tmp/x\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_structural_test.go", "\"python3 -c 'import shutil; getattr(shutil,\\\"co\\\"+\\\"py\\\")(\\\"/tmp/x\\\",\\\"\" + root + \"/a\\\")'\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_structural_test.go", "\"python3 -c 'import shutil; c=shutil.copy; c(\\\"/tmp/x\\\",\\\"\" + root + \"/a\\\")'\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_structural_test.go", "\"python3 -c 'import shutil; shutil.copy(\\\"/tmp/x\\\",\\\"\" + root + \"/a\\\")'\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_structural_test.go", "\"python3 -c 'from shutil import copy; copy(\\\"/tmp/x\\\",\\\"\" + root + \"/a\\\")'\",", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_triple_test.go", "{command: py(`open(r\"\"\"/tmp/a` + \"\\r\\n\" + `b\"\"\",\"w\")`), has: []string{\"/tmp/a\\nb\"}, lacks: []string{\"/tmp/a\\r\\nb\"}},", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/shellwrite_triple_test.go", "{command: py(`open(r\"\"\"/tmp/a` + \"\\r\" + `/n.md\"\"\",\"w\")`), has: []string{\"/tmp/a\\n/n.md\"}, lacks: []string{\"/tmp/a\\r/n.md\"}},", "E7 shell/program text analyzed as data, with expected path tokens; these commands are not executed"},
	{"internal/pabcd/hook/tempbase_test.go", "\"/tmp/TestReproductionRows/1\": \"\",", "E7 path-token examples that the lexical fixture-path guard accepts"},
	{"internal/pabcd/hook/tempbase_test.go", "\"/var/tmp\":                    \"\",", "E7 path-token examples that the lexical fixture-path guard accepts"},
	{"internal/pabcd/hook/tempbase_test.go", "if got := os.Getenv(\"TMPDIR\"); got != base {", "E7 observes TMPDIR after neutral-base selection; the environment choice is the assertion"},
	{"internal/pabcd/hook/tempbase_test.go", "got := os.Getenv(\"TMPDIR\")", "E7 observes TMPDIR after neutral-base selection; the environment choice is the assertion"},
	{"internal/pabcd/hook/tempbase_test.go", "if got := os.Getenv(\"TMPDIR\"); got == link {", "E7 observes TMPDIR after neutral-base selection; the environment choice is the assertion"},
	{"internal/pabcd/hook/worktree_legs_test.go", "if got := start.Handle(payload(map[string]any{\"hook_event_name\": \"SessionStart\", \"cwd\": \"/tmp\"})); got != \"\" {", "E7 outside-worktree cwd inputs test that hooks do not treat unrelated paths as managed worktrees"},
	{"internal/pabcd/hook/worktree_test.go", "for _, cwd := range []string{r.worktrees, r.home, \"/tmp\", \"\", \"   \"} {", "E7 outside-worktree cwd inputs test that hooks do not treat unrelated paths as managed worktrees"},
	{"internal/pabcd/hook/worktree_test.go", "if _, ctx := HandleWorktreeGuard(wtPayload(t, map[string]any{\"hook_event_name\": \"SessionStart\", \"session_id\": \"s2\", \"cwd\": \"/tmp\"}), r.env()); ctx != \"\" {", "E7 outside-worktree cwd inputs test that hooks do not treat unrelated paths as managed worktrees"},
	{"internal/pabcd/hook/worktree_test.go", "if _, ctx := r.prompt(t, \"/tmp\", \"s-quiet\", \"rename the worktree\"); ctx != \"\" {", "E7 outside-worktree cwd inputs test that hooks do not treat unrelated paths as managed worktrees"},
	{"internal/pabcd/hook/worktreedel_crw1165_first_attempt_test.go", "\"/tmp/crw1165-tools/cd \"+o+\" || exit; find . -delete\",", "E7 shell command spellings analyzed as data: path-named programs must not be mistaken for shell builtins"},
	{"internal/pabcd/hook/worktreedel_crw1165_first_attempt_test.go", "\"/tmp/crw1165-tools/set -e; cd \"+missing+\"; find . -delete\",", "E7 shell command spellings analyzed as data: path-named programs must not be mistaken for shell builtins"},
	{"internal/pabcd/hook/worktreedel_crw1165_first_attempt_test.go", "\"/tmp/crw1165-tools/cd \" + home + \" || exit; cd \" + root + \" && printf x > MEMORY.md\",", "E7 shell command spellings analyzed as data: path-named programs must not be mistaken for shell builtins"},
	{"internal/pabcd/host/session_test.go", "{\"stored cwd with a NUL\", map[string]any{\"cwd\": \"/tmp\\x00x\"}, msgCwd}, {\"null cwd\", map[string]any{\"cwd\": nil}, msgRelative}, {\"BLOB cwd\", map[string]any{\"cwd\": []byte(\"/tmp\")}, msgRelative},", "E7 malformed stored cwd values (NUL and BLOB) are decoder inputs"},
	{"internal/pabcd/reset/reset_linkperm_test.go", "{\"absolute\", filepath.Join(\"/tmp\", \"crw-927-outside.json\")},", "E7 an absolute outside path is a link-permission fixture input"},
	{"internal/pabcd/shellir/crw1058_post_test.go", "wantExecs(t, \"parallel echo {.} ::: /tmp/wt/slot.v1/repo\", \"echo\", []string{\"/tmp/wt/slot.v1/repo\"})", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/crw1058_post_test.go", "wantExecs(t, \"parallel echo {.} ::: /tmp/wt/slot.v1/repo.txt\", \"echo\", []string{\"/tmp/wt/slot.v1/repo\"})", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/crw1058_test.go", "wantExecs(t, \"parallel rm -rf {} ::: /tmp/wt/slot/repo\", \"rm\", []string{\"-rf\", \"/tmp/wt/slot/repo\"})", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/crw910_eval_test.go", "if ls.Dir.Path != \"/tmp\" || !ls.Dir.Known {", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/grader_test.go", "r, err := Analyze(\"while false; do cd /tmp; done; rm -rf ../repo\", \"/tmp/wt/slot/repo\")", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/grader_test.go", "r, err := Analyze(\"bash -c 'CMD=\\\"rm -rf ../repo\\\"; $CMD'\", \"/tmp/wt/slot/repo\")", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/grader_test.go", "if _, err := Analyze(cmd, \"/tmp/wt/slot/repo\"); !isUnreadable(err) {", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/shellir_test.go", "if got := r.Execs[len(r.Execs)-1].Dir; got.Path != \"/tmp\" || !got.Known {", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/shellir_test.go", "if len(r.Execs) != 2 || r.Execs[1].Dir.Path != \"/tmp\" || !r.Execs[1].Dir.Known {", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/shellir/stdin_alias_test.go", "{\"/tmp/dev/stdin\", known, false},", "E7 shell-IR analysis inputs and expected directory/argument values; the parsed commands are never executed"},
	{"internal/pabcd/state/corpus_test.go", "bind := strings.NewReplacer(\"$\"+\"{WS}\", \"/ws\", \"$\"+\"{TMP}\", \"/tmp/t\", \"$\"+\"{HOME}\", \"/h\", \"$\"+\"{CODEX_HOME}\", \"/codex\", \"$\"+\"{CXC_HOME}\", \"/cxc\")", "E7 recorded state-corpus cwd used to replay the frozen fixture"},
	{"internal/policystore/applied_test.go", "return &service.ScopeRegistry{Root: \"/tmp/scope\", Authority: \"isolated\"}, nil", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/applied_test.go", "return store.StateSelection{Path: \"/tmp/state\"}, nil", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/applied_test.go", "env := envOf(map[string]string{\"HOME\": \"/tmp/home\", \"CODEX_HOME\": \"/tmp/home/.codex\"})", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/applied_test.go", "return manageRelay{Socket: \"/tmp/app.sock\", State: \"/srv/relay\"}, nil", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "raw := \"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"effort\\\":\\\"high\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\"", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "raw := \"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"reasoningEffort\\\":\\\"high\\\",\\\"effort\\\":\\\"low\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\"", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "raw := \"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"reasoningEffort\\\":\\\"high\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\"", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "\"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"reasoningEffort\\\":\\\"high\\\",\\\"effort\\\":\\\"\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\",", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "\"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"reasoningEffort\\\":\\\"\\\",\\\"effort\\\":\\\"high\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\",", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "\"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"effort\\\":\\\"high\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\",", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/check_test.go", "\"{\\\"kind\\\":\\\"setException\\\",\\\"id\\\":\\\"legacy\\\",\\\"role\\\":\\\"child\\\",\\\"model\\\":\\\"m\\\",\\\"reasoningEffort\\\":\\\"high\\\",\\\"cwd\\\":[\\\"/tmp/project\\\"]}\",", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/policy_test.go", "\"    \\\"legacy\\\": {\\\"role\\\": \\\"parent\\\", \\\"model\\\": \\\"devin/swe-2\\\", \\\"reasoningEffort\\\": \\\"max\\\", \\\"cwd\\\": [\\\"/tmp/project\\\"]}\\n\" +", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/review_test.go", "\"    \\\"legacy\\\": {\\\"role\\\": \\\"child\\\", \\\"model\\\": \\\"devin/swe-2\\\", \\\"reasoningEffort\\\": \\\"max\\\", \\\"cwd\\\": [\\\"/tmp/project\\\"], \\\"reason\\\": \\\"temporary vendor migration\\\"}\\n\" +", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/review_test.go", "change := Change{Kind: KindSetException, ID: \"legacy\", Role: \"child\", Model: \"devin/swe-2\", Effort: \"high\", CWD: []string{\"/tmp/project\"}}", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/policystore/running_test.go", "return &service.ScopeRegistry{Root: \"/tmp/scope\", Authority: \"isolated\"}, nil", "E7 stubbed running-policy readers and configuration inputs; stubRunningSeams prevents opening a real relay store"},
	{"internal/policystore/running_test.go", "return store.StateSelection{Path: \"/tmp/state\"}, nil", "E7 stubbed running-policy readers and configuration inputs; stubRunningSeams prevents opening a real relay store"},
	{"internal/policystore/running_test.go", "{\"a relay naming no socket\", \"{\\\"manage\\\":{\\\"relay\\\":{\\\"state\\\":\\\"/tmp/relay\\\"}}}\\n\"},", "E7 stubbed running-policy readers and configuration inputs; stubRunningSeams prevents opening a real relay store"},
	{"internal/policystore/running_test.go", "env := envOf(map[string]string{\"HOME\": \"/tmp/home\", \"CODEX_HOME\": \"/tmp/home/.codex\"})", "E7 stubbed running-policy readers and configuration inputs; stubRunningSeams prevents opening a real relay store"},
	{"internal/policystore/wire_test.go", "change := Change{Kind: KindSetException, ID: \"legacy\", Model: \"devin/swe-2\", Effort: \"high\", CWD: []string{\"/tmp/project\"}}", "E7 execution-policy document/change fixtures; cwd is policy data, not a selected test directory"},
	{"internal/publishpolicy/scrub_test.go", "root := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), \"ci-private\", \"job-1549\")", "E7 tests that a configured temporary root outside conventional private roots is scrubbed, including its environment spelling"},
	{"internal/publishpolicy/scrub_test.go", "for _, key := range []string{\"TMPDIR\", \"TMP\", \"TEMP\"} {", "E7 tests that a configured temporary root outside conventional private roots is scrubbed, including its environment spelling"},
	{"internal/publishpolicy/scrub_test.go", "if os.TempDir() != root {", "E7 tests that a configured temporary root outside conventional private roots is scrubbed, including its environment spelling"},
	{"internal/publishpolicy/scrub_test.go", "t.Fatalf(\"temporary root = %q, want %q\", os.TempDir(), root)", "E7 tests that a configured temporary root outside conventional private roots is scrubbed, including its environment spelling"},
	{"internal/relay/cli/fixture_support_test.go", "// root and each temporary directory directly under os.TempDir() or the fixed root - as a", "E7 documentation of how recorded golden placeholders identify run-specific paths"},
	{"internal/relay/cli/fixture_support_test.go", "const fixedRoot = \"/tmp/crw-cli-parity\"", "E7 frozen parity fixture root: absolute paths participate in artifact revisions and scope hashes; each tree is locked"},
	{"internal/relay/cli/supervisor_test.go", "}{{[]string{\"--state\", state, \"supervisor-stage\", \"--event\", \"abc\", \"--observation\", \"a.json\"}, 4, \"--observation\"}, {[]string{\"--state\", state, \"supervisor-stage\", \"--project\", \"PRJ-1\", \"--recipient\", \"01someone\"}, 4, \"--recipient\"}, {[]string{\"--state\", state, \"supervisor-stage\", \"--project\", \"PRJ-1\", \"--observation\", \"a.json\"}, 4, \"observation\"}, {[]string{\"--state\", state, \"supervisor-send\", \"--message\", \"m\", \"--socket\", \"/tmp/s\"}, 2, \"unrecognized arguments: --socket\"}}", "E7 rejected supervisor argv fixture; the socket option is unrecognized before any connection"},
	{"internal/relay/dagsched/conflicts_test.go", "// sequential: t.Setenv(\"TMPDIR\") is process-wide.", "E7 comment explaining why tests that set TMPDIR must run sequentially"},
	{"internal/relay/dagsched/forgekit_test.go", "for _, forge := range []string{\" owner/repo \", \"owner/repo\\n\", \"repo\", \"/tmp/owner/repo\", \"/repo\", \"owner/\", \"a/b/c\", \"\"} {", "E7 absolute path spelling is invalid forge repository input"},
	{"internal/relay/dagsched/release_limits_test.go", "return fmt.Sprintf(`{\"source\": \"file:%d\", \"snapshot_uri\": \"/tmp/x/%d.md\", \"sha256\": \"abc\", \"captured_at\": \"2026-10-02T00:00:00Z\"}`, i, i)", "E7 synthetic manifest snapshot/artifact-root records and decoded expectations; these paths are not allocated"},
	{"internal/relay/dagsched/release_limits_test.go", "list(roots, func(i int) string { return fmt.Sprintf(`\"/tmp/x/%d\"`, i) }),", "E7 synthetic manifest snapshot/artifact-root records and decoded expectations; these paths are not allocated"},
	{"internal/relay/dagsched/release_manifest_test.go", "\"volatile\": [{\"source\": \"linear:doc\", \"snapshot_uri\": \"/tmp/x/doc.md\", \"sha256\": \"abc\", \"captured_at\": \"2026-10-02T00:00:00Z\"}],", "E7 synthetic manifest snapshot/artifact-root records and decoded expectations; these paths are not allocated"},
	{"internal/relay/dagsched/release_manifest_test.go", "\"artifact_roots\": [\"/tmp/x\"], \"allowed_recipients\": [\"parent\"],", "E7 synthetic manifest snapshot/artifact-root records and decoded expectations; these paths are not allocated"},
	{"internal/relay/dagsched/release_manifest_test.go", "len(req.Volatile) != 1 || req.Volatile[0].SnapshotURI != \"/tmp/x/doc.md\" || req.Volatile[0].CapturedAt != \"2026-10-02T00:00:00Z\" || req.Child.Title != \"Child\" || req.ArtifactRoots[0] != \"/tmp/x\" {", "E7 synthetic manifest snapshot/artifact-root records and decoded expectations; these paths are not allocated"},
	{"internal/relay/delivery/paritytree_test.go", "const parityRoot = \"/tmp/crw-delivery-parity\"", "E7 frozen parity fixture root: request/workspace/evidence digests include absolute paths; trees are held under locks"},
	{"internal/relay/hook/fixture_tree_test.go", "const canonicalFixtureRoot = \"/tmp/crw-oracle\"", "E7 frozen canonical fixture root: manifest and event digests require the recorded absolute path, held under an exclusive lock"},
	{"internal/relay/hook/journal_row_test.go", "for _, path := range []string{\"/tmp/control.sock\", \"/tmp/quote'and\\\"/control.sock\", \"/tmp/new\\nline/control.sock\", \"/tmp/한글/control.sock\"} {", "E7 socket/configuration path byte spellings are journal decoder inputs, including invalid UTF-8 and surrogate cases"},
	{"internal/relay/hook/journal_row_test.go", "{\"a state directory holding 0xff\", socket(store.PathRepr(\"/tmp/st\\xffate/control.sock\")), true},", "E7 socket/configuration path byte spellings are journal decoder inputs, including invalid UTF-8 and surrogate cases"},
	{"internal/relay/hook/journal_row_test.go", "{\"a state directory holding the bytes ED A0 80\", socket(store.PathRepr(\"/tmp/st\\xed\\xa0\\x80ate/control.sock\")), true},", "E7 socket/configuration path byte spellings are journal decoder inputs, including invalid UTF-8 and surrogate cases"},
	{"internal/relay/hook/journal_row_test.go", "{\"settings under a Codex home holding 0xff\", configuration(\"/tmp/c\\xed\\xb3\\xbf/\" + ConfigName), true},", "E7 socket/configuration path byte spellings are journal decoder inputs, including invalid UTF-8 and surrogate cases"},
	{"internal/relay/hook/journal_row_test.go", "{\"settings under a surrogate that is no byte\", configuration(\"/tmp/c\\xed\\xa0\\x80/\" + ConfigName), false},", "E7 socket/configuration path byte spellings are journal decoder inputs, including invalid UTF-8 and surrogate cases"},
	{"internal/relay/job/spawn_test.go", "file, args, err := BuildShell([]string{\"echo\", \"hi there\"}, \"/tmp/o\", \"/tmp/e\", \"\")", "E7 BuildShell argv/redirection fixture; constructing the command does not execute it"},
	{"internal/relay/managed/capture_test.go", "id := Identity{IssueKey: \"REL-1\", Fingerprint: \"fp-1\", Version: Schema, Workspace: \"/tmp/work\", MarkerRoot: \"/tmp/markers\", SocketIdentity: \"/tmp/socket\", CreateRequestID: \"create-1\", DispatchRequestID: \"business-1\"}", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "id := Identity{RequestID: \"req-1\", IssueKey: \"REL-1\", Fingerprint: \"fp-1\", Version: Schema, Workspace: \"/tmp/work\", MarkerRoot: \"/tmp/markers\", SocketIdentity: \"/tmp/socket\", CreateRequestID: \"create-1\", DispatchRequestID: \"business-1\"}", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "relation, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: \"parent\", HostID: \"host\", Cwd: sql.NullString{String: \"/parent\", Valid: true}}, Child: registry.Endpoint{TaskID: \"child\", HostID: \"host\", Cwd: sql.NullString{String: \"/tmp/work\", Valid: true}}, IssueKey: \"REL-1\", ArtifactRoots: []string{\"/tmp/work\"}, AllowedRecipients: []string{\"parent\"}, DispatchRequestID: \"live\"})", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "_, e = reg.Resume(ctx, relation.ID, 1, []string{\"/tmp/work\"}, []string{\"parent\"}, \"operator\")", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "_, e := s.DB.ExecContext(ctx, `INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,'reserved',0,?,?)`, \"req-direct\", \"REL-1\", \"fp-1\", Schema, \"/tmp/work\", \"/tmp/markers\", \"/tmp/socket\", \"create-x\", \"business-x\", delivery.NewFakeClock().ISO(), delivery.NewFakeClock().ISO())", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "_, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: \"parent\", HostID: \"host\", Cwd: sql.NullString{String: \"/parent\", Valid: true}}, Child: registry.Endpoint{TaskID: \"other\", HostID: \"host\", Cwd: sql.NullString{String: \"/tmp/work\", Valid: true}}, IssueKey: \"REL-1\", ArtifactRoots: []string{\"/tmp/work\"}, AllowedRecipients: []string{\"parent\"}, DispatchRequestID: \"raw\"})", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "_, e = reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: \"parent\", HostID: \"host\", Cwd: sql.NullString{String: \"/parent\", Valid: true}}, Child: registry.Endpoint{TaskID: \"other\", HostID: \"host\", Cwd: sql.NullString{String: \"/tmp/work\", Valid: true}}, IssueKey: \"REL-1\", ArtifactRoots: []string{\"/tmp/work\"}, AllowedRecipients: []string{\"parent\"}, DispatchRequestID: \"business-1\", DispatchTurnID: sql.NullString{String: \"standby\", Valid: true}, ManagedRequestID: \"req-1\"})", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/capture_test.go", "relation, e := reg.Register(ctx, registry.Registration{Parent: registry.Endpoint{TaskID: \"parent\", HostID: \"host\", Cwd: sql.NullString{String: \"/parent\", Valid: true}}, Child: registry.Endpoint{TaskID: \"child\", HostID: \"host\", Cwd: sql.NullString{String: \"/tmp/work\", Valid: true}}, IssueKey: \"REL-1\", ArtifactRoots: []string{\"/tmp/work\"}, AllowedRecipients: []string{\"parent\"}, DispatchRequestID: \"business-1\", DispatchTurnID: sql.NullString{String: \"standby\", Valid: true}, ManagedRequestID: \"req-1\"})", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/reconcile_orphan_test.go", "{\"a verified resume\", func(r map[string]any) { r[\"resumed\"] = map[string]any{\"cwd\": \"/tmp\"} }},", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/reconcile_part2_test.go", "{\"settings contain missing-thread text\", hostRefusal(\"thread/resume: settings differ: cwd '/tmp/thread not found'\", \"notLoaded\", map[string]any{\"resumed\": map[string]any{\"cwd\": \"/tmp/thread not found\"}}), false},", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/managed/reservation_test.go", "return Reservation{Store: s, Now: func() string { return \"2026-09-26T00:00:00.000000+00:00\" }}, Identity{RequestID: \"req-1\", IssueKey: \"REL-1\", Fingerprint: \"fp-1\", Version: Schema, Workspace: \"/tmp/work\", MarkerRoot: \"/tmp/markers\", SocketIdentity: \"/tmp/socket\", CreateRequestID: \"create-1\", DispatchRequestID: \"business-1\"}", "E7 managed identity, registration and observation records; path fields are synthetic stored data"},
	{"internal/relay/registry/registry_test.go", "_, err = r.Register(ctx, Registration{Parent: Endpoint{TaskID: \"parent\", HostID: \"host\"}, Child: Endpoint{TaskID: \"child\", HostID: \"host\"}, IssueKey: \"issue\", ArtifactRoots: []string{\"/tmp\"}, AllowedRecipients: []string{\"parent\"}, DispatchRequestID: \"dispatch\"})", "E7 artifact-root value in a synthetic registration record"},
	{"internal/relay/service/scope_root_test.go", "socket := \"/var/tmp/crw-scope-root/app.sock\"", "E7 fixed socket identity input whose normalized scope root and salted key are compared with the golden"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/plain\", `'/tmp/plain'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/a\\u00a0b\", `'/tmp/a\\xa0b'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/a\\u2028b\", `'/tmp/a\\u2028b'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/a\\xffb\", `'/tmp/a\\udcffb'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/a\\xed\\xa0\\x80b\", `'/tmp/a\\udced\\udca0\\udc80b'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/it's\", `\"/tmp/it's\"`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/it's \\\"q\\\"\", `'/tmp/it\\'s \"q\"'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/t\\tn\\nr\\r\\\\\", `'/tmp/t\\tn\\nr\\r\\\\'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\x01\\x7f\", `'/tmp/\\x01\\x7f'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\xe2\\x82\", `'/tmp/\\udce2\\udc82'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\ufffd\", \"'/tmp/\\ufffd'\"},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\U0001f600\", \"'/tmp/\\U0001f600'\"},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\u200b\", `'/tmp/\\u200b'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\U000e0001\", `'/tmp/\\U000e0001'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\ue000\", `'/tmp/\\ue000'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\u00e9\\u3042\", \"'/tmp/\\u00e9\\u3042'\"},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\u0378\", `'/tmp/\\u0378'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/st\\u0c5cate\", `'/tmp/st\\u0c5cate'`}, // unassigned in CPython 3.14's Unicode 16.0.0", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "{\"/tmp/\\u1680x\", `'/tmp/\\u1680x'`},", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "err := &os.PathError{Op: \"stat\", Path: \"/tmp/a\\u00a0\\xffb\", Err: syscall.EACCES}", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/store/pathrepr_test.go", "link := &os.LinkError{Op: \"rename\", Old: \"/tmp/\\u2028\", New: \"/tmp/\\xff\", Err: syscall.ENOENT}", "E7 path representation and synthetic PathError/LinkError byte fixtures; the exact path spelling is the subject"},
	{"internal/relay/supervisor/batch7_test.go", "base := map[string]any{\"schema\": \"reporting-observation/1\", \"reportingState\": \"unreported\", \"relationshipId\": \"rel-1\", \"executionGeneration\": float64(1), \"reason\": \"missing\", \"selectors\": map[string]any{\"state\": f.c.StoreDirectory(), \"markerRoot\": \"/tmp/markers\", \"workspace\": \"/tmp/work\", \"assignment\": \"a\", \"session\": \"s\", \"turn\": \"turn-1\"}}", "E7 reporting-observation selector records used as supervisor input"},
	{"internal/relay/supervisor/batch7_test.go", "return map[string]any{\"schema\": \"reporting-observation/1\", \"reportingState\": \"unreported\", \"relationshipId\": \"rel-1\", \"executionGeneration\": float64(1), \"reason\": \"missing\", \"selectors\": map[string]any{\"state\": f.c.StoreDirectory(), \"markerRoot\": \"/tmp/markers\", \"workspace\": \"/tmp/My Project\", \"assignment\": \"it's here\", \"session\": \"\\\"quoted\\\"\", \"turn\": \"turn-1\"}}", "E7 reporting-observation selector records used as supervisor input"},
	{"internal/relay/supervisor/eventless_guard_test.go", "f.c.Socket = \"/tmp/host.sock\"", "E7 recorded socket identity in supervisor fixtures; delivery uses an injected fake host rather than binding this path"},
	{"internal/relay/supervisor/layer_words_test.go", "f.c.Socket = \"/tmp/host.sock\"", "E7 recorded socket identity in supervisor fixtures; delivery uses an injected fake host rather than binding this path"},
	{"internal/relay/supervisor/partd_res_rcf_slf_test.go", "}{{\"networkAccess\", 0}, {\"networkAccess\", 1}, {\"networkAccess\", []any{1}}, {\"networkAccess\", \"false\"}, {\"networkAccess\", nil}, {\"excludeTmpdirEnvVar\", 0}, {\"excludeSlashTmp\", 1}, {\"writableRoots\", \"/tmp\"}, {\"writableRoots\", []any{7}}}", "E7 a scalar pathname is deliberately malformed writableRoots input"},
	{"internal/relay/supervisor/sch_57_74_capture_test.go", "reading := map[string]any{\"schema\": \"reporting-observation/1\", \"reportingState\": \"unreported\", \"relationshipId\": captureRelationID(t, c), \"executionGeneration\": float64(1), \"reason\": \"missing\", \"selectors\": map[string]any{\"state\": filepath.Join(c.StoreDirectory(), \"another-store\"), \"markerRoot\": \"/tmp/markers\", \"workspace\": \"/tmp/work\", \"assignment\": \"a\", \"session\": \"s\", \"turn\": \"turn-1\"}}", "E7 reporting-observation selector records used as supervisor input"},
	{"internal/relay/supervisor/sch_set1_capture_test.go", "_, parsed, _ := set1Command(t, b, \"--socket\", \"/tmp/s\", \"supervisor-send\", \"--message\", \"m\")", "E7 unknown socket-option argv and the expected parser residue, not a socket allocation"},
	{"internal/relay/supervisor/sch_set1_capture_test.go", "after, _, text := set1Command(t, b, \"supervisor-send\", \"--message\", \"m\", \"--socket\", \"/tmp/s\")", "E7 unknown socket-option argv and the expected parser residue, not a socket allocation"},
	{"internal/relay/supervisor/sch_set1_capture_test.go", "return []any{\"/tmp/s\"}", "E7 unknown socket-option argv and the expected parser residue, not a socket allocation"},
	{"internal/relay/supervisor/sch_set2_capture_test.go", "reading := map[string]any{\"schema\": \"reporting-observation/1\", \"reportingState\": \"unreported\", \"relationshipId\": captureRelationID(t, c), \"reason\": \"the turn settled without a report\", \"selectors\": map[string]any{\"state\": c.StoreDirectory(), \"markerRoot\": \"/marker\", \"workspace\": \"/tmp/workspace\", \"assignment\": \"asg-1\", \"session\": \"01child-session\", \"turn\": \"turn-unreported-1\"}}", "E7 reporting-observation selector records used as supervisor input"},
	{"internal/relay/supervisor/send_test.go", "f.c.Socket = \"/tmp/host.sock\"", "E7 recorded socket identity in supervisor fixtures; delivery uses an injected fake host rather than binding this path"},
	{"internal/relay/supervisor/wording_ids_test.go", "f.c.Socket = \"/tmp/host.sock\"", "E7 recorded socket identity in supervisor fixtures; delivery uses an injected fake host rather than binding this path"},
	{"internal/runtime/doctor/doctor_test.go", "checks, err := doctor.CheckRecord(fields, \"/tmp/x\", true, nil)", "E7 synthetic installed-record path passed to the record validator"},
	{"internal/runtime/doctor/doctor_test.go", "if _, err := doctor.CheckRecord(fields, \"/tmp/x\", false, nil); err == nil || !strings.Contains(err.Error(), \"missing: connected\") {", "E7 synthetic installed-record path passed to the record validator"},
	{"internal/runtime/doctor/doctor_test.go", "if _, err := doctor.CheckRecord(fields, \"/tmp/x\", false, nil); err == nil {", "E7 synthetic installed-record path passed to the record validator"},
	{"internal/runtime/doctor/harness_run_test.go", "_, out, errOut := run(t, []string{\"--json\", \"--key\", \"crw@local\", \"--codex-home\", \"/tmp/x\"}, map[string]string{}, healthy, allOn)", "E7 CLI Codex-home input and expected reported plugin path with injected healthy readers"},
	{"internal/runtime/doctor/harness_run_test.go", "if !strings.Contains(out, \"/tmp/x/plugins/cache\") {", "E7 CLI Codex-home input and expected reported plugin path with injected healthy readers"},
	{"internal/runtime/install/archive_test.go", "// sequential: t.Setenv(\"TMPDIR\") is process-wide.", "E7 comment explaining process-wide TMPDIR test isolation"},
	{"internal/runtime/install/proc_internal_test.go", "{\"a relative script outside\", FakeProcess{Pid: 407, Exe: \"/usr/bin/python3\", Cmdline: Argv(\"python3\", \"bin/codex-thread-bridge\"), Cwd: \"/tmp\"}, false, false, 0, false},", "E7 fake process cwd used to classify an outside relative script"},
	{"internal/sandboxroot/sandboxroottest/sandboxroottest_test.go", "// fakes were made by os.MkdirTemp(\"\", ...) below the relative directory, that is below the working directory.", "E7 comment describing the old relative-TMPDIR fixture behavior under regression"},
	{"internal/testsupport/golden/golden_test.go", "Check(t, \"state\", []byte(\"db at /tmp/run-1/relay.sqlite3\"), Substitute(\"/tmp/run-1\", \"<TMP>\"))", "E7 exact before/after path strings are the input to golden placeholder substitution"},
	{"internal/testsupport/golden/golden_test.go", "if strings.Contains(string(stored), \"/tmp/run-1\") || !strings.Contains(string(stored), \"<TMP>\") {", "E7 exact before/after path strings are the input to golden placeholder substitution"},
	{"internal/testsupport/golden/golden_test.go", "Check(t, \"state\", []byte(\"db at /tmp/run-2/relay.sqlite3\"), Substitute(\"/tmp/run-2\", \"<TMP>\"))", "E7 exact before/after path strings are the input to golden placeholder substitution"},
	{"internal/tmploc/dir_test.go", "{\"unset\", map[string]string{}, \"/tmp\", false},", "E7 expected default and ignored-environment behavior of the one-place temporary-directory policy"},
	{"internal/tmploc/dir_test.go", "{\"empty\", map[string]string{\"TMPDIR\": \"\"}, \"/tmp\", false},", "E7 expected default and ignored-environment behavior of the one-place temporary-directory policy"},
	{"internal/tmploc/dir_test.go", "{\"TMP and TEMP are not read\", map[string]string{\"TMP\": \"/a\", \"TEMP\": \"/b\"}, \"/tmp\", false},", "E7 expected default and ignored-environment behavior of the one-place temporary-directory policy"},
	{"internal/tmploc/dir_test.go", "if DefaultDir != \"/tmp\" {", "E7 expected default and ignored-environment behavior of the one-place temporary-directory policy"},
	{"internal/tmploc/dir_test.go", "if err != nil || dir != \"/tmp\" || source != SourceDefault {", "E7 expected default and ignored-environment behavior of the one-place temporary-directory policy"},
	{"internal/tmploc/socket_test.go", "if SocketRoot != \"/tmp\" || SocketBound != 103 {", "E7 asserts the one-place socket root and its length bound"},
	{"internal/tracking/issue_write_test.go", "for _, want := range []string{\"/home/me\", \"lina-os\", \"/tmp/x\"} {", "E7 private host path strings that a published issue body must not expose"},
	{"web/test/status-state.test.ts", "path: \"/tmp/execution-policy.json\",", "E7 synthetic execution-policy path in a UI state fixture"},
}

// Reasons of the variable-parent table.
const (
	tmpParentBeside   = "the directory of the file the write replaces (a parent spelling or rootDir of the path), which is never \"\": a bare name gives \".\""
	tmpParentRefresh  = "g.dir, the refresh's own directory, made by tmploc in newRefreshGit (base_refresh.go)"
	tmpParentResolved = "a root crwconfig.Resolve answered, which is absolute (strict resolve)"
)

// tmpNeverEmptyParents are the os.MkdirTemp / os.CreateTemp calls whose parent is a variable that
// can never be "" or relative, each with the reason (evidence/mkdirtemp-parents.md, bundle
// 1566-tmp-arch). A call whose parent is filepath.Dir(...) or a non-empty literal needs no entry. A
// new call with any other parent fails until it is listed here with its reason or moves to
// tmploc.MkdirTempIn, which refuses an empty or relative parent.
var tmpNeverEmptyParents = []tmpEntry{
	{"cmd/crw/repomap.go", `f, err := os.CreateTemp(dir, pattern)`, "the marker seam's one caller passes filepath.Dir(path)"},
	{"internal/crwconfig/verifier.go", `tmp, err := os.CreateTemp(dir, ".config-*.json")`, "dir is filepath.Dir of the configuration file's link target"},
	{"internal/dev/ci/local.go", `temp, err := os.MkdirTemp(root, localTempPrefix)`, "root is localWorkRoot's answer, which refuses a work root that is not absolute"},
	{"internal/dev/ci/local.go", `dir, err := os.MkdirTemp(root, localSocketPattern)`, "root is tmploc.SocketRoot, or a test's socketRoot option when it is not \"\""},
	{"internal/dev/cxccorpus/shipped.go", `tmp, err := os.CreateTemp(dir, ".hook-*.tmp")`, "dir is the shipped hooks directory below the checkout, checked to be a plain directory first"},
	{"internal/dev/skillport/stage.go", `if tmp, err = os.MkdirTemp(filepath.Join(root, SkillsRoot), ".stage-"); err != nil {`, "a join below the checkout root with a constant, never \"\""},
	{"internal/flow/report/send.go", `f, err = os.CreateTemp(dir, "crw-notice-*.txt")`, "dir is ManageSender.Dir, used only when it is absolute (the case above); an empty Dir goes to tmploc.CreateTemp"},
	{"internal/flow/trace/trace.go", `tmp, err := os.CreateTemp(dir, ".crw-flow-out-*")`, tmpParentBeside},
	{"internal/hookswitch/write.go", `f, err := os.CreateTemp(dir, ".switch.json.*.tmp")`, "dir is filepath.Dir of the switch file's path"},
	{"internal/integrator/queue.go", `tmp, err := os.CreateTemp(r.Cfg.QueueDir, ".enqueue-"+name+"-")`, "queue_dir is required and must be absolute (integrator Config validation)"},
	{"internal/manage/audit_drafts.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/audit_result_copy.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/audit_round.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/batch_report.go", `file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")`, tmpParentBeside},
	{"internal/manage/capacity.go", `file, err := os.CreateTemp(dir, pattern)`, "the state-write seam's one caller passes rootDir(path)"},
	{"internal/manage/dagreview_host.go", `file, err := os.CreateTemp(stateDir, dagHostStateFile+".*.tmp")`, "stateDir is the dag review host's state directory, the one its lock and offsets live in"},
	{"internal/manage/deliver.go", `file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")`, tmpParentBeside},
	{"internal/manage/improve_run.go", `tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")`, tmpParentBeside},
	{"internal/manage/premerge.go", `tmp, err := os.CreateTemp(dir, ".premerge-*.tmp")`, tmpParentBeside},
	{"internal/pabcd/stateroot/anchor.go", `tmp, err := os.CreateTemp(dir, "."+sessionID+".*.tmp")`, "dir is filepath.Dir of the anchor's path"},
	{"internal/policystore/policy_write.go", `file, err := os.CreateTemp(dir, writeTempPrefix)`, tmpParentBeside},
	{"internal/relay/store/diagnostic_probe.go", `if temp, err := os.CreateTemp(selection.Path, ".probe-"); err == nil {`, "selection.Path is the state directory the probe has just found to exist"},
	{"internal/review/agy/workdir.go", `root, err := os.MkdirTemp(parent, "crw-agy-")`, "parent is filepath.Abs of the work root (the line above), whose default is tmploc.Dir()"},
	{"internal/role/store.go", `f, err := os.CreateTemp(dir, path[strings.LastIndexByte(path, '/')+1:]+".*.tmp")`, tmpParentBeside},
	{"internal/runtime/install/mcp.go", `file, err := os.CreateTemp(dir, ".crw-write-")`, "dir is filepath.Dir of the record's path"},
	{"internal/runtime/pointer/pointer.go", `file, err := os.CreateTemp(parent, ".crw-pointer-")`, "parent is filepath.Dir of the pointer's path"},
	{"internal/skill/base_refresh_generated.go", `dir, err := os.MkdirTemp(g.dir, "witness-")`, tmpParentRefresh},
	{"internal/skill/base_refresh_regenerate.go", `dir, err := os.MkdirTemp(g.dir, "run-")`, tmpParentRefresh},
	{"internal/skill/merge_build_check.go", `work, err := os.MkdirTemp(g.dir, "merge-build-")`, tmpParentRefresh},
	{"internal/skill/merge_build_check_go.go", `made, err := os.MkdirTemp(tools.BuildWork, "merge-build-go-")`, "build_work is used only when it is not \"\" (the check above), and it is " + tmpParentResolved},
	{"internal/testsupport/testsupport.go", `root, err = os.MkdirTemp(top, "h-")`, "top is the inherited isolation root, used only when it is absolute and an existing directory"},
	{"internal/tools/install.go", `dir, err := os.MkdirTemp(toolsRoot, stagingPrefix+pin.Name+"-")`, "toolsRoot is tools_root, " + tmpParentResolved},
	{"internal/tools/tools.go", `return os.MkdirTemp(dir, pattern)`, "the download seam's one caller passes temp_root, " + tmpParentResolved + "; /tmp/crw when TMPDIR is unset (G-D1 = A)"},
}

// tmpGrep runs git grep over the tracked files and answers the hits.
func tmpGrep(t *testing.T, pattern string, paths ...string) []tmpHit {
	t.Helper()
	out, err := runGit(repoRoot(), append([]string{"grep", "-n", "-I", "-E", pattern, "--"}, paths...)...)
	if err != nil {
		t.Fatalf("git grep %s: %v", pattern, err)
	}
	var hits []tmpHit
	for _, raw := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		file, rest, ok := strings.Cut(raw, ":")
		number, text, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 {
			t.Fatalf("git grep line %q has no file:line: form", raw)
		}
		var line int
		if _, err := fmt.Sscanf(number, "%d", &line); err != nil {
			t.Fatalf("git grep line %q: %v", raw, err)
		}
		hits = append(hits, tmpHit{file, line, strings.TrimSpace(text)})
	}
	if len(hits) == 0 {
		t.Fatalf("git grep %s found nothing: the check reads no file", pattern)
	}
	return hits
}

// tmpDecisionHits are the hits of tmpDecisionPattern over every tracked file.
func tmpDecisionHits(t *testing.T) []tmpHit {
	return tmpGrep(t, tmpDecisionPattern)
}

// tmpParentHits are the os.MkdirTemp / os.CreateTemp calls the variable-parent rule judges: non-test
// Go outside tmploc (E1) and the recorded data (E3), with comment lines, empty parents (the main check)
// and the parents that are filepath.Dir(...) or a non-empty literal left out.
func tmpParentHits(t *testing.T) []tmpHit {
	call := regexp.MustCompile(tmpParentPattern)
	var judged []tmpHit
	for _, h := range tmpGrep(t, tmpParentPattern, "*.go", ":!*_test.go", ":!internal/tmploc/*.go") {
		if strings.HasPrefix(h.text, "//") || tmpRecordedData(h.file) {
			continue
		}
		loc := call.FindStringIndex(h.text)
		arg := h.text[loc[1]:]
		switch {
		case strings.HasPrefix(arg, `""`):
			continue // an empty parent: tmpDecisionPattern's
		case strings.HasPrefix(arg, `"`), strings.HasPrefix(arg, "filepath.Dir("):
			continue // a non-empty literal, or the directory of a path, which is never ""
		}
		judged = append(judged, h)
	}
	return judged
}

// tmpMetadataLines recognizes only complete, string-only exception entries.
// Other code in this file remains subject to the same location guard.
func tmpMetadataLines(src []byte) (map[int]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "tmp_decisions_test.go", src, 0)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(src), "\n")
	metadata := map[int]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			switch value.Names[0].Name {
			case "tmpTestLines", "tmpTmplocLines":
			default:
				continue
			}
			table, ok := value.Values[0].(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elem := range table.Elts {
				row, ok := elem.(*ast.CompositeLit)
				if !ok || len(row.Elts) != 3 {
					continue
				}
				literal := true
				for _, field := range row.Elts {
					text, ok := field.(*ast.BasicLit)
					if !ok || text.Kind != token.STRING {
						literal = false
						break
					}
				}
				start, end := fset.Position(row.Pos()), fset.Position(row.End())
				if !literal || start.Line != end.Line {
					continue
				}
				text := strings.TrimSpace(lines[start.Line-1])
				// Never cover another statement appended on the same line.
				if text != string(src[start.Offset:end.Offset])+"," {
					continue
				}
				metadata[start.Line] = text
			}
		}
	}
	return metadata, nil
}

// tmpCovered says which exception allows a hit ("" for none).
func tmpCovered(h tmpHit, metadata map[int]string) string {
	for _, g := range tmpGlobs {
		if g.covers(h.file) {
			return g.name
		}
	}
	for _, e := range tmpTmplocLines {
		if e.matches(h) {
			return "E1"
		}
	}
	if tmpRecordedPayload.matches(h) {
		return "E4"
	}
	if tmpRecordedScratch.matches(h) {
		return "E5"
	}
	for _, e := range tmpTestLines {
		if e.matches(h) {
			return "E7"
		}
	}
	if h.file == "internal/dev/ci/tmp_decisions_test.go" && metadata[h.line] == h.text && h.text != "" {
		return "E7 literal exception metadata"
	}
	return ""
}

// CRW-1579 (D8): every temporary-location decision in a tracked file is tmploc's, an exception with
// its reason. Anything else is a second place deciding a temporary location.
func TestTempLocationsComeFromTmploc(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(), "internal/dev/ci/tmp_decisions_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := tmpMetadataLines(src)
	if err != nil {
		t.Fatal(err)
	}
	var loose []string
	for _, h := range tmpDecisionHits(t) {
		if tmpCovered(h, metadata) == "" {
			loose = append(loose, h.String())
		}
	}
	if len(loose) > 0 {
		t.Errorf("%d line(s) decide a temporary location outside internal/tmploc and are no exception; "+
			"use tmploc (Dir, MkdirTemp, CreateTemp, MkdirTempIn) or t.TempDir() / tmploctest:\n%s", len(loose), strings.Join(loose, "\n"))
	}
}

// CRW-1579 (D8, R1-7): a non-test os.MkdirTemp / os.CreateTemp whose parent is a variable needs a
// reason that the variable is never "" or relative (tmpNeverEmptyParents); "" would be TMPDIR decided inside os, and a relative parent the working directory.
func TestTempParentsAreNeverEmpty(t *testing.T) {
	var loose []string
	for _, h := range tmpParentHits(t) {
		if !slices.ContainsFunc(tmpNeverEmptyParents, h.matchedBy) {
			loose = append(loose, h.String())
		}
	}
	if len(loose) > 0 {
		t.Errorf("%d os.MkdirTemp / os.CreateTemp call(s) take a parent that may be \"\" or relative; use tmploc.MkdirTempIn "+
			"(which refuses both) or list the call in tmpNeverEmptyParents with the reason it is never empty:\n%s", len(loose), strings.Join(loose, "\n"))
	}
}

func (h tmpHit) matchedBy(e tmpEntry) bool { return e.matches(h) }

// CRW-1579 (D8): every exception entry still matches a hit, so an entry whose site moved
// or changed is removed rather than left to allow a later line. An entry listed twice fails too.
func TestTempDecisionExceptionsAreLive(t *testing.T) {
	hits := tmpDecisionHits(t)
	for _, g := range tmpGlobs {
		if !slices.ContainsFunc(hits, func(h tmpHit) bool { return g.covers(h.file) }) {
			t.Errorf("exception %s (%s) covers no hit: remove it", g.name, g.reason)
		}
	}
	for _, file := range tmpPortProse {
		if !slices.ContainsFunc(hits, func(h tmpHit) bool { return h.file == file }) {
			t.Errorf("E6 lists %s, which has no hit: remove it", file)
		}
	}
	parentHits := tmpParentHits(t)
	for _, table := range []struct {
		name    string
		entries []tmpEntry
		hits    []tmpHit
	}{
		{"E1", tmpTmplocLines, hits},
		{"E4", []tmpEntry{tmpRecordedPayload}, hits},
		{"E5", []tmpEntry{tmpRecordedScratch}, hits},
		{"E7", tmpTestLines, hits},
		{"tmpNeverEmptyParents", tmpNeverEmptyParents, parentHits},
	} {
		seen := map[[2]string]bool{}
		for _, e := range table.entries {
			key := [2]string{e.file, e.text}
			if seen[key] {
				t.Errorf("%s lists %s: %q twice", table.name, e.file, e.text)
			}
			seen[key] = true
			if strings.TrimSpace(e.why) == "" {
				t.Errorf("%s lists %s: %q with no reason", table.name, e.file, e.text)
			}
			if !slices.ContainsFunc(table.hits, func(h tmpHit) bool { return e.matches(h) }) {
				t.Errorf("%s lists %s: %q, which no longer matches a line: remove it (or fix its text)", table.name, e.file, e.text)
			}
		}
	}
}

// CRW-1579 (D8, R2-2): the shell sites decide the temporary location in one shell form of D1, and
// no other spelling of TMPDIR is read in a shell script of the plugin or of scripts/. An unset or
// empty TMPDIR is /tmp; a relative one stops with tmpdir_not_absolute and exit status 2 before any
// directory is made. The behaviour is pinned by tmp_shell_sites_test.go.
const tmpShellPattern = `[$][{]?TMPDIR`

// tmpShellAssign is the one line that reads TMPDIR in a shell site; tmpShellForm is the whole form
// every shell site holds verbatim (the later uses read TMPBASE, which is no hit).
const tmpShellAssign = `TMPBASE="${TMPDIR:-/tmp}"`

const tmpShellForm = tmpShellAssign + `
case "$TMPBASE" in
  /*) ;;
  *) printf 'tmpdir_not_absolute: TMPDIR=%s; set TMPDIR to an absolute directory\n' "$TMPBASE" >&2; exit 2 ;;
esac
`

// tmpShellSites are the shell scripts that decide a temporary location.
var tmpShellSites = []string{
	"plugins/crw/skills/crw-dev-scaffolding/scripts/scaffold-audit.sh",
	"plugins/crw/skills/crw-dev-visualizer/scripts/diagram-to-html.sh",
}

func TestShellTempSitesUseTheSharedForm(t *testing.T) {
	perFile := map[string]int{}
	for _, h := range tmpGrep(t, tmpShellPattern, "plugins/*.sh", "scripts/*.sh") {
		if !slices.Contains(tmpShellSites, h.file) || h.text != tmpShellAssign {
			t.Errorf("%s reads TMPDIR outside the shared shell form (%s): %s", h.file, tmpShellAssign, h)
			continue
		}
		perFile[h.file]++
	}
	for _, file := range tmpShellSites {
		if perFile[file] != 1 {
			t.Errorf("%s holds %d reads of TMPDIR in the shared form, want 1", file, perFile[file])
		}
		text, err := os.ReadFile(filepath.Join(repoRoot(), file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(text), tmpShellForm) {
			t.Errorf("%s lacks the shared shell form verbatim:\n%s", file, tmpShellForm)
		}
	}
}

// CRW-1579 (D8): the lane ends with no pending site. The pending lists were the mechanism that let a
// site wait for its unit; they are gone, and this test fails if a table of that kind comes back in
// this file, so a site is either moved, an exception with its reason, or a failing check.
func TestNoPendingTempDecisions(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "tmp_decisions_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			if v, ok := spec.(*ast.ValueSpec); ok {
				for _, name := range v.Names {
					if strings.HasPrefix(name.Name, "tmpPending") || strings.HasPrefix(name.Name, "tmpU") {
						t.Errorf("%s is a pending-site table: move the site or give it an exception with its reason", name.Name)
					}
				}
			}
		}
	}
}

// TestTempDecisionTestExceptionsStayExact: a named test line covers its own line only - the same
// text with a comment appended, or the same text in another file, is not covered.
func TestTempDecisionTestExceptionsStayExact(t *testing.T) {
	e := tmpTestLines[0]
	if tmpCovered(tmpHit{e.file, 1, e.text}, nil) == "" {
		t.Fatal("the named source line must be covered")
	}
	for _, h := range []tmpHit{
		{e.file, 1, e.text + " // changed"},
		{"internal/dev/ci/new_site_test.go", 1, e.text},
		{"web/test/new-site.test.ts", 1, e.text},
	} {
		if got := tmpCovered(h, nil); got != "" {
			t.Errorf("unnamed test line covered by %s: %s", got, h)
		}
	}
}

// TestTempDecisionMetadataIsLiteralOnly: the metadata pass recognizes a complete, single-line,
// string-only row of the exception tables - never a row with a call, another table, or code on the
// same line.
func TestTempDecisionMetadataIsLiteralOnly(t *testing.T) {
	e := tmpTestLines[0]
	row := fmt.Sprintf("{%q, %q, %q},", e.file, e.text, e.why)
	cases := []struct {
		name, src string
		want      int
	}{
		{"literal row", "package ci\nvar tmpTestLines = []tmpEntry{\n" + row + "\n}\n", 1},
		{"call in row", "package ci\nvar tmpTestLines = []tmpEntry{\n" + fmt.Sprintf("{%q, %q, reason()},", e.file, e.text) + "\n}\n", 0},
		{"other table", "package ci\nvar other = []tmpEntry{\n" + row + "\n}\n", 0},
		{"same line code", "package ci\nvar tmpTestLines = []tmpEntry{" + strings.TrimSuffix(row, ",") + "}; var other = choose()\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := tmpMetadataLines([]byte(c.src))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != c.want {
				t.Fatalf("metadata lines = %v, want %d", got, c.want)
			}
			if c.want == 1 && got[3] != row {
				t.Fatalf("metadata lost exact source text: %v", got)
			}
		})
	}
}
