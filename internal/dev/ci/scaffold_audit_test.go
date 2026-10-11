//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scaffoldAuditScript is the one audit script: the scaffolding skill holds it and the backend skill links to it.
const (
	scaffoldAuditScript = "plugins/crw/skills/crw-dev-scaffolding/scripts/scaffold-audit.sh"
	backendScriptsDoc   = "plugins/crw/skills/crw-dev-backend/scripts/README.md"
)

// auditRun is the script's outcome on a fixture project.
type auditRun struct {
	code int
	out  string
}

func runScaffoldAudit(t *testing.T, files map[string]string) auditRun {
	t.Helper()
	project := t.TempDir()
	for name, text := range files {
		target := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := runCommand(t, project, nil, "bash", filepath.Join(repoRoot(), scaffoldAuditScript), project)
	return auditRun{got.code, got.stdout + got.stderr}
}

// have fails the test unless the output holds every line.
func (r auditRun) have(t *testing.T, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(r.out, line) {
			t.Errorf("output lacks %q:\n%s", line, r.out)
		}
	}
}

func TestScaffoldAuditMergedCoversBothCallers(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	node := map[string]string{
		"src/user/user.tool.js": "export const user = 1\n",
		"src/user/user.test.js": "test\n",
		"src/user/index.js":     "export * from './user.tool.js'\n",
		".gitignore":            "node_modules\n.env\n",
		".env.example":          "TOKEN=\n",
		"AGENTS.md":             "rules\n",
		"docs/architecture.md":  "shape\n",
	}
	with := func(base map[string]string, change map[string]string, drop ...string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range change {
			out[k] = v
		}
		for _, k := range drop {
			delete(out, k)
		}
		return out
	}
	t.Run("the script is the scaffolding skill's, and the backend skill links to it", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(repoRoot(), scaffoldAuditScript)); err != nil {
			t.Fatal(err)
		}
		doc, err := os.ReadFile(filepath.Join(repoRoot(), backendScriptsDoc))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc), "](../../crw-dev-scaffolding/scripts/scaffold-audit.sh)") {
			t.Errorf("%s does not link ../../crw-dev-scaffolding/scripts/scaffold-audit.sh", backendScriptsDoc)
		}
		if _, err := os.Stat(filepath.Join(repoRoot(), "plugins/crw/skills/crw-dev-backend/scripts/scaffold-audit.sh")); !os.IsNotExist(err) {
			t.Errorf("the backend skill still carries its own scaffold-audit.sh (%v)", err)
		}
	})
	t.Run("scaffolding caller: a Go project", func(t *testing.T) {
		goProject := map[string]string{
			"go.mod":                  "module x\n",
			"src/stock/stock.go":      "package stock\n",
			"src/stock/stock_test.go": "package stock\n",
			"README.md":               "x\n",
			".gitignore":              ".env\n",
			".env.example":            "A=\n",
			"CLAUDE.md":               "x\n",
		}
		run := runScaffoldAudit(t, goProject)
		run.have(t, "Feature-based structure", "Colocation (all Go files have tests)", "Public boundary exports (Go packages = boundaries)",
			"Source-of-truth docs/signals present", "Agent/context instructions present", "Fail: 0")
		if run.code != 0 {
			t.Errorf("exit = %d, want 0:\n%s", run.code, run.out)
		}
		bad := runScaffoldAudit(t, with(goProject, nil, "src/stock/stock_test.go"))
		bad.have(t, "Colocation (1 Go file(s) missing test)")
		if bad.code != 1 {
			t.Errorf("exit = %d, want 1:\n%s", bad.code, bad.out)
		}
		typeBased := runScaffoldAudit(t, with(goProject, map[string]string{"src/controllers/a.go": "package c\n", "src/controllers/a_test.go": "package c\n"}))
		typeBased.have(t, "Feature-based (found type-based dirs: controllers)")
	})
	t.Run("backend caller: a Node project", func(t *testing.T) {
		run := runScaffoldAudit(t, node)
		run.have(t, "Feature-based structure", "Colocation (all tools have tests)", "Public boundary exports", ".env safety (.gitignore + .env.example)",
			"Source-of-truth docs/signals present", "File length (all <500 lines)", "Agent/context instructions present", "Fail: 0")
		if run.code != 0 {
			t.Errorf("exit = %d, want 0:\n%s", run.code, run.out)
		}
	})
	t.Run("backend caller: .env safety", func(t *testing.T) {
		committed := runScaffoldAudit(t, with(node, map[string]string{".env": "TOKEN=secret\n"}))
		committed.have(t, "No .env committed (.env file found!)")
		if committed.code != 1 {
			t.Errorf("committed .env: exit = %d, want 1", committed.code)
		}
		unignored := runScaffoldAudit(t, with(node, map[string]string{".gitignore": "node_modules\n"}))
		unignored.have(t, ".env is not listed in .gitignore")
		if unignored.code != 1 {
			t.Errorf("unignored .env: exit = %d, want 1:\n%s", unignored.code, unignored.out)
		}
		noExample := runScaffoldAudit(t, with(node, nil, ".env.example"))
		noExample.have(t, ".env.example missing")
		if noExample.code != 0 {
			t.Errorf("missing .env.example is a warning: exit = %d, want 0", noExample.code)
		}
	})
	t.Run("backend caller: the missing pieces", func(t *testing.T) {
		noBoundary := runScaffoldAudit(t, with(node, nil, "src/user/index.js"))
		noBoundary.have(t, "Public boundary exports (1 feature(s) missing index.js/index.ts/__init__.py)")
		if noBoundary.code != 1 {
			t.Errorf("exit = %d, want 1", noBoundary.code)
		}
		noTest := runScaffoldAudit(t, with(node, nil, "src/user/user.test.js"))
		noTest.have(t, "Colocation (1 tool(s) missing colocated test)")
		bare := runScaffoldAudit(t, with(node, nil, "AGENTS.md", "docs/architecture.md"))
		bare.have(t, "Source-of-truth docs/signals absent (optional for small repos)", "Agent/context instructions missing")
		if bare.code != 0 {
			t.Errorf("missing docs and instructions are warnings: exit = %d, want 0:\n%s", bare.code, bare.out)
		}
		long := runScaffoldAudit(t, with(node, map[string]string{"src/user/user.tool.js": strings.Repeat("x\n", 501)}))
		long.have(t, "File length (<500L) — violations: user.tool.js:501L")
	})
	t.Run("the summary and the exit code", func(t *testing.T) {
		run := runScaffoldAudit(t, node)
		run.have(t, "Pass: ", "Score: ")
	})
}
