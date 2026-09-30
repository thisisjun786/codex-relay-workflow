package install

import (
	"bytes"
	"context"
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
// (crw_stop_hook.py, packaged until todo 43, and its <CODEX_HOME>/crw-stop-hook.py copy) run
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

// claimArchive gives path's document the archive name steps.retire gives it,
// <path>.superseded-<stamp>, chosen to sort after every archive already there (a future stamp
// included), so the recoveries that read the greatest name read the newest document. create
// makes the archive at a name and answers os.ErrExist when the name is taken, and the next
// suffix is tried.
func claimArchive(path string, create func(target string) error) (string, error) {
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
	for suffix := taken + 1; ; suffix++ {
		target := prefix + moment
		if suffix > 0 {
			target += "-" + pad3(suffix)
		}
		err := create(target)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return target, nil
	}
}

// Supersede gives the document at path a second name beside it, the archive name steps.retire
// would give it, and leaves path where it is. The archive is a hard link: the same file, so its
// bytes, permission bits and inode are the document's, and putting it back restores exactly what
// was there. Where the filesystem refuses a link the document is copied instead. The document
// is then replaced by a rename over path, so there is no moment at which path is absent and a
// Stop finds no settings; steps.retire's move-then-write had one.
func Supersede(path string) (string, error) {
	return claimArchive(path, func(target string) error {
		err := os.Link(path, target)
		if err == nil || errors.Is(err, os.ErrExist) || errors.Is(err, os.ErrNotExist) {
			return err
		}
		return copyExclusive(path, target)
	})
}

// copyExclusive creates target, which must not exist, holding source's bytes and permission bits.
func copyExclusive(source, target string) (err error) {
	handle, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(target)
		}
	}()
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if _, err = handle.Write(raw); err != nil {
		return err
	}
	if err = handle.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	return handle.Sync()
}

func pad3(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

// writeSettings writes a settings document atomically (a temporary sibling renamed over the
// path). It is a variable only so that a test can make the write fail.
var writeSettings = record.AtomicWrite

// settingsWrite is completion.write_configuration: decided twice, acted on once, and never
// over settings that say something else - except a Python-era document when replace allows it
// and wanted keeps every host fact it records (only the adapter and installedBy move), which is
// archived (a second name, never deleted) and replaced by wanted in one rename. It is decided
// on a look taken before anything is read, so a write lands only on that document.
func settingsWrite(ctx context.Context, path string, wanted Object, apply, replace bool) Object {
	return settingsWriteOn(ctx, path, lookAt(path), wanted, apply, replace)
}

// settingsWriteOn is settingsWrite decided on basis: the look at path taken before the reading
// the decision (and, for a transition, wanted itself) was built from. Under the settings lock the
// document is looked at again, and anything but the same document - another file renamed in, a
// rewrite, the same shape saying another mode - answers config_changed_underneath with nothing
// written: a document built from an earlier reading is never written over a newer one.
func settingsWriteOn(ctx context.Context, path string, basis look, wanted Object, apply, replace bool) Object {
	answer := Object{field("configuration", path), field("outcome", ""), field("applied", false), field("wrote", false)}
	if wrong := append(Complaints(wanted), unspellable(wanted)...); len(wrong) > 0 {
		return append(record.Set(answer, "outcome", ConfigWouldNotBeReadable), field("detail", strings.Join(wrong, "; ")), field("complaints", strs(wrong)))
	}
	outcome, found := settingsOutcome(path, wanted, replace)
	answer = record.Set(answer, "outcome", outcome)
	switch outcome {
	case ConfigUnchanged:
		return append(answer, field("detail", "these settings are already installed"))
	case ConfigDiffers:
		if PythonEra(found) && replace {
			return append(answer, field("detail", "the installed settings name the Python adapter and record host facts these flags do not (differingFields). Replacing them moves only the adapter and keeps every host fact - a silent rewrite of the mode, the roots or the isolation changes what a Stop does - so nothing was written"),
				field("differingFields", hostFactsDiffering(found, wanted)),
				field("repair", "rerun with flags that say what those settings say (--mode, --isolation-asserted-by, --marker-root, --db-path, --socket, --journal-root, --relay-command, --guard-timeout), or move "+path+" aside by hand and rerun to write these flags' settings instead"))
		}
		detail := "settings are already installed and say something else; this command does not overwrite them"
		if PythonEra(found) {
			detail = "the installed settings name the Python adapter, and they are replaced only by crw install immediately before it moves the pointer to a Go runtime, because until then the Python adapter is the one the pointer reaches"
		}
		return append(answer, field("detail", detail), field("differingFields", differing(found, wanted)))
	case ConfigUnreadable, ConfigUnreachable:
		return append(answer, field("detail", "the settings could not be read, so nothing was written: an unreadable document is never overwritten"))
	}
	if !apply {
		detail := "would write these settings; nothing was written"
		if outcome == ConfigReplaced {
			detail = "would archive the Python-era settings beside " + path + " and replace them by these, which keep every host fact they record; nothing was written"
		}
		return append(record.Set(answer, "outcome", ConfigWouldCreate), field("detail", detail))
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
	if again, _ := settingsOutcome(path, wanted, replace); again != outcome || !lookAt(path).same(basis) {
		return append(record.Set(answer, "outcome", ConfigChangedUnderneath), field("detail", "the settings at "+path+" changed after they were read (another file, size, modification time or bytes), so nothing was written: settings decided on an earlier reading are never written over a newer document"),
			field("repair", "rerun to decide against the file as it now stands"))
	}
	if outcome == ConfigReplaced {
		return replaceSettings(path, basis, wanted, answer)
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

// replaceSettings replaces a Python-era document by wanted, under the settings lock, with no
// moment at which path is absent: the document is given its archive name first (steps.retire's
// name, so the recoveries that read the newest archive find it), and wanted is then renamed over
// path. The archive has to hold the document the replacement was decided on (basis), or the
// archive name is dropped and nothing is written. A write that fails leaves path holding the
// document it held and drops the archive name; a write that lands and does not read back as
// written means path now holds bytes this run did not write, and those are left where they are
// with the archive kept (settleBack).
func replaceSettings(path string, basis look, wanted, answer Object) Object {
	archived, err := Supersede(path)
	if err != nil {
		return append(record.Set(answer, "outcome", ConfigNotWritten), field("detail", "the Python-era settings could not be archived, so nothing was written: "+store.PythonOSError(err)))
	}
	if kept := lookAt(archived); kept.failed != "" || !bytes.Equal(kept.raw, basis.raw) {
		_ = os.Remove(archived)
		return append(record.Set(answer, "outcome", ConfigChangedUnderneath), field("detail", "the settings at "+path+" changed while they were being archived, so the archive name was dropped and nothing was written"),
			field("repair", "rerun to decide against the file as it now stands"))
	}
	encoded := record.Encode(wanted)
	if err := writeSettings(path, encoded); err != nil {
		return failedReplacement(path, answer, archived, encoded, "the Go adapter's settings could not be written ("+store.PythonOSError(err)+")")
	}
	answer = record.Set(answer, "wrote", true)
	if !readsBackAs(path, wanted) {
		return failedReplacement(path, append(answer, field("readBack", false)), archived, encoded, "the Go adapter's settings were written and did not read back as written")
	}
	return append(record.Set(answer, "applied", true), field("readBack", true), field("retired", archived),
		field("detail", "the Python-era settings were archived to "+archived+" and replaced by the Go adapter's in one rename"))
}

// failedReplacement settles a replacement that did not land as written. The answer carries
// 'retired' only while the archive still exists, which is when path was not put back.
func failedReplacement(path string, answer Object, archived string, wrote []byte, why string) Object {
	back := settleBack(path, archived, wrote)
	answer = append(answer, field("putBack", back))
	if record.Get(back, "undone") == true {
		return append(record.Set(answer, "outcome", ConfigNotWritten), field("detail", why+", so the Python-era settings were kept: "+scopeStr(record.Get(back, "detail"))))
	}
	return append(record.Set(answer, "outcome", ConfigAppliedUnverified), field("retired", archived),
		field("detail", why+", and the Python-era settings archived at "+archived+" were not put back: "+scopeStr(record.Get(back, "detail"))))
}

// settingsOutcome is completion.config_outcome, with the Python-era replacement: a Python-era
// document is replaced only by a wanted that records every host fact it records.
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
	if replace && value != nil && PythonEra(value) && len(asList(hostFactsDiffering(value, wanted))) == 0 {
		return ConfigReplaced, value
	}
	return ConfigDiffers, value
}

// adapterKeys are what a replacement moves; every other key is a host fact it keeps.
var adapterKeys = map[string]bool{"adapterInterpreter": true, "adapterEntryPoint": true, "installedBy": true}

// hostFactsDiffering is differing without the adapter keys and installedBy.
func hostFactsDiffering(found, wanted Object) any {
	out := []any{}
	for _, key := range asList(differing(found, wanted)) {
		if !adapterKeys[key.(string)] {
			out = append(out, key)
		}
	}
	return out
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

// transition is what a promotion does to the plugin-owned Stop settings, and how to undo it.
// undo is never nil: it puts back whatever this transition changed, refused or not, and says
// so; a caller runs it on every path on which the pointer does not end up where the transition
// prepared for.
type transition struct {
	report  Object
	refused string
	undo    func() Object
}

// transitionSettings is transitionSettingsFor the Go runtime a promotion moves the pointer to
// (its callers pass doctor.KindGoRuntime); a rollback, which may name a venv, calls
// transitionSettingsFor with what that venv serves.
func transitionSettings(ctx context.Context, codexHome, pointerPath, targetKind string) transition {
	return transitionSettingsFor(ctx, codexHome, pointerPath, targetKind, goProvides)
}

// transitionSettingsFor makes sure the plugin-owned Stop settings name an adapter the pointer
// still reaches once it names a runtime of targetKind (which serves what target answers yes
// to), BEFORE it moves. There is one plugin-owned document for both runtime kinds: adapter
// /usr/bin/env <pointer>/bin/crw-completion-hook, which is the Go hook on a Go runtime and the
// Python Stop adapter's console script on a venv, both reading the settings path as their one
// argument. So the only rewrite is forward: a Python-era document (adapterInterpreter
// .../current/bin/python3, which vanishes when the pointer leaves the venv) is archived and
// replaced by its Go variant - the same host facts, only the adapter moved - in one rename.
// That document is valid on both sides of the swap, so there is no window. No move to a venv
// rewrites the settings back: every path the live document reaches through the pointer must be
// served by the target, or the move is refused with nothing changed.
//
// The Go variant is built from one look at the document and written only onto that document
// (settingsWriteOn). When another writer replaced it in between, nothing is written and the
// transition is decided again from a fresh reading, a bounded number of times.
func transitionSettingsFor(ctx context.Context, codexHome, pointerPath, targetKind string, target provides) transition {
	var last transition
	for attempt := 0; attempt < transitionAttempts; attempt++ {
		decided, changed := transitionOnce(ctx, codexHome, pointerPath, targetKind, target)
		if !changed {
			if attempt > 0 {
				decided.report = append(decided.report, field("rebuiltFromFreshReading", int64(attempt)))
			}
			return decided
		}
		last = decided
	}
	path := filepath.Join(codexHome, SettingsName)
	return transition{report: last.report, refused: "the Stop settings at " + path + " changed under this run each of the " + strconv.Itoa(transitionAttempts) + " times it read them, so nothing was written and the pointer was not moved: stop whatever is rewriting them, then rerun", undo: func() Object { return nil }}
}

// transitionAttempts bounds how often a transition is decided again after the settings changed
// under it.
const transitionAttempts = 3

// transitionOnce is one decision of transitionSettingsFor on one look at the settings, and
// whether the settings turned out to have changed under it (nothing was written then).
func transitionOnce(ctx context.Context, codexHome, pointerPath, targetKind string, target provides) (transition, bool) {
	path := filepath.Join(codexHome, SettingsName)
	none := func() Object { return nil }
	basis := lookAt(path)
	found := reading.ReadJSON(path, "the completion hook configuration", nil, nil)
	report := Object{field("configuration", path), field("state", found.State)}
	if found.OK() && !basis.holds(found.Value) {
		return transition{report: report, undo: none}, true
	}
	switch {
	case found.State == reading.Absent:
		return transition{report: append(report, field("action", "none"), field("detail", "no Stop settings exist, so nothing names an adapter the swap could strand")), undo: none}, false
	case !found.OK():
		return transition{report: report, refused: "the Stop settings at " + path + " could not be read (" + found.Detail + "), so whether the swap would strand the adapter they name was not established", undo: none}, false
	}
	document, _ := found.Value.(Object)
	if record.Get(document, "owner") != "plugin" {
		return transition{report: append(report, field("action", "none"), field("detail", "these settings belong to a user-owned registration; what its command runs through the pointer was judged with the second owners")), undo: none}, false
	}
	if targetKind == doctor.KindGoRuntime && PythonEra(document) {
		wanted := GoVariant(document, pointerPath)
		if wrong := Complaints(wanted); len(wrong) > 0 {
			return transition{report: report, refused: "the Go variant of the Python-era settings would be refused by the Go hook: " + strings.Join(wrong, "; "), undo: none}, false
		}
		if stranded := strandedSettings(wanted, pointerPath, target); len(stranded) > 0 {
			return transition{report: report, refused: "the Go variant of the Python-era settings would reach through the pointer what the runtime does not provide: " + strings.Join(stranded, "; "), undo: none}, false
		}
		written := settingsWriteOn(ctx, path, basis, wanted, true, true)
		if record.Get(written, "outcome") == ConfigChangedUnderneath {
			return transition{report: append(report, field("write", written)), undo: none}, true
		}
		retired, _ := record.Get(written, "retired").(string)
		undo := none
		if retired != "" {
			encoded := record.Encode(wanted)
			undo = func() Object { return lockedSettleBack(path, retired, encoded) }
		}
		if record.Get(written, "outcome") != ConfigReplaced || record.Get(written, "readBack") != true {
			return transition{report: append(report, field("write", written)), refused: "the Python-era settings could not be replaced by the Go adapter's: " + scopeStr(record.Get(written, "detail")), undo: undo}, false
		}
		return transition{report: append(report, field("action", "replaced"), field("retired", retired), field("write", written),
			field("detail", "the Python-era settings were archived to "+retired+" and replaced by the Go adapter's, before the pointer moved; that one document serves a Go runtime and a venv alike")), undo: undo}, false
	}
	if wrong := strandedSettings(document, pointerPath, target); len(wrong) > 0 {
		return transition{report: append(report, field("action", "none")), refused: "the Stop settings at " + path + " would reach no adapter once the pointer names this runtime: " + strings.Join(wrong, "; ") + ". Nothing was changed: no promotion or rollback rewrites these settings. Point them at what this runtime serves by hand, or choose a runtime that serves them", undo: none}, false
	}
	detail := "every path the settings reach through the pointer is one this runtime serves, so they are left exactly as they are"
	switch {
	case GoEra(document) && targetKind == doctor.KindPythonVenv:
		detail = "the settings run " + AdapterInterpreter + " " + filepath.Join(pointerPath, "bin", definition.HookScript) + ", which on this venv is the Python Stop adapter's console script and reads the settings path as its argument as the Go hook does, so the one document serves both runtime kinds and is left exactly as it is"
	case GoEra(document):
		detail = "the settings already name the Go adapter through the pointer"
	case PythonEra(document):
		detail = "the settings name the Python adapter, which this venv serves through the pointer, so they are left exactly as they are"
	}
	return transition{report: append(report, field("action", "none"), field("detail", detail)), undo: none}, false
}

// strandedSettings is every path a settings document names through the pointer that the target
// does not serve, as "<key> <path>" reasons.
func strandedSettings(document Object, pointerPath string, target provides) []string {
	var out []string
	for _, key := range []string{"adapterInterpreter", "adapterEntryPoint", "relayExecutable"} {
		value, _ := record.Get(document, key).(string)
		if rel, through := throughPointer(value, pointerPath); through && !target(rel) {
			out = append(out, key+" "+value+" names "+rel+" through the pointer, which this runtime does not provide")
		}
	}
	return out
}

// lockedSettleBack is settleBack under the settings lock the Python writers take.
func lockedSettleBack(path, archived string, wrote []byte) Object {
	lock, err := record.Lock(path, 0)
	if err != nil {
		return Object{field("undone", false), field("detail", "the settings lock could not be taken, so the settings archived at "+archived+" were not put back: "+err.Error())}
	}
	defer lock.Release()
	return settleBack(path, archived, wrote)
}

// settleBack returns path to the document archived from it, and consumes the archive, only where
// that restores exactly what this run found and destroys nothing it did not write:
//   - path is still the archived file itself (or, for a copied archive, holds its bytes): the
//     replacement never landed, so only the archive name is dropped;
//   - path holds exactly the bytes this run wrote (wrote): the archive is exchanged back over it
//     in one step, which puts the document's own file (inode, bytes, permission bits) back, and
//     the displaced file is removed only when it is this run's - otherwise it is exchanged back
//     again and both are left;
//   - path is gone: the archived file is linked back to it (never over anything);
//   - path holds anything else: another writer's document, left where it is with the archive
//     kept beside it and both named.
func settleBack(path, archived string, wrote []byte) Object {
	archivedInfo, err := os.Lstat(archived)
	if err != nil {
		return Object{field("undone", false), field("detail", "the settings archived at "+archived+" could not be read: "+store.PythonOSError(err))}
	}
	dropArchive := func(why string) Object {
		if err := os.Remove(archived); err != nil {
			return Object{field("undone", true), field("residual", archived), field("detail", why+"; the archive name "+archived+" could not be removed: "+store.PythonOSError(err))}
		}
		return Object{field("undone", true), field("detail", why+"; the archive name "+archived+" was removed")}
	}
	if pathInfo, err := os.Lstat(path); err == nil && os.SameFile(pathInfo, archivedInfo) {
		return dropArchive(path + " is still the file this run found")
	}
	kept, err := os.ReadFile(archived)
	if err != nil {
		return Object{field("undone", false), field("detail", "the settings archived at "+archived+" could not be read: "+store.PythonOSError(err))}
	}
	now, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(now, kept):
		return dropArchive(path + " still holds the settings as they were found")
	case err == nil && wrote != nil && bytes.Equal(now, wrote):
		return exchangeBack(path, archived, wrote)
	case errors.Is(err, os.ErrNotExist):
		// Nothing is there to destroy: the found document gets its name back by a link, which
		// fails rather than replace anything that appeared since.
		if err := os.Link(archived, path); err != nil {
			return Object{field("undone", false), field("retained", archived), field("detail", path+" was gone and the settings archived at "+archived+" could not be given its name again: "+store.PythonOSError(err))}
		}
		return dropArchive(path + " was gone, and the settings this run found were given that name again")
	case err != nil:
		return Object{field("undone", false), field("retained", archived), field("detail", path+" could not be read ("+store.PythonOSError(err)+"), so whether it still holds what this run wrote is not established; it was left as it is, and the settings this run found stay at "+archived)}
	}
	return Object{field("undone", false), field("retained", archived), field("detail", path+" holds a document this run did not write - another writer's, which is never removed or overwritten - so it was left as it is, and the settings this run found stay at "+archived+"; decide between the two by hand")}
}

// exchangeBack puts the archived document back over path, which held exactly wrote when it was
// read, by exchanging the two names in one step (renameat2 RENAME_EXCHANGE on Linux,
// renamex_np RENAME_SWAP on darwin): a writer that replaced path in between is displaced to the
// archive name rather than destroyed, and is exchanged back when what was displaced is not this
// run's. Where no atomic exchange is available (another platform, or a filesystem without one)
// nothing is renamed over the active path: both files are left, and the answer says how to put
// the settings back by hand.
func exchangeBack(path, archived string, wrote []byte) Object {
	switch err := swapNames(archived, path); {
	case errors.Is(err, errNoExchange):
		return Object{field("undone", false), field("retained", archived),
			field("detail", "this platform or filesystem cannot exchange two names in one step, and a rename over "+path+" could destroy a document another writer saved after it was read, so nothing was renamed: "+path+" still holds the settings this run wrote and the settings it found stay at "+archived),
			field("recoveryRequires", "put the settings this run found back by hand once nothing else writes "+path+": mv "+archived+" "+path)}
	case err != nil:
		return Object{field("undone", false), field("retained", archived), field("detail", "the settings archived at "+archived+" could not be put back: "+store.PythonOSError(err))}
	}
	displaced, err := os.ReadFile(archived)
	if err == nil && bytes.Equal(displaced, wrote) {
		if err := os.Remove(archived); err != nil {
			return Object{field("undone", true), field("residual", archived), field("detail", "the settings this run found were put back, and what this run wrote, now at "+archived+", could not be removed: "+store.PythonOSError(err))}
		}
		return Object{field("undone", true), field("detail", "the settings this run found were put back (the same file, exchanged for what this run wrote), and what this run wrote was removed")}
	}
	if err := swapNames(archived, path); err != nil {
		return Object{field("undone", false), field("retained", archived), field("detail", "another writer replaced "+path+" while the settings this run found were being put back; they are at "+path+" and that writer's document is at "+archived+", and exchanging them again failed: "+store.PythonOSError(err)),
			field("recoveryRequires", "decide between "+path+" and "+archived+" by hand")}
	}
	return Object{field("undone", false), field("retained", archived), field("detail", "another writer replaced "+path+" while the settings this run found were being put back, so its document was left at "+path+" and the settings this run found stay at "+archived)}
}

// swapNames is exchange, a variable only so that a test can take the exchange away.
var swapNames = exchange

func scopeStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return evidence.Dumps(v, false, false, true)
}
