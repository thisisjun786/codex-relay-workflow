package doctor

// This file replays testdata/harness/drift/oracle.json over the harness drift and ast-grep checks
// and ports the oracle tests that call only those two functions: cxc-ops/test/fixtures.test.ts
// (L21.3, runDriftCheck) and cxc-ops/test/ast-grep.test.ts (the three runAstGrepCheck cases). The
// recorded answers keep the oracle spelling, recorded by testdata/harness/drift/record-drift.mjs
// from the read-only CXC v0.2.40 dist with Node v24.20.0.
//
// Names decision: the recorded files keep the oracle's spelling (skills/ast-grep/...); the replay
// builds every tree through the rename the table declares (contract/schema/cxc/name-substitution.json:
// the folder cxc-ast-grep becomes crw-ast-grep), the same substitution the corpus replayer applies,
// and checks the helper call the port makes against that path.
//
// Engine errors: a recorded check whose evidence the oracle built from a swallowed readFileSync or
// JSON.parse error carries evidencePrefix + errorClass instead of comparable text, because the
// oracle text names a host path (ENOENT, EISDIR) or a V8 parse message (SyntaxError); the replay
// asserts the Go error text of the same class. A swallowed V8 TypeError names no host and is
// compared byte for byte.
//
// The runner seam: the oracle reads spawnSync error.code ENOENT and status 127/9009 as a missing
// interpreter, and a killed timeout (status null, signal SIGTERM, error ETIMEDOUT) falls through
// to the install hint. HarnessRun keeps only the status, so a recorded ENOENT is replayed as a run
// with no status and a recorded ETIMEDOUT as a killed run -- the status Go's os.ProcessState
// ExitCode answers for a signalled process (-1).
//
// Every case runs in a t.TempDir() tree; neither function reads HOME, CODEX_HOME or CRW_HOME, so no
// real state is reachable here (the recorder itself ran under temporary homes).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type harnessDriftCheckRecorded struct {
	Name           string `json:"name"`
	Severity       string `json:"severity"`
	Evidence       string `json:"evidence"`
	Repair         string `json:"repair"`
	EvidencePrefix string `json:"evidencePrefix"`
	ErrorClass     string `json:"errorClass"`
}

type harnessDriftCaseRecorded struct {
	Name    string                      `json:"name"`
	Files   map[string]string           `json:"files"`
	Dirs    []string                    `json:"dirs"`
	Outside map[string]string           `json:"outside"`
	Checks  []harnessDriftCheckRecorded `json:"checks"`
}

type harnessDriftRunRecorded struct {
	Status *int   `json:"status"`
	Error  string `json:"error"`
	Signal string `json:"signal"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

type harnessDriftCallRecorded struct {
	File    string   `json:"file"`
	Args    []string `json:"args"`
	Timeout int      `json:"timeout"`
}

type harnessDriftAstGrepCaseRecorded struct {
	Name  string                    `json:"name"`
	Files map[string]string         `json:"files"`
	Run   *harnessDriftRunRecorded  `json:"run"`
	Check harnessDriftCheckRecorded `json:"check"`
	Call  *harnessDriftCallRecorded `json:"call"`
}

type harnessDriftOracle struct {
	Oracle  string                            `json:"oracle"`
	Drift   []harnessDriftCaseRecorded        `json:"drift"`
	AstGrep []harnessDriftAstGrepCaseRecorded `json:"astGrep"`
}

func harnessDriftOracleRecorded(t *testing.T) harnessDriftOracle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "harness", "drift", "oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded harnessDriftOracle
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") {
		t.Fatalf("oracle.json names %q, want the CXC v0.2.40 commit", recorded.Oracle)
	}
	if len(recorded.Drift) == 0 || len(recorded.AstGrep) == 0 {
		t.Fatalf("oracle.json holds an empty group: %d drift, %d ast-grep", len(recorded.Drift), len(recorded.AstGrep))
	}
	return recorded
}

// harnessDriftRenamed is the names decision for the one path these checks spell: the ast-grep
// skill folder (contract/schema/cxc/name-substitution.json, cxc-ast-grep -> crw-ast-grep).
func harnessDriftRenamed(path string) string {
	return strings.ReplaceAll(path, "skills/ast-grep/", "skills/crw-ast-grep/")
}

// harnessDriftTree builds one case's plugin root in a fresh temp directory, writing the recorded
// files and directories through the names decision.
func harnessDriftTree(t *testing.T, files map[string]string, dirs []string) string {
	t.Helper()
	plugin := filepath.Join(t.TempDir(), "plugin")
	harnessDriftWriteTree(t, plugin, files, dirs)
	return plugin
}

// harnessDriftWriteTree writes a recorded case's relative files and directories under root,
// through the names decision. The outside files of a case are written against the case root.
func harnessDriftWriteTree(t *testing.T, root string, files map[string]string, dirs []string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(harnessDriftRenamed(rel))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(harnessDriftRenamed(rel)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// harnessDriftCheckMatches judges one recorded check against the port's answer.
func harnessDriftCheckMatches(t *testing.T, got HarnessCheck, want harnessDriftCheckRecorded) {
	t.Helper()
	if got.Name != want.Name || string(got.Severity) != want.Severity {
		t.Fatalf("check = [%s] %s, want [%s] %s", got.Severity, got.Name, want.Severity, want.Name)
	}
	switch want.ErrorClass {
	case "", "TypeError":
		// No engine error, or the deterministic V8 TypeError text: byte for byte.
		if got.Evidence != want.Evidence {
			t.Fatalf("%s evidence = %q, want %q", got.Name, got.Evidence, want.Evidence)
		}
	case "ENOENT":
		harnessDriftEvidenceClass(t, got, want, "no such file or directory")
	case "EISDIR":
		harnessDriftEvidenceClass(t, got, want, "is a directory")
	case "SyntaxError":
		harnessDriftEvidenceClass(t, got, want, "")
	default:
		t.Fatalf("%s: unknown recorded error class %q", want.Name, want.ErrorClass)
	}
	if got.Repair != want.Repair {
		t.Fatalf("%s repair = %q, want %q", got.Name, got.Repair, want.Repair)
	}
}

// harnessDriftEvidenceClass asserts the shape of a check whose oracle evidence was built from a
// swallowed engine error naming a host path or carrying a V8 parse message: the recorded prefix, a
// non-empty Go error text, and the errno phrase of that class where one exists.
func harnessDriftEvidenceClass(t *testing.T, got HarnessCheck, want harnessDriftCheckRecorded, marker string) {
	t.Helper()
	rest, ok := strings.CutPrefix(got.Evidence, want.EvidencePrefix)
	if !ok {
		t.Fatalf("%s evidence = %q, want the prefix %q", want.Name, got.Evidence, want.EvidencePrefix)
	}
	if rest == "" {
		t.Fatalf("%s evidence = %q, want a non-empty engine error text", want.Name, got.Evidence)
	}
	if marker != "" && !strings.Contains(rest, marker) {
		t.Fatalf("%s evidence = %q, want the %s class marker %q", want.Name, got.Evidence, want.ErrorClass, marker)
	}
}

// harnessDriftRun maps a recorded spawnSync shape onto the Go seam: a process that never started
// (error ENOENT) has no status; a probe the oracle killed at the timeout (status null, signal
// SIGTERM, error ETIMEDOUT) is a run whose process died on a signal, which Go reports with the -1
// exit code of os.ProcessState.ExitCode; every other shape carries its own status.
func harnessDriftRun(t *testing.T, want *harnessDriftRunRecorded) HarnessRun {
	t.Helper()
	if want == nil {
		t.Fatal("the case drives no runner shape")
	}
	run := HarnessRun{Status: want.Status, Stdout: want.Stdout, Stderr: want.Stderr}
	if want.Status == nil && want.Error == "ETIMEDOUT" {
		killed := harnessDriftKilled
		run.Status = &killed
	}
	return run
}

func TestHarnessDriftChecksRecorded(t *testing.T) {
	for _, want := range harnessDriftOracleRecorded(t).Drift {
		t.Run(want.Name, func(t *testing.T) {
			plugin := harnessDriftTree(t, want.Files, want.Dirs)
			harnessDriftWriteTree(t, filepath.Dir(plugin), want.Outside, nil)
			wanted := want.Checks
			if strings.HasPrefix(want.Name, "intentionally_changed_") {
				wanted = harnessDriftIntentionallyChanged(t, want.Name)
			}
			got := HarnessDriftChecks(plugin)
			if len(got) != len(wanted) {
				t.Fatalf("checks = %+v, want the %d expected", got, len(wanted))
			}
			for i := range wanted {
				gotCheck, wantCheck := got[i], wanted[i]
				if strings.Contains(want.Name, "lone_surrogate") {
					// The recorders' toWellFormed convention: a recorded string a JSON writer carries
					// as \"\\ud800\" reads back through encoding/json as U+FFFD, and UTF-8 encoding
					// writes a lone surrogate the same way. The port keeps the oracle's raw WTF-8
					// value for the writer to spell.
					gotCheck.Evidence = strings.ToValidUTF8(gotCheck.Evidence, "\uFFFD")
					wantCheck.Evidence = strings.ToValidUTF8(wantCheck.Evidence, "\uFFFD")
				}
				harnessDriftCheckMatches(t, gotCheck, wantCheck)
			}
		})
	}
}

// harnessDriftIntentionallyChanged is the port's answer for a case whose oracle answer this PR
// deliberately changes, the form hooktrust_entries_test.go uses for its intentionally_changed_
// cases. Only one exists: a Devin review finding of kind security (port: fixed in
// docs/port-cxc/known-defects.md), a manifest mcpServers reference that resolves outside the
// plugin root -- the oracle reads wherever path.join lands, the port refuses it.
func harnessDriftIntentionallyChanged(t *testing.T, name string) []harnessDriftCheckRecorded {
	t.Helper()
	switch name {
	case "intentionally_changed_mcp_reference_escapes_root":
		return []harnessDriftCheckRecorded{
			{Name: "drift:version", Severity: "PASS", Evidence: "declared plugin version 0.4.0"},
			{Name: "drift:mcp", Severity: "FAIL", Evidence: "mcpServers -> ../outside.json resolves outside the plugin root"},
			{Name: "known-issues", Severity: "WARN", Evidence: "drift FAIL in [drift:mcp] \u2014 re-run `npm run build`, then inspect the named file before reinstalling"},
		}
	}
	t.Fatalf("no port answer recorded for the intentionally changed case %q", name)
	return nil
}

func TestHarnessAstGrepCheckRecorded(t *testing.T) {
	for _, want := range harnessDriftOracleRecorded(t).AstGrep {
		t.Run(want.Name, func(t *testing.T) {
			plugin := harnessDriftTree(t, want.Files, nil)
			var calls []harnessDriftCallRecorded
			run := func(file string, args []string, timeout time.Duration) HarnessRun {
				calls = append(calls, harnessDriftCallRecorded{File: file, Args: args, Timeout: int(timeout / time.Millisecond)})
				return harnessDriftRun(t, want.Run)
			}
			harnessDriftCheckMatches(t, HarnessAstGrepCheck(plugin, run), want.Check)
			if want.Call == nil {
				if len(calls) != 0 {
					t.Fatalf("the check ran the probe %d time(s), want none", len(calls))
				}
				return
			}
			if len(calls) != 1 {
				t.Fatalf("probe calls = %d, want exactly one", len(calls))
			}
			// The recorded call is the oracle's; the port makes it through the names decision,
			// and {ROOT} is the case root the recorder substituted.
			expected := make([]string, 0, len(want.Call.Args))
			for _, arg := range want.Call.Args {
				arg = strings.ReplaceAll(arg, "{ROOT}", filepath.Dir(plugin))
				expected = append(expected, harnessDriftRenamed(arg))
			}
			got := calls[0]
			if got.File != want.Call.File || got.Timeout != want.Call.Timeout || !slices.Equal(got.Args, expected) {
				t.Fatalf("probe = %s %v (%dms), want %s %v (%dms)", got.File, got.Args, got.Timeout, want.Call.File, expected, want.Call.Timeout)
			}
		})
	}
}

// A port-only case, not a corpus claim (hence no intentionally_changed_ prefix): the seam wording
// inherited from harness_report.go says a runner that cannot start a process and one it killed
// both answer Status nil, and under that wording a killed ast-grep probe reads as the missing
// interpreter instead of the oracle's install hint. The recorded timeout_killed case drives the
// other reading (-1, what ExitCode answers for a signalled process); this case pins the collapse so
// it cannot change unnoticed.
func TestHarnessAstGrepCheckKilledWithNoStatusPort(t *testing.T) {
	plugin := harnessDriftTree(t, map[string]string{"skills/ast-grep/scripts/ast_grep_helper.py": "# stub\n"}, nil)
	check := HarnessAstGrepCheck(plugin, func(string, []string, time.Duration) HarnessRun {
		return HarnessRun{Stderr: "signal: killed"}
	})
	want := HarnessCheck{Name: "ast-grep", Severity: HarnessWarn, Evidence: "python3 not found - install Python 3.9+ to run the ast-grep helper"}
	if check != want {
		t.Fatalf("HarnessAstGrepCheck = %+v, want the missing-interpreter reading %+v", check, want)
	}
}
