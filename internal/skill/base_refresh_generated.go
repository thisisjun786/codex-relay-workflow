package skill

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// reconstructOutputs tests one independent original-tree witness. Every declared
// regeneration output is absent, including nonconflicts and other commands'
// outputs. Fresh input-only Git prevents recovering an output from HEAD/history.
// The command and remaining inputs are trusted, as in the existing evaluator;
// this is eligibility evidence, not a sandbox or a semantic ownership detector.
func (g *refreshGit) reconstructOutputs(ctx context.Context, tree, command string, regions []dagsched.Region, paths []string, timeout time.Duration) map[string]bool {
	ok := map[string]bool{}
	entries, err := g.treeEntries(ctx, tree)
	if err != nil {
		return ok
	}
	dir, err := os.MkdirTemp(g.dir, "witness-")
	if err != nil {
		return ok
	}
	defer os.RemoveAll(dir)
	if err := g.checkoutTree(ctx, tree, dir); err != nil {
		return ok
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ok
	}
	defer root.Close()
	for p := range entries {
		output := false
		for _, r := range regions {
			if strings.HasPrefix(r.Rule, dagsched.RuleRegeneratePref) &&
				((r.Kind == "tree" && (p == r.Path || strings.HasPrefix(p, r.Path+"/"))) || (r.Kind != "tree" && p == r.Path)) {
				output = true
			}
		}
		if output {
			if err := root.Remove(p); err != nil {
				return ok
			}
		}
	}
	// Replace metadata completely: git init on top of it retains borrowed objects.
	if err := root.RemoveAll(".git"); err != nil {
		return ok
	}
	if _, _, err := gitAt(ctx, g.isoEnv, "init", "-q", "--object-format="+g.format, dir); err != nil {
		return ok
	}
	// Force-add original tracked inputs, even when the tree's .gitignore excludes them.
	if _, _, err := gitAt(ctx, g.isoEnv, "-C", dir, "add", "-f", "--all"); err != nil {
		return ok
	}
	if _, _, err := gitAt(ctx, g.isoEnv, "-C", dir, "-c", "user.name=base-refresh", "-c", "user.email=base-refresh@example.com", "commit", "-q", "--allow-empty", "-m", "regeneration inputs"); err != nil {
		return ok
	}
	failure, err := runCommand(ctx, dir, command, timeout)
	if err != nil || failure != nil {
		return ok
	}
	for _, p := range paths {
		entry := entries[p]
		if entry.kind != "blob" || (entry.mode != "100644" && entry.mode != "100755") {
			continue
		}
		want, err := g.blob(ctx, entry.oid)
		if err != nil {
			continue
		}
		info, err := root.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		mode := "100644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		got, err := root.ReadFile(filepath.ToSlash(p))
		ok[p] = err == nil && string(got) == want && mode == entry.mode
	}
	return ok
}
