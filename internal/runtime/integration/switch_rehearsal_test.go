//go:build integration

package integration_test

// The 0.2.40 reinstall rehearsal and the return after a failure, proven with `crw install switch cxc`
// in an isolated home (CRW-201). The test runs the real crw binary against a Codex home under
// t.TempDir(): HOME, CODEX_HOME and the XDG roots all lie in it, and nothing here names the real
// ~/.codex. When a Codex binary is on PATH (CRW_TEST_CODEX names one explicitly) the CXC install and
// its reinstall are done by `codex plugin marketplace add / add / remove` against a fixture marketplace
// that stands in for codexclaw 0.2.40, so the measured behaviour of the real CLI is part of the proof;
// without one the same state is written by hand and the reinstall is stood in for by the key turning
// back on, which is what the CLI does (docs/port-cxc/known-defects/CRW-201.md).

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type switchRehearsal struct {
	root, home, codexHome, crw, codex string
	env                               []string
}

func newSwitchRehearsal(t *testing.T) *switchRehearsal {
	t.Helper()
	root := t.TempDir()
	r := &switchRehearsal{root: root, home: filepath.Join(root, "home"), codexHome: filepath.Join(root, "codex")}
	for _, dir := range []string{r.home, r.codexHome, filepath.Join(root, "bin"), filepath.Join(root, "xdg"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.crw = filepath.Join(root, "bin", "crw")
	if named := os.Getenv("CRW_TEST_BINARY"); named != "" {
		abs, err := filepath.Abs(named)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, r.crw, string(readFile(t, abs)), 0o755)
	} else {
		goBuild(t, r.crw, "0.0.0-switch-rehearsal")
	}
	r.codex = os.Getenv("CRW_TEST_CODEX")
	if r.codex == "" {
		r.codex, _ = exec.LookPath("codex")
	}
	r.env = []string{"HOME=" + r.home, "CODEX_HOME=" + r.codexHome, "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg", "config"), "XDG_DATA_HOME=" + filepath.Join(root, "xdg", "data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "xdg", "state"), "XDG_CACHE_HOME=" + filepath.Join(root, "xdg", "cache"), "TMPDIR=" + filepath.Join(root, "tmp"), "PATH=" + os.Getenv("PATH")}
	return r
}

func (r *switchRehearsal) run(t *testing.T, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env, cmd.Dir = r.env, r.root
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return stdout.String(), stderr.String(), code
}

func (r *switchRehearsal) crwOK(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := r.run(t, r.crw, append([]string{"install", "switch"}, args...)...)
	if code != 0 {
		t.Fatalf("crw install switch %v: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

type rehearsalStatus struct {
	State   string `json:"state"`
	Plugins map[string]struct {
		Enabled bool `json:"enabled"`
	} `json:"plugins"`
	Roles []struct{ Role, Owner string } `json:"roles"`
	Notes []string                       `json:"notes"`
}

func (r *switchRehearsal) status(t *testing.T) rehearsalStatus {
	t.Helper()
	var s rehearsalStatus
	if err := json.Unmarshal([]byte(r.crwOK(t, "status", "--json")), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *switchRehearsal) cxcRole(name string) string {
	body := "# codexclaw subagent role: " + name + "\n"
	return fmt.Sprintf("# codexclaw-managed: %x\n%s", sha256.Sum256([]byte(body)), body)
}

// installCXC leaves the Codex home as a CXC 0.2.40 host: the plugin enabled, written in the tight
// spelling `enabled=true`, crw@crw enabled, and CXC-owned executor and architect role files.
func (r *switchRehearsal) installCXC(t *testing.T) {
	t.Helper()
	fixture := filepath.Join(r.root, "fixture")
	writeFile(t, filepath.Join(fixture, ".agents", "plugins", "marketplace.json"), `{"name":"codexclaw","interface":{"displayName":"codexclaw 0.2.40 fixture"},"plugins":[{"name":"codexclaw","source":{"source":"local","path":"./plugins/codexclaw"},"policy":{"installation":"AVAILABLE","authentication":"ON_USE"},"category":"Developer Tools"}]}`, 0o644)
	writeFile(t, filepath.Join(fixture, "plugins", "codexclaw", ".codex-plugin", "plugin.json"), `{"name":"codexclaw","version":"0.2.40","description":"fixture standing in for CXC 0.2.40","interface":{"displayName":"codexclaw fixture"}}`, 0o644)
	config := "[plugins.\"codexclaw@codexclaw\"]\nenabled = true\n"
	if r.codex != "" {
		for _, args := range [][]string{{"plugin", "marketplace", "add", fixture}, {"plugin", "add", "codexclaw@codexclaw"}} {
			if out, errOut, code := r.run(t, r.codex, args...); code != 0 {
				t.Skipf("the Codex binary %s cannot install the fixture marketplace here (exit %d): %s%s", r.codex, code, out, errOut)
			}
		}
		config = string(readFile(t, filepath.Join(r.codexHome, "config.toml")))
	}
	if !strings.Contains(config, "enabled = true") {
		t.Fatalf("the CXC install left no enabled key:\n%s", config)
	}
	config = strings.Replace(config, "enabled = true", "enabled=true", 1) + "\n[plugins.\"crw@crw\"]\nenabled = true\n"
	writeFile(t, filepath.Join(r.codexHome, "config.toml"), config, 0o644)
	for _, name := range []string{"executor", "architect"} {
		writeFile(t, filepath.Join(r.codexHome, "agents", name+".toml"), r.cxcRole(name), 0o644)
	}
}

// reinstall is `codex plugin remove` then `codex plugin add` of the same version, or, without a Codex
// binary, the key turning back on that the measurement found.
func (r *switchRehearsal) reinstall(t *testing.T) {
	t.Helper()
	if r.codex == "" {
		path := filepath.Join(r.codexHome, "config.toml")
		writeFile(t, path, strings.Replace(string(readFile(t, path)), "enabled = false", "enabled = true", 1), 0o644)
		return
	}
	for _, args := range [][]string{{"plugin", "remove", "codexclaw@codexclaw"}, {"plugin", "add", "codexclaw@codexclaw"}} {
		if out, errOut, code := r.run(t, r.codex, args...); code != 0 {
			t.Fatalf("codex %v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
}

func (r *switchRehearsal) hostBytes(t *testing.T) map[string]string {
	t.Helper()
	tree := map[string]string{}
	for _, rel := range []string{"config.toml", "agents/executor.toml", "agents/architect.toml"} {
		tree[rel] = string(readFile(t, filepath.Join(r.codexHome, rel)))
	}
	return tree
}

var rehearsalKeyLine = regexp.MustCompile(`(?m)^enabled=true$`)

func TestSwitchRehearsalReinstallThenReturn(t *testing.T) {
	r := newSwitchRehearsal(t)
	r.installCXC(t)
	before := r.hostBytes(t)
	if s := r.status(t); s.State != "cxc" {
		t.Fatalf("the CXC host reads %q, want cxc: %+v", s.State, s)
	}

	r.crwOK(t, "crw")
	if s := r.status(t); s.State != "crw" || s.Plugins["codexclaw"].Enabled || !s.Plugins["crw"].Enabled {
		t.Fatalf("after switch crw: %+v", s)
	}

	// The 0.2.40 reinstall. Whether the real CLI turns the key back on is measured, not assumed; either
	// answer must leave status truthful and the switch able to put it right.
	r.reinstall(t)
	s := r.status(t)
	t.Logf("after the reinstall the CLI left state %q (codex binary: %q)", s.State, r.codex)
	switch s.State {
	case "conflict":
		if !s.Plugins["codexclaw"].Enabled || len(s.Notes) == 0 {
			t.Fatalf("conflict without the plugin on or a note: %+v", s)
		}
		r.crwOK(t, "crw")
		if s = r.status(t); s.State != "crw" {
			t.Fatalf("switch crw did not put the conflict right: %+v", s)
		}
	case "crw":
	default:
		t.Fatalf("after the reinstall: %+v", s)
	}

	// The rehearsal failed: return to CXC.
	r.crwOK(t, "cxc")
	s = r.status(t)
	if s.State != "cxc" || !s.Plugins["codexclaw"].Enabled {
		t.Fatalf("after switch cxc: %+v", s)
	}
	after := r.hostBytes(t)
	for _, name := range []string{"agents/executor.toml", "agents/architect.toml"} {
		if after[name] != before[name] {
			t.Fatalf("%s differs after the return:\n%q\nwant\n%q", name, after[name], before[name])
		}
	}
	if !rehearsalKeyLine.MatchString(after["config.toml"]) {
		t.Fatalf("the key line was not given back as it was (`enabled=true`):\n%s", after["config.toml"])
	}
	if r.codex == "" && after["config.toml"] != before["config.toml"] {
		t.Fatalf("config.toml differs after cxc -> crw -> cxc:\n%q\nwant\n%q", after["config.toml"], before["config.toml"])
	}
}

func TestSwitchRehearsalAFailedReturnLeavesTheHostAsItWas(t *testing.T) {
	r := newSwitchRehearsal(t)
	r.installCXC(t)
	original := r.hostBytes(t)
	r.crwOK(t, "crw")
	switched := r.hostBytes(t)

	// The backup of the CXC executor role file is gone, so the way back cannot finish.
	var manifest struct {
		Switch struct {
			Roles []struct {
				Role       string  `json:"role"`
				BackupPath *string `json:"backupPath"`
			} `json:"roles"`
		} `json:"switch"`
	}
	if err := json.Unmarshal(readFile(t, filepath.Join(r.codexHome, ".crw-install.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	var backup string
	for _, role := range manifest.Switch.Roles {
		if role.Role == "executor" && role.BackupPath != nil {
			backup = *role.BackupPath
		}
	}
	if backup == "" {
		t.Fatalf("the manifest records no executor backup: %+v", manifest)
	}
	saved := readFile(t, backup)
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := r.run(t, r.crw, "install", "switch", "cxc"); code == 0 {
		t.Fatalf("switch cxc succeeded without the backup:\n%s%s", out, errOut)
	}
	if got := r.hostBytes(t); fmt.Sprint(got) != fmt.Sprint(switched) {
		t.Fatalf("the failed return changed the host:\n%v\nwant\n%v", got, switched)
	}
	if s := r.status(t); s.State != "crw" {
		t.Fatalf("after the failed return: %+v", s)
	}

	// With the backup back, the same command finishes and the host is the CXC host again.
	writeFile(t, backup, string(saved), 0o644)
	r.crwOK(t, "cxc")
	if got := r.hostBytes(t); fmt.Sprint(got) != fmt.Sprint(original) {
		t.Fatalf("the host differs from the CXC original after the return:\n%v\nwant\n%v", got, original)
	}
	if s := r.status(t); s.State != "cxc" {
		t.Fatalf("after the return: %+v", s)
	}
}
