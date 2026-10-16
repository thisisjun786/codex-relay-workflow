package publication

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

// Repository reads only github.com remotes, whatever the spelling; anything else is "".
func TestRepositoryParsesOnlyGitHubRemotes(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r":          "o/r",
		"https://github.com/o/r.git":      "o/r",
		"ssh://git@github.com/o/r":        "o/r",
		"ssh://git@github.com/o/r.git":    "o/r",
		"git@github.com:o/r":              "o/r",
		"git@github.com:o/r.git":          "o/r",
		"https://gitlab.com/o/r":          "",
		"https://github.com.evil.com/o/r": "",
		"git://github.com/o/r":            "",
		"ftp://github.com/o/r":            "",
		"https://github.com/o/r?x=1":      "",
		"https://github.com/o/r#frag":     "",
		"/local/path/o/r":                 "",
		"https://github.com/":             "",
		"":                                "",
	} {
		if got := Repository(in); got != want {
			t.Errorf("Repository(%q) = %q, want %q", in, got, want)
		}
	}
}

// ForCheckout resolves the repository from GH_REPO in its forms, else the origin remote of the
// checkout, and a checkout that cannot resolve one gets the defaults, not an error.
func TestForCheckout(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--bare")

	// GH_REPO wins: OWNER/NAME, github.com/OWNER/NAME and the URL all resolve
	for _, form := range []string{"o/r", "github.com/o/r", "https://github.com/o/r"} {
		t.Setenv("GH_REPO", form)
		repo, p, err := ForCheckout(context.Background(), dir)
		if err != nil || repo != "o/r" {
			t.Fatalf("GH_REPO %q: repo %q err %v", form, repo, err)
		}
		if !reflect.DeepEqual(p, publishpolicy.Policy{}) {
			t.Errorf("policy %+v", p)
		}
	}
	// no GH_REPO: the origin remote of the checkout is read
	os.Unsetenv("GH_REPO")
	git("remote", "add", "origin", "git@github.com:o2/r2.git")
	repo, _, err := ForCheckout(context.Background(), dir)
	if err != nil || repo != "o2/r2" {
		t.Errorf("remote lookup: %q %v", repo, err)
	}
	// an inherited GIT_DIR must not redirect the lookup elsewhere
	t.Setenv("GIT_DIR", "/nonexistent/git-dir")
	if repo, _, err = ForCheckout(context.Background(), dir); err != nil || repo != "o2/r2" {
		t.Errorf("GIT_DIR redirected the lookup: %q %v", repo, err)
	}
	os.Unsetenv("GIT_DIR")
	// no remote at all: defaults, never a gate
	empty := t.TempDir()
	g := exec.Command("git", "-C", empty, "init", "--bare")
	if out, err := g.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if repo, p, err := ForCheckout(context.Background(), empty); err != nil || repo != "" || !reflect.DeepEqual(p, publishpolicy.Policy{}) {
		t.Errorf("no remote: %q %+v %v", repo, p, err)
	}
}

// Policy is the defaults for an empty repository, else the configured file's entry.
func TestPolicyDefaultsAndConfigured(t *testing.T) {
	if p, err := Policy("", func(string) string { return "" }); err != nil || !reflect.DeepEqual(p, publishpolicy.Policy{}) {
		t.Fatalf("empty repo: %+v, %v", p, err)
	}
	home := t.TempDir()
	cfg := filepath.Join(home, "cfg.json")
	if err := os.WriteFile(cfg, []byte(`{"publication":{"repositories":{"o/r":{"hostnames":["build-box"]}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string {
		if k == "CRW_CONFIG" {
			return cfg
		}
		return ""
	}
	p, err := Policy("o/r", getenv)
	if err != nil || len(p.Hostnames) != 1 {
		t.Errorf("configured policy: %+v, %v", p, err)
	}
	if _, err := Policy("bad repo", getenv); err == nil {
		t.Error("an invalid repository was accepted")
	}
}
