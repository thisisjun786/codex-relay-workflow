package integrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A temporary git world: a bare remote, the checkout (a clone of it), another clone that
// authors heads, the queue, and shell stubs for the gate, the verifier and the version
// re-derivation. Nothing here touches a real host path.

const manifestPath = "plugins/crw/.codex-plugin/plugin.json"

type world struct {
	t        *testing.T
	root     string
	remote   string
	checkout string
	other    string
	ctl      string
	cfg      Config
	r        *Runner
	env      []string

	mu    sync.Mutex
	calls [][]string
	now   time.Time
}

func (w *world) gitEnv() []string {
	return append(os.Environ(),
		"HOME="+filepath.Join(w.root, "home"),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
}

func (w *world) git(dir string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = w.gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *world) write(path, content string) {
	w.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		w.t.Fatal(err)
	}
}

func manifest(version string) string {
	return "{\n  \"name\": \"crw\",\n  \"version\": \"" + version + "\",\n  \"description\": \"x\"\n}\n"
}

const gateScript = `#!/bin/sh
echo "$1 $2" >> "$CTL_DIR/gate-calls"
if [ -f "$CTL_DIR/block-$1" ]; then echo "BLOCK: no evaluation"; exit 1; fi
echo "PASS evaluated"
`

const verifyScript = `#!/bin/sh
n=$(cat "$CTL_DIR/verify-n" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "$CTL_DIR/verify-n"
echo "$CRW_VERIFY_HEAD $CRW_VERIFY_BASE $CRW_VERIFY_RECORD $(pwd)" >> "$CTL_DIR/verify-calls"
[ -f "$CTL_DIR/during-verify.sh" ] && sh "$CTL_DIR/during-verify.sh"
want="v-$(ls plugins/crw/skills | wc -l | tr -d ' ')"
have=$(grep -o '"version": "[^"]*"' plugins/crw/.codex-plugin/plugin.json | cut -d'"' -f4)
if [ "$want" != "$have" ]; then echo "version drift $have != $want"; exit 9; fi
rc=$(sed -n "${n}p" "$CTL_DIR/verify-rc" 2>/dev/null)
[ -z "$rc" ] && rc=0
echo '{"ok":true}' > "$CRW_VERIFY_RECORD"
exit $rc
`

const recordScript = `#!/bin/sh
echo "$TMPDIR_FROM_CONFIG" >> "$CTL_DIR/record-calls"
n=$(ls plugins/crw/skills | wc -l | tr -d ' ')
sed "s/\"version\": \"[^\"]*\"/\"version\": \"v-$n\"/" plugins/crw/.codex-plugin/plugin.json > plugins/crw/.codex-plugin/plugin.json.new && mv plugins/crw/.codex-plugin/plugin.json.new plugins/crw/.codex-plugin/plugin.json
`

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, root: root, remote: filepath.Join(root, "remote.git"), checkout: filepath.Join(root, "checkout"),
		other: filepath.Join(root, "other"), ctl: filepath.Join(root, "ctl"), now: time.Date(2026, 10, 11, 3, 4, 5, 0, time.Local)}
	for _, d := range []string{"home", "ctl", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	w.git(root, "init", "-q", "--bare", "-b", "dev", w.remote)
	w.git(root, "clone", "-q", w.remote, w.other)
	w.git(w.other, "checkout", "-q", "-b", "dev")
	w.write(filepath.Join(w.other, "README.md"), "base\n")
	w.write(filepath.Join(w.other, "shared.txt"), "line1\nline2\nline3\n")
	w.write(filepath.Join(w.other, manifestPath), manifest("v-1"))
	w.write(filepath.Join(w.other, "plugins/crw/skills/s0.txt"), "s0\n")
	w.git(w.other, "add", "-A")
	w.git(w.other, "commit", "-q", "-m", "base")
	w.git(w.other, "push", "-q", "origin", "dev")
	w.git(root, "clone", "-q", w.remote, w.checkout)

	for name, body := range map[string]string{"gate.sh": gateScript, "verify.sh": verifyScript, "record.sh": recordScript} {
		w.write(filepath.Join(root, "scripts", name), body)
	}
	w.cfg = Config{
		Checkout: w.checkout, QueueDir: filepath.Join(root, "queue"), LockFile: filepath.Join(root, "lock"),
		RunsDir: filepath.Join(root, "runs"), WorkRoot: filepath.Join(root, "work"),
		GateCommand:          []string{filepath.Join(root, "scripts/gate.sh"), "{issue}", "{head}"},
		VerifyCommand:        []string{filepath.Join(root, "scripts/verify.sh")},
		RecordVersionCommand: []string{filepath.Join(root, "scripts/record.sh")},
		RecordVersionEnv:     map[string]string{"TMPDIR_FROM_CONFIG": "{tmpdir}"},
	}
	cfg, err := w.cfg.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	w.cfg = cfg
	w.env = append(w.gitEnv(), "CTL_DIR="+w.ctl, "OTHER_DIR="+w.other)
	w.r = &Runner{Cfg: cfg, Env: w.env, Now: func() time.Time { return w.now }, Sleep: func(context.Context, time.Duration) error { return nil },
		Exec: func(ctx context.Context, c Cmd) (int, error) {
			w.mu.Lock()
			w.calls = append(w.calls, append([]string(nil), c.Argv...))
			w.mu.Unlock()
			return OSExec(ctx, c)
		}}
	return w
}

// head authors a commit on top of dev in the other clone and publishes it as a branch of the
// remote (so the checkout's fetch sees the commit), and returns its sha.
func (w *world) head(name string, files map[string]string, publish bool) string {
	w.t.Helper()
	w.git(w.other, "fetch", "-q", "origin")
	w.git(w.other, "checkout", "-q", "-B", name, "origin/dev")
	for p, body := range files {
		w.write(filepath.Join(w.other, p), body)
	}
	w.git(w.other, "add", "-A")
	w.git(w.other, "commit", "-q", "-m", name)
	sha := w.git(w.other, "rev-parse", "HEAD")
	if publish {
		w.git(w.other, "push", "-q", "origin", name+":refs/heads/"+name)
	}
	return sha
}

func (w *world) pend(num, head string) {
	w.t.Helper()
	w.write(filepath.Join(w.cfg.QueueDir, "pending", num), head+"\n")
}

func (w *world) ctlWrite(name, body string) {
	w.t.Helper()
	w.write(filepath.Join(w.ctl, name), body)
}

func (w *world) ctlRead(name string) string {
	data, _ := os.ReadFile(filepath.Join(w.ctl, name))
	return string(data)
}

func (w *world) where(num string) string {
	for _, d := range []string{"pending", "done", "conflict", "blocked"} {
		if _, err := os.Stat(filepath.Join(w.cfg.QueueDir, d, num)); err == nil {
			return d
		}
	}
	return ""
}

func (w *world) remoteDev() string { return w.git(w.remote, "rev-parse", "dev") }

func (w *world) pass() Report {
	w.t.Helper()
	w.now = w.now.Add(time.Minute)
	return w.r.Pass(context.Background())
}

func (w *world) argvCalls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.calls))
	for i, c := range w.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func (w *world) fileAt(rev, path string) string { return w.git(w.remote, "show", rev+":"+path) }

func (w *world) mustWhere(num, want string) {
	w.t.Helper()
	if got := w.where(num); got != want {
		w.t.Fatalf("queue file %s is in %q, want %q", num, got, want)
	}
}

func requireExit(t *testing.T, rep Report, exit int, result string) {
	t.Helper()
	if rep.Exit != exit || rep.Result != result {
		t.Fatalf("pass = exit %d result %q (error %q), want exit %d result %q", rep.Exit, rep.Result, rep.Error, exit, result)
	}
}
