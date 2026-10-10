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
// file whose pid is gone; and no state read or write waits for either.

const sigkillID = "conc"

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
// and the next WithSessionLock is never blocked: it runs at once with no wait, writes the state, and leaves no lock behind.
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
			if row.phase != "" {
				got, unreadable := state.ReadStateStrict(cwd, sigkillID)
				if unreadable || got.Phase != row.phase {
					t.Fatalf("%s: state phase %q unreadable=%v, want %q", row.step, got.Phase, unreadable, row.phase)
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
			for _, name := range sigkillList(t, cwd) { // the hooks never sweep: only the lock is gone, the orphans stay
				if strings.HasSuffix(name, ".lock") {
					t.Fatalf("%s: %s is left", row.step, name)
				}
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
