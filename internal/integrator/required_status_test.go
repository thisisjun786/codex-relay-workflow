package integrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

func rsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

type requiredStatusWorld struct {
	repo, bare, head, base string
	fake                   *trackingfake.Server
}

func newRequiredStatusWorld(t *testing.T) *requiredStatusWorld {
	t.Helper()
	root := t.TempDir()
	w := &requiredStatusWorld{repo: filepath.Join(root, "repo"), bare: filepath.Join(root, "bare.git"), fake: trackingfake.New(t, "o/r")}
	rsGit(t, root, "init", "-q", w.repo)
	rsGit(t, root, "init", "-q", "--bare", w.bare)
	rsGit(t, w.repo, "config", "user.name", "t")
	rsGit(t, w.repo, "config", "user.email", "t@x")
	for _, f := range []string{"f.txt", "go.mod"} {
		body := "base\n"
		if f == "go.mod" {
			body = "module example.invalid/x\n\ngo 1.27\n"
		}
		if err := os.WriteFile(filepath.Join(w.repo, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		rsGit(t, w.repo, "add", f)
	}
	rsGit(t, w.repo, "commit", "-qm", "base")
	w.base = rsGit(t, w.repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(w.repo, "f.txt"), []byte("head\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rsGit(t, w.repo, "commit", "-qam", "head")
	w.head = rsGit(t, w.repo, "rev-parse", "HEAD")
	rsGit(t, w.repo, "remote", "add", "dest", w.bare)
	w.fake.CommitExists = func(head string) bool {
		cmd := exec.Command("git", "-C", w.bare, "cat-file", "-e", head+"^{commit}")
		return cmd.Run() == nil
	}
	return w
}

func (w *requiredStatusWorld) record(t *testing.T, head, base string) []byte {
	t.Helper()
	keys, err := dagsched.CommitVerificationKeys(context.Background(), w.repo, head)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]string{}
	for name, v := range keys.Pins {
		tools[name] = v
	}
	raw, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{
		Runner: "local", Repository: "o/r", BaseCommit: base, HeadCommit: head, TreeHash: keys.Tree,
		CiDigest: keys.CiDigest, Tools: tools, Pins: keys.Pins,
		Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass",
		PinMismatch: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (w *requiredStatusWorld) gitSeam(ctx context.Context, dir string, args ...string) (string, error) {
	if strings.Join(args, " ") == "remote get-url --push --all dest" {
		return "https://github.com/o/r.git", nil
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", errors.New("git failed: " + string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

func (w *requiredStatusWorld) publisher(ctxName string) RequiredStatusPublisher {
	return RequiredStatusPublisher{Context: ctxName, Git: w.gitSeam, GitHub: func(repo string) (*tracking.GitHub, error) {
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(w.fake.URL))
	}}
}

func TestRequiredStatusPublishStagesAndPosts(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	if err := w.publisher("dev-gate").Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := rsGit(t, w.bare, "rev-parse", "refs/heads/ci/required-status/"+w.head); got != w.head {
		t.Errorf("staging ref = %s, want %s", got, w.head)
	}
	statuses := w.fake.Statuses(w.head)
	if len(statuses) != 1 || statuses[0].Context != "dev-gate" || statuses[0].State != "success" || statuses[0].TargetURL != "" {
		t.Errorf("statuses = %+v, want one success dev-gate with no target URL", statuses)
	}
}

func TestRequiredStatusRefusesABadRecord(t *testing.T) {
	w := newRequiredStatusWorld(t)
	other := w.record(t, w.base, w.base)
	if err := w.publisher("dev-gate").Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, other); err == nil {
		t.Fatal("a record of another head published a status")
	}
	if got := w.fake.Statuses(w.head); len(got) != 0 {
		t.Errorf("statuses = %+v, want none", got)
	}
	if code := exec.Command("git", "-C", w.bare, "rev-parse", "--verify", "-q", "refs/heads/ci/required-status/"+w.head).Run(); code == nil {
		t.Error("the staging ref was pushed for an invalid record")
	}
}

func TestRequiredStatusDisabledIsFree(t *testing.T) {
	w := newRequiredStatusWorld(t)
	if err := w.publisher("").Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, nil); err != nil {
		t.Fatalf("empty context returned %v", err)
	}
	if got := w.fake.Statuses(w.head); len(got) != 0 {
		t.Errorf("statuses = %+v, want none", got)
	}
}

func TestRequiredStatusSHA(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("a", 40):                  true,
		strings.Repeat("b", 64):                  true,
		strings.Repeat("a", 41):                  false,
		strings.Repeat("b", 63):                  false,
		strings.Repeat("b", 65):                  false,
		strings.Repeat("g", 40):                  false,
		strings.ToUpper(strings.Repeat("a", 40)): false,
		"":                                       false,
		"refs/heads/x":                           false,
	} {
		if got := requiredStatusSHA(s); got != want {
			t.Errorf("requiredStatusSHA(len=%d %q) = %v, want %v", len(s), s[:min(10, len(s))], got, want)
		}
	}
}

func TestRequiredStatusSHA256Repo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if out, err := exec.Command("git", "init", "-q", "--object-format=sha256", repo).CombinedOutput(); err != nil {
		t.Fatalf("sha256 repo: %v\n%s", err, out)
	}
	bare := filepath.Join(root, "bare.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "--object-format=sha256", bare).CombinedOutput(); err != nil {
		t.Fatalf("sha256 bare: %v\n%s", err, out)
	}
	rsGit(t, repo, "config", "user.name", "t")
	rsGit(t, repo, "config", "user.email", "t@x")
	rsGit(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	base := rsGit(t, repo, "rev-parse", "HEAD")
	rsGit(t, repo, "commit", "-q", "--allow-empty", "-m", "head")
	head := rsGit(t, repo, "rev-parse", "HEAD")
	if len(base) != 64 || len(head) != 64 {
		t.Fatalf("sha256 repo SHAs are %d/%d chars", len(base), len(head))
	}
	w := &requiredStatusWorld{repo: repo, bare: bare, head: head, base: base, fake: trackingfake.New(t, "o/r")}
	rsGit(t, repo, "remote", "add", "dest", bare)
	w.fake.CommitExists = func(h string) bool {
		return exec.Command("git", "-C", bare, "cat-file", "-e", h+"^{commit}").Run() == nil
	}
	raw := w.record(t, head, base)
	if err := w.publisher("dev-gate").Publish(context.Background(), repo, "dest", "dev", head, base, raw); err != nil {
		t.Fatalf("Publish on a sha256 repo: %v", err)
	}
	if got := w.fake.Statuses(head); len(got) != 1 || got[0].State != "success" {
		t.Errorf("statuses = %+v", got)
	}
}

func TestRequiredStatusDestinations(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	urls := map[string]string{
		"multi":      "https://github.com/o/r.git\nhttps://github.com/other/repo.git",
		"not github": "file:///tmp/x.git",
		"empty":      "",
	}
	for name, u := range urls {
		p := RequiredStatusPublisher{Context: "dev-gate", Git: func(ctx context.Context, dir string, args ...string) (string, error) {
			if strings.Join(args, " ") == "remote get-url --push --all dest" {
				return u, nil
			}
			return w.gitSeam(ctx, dir, args...)
		}, GitHub: w.publisher("dev-gate").GitHub}
		if err := p.Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err == nil {
			t.Errorf("%s: destinations %q published", name, u)
		}
		if got := w.fake.Statuses(w.head); len(got) != 0 {
			t.Fatalf("%s: statuses = %+v", name, got)
		}
	}
}

func TestRequiredStatusLiteralURL(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	calls := 0
	p := RequiredStatusPublisher{Context: "dev-gate", Git: func(ctx context.Context, dir string, args ...string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "remote get-url") {
			calls++
		}
		if strings.Join(args, " ") == "push --no-follow-tags https://github.com/o/r.git "+w.head+":refs/heads/ci/required-status/"+w.head {
			_, err := w.gitSeam(ctx, dir, "push", "--no-follow-tags", w.bare, w.head+":refs/heads/ci/required-status/"+w.head)
			return "", err
		}
		return w.gitSeam(ctx, dir, args...)
	}, GitHub: w.publisher("dev-gate").GitHub}
	if err := p.Publish(context.Background(), w.repo, "https://github.com/o/r.git", "dev", w.head, w.base, raw); err != nil {
		t.Fatalf("a literal GitHub URL destination: %v", err)
	}
	if calls != 0 {
		t.Errorf("the literal URL destination still ran %d remote lookups", calls)
	}
	if got := w.fake.Statuses(w.head); len(got) != 1 || got[0].State != "success" {
		t.Errorf("statuses = %+v", got)
	}
}

func TestRequiredStatusStageFailure(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	p := RequiredStatusPublisher{Context: "dev-gate", Git: func(ctx context.Context, dir string, args ...string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "push ") {
			return "", errors.New("push rejected")
		}
		return w.gitSeam(ctx, dir, args...)
	}, GitHub: w.publisher("dev-gate").GitHub}
	if err := p.Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err == nil {
		t.Fatal("a failed staging push published a status")
	}
	if got := w.fake.Statuses(w.head); len(got) != 0 {
		t.Errorf("statuses = %+v", got)
	}
}

func TestRequiredStatusStagingCollision(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	for _, branch := range []string{"ci/required-status/" + w.head, "refs/heads/ci/required-status/" + w.head} {
		if err := w.publisher("dev-gate").Publish(context.Background(), w.repo, "dest", branch, w.head, w.base, raw); err == nil {
			t.Errorf("target %q accepted", branch)
		}
	}
}

func TestRequiredStatusContextInvalid(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	for _, ctx2 := range []string{" dev-gate", "dev\u0007gate", "dev gate\t"} {
		if err := w.publisher(ctx2).Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err == nil {
			t.Errorf("context %q published", ctx2)
		}
		if got := w.fake.Statuses(w.head); len(got) != 0 {
			t.Fatalf("context %q: statuses = %+v", ctx2, got)
		}
	}
}

func TestRequiredStatusErrorsDoNotEcho(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	err := w.publisher("dev-gate").Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, []byte("not json"))
	if err == nil {
		t.Fatal("a malformed record published")
	}
	p := RequiredStatusPublisher{Context: "dev-gate", Git: func(ctx context.Context, dir string, args ...string) (string, error) {
		if strings.HasPrefix(strings.Join(args, " "), "merge-base") || strings.HasPrefix(strings.Join(args, " "), "push ") {
			return "", errors.New("fatal: could not read password for https://secret@example.invalid")
		}
		return w.gitSeam(ctx, dir, args...)
	}, GitHub: w.publisher("dev-gate").GitHub}
	if err := p.Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err == nil {
		t.Fatal("a refused git call published")
	} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "example.invalid") {
		t.Errorf("the error echoes git output: %v", err)
	}
}

func TestRequiredStatusUnknownLocalFields(t *testing.T) {
	w := newRequiredStatusWorld(t)
	raw := w.record(t, w.head, w.base)
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["future_verifier_field"] = map[string]any{"kept": true}
	delete(fields, "digest")
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := dagsched.VerificationRecordDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	fields["digest"] = digest
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := dagsched.DecodeVerificationRecord(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Raw, raw) {
		t.Fatal("the decoded record does not carry the exact receipt bytes")
	}
	if err := w.publisher("dev-gate").Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, decoded.Raw); err != nil {
		t.Fatalf("a receipt with unknown members was refused: %v", err)
	}
	if got := w.fake.Statuses(w.head); len(got) != 1 || got[0].State != "success" {
		t.Errorf("statuses = %+v", got)
	}
}

func TestRequiredStatusInvalidReceipts(t *testing.T) {
	mutate := func(t *testing.T, w *requiredStatusWorld, fields map[string]any) []byte {
		t.Helper()
		raw := w.record(t, w.head, w.base)
		if fields == nil {
			return raw
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		delete(doc, "digest")
		for k, v := range fields {
			doc[k] = v
		}
		body, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if _, tampered := fields["digest"]; !tampered {
			digest, err := dagsched.VerificationRecordDigest(body)
			if err != nil {
				t.Fatal(err)
			}
			doc["digest"] = digest
			body, err = json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
		}
		return body
	}
	cases := map[string]func(t *testing.T, w *requiredStatusWorld) []byte{
		"missing":   func(t *testing.T, w *requiredStatusWorld) []byte { return nil },
		"malformed": func(t *testing.T, w *requiredStatusWorld) []byte { return []byte("not json") },
		"failed": func(t *testing.T, w *requiredStatusWorld) []byte {
			return mutate(t, w, map[string]any{"result": "fail"})
		},
		"wrong head": func(t *testing.T, w *requiredStatusWorld) []byte {
			return mutate(t, w, map[string]any{"headCommit": w.base})
		},
		"wrong base": func(t *testing.T, w *requiredStatusWorld) []byte {
			return mutate(t, w, map[string]any{"baseCommit": w.head})
		},
		"wrong tree": func(t *testing.T, w *requiredStatusWorld) []byte {
			return mutate(t, w, map[string]any{"treeHash": strings.Repeat("0", 40)})
		},
		"tampered digest": func(t *testing.T, w *requiredStatusWorld) []byte {
			return mutate(t, w, map[string]any{"digest": "sha256:" + strings.Repeat("0", 64)})
		},
	}
	wantReason := map[string]string{
		"failed":          `result is "fail"`,
		"wrong head":      "verification head is not the integration head",
		"wrong base":      "covers base",
		"wrong tree":      "names tree",
		"tampered digest": "does not match its contents",
	}
	for name, make := range cases {
		t.Run(name, func(t *testing.T) {
			w := newRequiredStatusWorld(t)
			raw := make(t, w)
			pushes := 0
			p := RequiredStatusPublisher{Context: "dev-gate", Git: func(ctx context.Context, dir string, args ...string) (string, error) {
				if len(args) > 0 && args[0] == "push" {
					pushes++
				}
				return w.gitSeam(ctx, dir, args...)
			}, GitHub: w.publisher("dev-gate").GitHub}
			if err := p.Publish(context.Background(), w.repo, "dest", "dev", w.head, w.base, raw); err == nil {
				t.Fatalf("receipt %q published", name)
			} else if want := wantReason[name]; want != "" && !strings.Contains(err.Error(), want) {
				t.Fatalf("receipt %q refused for %v, want %q", name, err, want)
			}
			if pushes != 0 {
				t.Errorf("receipt %q ran %d pushes", name, pushes)
			}
			if len(w.fake.Requests()) != 0 {
				t.Errorf("receipt %q made API calls: %v", name, w.fake.Requests())
			}
		})
	}
}
