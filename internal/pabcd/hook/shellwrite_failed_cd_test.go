package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func TestPathlibCreateFailedCD(t *testing.T) {
	for _, method := range []string{"touch", "mkdir"} {
		for _, receiver := range []string{`p`, `Path("x")`, `Path(name)`} {
			for _, scene := range []struct {
				name, prefix       string
				outside, protected bool
			}{
				{"semicolon", "cd '{D}'; ", false, true},
				{"newline", "cd '{D}'\n", false, true},
				{"or-true", "cd '{D}' || true; ", false, true},
				{"subshell", "(cd '{D}'); ", false, true},
				{"function", "f() { cd '{D}'; true; }; f; ", false, true},
				{"block-and", "{ cd '{D}'; true; } && ", false, true},
				{"negated-and", "! cd '{D}' && ", false, true},
				{"after-and-list", "cd '{D}' && true; ", false, true},
				{"or-list-and", "cd '{D}' || true && ", false, true},
				{"outside-cwd", "", true, false},
				{"and", "cd '{W}' && ", false, false},
				{"and-absent", "cd '{D}' && ", false, false},
				{"and-chain", "cd '{W}' && true && ", false, false},
				{"and-two-cd", "cd '{W}' && cd . && ", false, false},
				{"and-memory", "cd '{M}' && ", true, true},
			} {
				t.Run(method+"/"+receiver+"/"+scene.name, func(t *testing.T) {
					work, root, env := gateScene(t)
					if err := os.MkdirAll(root, 0o755); err != nil {
						t.Fatal(err)
					}
					absent := filepath.Join(t.TempDir(), "absent-directory")
					prefix := strings.NewReplacer("{D}", absent, "{W}", work, "{M}", root).Replace(scene.prefix)
					cwd := root
					if scene.outside {
						cwd = work
					}
					command := prefix + `python3 -c 'from pathlib import Path; name="x"; p=Path("x"); ` + receiver + "." + method + "()'"
					payload := gateBash(t, cwd, command)
					if out := HandleMemoryWriteGate(payload, env); strings.Contains(out, "MEMORY-WRITE-GATE") != scene.protected {
						t.Errorf("without grant: %q, want protected=%v", out, scene.protected)
					}
					gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
					if out := HandleMemoryWriteGate(payload, env); out != "" {
						t.Errorf("with grant: %q", out)
					}
					if kept := state.ReadState(cwd, gateSession).MemoryWriteGrant; kept == scene.protected {
						t.Errorf("grant kept=%v, want %v", kept, !scene.protected)
					}
					if out := HandleMemoryWriteGate(payload, env); strings.Contains(out, "MEMORY-WRITE-GATE") != scene.protected {
						t.Errorf("after grant: %q, want protected=%v", out, scene.protected)
					}
					if receiver == "p" && (scene.name == "semicolon" || scene.name == "newline" || scene.name == "or-true" || scene.name == "function" || scene.name == "block-and") {
						run := exec.Command("bash", "--noprofile", "--norc", "-c", command)
						run.Dir = cwd
						if out, err := run.CombinedOutput(); err != nil {
							t.Fatalf("real shell: %v: %s", err, out)
						}
						info, err := os.Stat(filepath.Join(root, "x"))
						if err != nil {
							t.Fatal(err)
						}
						if info.IsDir() != (method == "mkdir") {
							t.Fatal("wrong destination type")
						}
						t.Log("real shell exited 0 and created the protected destination after failed cd")
					}
				})
			}
		}
	}
}
