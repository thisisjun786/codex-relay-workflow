package spawn

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1579 (U-A5-3): the spawn hook takes its temporary root from internal/tmploc, the one place, and only for a spawn event. A
// TMPDIR it refuses (not absolute, or a directory it cannot use) denies the spawn with the reason, before anything is minted, spent
// or replayed; every other event and tool keeps its answer whatever TMPDIR holds.

// spawnTmpdirRig is a rig whose environment answers TMPDIR with tmpdir (set is false for an unset TMPDIR).
func spawnTmpdirRig(t *testing.T, tmpdir string, set bool) *spawnHookRig {
	t.Helper()
	r := spawnHookNewRig(t, nil, spawnHookCase{})
	base := r.env
	r.env = func(key string) (string, bool) {
		if key == "TMPDIR" {
			return tmpdir, set
		}
		return base(key)
	}
	return r
}

// spawnTmpdirRun runs one raw payload through the real entry, RunHook, and returns its answer.
func spawnTmpdirRun(t *testing.T, r *spawnHookRig, raw string) string {
	t.Helper()
	var out bytes.Buffer
	if code := RunHook(context.Background(), strings.NewReader(raw), &out, r.env); code != 0 {
		t.Fatalf("RunHook exit %d, want 0 (answer %q)", code, out.String())
	}
	return out.String()
}

// spawnTmpdirSpawn is a root spawn that asks for recursion, so the hook would mint a grant below its temporary root.
func spawnTmpdirSpawn(ws string) string {
	return `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"tmp-s1","cwd":` + strconv.Quote(ws) +
		`,"tool_use_id":"tmp-t1","tool_input":{"agent_type":"worker","message":"look around ` + SubspawnToken + `"}}`
}

// spawnTmpdirDenyReason is the permissionDecisionReason of a deny envelope, or fails the test for any other answer.
func spawnTmpdirDenyReason(t *testing.T, answer string) string {
	t.Helper()
	var envelope struct {
		HookSpecificOutput struct {
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(answer), &envelope); err != nil || envelope.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("answer is not a deny envelope: %q", answer)
	}
	return envelope.HookSpecificOutput.PermissionDecisionReason
}

func TestRunHook_relativeTMPDIR_deniesASpawnWithTheReason(t *testing.T) {
	r := spawnTmpdirRig(t, "rel", true)
	wd := t.TempDir()
	t.Chdir(wd) // a relative TMPDIR would be read against the process working directory
	answer := spawnTmpdirRun(t, r, spawnTmpdirSpawn(r.ws))
	reason := spawnTmpdirDenyReason(t, answer)
	if !strings.Contains(reason, "tmpdir_not_absolute") || !strings.Contains(reason, `TMPDIR="rel"`) {
		t.Errorf("deny reason %q does not name tmpdir_not_absolute and the value", reason)
	}
	if strings.Contains(answer, "CRW-SUBSPAWN-GRANT:") {
		t.Errorf("a refused TMPDIR minted a grant: %q", answer)
	}
	entries, err := os.ReadDir(wd)
	spawnHookMust(t, err)
	if len(entries) != 0 {
		t.Errorf("the refused spawn made %d entries below the working directory (first %q)", len(entries), entries[0].Name())
	}
}

func TestRunHook_relativeTMPDIR_leavesOtherEventsAlone(t *testing.T) {
	r := spawnTmpdirRig(t, "rel", true)
	wd := t.TempDir()
	t.Chdir(wd)
	ws := strconv.Quote(r.ws)
	for name, raw := range map[string]string{
		"PostToolUse of a spawn": `{"hook_event_name":"PostToolUse","tool_name":"spawn_agent","session_id":"tmp-s1","cwd":` + ws +
			`,"tool_use_id":"tmp-t2","tool_input":{"message":"look around ` + SubspawnToken + `"},"tool_response":{}}`,
		"PreToolUse of another tool": `{"hook_event_name":"PreToolUse","tool_name":"exec_command","session_id":"tmp-s1","cwd":` + ws +
			`,"tool_use_id":"tmp-t3","tool_input":{"cmd":"ls"}}`,
	} {
		if answer := spawnTmpdirRun(t, r, raw); answer != "" {
			t.Errorf("%s: answer %q, want the empty answer", name, answer)
		}
	}
	entries, err := os.ReadDir(wd)
	spawnHookMust(t, err)
	if len(entries) != 0 {
		t.Errorf("other events made %d entries below the working directory", len(entries))
	}
}

// Parent decision R2-1 (Q63ANS1722): an absolute TMPDIR the hook cannot use is no longer dropped silently. The spawn is denied with
// tmploc's temp_dir_unusable form naming the directory and its source, where the base let it on without its grant.
func spawnTmpdirRequireUnusable(t *testing.T, reason, dir string) {
	t.Helper()
	if !strings.Contains(reason, "temp_dir_unusable: "+dir+" (env): ") || !strings.Contains(reason, "set TMPDIR to a writable directory") {
		t.Errorf("deny reason %q does not name temp_dir_unusable, %s and its source env", reason, dir)
	}
}

func TestSpawnHook_refusesATMPDIRThatIsARegularFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "tmpfile")
	spawnHookMust(t, os.WriteFile(file, []byte("not a directory"), 0o600))
	r := spawnTmpdirRig(t, file, true)
	answer := spawnTmpdirRun(t, r, spawnTmpdirSpawn(r.ws))
	spawnTmpdirRequireUnusable(t, spawnTmpdirDenyReason(t, answer), file)
}

func TestSpawnHook_refusesAnUnwritableTMPDIR(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a directory of mode 0500; the case needs a user the mode binds")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	spawnHookMust(t, os.Mkdir(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	r := spawnTmpdirRig(t, dir, true)
	answer := spawnTmpdirRun(t, r, spawnTmpdirSpawn(r.ws))
	spawnTmpdirRequireUnusable(t, spawnTmpdirDenyReason(t, answer), dir)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the unwritable TMPDIR holds %d entries (err %v)", len(entries), err)
	}
}

// A usable TMPDIR still mints the grant: the refusals above are about the directory, not about the spawn.
func TestSpawnHook_aUsableTMPDIRStillMints(t *testing.T) {
	r := spawnTmpdirRig(t, filepath.Join(t.TempDir(), "made-on-demand"), true)
	if answer := spawnTmpdirRun(t, r, spawnTmpdirSpawn(r.ws)); !strings.Contains(answer, "CRW-SUBSPAWN-GRANT:") {
		t.Errorf("a usable TMPDIR minted no grant: %q", answer)
	}
}
