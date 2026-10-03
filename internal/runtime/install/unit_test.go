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
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

const (
	policyVar   = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
	scopeVar    = "CODEX_SESSION_RELAY_SCOPE_DIR"
	notLoaded   = "LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nNeedDaemonReload=no\n"
	freshStatus = "{\"enabled\":false,\"launchPolicy\":{\"source\":null},\"scopeAuthority\":\"production\"}"
)

// fakeManager is systemctl --user: it records every call, answers show from a fixed text and can fail a
// verb. Nothing here reaches the real user manager.
type fakeManager struct {
	calls []string
	show  string
	fail  map[string]error
	// Once enable has run, show answers afterEnable when set, else that unitPath is loaded: a name loads when its unit file does.
	afterEnable, unitPath string
}

func (m *fakeManager) run(_ context.Context, args ...string) (string, error) {
	m.calls = append(m.calls, strings.Join(args, " "))
	if err := m.fail[args[0]]; err != nil {
		return "", err
	}
	if args[0] == "show" {
		if strings.Contains(m.verbs(), "enable") && m.afterEnable != "" {
			return m.afterEnable, nil
		}
		if strings.Contains(m.verbs(), "enable") && m.unitPath != "" {
			return loadedFrom(m.unitPath, "inactive"), nil
		}
		return m.show, nil
	}
	return "", nil
}

// verbs is the first word of every call, space separated.
func (m *fakeManager) verbs() string {
	var out []string
	for _, call := range m.calls {
		out = append(out, strings.Fields(call)[0])
	}
	return strings.Join(out, " ")
}

func loadedFrom(path, active string) string {
	return "LoadState=loaded\nActiveState=" + active + "\nFragmentPath=" + path + "\nDropInPaths=\nNeedDaemonReload=no\n"
}

// unitHost is a temporary HOME with a stub relay at the pointer path: a script that records its arguments
// and environment and answers service status with a canned document.
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
	put(t, filepath.Join(u.dest, "bin-stub", "bin", "codex-session-relay"), "#!/bin/sh\necho \"$@\" >> \"$STUB_OUT/argv\"\nenv | sort > \"$STUB_OUT/env\"\ncat \"$STUB_OUT/status.json\"\n", 0o755)
	if err := os.Symlink(filepath.Join(u.dest, "bin-stub"), filepath.Join(u.dest, "current")); err != nil {
		t.Fatal(err)
	}
	u.status(freshStatus)
	u.manager.unitPath = u.path()
	u.o = install.Options{Dest: u.dest, Socket: filepath.Join(home, "app.sock"), State: filepath.Join(home, "state"), Systemctl: u.manager.run,
		Env: scope.Env{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "STUB_OUT=" + u.out, policyVar + "=/elsewhere/policy.json", scopeVar + "=/elsewhere/scopes"}}
	return u
}

func (u *unitHost) status(document string) {
	put(u.t, filepath.Join(u.out, "status.json"), document, 0o644)
}
func (u *unitHost) path() string { return filepath.Join(u.dir, doctor.ServiceUnit) }

func (u *unitHost) text() string {
	raw, err := os.ReadFile(u.path())
	if err != nil {
		u.t.Fatal(err)
	}
	return string(raw)
}

// run registers (or removes) and checks the exit status and outcome.
func (u *unitHost) run(s install.ServiceOptions, code int, outcome string) record.Object {
	u.t.Helper()
	s.UnitDir = u.dir
	result, got := install.RegisterService(context.Background(), u.o, s)
	if got != code || at(result, "outcome") != outcome {
		u.t.Fatalf("exit %d (want %d), outcome %v (want %s)\n%s", got, code, at(result, "outcome"), outcome, golden.Canon(result))
	}
	return result
}

// unchanged fails the test when a refusal asked systemd for anything but a reading, or changed what stood at the
// unit path.
func (u *unitHost) unchanged(before string) {
	u.t.Helper()
	for _, verb := range strings.Fields(u.manager.verbs()) {
		if verb != "show" {
			u.t.Fatalf("a refusal asked systemd to change something: %v", u.manager.calls)
		}
	}
	if got := snapshot(u.path()); got != before {
		u.t.Fatalf("a refusal changed what stood at the unit path:\n%s\nwas:\n%s", got, before)
	}
}

// snapshot is what stands at path: nothing, a link's target, a directory, or a file's bytes.
func snapshot(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return "absent"
	case info.Mode()&os.ModeSymlink != 0:
		target, _ := os.Readlink(path)
		return "link " + target
	case info.IsDir():
		return "dir"
	}
	raw, _ := os.ReadFile(path)
	return "file " + string(raw)
}

func warned(result record.Object, want string) bool {
	for _, w := range golden.List(at(result, "warnings")) {
		if s, _ := w.(string); strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// put writes a file with the given mode, creating its directory.
func put(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	write(t, path, text)
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A fresh registration writes the one unit, asks the manager and enables it by path, never starts anything; its status
// read runs through the pointer's relay without the policy and scope variables though the caller sets both.
func TestRegisterWritesTheUnitAndEnablesIt(t *testing.T) {
	u := newUnitHost(t)
	result := u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	relay, state, socket := filepath.Join(u.dest, "current", "bin", "codex-session-relay"), u.o.State, u.o.Socket
	common := relay + " --state " + state + " --socket " + socket
	want := "# Written by crw install register-service and owned by crw install: change it with that command, not by hand.\n" +
		"[Unit]\nDescription=CRW relay service (starts the relay service once when the user manager starts)\nX-CRW-Owner=crw-install\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nTimeoutStartSec=60\nUnsetEnvironment=" + policyVar + " " + scopeVar + "\n" +
		"ExecStart=" + common + " service start\nExecStop=-" + common + " service stop\n\n[Install]\nWantedBy=default.target\n"
	if info, err := os.Stat(u.path()); err != nil || u.text() != want || info.Mode().Perm() != 0o644 {
		t.Fatalf("unit text:\n%s\nwant:\n%s", u.text(), want)
	}
	if u.manager.verbs() != "show enable show" || u.manager.calls[1] != "enable "+u.path() {
		t.Fatalf("systemctl calls: %v", u.manager.calls)
	}
	env, _ := os.ReadFile(filepath.Join(u.out, "env"))
	argv, _ := os.ReadFile(filepath.Join(u.out, "argv"))
	if strings.Contains(string(env), policyVar) || strings.Contains(string(env), scopeVar) || strings.TrimSpace(string(argv)) != "--socket "+socket+" --state "+state+" service status" {
		t.Fatalf("the status read ran as:\n%s\nenvironment:\n%s", argv, env)
	}
	if at(result, "serviceEnabled") != false || !warned(result, "service intent is not enabled") || !warned(result, "no execution policy is declared") {
		t.Fatalf("a fresh state: %s", golden.Canon(result))
	}
}

// Registering again converges (enable runs again); a dry run writes nothing and only reads.
func TestRegisterIsIdempotentAndDryRunWritesNothing(t *testing.T) {
	u := newUnitHost(t)
	u.run(install.ServiceOptions{DryRun: true}, install.OK, install.UnitWouldCreate)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) || u.manager.verbs() != "show" {
		t.Fatalf("dry run: %v %v", err, u.manager.calls)
	}
	u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	first, _ := os.Stat(u.path())
	u.run(install.ServiceOptions{}, install.OK, install.UnitUnchanged)
	if second, _ := os.Stat(u.path()); !second.ModTime().Equal(first.ModTime()) || u.manager.verbs() != "show show enable show show enable show" {
		t.Fatalf("rerun: %v", u.manager.calls)
	}
}

// Nothing the installer did not write is adopted or overwritten, and a manager that cannot be asked is not one that said no.
func TestRegisterRefusesWhatItDoesNotOwnOrCannotResolve(t *testing.T) {
	other := func(u *unitHost) string { return loadedFrom("/usr/lib/systemd/user/"+doctor.ServiceUnit, "inactive") }
	for _, c := range []struct {
		name, outcome string
		arrange       func(u *unitHost) string // returns what the manager shows
	}{
		{"a file without the owner key", install.UnitForeign, func(u *unitHost) string {
			put(t, u.path(), "[Service]\nExecStart=/bin/true\n", 0o644)
			return notLoaded
		}},
		{"a symbolic link", install.UnitForeign, func(u *unitHost) string {
			os.MkdirAll(u.dir, 0o755)
			os.Symlink("/etc/hostname", u.path())
			return notLoaded
		}},
		{"a directory", install.UnitForeign, func(u *unitHost) string { os.MkdirAll(u.path(), 0o755); return notLoaded }},
		{"a drop-in directory", install.UnitModified, func(u *unitHost) string { os.MkdirAll(u.path()+".d", 0o755); return notLoaded }},
		{"an owned unit that says something else", install.UnitDiffers, func(u *unitHost) string {
			put(t, u.path(), "[Unit]\n"+doctor.UnitOwnerLine+"\n[Service]\nExecStart=/bin/other\n", 0o644)
			return notLoaded
		}},
		{"another unit that runs the relay's start", install.UnitSecondOwner, func(u *unitHost) string {
			put(t, filepath.Join(u.dir, "hand-made.service"), "[Service]\nExecStart=/opt/bin/codex-session-relay --socket /s service start\n", 0o644)
			return notLoaded
		}},
		{"a name loaded from another fragment", install.UnitNameTaken, other},
		{"a masked name", install.UnitNameTaken, func(*unitHost) string { return strings.Replace(notLoaded, "not-found", "masked", 1) }},
		{"a unit with drop-ins", install.UnitModified, func(u *unitHost) string {
			return strings.Replace(loadedFrom(u.path(), "inactive"), "DropInPaths=", "DropInPaths=/x/a.conf", 1)
		}},
		{"a stale definition", install.UnitModified, func(u *unitHost) string {
			return strings.Replace(loadedFrom(u.path(), "inactive"), "NeedDaemonReload=no", "NeedDaemonReload=yes", 1)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitHost(t)
			u.manager.show = c.arrange(u)
			before := snapshot(u.path())
			u.run(install.ServiceOptions{}, install.Refused, c.outcome)
			u.unchanged(before)
		})
	}
	u := newUnitHost(t)
	u.manager.fail = map[string]error{"show": errors.New("Failed to connect to bus")}
	u.run(install.ServiceOptions{}, install.Refused, install.UnitUnreadable)
}

// Inputs the unit cannot carry, or must not live where a runtime's removal would delete them.
func TestRegisterRefusesInputsTheUnitCannotCarry(t *testing.T) {
	for _, c := range []struct{ socket, name string }{{"a b.sock", ""}, {"none", ""}, {"", "../x.txt"}, {"", "-x.service"}} {
		u := newUnitHost(t)
		if c.socket == "none" {
			u.o.Socket = ""
		} else if c.socket != "" {
			u.o.Socket = filepath.Join(u.home, c.socket)
		}
		u.run(install.ServiceOptions{UnitName: c.name}, install.Usage, "usage")
		if _, err := os.Stat(u.dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%+v: something was written", c)
		}
	}
	u := newUnitHost(t)
	os.Remove(filepath.Join(u.dest, "current"))
	u.run(install.ServiceOptions{}, install.Refused, install.UnitUnreadable)
	u = newUnitHost(t)
	inside, linked := filepath.Join(u.dest, "bin-stub", "units"), filepath.Join(u.home, "linked")
	os.MkdirAll(inside, 0o755)
	os.Symlink(inside, linked)
	for _, dir := range []string{inside, linked} {
		if result, code := install.RegisterService(context.Background(), u.o, install.ServiceOptions{UnitDir: dir}); code != install.Refused || at(result, "outcome") != install.UnitDirInRuntime {
			t.Fatalf("%s: %s", dir, golden.Canon(result))
		}
	}
}

// The boot start's inputs come from the relay's own status: an unreadable declaration refuses, a ready service warns
// of nothing, an unreadable status warns, and a relative socket is resolved so the unit names absolute paths only.
func TestRegisterReadsTheBootStartInputs(t *testing.T) {
	u := newUnitHost(t)
	u.status("{\"enabled\":true,\"launchPolicy\":{\"source\":\"unreadable_record\",\"detail\":\"not JSON\"}}")
	u.run(install.ServiceOptions{}, install.Refused, install.UnitUnreadable)
	u.status("{\"enabled\":true,\"launchPolicy\":{\"source\":\"record\"},\"scopeAuthority\":\"production\"}")
	result := u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if len(golden.List(at(result, "warnings"))) != 0 || at(result, "launchPolicySource") != "record" || at(result, "serviceEnabled") != true {
		t.Fatalf("an enabled, declared service: %s", golden.Canon(result))
	}
	u = newUnitHost(t)
	u.status("not json")
	u.o.Env = append(u.o.Env, "XDG_STATE_HOME="+u.home+"/state-home")
	u.o.Socket = "relative.sock"
	result = u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	if !warned(result, "could not be read") || !warned(result, "XDG_STATE_HOME") || strings.Contains(u.text(), " --socket relative.sock") {
		t.Fatalf("an unreadable status: %s\n%s", golden.Canon(result), u.text())
	}
}

// An isolated target carries its scope into the unit and allows it; a production one unsets the scope variable.
func TestRegisterCarriesAnIsolatedScope(t *testing.T) {
	u := newUnitHost(t)
	scopes := filepath.Join(u.home, "scopes")
	u.status("{\"enabled\":true,\"launchPolicy\":{\"source\":\"record\"},\"scopeAuthority\":\"isolated\"}")
	result := u.run(install.ServiceOptions{ScopeDir: scopes}, install.OK, install.UnitCreated)
	text := u.text()
	raw, _ := os.ReadFile(filepath.Join(u.out, "env"))
	env := string(raw)
	if !strings.Contains(text, "Environment="+scopeVar+"="+scopes+"\n") || !strings.Contains(text, "service start --allow-isolated-scope\n") || !strings.Contains(text, "UnsetEnvironment="+policyVar+"\n") ||
		at(result, "scopeAuthority") != "isolated" || !strings.Contains(env, scopeVar+"="+scopes+"\n") || strings.Contains(env, policyVar) {
		t.Fatalf("isolated unit:\n%s\nstatus read environment:\n%s", text, env)
	}
}

// Remove disables then deletes only what it wrote and only when it is not running; every other case changes nothing.
func TestRemoveDisablesAndDeletesOnlyWhatItWrote(t *testing.T) {
	u := newUnitHost(t)
	u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
	u.manager.calls, u.manager.show = nil, loadedFrom(u.path(), "inactive")
	u.run(install.ServiceOptions{Remove: true, DryRun: true}, install.OK, install.UnitWouldRemove)
	u.run(install.ServiceOptions{Remove: true}, install.OK, install.UnitRemoved)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) || u.manager.verbs() != "show show disable daemon-reload" {
		t.Fatalf("remove: %v %v", err, u.manager.calls)
	}
	u.run(install.ServiceOptions{Remove: true}, install.OK, install.UnitAbsent)
	for _, c := range []struct{ name, outcome, file, show string }{
		{"a running unit", install.UnitActive, "", loadedFrom("UNIT", "active")},
		{"a file that is not the installer's", install.UnitForeign, "[Service]\nExecStart=/bin/true\n", loadedFrom("UNIT", "inactive")},
		{"an install section that reaches another unit", install.UnitModified, "[Unit]\n" + doctor.UnitOwnerLine + "\n[Install]\nWantedBy=default.target\nAlso=other.service\n", loadedFrom("UNIT", "inactive")},
		{"another fragment", install.UnitNameTaken, "", loadedFrom("/usr/lib/systemd/user/"+doctor.ServiceUnit, "inactive")},
		{"a second install section that systemd reads too", install.UnitModified, "[Unit]\n" + doctor.UnitOwnerLine + "\n[Install] \nAlso=other.service\n[Install]\nWantedBy=default.target\n", loadedFrom("UNIT", "inactive")},
		{"an Also= joined from continued lines", install.UnitModified, "[Unit]\n" + doctor.UnitOwnerLine + "\n[Install]\nWantedBy=default.target\nAlso\\\n=other.service\n", loadedFrom("UNIT", "inactive")},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUnitHost(t)
			u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
			if c.file != "" {
				put(t, u.path(), c.file, 0o644)
			}
			kept := snapshot(u.path())
			u.manager.calls, u.manager.show = nil, strings.ReplaceAll(c.show, "UNIT", u.path())
			u.run(install.ServiceOptions{Remove: true}, install.Refused, c.outcome)
			u.unchanged(kept)
		})
	}
}

// Removal trusts a file only if it is exactly what the command writes with values it could have written: a NUL is a
// line break to systemd, so a generated-looking file whose scope value carries one is not trusted.
func TestRemoveRefusesAGeneratedLookingFileWhoseValueCarriesNul(t *testing.T) {
	u := newUnitHost(t)
	scopes := filepath.Join(u.home, "scopes")
	u.run(install.ServiceOptions{ScopeDir: scopes}, install.OK, install.UnitCreated)
	line := "Environment=" + scopeVar + "=" + scopes
	put(t, u.path(), strings.Replace(u.text(), line, line+"\x00[Install]\x00Also=other.service\x00[Service]\x00#", 1), 0o644)
	kept := snapshot(u.path())
	u.manager.calls, u.manager.show = nil, loadedFrom(u.path(), "inactive")
	u.run(install.ServiceOptions{Remove: true}, install.Refused, install.UnitModified)
	u.unchanged(kept)
}

// A step that fails says what stands: exit 3 when a change may have landed (written and not enabled, a failed enable or
// disable, deleted and not reloaded).
func TestStepsThatFailSayWhatStands(t *testing.T) {
	u := newUnitHost(t)
	u.manager.fail = map[string]error{"enable": errors.New("boom")}
	if result := u.run(install.ServiceOptions{}, install.Incomplete, install.UnitNotEnabled); at(result, "applied") != true || u.text() == "" {
		t.Fatalf("the unit file should stand: %s", golden.Canon(result))
	}
	u.manager.fail = nil
	u.run(install.ServiceOptions{}, install.OK, install.UnitUnchanged)
	if !strings.HasSuffix(u.manager.verbs(), "enable show") {
		t.Fatalf("the rerun did not enable: %v", u.manager.calls)
	}
	u.manager.show, u.manager.fail = loadedFrom(u.path(), "inactive"), map[string]error{"disable": errors.New("boom")}
	u.run(install.ServiceOptions{Remove: true}, install.Incomplete, install.UnitNotDisabled)
	if u.text() == "" {
		t.Fatal("the file was deleted although disable failed")
	}
	u.manager.fail = map[string]error{"daemon-reload": errors.New("boom")}
	u.run(install.ServiceOptions{Remove: true}, install.Incomplete, install.UnitNotReloaded)
	if _, err := os.Stat(u.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file should be gone: %v", err)
	}
}

// Only a loaded name shows its drop-ins, so the unit is read again after enable and any drop-in is reported.
func TestRegisterReportsDropInsThatOnlyShowOnceTheNameIsLoaded(t *testing.T) {
	for _, c := range []struct{ name, after, outcome string }{
		{"drop-ins", "DropInPaths=/x/crw-.service.d/o.conf", install.UnitModified},
		{"a name the manager still does not load", "LoadState=not-found", install.UnitUnreadable},
	} {
		u := newUnitHost(t)
		u.manager.afterEnable = strings.Replace(loadedFrom(u.path(), "inactive"), "DropInPaths=", c.after, 1)
		if strings.HasPrefix(c.after, "LoadState") {
			u.manager.afterEnable = notLoaded
		}
		result := u.run(install.ServiceOptions{}, install.Incomplete, c.outcome)
		if at(result, "applied") != true || u.manager.verbs() != "show enable show" {
			t.Fatalf("%s after enable: %v\n%s", c.name, u.manager.calls, golden.Canon(result))
		}
	}
}

// A unit file replaced between the decision and the lock is never acted on: not enabled again, not deleted.
func TestNeitherRegisterNorRemoveActsOnAFileThatChangedUnderneath(t *testing.T) {
	for _, remove := range []bool{false, true} {
		u := newUnitHost(t)
		u.run(install.ServiceOptions{}, install.OK, install.UnitCreated)
		u.manager.calls, u.manager.show = nil, loadedFrom(u.path(), "inactive")
		replaced := u.text() + "# replaced\n"
		restore := install.ReplaceBeforeWriteLock(func(string) { put(t, u.path(), replaced, 0o644) })
		u.run(install.ServiceOptions{Remove: remove}, install.Refused, install.UnitUnreadable)
		restore()
		u.unchanged("file " + replaced)
	}
}

// parseUnit reads what a manager would act on: start and stop, and what the unit sets and unsets.
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

// The unit and the maintenance order do not fight. The unit's own ExecStart and ExecStop run as the real relay against a
// fake App Server in an isolated scope: an update is refused while the relay runs, lands after the stop, leaves the
// unit file and the manager untouched, and the unit's ExecStart then starts the new runtime.
func TestUpdateAndStopStartDoNotFightTheUnit(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	updated := runtimeDir(h, "0.9.1", second, t)
	h.mustInstall(t, "install", first)
	units := filepath.Join(h.home, "units")
	manager := &fakeManager{show: notLoaded, unitPath: filepath.Join(units, doctor.ServiceUnit)}
	o := h.options()
	o.Systemctl = manager.run
	if result, code := install.RegisterService(context.Background(), o, install.ServiceOptions{UnitDir: units, ScopeDir: filepath.Join(h.home, "scopes")}); code != install.OK {
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
	// The relay gives its supervisor 20 s to report ready and a host under heavy load can take longer. A start it
	// abandons leaves a partial store its next start refuses (store_owned_by_other), so the one retry starts
	// from a clean temporary state.
	startUnit := func() map[string]any {
		started := call(start...)
		for attempt := 1; attempt < 3 && started["reason"] == "did_not_report" && started["child"] != "still_running"; attempt++ {
			if stopped := call(stop...); stopped["ok"] != true && stopped["reason"] != "not_running" {
				break
			}
			_ = os.RemoveAll(h.relayState)
			_ = os.MkdirAll(h.relayState, 0o700)
			verb("enable")
			started = call(start...)
		}
		return started
	}
	// The relay's own stop checks who owns a process before it signals it; cleanup relies on that and reports what it cannot stop.
	t.Cleanup(func() {
		stopped, status := call(stop...), verb("status")
		if stopped == nil || status == nil || stopped["ok"] != true && stopped["reason"] != "not_running" || status["running"] != false || status["workerPid"] != nil {
			t.Errorf("the relay this test started may still run on %s (stop: %v, status: %v); stop it by hand", h.relayState, stopped, status)
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
	if after, _ := os.ReadFile(unitPath); string(after) != unit || len(manager.calls) != calls {
		t.Fatalf("the update touched the unit or the manager: %d calls, was %d", len(manager.calls), calls)
	}
	if started := startUnit(); started["ok"] != true {
		t.Fatalf("the unit's ExecStart after the update: %v", started)
	}
	pid, _ := verb("status")["pid"].(float64)
	if exe, err := os.Readlink("/proc/" + strconv.Itoa(int(pid)) + "/exe"); err != nil || !strings.HasPrefix(exe, updated+string(filepath.Separator)) {
		t.Fatalf("the restarted relay runs %q (%v), not out of %s", exe, err, updated)
	}
}
