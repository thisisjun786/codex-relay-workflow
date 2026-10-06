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

// The manifest that decides the edit is read inside the lock, not from the copy the command saw
// before it started waiting: a writer that publishes while this command waits must be seen, so the
// restore cannot be computed from a manifest a concurrent writer has already replaced (failure
// class 3, the check-then-act race). The two manifests disagree on the recorded prior value, and
// the resulting config.toml tells which one decided.
func TestConfigSetReadsTheManifestInsideTheLock(t *testing.T) {
	home, path := configSetHome(t, "[memories]\ndedicated_tools = true\n", true)
	// Before the lock: the key was set from "false", so an unset would restore that value.
	stale := configSetManifest(t, home)
	prior := "false"
	stale.TableKeys[configSetKey] = TableKeyRecord{"memories", "dedicated_tools", &prior, "true", true}
	deactivationSaveManifest(t, home, stale)

	held := configLockWritersHold(t, path)
	started := make(chan struct{})
	done := make(chan ConfigSetOutcome, 1)
	go func() {
		close(started)
		r, _ := ApplyManagedKey(ConfigSetDeps{CodexHome: home, ConfigPath: path}, configSetKey, nil)
		done <- r
	}()
	<-started
	time.Sleep(200 * time.Millisecond) // the command is waiting on the lock now
	// While it waits, another writer publishes: this key was set from an absent value.
	fresh := configSetManifest(t, home)
	fresh.TableKeys[configSetKey] = TableKeyRecord{"memories", "dedicated_tools", nil, "true", true}
	deactivationSaveManifest(t, home, fresh)
	held.Release()

	r := <-done
	if !r.OK || r.AppliedValue != "(absent)" || r.PriorValue != nil {
		t.Fatalf("outcome %+v", r)
	}
	if got := activationRead(t, path); strings.Contains(got, "dedicated_tools") {
		t.Fatalf("the manifest read before the lock decided the restore: %q", got)
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
	// A refusal is not a deactivation: it must not leave self-healing opted out.
	if marker, e := ReadSelfHealMarkerFile(home); e != nil || marker != nil {
		t.Fatalf("the refused deactivation recorded consent state: %+v, %v", marker, e)
	}
	held.Release()

	// Control: the released lock restores the key exactly as before.
	var after [][]string
	r, err := Deactivate(deactivationDeps(home, deactivationRun(t, path, map[string]bool{}, &after)))
	if err != nil || !reflect.DeepEqual(r.RestoredKeys, []string{"memories.dedicated_tools"}) {
		t.Fatalf("deactivation after the lock was released = %+v, %v", r, err)
	}
	marker, err := ReadSelfHealMarkerFile(home)
	if err != nil || marker == nil || marker.OptedOut == nil || !*marker.OptedOut {
		t.Fatalf("the deactivation did not record the opt-out: %+v, %v", marker, err)
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

// An uninstall with nothing to write is not gated on the lock: with no owned table key and no flag
// CRW enabled, the injected CLI has nothing to disable and no config.toml write to serialize, so a
// busy lock must not fail the command.
func TestDeactivateWithNothingToWriteTakesNoLock(t *testing.T) {
	home := activationHome(t)
	path := filepath.Join(home, "config.toml")
	activationWrite(t, path, deactivationConfig)
	m := deactivationManifest(t, home, nil, map[string]FlagRecord{"hooks": {PriorEnabled: true}})
	m.ConfigPath = path
	deactivationSaveManifest(t, home, m)
	held := configLockWritersHold(t, path)
	defer held.Release()
	r, err := Deactivate(deactivationDeps(home, func(a []string) CodexRunResult {
		if a[1] != "list" {
			t.Fatalf("a disable reached the runner: %v", a)
		}
		return CodexRunResult{}
	}))
	if err != nil || r.FileDrifted || len(r.RestoredKeys) != 0 || len(r.Disabled) != 0 {
		t.Fatalf("result=%+v error=%v", r, err)
	}
	if activationRead(t, path) != deactivationConfig {
		t.Fatal("the no-op deactivation changed config.toml")
	}
}

// An explicitly empty config path names no file: neither writer derives a sidecar in the working
// directory, and the missing-path no-op behaviour is kept.
func TestEmptyConfigPathTakesNoLock(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	empty := ""
	change, err := SetMultiAgentV2State(MultiAgentV2Deps{ConfigPath: &empty, Run: func([]string) CodexRunResult {
		t.Fatal("the empty override reached the runner")
		return CodexRunResult{}
	}}, MultiAgentV1)
	if err != nil || change.Changed {
		t.Fatalf("the empty override no-op changed: %+v, %v", change, err)
	}

	home := t.TempDir()
	m := deactivationManifest(t, home, nil, nil)
	m.ConfigPath = ""
	deactivationSaveManifest(t, home, m)
	if _, err := Deactivate(deactivationDeps(home, func([]string) CodexRunResult { return CodexRunResult{} })); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("an empty config path wrote into the working directory: %v, %v", entries, err)
	}
}
