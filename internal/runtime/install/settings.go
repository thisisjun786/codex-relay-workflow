package install

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// SettingsName is the Stop hook's settings record under the Codex home.
const SettingsName = hook.ConfigName

// retiredKeys are the settings keys a document may still carry that nothing reads since
// decision 66: the adapter the retired Python launchers ran. A document that differs from the
// one this command would write only by them, and by installedBy, is rewritten without them.
var retiredKeys = map[string]bool{"adapterInterpreter": true, "adapterEntryPoint": true}

// Outcomes of writing the settings record (completion.CONFIG_*), and ConfigReplaced: a document
// that differed only by the retired keys, rewritten without them.
const (
	ConfigCreated            = "config_created"
	ConfigUnchanged          = "config_unchanged"
	ConfigWouldCreate        = "config_would_create"
	ConfigDiffers            = "config_differs"
	ConfigChangedUnderneath  = "config_changed_underneath"
	ConfigAppliedUnverified  = "config_applied_unverified"
	ConfigWouldNotBeReadable = "config_would_not_be_readable"
	ConfigReplaced           = "config_replaced"
	ConfigNotWritten         = "config_not_written"
	ConfigUnreadable         = "config_unreadable"
	ConfigUnreachable        = "config_unreachable"
	// Interrupted is a settings or bridge-record write whose command was interrupted while it
	// waited for the file's lock: nothing was written.
	Interrupted = "interrupted"
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

// Document is the settings a Go install writes for the plugin-owned Stop registration: the relay
// named through the owned pointer, so an update moves the pointer and the settings keep naming
// the right runtime.
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
	return append(document, field("owner", "plugin")), nil
}

// Complaints are why the Go Stop hook would refuse a document, as its own reader says it.
func Complaints(document Object) []string { return hook.Complaints(document) }

// writeSettings writes a settings document atomically (a temporary sibling renamed over the
// path). It is a variable only so that a test can make the write fail.
var writeSettings = record.AtomicWrite

// settingsWrite is completion.write_configuration: decided twice, acted on once, and never
// over settings that say something else - but for a document that differs only by the retired
// keys and installedBy, which it rewrites without them (config_replaced, keeping every host fact
// the document records). It is decided on a look taken before anything is read,
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
		detail := "would write these settings; nothing was written"
		if outcome == ConfigReplaced {
			detail = "would rewrite the settings at " + path + ", which differ from these only by the retired keys (" + strings.Join(retiredIn(found), ", ") + ") and installedBy, without them; nothing was written"
		}
		return append(record.Set(answer, "outcome", ConfigWouldCreate), field("detail", detail))
	}
	beforeWriteLock(path)
	lock, err := record.Lock(ctx, path, 0)
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
		return append(record.Set(answer, "outcome", ConfigNotWritten), field("detail", "the settings could not be written, so the file stands as it was found: "+err.Error()))
	}
	readBack := readsBackAs(path, wanted)
	answer = append(record.Set(record.Set(answer, "applied", true), "wrote", true), field("readBack", readBack))
	if !readBack {
		return append(record.Set(answer, "outcome", ConfigAppliedUnverified), field("detail", "the settings were written and could not be read back as written; no hook should be relied on until they can be"))
	}
	if outcome == ConfigReplaced {
		return append(answer, field("retiredFields", strs(retiredIn(found))),
			field("detail", "the settings differed from these only by the retired keys and installedBy, and were rewritten without them; every other host fact they recorded is kept"))
	}
	return answer
}

// retiredIn is the retired keys a document carries, sorted.
func retiredIn(document Object) []string {
	var out []string
	for _, f := range document {
		if retiredKeys[f.Key] {
			out = append(out, f.Key)
		}
	}
	sort.Strings(out)
	return out
}

// readsBackAs reports whether the document at path now reads as wanted.
func readsBackAs(path string, wanted Object) bool {
	back := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	return back.OK() && pyjson.Dumps(back.Value, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) == pyjson.Dumps(wanted, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
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
	if pyjson.Dumps(found.Value, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) == pyjson.Dumps(wanted, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) {
		return ConfigUnchanged, value
	}
	if value != nil && len(retiredIn(value)) > 0 && len(retiredIn(wanted)) == 0 {
		rest := false
		for _, key := range asList(differing(value, wanted)) {
			rest = rest || !retiredKeys[key.(string)] && key != "installedBy"
		}
		if !rest {
			return ConfigReplaced, value
		}
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
		if pyjson.Dumps(record.Get(found, key), pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) != pyjson.Dumps(record.Get(wanted, key), pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) {
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
// does not serve, as "<key> <path>" reasons: the relay it runs (the retired adapter keys run
// nothing).
func strandedSettings(document Object, pointerPath string) []string {
	var out []string
	value, _ := record.Get(document, "relayExecutable").(string)
	if rel, through := throughPointer(value, pointerPath); through && !goProvides(rel) {
		out = append(out, "relayExecutable "+value+" names "+rel+" through the pointer, which this runtime does not provide")
	}
	return out
}

func scopeStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return pyjson.Dumps(v, pyjson.Options{})
}
