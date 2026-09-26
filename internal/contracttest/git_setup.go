package contracttest

import (
	"context"
	"os"
	"path/filepath"
)

func (r *gitBridgeRun) hostileGit() {
	marker := filepath.Join(r.root, "unexpected-effect")
	config := filepath.Join(r.root, "conditional-config")
	if err := os.WriteFile(config, []byte("[filter \"conditional\"]\n smudge = touch "+marker+"; cat\n"), 0600); err != nil {
		r.t.Fatal(err)
	}
	if _, err := gitCommand(context.Background(), r.repo, "config", "includeIf.gitdir:"+r.repo+"/.git/worktrees/.path", config); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.repo, ".gitattributes"), []byte("tracked filter=example\n"), 0600); err != nil {
		r.t.Fatal(err)
	}
	if _, err := gitCommand(context.Background(), r.repo, "add", ".gitattributes"); err != nil {
		r.t.Fatal(err)
	}
	if _, err := gitCommand(context.Background(), r.repo, "commit", "-m", "attributes"); err != nil {
		r.t.Fatal(err)
	}
	r.base = gitValue(r.t, context.Background(), r.repo, "rev-parse", "HEAD")
	r.args.Revision = r.base
	script := filepath.Join(r.repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0700); err != nil {
		r.t.Fatal(err)
	}
	for _, name := range []string{"filter.example.smudge", "filter.example.clean", "core.fsmonitor"} {
		if _, err := gitCommand(context.Background(), r.repo, "config", name, script); err != nil {
			r.t.Fatal(err)
		}
	}
	if _, err := gitCommand(context.Background(), r.repo, "config", "filter.example.required", "true"); err != nil {
		r.t.Fatal(err)
	}
	_ = os.Remove(marker)
}
