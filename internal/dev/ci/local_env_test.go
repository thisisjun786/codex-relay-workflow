//go:build dev

package ci

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1489: a step gets a fresh TMPDIR of its own, made directly below the caller's TMPDIR, and never
// the caller's TMPDIR itself: a test that walks up from TMPDIR looking for a repository then finds only
// what the run made, and a directory the caller shares with other runs is not where the step writes.
func TestLocal_a_step_gets_a_fresh_TMPDIR_not_the_callers(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := fmt.Sprintf(`test "$TMPDIR" != %[1]q && test "$(dirname "$TMPDIR")" = %[1]q && `+
		`case "$(basename "$TMPDIR")" in ci[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;; *) exit 1 ;; esac && `+
		`test -z "$(ls -A "$TMPDIR")"`, dir)
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s), want a fresh, empty TMPDIR named ci<8 hex> directly below the caller's", step.Result, step.Reason)
	}
}

// A failing test that is not among the last lines of a long output still reaches the reason.
func TestLocal_a_failed_test_before_a_long_tail_still_reaches_the_reason(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	command := `echo "--- FAIL: TestEarly"; i=0; while [ $i -lt 80 ]; do echo "ok line $i"; i=$((i+1)); done; echo "make: *** Error 1"; exit 1`
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reason := made.Jobs[0].Steps[1].Reason; !strings.Contains(reason, "--- FAIL: TestEarly") {
		t.Errorf("the reason %q does not carry the failing test", reason)
	}
}

// A step stops at its first failed command, as the runner's bash -eo pipefail does, so a failure
// in the middle of a multi-command step cannot be hidden by a later command that passes.
func TestLocal_a_step_stops_at_its_first_failed_command(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("false\necho after"), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localFailed {
		t.Errorf("the step is %q, want %q: a later command hid the failure", step.Result, localFailed)
	}
}

// A step leaves GOENV unset, as CI does, so Go reads its environment file from the run's own
// home (XDG_CONFIG_HOME) and never from the caller's GOENV, which a product test in the same run
// would otherwise see as the caller's setting.
func TestLocal_a_step_leaves_GOENV_unset_and_reads_its_own_home(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "host-goenv"))
	command := `test -z "${GOENV+set}" && case "$(go env GOENV)" in "$HOME"/*) ;; *) exit 1 ;; esac`
	opts := localRunOptions(repo, localFixturePlan(command), record)
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if step := made.Jobs[0].Steps[1]; step.Result != localPassed {
		t.Errorf("the step is %q (%s), want GOENV unset and Go's file under the run's home", step.Result, step.Reason)
	}
}

// The tool versions a run records are the ones the reuse check computes from the same commit, so a
// record made on this host is never refused for a tool the host lacks (the fetched tools name their pins).
func TestLocal_the_recorded_tool_versions_match_the_reuse_keys(t *testing.T) {
	repo := newLocalFixture(t)
	record := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("echo hello"), record)
	current, err := localCurrentKeys(opts)
	if err != nil {
		t.Fatal(err)
	}
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"gitleaks", "staticcheck"} {
		if made.Tools[name] != current.Tools[name] {
			t.Errorf("%s: the record says %q, the reuse key says %q", name, made.Tools[name], current.Tools[name])
		}
	}
}

// CRW-964 parent ruling 2, d1: GOWORK is off in every step and every tool probe, so an ancestor
// go.work never selects another module set.
func TestLocal_a_step_never_selects_a_go_workspace(t *testing.T) {
	temp := t.TempDir()
	env, err := localStepEnv(filepath.Join(temp, "home"), temp, localOptions{output: filepath.Join(temp, "output")})
	if err != nil {
		t.Fatal(err)
	}
	if !localContains(env, "GOWORK=off") {
		t.Errorf("the step environment does not disable go.work: %v", env)
	}
	if probe := localProbeEnv(filepath.Join(temp, "probe"), "/usr/bin"); !localContains(probe, "GOWORK=off") {
		t.Errorf("the tool probe does not disable go.work: %v", probe)
	}
}

// CRW-1494: a step env carries exactly one TMPDIR entry. The inherited-variable loop skips the caller's
// TMPDIR and the step's own is appended once, so a later entry never shadows an earlier one by accident.
func TestLocal_a_step_env_has_exactly_one_TMPDIR(t *testing.T) {
	count := func(env []string) (int, string) {
		n, last := 0, ""
		for _, entry := range env {
			if strings.HasPrefix(entry, "TMPDIR=") {
				n++
				last = entry
			}
		}
		return n, last
	}
	t.Setenv("TMPDIR", t.TempDir()) // a caller TMPDIR that exists, so t.TempDir keeps working
	for _, tc := range []struct {
		name    string
		stepTmp func(temp string) string
		want    func(temp string) string
	}{
		{"a configured step TMPDIR", func(temp string) string { return filepath.Join(temp, "step") }, func(temp string) string { return "TMPDIR=" + filepath.Join(temp, "step") }},
		{"no step TMPDIR", func(string) string { return "" }, func(temp string) string { return "TMPDIR=" + filepath.Join(temp, "tmp") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			temp := t.TempDir()
			opts := localOptions{output: filepath.Join(t.TempDir(), "output"), stepTmp: tc.stepTmp(temp)}
			env, err := localStepEnv(filepath.Join(temp, "home"), temp, opts)
			if err != nil {
				t.Fatal(err)
			}
			n, last := count(env)
			if n != 1 {
				t.Errorf("the step environment has %d TMPDIR entries, want exactly one: %v", n, env)
			}
			if want := tc.want(temp); last != want {
				t.Errorf("the step TMPDIR entry is %q, want %q", last, want)
			}
		})
	}
}
