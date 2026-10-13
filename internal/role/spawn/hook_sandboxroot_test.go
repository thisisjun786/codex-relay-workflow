package spawn

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265 (pre-merge evaluation of f0691fdd, d1): a native V2 spawn whose ciphertext task names an opted-in claimed dispatch by its
// task_name is a managed spawn as much as a marked one, and its child works in the parent's cwd. CRW-1265 refused a parent outside
// every dedicated root; since CRW-1308 the call is managed and issued the attempt there too, and the rule's verdict is a recorded
// warning. A parent in a dedicated root is managed with no warning, and a call that names no dispatch is nobody's business of the rule.
func TestSpawnHookV2TaskNameRefusesAParentOutsideTheSandboxRoots(t *testing.T) {
	const store = `{"roles":{"reviewer":{"mode":"model","model":"gpt-6.1-sol","effort":"high"}}}`
	input := `{"task_name":"independent_review","agent_type":"reviewer","fork_turns":"none","message":` + strconv.Quote(spawnV2Fernet()) + `}`
	direct := `{"task_name":"other_name","agent_type":"reviewer","fork_turns":"none","model":"user/model","reasoning_effort":"low","message":` + strconv.Quote(spawnV2Fernet()) + `}`
	c := spawnHookCase{}
	c.Env.Store = &[]string{store}[0]
	rig := spawnHookNewRig(t, nil, c)
	file := v2ChainLedger(t, rig.ws, "independent_review", "reviewer")
	w := sandboxroottest.Fake(t) // the workspace is outside the fake sandbox base and every project: warned about
	// A call that names no dispatch of the session is not judged.
	if answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "direct-1", direct), rig.env); !strings.Contains(answer, `"permissionDecision":"allow"`) {
		t.Fatalf("a call naming no dispatch was refused: %q", answer)
	}
	if records := w.Warnings(); len(records) != 0 {
		t.Fatalf("a call naming no dispatch was judged: %+v", records)
	}
	answer := RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-1", input), rig.env)
	if !strings.Contains(answer, `"permissionDecision":"allow"`) || strings.Contains(answer, sandboxroot.Code) {
		t.Fatalf("a V2 call naming a dispatch from outside every dedicated root: %q", answer)
	}
	if issued, tool := v2ChainRead(t, file); !issued || tool != "native-1" {
		t.Fatalf("issued = %v to %q", issued, tool)
	}
	w.Warned(t, "role/managed_spawn", sandboxroot.RuleOutsideDedicatedRoot, "")
	// A workspace that is a project root: the same call is managed and issued, with no warning.
	rig = spawnHookNewRig(t, nil, c)
	file = v2ChainLedger(t, rig.ws, "independent_review", "reviewer")
	w.WriteConfig(t, []string{rig.ws})
	w.ClearWarnings(t)
	answer = RunSpawnAttachHook(v2ChainStdin(rig.ws, "native-3", input), rig.env)
	if !strings.Contains(answer, `"permissionDecision":"allow"`) {
		t.Fatalf("a V2 call from a project root: %q", answer)
	}
	if issued, tool := v2ChainRead(t, file); !issued || tool != "native-3" {
		t.Fatalf("issued = %v to %q", issued, tool)
	}
	if records := w.Warnings(); len(records) != 0 {
		t.Fatalf("a project root was warned about: %+v", records)
	}
}
