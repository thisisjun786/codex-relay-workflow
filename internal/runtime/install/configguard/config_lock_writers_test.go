package configguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-866: the three remaining CRW writers of config.toml take the same sidecar lock retrust and
// the activation take (CRW-844). A CRW writer holding it makes each of them refuse as busy with the
// shared message and write nothing, so two writers never interleave on one config.toml. The
// controls pin that a free lock leaves each command's answer exactly as it is today.

// configLockWritersHold takes the sidecar lock the way another CRW writer does.
func configLockWritersHold(t *testing.T, path string) *crwdir.ConfigLock {
	t.Helper()
	held, err := crwdir.LockConfig(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

// configLockWritersRefused reports whether the command answered the shared busy refusal.
func configLockWritersRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), crwdir.ConfigLockBusy) {
		t.Fatalf("the writer did not refuse while the lock was held: %v", err)
	}
}

// crw install config set/unset (configset.go): the read, the computation, the backup and the
// publish of config.toml are one critical section under the lock.
func TestConfigSetTakesTheConfigLock(t *testing.T) {
	home, path := configSetHome(t, configSetOriginal, true)
	held := configLockWritersHold(t, path)
	beforeConfig, beforeManifest := activationRead(t, path), activationRead(t, manifestPath(home))
	value := true
	r, err := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path, Now: func() string { return "2026-08-29T01:02:03.456Z" }}, configSetKey, &value)
	configLockWritersRefused(t, err)
	if r.OK {
		t.Fatalf("the refused config set reported success: %+v", r)
	}
	if activationRead(t, path) != beforeConfig || activationRead(t, manifestPath(home)) != beforeManifest {
		t.Fatal("the refused config set wrote")
	}
	held.Release()

	// Control: the released lock lets the command through with today's answer.
	got := configSetApply(t, home, path, &value)
	if !got.Changed || got.PriorValue != nil || got.AppliedValue != "true" || got.BackupPath == nil {
		t.Fatalf("config set after the lock was released = %+v", got)
	}
}

// crw install features disable (deactivate.go): the drift check, the read and the restore of
// config.toml are under the lock; the CLI calls that follow rewrite the file themselves and stay
// outside it.
func TestDeactivateTakesTheConfigLock(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	deactivationManifest(t, home, map[string]TableKeyRecord{"memories.dedicated_tools": deactivationKey(nil)}, nil)
	held := configLockWritersHold(t, path)
	before := activationRead(t, path)
	calls := 0
	_, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { calls++; return CodexRunResult{} }))
	configLockWritersRefused(t, err)
	if calls != 0 || activationRead(t, path) != before {
		t.Fatalf("the refused deactivation wrote or reached the CLI: calls=%d config=%q", calls, activationRead(t, path))
	}
	held.Release()

	// Control: the released lock restores the key exactly as before.
	var after [][]string
	r, err := Deactivate(deactivationDeps(home, deactivationRun(t, path, map[string]bool{}, &after)))
	if err != nil || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) {
		t.Fatalf("deactivation after the lock was released = %+v, %v", r, err)
	}
	if got := activationRead(t, path); got != "[memories]\ngenerate_memories = true\n" {
		t.Fatalf("restored config = %q", got)
	}
}

// multi-agent v2 restore (multiagentv2.go): the lock spans the pre-image read, the injected runner
// that rewrites config.toml and the repair's publish.
func TestMultiAgentV2SetTakesTheConfigLock(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, "[features.multi_agent_v2]\nenabled = false\nmax = 7\n")
	held := configLockWritersHold(t, path)
	before := activationRead(t, path)
	calls := 0
	_, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult { calls++; return CodexRunResult{} }}, MultiAgentV2)
	configLockWritersRefused(t, err)
	if calls != 0 || activationRead(t, path) != before {
		t.Fatalf("the refused multi-agent v2 set wrote or reached the runner: calls=%d config=%q", calls, activationRead(t, path))
	}
	held.Release()

	// Control: the released lock repairs the table exactly as before.
	var after [][]string
	got, err := SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: multiAgentFake(t, path, &after)}, MultiAgentV2)
	if err != nil || !got.Changed || got.Version != MultiAgentV2 || !got.V2Enabled {
		t.Fatalf("multi-agent v2 set after the lock was released = %+v, %v", got, err)
	}
	want := "[features]\n\n[features.multi_agent_v2]\nenabled = true\nmax = 7\n"
	if got := activationRead(t, path); got != want {
		t.Fatalf("repaired config = %q, want %q", got, want)
	}
	if _, err := os.Stat(path + ".crw-lock"); err != nil {
		t.Fatalf("the sidecar was not left in place: %v", err)
	}
}
