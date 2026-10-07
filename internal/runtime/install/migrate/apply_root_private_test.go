package migrate

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// migrateRootPrivateUmask makes a mkdir's permission bits the ones the defect was found under, where 0777 becomes 0755,
// and restores the process umask so no other test of this package sees it. The umask is process-wide, and no test here
// runs in parallel.
func migrateRootPrivateUmask(t *testing.T) {
	t.Helper()
	old := unix.Umask(0o022)
	t.Cleanup(func() { unix.Umask(old) })
}

// migrateRootPrivateWorkspace is an isolated workspace with its project pair open, as EnsureProjectRoot's caller has it:
// the workspace exists and the destination root does not.
func migrateRootPrivateWorkspace(t *testing.T) (string, *Pair) {
	t.Helper()
	ws := isolate(t) + "/ws"
	mkdirs(t, ws)
	r, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = r.Close() })
	return ws, r.Project
}

// migrateRootPrivateFailRootCreation makes the publisher's destination-root creation fail after the mkdir: the root is
// on disk with the mode the mkdir left it, and the call returns EIO as the parent-directory sync in EnsureChild does, so
// the mode chmod EnsureProjectRoot runs next never happens. That is the state a run interrupted between the mkdir and
// the chmod leaves, and the state a rerun then finds as an existing root. The call reports made=false and no root, as the
// real EnsureChild does when its parent sync fails after a successful mkdir (roots.go:498-500).
func migrateRootPrivateFailRootCreation(p *Publisher) {
	create := p.ensureDest
	p.ensureDest = func(pair *Pair, perm uint32) (*Dir, bool, error) {
		if _, made, err := create(pair, perm); err != nil {
			return nil, made, err
		}
		return nil, false, unix.EIO
	}
}

// migrateRootPrivateWantPrivate fails when dir grants group or other any access, which is what "never wider than 0700"
// means: a 0644 file inside such a directory cannot be read by another user.
func migrateRootPrivateWantPrivate(t *testing.T, dir string) {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("%s is %v; a root that exists must never be wider than 0700", dir, perm)
	}
}

// The reported defect: the root's mkdir used 0777 (0755 under the umask) and the chmod that narrows it to the private
// marker mode comes after EnsureChild's parent-directory sync, so a failure there leaves a root another user can enter.
func TestMigrateRootPrivateAfterAFailedRootSync(t *testing.T) {
	migrateRootPrivateUmask(t)
	ws, pair := migrateRootPrivateWorkspace(t)
	p := newPub(t)
	migrateRootPrivateFailRootCreation(p)
	if root, made, err := p.EnsureProjectRoot(pair); root != nil || made || !errors.Is(err, unix.EIO) {
		t.Fatalf("EnsureProjectRoot = %v, %v, %v; want no root, no made and the interrupted creation", root, made, err)
	}
	migrateRootPrivateWantPrivate(t, filepath.Join(ws, crwdir.DirName))
}

// The rerun a person makes after that failure: it finds the root and copies state into it, so the root must still be at
// most 0700 once the run has finished. The source root here is 0755, so the destination root legitimately ends narrower
// than the source; the run reports that it kept the root's mode.
func TestMigrateRootPrivateRerunAfterAFailedRootSync(t *testing.T) {
	migrateRootPrivateUmask(t)
	ws, r, plan := apPlan(t, map[string]string{"ledger.jsonl": "{\"v\":1}"}, nil)
	pub := newPub(t)
	migrateRootPrivateFailRootCreation(pub)
	if _, err := applyWith(r, plan, pub); !errors.Is(err, unix.EIO) {
		t.Fatalf("the interrupted run: %v", err)
	}
	if _, err := os.Lstat(apDst(ws, "ledger.jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the interrupted run must not copy state: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(ws, ProjectSourceName)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("this case needs a 0755 source root: %v %v", fi, err)
	}
	// A rerun is a new process: its own roots and its own publisher.
	again, err := Open(Options{Scope: ScopeProject, Cwd: ws})
	must(t, err)
	t.Cleanup(func() { _ = again.Close() })
	plan2, err := classify(again)
	must(t, err)
	res, err := apply(again, plan2)
	must(t, err)
	migrateRootPrivateWantPrivate(t, apDst(ws, ""))
	if fi, err := os.Stat(apDst(ws, "ledger.jsonl")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the copied file keeps its source mode: %v %v", fi, err)
	}
	if note := apItem(t, res, ".").Note; !strings.Contains(note, "kept its mode") {
		t.Errorf("the rerun must report the root it kept, note = %q", note)
	}
}

// The control: without a failure the run's result is what it was. EnsureProjectRoot still finishes the root it made at
// the private marker mode and publishes the canonical .gitignore, and a run whose source root is private still ends with
// the destination root at that source mode.
func TestMigrateRootPrivateControls(t *testing.T) {
	migrateRootPrivateUmask(t)
	t.Run("EnsureProjectRoot without a failure", func(t *testing.T) {
		ws, pair := migrateRootPrivateWorkspace(t)
		root, made, err := newPub(t).EnsureProjectRoot(pair)
		must(t, err)
		if root == nil || !made {
			t.Fatalf("EnsureProjectRoot = %v, %v; want a root this call made", root, made)
		}
		fi, err := os.Stat(filepath.Join(ws, crwdir.DirName))
		if err != nil || fi.Mode().Perm() != applyTempMode || fi.Mode()&fs.ModeSticky == 0 {
			t.Errorf("the root must still be finished at the private marker mode: %v %v", fi, err)
		}
		ignore := filepath.Join(ws, crwdir.DirName, ".gitignore")
		if got := get(t, ignore); got != crwdir.GitignoreText {
			t.Errorf(".gitignore = %q", got)
		}
		if fi, err := os.Stat(ignore); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("the published .gitignore keeps its mode: %v %v", fi, err)
		}
	})
	t.Run("a completed run finishes the root at the source mode", func(t *testing.T) {
		ws, r, plan := apPlan(t, map[string]string{"ledger.jsonl": "{\"v\":1}"}, apPrivateSource(t, ""))
		if _, err := apply(r, plan); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(apDst(ws, ""))
		if err != nil || fi.Mode().Perm() != 0o700 || fi.Mode()&fs.ModeSticky != 0 {
			t.Errorf("the completed run must finish the root at the source mode: %v %v", fi, err)
		}
		if fi, err := os.Stat(apDst(ws, "ledger.jsonl")); err != nil || fi.Mode().Perm() != 0o644 {
			t.Errorf("the copied file keeps its source mode: %v %v", fi, err)
		}
	})
}

// The issue's first requirement, pinned at the seam itself: the mode the root's creation is asked for is 0o700, not
// 0o777. The behavioural cases above observe the root that is left behind; this one names the mode that decides it.
func TestMigrateRootPrivateRootCreationModeIsPrivate(t *testing.T) {
	migrateRootPrivateUmask(t)
	_, pair := migrateRootPrivateWorkspace(t)
	p := newPub(t)
	var asked []uint32
	create := p.ensureDest
	p.ensureDest = func(pair *Pair, perm uint32) (*Dir, bool, error) {
		asked = append(asked, perm)
		return create(pair, perm)
	}
	if root, made, err := p.EnsureProjectRoot(pair); err != nil || root == nil || !made {
		t.Fatalf("EnsureProjectRoot = %v, %v, %v", root, made, err)
	}
	if len(asked) != 1 || asked[0] != 0o700 {
		t.Errorf("the root's creation was asked for %v, want one 0o700", asked)
	}
}

// Every Publisher in the repository comes from NewPublisher, and EnsureProjectRoot reaches the destination root only
// through the seam, so a constructor that left it nil would panic there instead of creating a private root. The seam
// is otherwise unset by construction, which is the one way this field differs from rename and at.
func TestMigrateRootPrivateSeamDefaultIsArmed(t *testing.T) {
	migrateRootPrivateUmask(t)
	p := newPub(t)
	if p.ensureDest == nil {
		t.Fatal("NewPublisher left the destination-root creation seam nil")
	}
	_, pair := migrateRootPrivateWorkspace(t)
	root, made, err := p.ensureDest(pair, 0o700)
	must(t, err)
	if root == nil || !made || pair.Dest != root {
		t.Fatalf("the seam default must be Pair.EnsureDest: root = %v, made = %v, pair.Dest = %v", root, made, pair.Dest)
	}
	if fi, err := os.Stat(filepath.Join(pair.DestPath)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the default seam must create the root at the mode it was given: %v %v", fi, err)
	}
}

// The merged rule (CRW-813's made result beside this issue's mkdir mode): a root this call did not create reports
// made=false and is left at the mode it has. Here that is the 0700 root a failed creation left, so the rerun neither
// claims it as its own nor chmods it to the private marker, and the root stays at most 0700.
func TestMigrateRootPrivateExistingRootIsNotClaimedOrRetightened(t *testing.T) {
	migrateRootPrivateUmask(t)
	ws, pair := migrateRootPrivateWorkspace(t)
	mkdirs(t, filepath.Join(ws, crwdir.DirName))
	must(t, os.Chmod(filepath.Join(ws, crwdir.DirName), 0o700))
	root, made, err := newPub(t).EnsureProjectRoot(pair)
	must(t, err)
	if root == nil || made {
		t.Fatalf("EnsureProjectRoot = %v, %v; want the existing root and made=false", root, made)
	}
	migrateRootPrivateWantPrivate(t, filepath.Join(ws, crwdir.DirName))
	if got := get(t, filepath.Join(ws, crwdir.DirName, ".gitignore")); got != crwdir.GitignoreText {
		t.Errorf(".gitignore = %q", got)
	}
}

// The merged rule where it differs from the derivation this issue's parent used before the merge. The root is absent
// when the pair is opened, so 'made := pair.Dest == nil' would call the root this call's own and tighten it; a racer
// creates it in between, and the merged rule takes made from the creation's own result instead, so the racer's root
// keeps the mode it has. This case is red on the pre-merge derivation, which chmods it to the private marker.
func TestMigrateRootPrivateRacerRootKeepsItsMode(t *testing.T) {
	migrateRootPrivateUmask(t)
	ws, pair := migrateRootPrivateWorkspace(t)
	if pair.Dest != nil {
		t.Fatal("this case needs the destination root to be absent when the pair is opened")
	}
	// The racer creates the root after Open and before the call, so pair.Dest is still nil when it runs.
	mkdirs(t, filepath.Join(ws, crwdir.DirName))
	must(t, os.Chmod(filepath.Join(ws, crwdir.DirName), 0o755))
	root, made, err := newPub(t).EnsureProjectRoot(pair)
	must(t, err)
	if root == nil || made {
		t.Fatalf("EnsureProjectRoot = %v, %v; want the racer's root and made=false", root, made)
	}
	fi, err := os.Stat(filepath.Join(ws, crwdir.DirName))
	if err != nil || fi.Mode().Perm() != 0o755 || fi.Mode()&fs.ModeSticky != 0 {
		t.Errorf("a root this call did not create must keep its mode: %v %v", fi, err)
	}
	if got := get(t, filepath.Join(ws, crwdir.DirName, ".gitignore")); got != crwdir.GitignoreText {
		t.Errorf(".gitignore = %q", got)
	}
}
