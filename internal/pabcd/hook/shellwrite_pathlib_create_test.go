package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// TestPathlibCreateRealGate runs the real PreToolUse entry points (CRW-951, fix round 1): a Python pathlib touch() or mkdir() of
// the protected memory root is refused without a grant, spends the grant once, and is refused again; a command that writes
// nothing there (a Node Path().touch() method, the text p.touch() as data, a work tree path) passes and spends nothing.
func TestPathlibCreateRealGate(t *testing.T) {
	write := []string{
		`python3 -c 'from pathlib import Path; root="{M}"; Path(root).joinpath("a").touch()'`,
		`python3 -c 'from pathlib import Path; root="{M}"; Path(root).joinpath("a").mkdir()'`,
		`python3 -c 'from pathlib import Path; m="{M}/a"; Path(m).touch()'`,
		`python3 -c 'from pathlib import Path; Path("{M}/d").mkdir(parents=True, exist_ok=True)'`,
		`python3 -c 'import os; print(f"{p.touch()}", os.path.exists("/w/x"))'`,
		`python3 -c 'from pathlib import Path; root="{M}"; Path(f"{root}/n.md").touch()'`,
		`python3 -c 'from pathlib import Path; root="{M}"; Path(f"{root}/n").mkdir()'`,
		`python3 -c 'from pathlib import Path; root="{M}"; Path(f"{root}/n.md").write_text("x")'`,
		`python3 -c 'from pathlib import Path; p=Path("{M}/n.md"); exec("p.\x74ouch()")'`,
		`python3 -c 'from pathlib import Path; p=Path("{M}/n.md"); exec("p.\x77rite_text(\"x\")")'`,
		`python3 -c 'from pathlib import Path; os=Path("{M}/d"); os.mkdir()'`,
		`python3 -c "import os; print(f\"{'a' + str(p.touch())}\", os.path.exists('{M}/n.md'))"`,
		`python3 -c 'from pathlib import Path; p=Path("{M}/n.md"); exec("\x65\x78\x65\x63\x28\x22\x70\x2e\x5c\x78\x37\x34\x6f\x75\x63\x68\x28\x29\x22\x29")'`,
		`python3 -c 'from pathlib import Path; p=Path("{M}/n.md"); exec("p.\x77rite_bytes(b\"x\")")'`,
		`python3 -c 'from pathlib import Path; p=Path("{M}/n.md"); exec("\x67etattr(p, \"write_text\")(\"x\")")'`,
	}
	pass := []string{
		`node -e 'function Path(x) { return {touch() { console.log(x) }} }; Path("{M}/a").touch()'`,
		`node -e 'function Path(x) { return {mkdir() { console.log(x) }} }; Path("{M}/d").mkdir()'`,
		`python3 -c 'import os; print("p.touch()", os.path.exists("{M}/x"))'`,
		`python3 -c 'import os; print(os.path.exists("{M}/x")) # p.mkdir()'`,
		`python3 -c 'from pathlib import Path; Path("/w/x").touch()'`,
		`python3 -c 'from pathlib import Path; print(Path("{M}/a").exists())'`,
		`python3 -c 'import os; print(f"p.touch()", os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; print("exec p.touch()", os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; print(os.path.exists("{M}/n.md")) # execute p.touch()'`,
		`python3 -c 'from pathlib import Path; Path(f"/w/x").touch()'`,
		`python3 -c 'import os; os.mkdir("/w/d")'`,
		`python3 -c "import os; print(f\"{'exec'} p.touch()\", os.path.exists('{M}/n.md'))"`,
		`python3 -c "import os; print(f\"{'p.touch()'}\", os.path.exists('{M}/n.md'))"`,
		`python3 -c "import os; print(f\"{f'p.touch()'}\", os.path.exists('{M}/n.md'))"`,
		"python3 -c \"import os\nprint(f\\\"{'x' # execute p.touch()\n}\\\", os.path.exists('{M}/n.md'))\"",
		`python3 -c 'import os; exec("print(\"p.\x74ouch()\")"); print(os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; exec("print(\"p.\x77rite_text()\")"); print(os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; exec("print(\"p.\x77rite_bytes()\")"); print(os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; exec("# p.\x77rite_text()\nprint(1)"); print(os.path.exists("{M}/n.md"))'`,
		`python3 -c 'import os; exec("print(\"\x67etattr\")"); print(os.path.exists("{M}/n.md"))'`,
	}
	for _, tmpl := range write {
		t.Run("write "+tmpl, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			cmd := strings.ReplaceAll(tmpl, "{M}", root)
			if reason := gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env)); !strings.Contains(reason, "MEMORY-WRITE-GATE") {
				t.Fatalf("no grant: reason %q", reason)
			}
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
				t.Fatalf("with a grant the write was refused: %s", out)
			}
			if state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("the grant was not spent by the write")
			}
			gateDeny(t, HandleMemoryWriteGate(gateBash(t, cwd, cmd), env))
		})
	}
	for _, tmpl := range pass {
		t.Run("pass "+tmpl, func(t *testing.T) {
			cwd, root, env := gateScene(t)
			cmd := strings.ReplaceAll(tmpl, "{M}", root)
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env); out != "" {
				t.Fatalf("refused: %s", out)
			}
			if !state.ReadState(cwd, gateSession).MemoryWriteGrant {
				t.Error("a command that writes nothing spent the grant")
			}
			if out := HandleGitHubPostGuard(gateBash(t, cwd, cmd)); out != "" {
				t.Errorf("the GitHub post guard answered %s", out)
			}
		})
	}
}
