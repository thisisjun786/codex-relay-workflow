package stateroot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Verification round 2: a thread that has no anchor yet is recorded whatever its root is, the
// process's own cwd included (an absent anchor is not the empty path, which resolves to that cwd).
func TestTheFirstAnchorIsRecordedAtTheProcessCwd(t *testing.T) {
	t.Run("Guard", func(t *testing.T) {
		a, b := t.TempDir(), t.TempDir()
		env := envAt(t.TempDir())
		t.Chdir(a)
		if Guard(env, a, a, session) != nil {
			t.Fatal("the first resume at the root was refused")
		}
		if anchorOf(t, env) != a {
			t.Fatalf("anchor %q, want %q", anchorOf(t, env), a)
		}
		Moved(env, a, a, session)
		writePhase(t, a, state.PhaseP)
		if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
			t.Fatalf("a SessionStart away from work in flight at the process cwd: %+v", c)
		}
	})
	t.Run("Moved", func(t *testing.T) {
		a := t.TempDir()
		env := envAt(t.TempDir())
		t.Chdir(a)
		// The host reported no cwd, so the check anchored nothing; the confirmed resume to the
		// process cwd is the thread's first root.
		if Guard(env, "", a, session) != nil {
			t.Fatal("the resume was refused")
		}
		Moved(env, "", a, session)
		if anchorOf(t, env) != a {
			t.Fatalf("anchor %q after the confirmed resume, want %q", anchorOf(t, env), a)
		}
	})
}

// Verification round 2: a resume whose anchor cannot be recorded is refused before anything is
// sent, since its SessionStart could not be guarded; an anchor that already names the root needs no
// write and goes ahead.
func TestGuardRefusesWhenTheAnchorCannotBeRecorded(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	home := filepath.Join(t.TempDir(), "crw-home")
	if err := os.WriteFile(home, []byte("a file, not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := envAt(home)
	if err := Guard(env, a, a, session); err == nil {
		writePhase(t, a, state.PhaseP)
		t.Fatalf("Guard went ahead without an anchor; Bootstrap at another cwd then gives %v", Bootstrap(env, b, session))
	} else if anchorErr := (*AnchorError)(nil); !errors.As(err, &anchorErr) || CodeOf(err) != AnchorCode ||
		!strings.HasPrefix(err.Error(), AnchorCode+": ") || !strings.Contains(err.Error(), a) || anchorErr.Path != AnchorPath(env, session) {
		t.Fatalf("refusal %v (code %s)", err, CodeOf(err))
	}
	// A conflict is still reported as the conflict it is.
	writePhase(t, a, state.PhaseP)
	if err := Guard(env, a, b, session); CodeOf(err) != Code || conflictOf(err) == nil {
		t.Fatalf("a refused move with an unwritable home: %v", err)
	}
	if err := os.Remove(state.StatePath(a, session)); err != nil {
		t.Fatal(err)
	}
	t.Run("an anchor already names the root", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		env := envAt(t.TempDir())
		if err := Guard(env, a, a, session); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(AnchorPath(env, session))
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if err := Guard(env, a, b, session); err != nil {
			t.Fatalf("a resume whose anchor is already recorded: %v", err)
		}
	})
}

// Verification round 2: a state file that already exists at the SessionStart cwd does not exempt it
// from the anchored root: a legacy IDLE state at B beside work in flight at A is refused, as the
// resume to B is, and names A.
func TestBootstrapJudgesAnExistingStateAgainstTheAnchor(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseIdle, state.PhaseP} {
		t.Run(string(phase), func(t *testing.T) {
			a, b := t.TempDir(), t.TempDir()
			env := envAt(t.TempDir())
			writePhase(t, a, state.PhaseP)
			if Guard(env, a, a, session) != nil {
				t.Fatal("the resume at the root was refused")
			}
			writePhase(t, b, phase)
			if Resolve(env, b, b, session) == nil {
				t.Fatal("the resume to B went ahead")
			}
			if c := Bootstrap(env, b, session); c == nil || c.NativeCwd != a {
				t.Fatalf("the bootstrap at B with a state there: %+v", c)
			}
		})
	}
}
