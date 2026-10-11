package spawn

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// CRW-1257 (real-environment trial r1, S5-s6): the SessionStart notice tells the model that an explicit user request for a direct native
// spawn is not refused because the hook allows it and marks it unmanaged. This pins the hook side of that sentence for every role: a direct
// spawn with no dispatch marker is allowed, carries the unmanaged notice, denies nothing, and leaves no dispatch record or receipt behind.
func TestSpawnHookAllowsAnExplicitNativeSpawnAsUnmanagedForEveryRole(t *testing.T) {
	for _, role := range []string{"explorer", "executor", "reviewer", "architect"} {
		t.Run(role, func(t *testing.T) {
			rig := spawnHookNewRig(t, nil, spawnHookCase{})
			home, _ := rig.env("CRW_HOME")
			store := `{"roles":{"` + role + `":{"mode":"model","model":"rec/primary","fallback":{"model":"rec/fallback"}}}}`
			spawnHookMust(t, os.WriteFile(filepath.Join(home, "subagents.json"), []byte(store), 0o600))
			files := func() []string {
				var out []string
				for _, root := range []string{filepath.Dir(home)} {
					_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
						if err == nil && !d.IsDir() {
							out = append(out, p)
						}
						return nil
					})
				}
				return out
			}
			before := files()
			event := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":` + strconv.Quote(rig.ws) + `,"tool_use_id":"n1","tool_input":{"agent_type":"` + role + `","message":"native spawn, no dispatch: look around"}}`
			got := RunSpawnAttachHook(event, rig.env)
			if !strings.Contains(got, `"permissionDecision":"allow"`) || strings.Contains(got, `"deny"`) {
				t.Fatalf("an explicit native %s spawn was not allowed: %.400q", role, got)
			}
			if !strings.Contains(got, "This direct spawn is not managed by first-fallback tracking") {
				t.Errorf("the allowed %s spawn is not marked unmanaged: %.400q", role, got)
			}
			if after := files(); len(after) != len(before) {
				t.Errorf("the unmanaged %s spawn left records behind: before %v, after %v", role, before, after)
			}
		})
	}
}
