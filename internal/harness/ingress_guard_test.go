package harness

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
)

func TestGuardInputFailuresDenyBeforeEffects(t *testing.T) {
	for _, id := range []string{
		"pre-tool-use-guarding-managed-worktree-deletion",
		"pre-tool-use-guarding-memory-write",
		"pre-tool-use-guarding-automation-ownership",
		"pre-tool-use-guarding-goal-complete",
	} {
		for _, renamed := range []bool{false, true} {
			for _, failure := range []string{"partial EIO", "directory fd", "oversized"} {
				t.Run(id+"/"+failure+map[bool]string{true: "/renamed"}[renamed], func(t *testing.T) {
					env, home, _ := hookEnv(t)
					ran := false
					legs := only(id, func(Call) string { ran = true; return "" })
					if renamed {
						legs[0].Slug = "renamed-protection"
					}
					var in io.Reader
					switch failure {
					case "partial EIO":
						in = io.MultiReader(strings.NewReader(payload("PreToolUse", home, "")), iotest.ErrReader(syscall.EIO))
					case "directory fd":
						f, err := os.Open(t.TempDir())
						if err != nil {
							t.Fatal(err)
						}
						defer f.Close()
						in = f
					case "oversized":
						in = strings.NewReader(strings.Repeat("x", MaxStdinBytes+1))
					}
					var out, errOut strings.Builder
					code := Hook(context.Background(), argsOf(legs[0]), in, &out, &errOut, lookup(env), legs)
					var answer struct {
						HookSpecificOutput struct{ HookEventName, PermissionDecision string }
					}
					err := json.Unmarshal([]byte(out.String()), &answer)
					if code != 0 || err != nil || answer.HookSpecificOutput.HookEventName != "PreToolUse" || answer.HookSpecificOutput.PermissionDecision != "deny" || ran || len(records(t, home)) != 0 || errOut.Len() != 0 {
						t.Fatalf("code=%d answer=%q error=%v handler=%v records=%d stderr=%q", code, out.String(), err, ran, len(records(t, home)), errOut.String())
					}
				})
			}
		}
	}
}

func TestAdvisoryInputFailuresReleaseBeforeEffects(t *testing.T) {
	for _, id := range []string{"session-start-bootstrapping-pabcd-state", "pre-tool-use-linting-apply-patch", "permission-request-allowing-agent-thread"} {
		for _, fail := range []bool{false, true} {
			ran := false
			legs := only(id, func(Call) string { ran = true; return "unexpected" })
			var in io.Reader = strings.NewReader(strings.Repeat("x", MaxStdinBytes+1))
			if fail {
				in = iotest.ErrReader(syscall.EIO)
			}
			var out, errOut strings.Builder
			if code := Hook(context.Background(), argsOf(legs[0]), in, &out, &errOut, lookup(map[string]string{}), legs); code != 0 || out.Len() != 0 || ran {
				t.Errorf("%s readError=%v: code=%d out=%q handler=%v", id, fail, code, out.String(), ran)
			}
		}
	}
}
