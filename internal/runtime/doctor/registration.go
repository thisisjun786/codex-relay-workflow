package doctor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Registrations are the places a host is told to start a Go runtime's components, each judged
// as a whole document, the way the program that reads it accepts it, and then against the
// selected runtime:
//
//   - the Stop settings (crw-completion-hook.json), through the Go hook's own acceptance check
//     (hook.ReadSettings: a document it refuses runs no relay); then relayExecutable and, for a
//     plugin owner, the [adapterInterpreter, adapterEntryPoint, <settings>] invocation of the
//     packaged launcher (plugins/crw/wiring/crw_stop_hook.py);
//   - every Stop command of <CODEX_HOME>/hooks.json that runs a Stop adapter, which is what a
//     user-owned registration runs (runtime_install.py hook appends it there);
//   - the plugin-owned bridge record (crw-bridge-mcp.json), through the packaged launcher's
//     record contract (plugins/crw/wiring/crw_bridge_mcp.py): a record it refuses starts no
//     bridge;
//   - config.toml's mcp_servers, read whole as codexconfig.registration_view reads it (a
//     malformed table makes the configuration unreadable), and every table that starts the
//     bridge: the one named codex-thread-bridge, the one a user-owned bridge record names, and
//     any whose command runs the bridge (runtime_install._starts_this_bridge).
//
// Each is the OPS-2.1 registration signal for its component: one that starts something other
// than the selected runtime's entry point under the component's name, or that its consumer
// refuses, is a conflict; one whose answer depends on something this command cannot read (the
// host's own PATH, an expansion, a file it may not open) stops classification. No value a
// consumer requires to be absolute is ever looked up on this command's PATH.
type Registrations map[string]*componentRegistrations

type componentRegistrations struct {
	report    []any
	conflicts []string
	unread    []string
}

func registrationEntry(source string, field, command any) Object {
	return Object{{Key: "source", Value: source}, {Key: "field", Value: field}, {Key: "command", Value: command}}
}

func (c *componentRegistrations) add(entry Object, resolves, agrees any, detail string) {
	c.report = append(c.report, append(append(Object{}, entry...), record.Object{{Key: "resolves", Value: resolves}, {Key: "agrees", Value: agrees}, {Key: "detail", Value: detail}}...))
}

func (c *componentRegistrations) conflict(entry Object, resolves any, detail string) {
	c.conflicts = append(c.conflicts, detail)
	c.add(entry, resolves, false, detail)
}

func (c *componentRegistrations) unreadable(entry Object, resolves any, what, why string) {
	c.unread = append(c.unread, what+" ("+why+")")
	c.add(entry, resolves, nil, why)
}

// crwModes are the component names crw runs for a mode argument (crw relay, crw bridge, crw hook).
var crwModes = map[string]string{"relay": definition.Relay, "bridge": definition.Bridge, "hook": definition.HookScript}

// ReadRegistrations reads every registration of the relay and the bridge and judges it against
// the runtime at target (the pointer's resolved target). env supplies HOME for the expansions a
// hook command's shell makes.
func ReadRegistrations(ctx context.Context, codexHome, target string, env scope.Env) Registrations {
	out := Registrations{definition.Relay: {}, definition.Bridge: {}}
	crw, err := record.Resolve(filepath.Join(target, "bin", "crw"))
	if err != nil {
		for _, c := range out {
			c.unread = append(c.unread, "the selected runtime's binary "+filepath.Join(target, "bin", "crw"))
		}
		return out
	}
	// The expansions a hook command's shell makes that the scan's grammar makes too; PATH is only
	// where the grammar finds what a bare command around a hook is (a shell whose -c program it
	// reads, a script): a hook word itself must be absolute.
	j := judge{crw: crw, x: Expander{Vars: map[string]string{"HOME": env.Get("HOME"), "CODEX_HOME": codexHome}, Path: env.Get("PATH")}, override: env.Get(settingsEnv)}

	relay := out[definition.Relay]
	stop := filepath.Join(codexHome, hook.ConfigName)
	j.stopSettings(ctx, relay, stop)
	j.stopHooks(relay, filepath.Join(codexHome, "hooks.json"), stop)

	bridge := out[definition.Bridge]
	named := j.bridgeRecord(bridge, filepath.Join(codexHome, "crw-bridge-mcp.json"))
	j.codexConfig(bridge, filepath.Join(codexHome, "config.toml"), named)
	return out
}

type judge struct {
	crw      string   // the selected runtime's binary, resolved
	x        Expander // what a hook command's shell expands that the scan's grammar expands too
	override string   // $CRW_COMPLETION_HOOK_CONFIG, which a hook naming no settings reads
}

// executable judges one registered command the host execs directly: path (absolute) is what
// the host executes, command is how the registration spells it. It starts the selected
// component when path resolves to the selected runtime's binary and crw is invoked under the
// component's name (the command's own basename, which crw dispatches on, or crw with that
// component's mode argument). A path that does not exist starts nothing.
func (j judge) executable(into *componentRegistrations, source, field, command, path string, args []string, want string) {
	entry := registrationEntry(source, field, command)
	resolved, err := record.Resolve(path)
	if err == nil {
		// Resolve answers lexically past a missing component, as Path.resolve() does.
		_, err = os.Stat(resolved)
	}
	switch {
	case errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		into.conflict(entry, nil, source+" "+field+" names "+command+", which does not exist, so the host starts nothing")
		return
	case err != nil:
		into.unreadable(entry, nil, source+" "+field+" "+command, "the command could not be resolved: "+err.Error())
		return
	}
	invoked := filepath.Base(command)
	if invoked == "crw" && len(args) > 0 {
		if mode, ok := crwModes[args[0]]; ok {
			invoked = mode
		} else {
			invoked = "crw " + args[0]
		}
	}
	switch {
	case resolved != j.crw:
		into.conflict(entry, resolved, source+" "+field+" names "+command+", which resolves to "+resolved+", not the selected runtime's "+j.crw+", so the host starts something other than the runtime that was promoted")
	case invoked != want:
		into.conflict(entry, resolved, source+" "+field+" starts the selected runtime's binary as "+invoked+" rather than as "+want+", so it does not run "+want)
	default:
		into.add(entry, resolved, true, "starts the selected runtime's "+want)
	}
}

// stopSettings judges the Stop settings as the Go hook reads them (hook.ReadSettings, the check
// every Stop runs before it asks the relay anything): a document it refuses is a conflict naming
// its complaints, because the hook then releases without running any relay; one it cannot read
// is unreadable. An accepted document's relayExecutable is judged, and a plugin owner's adapter
// invocation. The launcher's own gates (owner plugin, configVersion absent or 1, absolute
// adapter paths) are all inside the hook's check.
func (j judge) stopSettings(ctx context.Context, into *componentRegistrations, path string) {
	document, failure, detail := hook.ReadSettings(ctx, path)
	entry := registrationEntry(path, nil, nil)
	switch failure {
	case "":
	case "config_absent":
		return
	case "config_malformed":
		into.conflict(entry, nil, "the Stop hook refuses the Stop settings "+path+" ("+detail+"), so a Stop runs no relay")
		return
	default:
		into.unreadable(entry, nil, "the Stop settings "+path, detail)
		return
	}
	relay, _ := record.Get(document, "relayExecutable").(string)
	j.executable(into, path, "relayExecutable", relay, relay, nil, definition.Relay)
	if record.Get(document, "owner") != "plugin" {
		return
	}
	entryPoint, _ := record.Get(document, "adapterEntryPoint").(string)
	interpreter, _ := record.Get(document, "adapterInterpreter").(string)
	j.executable(into, path, "adapterEntryPoint", entryPoint, entryPoint, nil, definition.HookScript)
	j.interpreter(into, path, interpreter, entryPoint)
}

// systemEnv are the spellings of the system's env program. The launcher runs its interpreter by
// the path the settings give it, so env is recognised by that path, never by a basename: any
// file can be named env, and a multi-call binary (/bin/env -> busybox, or coreutils) dispatches on
// the name it is run under, which is this one.
var systemEnv = []string{"/usr/bin/env", "/bin/env"}

// interpreter judges the Stop settings' adapterInterpreter against the launcher contract of
// decision 18 (as todo 38 corrects it): the packaged launcher (plugins/crw/wiring/crw_stop_hook.py
// and its <CODEX_HOME>/crw-stop-hook.py copy) runs [adapterInterpreter, adapterEntryPoint,
// <settings>]. That reaches the Go hook only when adapterInterpreter is the system env
// (systemEnv, resolving to a native executable regular file), so that the entry point runs as
// crw-completion-hook with the settings path as its one argument, and only when the entry point
// holds no '=', which env reads as a NAME=VALUE assignment before the program (it would then run
// the settings path). Any other interpreter (a Python interpreter, a shell, the crw binary
// itself, another file named env) is a conflict naming the invocation it produces; one that
// cannot be examined is unreadable.
func (j judge) interpreter(into *componentRegistrations, source, interpreter, entryPoint string) {
	invocation := "the packaged launcher runs [" + interpreter + ", " + entryPoint + ", <settings>]"
	entry := registrationEntry(source, "adapterInterpreter", interpreter)
	conflict := func(resolves any, why string) {
		into.conflict(entry, resolves, source+" adapterInterpreter: "+invocation+", and "+why)
	}
	unread := func(resolves any, why string) {
		into.unreadable(entry, resolves, source+" adapterInterpreter "+interpreter, why)
	}
	if !filepath.IsAbs(interpreter) {
		conflict(nil, "the launcher declines an interpreter that is not an absolute path, so it runs nothing")
		return
	}
	resolved, err := record.Resolve(interpreter)
	if err != nil {
		unread(nil, "the interpreter could not be resolved: "+err.Error())
		return
	}
	info, err := os.Stat(resolved)
	switch {
	case errors.Is(err, os.ErrNotExist):
		conflict(resolved, interpreter+" does not exist, so the launcher starts nothing")
		return
	case err != nil:
		unread(resolved, "the interpreter could not be examined: "+store.PythonOSError(err))
		return
	case !info.Mode().IsRegular():
		conflict(resolved, interpreter+" is not a regular file, so the launcher starts nothing")
		return
	}
	if err := unix.Access(resolved, unix.X_OK); errors.Is(err, unix.EACCES) {
		conflict(resolved, interpreter+" is not executable by this user, so the launcher starts nothing")
		return
	} else if err != nil {
		unread(resolved, "whether the interpreter is executable could not be read: "+err.Error())
		return
	}
	e := Classify(interpreter, "", "")
	isSystemEnv := false
	for _, spelling := range systemEnv {
		isSystemEnv = isSystemEnv || store.PathlibSpelling(interpreter) == spelling
	}
	switch {
	case resolved == j.crw:
		conflict(resolved, interpreter+" is the selected crw binary itself, started as "+filepath.Base(interpreter)+", which reads "+entryPoint+" as its first argument instead of running the hook with the settings path")
	case e.Python:
		conflict(resolved, interpreter+" is a Python interpreter ("+e.Detail+"), which runs "+entryPoint+" as a Python program rather than the selected runtime's hook")
	case !isSystemEnv:
		conflict(resolved, interpreter+" (resolving to "+resolved+") is not the system env ("+strings.Join(systemEnv, " or ")+"), so what it does with "+entryPoint+" is not running it as "+definition.HookScript+" with the settings path")
	case !isNative(resolved):
		unread(resolved, "the system env "+interpreter+" resolves to "+resolved+", which is not a native program, so what it runs could not be established")
	case strings.ContainsRune(entryPoint, '='):
		conflict(resolved, "env reads "+entryPoint+" as a NAME=VALUE assignment because it holds '=', and runs the settings path as the program instead")
	default:
		into.add(entry, resolved, true, "env runs "+entryPoint+" as "+definition.HookScript+" with the settings path")
	}
}

// pythonStopAdapter is the checkout's Python Stop adapter (completion.ENTRY_POINT_NAME). A Stop
// command may also run the Go hook (crw-completion-hook, or crw hook), which hookWord judges, or
// the packaged launcher, which runs what the settings name and stands down unless the plugin owns
// them, so it is judged through the settings rather than here.
const pythonStopAdapter = "completion_hook.py"

// stopHooks judges every Stop command of hooks.json that runs a Stop adapter. The host runs
// each of them on every Stop, whoever the settings name as owner: for a user owner (or none,
// which completion.owner_of reads as user) they are the registration, and for a plugin owner
// they run beside the plugin's declared hook.
func (j judge) stopHooks(into *componentRegistrations, path, settings string) {
	read := reading.ReadJSON(path, "hooks.json", nil, jsonObject("hooks.json"))
	switch {
	case read.State == reading.Absent:
		return
	case !read.OK():
		into.unreadable(registrationEntry(path, nil, nil), nil, "hooks.json "+path, read.Detail)
		return
	}
	shape := func(field, why string) {
		into.unreadable(registrationEntry(path, field, nil), nil, "hooks.json "+path+" "+field, why+", so which Stop commands the host runs cannot be read")
	}
	events, present := record.Lookup(read.Value.(Object), "hooks")
	if !present || events == nil {
		return
	}
	byEvent, ok := events.(Object)
	if !ok {
		shape("hooks", "hooks is "+scope.TypeName(events)+", not an object")
		return
	}
	stop, present := record.Lookup(byEvent, "Stop")
	if !present || stop == nil {
		return
	}
	groups, ok := stop.([]any)
	if !ok {
		shape("hooks.Stop", "hooks.Stop is "+scope.TypeName(stop)+", not a list")
		return
	}
	for g, raw := range groups {
		groupField := fmt.Sprintf("hooks.Stop[%d]", g)
		group, ok := raw.(Object)
		if !ok {
			shape(groupField, groupField+" is "+scope.TypeName(raw)+", not an object")
			continue
		}
		entries, present := record.Lookup(group, "hooks")
		if !present || entries == nil {
			continue
		}
		list, ok := entries.([]any)
		if !ok {
			shape(groupField+".hooks", groupField+".hooks is "+scope.TypeName(entries)+", not a list")
			continue
		}
		for h, raw := range list {
			field := fmt.Sprintf("%s.hooks[%d]", groupField, h)
			entry, ok := raw.(Object)
			if !ok {
				shape(field, field+" is "+scope.TypeName(raw)+", not an object")
				continue
			}
			command, present := record.Lookup(entry, "command")
			if !present || command == nil {
				continue
			}
			text, ok := command.(string)
			if !ok {
				shape(field+".command", field+".command is "+scope.TypeName(command)+", not a string")
				continue
			}
			j.stopCommand(into, path, field+".command", text, settings)
		}
	}
}

// stopCommand judges one Stop command line through the retention scan's reader
// (readStopCommand: its allowlisted grammar, with exec, env and sh -c programs followed): each
// command in it that runs a Stop adapter is judged, and a command whose hooks cannot all be told
// (a word or construct outside the grammar, a script that may run the adapter itself) is
// unreadable. The packaged launcher runs what the settings name, so it is judged through them.
func (j judge) stopCommand(into *componentRegistrations, source, field, command, settings string) {
	calls, unknown := readStopCommand(argvJudge{c: Classifier{Expand: j.x}}, command)
	for _, call := range calls {
		switch base := filepath.Base(call.argv[call.at].Written); {
		case launcherEntries[base]:
		case base == pythonStopAdapter:
			into.conflict(registrationEntry(source, field, command), nil, source+" "+field+" runs the checkout's Python Stop adapter "+call.argv[call.at].Written+" (through "+call.argv[0].Written+"), not the selected runtime's "+definition.HookScript)
		default:
			j.hookCall(into, source, field, command, call, settings)
		}
	}
	if unknown != "" {
		into.unreadable(registrationEntry(source, field, command), nil, source+" "+field, unknown+", so whether it starts the relay's hook cannot be told")
	}
}

// hookCall judges one call of the Go hook: its word must be the program the command executes
// (not an argument of another), an absolute path once the grammar's expansions are made, and it
// must read the settings this command judged.
func (j judge) hookCall(into *componentRegistrations, source, field, command string, call stopAdapterCall, settings string) {
	entry := registrationEntry(source, field, command)
	word := call.argv[call.at]
	if call.at > 0 {
		into.conflict(entry, nil, source+" "+field+" hands "+word.Written+" to "+call.argv[0].Written+" as an argument, so the host's Stop does not run it as "+definition.HookScript)
		return
	}
	var args []string
	if call.crwHook {
		args = []string{"hook"}
	}
	switch {
	case word.Missing != "":
		into.unreadable(entry, nil, source+" "+field+" "+word.Written, "it needs "+word.Missing+", an expansion this doctor does not make")
		return
	case !filepath.IsAbs(word.Value) && strings.Contains(word.Value, "/"):
		into.unreadable(entry, nil, source+" "+field+" "+word.Written, "a relative path, which resolves in the session's workspace")
		return
	case !filepath.IsAbs(word.Value):
		into.unreadable(entry, nil, source+" "+field+" "+word.Written, "a bare command the host's shell looks up on the session's PATH, which this doctor does not read")
		return
	}
	j.executable(into, source, field, word.Written, word.Value, args, definition.HookScript)
	named := call.settings
	switch {
	case call.launcher:
		// crw hook --plugin-launch reads the default settings, the ones judged here
	case named.Written == "" && j.override != "":
		if store.PathlibSpelling(j.override) != store.PathlibSpelling(settings) {
			into.unreadable(entry, nil, source+" "+field+" settings", "the hook names no settings, so it reads $"+settingsEnv+" ("+j.override+"), not the Stop settings "+settings+" this doctor judged")
		}
	case named.Written == "":
		// the hook reads its default settings, the ones judged here
	case named.Missing != "":
		into.unreadable(entry, nil, source+" "+field+" settings argument "+named.Written, "it needs "+named.Missing+", an expansion this doctor does not make")
	case !filepath.IsAbs(named.Value):
		into.unreadable(entry, nil, source+" "+field+" settings argument "+named.Written, "a relative path, which resolves in the session's workspace")
	case store.PathlibSpelling(named.Value) != store.PathlibSpelling(settings):
		into.unreadable(entry, nil, source+" "+field+" settings argument "+named.Written, "the hook reads "+named.Value+", not the Stop settings "+settings+" this doctor judged, so the relay it starts is not known")
	}
}

// bridgeRecord judges the bridge record as the packaged launcher reads it, and returns the
// server name a user-owned record gives its configuration entry.
func (j judge) bridgeRecord(into *componentRegistrations, path string) []string {
	read := reading.ReadJSON(path, "the bridge record", nil, jsonObject("the bridge record"))
	switch {
	case read.State == reading.Absent:
		return nil
	case !read.OK():
		into.unreadable(registrationEntry(path, nil, nil), nil, "the bridge record "+path, read.Detail)
		return nil
	}
	document := read.Value.(Object)
	switch record.Get(document, "owner") {
	case "plugin":
		j.pluginBridge(into, path, document)
	case "user":
		if name, ok := record.Get(document, "serverName").(string); ok && name != "" {
			return []string{name}
		}
	}
	return nil
}

// pluginBridge applies crw_bridge_mcp.py's record contract before judging what it execs: a
// record the launcher refuses (it exits 2 before exec) starts no bridge, which is a conflict.
func (j judge) pluginBridge(into *componentRegistrations, path string, document Object) {
	refuse := func(field any, why string) {
		into.conflict(registrationEntry(path, field, nil), nil, "the packaged bridge launcher refuses "+path+": "+why+", so it starts no bridge")
	}
	version := record.Get(document, "recordVersion")
	number, integral := pyInteger(version)
	if !integral || number != 1 && number != 2 {
		refuse("recordVersion", "it is version "+evidence.Repr(version)+", and the launcher reads versions 1 and 2")
		return
	}
	if name := record.Get(document, "serverName"); name != nil && name != definition.Bridge {
		refuse("serverName", "it names the server "+evidence.Repr(name)+", and the package declares '"+definition.Bridge+"'")
		return
	}
	executable, isString := record.Get(document, "bridgeExecutable").(string)
	if !isString || !filepath.IsAbs(executable) {
		refuse("bridgeExecutable", "it must name bridgeExecutable as an absolute path, found "+evidence.Repr(record.Get(document, "bridgeExecutable")))
		return
	}
	var args []string
	if raw := record.Get(document, "args"); raw != nil {
		list, ok := raw.([]any)
		for _, word := range list {
			text, isString := word.(string)
			ok = ok && isString
			args = append(args, text)
		}
		if !ok {
			refuse("args", "it must list args as strings, found "+evidence.Repr(raw))
			return
		}
	}
	_, hasPolicy := record.Lookup(document, "executionPolicy")
	switch {
	case number == 1 && hasPolicy:
		refuse("executionPolicy", "it is version 1 and names an execution policy, which only a version 2 record carries")
		return
	case number == 2 && !j.policy(into, path, record.Get(document, "executionPolicy"), refuse):
		return
	}
	j.executable(into, path, "bridgeExecutable", executable, executable, args, definition.Bridge)
}

// policy is the launcher's policy_environment for a version-2 record, minus the two variables it
// compares against its own environment (Codex's, which this command does not read): the
// reference is exactly {digest, path}, the path absolute with no surrounding whitespace or
// control character, the digest 64 lowercase hex digits, and the file a regular file whose bytes
// hash to it. A policy that does not exist, is not a regular file or changed is refused; one this
// command could not open or read for another reason is unreadable.
func (j judge) policy(into *componentRegistrations, path string, reference any, refuse func(field any, why string)) bool {
	object, ok := reference.(Object)
	_, hasPath := record.Lookup(object, "path")
	_, hasDigest := record.Lookup(object, "digest")
	if !ok || len(object) != 2 || !hasPath || !hasDigest {
		refuse("executionPolicy", "it is version 2 and must name executionPolicy as an object with exactly digest and path")
		return false
	}
	file, _ := record.Get(object, "path").(string)
	if file == "" || file != strings.TrimSpace(file) || strings.IndexFunc(file, func(r rune) bool { return r < 32 || r == 127 }) >= 0 || !filepath.IsAbs(file) {
		refuse("executionPolicy.path", "it must name the execution policy as an absolute path with no surrounding whitespace or control characters, found "+evidence.Repr(record.Get(object, "path")))
		return false
	}
	digest, _ := record.Get(object, "digest").(string)
	if !sha256Hex.MatchString(digest) {
		refuse("executionPolicy.digest", "it must name the execution policy digest as 64 lowercase hexadecimal characters")
		return false
	}
	entry := registrationEntry(path, "executionPolicy.path", file)
	// The launcher opens os.fsencode(path): a surrogate escape U+DC80..U+DCFF is the byte it
	// stands for, and any other surrogate is a path it refuses.
	name, encodable := reading.FSEncode(file)
	if !encodable {
		refuse("executionPolicy.path", "it names an execution policy path this system cannot encode, found "+evidence.Repr(file))
		return false
	}
	handle, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			refuse("executionPolicy.path", "the execution policy "+file+" does not exist")
		} else {
			into.unreadable(entry, nil, "the execution policy "+file+" the bridge record "+path+" names", store.PythonOSError(err))
		}
		return false
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err == nil && !info.Mode().IsRegular() {
		refuse("executionPolicy.path", "the execution policy "+file+" is not a regular file")
		return false
	}
	sum := sha256.New()
	if err == nil {
		_, err = io.Copy(sum, handle)
	}
	if err != nil {
		into.unreadable(entry, nil, "the execution policy "+file+" the bridge record "+path+" names", store.PythonOSError(err))
		return false
	}
	if actual := hex.EncodeToString(sum.Sum(nil)); actual != digest {
		refuse("executionPolicy.digest", "the execution policy at "+file+" now hashes to "+actual+", and the record was written when it hashed to "+digest)
		return false
	}
	return true
}

// pyInteger is the integer a decoded JSON value equals under Python's ==, which is how the
// launchers compare recordVersion and configVersion: true is 1 and 1.0 is 1.
func pyInteger(v any) (int64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n == math.Trunc(n) && math.Abs(n) < 1<<53 {
			return int64(n), true
		}
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

// codexConfig judges config.toml's mcp_servers: the whole table is read as
// codexconfig.registration_view reads it, so a server entry that is not a table, a command that
// is not a string or args that are not a list of strings makes the configuration unreadable;
// then every table that starts the bridge is judged (named is the table a user-owned bridge
// record names).
func (j judge) codexConfig(into *componentRegistrations, path string, named []string) {
	read := reading.ReadText(path, "config.toml")
	switch {
	case read.State == reading.Absent:
		return
	case !read.OK():
		into.unreadable(registrationEntry(path, nil, nil), nil, "the Codex configuration "+path, read.Detail)
		return
	}
	var document map[string]any
	if _, err := toml.Decode(read.Value.(string), &document); err != nil {
		into.unreadable(registrationEntry(path, nil, nil), nil, "the Codex configuration "+path, err.Error())
		return
	}
	raw, present := document["mcp_servers"]
	if !present {
		return
	}
	malformed := func(field, why string) {
		into.unreadable(registrationEntry(path, field, nil), nil, "the Codex configuration "+path, why)
	}
	servers, ok := raw.(map[string]any)
	if !ok {
		malformed("mcp_servers", "mcp_servers is a table of servers, found "+tomlType(raw))
		return
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		table, ok := servers[name].(map[string]any)
		if !ok {
			malformed("mcp_servers."+name, "the registration for "+evidence.Repr(name)+" is a table, found "+tomlType(servers[name]))
			return
		}
		if command, present := table["command"]; present {
			if _, ok := command.(string); !ok {
				malformed("mcp_servers."+name+".command", evidence.Repr(name)+" has a command that is not a string, it is "+tomlType(command))
				return
			}
		}
		if args, present := table["args"]; present {
			if _, ok := tomlStrings(args); !ok {
				malformed("mcp_servers."+name+".args", evidence.Repr(name)+" has args that are not a list of strings, they are "+tomlType(args))
				return
			}
		}
	}
	judged := map[string]bool{definition.Bridge: true}
	for _, name := range named {
		judged[name] = true
	}
	for _, name := range names {
		table := servers[name].(map[string]any)
		command, _ := table["command"].(string)
		args, _ := tomlStrings(table["args"])
		if !judged[name] && !startsTheBridge(command, args) {
			continue
		}
		field := "mcp_servers." + name + ".command"
		if command == "" {
			into.unreadable(registrationEntry(path, field, nil), nil, path+" "+field, "missing or empty, so what the table starts cannot be read")
			continue
		}
		j.mcpCommand(into, path, field, command, args, table["env"])
	}
}

// startsTheBridge is runtime_install._starts_this_bridge widened to what a Go or Python host may
// register under any table name: the bridge's console script as the command or as an argument
// (an interpreter running it), crw bridge, or python -m codex_thread_bridge.
func startsTheBridge(command string, args []string) bool {
	switch base := filepath.Base(command); {
	case base == definition.Bridge:
		return true
	case base == "crw" && len(args) > 0 && args[0] == "bridge":
		return true
	}
	for i, arg := range args {
		if filepath.Base(arg) == definition.Bridge {
			return true
		}
		if arg == "-m" && i+1 < len(args) && (args[i+1] == "codex_thread_bridge" || strings.HasPrefix(args[i+1], "codex_thread_bridge.")) {
			return true
		}
	}
	return false
}

// mcpCommand judges an mcp_servers command, which Codex spawns without a shell. An absolute one
// is executed as written; a bare one is looked up on the PATH the spawned server gets, which is
// the table's env.PATH when it sets one and otherwise Codex's own, which this command does not
// read; a relative one resolves in whatever directory Codex spawns it from.
func (j judge) mcpCommand(into *componentRegistrations, source, field, command string, args []string, env any) {
	entry := registrationEntry(source, field, command)
	if filepath.IsAbs(command) {
		j.executable(into, source, field, command, command, args, definition.Bridge)
		return
	}
	if strings.Contains(command, "/") {
		into.unreadable(entry, nil, source+" "+field+" "+command, "a relative path, which resolves wherever Codex starts the server")
		return
	}
	variables, _ := env.(map[string]any)
	path, set := variables["PATH"].(string)
	if !set {
		into.unreadable(entry, nil, source+" "+field+" "+command, "a bare command Codex looks up on its own PATH, which this doctor does not read, and the table sets no env.PATH")
		return
	}
	found, err := lookPath(command, path)
	switch {
	case errors.Is(err, errRelativePath):
		into.unreadable(entry, nil, source+" "+field+" "+command, "a bare command looked up on the table's env.PATH ("+path+"), whose relative or empty directory comes first, so what it names depends on the directory Codex starts the server in")
		return
	case err != nil:
		into.unreadable(entry, nil, source+" "+field+" "+command, "a bare command no directory on the table's env.PATH ("+path+") holds")
		return
	}
	j.executable(into, source, field, command, found, args, definition.Bridge)
}

// tomlStrings is a decoded TOML array of strings.
func tomlStrings(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, v == nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

// tomlType is type(value).__name__ of what tomllib decodes for a TOML value.
func tomlType(v any) string {
	switch t := v.(type) {
	case string:
		return "str"
	case int64:
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case []any, []map[string]any:
		return "list"
	case map[string]any:
		return "dict"
	case time.Time:
		// The decoder marks a local date or time by its location's name.
		switch t.Location().String() {
		case "date-local":
			return "date"
		case "time-local":
			return "time"
		}
		return "datetime"
	}
	return fmt.Sprintf("%T", v)
}

// of is one component's registrations, or none.
func (r Registrations) of(name string) *componentRegistrations {
	if c, ok := r[name]; ok {
		return c
	}
	return &componentRegistrations{}
}
