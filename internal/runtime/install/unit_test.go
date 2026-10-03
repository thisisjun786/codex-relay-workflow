package install_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

const (
	policyVar = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
	scopeVar  = "CODEX_SESSION_RELAY_SCOPE_DIR"
	notLoaded = "LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n"
	freshOnes = `{"enabled":false,"launchPolicy":{"source":null},"scopeAuthority":"production","running":false}`
)

// fakeManager is systemctl --user: it records every call, answers show from a fixed text and can
// fail a verb. Nothing here reaches the real user manager.
type fakeManager struct {
	calls [][]string
	show  string
	fail  map[string]error
}

func (m *fakeManager) run(_ context.Context, args ...string) (string, error) {
	m.calls = append(m.calls, args)
	if err := m.fail[args[0]]; err != nil {
		return "", err
	}
	if args[0] == "show" {
		return m.show, nil
	}
	return "", nil
}

func (m *fakeManager) verbs() (out []string) {
	for _, call := range m.calls {
		out = append(out, call[0])
	}
	return out
}

func loadedFrom(path, active string) string {
	return "LoadState=loaded\nActiveState=" + active + "\nFragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n"
}

// unitHost is a temporary HOME with a stub relay at the pointer path: a script that records its
// arguments and environment and answers service status with a canned document.
type unitHost struct {
	t                    *testing.T
	home, dest, dir, out string
	manager              *fakeManager
	o                    install.Options
}

func newUnitHost(t *testing.T) *unitHost {
	t.Helper()
	home := t.TempDir()
	u := &unitHost{t: t, home: home, dest: filepath.Join(home, ".local", "share", "crw-runtime"), dir: filepath.Join(home, "units"), out: filepath.Join(home, "stub-out"), manager: &fakeManager{show: notLoaded}}
	stub := filepath.Join(u.dest, "bin-stub", "bin", "codex-session-relay")
	put(t, stub, "#!/bin/sh\necho \"$@\" >> \"$STUB_OUT/argv\"\nenv | sort > \"$STUB_OUT/env\"\ncat \"$STUB_OUT/status.json\"\n", 0o755)
	if err := os.Symlink(filepath.Join(u.dest, "bin-stub"), filepath.Join(u.dest, "current")); err != nil {
		t.Fatal(err)
	}
	u.status(freshOnes)
	u.o = install.Options{Dest: u.dest, Socket: filepath.Join(home, "app.sock"), State: filepath.Join(home, "state"), Systemctl: u.manager.run,
		Env: scope.Env{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "STUB_OUT=" + u.out, policyVar + "=/elsewhere/policy.json", scopeVar + "=/elsewhere/scopes"}}
	return u
}

func (u *unitHost) status(document string) {
	u.t.Helper()
	put(u.t, filepath.Join(u.out, "status.json"), document, 0o644)
}
func (u *unitHost) path() string { return filepath.Join(u.dir, doctor.ServiceUnit) }

func (u *unitHost) register(s install.ServiceOptions) (record.Object, int) {
	s.UnitDir = u.dir
	return install.RegisterService(context.Background(), u.o, s)
}

func (u *unitHost) text() string {
	raw, err := os.ReadFile(u.path())
	if err != nil {
		u.t.Fatal(err)
	}
	return string(raw)
}

func (u *unitHost) expect(result record.Object, code, want int, outcome string) {
	u.t.Helper()
	if code != want || at(result, "outcome") != outcome {
		u.t.Fatalf("exit %d (want %d), outcome %v (want %s)\n%s", code, want, at(result, "outcome"), outcome, golden.Canon(result))
	}
}

func contains(values any, want string) bool {
	for _, v := range golden.List(values) {
		if s, _ := v.(string); strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// A fresh registration writes the one unit, asks the manager about the name, and enables it by path:
// no start, stop, restart or reload. The status read the unit's inputs come from runs through the
// pointer's relay in an environment without the policy and scope variables, even though the
// registering environment sets both, and it only reads: the intent is never written.
func TestRegisterWritesTheUnitAndEnablesIt(t *testing.T) {
	u := newUnitHost(t)
	result, code := u.register(install.ServiceOptions{})
	u.expect(result, code, install.OK, install.UnitCreated)
	relay, state, socket := filepath.Join(u.dest, "current", "bin", "codex-session-relay"), u.o.State, u.o.Socket
	common := relay + " --state " + state + " --socket " + socket
	want := "# Written by crw install register-service and owned by crw install: change it with that command, not by hand.\n" +
		"[Unit]\nDescription=CRW relay service (starts the relay service once when the user manager starts)\nX-CRW-Owner=crw-install\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nTimeoutStartSec=60\nUnsetEnvironment=" + policyVar + " " + scopeVar + "\n" +
		"ExecStart=" + common + " service start\nExecStop=-" + common + " service stop\n\n[Install]\nWantedBy=default.target\n"
	if got := u.text(); got != want {
		t.Fatalf("unit text:\n%s\nwant:\n%s", got, want)
	}
	if info, _ := os.Stat(u.path()); info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", info.Mode())
	}
	if verbs := strings.Join(u.manager.verbs(), " "); verbs != "show enable" || u.manager.calls[1][1] != u.path() {
		t.Fatalf("systemctl calls: %v", u.manager.calls)
	}
	env, _ := os.ReadFile(filepath.Join(u.out, "env"))
	argv, _ := os.ReadFile(filepath.Join(u.out, "argv"))
	if strings.Contains(string(env), policyVar) || strings.Contains(string(env), scopeVar) || strings.TrimSpace(string(argv)) != "--socket "+socket+" --state "+state+" service status" {
		t.Fatalf("the status read ran as:\n%s\nenvironment:\n%s", argv, env)
	}
	if at(result, "serviceEnabled") != false || !contains(at(result, "warnings"), "service intent is not enabled") || !contains(at(result, "warnings"), "no execution policy is declared") {
		t.Fatalf("a fresh state: %s", golden.Canon(result))
	}
}

// Registering again converges instead of failing: the file is left alone and enable runs again, so a
// unit someone disabled by hand is enabled again. A dry run writes nothing and asks systemd only to read.
func TestRegisterIsIdempotentAndDryRunWritesNothing(t *testing.T) {
	u := newUnitHost(t)
	dry, code := u.register(install.ServiceOptions{DryRun: true})
	u.expect(dry, code, install.OK, install.UnitWouldCreate)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) || strings.Join(u.manager.verbs(), " ") != "show" {
		t.Fatalf("dry run: %v %v", err, u.manager.calls)
	}
	u.register(install.ServiceOptions{})
	first, _ := os.Stat(u.path())
	again, code := u.register(install.ServiceOptions{})
	u.expect(again, code, install.OK, install.UnitUnchanged)
	if second, _ := os.Stat(u.path()); !second.ModTime().Equal(first.ModTime()) || strings.Join(u.manager.verbs(), " ") != "show show enable show enable" || at(again, "applied") != false {
		t.Fatalf("rerun: %v", u.manager.calls)
	}
}

// Nothing the installer did not write is adopted or overwritten, and no enable follows a refusal.
func TestRegisterRefusesWhatItDoesNotOwn(t *testing.T) {
	for _, c := range []struct {
		name, outcome string
		arrange       func(u *unitHost)
	}{
		{"a file without the owner key", install.UnitForeign, func(u *unitHost) { put(t, u.path(), "[Service]\nExecStart=/bin/true\n", 0o644) }},
		{"a symbolic link", install.UnitForeign, func(u *unitHost) { os.MkdirAll(u.dir, 0o755); os.Symlink("/etc/hostname", u.path()) }},
		{"a directory", install.UnitForeign, func(u *unitHost) { os.MkdirAll(u.path(), 0o755) }},
		{"a drop-in directory", install.UnitModified, func(u *unitHost) { os.MkdirAll(u.path()+".d", 0o755) }},
		{"an owned unit that says something else", install.UnitDiffers, func(u *unitHost) {
			put(t, u.path(), "[Unit]\n"+doctor.UnitOwnerLine+"\n[Service]\nExecStart=/bin/other\n", 0o644)
		}},
		{"another unit that runs the relay's start", install.UnitSecondOwner, func(u *unitHost) {
			put(t, filepath.Join(u.dir, "hand-made.service"), "[Service]\nExecStart=/opt/bin/codex-session-relay --socket /s service start\n", 0o644)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitHost(t)
			c.arrange(u)
			result, code := u.register(install.ServiceOptions{})
			u.expect(result, code, install.Refused, c.outcome)
			if strings.Contains(strings.Join(u.manager.verbs(), " "), "enable") {
				t.Fatalf("a refusal enabled something: %v", u.manager.calls)
			}
		})
	}
}

// The name is resolved by the manager, not by the file alone, and a manager that cannot be asked is not
// a manager that said no.
func TestRegisterAsksTheManager(t *testing.T) {
	for _, c := range []struct{ name, show, outcome string }{
		{"loaded from another fragment", loadedFrom("/usr/lib/systemd/user/crw-relay.service", "inactive"), install.UnitNameTaken},
		{"masked", "LoadState=masked\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n", install.UnitNameTaken},
		{"with drop-ins", strings.Replace(loadedFrom("UNIT", "inactive"), "DropInPaths=", "DropInPaths=/x/crw-relay.service.d/a.conf", 1), install.UnitModified},
		{"with a stale definition", strings.Replace(loadedFrom("UNIT", "inactive"), "NeedDaemonReload=no", "NeedDaemonReload=yes", 1), install.UnitModified},
		{"in an unknown state", "LoadState=error\n", install.UnitUnreadable},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitHost(t)
			u.manager.show = strings.ReplaceAll(c.show, "UNIT", u.path())
			result, code := u.register(install.ServiceOptions{})
			u.expect(result, code, install.Refused, c.outcome)
			if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refusal wrote the unit: %v", err)
			}
		})
	}
	u := newUnitHost(t)
	u.manager.fail = map[string]error{"show": errors.New("Failed to connect to bus")}
	result, code := u.register(install.ServiceOptions{})
	u.expect(result, code, install.Refused, install.UnitUnreadable)
}

// Inputs the unit cannot carry, or must not live where a runtime's removal would delete them.
func TestRegisterRefusesInputsTheUnitCannotCarry(t *testing.T) {
	u := newUnitHost(t)
	u.o.Socket = filepath.Join(u.home, "a socket.sock")
	result, code := u.register(install.ServiceOptions{})
	u.expect(result, code, install.Usage, "usage")
	for _, name := range []string{"x.txt", "-x.service", "../x.service", "a b.service"} {
		u = newUnitHost(t)
		if result, code = u.register(install.ServiceOptions{UnitName: name}); code != install.Usage {
			t.Fatalf("%s: %s", name, golden.Canon(result))
		}
	}
	u = newUnitHost(t)
	u.o.Socket = ""
	if _, code = u.register(install.ServiceOptions{}); code != install.Usage {
		t.Fatalf("no socket: exit %d", code)
	}
	u = newUnitHost(t)
	os.Remove(filepath.Join(u.dest, "current"))
	result, code = u.register(install.ServiceOptions{})
	u.expect(result, code, install.Refused, install.UnitUnreadable)
	u = newUnitHost(t)
	inside := filepath.Join(u.dest, "bin-stub", "units")
	linked := filepath.Join(u.home, "linked")
	os.MkdirAll(inside, 0o755)
	os.Symlink(inside, linked)
	for _, dir := range []string{inside, linked} {
		result, code = install.RegisterService(context.Background(), u.o, install.ServiceOptions{UnitDir: dir})
		u.expect(result, code, install.Refused, install.UnitDirInRuntime)
	}
	if _, err := os.Stat(filepath.Join(inside, doctor.ServiceUnit)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a unit was written under the runtime: %v", err)
	}
}

// What the boot start will find is read through the relay's own status: an unreadable launch declaration
// refuses as its start would, an enabled and declared service raises no warning of its own, a status that
// cannot be read is a warning, and a relative socket is resolved so the unit only ever names absolute paths.
func TestRegisterReadsTheBootStartInputs(t *testing.T) {
	u := newUnitHost(t)
	u.status(`{"enabled":true,"launchPolicy":{"source":"unreadable_record","detail":"not JSON"},"scopeAuthority":"production"}`)
	result, code := u.register(install.ServiceOptions{})
	u.expect(result, code, install.Refused, install.UnitUnreadable)
	u.status(`{"enabled":true,"launchPolicy":{"source":"record"},"scopeAuthority":"production"}`)
	result, code = u.register(install.ServiceOptions{})
	u.expect(result, code, install.OK, install.UnitCreated)
	if len(golden.List(at(result, "warnings"))) != 0 || at(result, "launchPolicySource") != "record" || at(result, "serviceEnabled") != true {
		t.Fatalf("an enabled, declared service: %s", golden.Canon(result))
	}
	u = newUnitHost(t)
	u.status("not json")
	u.o.Env = append(u.o.Env, "XDG_STATE_HOME="+u.home+"/state-home")
	u.o.Socket = "relative.sock"
	result, code = u.register(install.ServiceOptions{})
	u.expect(result, code, install.OK, install.UnitCreated)
	if !contains(at(result, "warnings"), "could not be read") || !contains(at(result, "warnings"), "XDG_STATE_HOME") || strings.Contains(u.text(), " --socket relative.sock") {
		t.Fatalf("an unreadable status: %s\n%s", golden.Canon(result), u.text())
	}
}

// An isolated target carries its scope into the unit and allows it; a production one unsets the scope
// variable the manager might hold.
func TestRegisterCarriesAnIsolatedScope(t *testing.T) {
	u := newUnitHost(t)
	scopes := filepath.Join(u.home, "scopes")
	u.status(`{"enabled":true,"launchPolicy":{"source":"record"},"scopeAuthority":"isolated"}`)
	result, code := u.register(install.ServiceOptions{ScopeDir: scopes})
	u.expect(result, code, install.OK, install.UnitCreated)
	text := u.text()
	if !strings.Contains(text, "Environment="+scopeVar+"="+scopes+"\n") || !strings.Contains(text, "service start --allow-isolated-scope\n") || !strings.Contains(text, "UnsetEnvironment="+policyVar+"\n") || at(result, "scopeAuthority") != "isolated" {
		t.Fatalf("isolated unit:\n%s\n%s", text, golden.Canon(result))
	}
	env, _ := os.ReadFile(filepath.Join(u.out, "env"))
	if !strings.Contains(string(env), scopeVar+"="+scopes+"\n") || strings.Contains(string(env), policyVar) {
		t.Fatalf("the status read's environment:\n%s", env)
	}
}

// Remove disables and deletes only what it wrote, only when the manager resolves the name to that file and
// says it is not running, in that order; every other case leaves the file and calls nothing that changes it.
func TestRemoveDisablesAndDeletesOnlyWhatItWrote(t *testing.T) {
	u := newUnitHost(t)
	u.register(install.ServiceOptions{})
	u.manager.calls = nil
	u.manager.show = loadedFrom(u.path(), "inactive")
	result, code := u.register(install.ServiceOptions{Remove: true})
	u.expect(result, code, install.OK, install.UnitRemoved)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) || strings.Join(u.manager.verbs(), " ") != "show disable daemon-reload" {
		t.Fatalf("remove: %v %v", err, u.manager.calls)
	}
	result, code = u.register(install.ServiceOptions{Remove: true})
	u.expect(result, code, install.OK, install.UnitAbsent)
	for _, c := range []struct{ name, outcome, file, show string }{
		{"a running unit", install.UnitActive, "", "active"},
		{"an activating unit", install.UnitActive, "", "activating"},
		{"a file that is not the installer's", install.UnitForeign, "[Service]\nExecStart=/bin/true\n", "inactive"},
		{"an install section that reaches another unit", install.UnitModified, "[Unit]\n" + doctor.UnitOwnerLine + "\n[Install]\nWantedBy=default.target\nAlso=other.service\n", "inactive"},
		{"another fragment", install.UnitNameTaken, "", "elsewhere"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitHost(t)
			u.register(install.ServiceOptions{})
			if c.file != "" {
				put(t, u.path(), c.file, 0o644)
			}
			kept := u.text()
			u.manager.calls, u.manager.show = nil, loadedFrom(u.path(), c.show)
			if c.show == "elsewhere" {
				u.manager.show = loadedFrom("/usr/lib/systemd/user/crw-relay.service", "inactive")
			}
			result, code := u.register(install.ServiceOptions{Remove: true})
			u.expect(result, code, install.Refused, c.outcome)
			for _, verb := range u.manager.verbs() {
				if verb != "show" {
					t.Fatalf("a refusal asked systemd to change something: %v", u.manager.calls)
				}
			}
			if u.text() != kept {
				t.Fatal("a refusal changed the unit file")
			}
		})
	}
	u = newUnitHost(t)
	u.register(install.ServiceOptions{})
	u.manager.calls, u.manager.show = nil, loadedFrom(u.path(), "inactive")
	if result, code = u.register(install.ServiceOptions{Remove: true, DryRun: true}); code != install.OK || at(result, "outcome") != install.UnitWouldRemove || strings.Join(u.manager.verbs(), " ") != "show" {
		t.Fatalf("dry run: %v", u.manager.calls)
	}
	os.MkdirAll(u.path()+".d", 0o755)
	result, code = u.register(install.ServiceOptions{Remove: true})
	u.expect(result, code, install.Refused, install.UnitModified)
}

// A step that fails says what stands. A unit written and not enabled is exit 3, and a rerun finishes the job;
// a disable that fails keeps the file; a reload that fails after the delete is exit 3 too.
func TestStepsThatFailSayWhatStands(t *testing.T) {
	u := newUnitHost(t)
	u.manager.fail = map[string]error{"enable": errors.New("boom")}
	result, code := u.register(install.ServiceOptions{})
	u.expect(result, code, install.Incomplete, install.UnitNotEnabled)
	if at(result, "applied") != true || u.text() == "" {
		t.Fatalf("the unit file should stand: %s", golden.Canon(result))
	}
	u.manager.fail = nil
	result, code = u.register(install.ServiceOptions{})
	u.expect(result, code, install.OK, install.UnitUnchanged)
	if u.manager.verbs()[len(u.manager.verbs())-1] != "enable" {
		t.Fatalf("the rerun did not enable: %v", u.manager.calls)
	}
	u.manager.show, u.manager.fail = loadedFrom(u.path(), "inactive"), map[string]error{"disable": errors.New("boom")}
	result, code = u.register(install.ServiceOptions{Remove: true})
	u.expect(result, code, install.Refused, install.UnitNotDisabled)
	if u.text() == "" {
		t.Fatal("the file was deleted although disable failed")
	}
	u.manager.fail = map[string]error{"daemon-reload": errors.New("boom")}
	result, code = u.register(install.ServiceOptions{Remove: true})
	u.expect(result, code, install.Incomplete, install.UnitNotReloaded)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file should be gone: %v", err)
	}
}

// parseUnit reads the lines a manager would act on: the executable and arguments of start and stop, what
// the unit sets and what it unsets.
func parseUnit(unit string) (start, stop []string, set scope.Env, unset []string) {
	for _, line := range strings.Split(unit, "\n") {
		switch key, value, _ := strings.Cut(line, "="); key {
		case "ExecStart":
			start = strings.Fields(value)
		case "ExecStop":
			stop = strings.Fields(strings.TrimPrefix(value, "-"))
		case "Environment":
			set = append(set, value)
		case "UnsetEnvironment":
			unset = strings.Fields(value)
		}
	}
	return start, stop, set, unset
}

// The unit and the maintenance order do not fight. With the unit registered (a fake manager) and its own
// ExecStart and ExecStop run as the real relay against a fake App Server in an isolated scope: the relay
// starts; an update is refused while it runs, whoever started it; after the stop the update lands and moves
// the pointer; the unit file is byte for byte what it was and the manager was never called; and the unit's
// ExecStart then starts the new runtime.
func TestUpdateAndStopStartDoNotFightTheUnit(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	updated := runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	manager := &fakeManager{show: notLoaded}
	o := h.options()
	o.Systemctl = manager.run
	units := filepath.Join(h.home, "units")
	result, code := install.RegisterService(context.Background(), o, install.ServiceOptions{UnitDir: units, ScopeDir: filepath.Join(h.home, "scopes")})
	if code != install.OK {
		t.Fatalf("register: %s", golden.Canon(result))
	}
	unitPath := filepath.Join(units, doctor.ServiceUnit)
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	unit, calls := string(raw), len(manager.calls)
	start, stop, set, unset := parseUnit(unit)
	env := h.env
	for _, name := range unset {
		env = env.Without(name)
	}
	env = append(env, set...)
	call := func(words ...string) map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, words[0], words[1:]...)
		cmd.Env = env
		out, _ := cmd.Output()
		var parsed map[string]any
		_ = json.Unmarshal(out, &parsed)
		return parsed
	}
	verb := func(v string) map[string]any {
		return call(append(append([]string{}, stop[:len(stop)-2]...), "service", v)...)
	}
	// The relay gives its supervisor 20 s to report ready and a host under heavy load can take longer. A start
	// it abandons leaves a partial store its next start refuses (store_owned_by_other), so the one retry
	// starts from a clean temporary state.
	startUnit := func() map[string]any {
		started := call(start...)
		for attempt := 1; attempt < 3 && started["reason"] == "did_not_report"; attempt++ {
			call(stop...)
			_ = os.RemoveAll(h.relayState)
			_ = os.MkdirAll(h.relayState, 0o700)
			verb("enable")
			started = call(start...)
		}
		return started
	}
	t.Cleanup(func() {
		call(stop...)
		for _, key := range []string{"pid", "workerPid"} {
			if pid, _ := verb("status")[key].(float64); pid > 0 {
				_ = syscall.Kill(int(pid), syscall.SIGKILL)
			}
		}
	})
	if enabled := verb("enable"); enabled["ok"] != true {
		t.Fatalf("service enable: %v", enabled)
	}
	if started := startUnit(); started["ok"] != true || verb("status")["running"] != true {
		t.Fatalf("the unit's ExecStart did not start the relay: %v", started)
	}
	blocked, code := install.Install(context.Background(), o, "update", install.Source{From: second})
	if code != install.Refused || at(blocked, "swapGate", "verdict") != "BLOCKED" || h.pointerTarget(t) == updated {
		t.Fatalf("an update while the relay runs: exit %d\n%s", code, golden.Canon(blocked))
	}
	if call(stop...); verb("status")["running"] != false {
		t.Fatal("the unit's ExecStop did not stop the relay")
	}
	if landed, code := install.Install(context.Background(), o, "update", install.Source{From: second}); code != install.OK || h.pointerTarget(t) != updated {
		t.Fatalf("an update after the stop: exit %d\n%s", code, golden.Canon(landed))
	}
	after, _ := os.ReadFile(unitPath)
	if string(after) != unit || len(manager.calls) != calls {
		t.Fatalf("the update touched the unit or the manager: %d calls, was %d", len(manager.calls), calls)
	}
	if started := startUnit(); started["ok"] != true {
		t.Fatalf("the unit's ExecStart after the update: %v", started)
	}
	pid, _ := verb("status")["pid"].(float64)
	exe, err := os.Readlink("/proc/" + strconv.Itoa(int(pid)) + "/exe")
	if err != nil || !strings.HasPrefix(exe, updated+string(filepath.Separator)) {
		t.Fatalf("the restarted relay runs %q (%v), not out of %s", exe, err, updated)
	}
}

// put writes a file with the given mode, creating its directory.
func put(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	write(t, path, text)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
