package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// The two variables a boot-started relay must not inherit from the user manager: the manager passes
// its whole environment to units, and either would change the policy or the scope the registration
// was made for.
const (
	policyVariable = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
	scopeVariable  = "CODEX_SESSION_RELAY_SCOPE_DIR"
)

// Outcomes of register-service.
const (
	UnitCreated      = "unit_created"
	UnitUnchanged    = "unit_unchanged"
	UnitWouldCreate  = "unit_would_create"
	UnitRemoved      = "unit_removed"
	UnitWouldRemove  = "unit_would_remove"
	UnitAbsent       = "unit_absent"
	UnitForeign      = "unit_foreign"
	UnitDiffers      = "unit_differs"
	UnitModified     = "unit_modified"
	UnitNameTaken    = "unit_name_taken"
	UnitUnreadable   = "unit_unreadable"
	UnitSecondOwner  = "unit_second_owner"
	UnitDirInRuntime = "unit_dir_in_runtime"
	UnitActive       = "unit_active"
	UnitNotEnabled   = "unit_not_enabled"
	UnitNotDisabled  = "unit_not_disabled"
	UnitNotReloaded  = "unit_not_reloaded"
)

// ServiceOptions are crw install register-service's inputs.
type ServiceOptions struct {
	UnitName, UnitDir, ScopeDir string
	Remove, DryRun              bool
}

var (
	unitNamePattern = regexp.MustCompile("^[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}\\.service$")
	// unsafeInUnit is what systemd would split, expand or unescape in an Exec line or Environment value.
	unsafeInUnit = regexp.MustCompile("[\\s\"'\\\\%$;\\x00-\\x1f\\x7f]")
	relayStarts  = regexp.MustCompile("(?m)^\\s*ExecStart=.*(codex-session-relay|crw relay)\\b.*\\bservice (start|run)\\b")
	busyStates   = map[string]bool{"active": true, "activating": true, "deactivating": true, "reloading": true}
)

// systemctl is systemctl --user in this process's environment, which it needs for the user bus.
func (o Options) systemctl(ctx context.Context, args ...string) (string, error) {
	if o.Systemctl != nil {
		return o.Systemctl(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output()
	var failed *exec.ExitError
	if errors.As(err, &failed) {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(failed.Stderr)))
	}
	return string(out), err
}

// unitRun is one register-service run.
type unitRun struct {
	ctx             context.Context
	o               Options
	name, dir, path string
	scopeDir        string
	calls           []any
}

func (u *unitRun) systemctl(args ...string) (string, error) {
	out, err := u.o.systemctl(u.ctx, args...)
	call := Object{field("argv", strs(append([]string{"systemctl", "--user"}, args...))), field("ok", err == nil)}
	if err != nil {
		call = append(call, field("error", err.Error()))
	}
	u.calls = append(u.calls, call)
	return out, err
}

// answer is the result document: the outcome, whether anything landed, and what this run asked systemd.
func (u *unitRun) answer(outcome string, applied bool, extra ...contract.Field) Object {
	base := Object{field("command", "register-service"), field("unit", u.name), field("unitFile", u.path), field("outcome", outcome), field("applied", applied)}
	return append(append(base, extra...), field("systemctl", u.calls))
}

func (u *unitRun) refuse(outcome, detail string, extra ...contract.Field) (Object, int) {
	return u.answer(outcome, false, append([]contract.Field{field("detail", detail)}, extra...)...), Refused
}

// renderUnit is the unit file: the relay's own start, once, when the user manager starts. It names the
// owned pointer and never a runtime directory, so an update or rollback never rewrites it, and it
// carries no restart policy, so after that one start only an operator acts on the relay through it.
func renderUnit(relay, state, socket, scopeDir string) string {
	common := relay + " --state " + state + " --socket " + socket
	start, unset, env := common+" service start", policyVariable+" "+scopeVariable, ""
	if scopeDir != "" {
		start += " --allow-isolated-scope"
		unset, env = policyVariable, "Environment="+scopeVariable+"="+scopeDir+"\n"
	}
	return "# Written by crw install register-service and owned by crw install: change it with that command, not by hand.\n" +
		"[Unit]\nDescription=CRW relay service (starts the relay service once when the user manager starts)\n" + doctor.UnitOwnerLine + "\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nTimeoutStartSec=60\nUnsetEnvironment=" + unset + "\n" + env +
		"ExecStart=" + start + "\nExecStop=-" + common + " service stop\n\n[Install]\nWantedBy=default.target\n"
}

// unitFile is what the unit directory holds at the target path.
type unitFile struct {
	present, owned bool
	text, problem  string
}

func readUnit(path string) unitFile {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return unitFile{}
	case err != nil:
		return unitFile{present: true, problem: err.Error()}
	case !info.Mode().IsRegular():
		return unitFile{present: true, problem: "it is not a regular file (a symbolic link, a directory or something else)"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return unitFile{present: true, problem: err.Error()}
	}
	return unitFile{present: true, text: string(raw), owned: strings.Contains("\n"+string(raw), "\n"+doctor.UnitOwnerLine+"\n")}
}

// installOnly is whether the unit's [Install] section holds nothing but WantedBy=default.target, the one
// activation link this command makes: disable follows Also= into other units.
func installOnly(text string) bool {
	_, install, found := strings.Cut(text, "\n[Install]\n")
	return found && strings.TrimSpace(install) == "WantedBy=default.target"
}

// otherRelayUnits are the other units in dir that run the relay's service start or run: a second owner of
// the surface. A file that cannot be read is an unanswered question and refuses.
func otherRelayUnits(dir, name string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var found []string
	for _, entry := range entries {
		if entry.Name() == name || !strings.HasSuffix(entry.Name(), ".service") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if relayStarts.Match(raw) {
			found = append(found, entry.Name())
		}
	}
	return found, nil
}

// manager is what the user manager says about the unit's name: its readings, and a refusal outcome with
// why, or "". The disk is not enough. systemd resolves a name through its whole search path, and disable
// re-reads the disk and follows Also= in drop-ins, so the name must resolve to this file, carry no drop-in,
// and the manager's definition must be current.
func (u *unitRun) manager() (map[string]string, string, string) {
	out, err := u.systemctl("show", "-p", "LoadState", "-p", "FragmentPath", "-p", "ActiveState", "-p", "DropInPaths", "-p", "NeedDaemonReload", u.name)
	if err != nil {
		return nil, UnitUnreadable, "the user manager could not be asked about " + u.name + " (" + err.Error() + ")"
	}
	view := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			view[key] = value
		}
	}
	switch view["LoadState"] {
	case "not-found":
		return view, "", ""
	case "loaded":
	case "masked":
		return view, UnitNameTaken, u.name + " is masked"
	default:
		return view, UnitUnreadable, fmt.Sprintf("the user manager reports %s as %q", u.name, view["LoadState"])
	}
	loaded, _ := record.Resolve(view["FragmentPath"])
	target, _ := record.Resolve(u.path)
	switch {
	case loaded == "" || loaded != target:
		return view, UnitNameTaken, "the user manager loads " + u.name + " from " + view["FragmentPath"] + ", not from " + u.path
	case view["DropInPaths"] != "" || view["NeedDaemonReload"] != "no":
		return view, UnitModified, u.name + " carries drop-ins (" + view["DropInPaths"] + ") or the manager's definition is stale (NeedDaemonReload=" + view["NeedDaemonReload"] + "); check the drop-ins and run systemctl --user daemon-reload, then rerun"
	}
	return view, "", ""
}

// RegisterService is crw install register-service: write the one systemd user unit that starts the relay
// service when the user manager starts, and enable it (systemctl enable makes the default.target link and
// reloads the manager). It never starts, stops or restarts anything and never touches the service intent, and
// it refuses to adopt or overwrite a unit it did not write. With Remove it disables and deletes the unit it wrote.
func RegisterService(ctx context.Context, o Options, s ServiceOptions) (Object, int) {
	u := &unitRun{ctx: ctx, o: o, name: s.UnitName}
	usage := func(complaint string) (Object, int) {
		return u.answer("usage", false, field("error", complaint), field("note", "nothing was written.")), Usage
	}
	if u.name == "" {
		u.name = doctor.ServiceUnit
	}
	if !unitNamePattern.MatchString(u.name) {
		return usage("--unit-name must be a plain name ending in .service, like " + doctor.ServiceUnit)
	}
	var err error
	if u.dir = s.UnitDir; u.dir == "" {
		u.dir, err = doctor.UnitDir(o.Env)
	}
	if err == nil {
		u.dir, err = absolute(u.dir)
	}
	if err != nil {
		return usage("the unit directory could not be established: " + err.Error())
	}
	u.path = filepath.Join(u.dir, u.name)
	if s.ScopeDir != "" {
		if u.scopeDir, err = absolute(s.ScopeDir); err != nil {
			return usage("--scope-dir: " + err.Error())
		}
	}
	if record.Under(u.dir, o.Dest) {
		return u.refuse(UnitDirInRuntime, "the unit directory "+u.dir+" lies inside the installer's destination "+o.Dest+", where a runtime's removal would delete the unit file with it; use a directory outside it")
	}
	if s.Remove {
		return u.remove(s.DryRun)
	}
	if o.Socket == "" {
		return usage("--socket is required: the unit names the App Server socket the relay serves")
	}
	socket, err := absolute(o.Socket)
	if err != nil {
		return usage("--socket: " + err.Error())
	}
	selection, err := store.ResolveStateDir(o.State, socket)
	if err != nil {
		return usage("the relay state directory could not be resolved (" + err.Error() + "); give --state")
	}
	relay := filepath.Join(o.Dest, "current", "bin", definition.Relay)
	for _, one := range []struct{ what, value string }{{"the relay executable", relay}, {"the state directory", selection.Path}, {"the socket", socket}, {"--scope-dir", u.scopeDir}} {
		if unsafeInUnit.MatchString(one.value) {
			return usage(fmt.Sprintf("%s %q holds whitespace, a quote, a backslash, %%, $, ; or a control character, which systemd would split or expand in the unit", one.what, one.value))
		}
	}
	if info, err := os.Stat(relay); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return u.refuse(UnitUnreadable, "no runtime is installed to start: "+relay+" is not an executable file; run crw install install first")
	}
	wanted := renderUnit(relay, selection.Path, socket, u.scopeDir)
	basis, file := lookAt(u.path), readUnit(u.path)
	outcome := UnitWouldCreate
	switch {
	case file.problem != "":
		return u.refuse(UnitForeign, u.path+" exists and is not a file this command can own: "+file.problem)
	case file.present && !file.owned:
		return u.refuse(UnitForeign, u.path+" exists and is not the installer's unit (it lacks "+doctor.UnitOwnerLine+"); it is left alone")
	case file.present && file.text != wanted:
		return u.refuse(UnitDiffers, u.path+" is the installer's unit and says something else; it is not overwritten. Run register-service --remove, then register again")
	case file.present:
		outcome = UnitUnchanged
	}
	if _, err := os.Lstat(u.path + ".d"); err == nil {
		return u.refuse(UnitModified, u.path+".d exists: a drop-in could change what enable and disable reach")
	}
	others, err := otherRelayUnits(u.dir, u.name)
	switch {
	case err != nil:
		return u.refuse(UnitUnreadable, "the units in "+u.dir+" could not all be read ("+err.Error()+"), so whether another unit already starts the relay was not established")
	case len(others) > 0:
		return u.refuse(UnitSecondOwner, strings.Join(others, ", ")+" already runs the relay's service start or run; a second unit would race it. Remove that unit first, by hand", field("units", strs(others)))
	}
	if _, refused, why := u.manager(); refused != "" {
		return u.refuse(refused, why)
	}
	inputs, warnings, unreadable := u.bootInputs(relay, selection.Path, socket)
	if unreadable != "" {
		return u.refuse(UnitUnreadable, unreadable)
	}
	extra := append(inputs, field("warnings", strs(warnings)), field("socket", socket), field("stateDirectory", selection.Path))
	if s.DryRun {
		return u.answer(outcome, false, append(extra, field("note", "dry run: nothing was written and systemd was only read."))...), OK
	}
	wrote := false
	if outcome == UnitWouldCreate {
		beforeWriteLock(u.path)
		lock, lockErr := record.Lock(ctx, u.path, 0)
		if lockErr != nil {
			return u.refuse(UnitUnreadable, "the unit could not be locked for writing ("+lockErr.Error()+"); nothing was written")
		}
		defer lock.Release()
		if !lookAt(u.path).same(basis) {
			return u.refuse(UnitUnreadable, u.path+" changed after it was read, so nothing was written; rerun to decide against the file as it now stands")
		}
		writeErr := record.AtomicWrite(u.path, []byte(wanted))
		if writeErr == nil {
			writeErr = os.Chmod(u.path, 0o644)
		}
		if writeErr != nil {
			return u.refuse(UnitUnreadable, "the unit could not be written: "+writeErr.Error())
		}
		if readUnit(u.path).text != wanted {
			return u.answer(UnitUnreadable, true, append(extra, field("detail", "the unit was written and could not be read back as written"))...), Incomplete
		}
		outcome, wrote = UnitCreated, true
	}
	if _, err := u.systemctl("enable", u.path); err != nil {
		state := "in place"
		if wrote {
			state = "written"
		}
		extra = append(extra, field("detail", "the unit file is "+state+" and systemctl enable failed ("+err.Error()+"); rerun to enable it"))
		if wrote {
			return u.answer(UnitNotEnabled, true, extra...), Incomplete
		}
		return u.answer(UnitNotEnabled, false, extra...), Refused
	}
	return u.answer(outcome, wrote, append(extra, field("note", "enabled for default.target; nothing was started. The relay starts when the user manager next starts, or when you run systemctl --user restart "+u.name+". Registered, enabled and observed after a host restart are three claims; only the last is evidence for alwaysActive."))...), OK
}

// bootInputs reads what the boot start will find, through the pointer's own relay and in the environment the
// unit will give it (the registering environment minus the two variables, plus the isolated scope when asked):
// the service intent and the launch declaration. It reads only. A disabled intent and an undeclared policy are
// warnings; a launch declaration the relay cannot read refuses, as its start would.
func (u *unitRun) bootInputs(relay, state, socket string) ([]contract.Field, []string, string) {
	env := u.o.Env.Without(policyVariable).Without(scopeVariable)
	if u.scopeDir != "" {
		env = env.With(scopeVariable, u.scopeDir)
	}
	status := scope.Relay(u.ctx, []string{"service", "status"}, relay, socket, state, env, false, 0)
	reading, _ := record.Get(status, "payload").(Object)
	var warnings []string
	enabled, _ := record.Get(reading, "enabled").(bool)
	policy, _ := record.Get(reading, "launchPolicy").(Object)
	source, _ := record.Get(policy, "source").(string)
	switch {
	case source == "unreadable_record":
		return nil, nil, "the launch declaration in the state directory cannot be read (" + fmt.Sprint(record.Get(policy, "detail")) + "), so the relay's start would refuse it"
	case reading == nil:
		warnings = append(warnings, "the relay's service status could not be read, so what the boot start will find was not checked")
	default:
		if !enabled {
			warnings = append(warnings, "the service intent is not enabled: the boot start will refuse service_disabled until service enable is run for "+state)
		}
		if source == "" {
			warnings = append(warnings, "no execution policy is declared for this service, so a boot-started relay withholds role-bound deliveries; a hand start may have taken the policy from the shell's "+policyVariable+", which this unit never has")
		}
	}
	if u.o.Env.Get("XDG_STATE_HOME") != "" {
		warnings = append(warnings, "the registering environment sets XDG_STATE_HOME and the unit does not carry it, so a relay started by the user manager writes its host record where the manager's own environment says, which can differ from where one started from this shell writes it")
	}
	var intent, declared any
	if reading != nil {
		intent = enabled
	}
	if source != "" {
		declared = source
	}
	return []contract.Field{field("serviceEnabled", intent), field("launchPolicySource", declared), field("scopeAuthority", record.Get(reading, "scopeAuthority"))}, warnings, ""
}

// remove is register-service --remove: disable and delete the unit this command wrote, never one it did not.
func (u *unitRun) remove(dryRun bool) (Object, int) {
	file := readUnit(u.path)
	switch {
	case !file.present:
		return u.answer(UnitAbsent, false, field("detail", "there is no unit file at "+u.path+"; nothing of this command's to remove")), OK
	case file.problem != "":
		return u.refuse(UnitForeign, u.path+" is not a file this command can own: "+file.problem)
	case !file.owned:
		return u.refuse(UnitForeign, u.path+" is not the installer's unit (it lacks "+doctor.UnitOwnerLine+"); it is left alone")
	case !installOnly(file.text):
		return u.refuse(UnitModified, u.path+" has an [Install] section other than WantedBy=default.target, so disable could reach other units; it is left alone")
	}
	if _, err := os.Lstat(u.path + ".d"); err == nil {
		return u.refuse(UnitModified, u.path+".d exists: a drop-in could make disable reach another unit")
	}
	view, refused, why := u.manager()
	switch {
	case refused != "":
		return u.refuse(refused, why)
	case busyStates[view["ActiveState"]]:
		return u.refuse(UnitActive, u.name+" is "+view["ActiveState"]+"; this command never stops a service. Run systemctl --user stop "+u.name+" (and the relay's own service stop) first")
	}
	if dryRun {
		return u.answer(UnitWouldRemove, false, field("note", "dry run: nothing was changed and systemd was only read.")), OK
	}
	if view["LoadState"] == "loaded" {
		if _, err := u.systemctl("disable", u.name); err != nil {
			return u.refuse(UnitNotDisabled, "systemctl disable failed ("+err.Error()+"); the unit file is kept")
		}
	}
	if err := os.Remove(u.path); err != nil {
		return u.refuse(UnitNotDisabled, "the unit was disabled but its file could not be deleted: "+err.Error())
	}
	if _, err := u.systemctl("daemon-reload"); err != nil {
		return u.answer(UnitNotReloaded, true, field("detail", "the unit file is deleted and systemctl daemon-reload failed ("+err.Error()+"); run it by hand")), Incomplete
	}
	return u.answer(UnitRemoved, true, field("note", "disabled and deleted. The relay itself was not stopped.")), OK
}
