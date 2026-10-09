package stateroot

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Resolve is the dry run's judgment: the anchor decides, and nothing is written.
func TestResolveJudgesAgainstTheAnchorAndWritesNothing(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseP)
	if Resolve(env, a, b, session) == nil {
		t.Fatal("a move away from work in flight was not refused")
	}
	if _, err := os.Stat(AnchorPath(env, session)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Resolve wrote an anchor: %v", err)
	}
	Guard(env, a, a, session)
	if Resolve(env, b, b, session) == nil {
		t.Fatal("Resolve ignored the anchor once the host reported another cwd")
	}
}

// The anchor follows the thread only once the host confirmed the resume: Moved records the target
// when nothing was in flight at the root the thread left, and never when it was.
func TestMovedRecordsTheTargetOnlyForAMoveThatWasAllowed(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	writePhase(t, a, state.PhaseIdle)
	Guard(env, a, b, session)
	if anchorOf(t, env) != a {
		t.Fatalf("anchor %q before the move", anchorOf(t, env))
	}
	Moved(env, a, b, session)
	if anchorOf(t, env) != b {
		t.Fatalf("anchor %q after the confirmed move, want %q", anchorOf(t, env), b)
	}
	// Work in flight at the new root: a later Moved to the old one is refused and records nothing.
	writePhase(t, b, state.PhaseP)
	Moved(env, b, a, session)
	if anchorOf(t, env) != b {
		t.Fatalf("a move away from work in flight moved the anchor to %q", anchorOf(t, env))
	}
	Moved(env, b, "", session)
	if anchorOf(t, env) != b {
		t.Fatalf("a resume that sent no cwd moved the anchor to %q", anchorOf(t, env))
	}
}

// A SessionStart bootstrap that went ahead at a new cwd re-anchors an anchored thread whose root
// held nothing in flight, and never moves an anchor off work in flight.
func TestBootstrappedFollowsAnAnchoredThreadOnlyOffAnIdleRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	env := envAt(t.TempDir())
	Bootstrapped(env, b, session)
	if anchorOf(t, env) != "" {
		t.Fatal("a standalone session got an anchor")
	}
	writePhase(t, a, state.PhaseIdle)
	Guard(env, a, a, session)
	Bootstrapped(env, b, session)
	if anchorOf(t, env) != b {
		t.Fatalf("anchor %q, want %q", anchorOf(t, env), b)
	}
	writePhase(t, b, state.PhaseP)
	Bootstrapped(env, a, session)
	if anchorOf(t, env) != b {
		t.Fatalf("anchor moved off work in flight to %q", anchorOf(t, env))
	}
}
