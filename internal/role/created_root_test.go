package role

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// CRW-1119 (CRW-1136 disposition d2): the created check lists and opens the native thread database under one resolved root.
// A CODEX_SQLITE_HOME that ends in <symlink>/.. names the directory the kernel resolves it to; the check read the listing of that
// directory but opened the lexically cleaned path, which here holds a database without the child, and refused a real child.
func TestCreatedCheckNativeReadsTheDatabaseOfThePhysicalRoot(t *testing.T) {
	ws, env, start, _ := dispatchTestFixture(t)
	base := t.TempDir()
	physical := filepath.Join(base, "physical")
	check(t, os.MkdirAll(filepath.Join(physical, "deep"), 0o700))
	lexical := filepath.Join(base, "lexical")
	check(t, os.MkdirAll(lexical, 0o700))
	check(t, os.Symlink(filepath.Join(physical, "deep"), filepath.Join(lexical, "link")))
	createdCheckSeed(t, physical, "child-a", "session-test")
	createdCheckSeed(t, lexical, "someone-else", "other-session") // the decoy the cleaned path names
	spelled := filepath.Join(lexical, "link") + "/.."
	old := env
	env = func(k string) (string, bool) {
		if k == "CODEX_SQLITE_HOME" {
			return spelled, true
		}
		return old(k)
	}
	dispatchTestClaimIssued(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
	check(t, err)
	if out.Action != "wait" {
		t.Fatalf("created of the physical root's child = %+v", out)
	}
}

// A relative database root is refused instead of being read below the working directory.
func TestCreatedCheckNativeRefusesARelativeRoot(t *testing.T) {
	ws, env, start, _ := dispatchTestFixture(t)
	wd := t.TempDir()
	t.Chdir(wd)
	check(t, os.Mkdir(filepath.Join(wd, "native"), 0o700))
	createdCheckSeed(t, filepath.Join(wd, "native"), "child-a", "session-test")
	old := env
	env = func(k string) (string, bool) {
		if k == "CODEX_SQLITE_HOME" {
			return "native", true
		}
		return old(k)
	}
	dispatchTestClaimIssued(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	if _, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil); err == nil {
		t.Fatal("a relative database root was read")
	}
}
