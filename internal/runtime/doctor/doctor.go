// Package doctor is `crw doctor`: the host-level diagnosis (runtime_install.py diagnose,
// ported by property) and `crw doctor retention-scan`, distinct from the relay's own
// `crw relay doctor`. It writes nothing.
//
// It reports which runtime the owned pointer selects (a Go binary or a Python venv) from the
// pointer target, pyvenv.cfg and the host record's selected map, without any interpreter,
// module-location or shebang-interpreter probe; whether the promotion lock is held; the host
// record's four-state reading; each settings record's executables and what they resolve to;
// the OPS-2.2 class of each component of a Go install; residue on the destination; and the
// OPS-6.2 check record. Relay readings are subprocesses of the selected relay executable.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/residue"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Object is a decoded JSON object.
type Object = record.Object

// Options are the inputs of one diagnosis.
type Options struct {
	Env          scope.Env
	CodexHome    string
	RecordPath   string
	Destination  *string
	RelayCommand string
	Socket       string
	State        string
	Temporary    bool
	// Now and CodexVersion are seams; nil uses the clock and `codex --version`.
	Now          func() time.Time
	CodexVersion func(context.Context) *string
}

// DefaultDestination is where the runtime lives (docs/port/decisions.md 11): XDG_DATA_HOME is
// not honoured on this path, as before.
func DefaultDestination(env scope.Env) string {
	home := env.Get("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".local", "share", "crw-runtime")
}

// CodexHome is CODEX_HOME, or ~/.codex.
func CodexHome(env scope.Env) string {
	if named := env.Get("CODEX_HOME"); named != "" {
		return named
	}
	home := env.Get("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".codex")
}

// SettingsFiles are the settings records the doctor reads, and the keys in each that name an
// executable.
var SettingsFiles = []struct {
	Name string
	Keys []string
}{
	{"crw-completion-hook.json", []string{"relayExecutable", "adapterEntryPoint", "adapterInterpreter"}},
	{"crw-bridge-mcp.json", []string{"bridgeExecutable"}},
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

func actingProcess() string {
	self, err := os.Executable()
	if err != nil {
		self = "crw"
	}
	return "crw doctor on " + self
}

// field is check.field.
func field(value, evidenceText string, command any, measuredAt string) Object {
	if measuredAt == "" {
		measuredAt = "unknown"
	}
	return Object{{Key: "value", Value: value}, {Key: "evidence", Value: evidenceText}, {Key: "command", Value: command}, {Key: "actingProcess", Value: actingProcess()}, {Key: "measuredAt", Value: measuredAt}}
}

// CheckFields are the OPS-6.2 record's fields: the six OPS-6.1 fields and settingsPreserved.
// check.py's 'imported' (a location read from an interpreter) has no Go counterpart.
var CheckFields = []string{"installed", "mcpExposed", "connected", "deliveryAccepted", "verificationComplete", "alwaysActive", "settingsPreserved"}

// CheckValues are the four values a result may take.
var CheckValues = []string{"verified", "not_verified", "unknown", "not_applicable"}

// CheckRecord is check.record without 'imported': every field must be stated, each with one of
// CheckValues, and the destination kind is recorded so a temporary proof is never read as a
// claim about a real Codex home.
func CheckRecord(fields map[string]Object, destination string, temporary bool, scopeValue any) (Object, error) {
	kind := "host"
	if temporary {
		kind = "temporary"
	}
	results := Object{}
	var missing []string
	for _, name := range CheckFields {
		result, ok := fields[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		value, _ := record.Get(result, "value").(string)
		known := false
		for _, v := range CheckValues {
			known = known || v == value
		}
		if !known {
			return nil, fmt.Errorf("unsupported result value: %q", value)
		}
		results = append(results, record.Object{{Key: name, Value: result}}...)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("every result must be stated, missing: %s", strings.Join(missing, ", "))
	}
	return Object{
		{Key: "recordVersion", Value: int64(1)},
		{Key: "destination", Value: destination},
		{Key: "destinationKind", Value: kind},
		{Key: "scope", Value: scopeValue},
		{Key: "results", Value: results},
		{Key: "note", Value: "These results never imply one another. installed, mcpExposed, connected, deliveryAccepted, verificationComplete and alwaysActive are the six OPS-6.1 fields; settingsPreserved is a further observation beside them. A result measured against a temporary destination is evidence about that destination only."},
	}, nil
}

// Runtime is what the owned pointer selects.
func Runtime(pointerPath, from string, rec Object) Object {
	out := Object{{Key: "pointer", Value: nullable(pointerPath)}, {Key: "pointerFrom", Value: from}}
	selected, _ := record.Get(rec, "selected").(Object)
	if selected == nil {
		selected = Object{}
	}
	var selectsKind any = "none"
	kinds := map[string]bool{}
	for _, f := range selected {
		location, _ := f.Value.(string)
		if location == "" {
			continue
		}
		kinds[selectionKind(location)] = true
	}
	switch {
	case len(kinds) == 1:
		for k := range kinds {
			selectsKind = k
		}
	case len(kinds) > 1:
		selectsKind = "mixed"
	}
	if pointerPath == "" {
		return append(out, record.Object{{Key: "state", Value: nil}, {Key: "kind", Value: "unknown"}, {Key: "detail", Value: "no pointer path is recorded or derivable"}, {Key: "recordSelects", Value: selected}, {Key: "recordSelectsKind", Value: selectsKind}, {Key: "agrees", Value: nil}}...)
	}
	read := pointer.Read(pointerPath)
	out = append(out, record.Object{{Key: "state", Value: read.State}, {Key: "target", Value: nullable(read.Target)}, {Key: "detail", Value: read.Detail}}...)
	kind := "unknown"
	var agrees any
	outside := []any{}
	switch read.State {
	case pointer.NoPointer:
		kind = "none"
	case pointer.Link:
		target, err := pointer.TargetOf(pointerPath, read.Target)
		if err != nil {
			out = record.Set(out, "detail", "the pointer's target could not be resolved: "+err.Error())
			break
		}
		out = append(out, record.Object{{Key: "targetResolves", Value: target}}...)
		kind = RuntimeKind(target)
		if len(selected) > 0 {
			for _, f := range selected {
				location, _ := f.Value.(string)
				if location == "" {
					continue
				}
				resolved, err := record.Resolve(location)
				if err != nil || !record.Within(resolved, target) {
					outside = append(outside, location)
				}
			}
			agrees = len(outside) == 0
		}
	}
	return append(out, record.Object{
		{Key: "kind", Value: kind},
		{Key: "placementRecorded", Value: record.PlacementRecorded(record.PointerEntryFor(record.Get(rec, "pointer"), pointerPath))},
		{Key: "recordSelects", Value: selected},
		{Key: "recordSelectsKind", Value: selectsKind},
		{Key: "agrees", Value: agrees},
		{Key: "outside", Value: outside},
	}...)
}

// selectionKind is the runtime kind a recorded selection lives in: a Python selection names a
// package directory inside a venv, a Go selection names <runtime>/bin beside crw.
func selectionKind(location string) string {
	for dir := location; ; dir = filepath.Dir(dir) {
		if kind := RuntimeKind(dir); kind != "unknown" {
			return kind
		}
		if parent := filepath.Dir(dir); parent == dir {
			return "unknown"
		}
	}
}

// ReadSettings reads one settings record and classifies every executable it names.
func ReadSettings(codexHome, name string, keys []string, pointerPath string) Object {
	path := filepath.Join(codexHome, name)
	read := reading.ReadJSON(path, name, nil, nil)
	out := Object{{Key: "path", Value: path}, {Key: "state", Value: read.State}}
	if !read.OK() {
		var refusal any
		if !read.Usable() {
			refusal = read.Refusal()
		}
		return append(out, record.Object{{Key: "reading", Value: refusal}, {Key: "executables", Value: Object{}}}...)
	}
	value, _ := read.Value.(Object)
	executables := Object{}
	for _, key := range keys {
		text, ok := record.Get(value, key).(string)
		if !ok || text == "" {
			continue
		}
		executables = append(executables, record.Object{{Key: key, Value: Classify(text, "", pointerPath).Object()}}...)
	}
	return append(out, record.Object{{Key: "reading", Value: nil}, {Key: "executables", Value: executables}}...)
}

// codexVersion is hostrecord's codexCli dimension: `codex --version`, first line.
func codexVersion(ctx context.Context) *string {
	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(run, "codex", "--version").Output()
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return nil
	}
	return &line
}

// classifyGo is the OPS-2.2 class of one component of a Go install reached through the pointer.
func classifyGo(c definition.Component, target string, rec Object, recordUsable bool, pointerRead Object, version *string, host string) Object {
	entry := filepath.Join(target, "bin", c.ConsoleScript)
	resolved, err := record.Resolve(entry)
	out := Object{{Key: "component", Value: c.Name}, {Key: "entryPoint", Value: entry}}
	signals := ownership.Signals{}
	if !recordUsable {
		signals.Unreadable = append(signals.Unreadable, "the host record")
	}
	if err != nil {
		signals.Unreadable = append(signals.Unreadable, "the entry point "+entry)
	}
	location := filepath.Dir(resolved)
	out = append(out, record.Object{{Key: "entryPointResolves", Value: resolved}, {Key: "location", Value: location}}...)
	_, component := record.Component(append(Object{}, rec...), c.Name)
	var install Object
	for _, raw := range record.Get(component, "installs").([]any) {
		candidate, _ := raw.(Object)
		recorded, _ := record.Get(candidate, "location").(string)
		if recorded == "" {
			continue
		}
		if real, err := record.Resolve(recorded); err == nil && real == location {
			install = candidate
		}
	}
	signals.EntryPointRecorded = install != nil
	digest, digestErr := record.FileDigest(resolved)
	var digestValue any
	if digestErr != nil {
		signals.Unreadable = append(signals.Unreadable, "the installed binary's digest")
	} else {
		digestValue = digest
		if install != nil {
			recorded, _ := record.Get(install, "binaryDigest").(string)
			matches := recorded == digest
			signals.DigestMatches = &matches
		}
	}
	wanted := map[string]string{"install": location, "host": host}
	if digestValue != nil {
		wanted["installDigest"] = digest
	}
	if version != nil {
		wanted["codexCli"] = *version
	}
	points := record.PointsFor(rec, c.Name, wanted)
	signals.HasPoint = len(points) > 0
	if agrees := record.Get(pointerRead, "agrees"); agrees == false {
		signals.PointerConflict = "the owned pointer names " + scope.PyStr(record.Get(pointerRead, "target")) + ", which does not contain the runtime this host record selects, so the command a host reaches is not the one that was promoted"
	}
	class, reasons := ownership.Classify(signals)
	return append(out, record.Object{
		{Key: "digest", Value: digestValue},
		{Key: "recordedDigest", Value: record.Get(install, "binaryDigest")},
		{Key: "pointsCovering", Value: int64(len(points))},
		{Key: "class", Value: class},
		{Key: "reasons", Value: strs(reasons)},
		{Key: "notChecked", Value: []any{"the Codex MCP registration (register-mcp, todo 38)", "the skill links (crw install skills, todo 39)", "the App Server dimension (observed by measure, todo 38)"}},
	}...)
}

func strs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Diagnose is `crw doctor --json`.
func Diagnose(ctx context.Context, o Options) Object {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	versionOf := codexVersion
	if o.CodexVersion != nil {
		versionOf = o.CodexVersion
	}
	codexHome := o.CodexHome
	if codexHome == "" {
		codexHome = CodexHome(o.Env)
	}
	recordPath := o.RecordPath
	if recordPath == "" {
		recordPath = record.Path(o.Env.Get)
	}
	host := record.Load(recordPath, definition.Version)
	var rec Object
	if host.Usable() {
		rec, _ = host.Value.(Object)
	}
	var unreadableDestination []string
	destination := ""
	if o.Destination != nil {
		destination = *o.Destination
		if !filepath.IsAbs(destination) {
			if cwd, err := os.Getwd(); err == nil {
				destination = cwd + "/" + destination
			} else {
				unreadableDestination = append(unreadableDestination, "the destination named by --dest could not be settled: "+*o.Destination+": "+err.Error())
				destination = ""
			}
		}
	}
	ownedEntry, _ := record.Get(rec, "pointer").(Object)
	owned, _ := record.Get(ownedEntry, "path").(string)
	from := "record"
	switch {
	case owned != "" && !filepath.IsAbs(owned):
		unreadableDestination = append(unreadableDestination, "the host record's pointer path is not absolute ("+owned+"), so it names no link this command can read and no destination it can survey")
		owned, from = "", "none"
	case owned == "" && destination != "":
		owned, from = pointer.Path(destination), "dest"
	case owned == "" && o.Destination == nil:
		owned, from = pointer.Path(DefaultDestination(o.Env)), "default"
	}
	runtime := Runtime(owned, from, rec)
	selected := record.Get(runtime, "kind")

	promotion := recordPath + record.PromotionLockSuffix
	lockState, lockDetail := record.Probe(promotion)
	var held any
	switch lockState {
	case record.Held:
		held = true
	case record.Free, record.NoFile:
		held = false
	}

	settings := Object{}
	for _, f := range SettingsFiles {
		settings = append(settings, record.Object{{Key: f.Name, Value: ReadSettings(codexHome, f.Name, f.Keys, owned)}}...)
	}

	components := Object{}
	classes := []string{}
	version := versionOf(ctx)
	hostname, _ := os.Hostname()
	target, _ := record.Get(runtime, "targetResolves").(string)
	for _, c := range definition.Components {
		var one Object
		switch {
		case selected == KindGoRuntime && target != "":
			one = classifyGo(c, target, rec, host.Usable(), runtime, version, hostname)
			classes = append(classes, scope.PyStr(record.Get(one, "class")))
		case selected == KindPythonVenv:
			one = Object{{Key: "component", Value: c.Name}, {Key: "class", Value: nil}, {Key: "classifiedBy", Value: "python3 scripts/runtime_install.py diagnose"},
				{Key: "reason", Value: "a Python install is preserved and classified by the Python installer until todo 44; this command classifies Go installs only"}}
		default:
			one = Object{{Key: "component", Value: c.Name}, {Key: "class", Value: nil}, {Key: "reason", Value: "the pointer selects no runtime this command can classify"}}
		}
		components = append(components, record.Object{{Key: c.Name, Value: one}}...)
	}

	residueRoot := (*string)(nil)
	if o.Destination != nil && destination != "" {
		residueRoot = &destination
	} else if o.Destination == nil && owned != "" {
		parent := filepath.Dir(owned)
		residueRoot = &parent
	}
	protection := func(directory string) (bool, *bool) {
		var selects *bool
		if rec != nil {
			yes := false
			if selectedMap, ok := record.Get(rec, "selected").(Object); ok {
				for _, f := range selectedMap {
					if location, ok := f.Value.(string); ok && location != "" && record.Under(location, directory) {
						yes = true
					}
				}
			}
			selects = &yes
		}
		names := pointer.Names(owned, directory)
		protected := selects == nil || *selects || names == nil || *names
		return protected, selects
	}
	var protect residue.Protection
	if residueRoot != nil {
		protect = protection
	}
	survey := residue.Survey(residueRoot, owned, record.Get(rec, "pointer"), protect, unreadableDestination)

	relayExecutable := o.RelayCommand
	if relayExecutable == "" && target != "" {
		candidate := filepath.Join(owned, "bin", definition.Relay)
		if _, err := os.Stat(candidate); err == nil {
			relayExecutable = candidate
		}
	}
	var summary Object
	var readings any
	if relayExecutable == "" {
		summary = Object{{Key: "skipped", Value: "no relay executable was found, so no scope reading was made"}}
	} else {
		scopeReadings := scope.Survey(ctx, relayExecutable, o.Socket, o.State, o.Env)
		readings = scopeReadings
		status := scope.Relay(ctx, []string{"service", "status"}, relayExecutable, o.Socket, o.State, o.Env, false, 0)
		summary = scope.Summarise(scopeReadings, o.Env, Object{
			{Key: "reading", Value: record.Get(status, "payload")},
			{Key: "state", Value: scope.ServiceState(status)},
			{Key: "invocation", Value: Object{{Key: "ok", Value: record.Get(status, "ok")}, {Key: "exitCode", Value: record.Get(status, "exitCode")}, {Key: "command", Value: record.Get(status, "command")}, {Key: "unreadable", Value: record.Get(status, "unreadable")}}},
			{Key: "note", Value: "who owns the service for this scope. A service is never started here, and no parent may stop one another parent is using (OPS-4.1)."},
		})
	}

	measured := stamp(now())
	installed := field("not_verified", "component classes: "+evidence.Dumps(classOf(components), false, false, true)+". Only 'own' is reusable (OPS-2.2).", "crw doctor", measured)
	switch {
	case selected == KindPythonVenv:
		installed = field("unknown", "the pointer selects a Python venv, which this command does not classify; python3 scripts/runtime_install.py diagnose does until todo 44", "crw doctor", "")
	case len(classes) > 0 && allOwn(classes):
		installed = field("verified", "component classes: "+evidence.Dumps(classOf(components), false, false, true)+". Only 'own' is reusable (OPS-2.2).", "crw doctor", measured)
	}
	connect := record.Get(summary, "socketConnect")
	connectedValue := "unknown"
	connectedAt := ""
	if connect != nil {
		connectedValue, connectedAt = "not_verified", measured
		if connect == "ok" {
			connectedValue = "verified"
		}
	}
	fields := map[string]Object{
		"installed":  installed,
		"mcpExposed": field("not_verified", "no tool names were observed: only a live session can list them, and a configuration entry alone never establishes this field.", nil, measured),
		"connected": field(connectedValue, "doctor actorReachability.socketConnect = "+pyRepr(connect)+". A socket file existing on disk does not establish this.",
			evidence.Dumps(record.Get(summary, "scopeCommand"), false, false, true), connectedAt),
		"deliveryAccepted":     field("not_applicable", "no trial was requested. This field requires an attempt that recorded a returned turn id, which means creating work, and this command creates none.", nil, ""),
		"verificationComplete": field("not_applicable", "OPS-6.4 is a property of a verdict at a head, not of an installation. This command observes no verdict and never infers one from a completed turn or a green check.", nil, ""),
		"alwaysActive":         field("not_verified", "no supervised runtime was enabled and no host restart was observed. Installation is not activation; this command enables no daemon.", nil, ""),
		"settingsPreserved":    field("verified", "doctor writes nothing, so every table in config.toml, every hook entry and every settings record is unchanged by it.", nil, measured),
	}

	checks, err := CheckRecord(fields, codexHome, o.Temporary, record.Get(summary, "stateDirectory"))
	if err != nil {
		// Every field above is stated with a known value, so this is a defect in this file.
		panic(err)
	}
	var hostReading any
	if !host.OK() {
		hostReading = host.Refusal()
	}
	var residualPaths any = record.Get(survey, "residualPaths")
	return Object{
		{Key: "command", Value: "doctor"},
		{Key: "definitionVersion", Value: int64(definition.Version)},
		{Key: "codexHome", Value: codexHome},
		{Key: "hostRecord", Value: recordPath},
		{Key: "hostRecordState", Value: host.State},
		{Key: "hostRecordStateMeaning", Value: "ABSENT is a clean host, PRESENT was read, UNREADABLE exists and its shape could not be read, ACCESS_ERROR could not be reached at all. They are four answers and none of them is inferred from another."},
		{Key: "hostRecordReading", Value: hostReading},
		{Key: "selected", Value: selected},
		{Key: "codexCli", Value: codexCli(version)},
		{Key: "runtime", Value: runtime},
		{Key: "promotionLock", Value: Object{{Key: "path", Value: promotion}, {Key: "state", Value: lockState}, {Key: "held", Value: held}, {Key: "detail", Value: lockDetail}}},
		{Key: "settings", Value: settings},
		{Key: "components", Value: components},
		{Key: "residualPaths", Value: residualPaths},
		{Key: "residue", Value: survey},
		{Key: "scope", Value: summary},
		{Key: "scopeReadings", Value: readings},
		{Key: "checks", Value: checks},
		{Key: "doctorBinary", Value: Object{{Key: "path", Value: self()}, {Key: "target", Value: record.Target()}, {Key: "source", Value: record.Source()}}},
	}
}

func self() any {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	return path
}

// codexCli is the codex-cli version observed now beside the one the App Server client is
// pinned against (docs/port/decisions.md 17), so drift is visible.
func codexCli(observed *string) Object {
	pinned := "codex-cli " + bridge.TestedHostVersion
	var value, agrees any
	if observed != nil {
		value, agrees = *observed, *observed == pinned
	}
	return Object{{Key: "observed", Value: value}, {Key: "pinnedAgainst", Value: pinned}, {Key: "agrees", Value: agrees}}
}

func classOf(components Object) Object {
	out := Object{}
	for _, f := range components {
		out = append(out, record.Object{{Key: f.Key, Value: record.Get(f.Value.(Object), "class")}}...)
	}
	return out
}

func allOwn(classes []string) bool {
	for _, c := range classes {
		if !ownership.Reusable(c) {
			return false
		}
	}
	return true
}

func pyRepr(v any) string {
	if s, ok := v.(string); ok {
		return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
	}
	return scope.PyStr(v)
}
