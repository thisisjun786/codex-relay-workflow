//go:build linux

package goalplan

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestWithGoalplanWriteLockUnreadableDirectoryIsUnreadable is CRW-975 (CRW-811 review): a plan directory
// that cannot be opened (EACCES) or a plan file that cannot be looked up in it is the "cannot be read"
// ReadGoalplan answers with nil, not a Go error. The oracle's orchestrate gate (orchestrate-cli.ts
// 606-622) goes on when the plan cannot be opened, so the lock reports unreadable and lets that caller
// decide.
func TestWithGoalplanWriteLockUnreadableDirectoryIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for name, mode := range map[string]os.FileMode{"no access": 0o000, "no search": 0o400} {
		t.Run(name, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			ran := false
			got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { ran = true; return 0, nil }, nil)
			if err != nil {
				t.Fatalf("an unreadable plan directory is an error, want Kind unreadable: %v", err)
			}
			if got.Kind != "unreadable" || got.Reason == "" || got.Refused || ran {
				t.Fatalf("got %+v ran=%v, want unreadable with a reason and no refusal", got, ran)
			}
			if ReadGoalplan(cwd, "demo") != nil {
				t.Fatal("ReadGoalplan was expected to answer nil for the same directory")
			}
		})
	}
}

// TestWithGoalplanWriteLockRevivalRefusalIsRefused marks the other unreadable: a plan the lock read but
// withholds from writers because a write would lose stored data. Its callers must not go on as if the plan
// were absent.
func TestWithGoalplanWriteLockRevivalRefusalIsRefused(t *testing.T) {
	cases := map[string]string{
		"repeated key": `{"objective":"dup",` + readTestPlan[1:],
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			writeReadFile(t, dir+"/"+GoalplanFile, body)
			ran := false
			got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { ran = true; return 0, nil }, nil)
			if err != nil || got.Kind != "unreadable" || !got.Refused || ran {
				t.Fatalf("got %+v ran=%v err=%v, want unreadable and Refused", got, ran, err)
			}
		})
	}
	t.Run("absent plan is not a refusal", func(t *testing.T) {
		cwd, dir := readWorkspace(t)
		if err := os.Remove(dir + "/" + GoalplanFile); err != nil {
			t.Fatal(err)
		}
		got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { return 0, nil }, nil)
		if err != nil || got.Kind != "unreadable" || got.Refused {
			t.Fatalf("got %+v err=%v, want unreadable without Refused", got, err)
		}
	})
}

// TestWithGoalplanWriteLockLinkedPlanDirectoryStaysAnError: only an access failure is the oracle's "cannot
// be read". A plan directory that is a symbolic link (found by the O_NOFOLLOW walk) or a path that is the
// wrong kind is a path-safety refusal and stays a Go error, as it was before the access failure was
// downgraded: the caller must not publish past it.
func TestWithGoalplanWriteLockLinkedPlanDirectoryStaysAnError(t *testing.T) {
	cwd, dir := readWorkspace(t)
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Fatal(err)
	}
	ran := false
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { ran = true; return 0, nil }, nil)
	if err == nil || ran || got.Kind == "ok" {
		t.Fatalf("a linked plan directory must be an error: got %+v err=%v ran=%v", got, err, ran)
	}
}

// TestWithGoalplanWriteLockPlanDirectoryNotADirectoryStaysAnError: a regular file where the plan directory
// belongs is the wrong kind of path, not an unreadable plan.
func TestWithGoalplanWriteLockPlanDirectoryNotADirectoryStaysAnError(t *testing.T) {
	cwd, dir := readWorkspace(t)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(dir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { return 0, nil }, nil)
	if err == nil || got.Kind == "ok" {
		t.Fatalf("a plan directory that is a file must be an error: got %+v err=%v", got, err)
	}
}

// TestWithGoalplanWriteLockUnrevivablePlanThatLosesTextIsRefused (CRW-975, evaluation of 50f1f3c2): a plan the
// reader returns no plan for is not always an absent or unreachable one. When the file holds text a write
// would lose (an unpaired surrogate escape) or a repeated key whose last value leaves the plan malformed,
// the lock must refuse it, as it does for a plan that revived, and not fall into the fail-open "unreadable".
func TestWithGoalplanWriteLockUnrevivablePlanThatLosesTextIsRefused(t *testing.T) {
	cases := map[string]string{
		"unpaired surrogate":                 strings.Replace(readTestPlan, `"o"`, `"\ud800"`, 1),
		"repeated key with a malformed last": `{"objective":"keep me",` + strings.Replace(readTestPlan, `"objective":"o"`, `"objective":null`, 1)[1:],
		"invalid UTF-8 in a malformed plan":  strings.Replace(readTestPlan, `"o"`, "null,\"x\":\"\xff\"", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cwd, dir := readWorkspace(t)
			writeReadFile(t, dir+"/"+GoalplanFile, body)
			if ReadGoalplan(cwd, "demo") != nil {
				t.Fatal("the fixture must be a plan the reader returns nothing for")
			}
			ran := false
			got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { ran = true; return 0, nil }, nil)
			if err != nil || got.Kind != "unreadable" || !got.Refused || ran {
				t.Fatalf("got %+v ran=%v err=%v, want unreadable and Refused", got, ran, err)
			}
		})
	}
	t.Run("an invalid JSON plan stays fail-open", func(t *testing.T) {
		cwd, dir := readWorkspace(t)
		writeReadFile(t, dir+"/"+GoalplanFile, `{"objective":`)
		got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { return 0, nil }, nil)
		if err != nil || got.Kind != "unreadable" || got.Refused {
			t.Fatalf("got %+v err=%v, want unreadable without Refused", got, err)
		}
	})
}

// TestWithGoalplanWriteLockSpecialPlanFileStaysAnError: a plan file that is a FIFO (or a link swapped in
// after the preliminary lookup) is a path-safety refusal, not an unreadable plan: the open walk found it
// and nothing may be published past it.
func TestWithGoalplanWriteLockSpecialPlanFileStaysAnError(t *testing.T) {
	cwd, dir := readWorkspace(t)
	file := dir + "/" + GoalplanFile
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Fatal(err)
	}
	ran := false
	got, err := WithGoalplanWriteLock(cwd, "demo", func(*Goalplan) (int, error) { ran = true; return 0, nil }, nil)
	if err == nil || ran || got.Kind == "ok" {
		t.Fatalf("a FIFO plan file must be an error: got %+v err=%v ran=%v", got, err, ran)
	}
}

// TestRevivalLossReadPlanLinkIsAPathSafetyFailure: the read after the lock's preliminary lookup opens the
// plan file with O_NOFOLLOW, so a link swapped in between is reported as the open failure the lock turns
// into a Go error (the lookup itself cannot be interleaved from a test).
func TestRevivalLossReadPlanLinkIsAPathSafetyFailure(t *testing.T) {
	cwd, dir := readWorkspace(t)
	file := dir + "/" + GoalplanFile
	moved := file + ".moved"
	if err := os.Rename(file, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, file); err != nil {
		t.Fatal(err)
	}
	parent, real, err := openPlanDir(cwd, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	read, lossFile := revivalLossReadPlan(parent, real, real+"/"+GoalplanFile, "demo")
	if read.Plan != nil || lossFile.openErr == nil || pathAbsent(lossFile.openErr) {
		t.Fatalf("a linked plan file must be an open failure that is not an absence: %+v %+v", read, lossFile)
	}
}
