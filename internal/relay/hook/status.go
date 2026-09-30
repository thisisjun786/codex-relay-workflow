package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	entryPointName        = "completion_hook.py"
	notRead               = "not_read"
	present               = "PRESENT"
	absent                = "ABSENT"
	unreadable            = "UNREADABLE"
	accessError           = "ACCESS_ERROR"
	registrationRelative  = "registration_names_a_relative_settings_path"
	registrationAmbiguous = "registrations_name_different_settings"
	relativeAdapter       = "registration_names_a_relative_adapter"
	workspaceDependent    = "workspace_dependent_spelling"
	noJournal             = "no_journal"
)

type statusRegistration struct {
	Identity string
	Command  string
	Timeout  any
	Target   string
	Settings string
	Matcher  any
	Native   bool
}

type settingsReading struct {
	Value  Object
	Raw    any
	State  string
	Detail string
	Path   string
}

func cell(value, evidence string, extra ...map[string]any) map[string]any {
	out := map[string]any{"value": value, "evidence": evidence}
	for _, fields := range extra {
		for k, v := range fields {
			out[k] = v
		}
	}
	return out
}

// Status reports registration, configured runtime, and firing evidence as independent cells.
// It writes nothing. The only process execution is the same bounded capability probing Python
// performs for the configured runtime and registered interpreter.
func Status(ctx context.Context, home string, environ map[string]string, event string) map[string]any {
	if event == "" {
		event = "Stop"
	}
	if home == "" && environ != nil {
		home = environ["CODEX_HOME"]
	}
	if home == "" {
		if environ == nil {
			home = codexHome()
		} else {
			home = defaultCodexHome()
		}
	}
	home = expandHome(home)
	hookPath := filepath.Join(home, "hooks.json")
	registration, ours, hookReadable := readRegistrations(hookPath, event)

	carried := []string{}
	for _, r := range ours {
		if r.Settings != "" {
			carried = append(carried, r.Settings)
		}
	}
	relative := []string{}
	absolute := []string{}
	for _, p := range carried {
		expanded := expandHome(p)
		if !filepath.IsAbs(expanded) {
			relative = append(relative, p)
		} else {
			absolute = append(absolute, settle(expanded))
		}
	}
	absolute = uniqueSources(absolute)
	silent := len(ours) - len(carried)
	configPath, err := configurationPath(home, environ, "")
	if err != nil {
		panic(err) // Like Python status, a path-resolution failure is not an absent file.
	}
	configurationSource := "this command's own resolution; no registration named one"
	reading := settingsReading{State: "config_absent", Detail: "no configuration at " + configPath, Path: configPath}
	failed := ""
	if len(absolute)+len(relative)+btoi(silent > 0) > 1 {
		all := append(append([]string{}, absolute...), relative...)
		sort.Strings(all)
		reading.Path = first(all, configPath)
		failed = registrationAmbiguous
		reading.Detail = "registrations name different settings files (" + strings.Join(all, ", ") + ") and every one of them runs, so none of them answers for the others"
		configurationSource = "the registered commands"
	} else if len(relative) > 0 {
		reading.Path = relative[0]
		failed = registrationRelative
		reading.Detail = "the registration names its settings with the relative path " + relative[0] + ", which the hook resolves against each session's workspace; no single file answers for it and none was read"
		configurationSource = "the registered command"
	} else {
		if len(absolute) > 0 {
			reading.Path = absolute[0]
			configurationSource = "the registered command"
		}
		reading = readStatusSettings(ctx, reading.Path, len(absolute) > 0)
		failed = reading.State
		if reading.Value != nil {
			failed = ""
		}
	}

	target, interpreter, startProbes := probeRegistrations(ctx, ours)
	named := namedJournals(ctx, ours, relative, startProbes)
	var settingsCell, relay, offers, marker, adapter, adapterInterpreter, journal map[string]any
	if failed != "" {
		settingsCell = cell(failed, reading.Detail, map[string]any{"configuration": reading.Path, "configurationSource": configurationSource})
		relay = cell(notRead, "no usable configuration names a runtime")
		offers = cell(notRead, "no usable configuration names a runtime")
		marker = cell(notRead, "no usable configuration names a marker root")
		adapter = cell(notRead, "no usable configuration names an adapter")
		adapterInterpreter = cell(notRead, "no usable configuration names an adapter interpreter")
		if raw, ok := reading.Raw.(Object); ok {
			if named := text(get(raw, "adapterEntryPoint")); named != "" && filepath.IsAbs(named) {
				adapter = scriptCell(named, "the recorded adapter entry point")
			}
			if named := text(get(raw, "adapterInterpreter")); named != "" && filepath.IsAbs(named) {
				adapterInterpreter = programCell(ctx, named, "the recorded adapter interpreter", true)
			}
		}
		journal = cell(notRead, "no usable configuration names a journal to read")
	} else {
		cfg := reading.Value
		settingsCell = cell(present, "settings read", map[string]any{"configuration": reading.Path, "configurationSource": configurationSource, "mode": get(cfg, "mode"), "dbPath": get(cfg, "dbPath"), "isolationAssertedBy": get(cfg, "isolationAssertedBy")})
		relay = presenceCell(text(get(cfg, "relayExecutable")), "the configured runtime", false)
		if relay["value"] == present {
			offers = offersGuard(ctx, text(get(cfg, "relayExecutable")), secondsDuration(get(cfg, "timeoutSeconds")))
		} else {
			offers = cell(notRead, "the configured runtime could not be asked: "+fmt.Sprint(relay["evidence"]))
		}
		marker = presenceCell(text(get(cfg, "markerRoot")), "the configured marker root", true)
		if text(get(cfg, "adapterEntryPoint")) != "" {
			adapter = scriptCell(text(get(cfg, "adapterEntryPoint")), "the recorded adapter entry point")
		} else {
			adapter = cell(notRead, "these settings record no adapter entry point, which is the user owner's shape: its registered command line carries the adapter instead")
		}
		if text(get(cfg, "adapterInterpreter")) != "" {
			adapterInterpreter = programCell(ctx, text(get(cfg, "adapterInterpreter")), "the recorded adapter interpreter", true)
		} else {
			adapterInterpreter = cell(notRead, "these settings record no adapter interpreter, which is the user owner's shape: its registered command line carries the interpreter instead")
		}
		journal = journalCell(cfg)
	}
	settingsCell["namedSettings"] = named

	ownerCell := cell(notRead, "the settings were not read, so the owner was not established")
	registrationReadHere := false
	registrationElsewhere := false
	if reading.Value != nil {
		owner := text(get(reading.Value, "owner"))
		if owner == "" {
			owner = "user"
		}
		registrationElsewhere = owner == "plugin"
		registrationReadHere = !registrationElsewhere
		where := any(filepath.Join(home, "hooks.json"))
		if owner == "plugin" {
			where = nil
		}
		ownerCell = cell(owner, "the owner recorded in these settings", map[string]any{"registeredWhere": where, "note": nil})
	}
	if reading.Raw != nil {
		if raw, ok := reading.Raw.(Object); ok {
			stated := text(get(raw, "owner"))
			registrationReadHere = stated == "" || stated == "user"
			registrationElsewhere = stated == "plugin"
		}
	}
	if registrationElsewhere {
		startProbes = append(startProbes, map[string]any{"registration": "the launcher these settings record", "adapter": adapter["value"], "interpreter": adapterInterpreter["value"]})
	}
	unregisteredRecords := 0
	if len(named) == 0 {
		unregisteredRecords = journalRecords(journal)
	}
	absence := decideAbsence(map[string]any{
		"registrationReadable": hookReadable, "adapterRegistrations": len(ours), "registrationReadHere": registrationReadHere, "registrationElsewhere": registrationElsewhere,
		"relativeSettings": len(relative) > 0, "silentRegistrations": silent, "namedJournals": named, "startProbes": startProbes, "unregisteredRecords": unregisteredRecords,
		"settledSettings": map[string]any{"settings": reading.Path, "settingsState": reading.State, "usable": reading.Value != nil, "refusedAs": failed, "detail": reading.Detail, "journalRoot": journal["journalRoot"], "journalPolicy": journal["journalPolicy"], "faultsOnly": journal["journalPolicy"] == "faults_only", "records": journalRecordsValue(journal), "recordsAnswer": journalAnswer(journal)},
	})
	return map[string]any{
		"command": "hook-status", "codexHome": home, "event": event, "registrationOwner": ownerCell, "registration": registration,
		"adapterEntryPoint": adapter, "adapterInterpreter": adapterInterpreter, "registeredCommandTarget": target, "registeredInterpreter": interpreter,
		"hostTrust":     cell(notRead, "whether the host loads and trusts these identities is recorded in its own state and is not read here"),
		"configuration": settingsCell, "relayExecutable": relay, "guardEvaluateOffered": offers, "markerRoot": marker, "firingJournal": journal, "firingRecordAbsence": absence,
		"budget": budgetCell(reading.Value, ours), "guardRecords": cell(notRead, "the guard's own per-observation records live in the marker and belong to the relay, not to this command"),
		"daemon": cell(notRead, "the Stop path reads the marker and a read-only database and never reaches the daemon, so this command does not infer one from a hook result"),
		"note":   "Registered, offered and observed to have fired are separate claims. A registration says a line is in the hook file; it does not say the host ran it, that the runtime it names can answer, or that any turn was judged. This command writes nothing of its own, but it is not inert: answering whether the runtime offers guard-evaluate means running that runtime with --help, and what that runtime does is outside this command's control.",
	}
}

func readRegistrations(path, event string) (map[string]any, []statusRegistration, bool) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cell("0", "hooks registered for "+event+" in the user hook file", map[string]any{"hookFile": path, "identities": []any{}, "thisAdapter": []any{}}), nil, true
	}
	if err != nil || infoUnreadable(path) {
		return cell(unreadable, "the hook file could not be read", map[string]any{"hookFile": path}), nil, false
	}
	// Decoded as json.loads decodes it, so a string keeps a lone surrogate escape ("\udcff") as
	// the code point Python holds, which fsencode then opens as the byte it stands for.
	value, err := Decode(raw)
	doc, isObject := plainJSON(value).(map[string]any)
	if err != nil || !isObject && value != nil {
		return cell(unreadable, "the hook file could not be read", map[string]any{"hookFile": path}), nil, false
	}
	hooksObj, ok := doc["hooks"].(map[string]any)
	if !ok && doc["hooks"] != nil {
		return cell(unreadable, "the hook file could not be read", map[string]any{"hookFile": path}), nil, false
	}
	groups, _ := hooksObj[event].([]any)
	ids := []any{}
	ours := []statusRegistration{}
	ownOut := []any{}
	for gi, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for hi, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id := fmt.Sprintf("user:%s:%d:%d", event, gi, hi)
			ids = append(ids, id)
			command := text(em["command"])
			words, ok := shellSplit(command)
			if !ok {
				continue
			}
			target, index := "", -1
			for i, w := range words {
				if filepath.Base(w) == entryPointName {
					target = w
					index = i
					break
				}
			}
			settings := ""
			native := false
			if index < 0 {
				target, settings, native = nativeRegistration(command)
				if !native {
					continue
				}
			} else if index+1 < len(words) {
				settings = words[index+1]
			}
			r := statusRegistration{Identity: id, Command: command, Timeout: em["timeout"], Target: target, Settings: settings, Matcher: gm["matcher"], Native: native}
			ours = append(ours, r)
			ownOut = append(ownOut, map[string]any{"identity": id, "command": command, "timeout": em["timeout"], "target": target, "settings": nilIfEmpty(settings), "matcher": gm["matcher"]})
		}
	}
	return cell(strconv.Itoa(len(ids)), "hooks registered for "+event+" in the user hook file", map[string]any{"hookFile": path, "identities": ids, "thisAdapter": ownOut}), ours, true
}

// readStatusSettings reads the settings at path. A path taken from JSON (a registration in the
// hook file) holds a str as Python does, and reaches the system as os.fsencode's bytes, as
// read_configuration opens it; a path from the environment is already the bytes it names. The
// reading names the path as given either way.
func readStatusSettings(ctx context.Context, path string, fromJSON bool) settingsReading {
	out := settingsReading{Path: path}
	system, encoded := path, true
	if fromJSON {
		system, encoded = fsencode(path)
	}
	if !encoded || strings.IndexByte(path, 0) >= 0 {
		out.State = "config_unreadable"
		out.Detail = "the configuration at " + path + " could not be opened"
		return out
	}
	info, err := os.Lstat(system)
	if errors.Is(err, os.ErrNotExist) {
		out.State = "config_absent"
		out.Detail = "no configuration at " + path
		return out
	}
	if err != nil {
		out.State = "config_unreachable"
		out.Detail = "whether anything exists at " + path + " could not be established: " + err.Error()
		return out
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(system)
		if errors.Is(err, os.ErrNotExist) {
			out.State, out.Detail = "config_unreadable", "a symbolic link whose target does not exist"
			return out
		}
		if errors.Is(err, syscall.ELOOP) {
			out.State, out.Detail = "config_unreadable", "a symbolic link that loops"
			return out
		}
		if err != nil {
			out.State, out.Detail = "config_unreachable", "a symbolic link whose target could not be resolved"
			return out
		}
	}
	if !info.Mode().IsRegular() {
		out.State = "config_unreadable"
		out.Detail = "the configuration at " + path + " is not a regular file"
		return out
	}
	raw, err := readRegular(ctx, system, 1<<20)
	if err != nil {
		out.State = "config_unreachable"
		out.Detail = "the configuration at " + path + " could not be read: " + err.Error()
		return out
	}
	raw = []byte(strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\r", "\n"))
	value, err := Decode(raw)
	if err != nil {
		out.State = "config_unreadable"
		out.Detail = "the configuration at " + path + " could not be decoded: " + err.Error()
		return out
	}
	out.Raw = value
	wrong := Complaints(value)
	if len(wrong) > 0 {
		out.State = "config_malformed"
		out.Detail = strings.Join(wrong, "; ")
		return out
	}
	out.Value = object(value)
	out.State = present
	return out
}

// presenceCell answers what is at a path the settings or the hook file name (or PATH's directory
// joined to such a name), which holds a str as Python does and reaches the system as os.fsencode's
// bytes; the cell names the path as given. A str os.fsencode refuses names nothing, as Python's
// lstat refuses it.
func presenceCell(path, what string, directory bool) map[string]any {
	system, encoded := fsencode(path)
	if !encoded {
		return cell(accessError, "whether anything exists at this path could not be established: "+fsencodeRefusal(path), map[string]any{"path": path})
	}
	info, err := os.Lstat(system)
	if errors.Is(err, os.ErrNotExist) {
		return cell(absent, "nothing exists at "+path, map[string]any{"path": path})
	}
	if err != nil {
		return cell(accessError, "whether anything exists at this path could not be established: "+err.Error(), map[string]any{"path": path})
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(system)
		if errors.Is(err, os.ErrNotExist) {
			return cell(unreadable, "a symbolic link whose target does not exist", map[string]any{"path": path})
		}
		if errors.Is(err, syscall.ELOOP) {
			return cell(unreadable, "a symbolic link that loops", map[string]any{"path": path})
		}
		if err != nil {
			return cell(accessError, "a symbolic link whose target could not be resolved: "+err.Error(), map[string]any{"path": path})
		}
	}
	right := info.Mode().IsRegular()
	if directory {
		right = info.IsDir()
	}
	if !right {
		return cell(unreadable, "something is at this path and it is not the "+what+" expected here", map[string]any{"path": path})
	}
	return cell(present, what, map[string]any{"path": path})
}
func scriptCell(path, label string) map[string]any { return presenceCell(path, label, false) }
func offersGuard(ctx context.Context, path string, timeout time.Duration) map[string]any {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	system, _ := fsencode(path) // presenceCell found it, so it encodes
	cmd := exec.CommandContext(c, system, "guard-evaluate", "--help")
	out, err := cmd.CombinedOutput()
	if c.Err() != nil {
		return cell(notRead, "the runtime did not answer --help within "+fmt.Sprint(timeout.Seconds())+"s")
	}
	if err == nil && strings.Contains(string(out), "guard-evaluate") && strings.Contains(string(out), "--marker-root") {
		return cell("guard-evaluate", "the configured runtime describes guard-evaluate when asked for its help")
	}
	code := 0
	var ex *exec.ExitError
	if errors.As(err, &ex) {
		code = ex.ExitCode()
	}
	if err != nil && !errors.As(err, &ex) {
		return cell("not_started", "the runtime could not be run: "+err.Error())
	}
	return cell("guard_rejected_the_call", "the configured runtime does not offer guard-evaluate", map[string]any{"exitCode": code, "stderr": truncate(string(out), 400)})
}

func probeRegistrations(ctx context.Context, ours []statusRegistration) (map[string]any, map[string]any, []map[string]any) {
	if len(ours) == 0 {
		return cell(notRead, "no registration for this adapter was found to check"), cell(notRead, "no registration for this adapter was found to check"), nil
	}
	commands := []any{}
	targets := []map[string]any{}
	interps := []map[string]any{}
	starts := []map[string]any{}
	loose := []any{}
	for _, r := range ours {
		commands = append(commands, r.Command)
		var tp map[string]any
		if r.Native && !strings.ContainsRune(r.Target, os.PathSeparator) {
			resolved, err := exec.LookPath(r.Target)
			if err != nil {
				tp = cell(absent, "PATH lookup for the registered native hook "+r.Target+" failed: "+err.Error(), map[string]any{"path": r.Target})
			} else {
				// shutil.which answers a str, os.fsdecode of the bytes it found, which presenceCell
				// encodes back: the raw lookup result would read bytes that spell a surrogate
				// (ED B3 BF) as the byte it escapes.
				tp = presenceCell(store.FSDecode(resolved), "the registered native hook found by PATH lookup", false)
			}
		} else if !filepath.IsAbs(r.Target) {
			tp = cell(relativeAdapter, "the registration names the adapter with the relative path "+r.Target+", which the hook resolves against each session's workspace; no single file answers for it and none was read", map[string]any{"path": r.Target})
			loose = append(loose, r.Target)
		} else {
			tp = scriptCell(r.Target, "the adapter script")
		}
		words, _ := shellSplit(r.Command)
		first := ""
		if len(words) > 0 {
			first = words[0]
		}
		var ip map[string]any
		if r.Native {
			// Native commands are the adapter, not Python interpreters. Never run
			// them with -c; the executable's presence supplies startability.
			ip = tp
			first = r.Target
		} else {
			ip = interpreterProbe(ctx, first, first != r.Target)
		}
		targets = append(targets, tp)
		interps = append(interps, map[string]any{"registration": r.Identity, "word": first, "resolved": ip["path"], "probe": ip})
		starts = append(starts, map[string]any{"registration": r.Identity, "adapter": tp["value"], "interpreter": ip["value"]})
	}
	chosenTarget := targets[0]
	for _, p := range targets {
		if p["value"] != present {
			chosenTarget = p
			break
		}
	}
	target := map[string]any{}
	for k, v := range chosenTarget {
		target[k] = v
	}
	target["commands"] = commands
	target["probes"] = mapsAsAny(targets)
	if len(loose) > 0 {
		target["relativeTargets"] = loose
	}
	chosen := interps[0]["probe"].(map[string]any)
	for _, p := range interps {
		q := p["probe"].(map[string]any)
		if q["value"] != present {
			chosen = q
			break
		}
	}
	interpreter := cell(fmt.Sprint(chosen["value"]), fmt.Sprint(chosen["evidence"])+"; this is the first word of the registered command, and a wrapper's own target is not followed", map[string]any{"interpreters": resolvedList(interps), "probes": mapsAsAny(interps)})
	return target, interpreter, starts
}
func interpreterProbe(ctx context.Context, named string, ask bool) map[string]any {
	if named == "" {
		return cell(notRead, "no registration named a program to run the adapter")
	}
	resolved := named
	if strings.ContainsRune(named, os.PathSeparator) {
		if !filepath.IsAbs(named) {
			return cell(workspaceDependent, "the registration names its interpreter with the relative path "+named+", which resolves differently in every workspace; it was not probed here", map[string]any{"path": named})
		}
	} else if p, e := exec.LookPath(named); e == nil {
		resolved = store.FSDecode(p) // shutil.which's str, as the native hook's lookup above
	}
	p := presenceCell(resolved, "the registered interpreter", false)
	p["path"] = resolved
	if p["value"] != present {
		return p
	}
	if !ask {
		return cell(notRead, "the registered interpreter is the adapter itself rather than a program registered to run it, so no interpreter question applies to this word and none was asked", map[string]any{"path": resolved})
	}
	return answersPython(ctx, resolved, "the registered interpreter")
}

// answersPython asks the program a registration or the settings name whether it runs a
// supported Python: it runs `<path> -c <version probe>` for at most 5 s and reads only the
// version it prints. It stays after todo 44 (decision 50): the status reading judges whatever
// Stop registrations a host holds, including a user hooks.json entry or Python-era settings no
// Go install wrote, and for such a registration whether it can start at all turns on whether the
// program it names runs Python; only asking the program answers that for a wrapper. Nothing
// else in the runtime executes a Python interpreter, and the contract corpus's hook status
// fixtures freeze this reading.
func answersPython(ctx context.Context, path, label string) map[string]any {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	source := "import sys;sys.stdout.write('crw-status-probe %d.%d' % (sys.version_info[0],sys.version_info[1]))"
	system, _ := fsencode(path) // presenceCell found it, so it encodes
	cmd := exec.CommandContext(c, system, "-c", source)
	out, err := cmd.Output()
	ran := path + " -c <version-and-nonce probe>"
	if c.Err() != nil {
		return cell(notRead, label+" was RUN as "+ran+" and did not answer within 5s, so whether it runs Python was not established", map[string]any{"path": path, "ran": ran})
	}
	said := strings.TrimSpace(string(out))
	parts := strings.Fields(said)
	if len(parts) == 2 && parts[0] == "crw-status-probe" {
		v := strings.Split(parts[1], ".")
		if len(v) == 2 {
			major, _ := strconv.Atoi(v[0])
			minor, _ := strconv.Atoi(v[1])
			if major < 3 || (major == 3 && minor < 10) {
				return cell("below_supported_python", label+" was RUN as "+ran+" and answered Python "+parts[1]+", below the supported 3.10", map[string]any{"path": path, "ran": ran})
			}
			return cell(present, label+" was RUN as "+ran+" and answered as a supported Python interpreter", map[string]any{"path": path, "ran": ran})
		}
	}
	if err != nil {
		return cell(notRead, label+" was RUN as "+ran+" and did not accept the question, which is also what a wrapper around an interpreter does, so whether it runs Python was not established here", map[string]any{"path": path, "ran": ran})
	}
	return cell(notRead, label+" was RUN as "+ran+" and did not answer as a Python interpreter", map[string]any{"path": path, "ran": ran})
}
func programCell(ctx context.Context, path, label string, ask bool) map[string]any {
	if !filepath.IsAbs(path) {
		return cell(workspaceDependent, "these settings name "+label+" with the relative path "+path+", which resolves differently in every workspace; it was not probed here", map[string]any{"path": path})
	}
	p := presenceCell(path, label, false)
	if ask && p["value"] == present {
		return answersPython(ctx, path, label)
	}
	return p
}

func namedJournals(ctx context.Context, ours []statusRegistration, relative []string, starts []map[string]any) []any {
	out := []any{}
	cache := map[string]map[string]any{}
	start := map[string]any{}
	for _, p := range starts {
		start[fmt.Sprint(p["registration"])] = startable(p)
	}
	for _, r := range ours {
		if r.Settings == "" || contains(relative, r.Settings) {
			continue
		}
		path := settle(expandHome(r.Settings))
		rd := readStatusSettings(ctx, path, true)
		detail := rd.Detail
		if rd.State == "config_unreadable" && strings.Contains(detail, "could not be decoded:") {
			detail = "the configuration could not be decoded"
		}
		entry := map[string]any{"registration": r.Identity, "startable": start[r.Identity], "settings": path, "settingsState": readingState(rd.State), "usable": rd.Value != nil, "refusedAs": nilIfEmpty(rd.State), "detail": nilIfEmpty(detail), "journalRoot": nil, "journalPolicy": nil, "faultsOnly": false, "records": nil, "recordsAnswer": "unestablished", "journal": nil}
		if rd.Value == nil {
			if rd.State != "config_unreachable" {
				entry["recordsAnswer"] = "no_records_kept"
			}
			out = append(out, entry)
			continue
		}
		root := journalDirectory(text(get(rd.Value, "journalRoot")))
		key := root
		if rootFS, ok := fsencode(root); ok {
			root = rootFS
		}
		if info, e := os.Stat(root); e == nil {
			key = fmt.Sprintf("%d:%d", statDev(info), statIno(info))
		}
		j := cache[key]
		if j == nil {
			j = journalCell(rd.Value)
			cache[key] = j
		}
		entry["journal"] = j
		entry["journalRoot"] = j["journalRoot"]
		entry["journalPolicy"] = j["journalPolicy"]
		entry["faultsOnly"] = j["journalPolicy"] == "faults_only"
		entry["records"] = jsonNumeric(journalRecordsValue(j))
		entry["recordsAnswer"] = journalAnswer(j)
		out = append(out, entry)
	}
	return out
}
func journalCell(cfg Object) map[string]any {
	root := text(get(cfg, "journalRoot"))
	policy := text(get(cfg, "journalPolicy"))
	if policy == "" {
		policy = "every_invocation"
	}
	if root == "" {
		return cell(noJournal, "no journal is configured, so this hook records nothing about its own invocations", map[string]any{"journalPolicy": policy})
	}
	// Python reads Path(root).expanduser() and names str() of it in every cell, so the root is
	// pathlib's spelling of the expanded root from here on. That str reaches the system as
	// os.fsencode's bytes, and a refusal counts its position in that spelling.
	root = journalDirectory(root)
	rootFS, encoded := fsencode(root)
	if !encoded {
		return cell(accessError, "the journal could not be opened: "+fsencodeRefusal(root), map[string]any{"journalRoot": root, "journalPolicy": policy})
	}
	if strings.IndexByte(root, 0) >= 0 {
		return cell(accessError, "the journal could not be opened: embedded null byte", map[string]any{"journalRoot": root, "journalPolicy": policy})
	}
	info, err := os.Lstat(rootFS)
	if errors.Is(err, os.ErrNotExist) {
		return cell(absent, "the journal directory does not exist, so this hook has recorded no invocation into it", map[string]any{"journalRoot": root, "journalPolicy": policy})
	}
	if err != nil || !info.IsDir() {
		return cell(accessError, "the journal could not be listed", map[string]any{"journalRoot": root, "journalPolicy": policy})
	}
	days := []string{}
	count := 0
	entries, e := os.ReadDir(rootFS)
	if e != nil {
		return cell(accessError, "the journal could not be listed: "+e.Error(), map[string]any{"journalRoot": root, "journalPolicy": policy})
	}
	dayRE := regexp.MustCompile(`^[0-9]{8}$`)
	rowRE := regexp.MustCompile(`^[0-9a-f]{32}\.json$`)
	for _, d := range entries {
		if !d.IsDir() || !dayRE.MatchString(d.Name()) {
			continue
		}
		days = append(days, d.Name())
		rows, e := os.ReadDir(filepath.Join(rootFS, d.Name()))
		if e != nil {
			return cell(accessError, "a journal day could not be listed", map[string]any{"journalRoot": root, "journalPolicy": policy, "days": days})
		}
		for _, r := range rows {
			if r.Type().IsRegular() && rowRE.MatchString(r.Name()) {
				count++
			}
		}
	}
	sort.Strings(days)
	return cell(strconv.Itoa(count), "invocations this hook recorded for itself", map[string]any{"journalRoot": root, "journalPolicy": policy, "days": days})
}

// journalDirectory is str(Path(root).expanduser()): the home in place of a leading ~ (a ~ nothing
// answers is left as written), then pathlib's spelling, which drops "." components, empty
// components and a trailing slash and keeps exactly two leading slashes.
func journalDirectory(root string) string {
	if expanded, err := store.ExpandUser(root); err == nil {
		root = expanded
	}
	return store.PathlibSpelling(root)
}

func journalAnswer(j map[string]any) string {
	if j["value"] == noJournal || j["journalPolicy"] == noJournal {
		return "no_records_kept"
	}
	if j["value"] == absent {
		return "counted"
	}
	if _, e := strconv.Atoi(fmt.Sprint(j["value"])); e == nil {
		return "counted"
	}
	return "unestablished"
}
func journalRecords(j map[string]any) int { n, _ := strconv.Atoi(fmt.Sprint(j["value"])); return n }
func journalRecordsValue(j map[string]any) any {
	if journalAnswer(j) == "counted" {
		return journalRecords(j)
	}
	return nil
}
func budgetCell(cfg Object, ours []statusRegistration) map[string]any {
	var budget any
	if cfg != nil {
		budget = get(cfg, "timeoutSeconds")
	}
	vals := []float64{}
	for _, r := range ours {
		if n, ok := numberStatus(r.Timeout); ok {
			vals = append(vals, n)
		}
	}
	if budget == nil || len(vals) == 0 {
		return cell(notRead, "both numbers are needed and one of them was not read", map[string]any{"guardBudgetSeconds": jsonNumeric(budget), "registeredTimeoutSeconds": nilIfEmptySlice(vals)})
	}
	sort.Float64s(vals)
	b, _ := numberStatus(budget)
	return cell(formatNumber(vals[0]-b), "seconds between this adapter's own budget and the timeout the host registered it with; measured cost per invocation is journalled as elapsedMs", map[string]any{"guardBudgetSeconds": jsonNumeric(budget), "registeredTimeoutSeconds": floatsAsAny(vals)})
}

// shellSplit implements the POSIX quoting subset emitted and accepted by shlex for hook commands.
func shellSplit(s string) ([]string, bool) {
	var out []string
	var b strings.Builder
	quote := byte(0)
	escaped := false
	word := false
	// Byte by byte: every character the split acts on is ASCII, and every other byte is copied as
	// it is, so a word keeps a str's lone surrogate (WTF-8) and any other byte it holds.
	for i := 0; i < len(s); i++ {
		r := s[i]
		if escaped {
			b.WriteByte(r)
			escaped = false
			word = true
			continue
		}
		if quote == 0 {
			switch r {
			case '\\':
				escaped = true
				word = true
			case '\'', '"':
				quote = r
				word = true
			case ' ', '\t', '\n':
				if word {
					out = append(out, b.String())
					b.Reset()
					word = false
				}
			default:
				b.WriteByte(r)
				word = true
			}
		} else if r == quote {
			quote = 0
		} else if r == '\\' && quote == '"' {
			escaped = true
		} else {
			b.WriteByte(r)
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	if word {
		out = append(out, b.String())
	}
	return out, true
}
func uniqueSources(paths []string) []string {
	sort.Strings(paths)
	out := []string{}
	seen := map[string]bool{}
	ids := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		key := p
		// A registration's settings path is a str from the hook file: os.fsencode's bytes.
		if system, encoded := fsencode(p); encoded {
			if info, e := os.Stat(system); e == nil {
				key = fmt.Sprintf("%d:%d", statDev(info), statIno(info))
			}
		}
		if ids[key] {
			continue
		}
		ids[key] = true
		out = append(out, p)
	}
	return out
}
func statDev(i fs.FileInfo) uint64 {
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Dev)
	}
	return 0
}
func statIno(i fs.FileInfo) uint64 {
	if s, ok := i.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}

// settle is completion._settled: os.path.abspath of the expanded path.
func settle(p string) string {
	p = expandHome(p)
	if a, e := abspath(p); e == nil {
		return a
	}
	return p
}

// expandHome is Path(p).expanduser() (store.ExpandUser: an empty HOME is the root, an unset one
// the passwd entry, ~user that user's home); a ~ nothing answers is left as written.
func expandHome(p string) string {
	if expanded, err := store.ExpandUser(p); err == nil {
		return expanded
	}
	return p
}
func secondsDuration(v any) time.Duration {
	n, ok := numberStatus(v)
	if !ok {
		return 5 * time.Second
	}
	return time.Duration(n * float64(time.Second))
}
func numberStatus(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, e := n.Float64()
		return f, e == nil
	}
	return 0, false
}
func infoUnreadable(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().Perm()&0o444 == 0
}
func readingState(state string) string {
	switch state {
	case "config_absent":
		return absent
	case "config_unreadable", "config_malformed":
		return unreadable
	case "config_unreachable":
		return accessError
	default:
		return state
	}
}
func jsonNumeric(v any) any {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return v
	}
}
func mapsAsAny(v []map[string]any) []any {
	out := make([]any, len(v))
	for i, m := range v {
		out[i] = m
	}
	return out
}
func floatsAsAny(v []float64) []any {
	out := make([]any, len(v))
	for i, n := range v {
		out[i] = n
	}
	return out
}
func resolvedList(v []map[string]any) []any {
	out := make([]any, 0, len(v))
	for _, p := range v {
		out = append(out, p["resolved"])
	}
	return out
}
func startable(p map[string]any) any {
	a, i := fmt.Sprint(p["adapter"]), fmt.Sprint(p["interpreter"])
	blocked := map[string]bool{absent: true, unreadable: true, "not_started": true, "below_supported_python": true}
	if blocked[a] || blocked[i] {
		return false
	}
	if a == present && i == present {
		return true
	}
	return nil
}
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func nilIfEmptySlice[T any](s []T) any {
	if len(s) == 0 {
		return nil
	}
	return s
}
func first(s []string, d string) string {
	if len(s) > 0 {
		return s[0]
	}
	return d
}
func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func formatNumber(n float64) string {
	if n == math.Trunc(n) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// AdapterIdentities is the identity of every entry in the user hook file at path that runs this
// adapter for event - the Python completion_hook.py entry point or a native crw hook command -
// and whether the file could be read at all (completion.adapter_entries). An unread file is
// not an empty one: the installer refuses a second owner on it rather than defaulting.
func AdapterIdentities(path, event string) ([]string, bool) {
	_, ours, readable := readRegistrations(path, event)
	identities := make([]string, 0, len(ours))
	for _, registration := range ours {
		identities = append(identities, registration.Identity)
	}
	return identities, readable
}

// plainJSON is a Decode value as encoding/json would have produced it for the hook file's
// readers: objects as maps and every number a float64, with each str kept as Decode holds it.
func plainJSON(value any) any {
	switch v := value.(type) {
	case Object:
		out := make(map[string]any, len(v))
		for _, field := range v {
			out[field.Key] = plainJSON(field.Value)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = plainJSON(item)
		}
		return out
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return value
}
