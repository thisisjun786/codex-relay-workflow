package install_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
)

// main runs `crw install` over env and answers the exit status, stdout and stderr.
func (h *host) main(t *testing.T, env scope.Env, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := install.Main(context.Background(), args, env, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// The destination is fixed: there is no --dest, every command acts on
// <home>/.local/share/crw-runtime, and a host record whose pointer is another link (a host
// installed at another destination) is refused by every command that would act on it - with
// the repair - and reported by status, so hook and register-mcp never write settings naming a
// pointer the host does not run.
func TestTheDestinationIsFixed(t *testing.T) {
	h := newHost(t)
	if code, _, stderr := h.main(t, h.env, "status", "--dest", h.dest); code != install.Usage || !strings.Contains(stderr, "-dest") {
		t.Fatalf("--dest: exit %d %s", code, stderr)
	}
	o := h.options()
	o.Dest = filepath.Join(h.home, "opt", "crw-runtime")
	if _, code := install.Install(context.Background(), o, "install", install.Source{From: archive(t, "0.9.0", "")}); code != install.OK {
		t.Fatalf("install elsewhere: exit %d", code)
	}
	for _, args := range [][]string{
		{"hook", "--owner", "plugin", "--marker-root", filepath.Join(h.home, "markers")},
		{"register-mcp", "--owner", "plugin"},
		{"rollback"},
		{"install", "--from", archive(t, "0.9.1", "")},
	} {
		code, stdout, _ := h.main(t, h.env, args...)
		if code != install.Refused || !strings.Contains(stdout, `"repair"`) || !strings.Contains(stdout, pointer.Path(filepath.Join(h.home, "opt", "crw-runtime"))) {
			t.Fatalf("%v: exit %d\n%s", args, code, stdout)
		}
	}
	for _, name := range []string{install.SettingsName, install.BridgeRecordName} {
		if _, err := os.Lstat(filepath.Join(h.codex, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was written for a pointer the host does not run", name)
		}
	}
	if code, stdout, _ := h.main(t, h.env, "status"); code != install.OK || !strings.Contains(stdout, `"agrees": false`) {
		t.Fatalf("status: exit %d\n%s", code, stdout)
	}

	fixed := newHost(t)
	if code, stdout, stderr := fixed.main(t, fixed.env, "hook", "--owner", "plugin", "--marker-root", filepath.Join(fixed.home, "markers")); code != install.OK {
		t.Fatalf("hook on the fixed destination: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(readFile(t, filepath.Join(fixed.codex, install.SettingsName)), filepath.Join(fixed.home, ".local", "share", "crw-runtime", "current", "bin", "crw-completion-hook")) {
		t.Fatal("hook did not name the fixed destination's pointer")
	}
}

// A path holding a byte that is not UTF-8 - from HOME, CODEX_HOME, XDG_STATE_HOME or a path flag
// - is refused as a usage error naming where it came from, never recorded with a replacement
// character; a relative XDG_STATE_HOME is refused as well, never read against the working
// directory, and so is a HOME pathlib spells apart from its lexical join (a "..", or exactly two
// leading slashes). A document built from the environment (a marker root) is refused by its writer.
func TestPathsTheInstallerCannotSpellAreRefused(t *testing.T) {
	h := newHost(t)
	for name, tc := range map[string]struct {
		env  scope.Env
		args []string
		want string
	}{
		"HOME":                      {env: h.env.With("HOME", h.home+"/d\xff"), args: []string{"status"}, want: "HOME"},
		"CODEX_HOME":                {env: h.env.With("CODEX_HOME", h.home+"/c\xff"), args: []string{"status"}, want: "CODEX_HOME"},
		"XDG_STATE_HOME":            {env: h.env.With("XDG_STATE_HOME", h.home+"/s\xff"), args: []string{"status"}, want: "XDG_STATE_HOME"},
		"a relative XDG_STATE_HOME": {env: h.env.With("XDG_STATE_HOME", "relstate"), args: []string{"status"}, want: "XDG_STATE_HOME"},
		"HOME with ..":              {env: h.env.With("HOME", h.home+"/x/.."), args: []string{"status"}, want: "HOME"},
		"HOME with a leading //":    {env: h.env.With("HOME", "/"+h.home), args: []string{"status"}, want: "which pathlib spells '/" + h.home + "'"},
		"--marker-root":             {env: h.env, args: []string{"hook", "--owner", "plugin", "--marker-root", h.home + "/m\xff"}, want: "--marker-root"},
		"--bridge-arg":              {env: h.env, args: []string{"register-mcp", "--owner", "plugin", "--bridge-arg", "--state-dir=" + h.home + "/l\xff"}, want: "--bridge-arg"},
	} {
		code, stdout, stderr := h.main(t, tc.env, tc.args...)
		if code != install.Usage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit %d\nstdout %s\nstderr %s", name, code, stdout, stderr)
		}
	}
	if _, err := os.Lstat(filepath.Join(h.codex, install.SettingsName)); !os.IsNotExist(err) {
		t.Fatal("a refused hook wrote settings")
	}
	// pathlib keeps exactly two leading slashes and folds three or more to one, as a lexical join
	// does, so a HOME starting with "///" names the fixed destination and is read.
	if code, stdout, stderr := h.main(t, h.env.With("HOME", "//"+h.home), "status"); code != install.OK || !strings.Contains(stdout, `"`+h.dest+`"`) {
		t.Fatalf("HOME with three leading slashes: exit %d\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	hook := h.hookOptions()
	hook.MarkerRoot = h.home + "/m\xff"
	if result, code := install.Hook(context.Background(), h.options(), hook); code == install.OK {
		t.Fatalf("a marker root that is not UTF-8: exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Lstat(filepath.Join(h.codex, install.SettingsName)); !os.IsNotExist(err) {
		t.Fatal("settings naming a path that is not UTF-8 were written")
	}
}

// An empty directory argument to rollback is a usage error, as it is for remove: an unset
// variable spelled as the directory must not send the host to the outgoing selection.
func TestAnEmptyRollbackDirectoryIsAUsageError(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	updated := h.pointerTarget(t)
	for _, empty := range []string{"", "   "} {
		if code, _, stderr := h.main(t, h.env, "rollback", empty); code != install.Usage || !strings.Contains(stderr, "empty directory") {
			t.Fatalf("%q: exit %d %s", empty, code, stderr)
		}
		if h.pointerTarget(t) != updated {
			t.Fatalf("%q: the pointer moved", empty)
		}
	}
}

// --execution-policy given empty, or given twice with an empty last value, is a path the policy
// reading refuses (runtime_install.py tests `is not None`), never "no policy": nothing is written
// and the bridge never starts with role checks off.
func TestAnEmptyExecutionPolicyIsRefused(t *testing.T) {
	h := newHost(t)
	policy, _ := h.policy(t)
	for _, args := range [][]string{
		{"register-mcp", "--owner", "plugin", "--execution-policy", ""},
		{"register-mcp", "--owner", "plugin", "--execution-policy", "   "},
		{"register-mcp", "--owner", "plugin", "--execution-policy", policy, "--execution-policy", ""},
	} {
		code, stdout, _ := h.main(t, h.env, args...)
		if code != install.Refused || !strings.Contains(stdout, install.PolicyUnreadable) {
			t.Fatalf("%v: exit %d\n%s", args, code, stdout)
		}
	}
	if _, err := os.Lstat(filepath.Join(h.codex, install.BridgeRecordName)); !os.IsNotExist(err) {
		t.Fatal("a record was written with the policy off")
	}
}

// A registration that reaches the pointer through another spelling of it - a symlinked home -
// is judged by the pointer's identity: a Stop registration running python3 through it is the
// second owner a Go promotion refuses, and a bridge table naming the pointer's bridge that way is
// the pointer's bridge, not a foreign one.
func TestTheSecondOwnerRuleKnowsThePointerByIdentity(t *testing.T) {
	h := newHost(t)
	venv, _ := h.pythonEraHost(t)
	alias := filepath.Join(filepath.Dir(h.home), filepath.Base(h.home)+"-alias")
	if err := os.Symlink(h.home, alias); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(alias) })
	aliasPointer := filepath.Join(alias, ".local", "share", "crw-runtime", "current")
	write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.codex-thread-bridge]\ncommand = \""+filepath.Join(aliasPointer, "bin", "codex-thread-bridge")+"\"\n")
	if err := os.Remove(filepath.Join(h.codex, install.SettingsName)); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+filepath.Join(aliasPointer, "bin", "python3")+` /repo/scripts/completion_hook.py /x.json", "timeout": 10}]}]}}`)
	first := archive(t, "0.9.0", "")
	result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: first})
	if code != install.Refused || at(result, "failedStep") != "refuse a second owner" || !strings.Contains(golden.Canon(at(result, "steps")), "provides no bin/python3") || h.pointerTarget(t) != venv {
		t.Fatalf("the registration through the alias: exit %d\n%s", code, golden.Canon(result))
	}
	if err := os.Remove(filepath.Join(h.codex, "hooks.json")); err != nil {
		t.Fatal(err)
	}
	h.mustInstall(t, "update", first)
}

// A rollback records as outgoing the runtime the host leaves - the one the pointer names -
// and, where the pointer does not move, keeps outgoing as it was. The split-selection repair
// therefore keeps the baseline, and after an interrupted rollback (the record selects one runtime
// and the pointer names another) a bare rollback refuses and names both, and finishing the move
// by name records the runtime the pointer left.
func TestOutgoingIsTheRuntimeThePointerLeaves(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old, updated := runtimeDir(h, "0.9.0", first, t), runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	if _, err := record.Update(h.record, 1, record.Delta{Select: []contract.Field{{Key: "codex-thread-bridge", Value: filepath.Join(old, "bin")}}}); err != nil {
		t.Fatal(err)
	}
	if result, code := install.Rollback(context.Background(), h.options(), updated); code != install.OK || at(h.hostRecord(t), "outgoing", "codex-thread-bridge", "selected") != filepath.Join(old, "bin") || at(h.hostRecord(t), "outgoing", "codex-session-relay", "selected") != filepath.Join(old, "bin") {
		t.Fatalf("the split repair: exit %d outgoing %s\n%s", code, golden.Canon(at(h.hostRecord(t), "outgoing")), golden.Canon(result))
	}
	if result, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != old {
		t.Fatalf("a bare rollback after the repair: exit %d\n%s", code, golden.Canon(result))
	}

	// Interrupted: the record selects old again and the pointer still names updated.
	var both []contract.Field
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge"} {
		both = append(both, contract.Field{Key: name, Value: filepath.Join(updated, "bin")})
	}
	if _, err := record.Update(h.record, 1, record.Delta{Select: both}); err != nil {
		t.Fatal(err)
	}
	if err := pointer.Place(pointer.Path(h.dest), updated); err != nil {
		t.Fatal(err)
	}
	var back []contract.Field
	for _, name := range []string{"codex-session-relay", "codex-thread-bridge"} {
		back = append(back, contract.Field{Key: name, Value: filepath.Join(old, "bin")})
	}
	if _, err := record.Update(h.record, 1, record.Delta{Select: back}); err != nil {
		t.Fatal(err)
	}
	if refused, code := install.Rollback(context.Background(), h.options(), ""); code != install.Refused || !strings.Contains(text(at(refused, "refused")), "interrupted") || h.pointerTarget(t) != updated {
		t.Fatalf("a bare rollback over an interrupted move: exit %d\n%s", code, golden.Canon(refused))
	}
	if result, code := install.Rollback(context.Background(), h.options(), old); code != install.OK || at(h.hostRecord(t), "outgoing", "codex-session-relay", "selected") != filepath.Join(updated, "bin") {
		t.Fatalf("finishing the move: exit %d outgoing %s\n%s", code, golden.Canon(at(h.hostRecord(t), "outgoing")), golden.Canon(result))
	}
	if result, code := install.Rollback(context.Background(), h.options(), ""); code != install.OK || h.pointerTarget(t) != updated {
		t.Fatalf("a bare rollback returns to what the pointer left: exit %d\n%s", code, golden.Canon(result))
	}
}

// Rollback holds its target's <env>.crw-lock - runtime_install.py's lock on the directory, which
// its reclaim holds while it removes one - from before it reads the record under the promotion
// lock until it has acted: held by another run, the rollback refuses with nothing written; and
// while the rollback commits, nobody else can take it.
func TestARollbackHoldsItsTargetsDirectoryLock(t *testing.T) {
	h := newHost(t)
	first := archive(t, "0.9.0", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", archive(t, "0.9.1", ""))
	updated := h.pointerTarget(t)
	saved := record.LockTimeout
	record.LockTimeout = 200 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	held, err := record.Lock(old, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := readFile(t, h.record)
	if refused, code := install.Rollback(context.Background(), h.options(), ""); code != install.Refused || !strings.Contains(text(at(refused, "refused")), old+record.LockSuffix) || readFile(t, h.record) != before || h.pointerTarget(t) != updated {
		t.Fatalf("beside another run's directory lock: exit %d\n%s", code, golden.Canon(refused))
	}
	held.Release()
	excluded := false
	restore := install.ReplaceSelectionCommit(func(path string, version int, delta record.Delta) (reading.Reading, error) {
		if other, err := record.Lock(old, 0); err == nil {
			other.Release()
		} else {
			excluded = true
		}
		return record.Update(path, version, delta)
	})
	result, code := install.Rollback(context.Background(), h.options(), "")
	restore()
	if code != install.OK || !excluded || h.pointerTarget(t) != old {
		t.Fatalf("exit %d, excluded %v\n%s", code, excluded, golden.Canon(result))
	}
}

// A claim runtime_install.py must read stays in its shape: settling the STAGING claim of a Python
// venv a rollback returns to writes COMPLETE with writtenBy runtime_install.py, which Python's
// own staging.shape accepts.
func TestAClaimRuntimeInstallPyWroteStaysOneItCanRead(t *testing.T) {
	h := newHost(t)
	venv, _ := h.pythonEraHost(t)
	h.mustInstall(t, "update", archive(t, "0.9.0", ""))
	write(t, staging.ClaimPath(venv), string(record.Encode(staging.Payload(staging.Staging, staging.WrittenByPython, "CRW-116", "1", 1, "host", "2026-09-25T00:40:21Z"))))
	result, code := install.Rollback(context.Background(), h.options(), venv)
	if code != install.OK || at(result, "claim", "settled") != true {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	claim := readFile(t, staging.ClaimPath(venv))
	if !strings.Contains(claim, `"writtenBy": "runtime_install.py"`) || !strings.Contains(claim, `"state": "COMPLETE"`) {
		t.Fatalf("claim:\n%s", claim)
	}
	if hostPython() == "" {
		return
	}
	cmd := exec.Command(hostPython(), "-c", "import json, sys\nsys.path.insert(0, sys.argv[1])\nfrom crw_runtime import staging\nstaging.shape(json.load(open(sys.argv[2])))", filepath.Join(golden.Root(), "scripts"), staging.ClaimPath(venv))
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("runtime_install.py's staging.shape refuses the claim: %v\n%s", err, stderr.String())
	}
}
