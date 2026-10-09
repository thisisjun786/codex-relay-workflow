package migrate

// apply_owned_dir_identity_followup_test.go holds the CRW-987 cases for the owned-directory identity work of CRW-887: a
// same-Pair retry whose parent sync failed twice, and the by-name chmod that a denied descriptor open takes. Each case
// names the behaviour it pins.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-987 d1: a same-Pair retry whose parent sync failed twice must not give the displaced root its files. The second
// attempt reopens the root it created and then fails its sync; the root is moved aside and another 0700 directory takes
// the name; the third attempt applies with the same pair. The files and the final mode step land in the replacement,
// which is reported as kept, and the displaced directory receives nothing. The head before this cycle pinned the reopened
// root before its sync succeeded, so the third attempt reused that handle and wrote into the displaced directory.
func TestMigrateFollowupSecondSyncFailureDoesNotPinTheRoot(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	root := apDst(ws, "")
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs <= 2 {
				return errApplyInterrupted
			}
		}
		return nil
	})
	for i := range 2 {
		if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	must(t, os.Rename(root, root+".ours"))
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, 0o700)
	clearAt()
	res, err := apply(r, p)
	must(t, err)
	if _, err := os.Lstat(filepath.Join(root+".ours", "sessions")); err == nil {
		t.Errorf("the displaced root received the files of the retry")
	}
	if _, err := os.Lstat(filepath.Join(root, "sessions", "a.json")); err != nil {
		t.Errorf("the replacement must hold the files: %v", err)
	}
	migrateOwnedDirIdentityWantRaw(t, root, 0o700)
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
	if ai := apItem(t, res, "."); !strings.Contains(ai.Note, "rwx------") {
		t.Errorf("the kept-mode note must name the replacement's mode, note = %q", ai.Note)
	}
}

// CRW-987 d1 control: when no replacement happens, two failed parent syncs and a clean third attempt still finish the
// root this run made at its source mode, with no kept-mode note.
func TestMigrateFollowupSecondSyncRetryFinishesTheRootWithoutAReplacement(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	syncs := 0
	clearAt := migrateOwnedDirIdentitySteps(t, func(step string) error {
		if step == "sync" {
			if syncs++; syncs <= 2 {
				return errApplyInterrupted
			}
		}
		return nil
	})
	for i := range 2 {
		if _, err := apply(r, p); !errors.Is(err, errApplyInterrupted) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	clearAt()
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
	migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
	if ai := apItem(t, res, "."); strings.Contains(ai.Note, "kept its mode") {
		t.Errorf("the root this run made must not be reported as kept, note = %q", ai.Note)
	}
}

// CRW-987 d3: a descriptor open that the umask denies (Darwin O_SEARCH answering EACCES or EPERM) takes the checked by-name
// chmod, which is a supported path, and the directory still ends at its requested mode with no temporary behind.
func TestMigrateFollowupDeniedPinTakesTheByNameFallback(t *testing.T) {
	restore := migrateOwnedDirIdentityPin
	t.Cleanup(func() { migrateOwnedDirIdentityPin = restore })
	for _, errno := range []error{unix.EACCES, unix.EPERM} {
		migrateOwnedDirIdentityPin = func(int, string) (int, error) { return -1, errno }
		ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
		if _, err := apply(r, p); err != nil {
			t.Fatalf("a denied open (%v) must take the by-name path and finish: %v", errno, err)
		}
		migrateOwnedDirIdentityWantRaw(t, apDst(ws, ""), 0o755)
		migrateOwnedDirIdentityWantRaw(t, apDst(ws, "sessions"), 0o755)
		migrateOwnedDirIdentityWantNoTemp(t, apDst(ws, ""))
	}
}

// CRW-987 d3: the defect record must not call the by-name fallback unreachable on macOS. It states the O_SEARCH success
// condition, the fallback and its replacement window, and every paragraph about macOS agrees with the code.
func TestMigrateFollowupDefectRecordDoesNotCallTheFallbackUnreachable(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "port-cxc", "known-defects", "CRW-887.md"))
	must(t, err)
	if !strings.Contains(string(body), "O_SEARCH") {
		t.Fatal("the record must state the O_SEARCH success condition")
	}
	for _, para := range strings.Split(string(body), "\n\n") {
		flat := strings.ToLower(strings.Join(strings.Fields(para), " "))
		if strings.Contains(flat, "macos") && strings.Contains(flat, "by-name") &&
			(strings.Contains(flat, "unreachable") || strings.Contains(flat, "no longer reachable") || strings.Contains(flat, "not reachable")) {
			t.Errorf("the record calls the by-name fallback unreachable on macOS: %q", flat)
		}
	}
}

// CRW-987 d1 (review): a root that existed when the run was opened, and that another directory has held since, is not adopted
// through its marker mode. The replacement keeps its mode and is reported; the displaced directory receives nothing.
func TestMigrateFollowupReplacedExistingRootIsNotAdopted(t *testing.T) {
	ws, _, _ := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	root := apDst(ws, "")
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, 0o700)
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	p, err := classify(r)
	must(t, err)
	must(t, os.Rename(root, root+".ours"))
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, applyTempRaw)
	res, err := apply(r, p)
	must(t, err)
	migrateOwnedDirIdentityWantRaw(t, root, applyTempRaw)
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
	if _, err := os.Lstat(filepath.Join(root+".ours", "sessions")); err == nil {
		t.Errorf("the displaced root received the files of the run")
	}
}

// CRW-987 d2 (review): when the pinned project root is replaced, the .gitignore judgement is made for the directory the run
// publishes into. The replacement's own .gitignore is the owner's, so it is kept and the run does not stop with a refusal.
func TestMigrateFollowupReplacedProjectRootKeepsItsOwnGitignore(t *testing.T) {
	ws, r, p := apPlan(t, migrateOwnedDirIdentityEntries(), nil)
	root := apDst(ws, "")
	pub := newPub(t)
	pub.at = func(step string) error {
		if step == "root" {
			return errApplyInterrupted
		}
		return nil
	}
	if _, err := applyWith(r, p, pub); !errors.Is(err, errApplyInterrupted) {
		t.Fatalf("the interrupted run: %v", err)
	}
	must(t, os.Rename(root, root+".ours"))
	mkdirs(t, root)
	migrateOwnedDirIdentitySetRaw(t, root, 0o700)
	must(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("mine\n"), 0o644))
	res, err := apply(r, p)
	must(t, err)
	got, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	must(t, err)
	if string(got) != "mine\n" {
		t.Errorf("the replacement's .gitignore must be kept as its owner wrote it, got %q", got)
	}
	if _, err := os.Lstat(filepath.Join(root+".ours", "sessions")); err == nil {
		t.Errorf("the displaced root received the files of the run")
	}
	migrateOwnedDirIdentityWantKeptNote(t, res, ".")
}
