package integrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

func TestRequiredStatus1632InheritedGitConfigUsesPushSources(t *testing.T) {
	const original = "https://github.com/from/project.git"
	const destination = "https://github.com/o/r.git"
	for _, mode := range []string{"empty", "decoy"} {
		t.Run(mode, func(t *testing.T) {
			rewriteConfig1624(t)
			w := newRequiredStatusWorld(t)
			raw := w.record(t, w.head, w.base)
			dir := t.TempDir()
			global := filepath.Join(dir, "global.gitconfig")
			if err := os.WriteFile(global, []byte("[url \""+destination+"\"]\n\tpushInsteadOf = "+original+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			inherited := os.DevNull
			if mode == "decoy" {
				inherited = filepath.Join(dir, "decoy.gitconfig")
				if err := os.WriteFile(inherited, []byte("[url \"https://example.invalid/wrong/project.git\"]\n\tpushInsteadOf = "+original+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			t.Setenv("GIT_CONFIG", inherited)
			config := filepath.Join(w.repo, ".git", "config")
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if actual := literalPushTarget1631(t, w, original); actual != destination {
				t.Fatalf("actual push uses %q, want %q", actual, destination)
			}
			r := NewRunner(Config{Git: "git"})
			p := w.publisher("dev-gate")
			pushes := 0
			p.Git = func(ctx context.Context, checkout string, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "push" {
					pushes++
					if len(args) != 4 || args[1] != "--no-follow-tags" || args[2] != original || args[3] != w.head+":refs/heads/ci/required-status/"+w.head {
						return "", errors.New("unexpected staging push")
					}
					args = append([]string(nil), args...)
					args[2] = w.bare
				}
				return r.gitOut(ctx, checkout, args...)
			}
			var clients []string
			github := p.GitHub
			p.GitHub = func(repo string) (*tracking.GitHub, error) {
				clients = append(clients, repo)
				return github(repo)
			}
			if err := p.Publish(context.Background(), w.repo, original, "dev", w.head, w.base, raw); err != nil {
				t.Fatalf("inherited GIT_CONFIG refused a valid push destination: %v", err)
			}
			if pushes != 1 || strings.Join(clients, ",") != "o/r" {
				t.Fatalf("wrong destination: pushes=%d clients=%v", pushes, clients)
			}
			if statuses := w.fake.Statuses(w.head); len(statuses) != 1 || statuses[0].State != "success" || statuses[0].Context != "dev-gate" {
				t.Fatalf("destination statuses = %+v", statuses)
			}
			if got := rsGit(t, w.bare, "rev-parse", "refs/heads/ci/required-status/"+w.head); got != w.head {
				t.Fatalf("staged %s, want %s", got, w.head)
			}
			after, err := os.ReadFile(config)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("persistent git config changed: %v", err)
			}
		})
	}
}
