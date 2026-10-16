package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The fake crw-dev of TestCILocalIntegrationVerifiesTheMergedTree: this test binary, started by a script with labFakeCrwDev set, answers
// `crw-dev ci local --commit H --base B --record R --runner local` the way the real one does for the lab: the sealed record of the commit
// H in its working directory (the merged tree), written to R, and one line of its arguments, directory and environment appended to the log.
const (
	labFakeCrwDev     = "CRW_LAB_FAKE_CRW_DEV"
	labFakeCrwDevLog  = "CRW_LAB_FAKE_CRW_DEV_LOG"
	labFakeCrwDevRepo = "CRW_LAB_FAKE_CRW_DEV_REPO"
)

// fakeCrwDevMain is the fake crw-dev's main: it answers 0 with the record written, or 1 with the reason.
func fakeCrwDevMain() int {
	args := os.Args[1:]
	dir, _ := os.Getwd()
	at, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	files, _ := filepath.Glob(filepath.Join(dir, "node-*.txt"))
	for i, f := range files {
		files[i] = filepath.Base(f)
	}
	// the physical path and mode of the TMPDIR it was given, which is removed after the verification
	tmpPhysical, tmpMode := "", ""
	if p, err := filepath.EvalSymlinks(os.Getenv("TMPDIR")); err == nil {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			tmpPhysical, tmpMode = p, fi.Mode().Perm().String()
		}
	}
	line, _ := json.Marshal(map[string]any{"args": args, "dir": dir, "head": strings.TrimSpace(string(at)), "files": files, "env": os.Environ(),
		"tmp_physical": tmpPhysical, "tmp_mode": tmpMode})
	if f, err := os.OpenFile(os.Getenv(labFakeCrwDevLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(line, '\n'))
		_ = f.Close()
	}
	flag := func(name string) string {
		if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if len(args) < 2 || args[0] != "ci" || args[1] != "local" || flag("--commit") == "" || flag("--record") == "" {
		fmt.Fprintf(os.Stderr, "fake crw-dev: not a ci local verification: %q\n", args)
		return 2
	}
	ctx := context.Background()
	head, base := flag("--commit"), flag("--base")
	keys, err := dagsched.CommitVerificationKeys(ctx, dir, head)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	tree, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", head+"^{tree}").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	raw, err := dagsched.SealVerificationRecord(dagsched.VerificationRecord{Runner: "local", Repository: os.Getenv(labFakeCrwDevRepo), BaseCommit: base, HeadCommit: head,
		TreeHash: strings.TrimSpace(string(tree)), CiDigest: keys.CiDigest, Tools: keys.Pins, Pins: keys.Pins, PinMismatch: []string{}, Dependencies: keys.Dependencies,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass", Jobs: []json.RawMessage{}})
	if err == nil {
		err = os.WriteFile(flag("--record"), raw, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake crw-dev:", err)
		return 1
	}
	return 0
}

// TestCILocalIntegrationVerifiesTheMergedTree (CRW-1285, the verification gap of CRW-1277): the shipped integration verifier
// CILocalIntegration, which the lab otherwise replaces, verifies each integration batch's merged tree: the configured crw-dev (here a
// fake one, a process of its own started through the shared dagsched.CommandVerifier) is run in the merged tree with `ci local --commit
// <merged head> --base <target tip> --record <the batch's record> --runner local`, its record lands the nodes, and the note of a crw-dev
// whose steps take go from PATH is written. A crw-dev that cannot start is a host error that names the setting.
func TestCILocalIntegrationVerifiesTheMergedTree(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1", After: []string{"A-1"}})
	log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
	crwDev := labCrwDevScript(t, log, l.repo)
	var notes bytes.Buffer
	tools := crwconfig.VerifierTools{CrwDev: crwDev, Config: "/lab/config.json"}
	OpsFor = func(ctx context.Context, req LaneRequest) (LaneWorld, error) {
		return BuildLane(ctx, req, LaneOptions{Verifier: labVerifier{l}, IntegrationVerify: CILocalIntegration(tools, &notes), SkillsDigest: "sha256:lab-skills", Reporter: &labReporter{l: l},
			UnitCheck: l.uc.seam(), Crosscheck: l.cc.seam(), Relay: func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }})
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	v := l.closed(l.runToEnd(spec.RunID))
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the integration verifier never ran crw-dev: %v", err)
	}
	var calls []fakeCrwDevCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var c fakeCrwDevCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	if len(calls) != 2 {
		t.Fatalf("%d verifications of merged trees, want one per node: %s", len(calls), raw)
	}
	for i, c := range calls {
		a := c.Args
		if len(a) != 10 || a[0] != "ci" || a[1] != "local" || a[2] != "--commit" || a[4] != "--base" || a[6] != "--record" || a[8] != "--runner" || a[9] != "local" {
			t.Errorf("call %d: crw-dev %q", i, a)
			continue
		}
		head := a[3]
		if out, err := exec.Command("git", "-C", l.repo, "merge-base", "--is-ancestor", head, "integration").CombinedOutput(); err != nil {
			t.Errorf("call %d verified %s, which did not land: %s", i, head, out)
		}
		// the directory is the merged tree: the commit it verified is checked out there, with the work of the nodes merged so far
		if c.Head != head {
			t.Errorf("call %d ran in %s at %s, not at the merged head %s", i, c.Dir, c.Head, head)
		}
		for _, key := range []string{"A-1", "A-2"}[:i+1] {
			if !slices.Contains(c.Files, "node-"+key+".txt") {
				t.Errorf("call %d: the merged tree %s has no work of %s: %v", i, c.Dir, key, c.Files)
			}
		}
	}
	if calls[1].Args[3] != v.Tip {
		t.Errorf("the last verified head %s is not the tip %s the milestone closed over", calls[1].Args[3], v.Tip)
	}
	if !strings.Contains(notes.String(), "[crw note] integration verification: verifier.go_root is not set in /lab/config.json") {
		t.Errorf("the note of a crw-dev whose steps take go from PATH: %q", notes.String())
	}

	// a crw-dev that cannot start is a host error naming the setting, not a failed verification
	err = CILocalIntegration(crwconfig.VerifierTools{CrwDev: filepath.Join(t.TempDir(), "no-crw-dev"), Config: "/lab/config.json"}, nil)(ctx, l.repo,
		[]string{"CRW_VERIFY_HEAD=h", "CRW_VERIFY_BASE=b", "CRW_VERIFY_RECORD=" + filepath.Join(t.TempDir(), "r.json")})
	var host *dagsched.IntegrationVerifierHostError
	if !errors.As(err, &host) || !strings.Contains(host.Detail, "could not start") || !strings.Contains(host.Detail, "verifier.crw_dev in /lab/config.json") {
		t.Errorf("a crw-dev that cannot start: %v", err)
	}
	// a tree with no cmd/crw-dev to run under a configured Go root is a host error too
	err = CILocalIntegration(crwconfig.VerifierTools{GoRoot: "/opt/go", Config: "/lab/config.json"}, nil)(ctx, t.TempDir(), nil)
	if !errors.As(err, &host) || !strings.Contains(host.Detail, "has no cmd/crw-dev") {
		t.Errorf("a tree with no crw-dev: %v", err)
	}
}

// fakeCrwDevCall is one line of the fake crw-dev's log.
type fakeCrwDevCall struct {
	Args  []string `json:"args"`
	Dir   string   `json:"dir"`
	Head  string   `json:"head"`
	Files []string `json:"files"`
	Env   []string `json:"env"`
	// TmpPhysical and TmpMode are the physical path and permission bits of the TMPDIR crw-dev was given
	TmpPhysical string `json:"tmp_physical"`
	TmpMode     string `json:"tmp_mode"`
}

// labCrwDevScript writes the script that starts this test binary as the fake crw-dev with its log at log, and returns its path.
func labCrwDevScript(t *testing.T, log, repo string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	crwDev := filepath.Join(t.TempDir(), "crw-dev")
	script := fmt.Sprintf("#!/bin/sh\n%s=1 %s=%q %s=%q exec %q \"$@\"\n", labFakeCrwDev, labFakeCrwDevLog, log, labFakeCrwDevRepo, repo, self)
	if err := os.WriteFile(crwDev, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return crwDev
}

// labCommitRepo is a repository with one commit, and that commit.
func labCommitRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=lab", "-c", "user.email=lab@example.invalid", "commit", "-q", "--allow-empty", "-m", "lab"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(head))
}

// envValue is the value of key in env (the last entry wins, as exec takes it), and whether env has it.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

// TestIntegrationVerifyDoesNotPassTheCallerEnvironment (CRW-1463, CRW-1489 C-2): the crw-dev an integration batch runs gets an environment
// built for it, not the caller's: HOME, TMPDIR, XDG_CONFIG_HOME, GOFLAGS, GOPATH and any other variable of the caller stay out; PATH,
// LANG, GOCACHE and the caller's module-source settings (GOPROXY) are kept; GOMODCACHE is the caller's effective module cache (here
// <GOPATH>/pkg/mod), so a `go run` of the tree finds the modules it already has; HOME and TMPDIR are a fresh directory of this
// verification, removed after it; the tools' env and the batch's CRW_VERIFY_* come last.
func TestIntegrationVerifyDoesNotPassTheCallerEnvironment(t *testing.T) {
	caller := t.TempDir()
	gopath := filepath.Join(caller, "gopath")
	set := map[string]string{
		"HOME": filepath.Join(caller, "home"), "TMPDIR": filepath.Join(caller, "tmp"), "XDG_CONFIG_HOME": filepath.Join(caller, "config"),
		"GOFLAGS": "-mod=mod", "GOPATH": gopath + string(os.PathListSeparator) + filepath.Join(caller, "second"), "CRW_LAB_CALLER_MARKER": "leaked",
		"LANG": "C.UTF-8", "GOCACHE": filepath.Join(caller, "gocache"), "GOPROXY": "https://lab-proxy.invalid", "GOMODCACHE": "",
		"XDG_STATE_HOME": filepath.Join(caller, "state"), "CRW_CI_HEAVY_GATE": "/lab/heavy_slot.sh", "CRW_CONFIG": "",
	}
	for k, v := range set {
		t.Setenv(k, v)
	}
	for _, key := range []string{"GOMODCACHE", "CRW_CONFIG"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	crwConfig := filepath.Join(set["XDG_CONFIG_HOME"], "crw", "config.json")
	if err := os.MkdirAll(filepath.Dir(crwConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crwConfig, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{set["HOME"], set["TMPDIR"]} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	repo, head := labCommitRepo(t)
	log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
	tools := crwconfig.VerifierTools{CrwDev: labCrwDevScript(t, log, repo), GoRoot: "/opt/lab-go", Config: "/lab/config.json"}
	record := filepath.Join(t.TempDir(), "record.json")
	batch := []string{"CRW_VERIFY_HEAD=" + head, "CRW_VERIFY_BASE=" + head, "CRW_VERIFY_RECORD=" + record}
	if err := CILocalIntegration(tools, nil)(context.Background(), repo, batch); err != nil {
		t.Fatalf("the verification: %v", err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the integration verifier never ran crw-dev: %v", err)
	}
	var call fakeCrwDevCall
	if err := json.Unmarshal(bytes.TrimSpace(raw), &call); err != nil {
		t.Fatal(err)
	}
	env := call.Env
	for _, key := range []string{"XDG_CONFIG_HOME", "GOFLAGS", "GOPATH", "CRW_LAB_CALLER_MARKER"} {
		if v, ok := envValue(env, key); ok {
			t.Errorf("the caller's %s reached crw-dev: %q", key, v)
		}
	}
	for _, key := range []string{"HOME", "TMPDIR"} {
		v, ok := envValue(env, key)
		if !ok || v == "" || v == set[key] {
			t.Errorf("crw-dev's %s is %q (set %v), not a fresh directory of the verification (the caller's is %s)", key, v, ok, set[key])
			continue
		}
		if _, err := os.Stat(v); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("crw-dev's %s %s is still there after the verification: %v", key, v, err)
		}
	}
	// the fresh TMPDIR lies directly below the caller's, resolved, and is named crw-<8 hex>
	// (TestIntegrationTempDir_isBelowTheRunsTempDirWhateverItsLength): no length moves it elsewhere
	callerTmp, err := filepath.EvalSymlinks(set["TMPDIR"])
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(call.TmpPhysical) != callerTmp || !integrationTmpName.MatchString(filepath.Base(call.TmpPhysical)) {
		t.Errorf("crw-dev's TMPDIR %q is not <%s>/crw-<8 hex>", call.TmpPhysical, callerTmp)
	}
	// ci local's own settings as the caller's ci local would find them: the work root below the caller's state directory, the
	// host's heavy-check gate, and the caller's crw configuration (named, since XDG_CONFIG_HOME stays out)
	if v, _ := envValue(env, "CRW_CONFIG"); v != crwConfig {
		t.Errorf("crw-dev's CRW_CONFIG is %q, want the caller's configuration %s", v, crwConfig)
	}
	for _, key := range []string{"LANG", "GOCACHE", "GOPROXY", "XDG_STATE_HOME", "CRW_CI_HEAVY_GATE"} {
		if v, _ := envValue(env, key); v != set[key] {
			t.Errorf("crw-dev's %s is %q, want the caller's %q", key, v, set[key])
		}
	}
	if v, _ := envValue(env, "GOMODCACHE"); v != filepath.Join(gopath, "pkg", "mod") {
		t.Errorf("crw-dev's GOMODCACHE is %q, want the caller's effective module cache %s", v, filepath.Join(gopath, "pkg", "mod"))
	}
	// the tools' env: the configured Go root first on the caller's PATH, its GOROOT and a local toolchain
	if v, _ := envValue(env, "PATH"); v != "/opt/lab-go/bin"+string(os.PathListSeparator)+os.Getenv("PATH") {
		t.Errorf("crw-dev's PATH is %q, want the Go root's bin before the caller's PATH", v)
	}
	for key, want := range map[string]string{"GOROOT": "/opt/lab-go", "GOTOOLCHAIN": "local", "CRW_VERIFY_HEAD": head, "CRW_VERIFY_BASE": head, "CRW_VERIFY_RECORD": record} {
		if v, _ := envValue(env, key); v != want {
			t.Errorf("crw-dev's %s is %q, want %q", key, v, want)
		}
	}
}

// TestIntegrationEnvKeepsTheModuleCache (CRW-1463, CRW-1489 C-2): integrationEnv points go at the module cache the caller's go uses,
// computed without running go: the caller's GOMODCACHE, else the first GOPATH entry's pkg/mod, else <caller HOME>/go/pkg/mod, each
// setting read from the caller's environment or else from its go env file (GOENV, else <user config dir>/go/env), which the fresh HOME
// no longer finds; `go env GOMODCACHE` under the built environment names it. Under the caller's real environment with GOPROXY=off, go
// loads what the tree-mode `go run -tags dev ./cmd/crw-dev` of the verification loads (go list -deps -tags dev ./cmd/crw-dev): an
// offline lookup in the kept cache, which fails once GOMODCACHE is left out and go looks in the fresh HOME.
func TestIntegrationEnvKeepsTheModuleCache(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go on PATH: %v", err)
	}
	fresh := t.TempDir()
	home, tmp := filepath.Join(fresh, "home"), filepath.Join(fresh, "tmp")
	for _, dir := range []string{home, tmp} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runGo := func(env []string, dir string, args ...string) (string, error) {
		cmd := exec.Command(goBin, args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	caller := t.TempDir()
	callerHome, gopath := filepath.Join(caller, "home"), filepath.Join(caller, "gopath")
	goEnvFile := func(dir, body string) string {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "env"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, "env")
	}
	xdg := filepath.Join(caller, "xdg")
	goEnvFile(filepath.Join(xdg, "go"), "GOPATH="+filepath.Join(caller, "xdg-gopath")+"\nGOPROXY=https://xdg-proxy.invalid\n")
	named := goEnvFile(filepath.Join(caller, "named"), "GOMODCACHE="+filepath.Join(caller, "named-cache")+"\n")
	if runtime.GOOS != "darwin" {
		goEnvFile(filepath.Join(callerHome, ".config", "go"), "GOPATH="+filepath.Join(caller, "file-gopath")+"\n")
	}
	type modCache struct {
		name   string
		caller map[string]string
		want   string
	}
	cases := []modCache{
		{"home only", map[string]string{"HOME": filepath.Join(caller, "bare-home")}, filepath.Join(caller, "bare-home", "go", "pkg", "mod")},
		{"gopath", map[string]string{"HOME": callerHome, "GOPATH": gopath + string(os.PathListSeparator) + filepath.Join(caller, "second")},
			filepath.Join(gopath, "pkg", "mod")},
		{"gomodcache", map[string]string{"HOME": callerHome, "GOPATH": gopath, "GOMODCACHE": filepath.Join(caller, "modcache")},
			filepath.Join(caller, "modcache")},
		{"go env file named by GOENV", map[string]string{"HOME": callerHome, "GOENV": named}, filepath.Join(caller, "named-cache")},
		{"go env file off", map[string]string{"HOME": callerHome, "GOENV": "off"}, filepath.Join(callerHome, "go", "pkg", "mod")},
	}
	if runtime.GOOS != "darwin" {
		cases = append(cases,
			modCache{"go env file of the caller's HOME", map[string]string{"HOME": callerHome}, filepath.Join(caller, "file-gopath", "pkg", "mod")},
			modCache{"go env file of XDG_CONFIG_HOME", map[string]string{"HOME": callerHome, "XDG_CONFIG_HOME": xdg}, filepath.Join(caller, "xdg-gopath", "pkg", "mod")})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := integrationEnv(func(k string) (string, bool) { v, ok := c.caller[k]; return v, ok }, home, tmp)
			if v, _ := envValue(env, "HOME"); v != home {
				t.Errorf("HOME is %q, want the fresh %s", v, home)
			}
			if v, _ := envValue(env, "TMPDIR"); v != tmp {
				t.Errorf("TMPDIR is %q, want the fresh %s", v, tmp)
			}
			for _, key := range []string{"GOPATH", "GOENV", "XDG_CONFIG_HOME"} {
				if v, ok := envValue(env, key); ok {
					t.Errorf("the caller's %s is kept: %q", key, v)
				}
			}
			// ci local's default work root stays below the caller's state directory, never below the fresh TMPDIR
			if v, _ := envValue(env, "XDG_STATE_HOME"); v != filepath.Join(c.caller["HOME"], ".local", "state") {
				t.Errorf("XDG_STATE_HOME is %q, want the caller's default state directory below %s", v, c.caller["HOME"])
			}
			got, err := runGo(env, t.TempDir(), "env", "GOMODCACHE")
			if err != nil || got != c.want {
				t.Errorf("go env GOMODCACHE is %q (%v), want %q", got, err, c.want)
			}
		})
	}
	if runtime.GOOS != "darwin" {
		// a module-source setting of the caller's go env file is kept as the caller's go has it
		env := integrationEnv(func(k string) (string, bool) {
			v, ok := map[string]string{"HOME": callerHome, "XDG_CONFIG_HOME": xdg}[k]
			return v, ok
		}, home, tmp)
		if got, err := runGo(env, t.TempDir(), "env", "GOPROXY"); err != nil || got != "https://xdg-proxy.invalid" {
			t.Errorf("go env GOPROXY is %q (%v), want the caller's go env file's", got, err)
		}
	}
	t.Run("the caller's crw configuration", func(t *testing.T) {
		config := filepath.Join(caller, "crw-home", ".config", "crw", "config.json")
		at := map[string]string{"HOME": filepath.Join(caller, "crw-home")}
		get := func(k string) (string, bool) { v, ok := at[k]; return v, ok }
		if v, ok := envValue(integrationEnv(get, home, tmp), "CRW_CONFIG"); ok {
			t.Errorf("CRW_CONFIG %q for a caller with no configuration file", v)
		}
		if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if v, _ := envValue(integrationEnv(get, home, tmp), "CRW_CONFIG"); v != config {
			t.Errorf("CRW_CONFIG is %q, want the caller's default configuration %s", v, config)
		}
		// a configuration home that mixes a symbolic link and ".." names the file the kernel finds, not the lexically cleaned one
		real := filepath.Join(caller, "real", "inner")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(caller, "link")); err != nil {
			t.Fatal(err)
		}
		raw := filepath.Join(caller, "real", "crw", "config.json")
		if err := os.MkdirAll(filepath.Dir(raw), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(raw, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		at["XDG_CONFIG_HOME"] = filepath.Join(caller, "link") + "/.."
		if v, _ := envValue(integrationEnv(get, home, tmp), "CRW_CONFIG"); v != at["XDG_CONFIG_HOME"]+"/crw/config.json" {
			t.Errorf("CRW_CONFIG is %q, want the raw join below %s", v, at["XDG_CONFIG_HOME"])
		}
		at["CRW_CONFIG"] = "/lab/named.json"
		if v, _ := envValue(integrationEnv(get, home, tmp), "CRW_CONFIG"); v != "/lab/named.json" {
			t.Errorf("CRW_CONFIG is %q, want the one the caller names", v)
		}
	})
	t.Run("offline lookup in the kept cache", func(t *testing.T) {
		if testing.Short() {
			t.Skip("loads the packages of cmd/crw-dev")
		}
		root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			t.Fatal(err)
		}
		getenv := func(k string) (string, bool) {
			if k == "GOPROXY" {
				return "off", true
			}
			return os.LookupEnv(k)
		}
		env := integrationEnv(getenv, home, tmp)
		load := []string{"list", "-mod=readonly", "-deps", "-tags", "dev", "./cmd/crw-dev"}
		if out, err := runGo(env, strings.TrimSpace(string(root)), load...); err != nil {
			t.Fatalf("go %v offline under integrationEnv: %v: %s", load, err, out)
		}
		// the control: without the kept GOMODCACHE, go looks in the fresh HOME's empty cache and the offline lookup fails
		var without []string
		for _, kv := range env {
			if !strings.HasPrefix(kv, "GOMODCACHE=") {
				without = append(without, kv)
			}
		}
		if out, err := runGo(without, strings.TrimSpace(string(root)), load...); err == nil {
			t.Errorf("go %v offline found the modules without the kept GOMODCACHE: %s", load, out)
		}
	})
}

// integrationTmpName is the name integrationTempDir gives the verification's own TMPDIR.
var integrationTmpName = regexp.MustCompile(`^crw-[0-9a-f]{8}$`)

// runIntegrationVerify runs CILocalIntegration with the fake crw-dev of labCrwDevScript over a fresh one-commit repository. Its files
// are made first; then setTmp sets the caller's TMPDIR, and the verification runs under it. It answers the verification's error, its
// notes, and the call crw-dev saw (ran false when crw-dev never ran).
func runIntegrationVerify(t *testing.T, setTmp func()) (call fakeCrwDevCall, ran bool, notes string, err error) {
	t.Helper()
	repo, head := labCommitRepo(t)
	log := filepath.Join(t.TempDir(), "crw-dev.jsonl")
	tools := crwconfig.VerifierTools{CrwDev: labCrwDevScript(t, log, repo), Config: "/lab/config.json"}
	record := filepath.Join(filepath.Dir(log), "record.json")
	batch := []string{"CRW_VERIFY_HEAD=" + head, "CRW_VERIFY_BASE=" + head, "CRW_VERIFY_RECORD=" + record}
	setTmp()
	var buf bytes.Buffer
	err = CILocalIntegration(tools, &buf)(context.Background(), repo, batch)
	raw, readErr := os.ReadFile(log)
	if errors.Is(readErr, os.ErrNotExist) {
		return fakeCrwDevCall{}, false, buf.String(), err
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if jsonErr := json.Unmarshal(bytes.TrimSpace(raw), &call); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	return call, true, buf.String(), err
}

// TestIntegrationTempDir_isBelowTheRunsTempDirWhateverItsLength (CRW-1569): the integration verification's own TMPDIR is
// <the run's TMPDIR, resolved>/crw-<8 hex>, mode 0700, removed after the verification. No length is counted and nothing moves it to
// another place: a 120-byte TMPDIR (longer when the test's own TMPDIR already is), a short one, a short link to a long directory (the
// physical parent) and an unset TMPDIR (tmploc.DefaultDir, /tmp) each give a directory directly below that one, and no note names a
// TMPDIR passed over. Sockets do not live below TMPDIR (tmploc.SocketDir), so ci local needs no room there.
func TestIntegrationTempDir_isBelowTheRunsTempDirWhateverItsLength(t *testing.T) {
	base := t.TempDir()
	long := base + "/" + strings.Repeat("t", max(1, 120-len(base)-1))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Logf("the long TMPDIR is %d bytes", len(long))
	short := filepath.Join(base, "s")
	if err := os.Mkdir(short, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "l")
	if err := os.Symlink(long, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, tmpdir string
		unset        bool
		parent       string
	}{
		{name: "a TMPDIR of 120 bytes or more", tmpdir: long, parent: long},
		{name: "a short TMPDIR", tmpdir: short, parent: short},
		{name: "a short link to a long directory", tmpdir: link, parent: long},
		{name: "no TMPDIR: the default", unset: true, parent: "/tmp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			call, ran, notes, err := runIntegrationVerify(t, func() {
				t.Setenv("TMPDIR", c.tmpdir)
				if c.unset {
					if err := os.Unsetenv("TMPDIR"); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err != nil || !ran {
				t.Fatalf("the verification: %v (crw-dev ran: %v); notes %q", err, ran, notes)
			}
			want, err := filepath.EvalSymlinks(c.parent)
			if err != nil {
				t.Fatal(err)
			}
			tmp, _ := envValue(call.Env, "TMPDIR")
			if filepath.Dir(call.TmpPhysical) != want || !integrationTmpName.MatchString(filepath.Base(call.TmpPhysical)) {
				t.Errorf("crw-dev's TMPDIR %q (physically %q) is not <%s>/crw-<8 hex>", tmp, call.TmpPhysical, want)
			}
			if call.TmpMode != "-rwx------" {
				t.Errorf("crw-dev's TMPDIR %q has mode %q, want 0700", call.TmpPhysical, call.TmpMode)
			}
			if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("crw-dev's TMPDIR %s is still there after the verification: %v", tmp, err)
			}
			if strings.Contains(notes, "TMPDIR") {
				t.Errorf("a note names a TMPDIR passed over: %q", notes)
			}
		})
	}
}

// TestIntegrationTempDir_refusesWithTheReason_neverTmp (CRW-1569): a TMPDIR the verification cannot use stops it with a host error that
// names the TMPDIR and why, before crw-dev runs. Nothing is made anywhere else: no /tmp directory, no crw-dev run.
func TestIntegrationTempDir_refusesWithTheReason_neverTmp(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, tmpdir, why string }{
		{"a TMPDIR that does not resolve", filepath.Join(base, "missing"), "does not exist"},
		{"a TMPDIR that is a file", file, "is not a directory"},
		{"a relative TMPDIR", "rel/tmp", "tmpdir_not_absolute"},
	}
	if os.Geteuid() != 0 {
		ro := filepath.Join(base, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
		cases = append(cases, struct{ name, tmpdir, why string }{"an unwritable TMPDIR", ro, "is not writable"})
	} else {
		t.Log("running as root: the unwritable row cannot be built (root writes everywhere)")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			call, ran, notes, err := runIntegrationVerify(t, func() { t.Setenv("TMPDIR", c.tmpdir) })
			var host *dagsched.IntegrationVerifierHostError
			if !errors.As(err, &host) {
				t.Fatalf("the verification under TMPDIR=%q: %v, want a host error (crw-dev ran: %v, its TMPDIR %q; notes %q)", c.tmpdir, err, ran, call.TmpPhysical, notes)
			}
			if !strings.Contains(host.Detail, c.tmpdir) || !strings.Contains(host.Detail, c.why) {
				t.Errorf("the host error %q does not name the TMPDIR %q and %q", host.Detail, c.tmpdir, c.why)
			}
			if ran {
				t.Errorf("crw-dev ran with TMPDIR %q after the caller's %q was refused", call.TmpPhysical, c.tmpdir)
			}
		})
	}
}
