package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1265. A relay started from the account home (a login shell, a user unit with no
// WorkingDirectory) must not keep that directory as its cwd: the supervisor and the worker run in
// the relay's state directory. Start and SpawnWorker both start their process through launch.
func TestRelayStartedFromHomeRunsInStateDir(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "relay-state")
	t.Chdir(home)
	s := &Service{Selection: store.StateSelection{Path: state}}
	cmd := exec.Command("/bin/sh", "-c", "pwd -P > cwd.out")
	if err := s.launch(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(state, "cwd.out"))
	if err != nil {
		t.Fatalf("the process did not run in the state directory: %v", err)
	}
	want, _ := filepath.EvalSymlinks(state)
	if got := strings.TrimSpace(string(raw)); got != want {
		t.Fatalf("the process ran in %s, want the state directory %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(home, "cwd.out")); err == nil {
		t.Fatal("the process wrote into the directory the relay was started from")
	}
}

// CRW-1265. The child runs in the state directory, so a socket path the caller gave relative to its own
// directory is handed over absolute, naming the file the caller meant; a state path that is not
// absolute leaves the child in the caller's directory and the socket as it was given.
func TestCommandKeepsTheCallersRelativeSocket(t *testing.T) {
	start := t.TempDir()
	t.Chdir(start)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// Joined as raw text, nothing cleaned: see TestCommandKeepsTheCallersSocketThroughALinkAndDotDot.
	want := wd + "/./app.sock"
	socketOf := func(s *Service) string {
		cmd, err := s.command("service", "run")
		if err != nil {
			t.Fatal(err)
		}
		for i, arg := range cmd.Args {
			if arg == "--socket" {
				return cmd.Args[i+1]
			}
		}
		t.Fatalf("no --socket in %v", cmd.Args)
		return ""
	}
	if got := socketOf(&Service{Selection: store.StateSelection{Path: filepath.Join(t.TempDir(), "state")}, Socket: "./app.sock"}); got != want {
		t.Fatalf("the child got socket %q, want %q", got, want)
	}
	if got := socketOf(&Service{Selection: store.StateSelection{Path: "state"}, Socket: "./app.sock"}); got != "./app.sock" {
		t.Fatalf("a child left in the caller's directory got socket %q", got)
	}
}

// sameFile is whether a and b, each read from the directory it is read in, name one file.
func sameFile(t *testing.T, a, b os.FileInfo) bool {
	t.Helper()
	return a != nil && b != nil && os.SameFile(a, b)
}

// CRW-1265. A relative socket that goes through a symbolic link and then ".." names the directory the
// kernel resolves, not the one a lexical clean names: the child, in the state directory, gets a path
// that still names the file the caller's spelling names.
func TestCommandKeepsTheCallersSocketThroughALinkAndDotDot(t *testing.T) {
	root := t.TempDir()
	caller, sub := filepath.Join(root, "caller"), filepath.Join(root, "other", "sub")
	for _, dir := range []string{caller, sub} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sub, filepath.Join(caller, "link")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(root, "other", "app.sock"), filepath.Join(caller, "app.sock")} {
		if err := os.WriteFile(f, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(caller)
	meant, err := os.Stat("link/../app.sock")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	cmd, err := (&Service{Selection: store.StateSelection{Path: state}, Socket: "link/../app.sock"}).command("service", "run")
	if err != nil {
		t.Fatal(err)
	}
	got := ""
	for i, arg := range cmd.Args {
		if arg == "--socket" {
			got = cmd.Args[i+1]
		}
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("the child got the relative socket %q", got)
	}
	named, _ := os.Stat(got)
	if !sameFile(t, meant, named) {
		t.Fatalf("the child got socket %q, which does not name the caller's link/../app.sock (%s)", got, filepath.Join(root, "other", "app.sock"))
	}
}

// CRW-1265. Start resolves the execution policy in the caller's directory and the supervisor runs in the
// state directory, so a policy the environment names relative to the caller's directory (through a link
// and "..", too) reaches the supervisor as a path that names the same file: the child reads the policy,
// and the digest, that the launch checked.
func TestStartHandsTheChildTheCallersRelativePolicy(t *testing.T) {
	const meant = `{"roles":{"parent":{"model":"m","reasoningEffort":"high"},"child":{"model":"m","reasoningEffort":"high"}}}`
	const other = `{"roles":{"parent":{"model":"n","reasoningEffort":"high"},"child":{"model":"n","reasoningEffort":"high"}}}`
	root := t.TempDir()
	caller, sub, state := filepath.Join(root, "caller"), filepath.Join(root, "other", "sub"), filepath.Join(root, "state")
	for _, dir := range []string{caller, sub, state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sub, filepath.Join(caller, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other", "policy.json"), []byte(meant), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caller, "policy.json"), []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, stated := range []string{"policy.json", "link/../policy.json", "./link/../policy.json"} {
		t.Run(stated, func(t *testing.T) {
			t.Chdir(caller)
			policy := ResolveLaunchPolicyAt(state, stated)
			if policy.Get("state") != "declared" {
				t.Fatalf("the caller's policy did not resolve: %v", policy)
			}
			s := &Service{Selection: store.StateSelection{Path: state}, Scope: &ScopeRegistry{Authority: "production"}}
			cmd, err := s.startCommand([]string{"service", "run"}, policy, "L")
			if err != nil {
				t.Fatal(err)
			}
			child := ""
			for _, v := range cmd.Env {
				if value, ok := strings.CutPrefix(v, execution.EnvPolicy+"="); ok {
					child = value
				}
			}
			// The child in the state directory reads the policy the same way the launch did.
			t.Chdir(state)
			got := ResolveLaunchPolicyAt(state, child)
			if got.Get("state") != "declared" || got.Get("digest") != policy.Get("digest") {
				t.Fatalf("the child, in the state directory, read %v from %q; the launch read digest %v", got, child, policy.Get("digest"))
			}
		})
	}
	// A state path that is not absolute leaves the child in the caller's directory, and the policy as given.
	t.Chdir(caller)
	policy := ResolveLaunchPolicyAt("state", "policy.json")
	cmd, err := (&Service{Selection: store.StateSelection{Path: "state"}, Scope: &ScopeRegistry{Authority: "production"}}).startCommand([]string{"service", "run"}, policy, "L")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, execution.EnvPolicy+"=policy.json") {
		t.Fatal("a child left in the caller's directory did not get the policy as the caller gave it")
	}
}
