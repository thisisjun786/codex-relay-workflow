package stateroot

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

const session = "0190cafe-0279-7000-8000-000000000279"

func envAt(crwHome string) host.LookupEnv {
	return func(key string) (string, bool) {
		if key == "CRW_HOME" {
			return crwHome, true
		}
		return "", false
	}
}

func writePhase(t *testing.T, root string, phase state.Phase) {
	t.Helper()
	s := state.DefaultState(session, "work")
	s.Phase, s.OrchestrationActive = phase, phase != state.PhaseIdle
	if phase != state.PhaseIdle {
		epoch := "epoch-1"
		s.PlanEpoch = &epoch
	}
	if err := state.WriteState(root, s); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesOnlyAMoveAwayFromWorkInFlight(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(a, alias); err != nil {
		t.Fatal(err)
	}
	if c := Check(a, b, session); c != nil {
		t.Fatalf("no state at the root, yet %v", c)
	}
	writePhase(t, a, state.PhaseIdle)
	if c := Check(a, b, session); c != nil {
		t.Fatalf("an IDLE root, yet %v", c)
	}
	writePhase(t, a, state.PhaseP)
	c := Check(a, b, session)
	if c == nil || c.Phase != "P" || c.Unreadable || c.StatePath != state.StatePath(a, session) || c.NativeCwd != a || c.TargetCwd != b {
		t.Fatalf("conflict %+v", c)
	}
	if !strings.HasPrefix(c.Error(), Code+": ") || !strings.Contains(c.Error(), "phase P") {
		t.Errorf("message %q", c.Error())
	}
	for _, same := range []string{a, alias, a + "/"} {
		if c := Check(a, same, session); c != nil {
			t.Errorf("%s is the root, yet %v", same, c)
		}
	}
	for _, none := range [][2]string{{"", b}, {a, ""}} {
		if c := Check(none[0], none[1], session); c != nil {
			t.Errorf("%q: %v", none, c)
		}
	}
	// A state file that cannot be read cannot be told from one in flight.
	if err := os.WriteFile(state.StatePath(a, session), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := Check(a, b, session); c == nil || !c.Unreadable || !strings.Contains(c.Error(), "cannot be read") {
		t.Fatalf("unreadable: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Check wrote at the target: %v", err)
	}
}

// conflictOf is the *Conflict a Guard error carries, nil for none.
func conflictOf(err error) *Conflict {
	var c *Conflict
	if errors.As(err, &c) {
		return c
	}
	return nil
}

func anchorOf(t *testing.T, env host.LookupEnv) string {
	t.Helper()
	raw, err := os.ReadFile(AnchorPath(env, session))
	if err != nil {
		return ""
	}
	var got anchor
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	return got.NativeCwd
}

// The root a CRW resume path resolves is recorded before anything is sent, whether or not the
// thread has a state file yet, and it is the root the thread is on: a target the resume has not
// reached is not recorded by the check.
func TestGuardAnchorsTheNativeRootNotTheTarget(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	if Guard(env, a, b, session) != nil || anchorOf(t, env) != a {
		t.Fatalf("a thread without state anchored %q, want %q", anchorOf(t, env), a)
	}
	writePhase(t, a, state.PhaseIdle)
	if Guard(env, a, b, session) != nil || anchorOf(t, env) != a {
		t.Fatalf("a move with nothing in flight anchored %q before the host confirmed it", anchorOf(t, env))
	}
	writePhase(t, a, state.PhaseP)
	if c := Guard(env, a, b, session); c == nil || anchorOf(t, env) != a {
		t.Fatalf("a refused move: %v, anchor %q", c, anchorOf(t, env))
	}
	if Guard(env, a, "", session) != nil || anchorOf(t, env) != a {
		t.Fatalf("a resume that keeps the root anchored %q", anchorOf(t, env))
	}
	if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
		t.Fatalf("bootstrap at the other cwd: %+v", c)
	}
	if c := Bootstrap(envAt(t.TempDir()), b, session); c != nil {
		t.Fatalf("a session without an anchor: %v", c)
	}
}

// Review P1: a resume the host moved away from the anchored root (an external resume changed the
// cwd it reports) is judged against the anchor, not the cwd the host reports now, so Guard and the
// SessionStart bootstrap name the same root. The settings-free resume, which sends no cwd, is the
// same.
func TestGuardJudgesAgainstThePreservedAnchorWhateverTheHostReportsNow(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseP)
	if Guard(env, a, a, session) != nil {
		t.Fatal("the first resume at the root was refused")
	}
	for name, target := range map[string]string{"cwd sent": b, "settings-free": ""} {
		c := conflictOf(Guard(env, b, target, session))
		if c == nil || c.NativeCwd != a || c.TargetCwd != b {
			t.Errorf("%s: host cwd %s, anchor %s: conflict %+v", name, b, a, c)
		}
	}
	if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
		t.Errorf("Bootstrap names another root: %+v", c)
	}
	if anchorOf(t, env) != a {
		t.Errorf("the anchor moved to %q", anchorOf(t, env))
	}
	// A resume back at the anchored root is the root.
	if c := Guard(env, b, a, session); c != nil {
		t.Errorf("a resume back at the root: %v", c)
	}
}

// Review P1: a resume that fails after the check leaves the thread where it was, so the anchor
// stays at the work, and the SessionStart of the thread that did not move is still guarded.
func TestAFailedResumeDoesNotLoseTheNativeAnchor(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseIdle)
	if Guard(env, a, b, session) != nil {
		t.Fatal("a move with nothing in flight was refused")
	}
	// The resume was rejected: no Moved. The thread is still at a and goes on to work there.
	writePhase(t, a, state.PhaseP)
	if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
		t.Fatalf("the failed resume lost the native anchor: %+v (anchor %q)", c, anchorOf(t, env))
	}
}

// Review P1: the first CRW resume of a thread that has no state file yet still anchors it, so a
// later SessionStart elsewhere, after the work started at the root, is guarded.
func TestTheFirstResumeAnchorsBeforeSessionStartCreatesState(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	if Guard(env, a, a, session) != nil {
		t.Fatal("the first resume was refused")
	}
	if _, err := state.EnsureState(a, session); err != nil {
		t.Fatal(err)
	}
	writePhase(t, a, state.PhaseP)
	if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
		t.Fatalf("a thread resumed by CRW before it had state is unguarded: %+v", c)
	}
	// The standalone control: nothing resolved it, so there is no anchor and no guard.
	if c := Bootstrap(envAt(t.TempDir()), b, session); c != nil {
		t.Fatalf("a standalone session: %v", c)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// CRW-1140 criterion 2: a session whose source is bound to a linked worktree B keeps its state at
// its native root A. A resume at A goes ahead and the source identity is still B's; a resume at B
// is a move of the state, not of the source, and is refused while the work is in flight.
func TestALinkedSourceBindingKeepsTheStateAtTheNativeRoot(t *testing.T) {
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
	} {
		t.Setenv(name, value)
	}
	a, b := filepath.Join(base, "repo-A"), filepath.Join(base, "repo-B")
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, a, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(a, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, a, "add", "tracked.txt")
	gitIn(t, a, "commit", "-qm", "init")
	gitIn(t, a, "worktree", "add", "-q", "-b", "work", b)
	if _, err := sourcesession.Bind(a, session, b); err != nil {
		t.Fatal(err)
	}
	writePhase(t, a, state.PhaseP)
	before, err := os.ReadFile(state.StatePath(a, session))
	if err != nil {
		t.Fatal(err)
	}
	env := envAt(t.TempDir())
	if c := Guard(env, a, a, session); c != nil {
		t.Fatalf("a resume at the native root: %v", c)
	}
	if c := conflictOf(Guard(env, a, b, session)); c == nil || c.NativeCwd != a {
		t.Fatalf("a resume at the source worktree: %+v", c)
	}
	if c := Bootstrap(env, b, session); c == nil {
		t.Fatal("a bootstrap at the source worktree was not refused")
	}
	if c := Bootstrap(env, a, session); c != nil {
		t.Fatalf("a bootstrap at the native root: %v", c)
	}
	id, err := sourcesession.Capture(a, session, sourcesession.CaptureOptions{})
	if err != nil || id.SourceRoot == nil || *id.SourceRoot != b {
		t.Fatalf("source identity %+v (%v), want root %s", id, err, b)
	}
	if after, _ := os.ReadFile(state.StatePath(a, session)); string(after) != string(before) {
		t.Fatal("the native state changed")
	}
	if _, err := os.Stat(state.StatePath(b, session)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the source worktree got a state file: %v", err)
	}
}
