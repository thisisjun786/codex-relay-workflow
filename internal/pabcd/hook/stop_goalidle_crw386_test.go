package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-386 (the parent role skips the goal-idle block): the cases pin the role reader's effect on a Stop.

// stopGitEnv isolates git from the machine the way promptDcloseGitEnv does, with discovery stopping at ceiling.
func stopGitEnv(t *testing.T, ceiling string) {
	t.Helper()
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": ceiling,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(name, value)
	}
}

// stopMakeRepo turns dir into a git repository with one commit.
func stopMakeRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "initial"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// stopIdleRig is a goal-active IDLE session bound to the plan "export" with one pending work-phase. git says whether the
// workspace is a repository (the hermetic git environment stops discovery at the case's own directory either way).
func stopIdleRig(t *testing.T, git bool) (string, host.LookupEnv) {
	t.Helper()
	cwd, env := stopRig(t, "active")
	stopGitEnv(t, filepath.Dir(cwd))
	if git {
		stopMakeRepo(t, cwd)
	}
	sessionHookStateFile(t, cwd, stopSID, func(s *state.State) { s.Slug = "export" })
	stopWritePlan(t, cwd, "export", func(p *goalplan.Goalplan) {
		p.WorkPhases = []goalplan.GoalplanWorkPhase{stopWorkPhase("wp1", "Exporter", goalplan.WorkPhasePending)}
		p.Criteria = []goalplan.GoalplanCriterion{{ID: "c-1", Scenario: "s", ExpectedEvidence: "green", Status: goalplan.CriterionOpen}}
	})
	return cwd, env
}

// roleSpy is a role reader that records how many times it was asked and for which session.
type roleSpy struct {
	role  PromptRole
	calls int
	asked []string
}

func (r *roleSpy) read(cwd, sessionID string) PromptRole {
	r.calls++
	r.asked = append(r.asked, cwd+"|"+sessionID)
	return r.role
}

// CRW-386: a session the registry verifies as a project parent or a supervisor gets no goal-idle block at all, spends no
// budget and writes nothing, even with a bound goalplan and remaining work; a dispatched task, an unknown role and a session
// with no reader get the oracle's block, byte for byte.
func TestStopGoalIdleBlockIsSkippedForACoordinatingSeat(t *testing.T) {
	for _, git := range []bool{true, false} {
		oracleCwd, oracleEnv := stopIdleRig(t, git)
		oracle := stopBlockReason(t, stopRun(oracleCwd, oracleEnv))
		for _, c := range []struct {
			name string
			role PromptRole
			skip bool
		}{
			{"parent", PromptRoleParent, true},
			{"supervisor", PromptRoleSupervisor, true},
			{"dispatched task", PromptRoleTask, false},
			{"unknown", PromptRoleUnknown, false},
		} {
			cwd, env := stopIdleRig(t, git)
			before := stopStateBytes(t, cwd)
			spy := &roleSpy{role: c.role}
			a := StopHandleWithRole(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, spy.read)
			if spy.calls != 1 || len(spy.asked) != 1 || spy.asked[0] != cwd+"|"+stopSID {
				t.Errorf("git=%v %s: the reader is asked once for this session: %v", git, c.name, spy.asked)
			}
			if c.skip {
				if a != (StopAnswer{}) {
					t.Errorf("git=%v %s: want a release, got %+v", git, c.name, a)
				}
				if stopStateBytes(t, cwd) != before {
					t.Errorf("git=%v %s: the release wrote the state (it spent budget)", git, c.name)
				}
				continue
			}
			if got := stopBlockReason(t, a); got != oracle {
				t.Errorf("git=%v %s: not the oracle's block:\n got %q\nwant %q", git, c.name, got, oracle)
			}
		}
		// No reader at all is an unknown role.
		cwd, env := stopIdleRig(t, git)
		if got := stopBlockReason(t, StopHandleWithRole(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, nil)); got != oracle {
			t.Errorf("git=%v nil reader: %q", git, got)
		}
	}
}

// CRW-386: the role is read only when a goal-idle block would be answered; every earlier release keeps the registry unread, and
// a Stop with a cycle in flight is not the goal-idle block, so a parent's in-flight Stop is unchanged.
func TestStopReadsTheRoleOnlyForAGoalIdleBlock(t *testing.T) {
	cases := map[string]func(t *testing.T) (string, host.LookupEnv){
		"no goal": func(t *testing.T) (string, host.LookupEnv) {
			cwd, _ := stopIdleRig(t, true)
			_, env := stopRig(t, "")
			return cwd, env
		},
		"no bound plan": func(t *testing.T) (string, host.LookupEnv) {
			cwd, env := stopRig(t, "active")
			return cwd, env
		},
		"in flight": func(t *testing.T) (string, host.LookupEnv) {
			cwd, env := stopIdleRig(t, true)
			stopInFlight(t, cwd, state.PhaseB, func(s *state.State) { s.Slug = "export" })
			return cwd, env
		},
	}
	for name, setup := range cases {
		cwd, env := setup(t)
		spy := &roleSpy{role: PromptRoleParent}
		a := StopHandleWithRole(StopPayload{Cwd: cwd, SessionID: stopSID}, "linux", env, spy.read)
		if spy.calls != 0 {
			t.Errorf("%s: the registry was read %d times", name, spy.calls)
		}
		if name == "in flight" {
			if got := stopBlockReason(t, a); !strings.HasPrefix(got, "[crw — continue PABCD] You are mid-cycle at B") {
				t.Errorf("a parent's in-flight Stop must keep the continuation block: %q", got)
			}
		}
	}
}
