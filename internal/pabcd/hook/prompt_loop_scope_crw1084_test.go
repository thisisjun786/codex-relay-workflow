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
		// A project request that carries a negation, an example or a quotation of the exception is still a project request.
		{"a project request with a negated implementation", "Start crw-loop for the migration project. Do not implement this task.", PromptRoleUnknown, "pointer"},
		{"a project request with a negation in its own clause", "Start crw-loop for the migration project without implementing this task in this session", PromptRoleUnknown, "pointer"},
		{"a project request with a negated clause after but", "Start crw-loop for the migration project but don't implement this task", PromptRoleUnknown, "pointer"},
		{"a project request with a backtick example", "Start crw-loop for the migration project.\nExample: `fix this task`", PromptRoleUnknown, "pointer"},
		{"a project request with a quoted example", "Start crw-loop for the migration project.\n\"Implement this task\" is only an example.", PromptRoleUnknown, "pointer"},
		{"a project request with a tilde-fenced example", "Start crw-loop for the migration project.\n~~~\nImplement this task in this session\n~~~", PromptRoleUnknown, "pointer"},
		{"a project request with a listed example", "Start crw-loop for the migration project.\n- implement this task", PromptRoleUnknown, "pointer"},
		{"a project request whose verb and task are in different clauses", "Start crw-loop for the migration project. Fix the build. Work in this session.", PromptRoleUnknown, "pointer"},
		{"a Korean project request with a negated implementation", "crw-loop 돌려서 프로젝트 조정해줘, 이 작업 구현하지 마", PromptRoleUnknown, "pointer"},
		{"a tilde fence closed, then a real request", "~~~\nRun crw-loop for the migration project.\n~~~\nStart crw-loop for the migration project.", PromptRoleUnknown, "pointer"},
		// Verification round 2: a plain example or explanation of the exception is not the exception.
		{"a project request with a plain Example: line", "Start crw-loop for the migration project. Example: implement this task in this session", PromptRoleUnknown, "pointer"},
		{"a project request with a for-example clause", "Start crw-loop for the migration project. For example, implement this task in this session.", PromptRoleUnknown, "pointer"},
		{"a project request with an e.g. parenthetical", "Start crw-loop for the migration project (e.g. implement this task in this session).", PromptRoleUnknown, "pointer"},
		{"a project request with an Example: heading line", "Start crw-loop for the migration project.\nExample:\nimplement this task in this session", PromptRoleUnknown, "pointer"},
		{"a Korean project request with a 예: example", "마이그레이션 프로젝트에 crw-loop 시작해줘. 예: 이 세션에서 이 작업 구현해", PromptRoleUnknown, "pointer"},
		{"a Korean project request with a 예를 들어 explanation", "마이그레이션 프로젝트에 crw-loop 시작해줘. 예를 들어 이 세션에서 이 작업 구현해 같은 요청만 예외야", PromptRoleUnknown, "pointer"},
		{"an example line, then a real current-task request", "Start crw-loop for the migration project. Example: something\nImplement this task in this session.", PromptRoleUnknown, "recipe"},
		// An unrelated negation before the request does not negate the implement verb; one that governs the verb does.
		{"an unrelated negation before an explicit implementation", "Start crw-loop for the migration project; no questions, please implement this task in this session", PromptRoleUnknown, "recipe"},
		{"a negation that governs the verb after an unrelated one", "Start crw-loop for the migration project; no questions, and do not implement this task in this session", PromptRoleUnknown, "pointer"},
		{"a negation with filler words before the verb", "Start crw-loop for the migration project; I am asking you not to actually implement this task in this session", PromptRoleUnknown, "pointer"},
		// A generic "this project" as the place of a single-task fix is not project coordination.
		{"a single-task fix in this project", "Use crw-loop to fix the failing test in this project", PromptRoleUnknown, "recipe"},
		{"a build-error fix in the project", "Use crw-loop to fix the build error in the project", PromptRoleUnknown, "recipe"},
		{"a child process implementation", "Use crw-loop to implement the child process supervisor", PromptRoleUnknown, "recipe"},
		{"a Korean single-task fix in this project", "crw-loop 써서 이 프로젝트에서 실패하는 테스트 고쳐줘", PromptRoleUnknown, "recipe"},
		{"a loop for this project with no single-task target", "Start crw-loop for this project", PromptRoleUnknown, "pointer"},
		{"a loop on the issues in this project", "Run crw-loop on the issues in this project", PromptRoleUnknown, "pointer"},
		{"a fix in a named project", "Use crw-loop to fix the failing test in the migration project", PromptRoleUnknown, "pointer"},
		{"a fix in this project that also coordinates children", "Use crw-loop to fix the failing test in this project and coordinate the children", PromptRoleUnknown, "pointer"},
		{"a negated fix in this project", "Use crw-loop for this project, but don't fix the failing test in this project", PromptRoleUnknown, "pointer"},
		// Post-evaluation (ec92b03a): the noun coordinate is not coordination, and children implementing is not this session.
		{"a single-task fix of coordinate values", "Use crw-loop to fix rounding of coordinate values in the parser.", PromptRoleUnknown, "recipe"},
		{"a dispatched task fixing coordinates", "Use crw-loop to fix the coordinates in the parser", PromptRoleTask, "recipe"},
		{"coordination while children implement", "Run crw-loop in this session to coordinate the migration project while child tasks implement their assigned issues.", PromptRoleUnknown, "pointer"},
		// Verification round 3 (795db82a): a coordinate verb after an adverb or a subject is coordination, and a current-task
		// implementation after a child process or after consulting the children stays this session's.
		{"an adverb before the coordinate verb", "Use crw-loop to actively coordinate the lanes.", PromptRoleUnknown, "pointer"},
		{"a subject before the coordinate verb", "Use crw-loop so we coordinate the lanes.", PromptRoleUnknown, "pointer"},
		{"a current-task fix after inspecting a child process", "Use crw-loop in this session to inspect the child process and fix its crash in this project.", PromptRoleUnknown, "recipe"},
		{"a current-task implementation after consulting the children", "Use crw-loop in this session to consult the children and implement this task.", PromptRoleUnknown, "recipe"},
		// Verification round 4 (b1ffd290): a sentence adverb between another agent and the implement verb keeps the verb that
		// agent's; only a comma or a coordinating conjunction right after the noun starts this session's own verb.
		{"children then implement", "Use crw-loop in this session while the children then implement their issues.", PromptRoleUnknown, "pointer"},
		{"child tasks then implement", "Use crw-loop in this session while child tasks then implement their assigned issues.", PromptRoleUnknown, "pointer"},
		{"they also implement", "Use crw-loop in this session while they also implement the project issues.", PromptRoleUnknown, "pointer"},
		{"children meanwhile implement", "Use crw-loop in this session while the children meanwhile implement their issues.", PromptRoleUnknown, "pointer"},
		{"children each implement", "Use crw-loop in this session while the children each implement their own issue.", PromptRoleUnknown, "pointer"},
		{"a current-task implementation after a comma and then", "Use crw-loop in this session to consult the children, then implement this task.", PromptRoleUnknown, "recipe"},
		// CRW-1166
		{"an object noun then implement", "Use crw-loop in this session to consult the children then implement this task.", PromptRoleUnknown, "recipe"},
		{"an object noun then implement in a named project", "Use crw-loop in this session to consult the workers then implement this task in the migration project.", PromptRoleUnknown, "recipe"},
		{"an object noun, then, implement", "Use crw-loop in this session to consult the children, then, implement this task", PromptRoleUnknown, "recipe"},
		{"a subordinate subject then implement", "Use crw-loop in this session as the children, then, implement their issues.", PromptRoleUnknown, "pointer"},
		{"a non-coordination subject meanwhile implements", "Use crw-loop in this session while the workers meanwhile implement their issues.", PromptRoleUnknown, "pointer"},
		{"a Korean value adjustment", "crw-loop로 타임아웃 값 조정해줘", PromptRoleUnknown, "recipe"},
		{"a Korean lane coordination", "crw-loop로 레인들을 조정해줘", PromptRoleUnknown, "pointer"},
		{"a Korean connective after an object child", "crw-loop 돌려서 현재 세션에서 자식 작업을 확인하고 구현해", PromptRoleUnknown, "recipe"},
		{"a Korean connective after a subject child", "crw-loop 돌려서 현재 세션에서 자식이 확인하고 구현해", PromptRoleUnknown, "pointer"},
		{"a Korean connective then a delegated causative", "crw-loop 돌려서 현재 세션에서 자식에게 확인하고 구현하게 해", PromptRoleUnknown, "pointer"},
		{"a Korean connective then an explicit worker subject", "crw-loop 돌려서 현재 세션에서 자식 작업을 확인하고 워커가 구현해", PromptRoleUnknown, "pointer"},
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
		// A requested parent native goal follows crw-run's goal-mode lifecycle; only the implementation goalplan and FSM are barred.
		for _, want := range []string{"goal-mode lifecycle", "creates no implementation goalplan or FSM"} {
			if !strings.Contains(d, want) {
				t.Errorf("parent=%v: the pointer lacks %q", parent, want)
			}
		}
		if strings.Contains(d, "creates no goal,") {
			t.Errorf("parent=%v: the pointer bars the parent goal that was asked for", parent)
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

// PromptSubmitHandle holds no role reader (the harness hands PromptSubmitHandleWithRole one): with no seam the role is unknown,
// and a project request still gets the pointer rather than a parent claim.
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
		{"Start crw-loop for the migration project. Do not implement this task.", LoopScopeProject},
		{"Start crw-loop for the migration project. Example: `fix this task`", LoopScopeProject},
		{"Start crw-loop for the migration project.\n\"Implement this task\" is only an example.", LoopScopeProject},
		{"Start crw-loop for the project but never fix this task", LoopScopeProject},
		{"Run crw-loop with no-tests and no-goal, implement this task in this session", LoopScopeCurrentTask},
		{"Run crw-loop for the project, no implementation of this task in this session", LoopScopeProject},
		{"Start crw-loop for the project; implement this task in this session", LoopScopeCurrentTask},
		{"Start crw-loop for the project. Please don't implement this issue here.", LoopScopeProject},
		{"Start crw-loop for the migration project. Example: implement this task in this session", LoopScopeProject},
		{"Start crw-loop for the migration project (for instance, implement this task in this session)", LoopScopeProject},
		{"Start crw-loop for the migration project. Sample: implement this task in this session", LoopScopeProject},
		{"crw-loop 프로젝트 조정 시작해. 예시: 이 세션에서 이 작업 구현해", LoopScopeProject},
		{"crw-loop 프로젝트 조정 시작해. 가령 이 세션에서 이 작업 구현해", LoopScopeProject},
		{"Start crw-loop for the migration project; no questions, please implement this task in this session", LoopScopeCurrentTask},
		{"Start crw-loop for the migration project; no questions, do not implement this task in this session", LoopScopeProject},
		{"Start crw-loop for the project; never, ever implement this task in this session", LoopScopeProject},
		{"Use crw-loop to fix the failing test in this project", LoopScopeNone},
		{"Use crw-loop to fix the lint errors across the codebase of this project", LoopScopeNone},
		{"Use crw-loop to implement the child process supervisor", LoopScopeNone},
		{"crw-loop로 이 프로젝트에서 빌드 오류 수정해", LoopScopeNone},
		{"crw-loop로 자식 프로세스 감시 코드 구현해", LoopScopeNone},
		{"Start crw-loop for this project", LoopScopeProject},
		{"Run crw-loop on the issues in this project", LoopScopeProject},
		{"Use crw-loop to fix the failing test in the migration project", LoopScopeProject},
		{"Use crw-loop to fix the failing test in this project and supervise the child tasks", LoopScopeProject},
		// Post-evaluation (ec92b03a): the noun "coordinate(s)" is data, not coordination intent.
		{"Use crw-loop to fix rounding of coordinate values in the parser.", LoopScopeNone},
		{"Use crw-loop to fix the coordinates in the parser", LoopScopeNone},
		{"Use crw-loop to fix the coordinate system conversion", LoopScopeNone},
		{"Use crw-loop for the coordinate transform bug", LoopScopeNone},
		{"Use crw-loop to coordinate the migration", LoopScopeProject},
		{"Run crw-loop and coordinate the lanes", LoopScopeProject},
		{"Run crw-loop for the coordination of the lanes", LoopScopeProject},
		{"Run crw-loop, coordinating the lanes", LoopScopeProject},
		// A this/current session plus an implement verb is the exception only when this session is the implementer.
		{"Run crw-loop in this session to coordinate the migration project while child tasks implement their assigned issues.", LoopScopeProject},
		{"Run crw-loop in this session to coordinate the migration project; the children will implement their issues", LoopScopeProject},
		{"Run crw-loop in this session and have the child tasks implement the issues of the project", LoopScopeProject},
		{"Run crw-loop in this session, ask the children to implement their issues", LoopScopeProject},
		{"이 세션에서 crw-loop로 프로젝트 조정해줘, 자식 작업이 구현하게 해", LoopScopeProject},
		{"Use crw-loop to implement this task in this session, and report to the children", LoopScopeCurrentTask},
		{"Use crw-loop in this session to implement the parser; child tasks wait", LoopScopeCurrentTask},
		// Verification round 3 (795db82a): coordination verbs in any verb position, the noun in noun positions.
		{"Use crw-loop to actively coordinate the lanes.", LoopScopeProject},
		{"Use crw-loop so we coordinate the lanes.", LoopScopeProject},
		{"Use crw-loop; coordinate the lanes", LoopScopeProject},
		{"Run crw-loop, then carefully coordinate the child lanes", LoopScopeProject},
		{"Run crw-loop so this session coordinates the lanes", LoopScopeProject},
		{"Use crw-loop to help coordinate the migration", LoopScopeProject},
		{"crw-loop로 레인들을 조정해줘", LoopScopeProject},
		{"Use crw-loop to fix coordinate rounding in the parser", LoopScopeNone},
		{"Use crw-loop to fix the bug in coordinate parsing", LoopScopeNone},
		{"Use crw-loop to normalize these coordinates", LoopScopeNone},
		{"Use crw-loop: the coordinate values are off by one, fix them", LoopScopeNone},
		{"Use crw-loop to round coordinates to six decimals", LoopScopeNone},
		// Explicit current-task implementations that mention a child process or another agent only as an object.
		{"Use crw-loop in this session to inspect the child process and fix its crash in this project.", LoopScopeCurrentTask},
		{"Use crw-loop in this session to consult the children and implement this task.", LoopScopeCurrentTask},
		{"Use crw-loop in this session to read the workers' notes, then fix this issue", LoopScopeCurrentTask},
		{"Use crw-loop in this session to restart the subprocess and fix the crash", LoopScopeCurrentTask},
		{"Use crw-loop to ask the other agents for context and then implement this task in this session", LoopScopeCurrentTask},
		{"crw-loop로 현재 세션에서 자식 프로세스 크래시를 고쳐줘", LoopScopeCurrentTask},
		// ...while another agent as the subject or delegate of the verb is still a coordination.
		{"Use crw-loop in this session to ask the children to review and implement their issues", LoopScopeProject},
		{"Use crw-loop in this session; the child tasks review and fix their issues", LoopScopeProject},
		{"Use crw-loop in this session, the workers will then implement the project issues", LoopScopeProject},
		{"Use crw-loop in this session while the children then implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session while child tasks then implement their assigned issues.", LoopScopeProject},
		{"Use crw-loop in this session while they also implement the project issues.", LoopScopeProject},
		{"Use crw-loop in this session while the child agents then also build their parts.", LoopScopeProject},
		{"Use crw-loop in this session while the children, then, implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session while the children meanwhile implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session while the child lanes later fix their issues.", LoopScopeProject},
		{"Use crw-loop in this session while the children each implement their own issue.", LoopScopeProject},
		{"Use crw-loop in this session to consult the children, then implement this task.", LoopScopeCurrentTask},
		{"Use crw-loop in this session to consult the children and then implement this task.", LoopScopeCurrentTask},
		// CRW-1166: the structure before the noun tells an object (a transitive verb) from a subject (a subordinating conjunction).
		{"Use crw-loop in this session to consult the children then implement this task.", LoopScopeCurrentTask},
		{"Use crw-loop in this session to consult the workers then implement this task in the migration project.", LoopScopeCurrentTask},
		{"Use crw-loop in this session to consult the children, then, implement this task", LoopScopeCurrentTask},
		{"Use crw-loop in this session to ask the child agents then fix this issue", LoopScopeCurrentTask},
		{"Use crw-loop in this session, check with the workers then implement this task", LoopScopeCurrentTask},
		{"Use crw-loop in this session as the children, then, implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session when the workers then implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session and let the children then implement their issues.", LoopScopeProject},
		// A subject that is no coordination word is still another agent's verb: the clause is a coordination.
		{"Use crw-loop in this session while the workers meanwhile implement their issues.", LoopScopeProject},
		{"Use crw-loop in this session while the subagents then implement their parts.", LoopScopeProject},
		{"Use crw-loop to fix the failing test, they said fix the lint", LoopScopeNone},
		// Korean: 조정 of a value is no coordination of lanes; -고 after an object child keeps this session the implementer.
		{"crw-loop로 타임아웃 값 조정해줘", LoopScopeNone},
		{"crw-loop로 렌더링 간격을 조정해", LoopScopeNone},
		{"crw-loop로 폰트 크기 재조정해줘", LoopScopeNone},
		{"crw-loop로 레인들을 조정해줘", LoopScopeProject},
		{"crw-loop로 작업 순서 조정해줘", LoopScopeProject},
		{"crw-loop로 현재 세션에서 자식 작업을 확인하고 구현해", LoopScopeCurrentTask},
		{"crw-loop로 현재 세션에서 자식 확인하고, 이 작업 구현해", LoopScopeCurrentTask},
		{"crw-loop로 현재 세션에서 워커에게 물어보고 구현해", LoopScopeCurrentTask},
		{"crw-loop로 현재 세션에서 자식이 확인하고 구현해", LoopScopeProject},
		{"crw-loop로 현재 세션에서 자식 작업이 확인하고 구현해", LoopScopeProject},
		{"crw-loop로 현재 세션에서 자식에게 구현하게 해", LoopScopeProject},
		{"crw-loop 돌려서 현재 세션에서 자식에게 확인하고 구현하게 해", LoopScopeProject},
		{"crw-loop 돌려서 현재 세션에서 자식에게 확인하고 구현하도록 해", LoopScopeProject},
		{"crw-loop 돌려서 현재 세션에서 자식 작업을 확인하고 워커가 구현해", LoopScopeProject},
		{"crw-loop 돌려서 현재 세션에서 자식 작업을 확인하고 하위 에이전트는 구현해", LoopScopeProject},
		{"crw-loop 돌려서 현재 세션에서 자식 작업을 확인하고 제가 구현해", LoopScopeCurrentTask},
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
