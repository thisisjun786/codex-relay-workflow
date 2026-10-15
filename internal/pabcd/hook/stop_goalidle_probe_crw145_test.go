package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-145 (post-evaluation round 3, d2 and d3): the source-identity investigation of a Stop owns the git it starts, and the
// block does not blame a missing source for a git that failed.

// processGone is whether pid no longer runs: it does not exist, or only waits to be reaped (a zombie).
func processGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}
	// "pid (comm) S ...": comm may hold spaces and parentheses, the state follows the last ")".
	rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
	return strings.HasPrefix(strings.TrimSpace(rest), "Z")
}

// A source check that runs out of the Stop's budget does not leave git, or the clean filter git started, running behind the hook
// process: the deadline is the investigation's own, and at it the whole process group of the git is killed and reaped before the
// Stop answers (d2: the budget bounded the wait only, so a filter that never returns outlived the hook).
func TestStopGoalIdleSourceCheckLeavesNoGitBehind(t *testing.T) {
	cwd, _ := stopIdleRig(t, true)
	pidFile := filepath.Join(t.TempDir(), "filter.pid")
	script := filepath.Join(t.TempDir(), "filter.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho $$ > '"+pidFile+"'\nexec sleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "filter.slow.clean", script}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(cwd, ".gitattributes"), []byte("tracked.txt filter=slow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A tracked file whose size is the index's and whose content and time are not: only the clean filter can tell status whether it
	// is modified (a different size would say so without it).
	if err := os.WriteFile(filepath.Join(cwd, "tracked.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := defaultStopSource()
	src.budget = 4 * time.Second
	t.Cleanup(func() { // never leave the filter behind, whatever the outcome
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	start := time.Now()
	got := stopCanArm(src, cwd, stopSID)
	took := time.Since(start)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("the clean filter did not start within the %v budget on this host: %v", src.budget, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("filter pid file %q: %v", b, err)
	}
	if !got.ok {
		t.Errorf("an investigation that outlasted the budget offers the oracle's arming option: %+v", got)
	}
	if took > src.budget+3*time.Second {
		t.Errorf("the Stop took %v with a stuck clean filter", took)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !processGone(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !processGone(pid) {
		t.Errorf("the clean filter (pid %d) still runs after the Stop's source check ended", pid)
	}
}

// d3: a git that cannot run is not a workspace without a git source. Binding a source tree runs the same git, so the block names
// the failure and does not offer the bind command a missing source would be given.
func TestStopGoalIdleBlockDoesNotBlameTheSourceForAFailedGit(t *testing.T) {
	cwd, env := stopIdleRig(t, false)
	t.Setenv("PATH", t.TempDir()) // no git on the hook's PATH
	got := stopBlockReason(t, stopRun(cwd, env))
	if strings.Contains(got, "crw relay session source") || strings.Contains(got, "orchestrate P --session") {
		t.Errorf("a failed git is offered binding or arming: %q", got)
	}
	if !strings.Contains(got, "cannot be armed") || !strings.Contains(got, "git could not read the source tree") {
		t.Errorf("the block does not name the git failure: %q", got)
	}
}
