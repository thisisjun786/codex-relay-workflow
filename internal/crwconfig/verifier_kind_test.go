package crwconfig

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1594: the verifier kind — local is the default, github-actions selects the hosted workflow. The remote
// settings are the repository (OWNER/REPO), the workflow (default "CRW CI") and the branch prefix (default
// "ci/"). `crw config verifier` records all four flags and reports the normalized kind and remote values, and
// a partial update keeps every other key, of the section and of the file.
func TestVerifierKindSelectsAndRoundtrips(t *testing.T) {
	_, env := rootsHome(t)
	// the default is local and names no remote
	if tools, err := LoadVerifierTools(env, configWrite(t, `{}`)); err != nil || tools.VerifierKind() != VerifierLocal {
		t.Errorf("the default kind: %+v %v", tools, err)
	}
	path := configWrite(t, `{"verifier":{"kind":"github-actions","repository":"owner/repo","workflow":"Nightly","branch_prefix":"runs/"},"other":{"x":1}}`)
	tools, err := LoadVerifierTools(env, path)
	if err != nil || tools.VerifierKind() != VerifierGitHubActions || tools.Repository != "owner/repo" ||
		tools.Workflow != "Nightly" || tools.BranchPrefix != "runs/" {
		t.Fatalf("the section: %+v %v", tools, err)
	}
	// the defaults of the remote fields
	tools, err = LoadVerifierTools(env, configWrite(t, `{"verifier":{"kind":"github-actions","repository":"owner/repo"}}`))
	if err != nil || tools.GitHubWorkflow() != "CRW CI" || tools.GitHubBranchPrefix() != "ci/" {
		t.Errorf("the remote defaults: %+v %v", tools, err)
	}
	// the flags record and the report names the normalized kind and remote values
	goRoot := fakeGoRoot(t)
	path = configWrite(t, `{"verifier":{"go_root":"`+goRoot+`"},"other":{"x":1}}`)
	code, out, errOut := configRun(t, env, "verifier", "--config", path,
		"--kind", "github-actions", "--repository", "owner/repo", "--workflow", "Nightly", "--branch-prefix", "runs/")
	if code != 0 {
		t.Fatalf("record: exit %d\n%s%s", code, out, errOut)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil || report["kind"] != "github-actions" ||
		report["repository"] != "owner/repo" || report["workflow"] != "Nightly" || report["branch_prefix"] != "runs/" {
		t.Errorf("the report: %s", out)
	}
	// a partial update keeps the keys it does not name, of the section and of the file
	code, _, _ = configRun(t, env, "verifier", "--config", path, "--workflow", "CI Two")
	if code != 0 {
		t.Fatalf("the partial update: exit %d", code)
	}
	data, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	verifier, _ := doc["verifier"].(map[string]any)
	if verifier["kind"] != "github-actions" || verifier["repository"] != "owner/repo" || verifier["workflow"] != "CI Two" ||
		verifier["branch_prefix"] != "runs/" || verifier["go_root"] != goRoot || doc["other"] == nil {
		t.Errorf("the file after the partial update: %s", data)
	}
}

// CRW-1594: a malformed remote section and an unknown kind are refused — never silently local. A repository
// that is not OWNER/REPO of GitHub identifiers, a missing repository, and a branch prefix that is not a safe
// ref prefix are all refused. The legacy local fail-open for tool paths is unchanged.
func TestVerifierKindRefusesAMalformedRemote(t *testing.T) {
	_, env := rootsHome(t)
	for document, want := range map[string]string{
		`{"verifier":{"kind":"github-actions"}}`:                                                  "repository",
		`{"verifier":{"kind":"github-actions","repository":"repo"}}`:                              "OWNER/REPO",
		`{"verifier":{"kind":"github-actions","repository":"o/r/x"}}`:                             "OWNER/REPO",
		`{"verifier":{"kind":"github-actions","repository":"https://github.com/o/r"}}`:            "OWNER/REPO",
		`{"verifier":{"kind":"github-actions","repository":"o/../r"}}`:                            "OWNER/REPO",
		`{"verifier":{"kind":"github-actions","repository":" o/r"}}`:                              "OWNER/REPO",
		`{"verifier":{"kind":"github-actions","repository":"o/r","branch_prefix":"ci"}}`:          "/",
		`{"verifier":{"kind":"github-actions","repository":"o/r","branch_prefix":"c i/"}}`:        "branch_prefix",
		`{"verifier":{"kind":"github-actions","repository":"o/r","branch_prefix":"refs/heads/"}}`: "branch_prefix",
		`{"verifier":{"kind":"bogus"}}`:                                                           "kind",
		`{"verifier":{"kind":3}}`:                                                                 "kind",
		`{"verifier":{"kind":true}}`:                                                              "kind",
	} {
		tools, err := LoadVerifierTools(env, configWrite(t, document))
		if err == nil {
			t.Errorf("%s: silently became %+v", document, tools)
			continue
		}
		if tools.VerifierKind() == VerifierGitHubActions && err == nil {
			t.Errorf("%s: accepted", document)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q in the reason", document, err, want)
		}
		if tools.Ignored != "" {
			t.Errorf("%s: was ignored instead of refused", document)
		}
	}
	// a remote section is refused at the file's section reader too
	file, err := Load(env, configWrite(t, `{"verifier":{"kind":"github-actions"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.VerifierTools(); err == nil {
		t.Error("the section reader accepted a remote without a repository")
	}
	// the legacy local fail-open stands: a bad go_root is ignored, and the verifier takes tools from PATH
	tools, err := LoadVerifierTools(env, configWrite(t, `{"verifier":{"go_root":"relative/go"}}`))
	if err != nil || tools.Ignored == "" {
		t.Errorf("the legacy fail-open: %+v %v", tools, err)
	}
	// and so does a local section with bad tool paths: kind local keeps the fail-open
	tools, err = LoadVerifierTools(env, configWrite(t, `{"verifier":{"kind":"local","go_root":"relative/go"}}`))
	if err != nil || tools.Ignored == "" {
		t.Errorf("a local section's fail-open: %+v %v", tools, err)
	}
}

// CRW-1594: GitHub ref rules on the branch prefix: no "..", "@{", backslash, whitespace or control, none of
// ~^:?*[, no double slash, no leading or trailing dot component and no .lock component; the prefix must end in "/".

// fakeGitHubTools is a PATH of fake git, gitleaks and gh executables for the remote probe: gh answers auth
// status, repo view and workflow view from the answers map ("auth" the exit code, "permission" the
// viewerPermission, "state" the workflow state), using only shell builtins, and records every call and the
// HOME and GH_CONFIG_DIR it saw.
func fakeGitHubTools(t *testing.T, dir string, answers map[string]string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("git", "#!/bin/sh\necho 'git version 2.43.0'\n")
	write("gitleaks", "#!/bin/sh\necho 'gitleaks version 8.18.0'\n")
	auth := answers["auth"]
	if auth == "" {
		auth = "0"
	}
	permission := answers["permission"]
	if permission == "" {
		permission = "WRITE"
	}
	state := answers["state"]
	if state == "" {
		state = "active"
	}
	gh := `#!/bin/sh
echo "gh $* HOME=$HOME GH_CONFIG_DIR=$GH_CONFIG_DIR" >> "` + filepath.Join(dir, "gh.log") + `"
case "$1 $2" in
  "auth status") exit ` + auth + ` ;;
  "repo view") echo '{"viewerPermission":"` + permission + `"}' ;;
  "api --paginate") echo '{"workflows":[{"id":7,"name":"CRW CI","path":".github/workflows/ci.yml","state":"` + state + `"}]}' ;;
esac
exit 0
`
	write("gh", gh)
	return bin
}

// CRW-1594: the remote probe needs no Go and no crw-dev: git and gitleaks on PATH, gh logged in to github.com,
// write permission on the repository and an active configured workflow. Its HOME and GH_CONFIG_DIR are the
// caller's, so gh reads its own login. Every bad or missing piece is a problem and the probe is not runnable.
func TestRemoteVerifierProbeNeedsNoGo(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	bin := fakeGitHubTools(t, dir, map[string]string{"auth": "0"})
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{"verifier":{"kind":"github-actions","repository":"owner/repo","workflow":"CRW CI"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "PATH=" + bin, "CRW_CONFIG=" + config, "GH_CONFIG_DIR=" + filepath.Join(home, "ghcfg")}
	p := ProbeVerifierTools(context.Background(), env, "")
	if p.Mode != "github-actions" || !p.Runnable || len(p.Problems) != 0 {
		t.Fatalf("the remote probe: %+v", p)
	}
	if p.Go != "" || p.GoVersion != "" || len(p.Candidates) != 0 {
		t.Errorf("the remote probe probed Go: %+v", p)
	}
	if got := strings.Join(p.Command, " "); !strings.Contains(got, "gh workflow run") || !strings.Contains(got, "CRW CI") || !strings.Contains(got, "owner/repo") {
		t.Errorf("the probe's command: %v", p.Command)
	}
	// the gh probes saw the caller's HOME and GH_CONFIG_DIR, so its own login applies
	log, err := os.ReadFile(filepath.Join(dir, "gh.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "HOME="+home) || !strings.Contains(string(log), "GH_CONFIG_DIR="+filepath.Join(home, "ghcfg")) {
		t.Errorf("the probes' environment: %s", log)
	}
	for _, want := range []string{"auth status", "repo view", "api --paginate repos/owner/repo/actions/workflows"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("no %s probe in %s", want, log)
		}
	}
}

func TestRemoteVerifierProbeReportsWhatIsMissing(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config.json")
	if err := os.WriteFile(config, []byte(`{"verifier":{"kind":"github-actions","repository":"owner/repo"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func(bin string) []string {
		return []string{"HOME=" + home, "PATH=" + bin, "CRW_CONFIG=" + config}
	}
	t.Run("not logged in", func(t *testing.T) {
		dir := t.TempDir()
		bin := fakeGitHubTools(t, dir, map[string]string{"auth": "1"})
		p := ProbeVerifierTools(context.Background(), base(bin), "")
		if p.Runnable || !hasProblem(p, "github.com") {
			t.Errorf("not logged in: %+v", p)
		}
	})
	t.Run("no write permission", func(t *testing.T) {
		dir := t.TempDir()
		bin := fakeGitHubTools(t, dir, map[string]string{"auth": "0", "permission": "READ"})
		p := ProbeVerifierTools(context.Background(), base(bin), "")
		if p.Runnable || !hasProblem(p, "permission") {
			t.Errorf("read-only: %+v", p)
		}
	})
	t.Run("a disabled workflow", func(t *testing.T) {
		dir := t.TempDir()
		bin := fakeGitHubTools(t, dir, map[string]string{"auth": "0", "state": "disabled_manually"})
		p := ProbeVerifierTools(context.Background(), base(bin), "")
		if p.Runnable || !hasProblem(p, "workflow") {
			t.Errorf("disabled: %+v", p)
		}
	})
	t.Run("no gh", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		for _, name := range []string{"git", "gitleaks"} {
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		p := ProbeVerifierTools(context.Background(), base(bin), "")
		if p.Runnable || !hasProblem(p, "gh") {
			t.Errorf("no gh: %+v", p)
		}
	})
	t.Run("no gitleaks", func(t *testing.T) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"git": "#!/bin/sh\necho 'git version 2.43.0'\n",
			"gh":  "#!/bin/sh\nexit 0\n",
		} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		p := ProbeVerifierTools(context.Background(), base(bin), "")
		if p.Runnable || !hasProblem(p, "gitleaks") {
			t.Errorf("no gitleaks: %+v", p)
		}
	})
}

func hasProblem(p VerifierProbe, word string) bool {
	for _, problem := range p.Problems {
		if strings.Contains(problem, word) {
			return true
		}
	}
	return false
}

// CRW-1594 rework: the repair preserves a readable explicit kind when only its repository is missing, and a
// repair to github-actions works even when a preserved local go_root points at an absent Go — bin/go is
// stat'd only for the local kind. A kind the raw section cannot even decode is repaired only through --kind.
func TestVerifierRepairKeepsTheKind(t *testing.T) {
	_, env := rootsHome(t)
	// a github-actions section missing only its repository: --repository alone repairs it, kind kept
	path := configWrite(t, `{"verifier":{"kind":"github-actions"}}`)
	code, out, errOut := configRun(t, env, "verifier", "--config", path, "--repository", "owner/repo")
	if code != 0 {
		t.Fatalf("the repository repair: exit %d\n%s%s", code, out, errOut)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(out), &report); err != nil || report["kind"] != "github-actions" {
		t.Fatalf("the repaired report: %s %v", out, err)
	}
	// a stale local go_root does not block a github-actions repair
	path = configWrite(t, `{"verifier":{"go_root":"/nonexistent/old/go"}}`)
	code, out, _ = configRun(t, env, "verifier", "--config", path, "--kind", "github-actions", "--repository", "owner/repo")
	if code != 0 {
		t.Fatalf("the repair over a stale go_root: exit %d\n%s", code, out)
	}
	// a kind that is no string is refused without --kind, repaired with it
	path = configWrite(t, `{"verifier":{"kind":3,"repository":"owner/repo"}}`)
	code, _, errOut = configRun(t, env, "verifier", "--config", path, "--repository", "owner/repo2")
	if code == 0 || !strings.Contains(errOut, "--kind") {
		t.Errorf("a non-string kind without --kind: exit %d %q", code, errOut)
	}
	code, out, _ = configRun(t, env, "verifier", "--config", path, "--kind", "github-actions")
	if code != 0 {
		t.Errorf("a non-string kind with --kind: exit %d %s", code, out)
	}
}

// CRW-1594 rework: DecodeGitHubWorkflow selects from the paginated workflows listing exactly one workflow,
// by name, numeric ID, full path or filename; absent, ambiguous, inactive and badly-pathed selections are errors.
func TestDecodeGitHubWorkflowSelection(t *testing.T) {
	page := func(workflows ...map[string]any) []byte {
		var out []byte
		for _, w := range workflows {
			raw, _ := json.Marshal(map[string]any{"workflows": []map[string]any{w}})
			out = append(out, raw...)
			out = append(out, '\n')
		}
		return out
	}
	ci := map[string]any{"id": 7, "name": "CRW CI", "path": ".github/workflows/ci.yml", "state": "active"}
	other := map[string]any{"id": 9, "name": "Nightly", "path": ".github/workflows/nightly.yml", "state": "active"}
	// selection across two pages, by name, by numeric ID, by path and by filename
	raw := append(page(ci), page(other)...)
	for selector, want := range map[string]int64{"CRW CI": 7, "Nightly": 9, "9": 9, ".github/workflows/ci.yml": 7, "ci.yml": 7} {
		info, err := DecodeGitHubWorkflow(raw, selector)
		if err != nil || info.ID != want {
			t.Errorf("selector %q: %+v %v", selector, info, err)
		}
	}
	// two workflows of one name on different pages are ambiguous
	dup := map[string]any{"id": 11, "name": "CRW CI", "path": ".github/workflows/ci2.yml", "state": "active"}
	if _, err := DecodeGitHubWorkflow(append(page(ci), page(dup)...), "CRW CI"); err == nil {
		t.Error("two workflows of one name selected")
	}
	if _, err := DecodeGitHubWorkflow(raw, "Missing"); err == nil {
		t.Error("an absent workflow selected")
	}
	inactive := map[string]any{"id": 3, "name": "Off", "path": ".github/workflows/off.yml", "state": "disabled_manually"}
	if _, err := DecodeGitHubWorkflow(page(inactive), "Off"); err == nil {
		t.Error("an inactive workflow selected")
	}
	badPath := map[string]any{"id": 4, "name": "Evil", "path": ".github/workflows/../evil.yml", "state": "active"}
	if _, err := DecodeGitHubWorkflow(page(badPath), "Evil"); err == nil {
		t.Error("a traversal path selected")
	}
}
