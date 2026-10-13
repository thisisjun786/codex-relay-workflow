package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifierHost is a host for the verifier_prerequisites fact: a home of its own, a PATH that holds nothing, a configuration file
// and a git repository the run's worktrees come from.
type verifierHost struct {
	home, config, repo string
	env                []string
}

func newVerifierHost(t *testing.T, goMod string) *verifierHost {
	t.Helper()
	h := &verifierHost{home: t.TempDir()}
	empty := filepath.Join(h.home, "empty-path")
	h.config = filepath.Join(h.home, "config.json")
	h.repo = filepath.Join(h.home, "repo")
	for _, d := range []string{empty, filepath.Join(h.repo, "cmd", "crw-dev")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.repo, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", h.repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	h.writeConfig(t, `{}`)
	h.env = []string{"HOME=" + h.home, "PATH=" + empty, "CRW_CONFIG=" + h.config}
	return h
}

func (h *verifierHost) writeConfig(t *testing.T, doc string) {
	t.Helper()
	if err := os.WriteFile(h.config, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

// goRoot makes a Go toolchain root whose bin/go reports version.
func (h *verifierHost) goRoot(t *testing.T, name, version string) string {
	t.Helper()
	root := filepath.Join(h.home, name)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte("#!/bin/sh\necho 'go version "+version+" linux/amd64'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// CRW-1319 (evaluation a48fe518 d1): the preflight shows the verifier's prerequisites as the fact verifier_prerequisites. With
// neither crw-dev nor go on PATH and nothing configured it is a warning with its reason, and the run starts all the same: the
// preview lists it with the reason and exits 0, a start spawns the run.
func TestStartShowsTheVerifierPrerequisitesAsAWarnFact(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	h := newVerifierHost(t, "module example.com/p\n\ngo 1.27\n")
	td.Verifier = func(ctx context.Context, repo string) Fact { return VerifierPrerequisites(ctx, h.env, repo) }
	lane := []string{"--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t), "--repo", h.repo}

	code, out, errOut := td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--preview"}, lane...)...)
	if code != 0 || len(td.spawned) != 0 || !strings.Contains(out, "warn  "+FactVerifier) || !strings.Contains(out, "crw-dev") || !strings.Contains(out, "go") {
		t.Fatalf("a preview with the verifier's tools missing: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	code, out, errOut = td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--json"}, lane...)...)
	var ans StartAnswer
	if code != 0 || len(td.spawned) != 1 || json.Unmarshal([]byte(strings.TrimPrefix(out, "<<spawned>>\n")), &ans) != nil {
		t.Fatalf("the warning stopped the start: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	last := ans.Plan.Facts[len(ans.Plan.Facts)-1]
	if len(ans.Plan.Facts) != 6 || last.Name != FactVerifier || last.Level != LevelWarn || last.Pass || last.Reason == "" ||
		!strings.Contains(last.Reason, "verifier.go_root") {
		t.Errorf("the verifier fact: %+v", ans.Plan.Facts)
	}
	if len(td.spawned[0].Spec.Forced) != 0 {
		t.Errorf("a warning is recorded as a forced fact: %v", td.spawned[0].Spec.Forced)
	}
}

// CRW-1319: the fact passes when the configured Go runs and the repository carries cmd/crw-dev; it is a warning when the go is
// older than the repository's go.mod asks (GOTOOLCHAIN=local builds nothing then) and when a tree verification has no
// cmd/crw-dev to run.
func TestVerifierPrerequisitesFact(t *testing.T) {
	h := newVerifierHost(t, "module example.com/p\n\ngo 1.27\n\ntoolchain go1.27.1\n")
	good := h.goRoot(t, "go-good", "go1.27.1")
	h.writeConfig(t, `{"verifier":{"go_root":"`+good+`"}}`)
	f := VerifierPrerequisites(context.Background(), h.env, h.repo)
	if f.Name != FactVerifier || f.Level != LevelPass || !f.Pass || f.Reason != "" || !strings.Contains(f.Reading, "go1.27.1") || !strings.Contains(f.Reading, good) {
		t.Errorf("a configured Go root: %+v", f)
	}

	old := h.goRoot(t, "go-old", "go1.25.5")
	h.writeConfig(t, `{"verifier":{"go_root":"`+old+`"}}`)
	f = VerifierPrerequisites(context.Background(), h.env, h.repo)
	if f.Level != LevelWarn || f.Pass || !strings.Contains(f.Reason, "go1.25.5") || !strings.Contains(f.Reason, "go.mod") {
		t.Errorf("a go older than go.mod asks: %+v", f)
	}

	h.writeConfig(t, `{"verifier":{"go_root":"`+good+`"}}`)
	if err := os.RemoveAll(filepath.Join(h.repo, "cmd")); err != nil {
		t.Fatal(err)
	}
	f = VerifierPrerequisites(context.Background(), h.env, h.repo)
	if f.Level != LevelWarn || !strings.Contains(f.Reason, "cmd/crw-dev") || !strings.Contains(f.Reason, "verifier.crw_dev") {
		t.Errorf("a repository without cmd/crw-dev: %+v", f)
	}

	// a section the verifier cannot use is a warning too, never a refusal of the start
	h.writeConfig(t, `{"verifier":{"go_root":"go"}}`)
	f = VerifierPrerequisites(context.Background(), h.env, h.repo)
	if f.Level != LevelWarn || !strings.Contains(f.Reason, "verifier.go_root") {
		t.Errorf("a malformed section: %+v", f)
	}
}
