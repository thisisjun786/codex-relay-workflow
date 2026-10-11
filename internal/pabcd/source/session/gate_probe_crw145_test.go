package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// CRW-145 (post-evaluation round 3, d2 and d3): the gate tells a git that could not answer from a directory in no repository, and
// the context-owned form leaves no git behind.

// fakeGit puts a git first on PATH that runs script (a /bin/sh body).
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func processGone(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return syscall.Kill(pid, 0) == syscall.ESRCH
	}
	rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
	return strings.HasPrefix(strings.TrimSpace(rest), "Z")
}

func TestCheckBoundTellsAFailedGitFromNoRepository(t *testing.T) {
	base := hermetic(t)
	cwd := filepath.Join(base, "plain")
	must(t, os.Mkdir(cwd, 0o755))
	for _, c := range []struct {
		name, script string
		noGit        bool
		want         GateCause
	}{
		{name: "git cannot be started", noGit: true, want: CauseProbeFailed},
		{name: "git fails", script: "echo 'fatal: detected dubious ownership' >&2; exit 128", want: CauseProbeFailed},
		{name: "git is killed", script: "kill -9 $$", want: CauseProbeFailed},
		// The probe's messages are asked for in the C locale, so a localized git still answers "no repository" in words the
		// classification reads; this git answers in German unless LC_ALL=C is set.
		{name: "no repository, localized git", script: `if [ "$LC_ALL" = C ]; then echo 'fatal: not a git repository (or any of the parent directories): .git' >&2; else echo 'fatal: kein Git-Repository' >&2; fi; exit 128`, want: CauseUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LC_ALL", "de_DE.UTF-8")
			if c.noGit {
				t.Setenv("PATH", t.TempDir())
			} else {
				fakeGit(t, c.script)
			}
			for name, got := range map[string]GateResult{"CheckBound": CheckBound(cwd, gateSession), "CheckBoundContext": CheckBoundContext(context.Background(), cwd, gateSession)} {
				if got.OK || got.Cause != c.want {
					t.Fatalf("%s: %+v, want cause %v", name, got, c.want)
				}
				if c.want == CauseProbeFailed && got.Probe == nil || c.want == CauseUnavailable && !source.NotARepository(got.Probe) {
					t.Errorf("%s: the probe failure is not carried: %v", name, got.Probe)
				}
				// The reason the command-line verbs print is the same in every case.
				if !strings.Contains(got.Reason, "no resolvable git source identity") {
					t.Errorf("%s: reason changed: %q", name, got.Reason)
				}
			}
		})
	}
}

// A real git in a directory in no repository is still a missing source, and one in a repository resolves, through the owned probe.
func TestCheckBoundContextOnRealGit(t *testing.T) {
	base := hermetic(t)
	plain := filepath.Join(base, "plain")
	must(t, os.Mkdir(plain, 0o755))
	if got := CheckBoundContext(context.Background(), plain, gateSession); got.OK || got.Cause != CauseUnavailable {
		t.Errorf("no repository: %+v", got)
	}
	repo := newRepo(t, base, "clean")
	if got := CheckBoundContext(context.Background(), repo, gateSession); !got.OK || got.Cause != CauseNone {
		t.Errorf("repository: %+v", got)
	}
}

// d2: the context owns the git. When it ends, the git and every process it started are killed and reaped before the call returns.
func TestCheckBoundContextLeavesNoGitBehind(t *testing.T) {
	base := hermetic(t)
	cwd := filepath.Join(base, "plain")
	must(t, os.Mkdir(cwd, 0o755))
	pids := filepath.Join(t.TempDir(), "pids")
	fakeGit(t, "echo $$ > '"+pids+"'; sleep 300 & echo $! >> '"+pids+"'; wait")
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	got := CheckBoundContext(ctx, cwd, gateSession)
	took := time.Since(start)
	b, err := os.ReadFile(pids)
	if err != nil {
		t.Skipf("the fake git did not start within the deadline: %v", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		t.Skipf("the fake git was cut short: %q", b)
	}
	t.Cleanup(func() {
		for _, f := range fields {
			if pid, err := strconv.Atoi(f); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	if got.OK || got.Cause != CauseProbeFailed || !errors.Is(got.Probe, context.DeadlineExceeded) {
		t.Errorf("a probe cut off by its context is a failed probe carrying the deadline: %+v", got)
	}
	if took > 10*time.Second {
		t.Errorf("the call took %v past a %v deadline", took, 700*time.Millisecond)
	}
	for _, f := range fields {
		pid, _ := strconv.Atoi(f)
		deadline := time.Now().Add(3 * time.Second)
		for !processGone(pid) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !processGone(pid) {
			t.Errorf("process %d of the probe still runs after CheckBoundContext returned", pid)
		}
	}
}
