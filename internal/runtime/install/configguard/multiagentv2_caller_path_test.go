package configguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// CRW-891: SetMultiAgentV2State takes crwdir.LockConfig keyed by lock.Target, but its content
// path stays the caller's config path (CRW-866 had replaced it with lock.Target). An injected
// runner that atomically replaces the symlink pathname therefore has to be read through the new
// file, and the repair has to be published there. These tests are red on the CRW-866 baseline.

const multiAgentCallerPathPre = "[features.multi_agent_v2]\nenabled = false\nmax = 7\n"
const multiAgentCallerPathPost = "[features]\nmulti_agent_v2 = true\n"
const multiAgentCallerPathRepaired = "[features]\n\n[features.multi_agent_v2]\nenabled = true\nmax = 7\n"

// multiAgentCallerPathRenameRunner is the injected runner of the issue's reproduction: it writes
// the new settings to a temporary file and renames it over the caller's path, so a symlink there is
// replaced by a regular file and the old target keeps its bytes.
func multiAgentCallerPathRenameRunner(t *testing.T, path, body string) CodexRunner {
	t.Helper()
	return func(args []string) CodexRunResult {
		tmp := path + ".runner.tmp"
		if err := os.WriteFile(tmp, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		return CodexRunResult{}
	}
}

// The issue's reproduction: the runner replaces the symlink pathname, so the post-image lives at
// the caller's path. The repair must be computed from that post-image, published through that path,
// and the reported change must match the live config.
func TestMultiAgentCallerPathReadsTheReplacedPath(t *testing.T) {
	home, path := multiAgentHome(t)
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, multiAgentCallerPathPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	deps := MultiAgentV2Deps{CodexHome: home, Run: multiAgentCallerPathRenameRunner(t, path, multiAgentCallerPathPost)}
	change, err := SetMultiAgentV2State(deps, MultiAgentV2)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the runner's rename did not replace the link: %v, %v", info, err)
	}
	live := activationRead(t, path)
	if live != multiAgentCallerPathRepaired {
		t.Fatalf("live config = %q, want %q", live, multiAgentCallerPathRepaired)
	}
	if activationRead(t, target) != multiAgentCallerPathPre {
		t.Fatalf("the repair reached the lock's old target: %q", activationRead(t, target))
	}
	if !change.Changed || change.Version != MultiAgentV2 || !change.V2Enabled {
		t.Fatalf("change = %+v", change)
	}
	state := ReadMultiAgentV2State(deps)
	if state.Version != change.Version || state.V2Enabled != change.V2Enabled {
		t.Fatalf("reported %+v but the live state is %+v", change, state)
	}
	if !IsMultiAgentV2Enabled(path) {
		t.Fatal("the repaired config must read back as enabled")
	}
}

// Control A: a regular-file config.toml, the same runner, exactly today's repair.
func TestMultiAgentCallerPathRegularFileControl(t *testing.T) {
	home, path := multiAgentHome(t)
	activationWrite(t, path, multiAgentCallerPathPre)
	deps := MultiAgentV2Deps{CodexHome: home, Run: multiAgentCallerPathRenameRunner(t, path, multiAgentCallerPathPost)}
	change, err := SetMultiAgentV2State(deps, MultiAgentV2)
	if err != nil || !change.Changed || change.Version != MultiAgentV2 || !change.V2Enabled {
		t.Fatalf("change = %+v, %v", change, err)
	}
	if got := activationRead(t, path); got != multiAgentCallerPathRepaired {
		t.Fatalf("config = %q, want %q", got, multiAgentCallerPathRepaired)
	}
}

// Control B: a runner that writes through the link into the target keeps today's repair: the link
// still resolves to lock.Target, so the content path and the lock's target are the same file.
func TestMultiAgentCallerPathWriteThroughTheLinkControl(t *testing.T) {
	home, path := multiAgentHome(t)
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, multiAgentCallerPathPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	deps := MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
		activationWrite(t, target, multiAgentCallerPathPost)
		return CodexRunResult{}
	}}
	change, err := SetMultiAgentV2State(deps, MultiAgentV2)
	if err != nil || !change.Changed || change.Version != MultiAgentV2 || !change.V2Enabled {
		t.Fatalf("change = %+v, %v", change, err)
	}
	if got := activationRead(t, path); got != multiAgentCallerPathRepaired {
		t.Fatalf("config through the link = %q, want %q", got, multiAgentCallerPathRepaired)
	}
	if link, err := os.Readlink(path); err != nil || link != "target.toml" {
		t.Fatalf("the symlink was replaced: %v, %v", link, err)
	}
}

// Control C: the lock is still keyed by lock.Target, so two writers that reach one file through
// different spellings share it: a holder of the resolved target's sidecar refuses this call, which
// writes nothing and does not reach the runner.
func TestMultiAgentCallerPathKeepsTheLockKeyedByTarget(t *testing.T) {
	home, path := multiAgentHome(t)
	target := filepath.Join(home, "target.toml")
	activationWrite(t, target, multiAgentCallerPathPre)
	if err := os.Symlink("target.toml", path); err != nil {
		t.Fatal(err)
	}
	held, err := crwdir.LockConfig(target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	calls := 0
	_, err = SetMultiAgentV2State(MultiAgentV2Deps{CodexHome: home, Run: func([]string) CodexRunResult {
		calls++
		return CodexRunResult{}
	}}, MultiAgentV2)
	if err == nil || !strings.Contains(err.Error(), crwdir.ConfigLockBusy) {
		t.Fatalf("the symlink spelling did not share the target's lock: %v", err)
	}
	if calls != 0 || activationRead(t, target) != multiAgentCallerPathPre {
		t.Fatalf("the refused call wrote or reached the runner: calls=%d config=%q", calls, activationRead(t, target))
	}
}
