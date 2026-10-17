package integrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

func rewriteConfig1624(t *testing.T, pairs ...[2]string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(pairs)))
	for i, pair := range pairs {
		t.Setenv("GIT_CONFIG_KEY_"+strconv.Itoa(i), pair[0])
		t.Setenv("GIT_CONFIG_VALUE_"+strconv.Itoa(i), pair[1])
	}
}

func rewritePublisher1624(t *testing.T, w *requiredStatusWorld, remote string, pushes *int, clients *[]string) (RequiredStatusPublisher, *trackingfake.Server) {
	t.Helper()
	source := trackingfake.New(t, "from/project")
	source.CommitExists = w.fake.CommitExists
	p := w.publisher("dev-gate")
	p.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "push" {
			*pushes++
			if len(args) != 4 || args[1] != "--no-follow-tags" || args[2] != remote || args[3] != w.head+":refs/heads/ci/required-status/"+w.head {
				return "", errors.New("unexpected staging push")
			}
			return w.gitSeam(ctx, dir, "push", "--no-follow-tags", w.bare, args[3])
		}
		return w.gitSeam(ctx, dir, args...)
	}
	p.GitHub = func(repo string) (*tracking.GitHub, error) {
		*clients = append(*clients, repo)
		server := w.fake
		if repo == "from/project" {
			server = source
		}
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(server.URL))
	}
	return p, source
}

func TestRequiredStatus1624LiteralPushRewritePublishesToDestination(t *testing.T) {
	const original = "https://github.com/from/project.git"
	const destination = "https://github.com/o/r.git"
	for _, tc := range []struct {
		name, remote string
		pairs        [][2]string
	}{
		{"https pushInsteadOf", original, [][2]string{{"url." + destination + ".pushInsteadOf", original}}},
		{"scp pushInsteadOf", "git@github.com:from/project.git", [][2]string{{"url." + destination + ".pushInsteadOf", "git@github.com:from/project.git"}}},
		{"ssh pushInsteadOf", "ssh://git@github.com/from/project.git", [][2]string{{"url.git@github.com:o/r.git.pushInsteadOf", "ssh://git@github.com/from/project.git"}}},
		{"longest prefix", original, [][2]string{{"url.https://github.com/wrong/.pushInsteadOf", "https://github.com/"}, {"url." + destination + ".pushInsteadOf", original}}},
		{"insteadOf", original, [][2]string{{"url." + destination + ".insteadOf", original}}},
		{"push beats fetch rewrite", original, [][2]string{{"url.https://github.com/wrong/project.git.insteadOf", original}, {"url." + destination + ".pushInsteadOf", original}}},
		{"probe name already configured", original, [][2]string{{"url." + destination + ".pushInsteadOf", original}, {"remote.crw-required-status-url.url", "https://github.com/stale/fetch.git"}, {"remote.crw-required-status-url.pushurl", "https://github.com/stale/push.git"}}},
		{"no rewrite", destination, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newRequiredStatusWorld(t)
			raw := w.record(t, w.head, w.base)
			rewriteConfig1624(t, tc.pairs...)
			config := filepath.Join(w.repo, ".git", "config")
			before, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			pushes := 0
			var clients []string
			p, source := rewritePublisher1624(t, w, tc.remote, &pushes, &clients)
			if err := p.Publish(context.Background(), w.repo, tc.remote, "dev", w.head, w.base, raw); err != nil {
				t.Fatal(err)
			}
			if strings.Join(clients, ",") != "o/r" || pushes != 1 {
				t.Fatalf("published to %v with %d pushes; want only actual destination o/r", clients, pushes)
			}
			if got := w.fake.Statuses(w.head); len(got) != 1 || got[0].State != "success" || got[0].Context != "dev-gate" {
				t.Fatalf("destination statuses = %+v", got)
			}
			if got := source.Statuses(w.head); len(got) != 0 {
				t.Fatalf("literal source received status: %+v", got)
			}
			if got := rsGit(t, w.bare, "rev-parse", "refs/heads/ci/required-status/"+w.head); got != w.head {
				t.Fatalf("staged %s, want %s", got, w.head)
			}
			after, err := os.ReadFile(config)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("URL probe changed persistent git config: %v", err)
			}
		})
	}
}

func TestRequiredStatus1624RefusesNonGitHubPushRewrite(t *testing.T) {
	for _, destination := range []string{"https://example.invalid/o/r.git", "file:///tmp/not-a-github-repository.git"} {
		t.Run(destination, func(t *testing.T) {
			const original = "https://github.com/from/project.git"
			w := newRequiredStatusWorld(t)
			raw := w.record(t, w.head, w.base)
			rewriteConfig1624(t, [2]string{"url." + destination + ".pushInsteadOf", original})
			pushes := 0
			var clients []string
			p, source := rewritePublisher1624(t, w, original, &pushes, &clients)
			err := p.Publish(context.Background(), w.repo, original, "dev", w.head, w.base, raw)
			if err == nil || pushes != 0 || len(clients) != 0 || len(source.Statuses(w.head)) != 0 || len(w.fake.Statuses(w.head)) != 0 {
				t.Fatalf("unsafe destination accepted: err=%v pushes=%d clients=%v", err, pushes, clients)
			}
		})
	}
}

func TestRequiredStatus1624UnreadableLiteralDestinationIsClosed(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "empty output"
		if fail {
			name = "git failure"
		}
		t.Run(name, func(t *testing.T) {
			const original = "https://github.com/from/project.git"
			w := newRequiredStatusWorld(t)
			raw := w.record(t, w.head, w.base)
			pushes := 0
			var clients []string
			p, _ := rewritePublisher1624(t, w, original, &pushes, &clients)
			git := p.Git
			p.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "-c" {
					if fail {
						return "", errors.New("private lookup detail")
					}
					return "", nil
				}
				return git(ctx, dir, args...)
			}
			err := p.Publish(context.Background(), w.repo, original, "dev", w.head, w.base, raw)
			if err == nil || pushes != 0 || len(clients) != 0 {
				t.Fatalf("unresolved destination accepted: err=%v pushes=%d clients=%v", err, pushes, clients)
			}
			if strings.Contains(err.Error(), "private lookup detail") {
				t.Fatalf("git output leaked: %v", err)
			}
		})
	}
}
