package spawn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1124: one spawn event reads the helper role settings once and resolves the dispatch root once; the managed issuance keeps
// only its own read of the record under the lock.

// spawnSnapshotCount counts the settings reads of the events run while it is installed, and calls after (when not nil) after each.
func spawnSnapshotCount(t *testing.T, after func(n int64)) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	read := spawnHookSettings
	spawnHookSettings = func(env host.LookupEnv) role.SettingsSnapshot {
		s := read(env)
		if count := n.Add(1); after != nil {
			after(count)
		}
		return s
	}
	t.Cleanup(func() { spawnHookSettings = read })
	return &n
}

// spawnSnapshotGit puts a git in front of PATH that logs each call and runs the real one, and returns the log's reader.
func spawnSnapshotGit(t *testing.T) func() int {
	t.Helper()
	real, err := exec.LookPath("git")
	spawnHookMust(t, err)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + strconv.Quote(log) + "\nexec " + strconv.Quote(real) + " \"$@\"\n"
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(log)
		return strings.Count(string(data), "\n")
	}
}

func TestSpawnHookReadsSettingsAndTheRootOnce(t *testing.T) {
	gitCalls := spawnSnapshotGit(t)
	reads := spawnSnapshotCount(t, nil)
	store := `{"roles":{"explorer":{"mode":"model","model":"rec/explorer","promptOverride":"Be brief."},"executor":{"mode":"model","model":"rec/exec","promptOverride":"Build it."}}}`

	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	home, _ := rig.env("CRW_HOME")
	spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(store), 0o600))
	direct := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + strconv.Quote(rig.ws) + `,"tool_use_id":"d1","tool_input":{"agent_type":"explorer","message":"look around"}}`
	if got := RunSpawnAttachHook(direct, rig.env); !strings.Contains(got, `"permissionDecision":"allow"`) {
		t.Fatalf("direct spawn = %.200q", got)
	}
	if reads.Load() != 1 || gitCalls() != 0 {
		t.Fatalf("a direct spawn read the settings %d times and ran git %d times, want 1 and 0", reads.Load(), gitCalls())
	}

	rig, ledger := spawnManagedDeepRig(t)
	home, _ = rig.env("CRW_HOME")
	spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(store), 0o600))
	reads.Store(0)
	before := gitCalls()
	if got := RunSpawnAttachHook(spawnManagedDeepPayload(rig.ws, "[CRW-DISPATCH:one:att-1]\nTASK: locate the owner", 0), rig.env); !strings.Contains(got, `"model":"rec/exec-primary"`) {
		t.Fatalf("managed spawn = %.200q", got)
	}
	if data, _ := os.ReadFile(ledger); !strings.Contains(string(data), `"spawnIssued": true`) && !strings.Contains(string(data), `"spawnIssued":true`) {
		t.Fatalf("the managed spawn was not issued: %s", data)
	}
	if reads.Load() != 1 || gitCalls()-before != 1 {
		t.Fatalf("a managed spawn read the settings %d times and ran git %d times, want 1 and 1", reads.Load(), gitCalls()-before)
	}
}

// A settings change while one event is assembled does not reach that event: its prompt, model and notice come from one snapshot,
// and the next event reads the new settings.
func TestSpawnHookOneEventOneSnapshot(t *testing.T) {
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	home, _ := rig.env("CRW_HOME")
	path := filepath.Join(home, "subagents.json")
	spawnHookMust(t, os.WriteFile(path, []byte(`{"roles":{"explorer":{"mode":"model","model":"first/model","promptOverride":"First prompt."}}}`), 0o600))
	second := `{"roles":{"explorer":{"mode":"model","model":"second/model","promptOverride":"Second prompt.","fallback":{"model":"second/fallback"}}}}`
	spawnSnapshotCount(t, func(n int64) {
		if n == 1 {
			spawnHookMust(t, os.WriteFile(path, []byte(second), 0o600))
		}
	})
	event := func(id string) string {
		return RunSpawnAttachHook(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":`+strconv.Quote(rig.ws)+`,"tool_use_id":"`+id+`","tool_input":{"agent_type":"explorer","message":"look around"}}`, rig.env)
	}
	got := event("e1")
	if !strings.Contains(got, "First prompt.") || !strings.Contains(got, `"model":"first/model"`) || strings.Contains(got, "second") || strings.Contains(got, "not managed by first-fallback") {
		t.Fatalf("the first event mixed two settings: %.600q", got)
	}
	got = event("e2")
	if !strings.Contains(got, "Second prompt.") || !strings.Contains(got, `"model":"second/model"`) || !strings.Contains(got, "not managed by first-fallback") {
		t.Fatalf("the next event did not read the new settings: %.600q", got)
	}
}
