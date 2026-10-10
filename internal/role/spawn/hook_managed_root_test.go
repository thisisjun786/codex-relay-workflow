package spawn

import (
	"os"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1124 (verification round 1): the root the preview resolved is the root the issuance records into, and a guard-wrapped managed
// spawn resolves it once, however many roles' prompts could have led its marker.

// The workspace is replaced by a link to another directory holding a ledger of the same name between the preview and the issuance:
// the issuance denies and writes neither ledger.
func TestSpawnHookManagedRootReplacedBetweenPreviewAndIssuanceIsDenied(t *testing.T) {
	rig, _ := spawnManagedDeepRig(t)
	outside := t.TempDir()
	outsideLedger := outside + "/.crw/dispatches/rec-s1/one.json"
	spawnHookMust(t, os.MkdirAll(outside+"/.crw/dispatches/rec-s1", 0o700))
	spawnHookMust(t, os.WriteFile(outsideLedger, []byte(spawnManagedDeepLedger), 0o600))
	read := spawnHookSettings
	spawnHookSettings = func(env host.LookupEnv) role.SettingsSnapshot {
		s := read(env)
		spawnHookMust(t, os.Rename(rig.ws, rig.ws+"-old"))
		spawnHookMust(t, os.Symlink(outside, rig.ws))
		return s
	}
	t.Cleanup(func() { spawnHookSettings = read })
	got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: build", 0), rig.env)
	if !spawnGrantClaimDenied(got) || !strings.Contains(got, "managed dispatch") {
		t.Fatalf("a replaced root was let through: %.300q", got)
	}
	if data, _ := os.ReadFile(outsideLedger); string(data) != spawnManagedDeepLedger {
		t.Fatalf("the ledger of the replacing directory changed: %s", data)
	}
	if data, _ := os.ReadFile(rig.ws + "-old/.crw/dispatches/rec-s1/one.json"); string(data) != spawnManagedDeepLedger {
		t.Fatalf("the ledger of the original root changed: %s", data)
	}
}

// A message wrapped in the guard resolves the dispatch root once, for one prompt-less role candidate and for roles with prompts alike.
func TestSpawnHookGuardedManagedSpawnResolvesTheRootOnce(t *testing.T) {
	for _, c := range []struct{ name, store, message string }{
		{"no prompts", "", V1ScopeBlock + "\n\n[CRW-DISPATCH:one:att-1]\nTASK: build"},
		{"prompts", `{"roles":{"explorer":{"mode":"default","promptOverride":"Explore."},"reviewer":{"mode":"default","promptOverride":"Review."},"executor":{"mode":"default","promptOverride":"Build it."}}}`,
			V1ScopeBlock + "\n\nBuild it.\n\n[CRW-DISPATCH:one:att-1]\nTASK: build"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig, ledger := spawnManagedDeepRig(t)
			if c.store != "" {
				home, _ := rig.env("CRW_HOME")
				spawnHookMust(t, os.WriteFile(home+"/subagents.json", []byte(c.store), 0o600))
			}
			gitCalls := spawnSnapshotGit(t)
			got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, c.message, 0), rig.env)
			if !strings.Contains(got, `"permissionDecision":"allow"`) || !strings.Contains(got, `"model":"rec/exec-primary"`) {
				t.Fatalf("guarded managed spawn = %.300q", got)
			}
			if n := gitCalls(); n != 1 {
				t.Fatalf("a guarded managed spawn ran git %d times, want 1", n)
			}
			if data, _ := os.ReadFile(ledger); !strings.Contains(string(data), `"spawnIssued": true`) && !strings.Contains(string(data), `"spawnIssued":true`) {
				t.Fatalf("not issued: %s", data)
			}
		})
	}
}
