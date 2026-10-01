// Package doctor is `crw doctor`: the host-level diagnosis (runtime_install.py diagnose,
// ported by property), distinct from the relay's own `crw relay doctor`, and the readings of the
// host's registrations and relay records `crw install remove` rests on (RegisteredMatching,
// RecordedDaemons). It writes nothing.
//
// It reports which runtime the owned pointer selects (a Go binary, or unknown) from the pointer
// target and the host record's selected map, without running anything; whether the promotion lock is held; the host
// record's four-state reading; each settings record's executables and what they resolve to;
// the OPS-2.2 class of each component of a Go install; residue on the destination; and the
// OPS-6.2 check record. Relay readings are subprocesses of the selected relay executable.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/exercise"
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
	// Now, CodexVersion and Hostname are seams; nil uses the clock, `codex --version` and
	// os.Hostname.
	Now          func() time.Time
	CodexVersion func(context.Context) *string
	Hostname     func() (string, error)
	// AppServer observes the App Server identity a measured point is compared on, through the
	// selected runtime's bridge (bridge is its entry point, <runtime>/bin/codex-thread-bridge).
	// nil is the observation itself: a read-only MCP session with that bridge
	// (exercise.Session, the one crw install exercises a candidate with), whose get_capabilities
	// answer is the dimension, as runtime_install.py observed it through check_connection. A
	// bridge that answers nothing leaves the dimension unread, which stops Go classification
	// exactly as runtime_install.classify_component stops on an unobserved App Server.
	//
	// The skill links are deliberately not a signal here, unlike in classify_component: an
	// installed Go product takes its skills from the plugin payload the Codex marketplace
	// installs, and skill links are a developer-checkout concern (crw-dev skills link, todo 39)
	// that no release archive or installer makes, so they say nothing about this install.
	AppServer func(ctx context.Context, bridge string) *string
}

// environ is env as os.environ.get reads it: the last spelling of a key wins, and a key set to
// the empty string is set.
func environ(env scope.Env) record.Environ {
	return func(key string) (string, bool) {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v, true
			}
		}
		return "", false
	}
}

// DefaultDestinationOf is where the runtime lives (docs/port/decisions.md 11), under
// Path.home(): HOME, or this user's passwd entry when HOME is not set. XDG_DATA_HOME is not
// honoured on this path, as before. A home nothing establishes is an error, never a relative
// path read against the working directory.
func DefaultDestinationOf(env scope.Env) (string, error) {
	home, err := record.Home(environ(env))
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "crw-runtime"), nil
}

// DefaultDestination is DefaultDestinationOf for a caller that cannot report the failure: the
// spelling is left unexpanded.
func DefaultDestination(env scope.Env) string {
	if destination, err := DefaultDestinationOf(env); err == nil {
		return destination
	}
	return filepath.Join("~", ".local", "share", "crw-runtime")
}

// CodexHomeOf is CODEX_HOME as given (runtime_install.py never expands it), or Path.home()/.codex.
func CodexHomeOf(env scope.Env) (string, error) {
	if named := env.Get("CODEX_HOME"); named != "" {
		return named, nil
	}
	home, err := record.Home(environ(env))
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

// CodexHome is CodexHomeOf for a caller that cannot report the failure: the spelling is left
// unexpanded.
func CodexHome(env scope.Env) string {
	if home, err := CodexHomeOf(env); err == nil {
		return home
	}
	return filepath.Join("~", ".codex")
}

// SettingsFiles are the settings records the doctor reads, and the keys in each that name an
// executable.
var SettingsFiles = []struct {
	Name string
	Keys []string
}{
	{"crw-completion-hook.json", []string{"relayExecutable"}},
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

// Runtime is what the owned pointer selects. Every component the definition requires must be
// selected by an absolute path; a selection that is missing, not a string, empty or relative is
// listed in unusableSelections and leaves agreement unestablished (agrees null), because a
// pointer cannot be said to contain a runtime the record does not name.
func Runtime(pointerPath, from string, rec Object) Object {
	out := Object{{Key: "pointer", Value: nullable(pointerPath)}, {Key: "pointerFrom", Value: from}}
	raw, present := record.Lookup(rec, "selected")
	selected, isObject := raw.(Object)
	if selected == nil {
		selected = Object{}
	}
	unusable := []any{}
	if present && !isObject {
		unusable = append(unusable, "selected is "+reading.JSONKind(raw)+", not an object")
	}
	for _, c := range definition.Components {
		value, ok := record.Lookup(selected, c.Name)
		location, isString := value.(string)
		switch {
		case !ok:
			unusable = append(unusable, c.Name+": no selection is recorded")
		case !isString:
			unusable = append(unusable, c.Name+": the selection is "+reading.JSONKind(value)+", not a path")
		case location == "":
			unusable = append(unusable, c.Name+": the selection is empty")
		case !filepath.IsAbs(location):
			unusable = append(unusable, c.Name+": the selection "+location+" is not an absolute path")
		}
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
		return append(out, record.Object{{Key: "state", Value: nil}, {Key: "kind", Value: "unknown"}, {Key: "detail", Value: "no pointer path is recorded or derivable"}, {Key: "recordSelects", Value: selected}, {Key: "recordSelectsKind", Value: selectsKind}, {Key: "unusableSelections", Value: unusable}, {Key: "agrees", Value: nil}}...)
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
		if rec == nil {
			break
		}
		for _, f := range selected {
			location, _ := f.Value.(string)
			if !filepath.IsAbs(location) {
				continue // unusable, listed above: never resolved against this process's directory
			}
			resolved, err := record.Resolve(location)
			if err != nil || !record.Within(resolved, target) {
				outside = append(outside, location)
			}
		}
		switch {
		case len(outside) > 0:
			agrees = false
		case len(unusable) == 0:
			agrees = true
		}
	}
	return append(out, record.Object{
		{Key: "kind", Value: kind},
		{Key: "placementRecorded", Value: record.PlacementRecorded(pointerEntryAbout(record.Get(rec, "pointer"), pointerPath))},
		{Key: "recordSelects", Value: selected},
		{Key: "recordSelectsKind", Value: selectsKind},
		{Key: "unusableSelections", Value: unusable},
		{Key: "agrees", Value: agrees},
		{Key: "outside", Value: outside},
	}...)
}

// pointerEntryAbout is record.PointerEntryFor with both paths read in their lexical form:
// "/d/current/" and "/d/current" are one link.
func pointerEntryAbout(entry any, path string) Object {
	o, ok := entry.(Object)
	recorded, _ := record.Get(o, "path").(string)
	if ok && recorded != "" && reading.Spelling(recorded) == reading.Spelling(path) {
		return o
	}
	return nil
}

// selectionKind is the runtime kind a recorded selection lives in: a Go selection names
// <runtime>/bin beside crw.
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
	read := reading.ReadJSON(path, name, nil, jsonObject(name))
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

// jsonObject is the shape of a settings record: valid JSON that is not an object is a record
// that cannot be read, never one that names no executables.
func jsonObject(what string) func(any) error {
	return func(value any) error {
		if _, ok := value.(Object); !ok {
			return reading.Fail("ValueError", what+" is "+reading.JSONKind(value)+", not a JSON object")
		}
		return nil
	}
}

// commandWaitDelay bounds how long a subprocess's output may be held open after it has exited
// or been killed, as scope.WaitDelay bounds the relay readings: a descendant that inherited the
// pipe would otherwise hold the reading (and the diagnosis) until it exits.
var commandWaitDelay = 5 * time.Second

// CodexVersion is hostrecord's codexCli dimension: `codex --version`, first line. A run that
// fails, times out, or whose output a descendant still holds open once it exits
// (exec.ErrWaitDelay) is an unread dimension (nil), never a version read from a partial output.
// crw install records a point's codexCli with it.
func CodexVersion(ctx context.Context) *string {
	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, "codex", "--version")
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.Output()
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
func classifyGo(c definition.Component, target string, rec Object, recordUsable bool, pointerRead Object, seen observations, registrations Registrations) Object {
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
	// The install entry at this location, the last one the record lists there.
	var install Object
	recordedAs := any(nil)
	installs, _ := record.Get(component, "installs").([]any)
	for _, raw := range installs {
		candidate, _ := raw.(Object)
		recorded, _ := record.Get(candidate, "location").(string)
		if recorded == "" {
			continue
		}
		if real, err := record.Resolve(recorded); err != nil || real != location {
			continue
		}
		install, recordedAs = candidate, "go"
	}
	signals.EntryPointRecorded = install != nil
	problems, unread := launchProblems(target)
	signals.Unlaunchable = problems
	signals.Unreadable = append(signals.Unreadable, unread...)
	var launchable any
	switch {
	case len(problems) > 0:
		launchable = false
	case len(unread) == 0:
		launchable = true
	}
	digest, digestErr := record.FileDigest(resolved)
	var digestValue any
	if digestErr != nil {
		signals.Unreadable = append(signals.Unreadable, "the installed binary's digest")
	} else {
		digestValue = digest
		if install != nil {
			// The comparison is made only against a recorded SHA-256. A binaryDigest that is
			// missing, not a string or not 64 lowercase hex digits records nothing these bytes
			// can agree with, so classification stops rather than reaching own without it.
			recorded, isString := record.Get(install, "binaryDigest").(string)
			if isString && sha256Hex.MatchString(recorded) {
				matches := recorded == digest
				signals.DigestMatches = &matches
			} else {
				signals.Unreadable = append(signals.Unreadable, "the recorded binaryDigest of the Go install entry at "+location+" (found "+reading.Show(record.Get(install, "binaryDigest"))+")")
			}
		}
	}
	// A dimension that could not be read is not a dimension that agrees, and it is never
	// dropped from the comparison: a point cannot be said to cover a reading nobody took, so
	// each one stops classification (runtime_install.classify_component's judged.answer) and no
	// point is looked up.
	wanted := map[string]string{"install": location}
	if digestValue != nil {
		wanted["installDigest"] = digest
	}
	dimensionsRead := true
	unreadDimension := func(what string) {
		signals.Unreadable = append(signals.Unreadable, what)
		dimensionsRead = false
	}
	if seen.version != nil {
		wanted["codexCli"] = *seen.version
	} else {
		unreadDimension("the Codex CLI version")
	}
	if seen.host != "" {
		wanted["host"] = seen.host
	} else {
		unreadDimension("this host's name" + seen.hostProblem)
	}
	if seen.appServer != nil {
		wanted["appServer"] = *seen.appServer
	} else {
		unreadDimension("the App Server identity (" + seen.appServerProblem + ")")
	}
	var points []Object
	if dimensionsRead {
		points = record.PointsFor(rec, c.Name, wanted)
	}
	signals.HasPoint = len(points) > 0
	// pointer_conflict_of: a pointer the selection lies outside is a conflict, and one whose
	// agreement could not be established (a selection missing, invalid or unresolvable) stops
	// classification.
	switch agrees := record.Get(pointerRead, "agrees"); {
	case agrees == false:
		signals.PointerConflict = "the owned pointer names " + reading.Text(record.Get(pointerRead, "target")) + ", which does not contain the runtime this host record selects, so the command a host reaches is not the one that was promoted"
	case agrees == nil && recordUsable:
		signals.Unreadable = append(signals.Unreadable, "whether the owned pointer "+reading.Text(record.Get(pointerRead, "target"))+" contains the runtime this host record selects ("+strings.Join(anyStrings(record.Get(pointerRead, "unusableSelections")), "; ")+")")
	}
	registered := registrations.of(c.Name)
	signals.RegistrationConflict = strings.Join(registered.conflicts, "; ")
	signals.Unreadable = append(signals.Unreadable, registered.unread...)
	class, reasons := ownership.Classify(signals)
	return append(out, record.Object{
		{Key: "registrations", Value: nonNil(registered.report)},
		{Key: "digest", Value: digestValue},
		{Key: "recordedDigest", Value: record.Get(install, "binaryDigest")},
		{Key: "recordedInstall", Value: recordedAs},
		{Key: "pointsCovering", Value: int64(len(points))},
		{Key: "launchable", Value: launchable},
		{Key: "class", Value: class},
		{Key: "reasons", Value: strs(reasons)},
		{Key: "notChecked", Value: strs(seen.notChecked)},
	}...)
}

// observations are the point dimensions this host runs under, which every component's
// classification shares. A reading that was not made carries why.
type observations struct {
	version          *string
	host             string
	hostProblem      string
	appServer        *string
	appServerProblem string
	// appServerBridge is the bridge the App Server was asked through, "" when none was asked.
	appServerBridge string
	notChecked      []string
}

// appServerReport is the report's appServer member: whether the selected bridge was asked and
// answered. The comparison itself is classifyGo's, on the same observation.
func (seen observations) appServerReport() Object {
	switch {
	case seen.appServerBridge == "":
		return Object{{Key: "observed", Value: false}, {Key: "bridge", Value: nil}, {Key: "detail", Value: "no Go runtime is selected, so no bridge was asked"}}
	case seen.appServer == nil:
		return Object{{Key: "observed", Value: false}, {Key: "bridge", Value: seen.appServerBridge},
			{Key: "detail", Value: "the selected bridge did not answer get_capabilities, so the App Server dimension is unread and classification stops on it rather than comparing a point against a reading nobody took"}}
	}
	return Object{{Key: "observed", Value: true}, {Key: "bridge", Value: seen.appServerBridge},
		{Key: "detail", Value: "the selected bridge answered get_capabilities, and its answer is the App Server dimension a point is compared on"}}
}

// observe makes the shared readings once; bridge is the selected runtime's bridge entry point,
// or "" when no Go runtime is selected and nothing is classified.
func observe(ctx context.Context, o Options, bridge string) observations {
	var seen observations
	versionOf := CodexVersion
	if o.CodexVersion != nil {
		versionOf = o.CodexVersion
	}
	seen.version = versionOf(ctx)
	hostname := os.Hostname
	if o.Hostname != nil {
		hostname = o.Hostname
	}
	name, err := hostname()
	switch {
	case err != nil:
		seen.hostProblem = " (" + err.Error() + ")"
	case name == "":
		seen.hostProblem = " (the name is empty)"
	default:
		seen.host = name
	}
	if bridge != "" {
		appServerOf := o.AppServer
		if appServerOf == nil {
			appServerOf = func(ctx context.Context, bridge string) *string {
				return exercise.Session(ctx, bridge, o.Socket, o.Env).AppServer()
			}
		}
		seen.appServerBridge = bridge
		seen.appServer = appServerOf(ctx, bridge)
		seen.appServerProblem = "the observation through " + bridge + " answered nothing"
	}
	return seen
}

// sha256Hex is a recorded binaryDigest: a SHA-256, lowercase hex.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// launchProblems is what keeps a host from launching a Go runtime's entry points, and what
// could not be examined: bin/crw must be a regular file the invoking user may execute (access(2)
// X_OK, the permission check exec makes, which root passes only with an execute bit set), and
// each compatibility link (definition.Links) must resolve to it. A digest says nothing about
// either: chmod 0644 leaves the bytes as they were.
func launchProblems(target string) (problems, unread []string) {
	crw := filepath.Join(target, "bin", "crw")
	info, err := os.Stat(crw)
	switch {
	case errors.Is(err, os.ErrNotExist):
		problems = append(problems, "the entry point "+crw+" does not exist")
	case err != nil:
		unread = append(unread, "the entry point "+crw)
	case !info.Mode().IsRegular():
		problems = append(problems, "the entry point "+crw+" is not a regular file")
	default:
		if err := unix.Access(crw, unix.X_OK); errors.Is(err, unix.EACCES) {
			problems = append(problems, "the entry point "+crw+" is not executable by this user ("+info.Mode().Perm().String()+")")
		} else if err != nil {
			unread = append(unread, "whether "+crw+" is executable")
		}
	}
	real, err := record.Resolve(crw)
	if err != nil {
		return problems, append(unread, "the entry point "+crw)
	}
	for _, name := range definition.Links() {
		link := filepath.Join(target, "bin", name)
		resolved, err := record.Resolve(link)
		switch {
		case err != nil:
			unread = append(unread, "the compatibility link "+link)
		case resolved != real:
			problems = append(problems, "the compatibility link "+link+" resolves to "+resolved+", not to "+real)
		}
	}
	return problems, unread
}

// anyStrings is the strings of a decoded list.
func anyStrings(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
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
	codexHome := o.CodexHome
	codexHomeProblem := ""
	if codexHome == "" {
		var err error
		if codexHome, err = CodexHomeOf(o.Env); err != nil {
			codexHomeProblem = "the Codex home could not be established: " + err.Error()
		}
	}
	recordPath := o.RecordPath
	var host reading.Reading
	if recordPath == "" {
		var err error
		if recordPath, err = record.PathOf(environ(o.Env)); err != nil {
			// Python's pathlib raises here; a path read against the working directory instead
			// would answer ABSENT, a clean host, about a record nobody looked for.
			host = reading.Reading{State: reading.AccessError, Exception: "RuntimeError", Detail: "the host record's path could not be established: " + err.Error()}
		}
	}
	if recordPath != "" {
		host = record.Load(recordPath, definition.Version)
	}
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
	case owned != "":
		// A trailing '/' would make lstat follow the link, and the residue survey would take
		// the pointer itself for its directory.
		owned = reading.Spelling(owned)
	case destination != "":
		owned, from = pointer.Path(destination), "dest"
	case o.Destination == nil:
		if defaultDestination, err := DefaultDestinationOf(o.Env); err == nil {
			owned, from = pointer.Path(defaultDestination), "default"
		} else {
			unreadableDestination = append(unreadableDestination, "the default destination could not be established: "+err.Error())
			from = "none"
		}
	}
	runtime := Runtime(owned, from, rec)
	selected := record.Get(runtime, "kind")

	var promotion any
	lockState, lockDetail := record.Unknown, "the host record's path was not established, so its promotion lock was not tested"
	if recordPath != "" {
		promotion = recordPath + record.PromotionLockSuffix
		lockState, lockDetail = record.Probe(recordPath + record.PromotionLockSuffix)
	}
	var held any
	switch lockState {
	case record.Held:
		held = true
	case record.Free, record.NoFile:
		held = false
	}

	settings := Object{}
	for _, f := range SettingsFiles {
		one := Object{{Key: "path", Value: nil}, {Key: "state", Value: reading.AccessError}, {Key: "reading", Value: reading.Reading{State: reading.AccessError, Detail: codexHomeProblem}.Refusal()}, {Key: "executables", Value: Object{}}}
		if codexHomeProblem == "" {
			one = ReadSettings(codexHome, f.Name, f.Keys, owned)
		}
		settings = append(settings, record.Object{{Key: f.Name, Value: one}}...)
	}

	components := Object{}
	classes := []string{}
	target, _ := record.Get(runtime, "targetResolves").(string)
	bridgeEntry := ""
	if selected == KindGoRuntime && target != "" {
		for _, c := range definition.Components {
			if c.Name == definition.Bridge {
				bridgeEntry = filepath.Join(target, "bin", c.ConsoleScript)
			}
		}
	}
	seen := observe(ctx, o, bridgeEntry)
	var registrations Registrations
	switch {
	case selected != KindGoRuntime || target == "":
	case codexHomeProblem != "":
		registrations = Registrations{definition.Relay: {unread: []string{codexHomeProblem}}, definition.Bridge: {unread: []string{codexHomeProblem}}}
	default:
		registrations = ReadRegistrations(ctx, codexHome, target, o.Env)
	}
	for _, c := range definition.Components {
		var one Object
		switch {
		case selected == KindGoRuntime && target != "":
			one = classifyGo(c, target, rec, host.Usable(), runtime, seen, registrations)
			classes = append(classes, reading.Text(record.Get(one, "class")))
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
	unreachable := ""
	if relayExecutable == "" && target != "" {
		// The pointer selects a runtime, so its relay is the one to ask. One that cannot be
		// reached (a broken or inaccessible link) is a failed reading of the selected runtime,
		// never the same answer as no selection at all.
		candidate := filepath.Join(owned, "bin", definition.Relay)
		if _, err := os.Stat(candidate); err == nil {
			relayExecutable = candidate
		} else {
			unreachable = "the selected runtime's relay " + candidate + " could not be reached (" + err.Error() + "), so no scope reading was made"
		}
	}
	var summary Object
	var readings any
	switch {
	case unreachable != "":
		summary = Object{{Key: "unreadable", Value: unreachable}, {Key: "relayExecutable", Value: filepath.Join(owned, "bin", definition.Relay)}}
	case relayExecutable == "":
		summary = Object{{Key: "skipped", Value: "no relay executable was found, so no scope reading was made"}}
	default:
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
	installed := field("not_verified", "component classes: "+pyjson.Dumps(classOf(components), pyjson.Options{})+". Only 'own' is reusable (OPS-2.2).", "crw doctor", measured)
	if len(classes) > 0 && allOwn(classes) {
		installed = field("verified", "component classes: "+pyjson.Dumps(classOf(components), pyjson.Options{})+". Only 'own' is reusable (OPS-2.2).", "crw doctor", measured)
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
		"mcpExposed": field("not_verified", "the doctor's own session with the bridge is not Codex's: only a Codex session can show which tools it exposes, and a configuration entry alone never establishes this field.", nil, measured),
		"connected": field(connectedValue, "doctor actorReachability.socketConnect = "+reading.Show(connect)+". A socket file existing on disk does not establish this.",
			pyjson.Dumps(record.Get(summary, "scopeCommand"), pyjson.Options{}), connectedAt),
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
		{Key: "codexHome", Value: nullable(codexHome)},
		{Key: "hostRecord", Value: nullable(recordPath)},
		{Key: "hostRecordState", Value: host.State},
		{Key: "hostRecordStateMeaning", Value: "ABSENT is a clean host, PRESENT was read, UNREADABLE exists and its shape could not be read, ACCESS_ERROR could not be reached at all. They are four answers and none of them is inferred from another."},
		{Key: "hostRecordReading", Value: hostReading},
		{Key: "selected", Value: selected},
		{Key: "codexCli", Value: codexCli(seen.version)},
		{Key: "appServer", Value: seen.appServerReport()},
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
