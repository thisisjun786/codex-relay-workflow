package install

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// SettingsName is the Stop hook's settings record under the Codex home.
const SettingsName = hook.ConfigName

// SettingsOverride is the variable a plugin-owned registration refuses (completion.CONFIG_ENV).
const SettingsOverride = "CRW_COMPLETION_HOOK_CONFIG"

// AdapterInterpreter is what a Go install records as adapterInterpreter. The legacy launchers
// (plugins/crw/wiring/crw_stop_hook.py, its <CODEX_HOME>/crw-stop-hook.py copy) run
// [adapterInterpreter, adapterEntryPoint, <settings>], and /usr/bin/env executes the entry
// point with the settings path as its one argument, which is what the Go hook reads as its
// settings. Recording the binary itself would hand the Go hook the binary's own path as its
// settings, and every Stop a cached launcher runs would evaluate nothing.
const AdapterInterpreter = "/usr/bin/env"

// Outcomes of writing the settings record (completion.CONFIG_*), plus the Python-era
// replacement a Go install performs before it moves the pointer.
const (
	ConfigCreated            = "config_created"
	ConfigUnchanged          = "config_unchanged"
	ConfigWouldCreate        = "config_would_create"
	ConfigDiffers            = "config_differs"
	ConfigChangedUnderneath  = "config_changed_underneath"
	ConfigAppliedUnverified  = "config_applied_unverified"
	ConfigWouldNotBeReadable = "config_would_not_be_readable"
	ConfigReplaced           = "config_replaced"
	ConfigUnreadable         = "config_unreadable"
	ConfigUnreachable        = "config_unreachable"
)

var configSettled = map[string]bool{ConfigCreated: true, ConfigUnchanged: true, ConfigWouldCreate: true, ConfigReplaced: true}

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

// Complaints are why the Go Stop hook would refuse a document, as its own reader says it.
func Complaints(document Object) []string { return hook.Complaints(document) }

// pythonName matches a Python interpreter's basename.
var pythonName = regexp.MustCompile(`^python[0-9.]*$`)

// PythonEra reports a plugin-owned document whose adapter is the Python one: an interpreter
// named python* or a .py entry point. Such a document runs only while the pointer names a venv.
func PythonEra(document Object) bool {
	if record.Get(document, "owner") != "plugin" {
		return false
	}
	interpreter, _ := record.Get(document, "adapterInterpreter").(string)
	entry, _ := record.Get(document, "adapterEntryPoint").(string)
	return pythonName.MatchString(filepath.Base(interpreter)) || strings.HasSuffix(entry, ".py")
}

// GoEra reports a plugin-owned document whose adapter is the Go hook reached through a pointer.
func GoEra(document Object) bool {
	if record.Get(document, "owner") != "plugin" {
		return false
	}
	entry, _ := record.Get(document, "adapterEntryPoint").(string)
	return record.Get(document, "adapterInterpreter") == AdapterInterpreter && filepath.Base(entry) == definition.HookScript
}

// GoVariant is the Go document for a Python-era one: the same host facts (relay, marker root,
// database, journal, socket, mode, budget) with only the adapter moved to the Go hook through
// the pointer.
func GoVariant(document Object, pointerPath string) Object {
	out := copyObject(document)
	out = record.Set(out, "adapterInterpreter", AdapterInterpreter)
	return record.Set(out, "adapterEntryPoint", filepath.Join(pointerPath, "bin", definition.HookScript))
}

// supersededPrefix is the archive name steps.retire gives a document it moves aside, so the
// Python transition's recovery finds it by the same glob.
func supersededPrefix(path string) string { return path + ".superseded-" }

var supersededStamp = regexp.MustCompile(`^(\d{8}T\d{6}Z)(?:-(\d{3,}))?$`)

// Retire moves path aside under a name nothing reads (steps.retire): <path>.superseded-<stamp>,
// chosen to sort after every archive already there, and never deletes it.
func Retire(path string) (string, error) {
	prefix := supersededPrefix(path)
	holder, stem := filepath.Dir(prefix), filepath.Base(prefix)
	entries, err := os.ReadDir(holder)
	if err != nil {
		return "", err
	}
	moment := time.Now().UTC().Format("20060102T150405Z")
	taken := -1
	type found struct {
		stamp  string
		suffix int
	}
	var reached []found
	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), stem)
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		match := supersededStamp.FindStringSubmatch(rest)
		if match == nil {
			continue
		}
		suffix := 0
		if match[2] != "" {
			suffix, _ = strconv.Atoi(match[2])
		}
		reached = append(reached, found{match[1], suffix})
		if match[1] > moment {
			moment = match[1]
		}
	}
	for _, f := range reached {
		if f.stamp == moment && f.suffix > taken {
			taken = f.suffix
		}
	}
	suffix := taken + 1
	for {
		target := prefix + moment
		if suffix > 0 {
			target += "-" + pad3(suffix)
		}
		handle, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			suffix++
			continue
		}
		if err != nil {
			return "", err
		}
		_ = handle.Close()
		if err := os.Rename(path, target); err != nil {
			_ = os.Remove(target)
			return "", err
		}
		return target, nil
	}
}

func pad3(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// newestRetired is the newest archived document beside path that satisfies keep.
func newestRetired(path string, keep func(Object) bool) (string, Object) {
	prefix := supersededPrefix(path)
	entries, err := os.ReadDir(filepath.Dir(prefix))
	if err != nil {
		return "", nil
	}
	var names []string
	for _, entry := range entries {
		if rest, ok := strings.CutPrefix(entry.Name(), filepath.Base(prefix)); ok && entry.Type().IsRegular() && supersededStamp.MatchString(rest) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		candidate := filepath.Join(filepath.Dir(prefix), name)
		read := reading.ReadJSON(candidate, "a retired completion hook configuration", nil, nil)
		if value, ok := read.Value.(Object); ok && read.OK() && keep(value) {
			return candidate, value
		}
	}
	return "", nil
}

// settingsWrite is completion.write_configuration: decided twice, acted on once, and never
// over settings that say something else - except a Python-era document when replace allows
// it, which is retired (moved aside, never deleted) and replaced by wanted.
func settingsWrite(path string, wanted Object, apply, replace bool) Object {
	answer := Object{field("configuration", path), field("outcome", ""), field("applied", false), field("wrote", false)}
	if wrong := Complaints(wanted); len(wrong) > 0 {
		return append(record.Set(answer, "outcome", ConfigWouldNotBeReadable), field("detail", strings.Join(wrong, "; ")), field("complaints", strs(wrong)))
	}
	outcome, found := settingsOutcome(path, wanted, replace)
	answer = record.Set(answer, "outcome", outcome)
	switch outcome {
	case ConfigUnchanged:
		return append(answer, field("detail", "these settings are already installed"))
	case ConfigDiffers:
		detail := "settings are already installed and say something else; this command does not overwrite them"
		if PythonEra(found) {
			detail = "the installed settings name the Python adapter, and they are replaced only by crw install immediately before it moves the pointer to a Go runtime, because until then the Python adapter is the one the pointer reaches"
		}
		return append(answer, field("detail", detail), field("differingFields", differing(found, wanted)))
	case ConfigUnreadable, ConfigUnreachable:
		return append(answer, field("detail", "the settings could not be read, so nothing was written: an unreadable document is never overwritten"))
	}
	if !apply {
		return append(record.Set(answer, "outcome", ConfigWouldCreate), field("detail", "would write these settings; nothing was written"))
	}
	lock, err := record.Lock(path, 0)
	if err != nil {
		return append(record.Set(answer, "outcome", "busy"), field("detail", err.Error()))
	}
	defer lock.Release()
	if again, _ := settingsOutcome(path, wanted, replace); again != outcome {
		return append(record.Set(answer, "outcome", ConfigChangedUnderneath), field("detail", "the settings changed after they were read, so nothing was written; rerun to decide against the file as it now stands"))
	}
	if outcome == ConfigReplaced {
		retired, err := Retire(path)
		if err != nil {
			return append(record.Set(answer, "outcome", ConfigDiffers), field("detail", "the Python-era settings could not be retired, so nothing was written: "+store.PythonOSError(err)))
		}
		answer = append(answer, field("retired", retired))
	}
	if err := record.AtomicWrite(path, record.Encode(wanted)); err != nil {
		return append(answer, field("detail", "the settings could not be written: "+store.PythonOSError(err)))
	}
	back := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	readBack := back.OK() && evidence.Dumps(back.Value, true, true, false) == evidence.Dumps(wanted, true, true, false)
	answer = append(record.Set(record.Set(answer, "applied", true), "wrote", true), field("readBack", readBack))
	if !readBack {
		return append(record.Set(answer, "outcome", ConfigAppliedUnverified), field("detail", "the settings were written and could not be read back as written; no hook should be relied on until they can be"))
	}
	if outcome == ConfigReplaced {
		return append(answer, field("detail", "the Python-era settings were retired beside this file and replaced by the Go adapter's"))
	}
	return answer
}

// settingsOutcome is completion.config_outcome, with the Python-era replacement.
func settingsOutcome(path string, wanted Object, replace bool) (string, Object) {
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
	if replace && value != nil && PythonEra(value) {
		return ConfigReplaced, value
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

// transition is what a promotion does to the plugin-owned Stop settings so the adapter they
// name exists on both sides of the pointer swap, and how to undo it.
type transition struct {
	report  Object
	refused string
	undo    func() Object
}

// transitionSettings carries the plugin-owned settings to the runtime kind the pointer is about
// to name, BEFORE it moves: a Python-era document (adapterInterpreter .../current/bin/python3,
// which vanishes when the pointer leaves the venv) is retired and replaced by its Go variant for
// a Go target, and for a Python target the newest retired Python-era document is put back. It
// is ordered before the swap so that no cached launcher is left naming an interpreter the
// pointer no longer reaches; undo reverses it when the promotion then fails.
func transitionSettings(codexHome, pointerPath, targetKind string) transition {
	path := filepath.Join(codexHome, SettingsName)
	none := func() Object { return nil }
	found := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	report := Object{field("configuration", path), field("state", found.State)}
	switch {
	case found.State == reading.Absent:
		return transition{report: append(report, field("action", "none"), field("detail", "no Stop settings exist, so nothing names an adapter the swap could strand")), undo: none}
	case !found.OK():
		return transition{report: report, refused: "the Stop settings at " + path + " could not be read (" + found.Detail + "), so whether the swap would strand the adapter they name was not established", undo: none}
	}
	document, _ := found.Value.(Object)
	switch {
	case record.Get(document, "owner") != "plugin":
		return transition{report: append(report, field("action", "none"), field("detail", "these settings belong to a user-owned registration, whose command names its interpreter itself")), undo: none}
	case targetKind == doctor.KindGoRuntime && GoEra(document):
		return transition{report: append(report, field("action", "none"), field("detail", "the settings already name the Go adapter through the pointer")), undo: none}
	case targetKind == doctor.KindGoRuntime && PythonEra(document):
		wanted := GoVariant(document, pointerPath)
		if wrong := Complaints(wanted); len(wrong) > 0 {
			return transition{report: report, refused: "the Go variant of the Python-era settings would be refused by the Go hook: " + strings.Join(wrong, "; "), undo: none}
		}
		written := settingsWrite(path, wanted, true, true)
		if record.Get(written, "outcome") != ConfigReplaced || record.Get(written, "readBack") != true {
			return transition{report: append(report, field("write", written)), refused: "the Python-era settings could not be replaced by the Go adapter's: " + scopeStr(record.Get(written, "detail")), undo: none}
		}
		retired, _ := record.Get(written, "retired").(string)
		return transition{report: append(report, field("action", "replaced"), field("retired", retired), field("write", written),
			field("detail", "the Python-era settings were retired to "+retired+" and replaced by the Go adapter's, before the pointer moved")),
			undo: func() Object { return putBack(path, retired) }}
	case targetKind == doctor.KindPythonVenv && GoEra(document):
		retired, previous := newestRetired(path, PythonEra)
		if previous == nil {
			return transition{report: report, refused: "the settings name the Go adapter and no retired Python-era settings exist beside " + path + " to put back, so after the swap the Stop hook would reach no adapter", undo: none}
		}
		moved, err := Retire(path)
		if err != nil {
			return transition{report: report, refused: "the Go settings could not be retired: " + store.PythonOSError(err), undo: none}
		}
		if err := os.Rename(retired, path); err != nil {
			_ = os.Rename(moved, path)
			return transition{report: report, refused: "the retired Python-era settings could not be put back: " + store.PythonOSError(err), undo: none}
		}
		return transition{report: append(report, field("action", "restored"), field("restoredFrom", retired), field("retired", moved),
			field("detail", "the Go settings were retired to "+moved+" and the Python-era settings from "+retired+" were put back, before the pointer moved")),
			undo: func() Object {
				if _, err := Retire(path); err != nil {
					return Object{field("undone", false), field("detail", store.PythonOSError(err))}
				}
				return putBack(path, moved)
			}}
	case targetKind == doctor.KindPythonVenv && PythonEra(document):
		return transition{report: append(report, field("action", "none"), field("detail", "the settings already name the Python adapter")), undo: none}
	}
	return transition{report: append(report, field("action", "none"), field("detail", "the settings name an adapter this command did not write, so they are left as they are")), undo: none}
}

// putBack moves an archived document over path again (an atomic rename).
func putBack(path, archived string) Object {
	if err := os.Rename(archived, path); err != nil {
		return Object{field("undone", false), field("detail", "the settings archived at "+archived+" could not be put back: "+store.PythonOSError(err))}
	}
	return Object{field("undone", true), field("detail", "the settings archived at "+archived+" were put back")}
}

func scopeStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return evidence.Dumps(v, false, false, true)
}
