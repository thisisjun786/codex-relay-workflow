package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Carried scripts admitted by CRW-1085 retain CRW-951's directory proof and
// environment when CRW-1104's cached analysis feeds the memory gate.
func TestRefreshCarriedPathlibDirectory(t *testing.T) {
	for _, method := range []string{"touch", "mkdir"} {
		for _, scene := range []struct {
			name, prefix string
			protected    bool
		}{
			{"protected-cwd", "", true},
			{"failed-cd", "cd '{W}/absent'; ", true},
			{"success-cd", "cd '{W}' && ", false},
		} {
			for _, nested := range []bool{false, true} {
				t.Run(method+"/"+scene.name+map[bool]string{false: "/script", true: "/child"}[nested], func(t *testing.T) {
					work, root, env := gateScene(t)
					if err := os.MkdirAll(root, 0o755); err != nil {
						t.Fatal(err)
					}
					body := strings.ReplaceAll(scene.prefix, "{W}", work) + `python3 -c 'from pathlib import Path; name="x"; Path(name).` + method + `()'`
					script := filepath.Join(root, "writer.sh")
					if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}
					command := "sh '" + script + "'"
					if nested {
						parent := filepath.Join(root, "parent.sh")
						if err := os.WriteFile(parent, []byte("#!/bin/sh\n./writer.sh\n"), 0o755); err != nil {
							t.Fatal(err)
						}
						command = "sh '" + parent + "'"
					}
					payload := gateBash(t, root, command)
					if out := HandleMemoryWriteGate(payload, env); strings.Contains(out, "MEMORY-WRITE-GATE") != scene.protected {
						t.Fatalf("without grant: %q, want protected=%v", out, scene.protected)
					}
					gateSeed(t, root, func(s *state.State) { s.MemoryWriteGrant = true })
					if out := HandleMemoryWriteGate(payload, env); out != "" {
						t.Fatalf("with grant: %q", out)
					}
					if kept := state.ReadState(root, gateSession).MemoryWriteGrant; kept == scene.protected {
						t.Fatalf("grant kept=%v, want %v", kept, !scene.protected)
					}
					if out := HandleMemoryWriteGate(payload, env); strings.Contains(out, "MEMORY-WRITE-GATE") != scene.protected {
						t.Fatalf("after grant: %q, want protected=%v", out, scene.protected)
					}
				})
			}
		}
	}
}
