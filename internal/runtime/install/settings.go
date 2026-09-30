package install

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// SettingsName is the Stop hook's settings record under the Codex home.
const SettingsName = hook.ConfigName

// SettingsOverride is the variable a plugin-owned registration refuses (completion.CONFIG_ENV).
const SettingsOverride = "CRW_COMPLETION_HOOK_CONFIG"

// AdapterInterpreter is what a Go install records as adapterInterpreter. The legacy launchers
// (crw_stop_hook.py, packaged until todo 43, and its <CODEX_HOME>/crw-stop-hook.py copy) run
// [adapterInterpreter, adapterEntryPoint, <settings>], and /usr/bin/env executes the entry
// point with the settings path as its one argument, which is what the Go hook reads as its
// settings. Recording the binary itself would hand the Go hook the binary's own path as its
// settings, and every Stop a cached launcher runs would evaluate nothing.
const AdapterInterpreter = "/usr/bin/env"

// Outcomes of writing the settings record (completion.CONFIG_*).
const (
	ConfigCreated            = "config_created"
	ConfigUnchanged          = "config_unchanged"
	ConfigWouldCreate        = "config_would_create"
	ConfigDiffers            = "config_differs"
	ConfigChangedUnderneath  = "config_changed_underneath"
	ConfigAppliedUnverified  = "config_applied_unverified"
	ConfigWouldNotBeReadable = "config_would_not_be_readable"
	ConfigNotWritten         = "config_not_written"
	ConfigUnreadable         = "config_unreadable"
	ConfigUnreachable        = "config_unreachable"
	// Interrupted is a settings or bridge-record write whose command was interrupted while it
	// waited for the file's lock: nothing was written.
	Interrupted = "interrupted"
)

var configSettled = map[string]bool{ConfigCreated: true, ConfigUnchanged: true, ConfigWouldCreate: true}

// HookSettings are the inputs of the plugin-owned settings document (completion.configuration
// with owner plugin).
type HookSettings struct {
	Destination, Relay, MarkerRoot, Database, Mode, JournalRoot, Issue, Isolation, Socket string
	Timeout                                                                               int64
	CodexHome                                                                             string
}

// settled is completion._settled: absolute, links left alone, "~" expanded.
func settled(path string) (string, error) {
	expanded, err := store.ExpandUser(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}

// Document is the settings a Go install writes for the plugin-owned Stop registration: the
// relay and the adapter named through the owned pointer, so an update moves the pointer and
// the settings keep naming the right runtime.
func (s HookSettings) Document(defaultMarker func() (string, error)) (Object, error) {
	relay := s.Relay
	if relay == "" {
		relay = filepath.Join(s.Destination, "current", "bin", definition.Relay)
	}
	relayPath, err := settled(relay)
	if err != nil {
		return nil, err
	}
	marker := s.MarkerRoot
	if marker == "" {
		if marker, err = defaultMarker(); err != nil {
			return nil, err
		}
	}
	markerPath, err := settled(marker)
	if err != nil {
		return nil, err
	}
	var database any
	if s.Database != "" {
		path, err := settled(s.Database)
		if err != nil {
			return nil, err
		}
		database = path
	}
	journal := s.JournalRoot
	if journal == "" {
		journal = filepath.Join(s.CodexHome, "crw-completion-hook", "journal")
	}
	journalPath, err := settled(journal)
	if err != nil {
		return nil, err
	}
	entry, err := settled(filepath.Join(s.Destination, "current", "bin", definition.HookScript))
	if err != nil {
		return nil, err
	}
	var issue, isolation any
	if s.Issue != "" {
		issue = s.Issue
	}
	if s.Isolation != "" {
		isolation = s.Isolation
	}
	document := Object{
		field("configVersion", int64(1)), field("event", "Stop"), field("relayExecutable", relayPath), field("markerRoot", markerPath),
		field("dbPath", database), field("mode", s.Mode), field("timeoutSeconds", s.Timeout), field("journalRoot", journalPath),
		field("journalPolicy", "every_invocation"), field("installedBy", issue), field("isolationAssertedBy", isolation),
	}
	if s.Socket != "" {
		socket, err := settled(s.Socket)
		if err != nil {
			return nil, err
		}
		document = append(document, field("socketPath", socket))
	}
	return append(document, field("owner", "plugin"), field("adapterInterpreter", AdapterInterpreter), field("adapterEntryPoint", entry)), nil
}

// Complaints are why the Go Stop hook would refuse a document, as its own reader says it, and
// why a launcher could not run the adapter it names: with adapterInterpreter /usr/bin/env an
// adapterEntryPoint holding '=' is read by env as a NAME=VALUE assignment, and env then executes
// the settings path instead, so every Stop would run nothing (the doctor calls such a document a
// conflict; this refuses to write one).
func Complaints(document Object) []string {
	found := hook.Complaints(document)
	interpreter, _ := record.Get(document, "adapterInterpreter").(string)
	entry, _ := record.Get(document, "adapterEntryPoint").(string)
	if interpreter != "" && filepath.Base(interpreter) == "env" && strings.Contains(entry, "=") {
		found = append(found, "adapterEntryPoint "+evidence.Repr(entry)+" contains '=', which "+interpreter+" reads as a NAME=VALUE assignment rather than the program to run, so a launcher running ["+interpreter+", adapterEntryPoint, <settings>] would execute the settings file and every Stop would reach no adapter; install under a destination whose path has no '='")
	}
	return found
}

// writeSettings writes a settings document atomically (a temporary sibling renamed over the
// path). It is a variable only so that a test can make the write fail.
var writeSettings = record.AtomicWrite

// settingsWrite is completion.write_configuration: decided twice, acted on once, and never
// over settings that say something else. It is decided on a look taken before anything is read,
// so a write lands only on that document: under the settings lock the document is looked at
// again, and anything but the same document - another file renamed in, a rewrite, the same shape
// saying another mode - answers config_changed_underneath with nothing written.
func settingsWrite(ctx context.Context, path string, wanted Object, apply bool) Object {
	basis := lookAt(path)
	answer := Object{field("configuration", path), field("outcome", ""), field("applied", false), field("wrote", false)}
	if wrong := append(Complaints(wanted), unspellable(wanted)...); len(wrong) > 0 {
		return append(record.Set(answer, "outcome", ConfigWouldNotBeReadable), field("detail", strings.Join(wrong, "; ")), field("complaints", strs(wrong)))
	}
	outcome, found := settingsOutcome(path, wanted)
	answer = record.Set(answer, "outcome", outcome)
	switch outcome {
	case ConfigUnchanged:
		return append(answer, field("detail", "these settings are already installed"))
	case ConfigDiffers:
		return append(answer, field("detail", "settings are already installed and say something else; this command does not overwrite them"), field("differingFields", differing(found, wanted)))
	case ConfigUnreadable, ConfigUnreachable:
		return append(answer, field("detail", "the settings could not be read, so nothing was written: an unreadable document is never overwritten"))
	}
	if !apply {
		return append(record.Set(answer, "outcome", ConfigWouldCreate), field("detail", "would write these settings; nothing was written"))
	}
	beforeWriteLock(path)
	lock, err := record.LockContext(ctx, path, 0)
	if err != nil {
		if ctx.Err() != nil {
			return append(record.Set(answer, "outcome", Interrupted), field("detail", interrupted(err)))
		}
		return append(record.Set(answer, "outcome", "busy"), field("detail", err.Error()))
	}
	defer lock.Release()
	if again, _ := settingsOutcome(path, wanted); again != outcome || !lookAt(path).same(basis) {
		return append(record.Set(answer, "outcome", ConfigChangedUnderneath), field("detail", "the settings at "+path+" changed after they were read (another file, size, modification time or bytes), so nothing was written: settings decided on an earlier reading are never written over a newer document"),
			field("repair", "rerun to decide against the file as it now stands"))
	}
	if err := writeSettings(path, record.Encode(wanted)); err != nil {
		return append(record.Set(answer, "outcome", ConfigNotWritten), field("detail", "the settings could not be written, so the file stands as it was found: "+store.PythonOSError(err)))
	}
	readBack := readsBackAs(path, wanted)
	answer = append(record.Set(record.Set(answer, "applied", true), "wrote", true), field("readBack", readBack))
	if !readBack {
		return append(record.Set(answer, "outcome", ConfigAppliedUnverified), field("detail", "the settings were written and could not be read back as written; no hook should be relied on until they can be"))
	}
	return answer
}

// readsBackAs reports whether the document at path now reads as wanted.
func readsBackAs(path string, wanted Object) bool {
	back := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	return back.OK() && evidence.Dumps(back.Value, true, true, false) == evidence.Dumps(wanted, true, true, false)
}

// settingsOutcome is completion.config_outcome.
func settingsOutcome(path string, wanted Object) (string, Object) {
	found := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	switch {
	case found.State == reading.Absent:
		return ConfigCreated, nil
	case found.State == reading.AccessError:
		return ConfigUnreachable, nil
	case !found.OK():
		return ConfigUnreadable, nil
	}
	value, _ := found.Value.(Object)
	if evidence.Dumps(found.Value, true, true, false) == evidence.Dumps(wanted, true, true, false) {
		return ConfigUnchanged, value
	}
	return ConfigDiffers, value
}

func differing(found, wanted Object) any {
	if found == nil {
		return nil
	}
	keys := map[string]bool{}
	for _, f := range found {
		keys[f.Key] = true
	}
	for _, f := range wanted {
		keys[f.Key] = true
	}
	var out []string
	for key := range keys {
		if evidence.Dumps(record.Get(found, key), true, true, false) != evidence.Dumps(record.Get(wanted, key), true, true, false) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return strs(out)
}

// transition is what a promotion found of the plugin-owned Stop settings: the report, and why
// the pointer may not move when they would reach no adapter once it does. It writes nothing.
type transition struct {
	report  Object
	refused string
}

// checkSettings makes sure the plugin-owned Stop settings name nothing through the pointer that a
// Go runtime does not serve, BEFORE the pointer moves: every promotion and rollback moves it to a
// Go runtime, and none of them rewrites the settings (decision 18). Settings a user owns are
// judged with the second owners.
func checkSettings(codexHome, pointerPath string) transition {
	path := filepath.Join(codexHome, SettingsName)
	found := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	report := Object{field("configuration", path), field("state", found.State)}
	switch {
	case found.State == reading.Absent:
		return transition{report: append(report, field("action", "none"), field("detail", "no Stop settings exist, so nothing names an adapter the swap could strand"))}
	case !found.OK():
		return transition{report: report, refused: "the Stop settings at " + path + " could not be read (" + found.Detail + "), so whether the swap would strand the adapter they name was not established"}
	}
	document, _ := found.Value.(Object)
	if record.Get(document, "owner") != "plugin" {
		return transition{report: append(report, field("action", "none"), field("detail", "these settings belong to a user-owned registration; what its command runs through the pointer was judged with the second owners"))}
	}
	if wrong := strandedSettings(document, pointerPath); len(wrong) > 0 {
		return transition{report: append(report, field("action", "none")), refused: "the Stop settings at " + path + " would reach no adapter once the pointer names this runtime: " + strings.Join(wrong, "; ") + ". Nothing was changed: no promotion or rollback rewrites these settings. Point them at what this runtime serves by hand, or choose a runtime that serves them"}
	}
	return transition{report: append(report, field("action", "none"), field("detail", "every path the settings reach through the pointer is one this runtime serves, so they are left exactly as they are"))}
}

// strandedSettings is every path a settings document names through the pointer that a Go runtime
// does not serve, as "<key> <path>" reasons.
func strandedSettings(document Object, pointerPath string) []string {
	var out []string
	for _, key := range []string{"adapterInterpreter", "adapterEntryPoint", "relayExecutable"} {
		value, _ := record.Get(document, key).(string)
		if rel, through := throughPointer(value, pointerPath); through && !goProvides(rel) {
			out = append(out, key+" "+value+" names "+rel+" through the pointer, which this runtime does not provide")
		}
	}
	return out
}

func scopeStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return evidence.Dumps(v, false, false, true)
}
