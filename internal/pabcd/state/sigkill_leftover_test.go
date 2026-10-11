package state_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/reset"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1255: what a process killed by SIGKILL at each step of the session state writes leaves in the sessions directory, whether the
// next lock acquirer is ever blocked by it, and which explicit command removes it. The allowed leftover (known-defects/CRW-1255.md):
// the owner process is gone; the next WithSessionLock takes a <key>.json.lock over, or `crw pabcd reset --state` removes a temp
// file whose pid is gone; and no state read or write waits for either. The two in-place rows are outside that definition: they
// stand for a filesystem with neither hard links nor a no-replace rename, on which a fresh WithSessionLock cannot create its
// lock either (placeSessionLock), so the tests run no acquirer for them (the test filesystem has both calls and an acquirer there
// would change the contract the row stands for); reset is their only recorded repair.

const sigkillID = "conc"

// sigkillInPlace reports the rows of the in-place fallback of ensureState, whose filesystem cannot take a fresh session lock.
func sigkillInPlace(step string) bool { return strings.HasPrefix(step, "ensure-inplace-") }

// sigkillUnreadable is the table's phase for a state file that a strict read judges unreadable (an empty file).
const sigkillUnreadable state.Phase = "unreadable"

// TestSigkillLeftoverChild is the child of the tests below: it runs one step and kills itself.
func TestSigkillLeftoverChild(t *testing.T) {
	step, cwd := os.Getenv("CRW_SIGKILL_STEP"), os.Getenv("CRW_SIGKILL_CWD")
	if step == "" {
		t.Skip("child of the SIGKILL leftover tests")
	}
	state.SigkillChild(step, cwd)
}

// sigkillLeftovers is the table: the step, the steps that run first to leave a dead owner's lock (the takeover), the names the
// killed process leaves with its pid and uuid replaced, and the phase the state file holds (empty: there is no state file).
var sigkillLeftovers = []struct {
	step   string
	before string
	names  []string
	phase  state.Phase
}{
	{"lock-stage-record", "", []string{"conc.json", "conc.json.lock.<pid>.<uuid>.tmp"}, state.PhaseB},
	{"lock-before-publish", "", []string{"conc.json", "conc.json.lock.<pid>.<uuid>.tmp"}, state.PhaseB},
	{"lock-held", "", []string{"conc.json", "conc.json.lock"}, state.PhaseB},
	{"lock-takeover-record", "lock-held", []string{"conc.json", "conc.json.lock", "conc.json.lock.<pid>.<uuid>.tmp"}, state.PhaseB},
	{"write-before-sync", "", []string{"conc.json", "conc.json.<pid>.<uuid>.tmp", "conc.json.lock"}, state.PhaseB},
	{"write-before-rename", "", []string{"conc.json", "conc.json.<pid>.<uuid>.tmp", "conc.json.lock"}, state.PhaseB},
	{"write-after-rename", "", []string{"conc.json", "conc.json.lock"}, state.PhaseC},
	{"ensure-before-link", "", []string{"conc.json.<pid>.<uuid>.tmp"}, ""},
	{"ensure-after-link", "", []string{"conc.json", "conc.json.<pid>.<uuid>.tmp"}, state.PhaseIdle},
	{"ensure-fallback-sync", "", []string{"conc.json.<pid>.<uuid>.tmp", "conc.json.<pid>.<uuid>.tmp"}, ""},
	{"ensure-fallback-publish", "", []string{"conc.json.<pid>.<uuid>.tmp", "conc.json.<pid>.<uuid>.tmp"}, ""},
	// the one write whose final path is not atomic (the best-effort limit of write.go publishWithoutLink): the file is created, then written
	{"ensure-inplace-write", "", []string{"conc.json", "conc.json.<pid>.<uuid>.tmp"}, sigkillUnreadable},
	{"ensure-inplace-sync", "", []string{"conc.json", "conc.json.<pid>.<uuid>.tmp"}, state.PhaseIdle},
}

var sigkillPidUUID = regexp.MustCompile(`\.[0-9]+\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.tmp$`)

func sigkillKilled(t *testing.T, step, cwd string) (pid int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSigkillLeftoverChild$")
	cmd.Env = append(os.Environ(), "CRW_SIGKILL_STEP="+step, "CRW_SIGKILL_CWD="+cwd)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("%s: the child was not killed: %v", step, err)
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("%s: the child ended without reaching its kill point: %v", step, exit)
	}
	return cmd.Process.Pid
}

func sigkillList(t *testing.T, cwd string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// sigkillScene builds the sessions directory with the step's predecessors and the killed process, and returns the pid of the last.
func sigkillScene(t *testing.T, i int) (cwd string, pid int) {
	t.Helper()
	row := sigkillLeftovers[i]
	cwd = t.TempDir()
	if strings.HasPrefix(row.step, "lock-") || strings.HasPrefix(row.step, "write-") {
		prev := state.DefaultState(sigkillID, "previous")
		prev.Phase = state.PhaseB
		if err := state.WriteState(cwd, prev); err != nil {
			t.Fatal(err)
		}
	}
	if row.before != "" {
		sigkillKilled(t, row.before, cwd)
	}
	pid = sigkillKilled(t, row.step, cwd)
	return cwd, pid
}

func TestSigkillLeftoverTableCoversEveryStep(t *testing.T) {
	var steps []string
	for _, row := range sigkillLeftovers {
		steps = append(steps, row.step)
	}
	if !slices.Equal(steps, state.SigkillSteps) {
		t.Fatalf("table %v, child steps %v", steps, state.SigkillSteps)
	}
}

// Each step leaves the names of the table, every temp file of them names the killed pid and is an orphan, the state file is whole,
// and (outside the two in-place rows) the next WithSessionLock is never blocked: it runs at once with no wait, writes the state, and leaves no lock behind.
func TestSigkillLeftoverNamesAndNextAcquirer(t *testing.T) {
	for i, row := range sigkillLeftovers {
		t.Run(row.step, func(t *testing.T) {
			cwd, pid := sigkillScene(t, i)
			var normal []string
			for _, name := range sigkillList(t, cwd) {
				if strings.HasSuffix(name, ".tmp") {
					if !strings.Contains(name, "."+strconv.Itoa(pid)+".") || !state.OrphanStateTemp(name) {
						t.Errorf("%s: %s does not name the killed pid %d or is not an orphan", row.step, name, pid)
					}
				}
				normal = append(normal, sigkillPidUUID.ReplaceAllString(name, ".<pid>.<uuid>.tmp"))
			}
			if !slices.Equal(normal, row.names) { // ReadDir sorts by name
				t.Fatalf("%s leaves %v, the table says %v", row.step, normal, row.names)
			}
			if row.phase == sigkillUnreadable {
				if _, unreadable := state.ReadStateStrict(cwd, sigkillID); !unreadable {
					t.Fatalf("%s: the state file reads as whole, the table says unreadable", row.step)
				}
			} else if row.phase != "" {
				got, unreadable := state.ReadStateStrict(cwd, sigkillID)
				if unreadable || got.Phase != row.phase {
					t.Fatalf("%s: state phase %q unreadable=%v, want %q", row.step, got.Phase, unreadable, row.phase)
				}
			}
			if sigkillInPlace(row.step) {
				return // no acquirer on the filesystem this row stands for; see the comment at the top of the file
			}
			var orphans []string // the real temp names before the next acquirer
			for _, name := range sigkillList(t, cwd) {
				if strings.HasSuffix(name, ".tmp") {
					orphans = append(orphans, name)
				}
			}
			slept := 0
			previous := state.SigkillSleepSeam(func(time.Duration) { slept++ })
			defer previous()
			entered := false
			err := state.WithSessionLock(cwd, sigkillID, func() error {
				entered = true
				next := state.ReadState(cwd, sigkillID)
				next.Phase = state.PhaseP
				next.SessionID = sigkillID
				return state.WriteState(cwd, next)
			})
			if err != nil || !entered || slept != 0 {
				t.Fatalf("%s: the next acquirer: err %v entered %v waited %d times", row.step, err, entered, slept)
			}
			if got, unreadable := state.ReadStateStrict(cwd, sigkillID); unreadable || got.Phase != state.PhaseP {
				t.Fatalf("%s: after the acquirer the state is %q unreadable=%v", row.step, got.Phase, unreadable)
			}
			if _, err := os.Lstat(state.SessionLockPath(cwd, sigkillID)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s: the lock is still there after the next acquirer released: %v", row.step, err)
			}
			var after []string
			for _, name := range sigkillList(t, cwd) { // the hooks never sweep: only the lock is gone, the orphans stay
				if strings.HasSuffix(name, ".lock") {
					t.Fatalf("%s: %s is left", row.step, name)
				}
				if strings.HasSuffix(name, ".tmp") {
					after = append(after, name)
				}
			}
			if !slices.Equal(after, orphans) {
				t.Fatalf("%s: the temp files before the next acquirer %v, after it %v; the hooks never sweep", row.step, orphans, after)
			}
		})
	}
}

// The explicit maintenance command, `crw pabcd reset --state`, removes every temp file of a killed writer and the state file with
// it; the only name it leaves is a dead owner's <key>.json.lock, which the next acquirer takes over and removes on release.
func TestSigkillLeftoverIsRemovedByResetAndTheLockByTheNextAcquirer(t *testing.T) {
	for i, row := range sigkillLeftovers {
		t.Run(row.step, func(t *testing.T) {
			cwd, _ := sigkillScene(t, i)
			result, err := reset.RunReset(cwd, reset.State)
			if err != nil {
				t.Fatal(err)
			}
			tmps := 0
			for _, name := range row.names {
				if strings.HasSuffix(name, ".tmp") {
					tmps++
				}
			}
			removedTmp := 0
			for _, path := range result.Removed {
				if strings.HasSuffix(path, ".tmp") {
					removedTmp++
				}
			}
			if removedTmp != tmps {
				t.Fatalf("%s: reset removed %d temp files %v, the table has %d", row.step, removedTmp, result.Removed, tmps)
			}
			want := []string{}
			if slices.Contains(row.names, "conc.json.lock") {
				want = []string{"conc.json.lock"}
			}
			if left := sigkillList(t, cwd); !slices.Equal(append([]string{}, left...), want) {
				t.Fatalf("%s: reset left %v, want %v (only a dead owner's lock)", row.step, left, want)
			}
			if sigkillInPlace(row.step) {
				return // no acquirer on the filesystem this row stands for; see the comment at the top of the file
			}
			entered := false
			if err := state.WithSessionLock(cwd, sigkillID, func() error { entered = true; return nil }); err != nil || !entered {
				t.Fatalf("%s: the acquirer after reset: err %v entered %v", row.step, err, entered)
			}
			if names := sigkillList(t, cwd); len(names) != 0 {
				t.Fatalf("%s: the directory is not clean after reset and one acquirer: %v", row.step, names)
			}
		})
	}
}

// The in-place fallback leaves a state file that is not whole (the best-effort limit): no later EnsureState repairs it and a strict
// read keeps calling it unreadable, and `crw pabcd reset --state` removes it with the temp file. Nothing here takes a session lock:
// on the filesystem the row stands for a fresh lock cannot be created, so a lock holder's write is not a repair.
func TestSigkillLeftoverInPlaceStateFileIsNotRepairedByEnsureButByReset(t *testing.T) {
	for i, row := range sigkillLeftovers {
		if row.step != "ensure-inplace-write" {
			continue
		}
		cwd, _ := sigkillScene(t, i)
		if raw, err := os.ReadFile(state.StatePath(cwd, sigkillID)); err != nil || len(raw) != 0 {
			t.Fatalf("the killed in-place create left %d bytes, err %v; want an empty file", len(raw), err)
		}
		if created, err := state.EnsureState(cwd, sigkillID); created || err != nil {
			t.Fatalf("EnsureState over the empty file: created %v err %v", created, err)
		}
		if _, unreadable := state.ReadStateStrict(cwd, sigkillID); !unreadable {
			t.Fatal("EnsureState repaired the empty state file; the allowed leftover and the table say it does not")
		}
		result, err := reset.RunReset(cwd, reset.State)
		if err != nil || len(result.Removed) != 2 {
			t.Fatalf("reset: removed %v err %v, want the state file and the temp file", result.Removed, err)
		}
		if names := sigkillList(t, cwd); len(names) != 0 {
			t.Fatalf("reset left %v", names)
		}
	}
}
