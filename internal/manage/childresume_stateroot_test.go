package manage

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
)

// resumeInFlightAt writes the child's PABCD state at root in phase P with a plan epoch and a
// goalplan beside it, and returns every byte under root/.crw.
func resumeInFlightAt(t *testing.T, root string) map[string]string {
	t.Helper()
	s := state.DefaultState("01child", "work")
	s.Phase, s.OrchestrationActive = state.PhaseP, true
	epoch := "epoch-1"
	s.PlanEpoch = &epoch
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".crw", "goalplans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".crw", "goalplans", "work.md"), []byte("# goalplan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return resumeTreeBytes(t, root)
}

func resumeTreeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(filepath.Join(root, ".crw"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		out[path] = string(raw)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// resumeSettingsAt is the authorized record with cwd as its working directory.
func resumeSettingsAt(cwd string) string {
	return strings.ReplaceAll(resumeTestSettings, `"/w"`, `"`+cwd+`"`)
}

// CRW-1140 criterion 1 on child-resume: the record names cwd B while the host reports the child at
// A, whose PABCD state is in flight. The run, and the dry run, refuse before thread/resume with
// state_root_conflict; A's bytes are kept and B gets no state.
func TestResumeRefusesARecordThatMovesInFlightStateToAnotherCwd(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "dry-run"}[dry], func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			before := resumeInFlightAt(t, a)
			host := resumeHost(t, "notLoaded")
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"cwd": a, "model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
			exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeSettingsAt(b), 0)
			e, _, _ := resumeEnv(t, exe)
			t.Setenv("CRW_HOME", t.TempDir())
			_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m", dryRun: dry})
			var failure *resumeFailure
			if !errors.As(err, &failure) || failure.Reason != "state_root_conflict" {
				t.Fatalf("err = %v, want state_root_conflict", err)
			}
			if !strings.Contains(failure.Detail, state.StatePath(a, "01child")) {
				t.Errorf("the refusal does not name the preserved state: %s", failure.Detail)
			}
			if code := resumeExit(failure); code != 4 {
				t.Errorf("state_root_conflict exits %d, want 4", code)
			}
			if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
				t.Fatalf("the host saw %q", methods)
			}
			if after := resumeTreeBytes(t, a); len(after) != len(before) {
				t.Fatalf("the native state changed: %v", after)
			} else {
				for k, v := range before {
					if after[k] != v {
						t.Fatalf("%s changed", k)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the target cwd got a state directory: %v", err)
			}
			// The dry run writes nothing, not even the anchor a resuming run records.
			if _, err := os.Stat(filepath.Join(os.Getenv("CRW_HOME"), "state-roots")); dry != errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("dry=%v: anchor directory %v", dry, err)
			}
		})
	}
}

// A record at the native cwd resumes as before, with the work in flight.
func TestResumeAtTheNativeCwdGoesAheadWithWorkInFlight(t *testing.T) {
	a := t.TempDir()
	resumeInFlightAt(t, a)
	host := resumeHost(t, "notLoaded")
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"cwd": a, "model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeSettingsAt(a), 0)
	e, _, _ := resumeEnv(t, exe)
	t.Setenv("CRW_HOME", t.TempDir())
	if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatal(err)
	}
	if n := host.Count("thread/resume"); n != 1 {
		t.Fatalf("thread/resume was called %d times", n)
	}
}

func resumeAnchor(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("CRW_HOME"), "state-roots", "01child.json"))
	if err != nil {
		return ""
	}
	var anchor struct{ NativeCwd string }
	if json.Unmarshal(raw, &anchor) != nil {
		t.Fatalf("anchor %s", raw)
	}
	return anchor.NativeCwd
}

// Review P1: the child was anchored at A, which holds its work, and the host now reports B (an
// external resume moved it). A record naming B is judged against A, in the run and the dry run, and
// the dry run writes nothing.
func TestResumeIsJudgedAgainstThePreservedAnchorWhenTheHostReportsAnotherCwd(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "dry-run"}[dry], func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			t.Setenv("CRW_HOME", t.TempDir())
			before := resumeInFlightAt(t, a)
			if c := stateroot.Guard(os.LookupEnv, a, a, "01child"); c != nil {
				t.Fatal(c)
			}
			host := resumeHost(t, "notLoaded")
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"cwd": b, "model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
			exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeSettingsAt(b), 0)
			e, _, _ := resumeEnv(t, exe)
			_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m", dryRun: dry})
			var failure *resumeFailure
			if !errors.As(err, &failure) || failure.Reason != "state_root_conflict" {
				t.Fatalf("err = %v, want state_root_conflict", err)
			}
			if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
				t.Fatalf("the host saw %q", methods)
			}
			if after := resumeTreeBytes(t, a); len(after) != len(before) || resumeAnchor(t) != a {
				t.Fatalf("the native state or anchor changed (anchor %q)", resumeAnchor(t))
			}
		})
	}
}

// A run the host took moves the anchor to the cwd the child now runs at; a dry run, which resumes
// nothing, leaves the anchor where it was.
func TestOnlyAResumeTheHostTookMovesTheAnchor(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(map[bool]string{false: "run", true: "dry-run"}[dry], func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			t.Setenv("CRW_HOME", t.TempDir())
			if _, err := state.EnsureState(a, "01child"); err != nil {
				t.Fatal(err)
			}
			if c := stateroot.Guard(os.LookupEnv, a, a, "01child"); c != nil {
				t.Fatal(c)
			}
			host := resumeHost(t, "notLoaded")
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"cwd": a, "model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
			exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeSettingsAt(b), 0)
			e, _, _ := resumeEnv(t, exe)
			if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m", dryRun: dry}); err != nil {
				t.Fatal(err)
			}
			want := a
			if !dry {
				want = b
			}
			if got := resumeAnchor(t); got != want {
				t.Fatalf("anchor %q, want %q", got, want)
			}
		})
	}
}
