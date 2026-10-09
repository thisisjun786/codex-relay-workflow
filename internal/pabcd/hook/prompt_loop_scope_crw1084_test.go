package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1084 (port: fixed): a loop-arm request that names a project is a scope choice, not the implementation recipe. Each case
// pins the emitting owner (the recipe LoopArmDirective, the scope pointer LoopScopeDirective, or nothing), the commands the
// emission carries and the state it leaves.

func loopScopeRun(t *testing.T, prompt string, role PromptRole) (answer string, s state.State) {
	t.Helper()
	cwd := t.TempDir()
	seams := &promptDcloseSeams{role: func(string, string) PromptRole { return role }}
	answer = promptSubmitHandleWith(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: prompt, TurnID: "t1", PabcdEnabled: true},
		"linux", promptSubmitHost(cwd), state.WithSessionLock, seams)
	return answer, state.ReadState(cwd, "s1")
}

const (
	loopScopePointerMarker = "scope choice (ORCH-MANDATE-01)"
	loopScopeRecipeMarker  = "orchestrate arming mandate (ORCH-MANDATE-01)"
)

func TestLoopArmScopeCases(t *testing.T) {
	const linearProject = "https://linear.app/acme/project/payments-1a2b3c4d5e6f"
	cases := []struct {
		name, prompt string
		role         PromptRole
		owner        string // "recipe", "pointer" or "none"
	}{
		// The recorded fixture's request: a project, no role evidence.
		{"new project coordination", "Start crw-loop for the migration project.", PromptRoleUnknown, "pointer"},
		{"a Linear project link", "Run crw-loop on " + linearProject, PromptRoleUnknown, "pointer"},
		{"coordination words", "crw-loop 돌려서 프로젝트 조정해줘", PromptRoleUnknown, "pointer"},
		{"an already bound parent, whatever it asks", "Run crw-loop for this task", PromptRoleParent, "pointer"},
		{"a single-task loop", "Run crw-loop for this task", PromptRoleUnknown, "recipe"},
		{"a single-task loop in Korean", "이 유닛 crw-loop로 알아서 끝까지 해줘", PromptRoleUnknown, "recipe"},
		{"a dispatched task that names the project", "Start crw-loop for the migration project.", PromptRoleTask, "recipe"},
		{"an explicit current-task implementation under a project link", "Use crw-loop to implement CRW-12 in this session; project " + linearProject, PromptRoleUnknown, "recipe"},
		{"an explicit current-task implementation for a registered parent", "crw-loop로 이 세션에서 구현해줘, 프로젝트 CRW", PromptRoleParent, "recipe"},
		{"plan-only", "Use crw-loop, plan only", PromptRoleUnknown, "recipe"},
		{"plan-only for a project", "Use crw-loop for the migration project, plan only", PromptRoleUnknown, "pointer"},
		{"no-goal no-FSM", "Run crw-loop for this task, no-goal and no-FSM", PromptRoleUnknown, "recipe"},
		{"a read-only explanation", "Explain how crw-loop works for a project", PromptRoleUnknown, "none"},
		{"a negation", "Do not run crw-loop for the project", PromptRoleUnknown, "none"},
		{"a backtick example", "Here is an example: `crw-loop` for the project.", PromptRoleUnknown, "none"},
		{"a backtick fence", "```\nRun crw-loop for the migration project.\n```", PromptRoleUnknown, "none"},
		{"a tilde fence", "~~~\nRun crw-loop for the migration project.\n~~~", PromptRoleUnknown, "none"},
		{"a longer tilde fence with an info string", "~~~~text\nRun crw-loop for the migration project.\n~~~~", PromptRoleUnknown, "none"},
		{"a tilde fence closed, then a real request", "~~~\nRun crw-loop for the migration project.\n~~~\nStart crw-loop for the migration project.", PromptRoleUnknown, "pointer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			answer, s := loopScopeRun(t, c.prompt, c.role)
			switch c.owner {
			case "recipe":
				if !strings.Contains(answer, loopScopeRecipeMarker) || strings.Contains(answer, loopScopePointerMarker) || !s.LoopArmSeen {
					t.Fatalf("want the recipe and loopArmSeen:\n%s\n%+v", answer, s)
				}
			case "pointer":
				if !strings.Contains(answer, loopScopePointerMarker) || strings.Contains(answer, loopScopeRecipeMarker) {
					t.Fatalf("want the scope pointer:\n%s", answer)
				}
				// The pointer starts no loop and arms nothing: none of the recipe's commands, no loopArmSeen, no goal or FSM state.
				for _, cmd := range []string{"loop init", "orchestrate P", "create_goal", "orchestrate status"} {
					if strings.Contains(answer, cmd) {
						t.Fatalf("the pointer carries %q:\n%s", cmd, answer)
					}
				}
				if s.LoopArmSeen || s.OrchestrationActive || s.Phase != state.PhaseIdle || len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "t1" {
					t.Fatalf("the pointer's state decision: %+v", s)
				}
			case "none":
				if strings.Contains(answer, loopScopePointerMarker) || strings.Contains(answer, loopScopeRecipeMarker) || s.LoopArmSeen {
					t.Fatalf("want no loop emission:\n%s\n%+v", answer, s)
				}
			}
		})
	}
}

// The recipe leads with the project-coordination branch (CRW-1084), before any of its steps.
func TestLoopArmRecipeRoutesProjectCoordinationFirst(t *testing.T) {
	for _, platform := range []string{"linux", "win32"} {
		d := LoopArmDirective(platform)
		branch := strings.Index(d, "Project coordination first")
		step1 := strings.Index(d, "1. Session id")
		if branch < 0 || step1 < 0 || branch > step1 {
			t.Fatalf("%s: the recipe lacks a leading project-coordination branch:\n%s", platform, d)
		}
		for _, want := range []string{"$crw:crw-run", "creates no implementation goalplan or FSM"} {
			if !strings.Contains(d[branch:step1], want) {
				t.Fatalf("%s: the branch lacks %q", platform, want)
			}
		}
	}
}

func TestLoopScopePointerNamesTheOwnersAndNoRecipeStep(t *testing.T) {
	for _, parent := range []bool{false, true} {
		d := LoopScopeDirective(parent)
		for _, want := range []string{"$crw:crw-run", "$crw:crw-loop", "$crw:crw-pabcd", "no-goal", "mention or quoted example alone is not authorization"} {
			if !strings.Contains(d, want) {
				t.Errorf("parent=%v: the pointer lacks %q", parent, want)
			}
		}
		if strings.Contains(d, "1. Session id") || strings.Contains(d, "`crw pabcd loop init") {
			t.Errorf("parent=%v: the pointer carries recipe steps", parent)
		}
		if parent != strings.Contains(d, "registered as a project parent") {
			t.Errorf("parent=%v: the registered-parent sentence is wrong", parent)
		}
		if n := strings.Count(d, "\n") + 1; n > 14 {
			t.Errorf("parent=%v: the pointer is %d lines, not short", parent, n)
		}
	}
}

// The pointer composes with the agbrowse request as the recipe does.
func TestLoopScopePointerComposesWithAgbrowse(t *testing.T) {
	answer, _ := loopScopeRun(t, "agbrowse로 검증하면서 crw-loop 프로젝트 돌려줘", PromptRoleUnknown)
	if !strings.Contains(answer, loopScopePointerMarker) || !strings.HasSuffix(answer, AgbrowseSearchDirective) {
		t.Fatal(answer)
	}
}

// Production holds no role reader until CRW-386 supplies the verified registry read: with no seam the role is unknown, and a
// project request still gets the pointer rather than a parent claim.
func TestLoopScopeWithoutARoleReaderReadsNoRole(t *testing.T) {
	cwd := t.TempDir()
	answer := PromptSubmitHandle(PromptSubmitPayload{Cwd: cwd, SessionID: "s1", Prompt: "Start crw-loop for the migration project.", TurnID: "t1", PabcdEnabled: true}, "linux", promptSubmitHost(cwd))
	if !strings.Contains(answer, loopScopePointerMarker) || strings.Contains(answer, "registered as a project parent") {
		t.Fatal(answer)
	}
}

func TestClassifyLoopArmScope(t *testing.T) {
	cases := []struct {
		prompt string
		want   LoopArmScope
	}{
		{"Start crw-loop for the migration project.", LoopScopeProject},
		{"Run crw-loop on https://linear.app/acme/project/p-1a2b3c4d5e6f", LoopScopeProject},
		{"crw-loop로 프로젝트 진행해", LoopScopeProject},
		{"Run crw-loop to coordinate the children", LoopScopeProject},
		{"Run crw-loop for this task", LoopScopeNone},
		{"Run crw-loop", LoopScopeNone},
		{"Implement the parser in this session using crw-loop for the project", LoopScopeCurrentTask},
		{"crw-loop로 현재 세션에서 구현해줘 (프로젝트 CRW)", LoopScopeCurrentTask},
		{"Run crw-loop on this issue, in the current session, and fix the build", LoopScopeCurrentTask},
		{"Run crw-loop in this session for the project", LoopScopeProject},
	}
	for _, c := range cases {
		if got := ClassifyLoopArmScope(c.prompt); got != c.want {
			t.Errorf("%q: %v, want %v", c.prompt, got, c.want)
		}
	}
}

// Fences are backtick or tilde (CRW-1084, port: fixed); the oracle knew only backticks and read a tilde example as a request.
func TestRequestLinesKnowsTildeFences(t *testing.T) {
	cases := []struct {
		prompt string
		want   []string
	}{
		{"~~~\nRun crw-loop\n~~~", []string{}},
		{"~~~~\nRun crw-loop\n~~~\nstill inside\n~~~~\nUse crw-pabcd to plan", []string{"Use crw-pabcd to plan"}},
		{"~~~text\nRun crw-loop\n~~~\nUse crw-pabcd to plan", []string{"Use crw-pabcd to plan"}},
		{"```\nRun crw-loop\n~~~\nstill inside\n```\nUse crw-pabcd to plan", []string{"Use crw-pabcd to plan"}},
		{"~~~\nRun crw-loop\n```\nstill inside\n~~~\nUse crw-pabcd to plan", []string{"Use crw-pabcd to plan"}},
		{"~~ Run crw-loop", []string{"~~ Run crw-loop"}},
	}
	for _, c := range cases {
		got := requestLines(c.prompt)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: %q, want %q", c.prompt, got, c.want)
		}
	}
}
