package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1119 (verification round 1): a recognized spawn wrapped in the hook's guard is denied for an unusable role only when that role
// can decide its routing: a direct spawn of a usable role is not stopped by another role's settings.
func TestSpawnHookGuardedDirectSpawnIsolatesAnUnusableRole(t *testing.T) {
	const broken = `{"roles":{"explorer":{"mode":"model","model":" "},"executor":{"mode":"model","model":"vendor/exec"}}}`
	spawn := func(rig *spawnHookRig, message string) string {
		return RunSpawnAttachHook(spawnReapplyPayload(rig.ws, "call-1", `{"agent_type":"executor","message":`+spawnHookQuote(message)+`}`), rig.env)
	}
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	home, _ := rig.env("CRW_HOME")
	spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(broken), 0o600))

	for _, guard := range []string{V1ScopeBlock, LeafGuardBlock} {
		got := spawn(rig, guard+"\n\nTASK: build it")
		if !strings.Contains(got, `"permissionDecision":"allow"`) || !strings.Contains(got, `"model":"vendor/exec"`) {
			t.Fatalf("guarded executor spawn beside a broken explorer = %.300q", got)
		}
	}
	// Text that could carry a managed marker behind a prompt nobody can read is undecided: denied with the store's error.
	for _, message := range []string{V1ScopeBlock + "\n\n[CRW-DISPATCH:one:att-1]\nTASK: build", V1ScopeBlock + "\n\nsome prompt\n\n[CRW-DISPATCH:one:att-1]\nTASK: build"} {
		got := spawn(rig, message)
		if !strings.Contains(got, `"permissionDecision":"deny"`) || !strings.Contains(got, "unusable helper role settings") {
			t.Fatalf("undecided managed routing = %.300q", got)
		}
	}
}
