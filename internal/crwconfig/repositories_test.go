package crwconfig

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestRepositoryCommandsSelectPathCommonWorktreeAndSlug(t *testing.T) {
	repo, wt := t.TempDir(), filepath.Join(t.TempDir(), "wt")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "dev")
	git("-c", "user.name=test", "-c", "user.email=test@invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "base")
	git("remote", "add", "origin", "git@github.com:owner/project.git")
	git("worktree", "add", "--detach", wt)
	config := filepath.Join(t.TempDir(), "config.json")
	getenv := func(k string) string {
		if k == "CRW_CONFIG" {
			return config
		}
		return ""
	}
	write := func(entries map[string]RepositoryCommands) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"repositories": entries})
		if err := os.WriteFile(config, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries := map[string]RepositoryCommands{"owner/project": {VerifyCommand: "node scripts/foundation.mjs"}, repo: {VerifyCommand: "sh path-verify", IntegrateCommand: "sh integrate"}}
	write(entries)
	for _, path := range []string{repo, wt} {
		c, err := LoadRepositoryCommands(context.Background(), getenv, path)
		if err != nil || c.VerifyCommand != "sh path-verify" || c.IntegrateCommand != "sh integrate" {
			t.Fatalf("path %s: %+v %v", path, c, err)
		}
	}
	delete(entries, repo)
	write(entries)
	c, err := LoadRepositoryCommands(context.Background(), getenv, wt)
	if err != nil || c.VerifyCommand != "node scripts/foundation.mjs" {
		t.Fatalf("slug: %+v %v", c, err)
	}
	git("remote", "set-url", "origin", "https://github.com/owner/project.git")
	c, err = LoadRepositoryCommands(context.Background(), getenv, wt)
	if err != nil || c.Key != "owner/project" {
		t.Fatalf("https slug: %+v %v", c, err)
	}
	// A plan may name a linked checkout. Its node worktrees still belong to
	// that repository, even though their common directory lives at repo/.git.
	other := filepath.Join(t.TempDir(), "node")
	git("worktree", "add", "--detach", other)
	entries[wt] = RepositoryCommands{VerifyCommand: "sh linked-verify"}
	write(entries)
	c, err = LoadRepositoryCommands(context.Background(), getenv, other)
	if err != nil || c.VerifyCommand != "sh linked-verify" {
		t.Fatalf("linked repository path: %+v %v", c, err)
	}
	write(map[string]RepositoryCommands{})
	c, err = LoadRepositoryCommands(context.Background(), getenv, repo)
	if err != nil || c != (RepositoryCommands{}) {
		t.Fatalf("fallback: %+v %v", c, err)
	}
}

func TestRepositoryCommandsRejectMalformedOrEmptyCommands(t *testing.T) {
	for _, section := range []string{`null`, `[]`, `{"owner/repo":null}`, `{"owner/repo":{"verify_command":" "}}`, `{"owner/repo":{"integrate_command":null}}`, `{"owner/repo":{"verify_command":3}}`, `{"bad":{"verify_command":"true"}}`} {
		path := configWrite(t, `{"repositories":`+section+`}`)
		if _, err := LoadRepositoryCommands(context.Background(), func(k string) string {
			if k == "CRW_CONFIG" {
				return path
			}
			return ""
		}, t.TempDir()); err == nil {
			t.Errorf("accepted repositories %s", section)
		}
	}
}
