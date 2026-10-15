package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// supervisorUnconfigured is the refusal of a run whose configuration leaves a value it needs empty:
// no management thread, or for register no host or no settings file. The relay is never called:
// there is nothing to bind and nothing to look up. The refusal names each missing key under
// supervisorSectionKey and the configuration file the run read, or says that none was read.
const supervisorUnconfigured = "supervisor_unconfigured"

// supervisorSectionKey is where the supervisor section lives in the configuration document: the
// supervisor key of the manage object.
const supervisorSectionKey = "manage.supervisor"

// supervisorSource is the --source the pair record carries. It names the command that wrote it, so
// a later reader can tell a record this command made from one another surface made.
const supervisorSource = "crw manage supervisor register"

// supervisorExitRefused is the status of a run the relay refused. The refusal itself is the relay's
// own stdout, passed through unchanged.
const supervisorExitRefused = 1

// supervisorExitHost is the status of a run the relay helper could not carry out at all: the state
// directory could not be resolved, or the relay could not be started.
const supervisorExitHost = 3

// supervisorHostValuesNote is what a run that passes a host value on the command line is told:
// every host value comes from the supervisor section of the configuration, so the command line
// cannot carry one.
const supervisorHostValuesNote = "host values come from the supervisor section of the configuration"

// supervisorSection is the configuration document's supervisor section: the management thread, the
// host it runs on, its working directory and the settings file the pair record cites. Every value
// is a host fact and comes from the configuration only, so no private path, thread id or host name
// is written into this repository.
type supervisorSection struct {
	TaskID       string `json:"task_id"`
	HostID       string `json:"host_id"`
	Cwd          string `json:"cwd"`
	SettingsFile string `json:"settings_file"`
}

// supervisorConfig is the configuration a run reads: the package's own configuration, the manage
// object of the crw configuration file (CRW_CONFIG, else crw/config.json below the configuration
// home), whose supervisor key is the section. It is a variable so a test can supply a section
// without writing a file; the file path is pinned through the real loader as well
// (supervisor_config_file_test.go). A host with no file or no section reads every value as empty,
// which is the specified refusal rather than a failure.
var supervisorConfig = func(e *Env) *Config { return coreDefaults(e) }

// supervisorReport is crw manage supervisor show: the recorded settings and the recorded store
// binding as one JSON object. Both are the relay's own bytes, spliced in unchanged, and a binding
// that is not recorded is null. settings is the settings-show answer whole (which carries the
// recorded settings under its own settings key), and binding is the store level's owner out of the
// linkage-up answer.
type supervisorReport struct {
	OK       bool            `json:"ok"`
	TaskID   string          `json:"taskId"`
	Settings json.RawMessage `json:"settings"`
	Binding  json.RawMessage `json:"binding"`
	Refusals json.RawMessage `json:"refusals"`
}

// supervisorRecord is crw manage supervisor register: the two relay answers, spliced in unchanged.
// binding is the linkage-bind answer and settings is the settings-record answer whole.
type supervisorRecord struct {
	OK       bool            `json:"ok"`
	TaskID   string          `json:"taskId"`
	Binding  json.RawMessage `json:"binding"`
	Settings json.RawMessage `json:"settings"`
}

// supervisorRefusal is crw manage supervisor show's refusal of a binding it will not report: the
// reason (binding_ambiguous or binding_state_unknown) and the relay's linkage-up answer whole, so
// the state and the contention a caller reads are the relay's own bytes.
type supervisorRefusal struct {
	OK     bool            `json:"ok"`
	Reason string          `json:"reason"`
	Detail json.RawMessage `json:"detail"`
}

const (
	supervisorUsage         = "usage: crw manage supervisor {register,show} ..."
	supervisorRegisterUsage = "usage: crw manage supervisor register [--task THREAD] [--host HOST]"
	supervisorShowUsage     = "usage: crw manage supervisor show [--task THREAD]"
)

var supervisorCommand = Command{
	Name:    "supervisor",
	Summary: "bind the calling management thread as its own management seat and read the pair back",
	Run:     supervisorRun,
}

func init() { Register(supervisorCommand) }

// supervisorRun is crw manage supervisor.
func supervisorRun(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		supervisorNote(e, supervisorUsage+"\n")
		supervisorNote(e, "crw manage supervisor: error: the following arguments are required: command\n")
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		return supervisorUsageWrite(e, supervisorUsage,
			"  register\tbind the calling management thread (--task, else CODEX_THREAD_ID, else the configured one) as its own management seat and record its pair",
			"  show\t\tprint the recorded binding and the recorded settings")
	case "register":
		return supervisorRegister(ctx, e, args[1:])
	case "show":
		return supervisorShow(ctx, e, args[1:])
	}
	supervisorNote(e, supervisorUsage+"\n")
	supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: invalid command %q (choose from 'register', 'show')\n", args[0]))
	return usageExit
}

// supervisorSectionOf reads the supervisor section of cfg. A section the document does not carry
// leaves every value empty and reports no error, which supervisorConfigured then refuses.
func supervisorSectionOf(e *Env, cfg *Config) (supervisorSection, int) {
	var section supervisorSection
	if err := cfg.Section("supervisor", &section); err != nil {
		supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: the supervisor section (%s) is not readable: %v\n", supervisorSectionKey, err))
		return section, usageExit
	}
	return section, 0
}

// supervisorSeat is the thread and host a run binds: the command line first (--task, --host), then
// the calling thread (CODEX_THREAD_ID) for the task, then the configuration's supervisor section,
// which stays the fallback for both (CRW-1333). The section's cwd and settings file are kept.
func supervisorSeat(e *Env, section supervisorSection, task, host string) supervisorSection {
	seat := section
	switch {
	case task != "":
		seat.TaskID = task
	case e.Getenv != nil && strings.TrimSpace(e.Getenv("CODEX_THREAD_ID")) != "":
		seat.TaskID = strings.TrimSpace(e.Getenv("CODEX_THREAD_ID"))
	}
	if host != "" {
		seat.HostID = host
	}
	return seat
}

// supervisorConfigured refuses a run whose seat leaves a value it needs empty, before any relay
// call. Every run needs the thread; register also needs the host (the binding) and the settings file
// (the pair record cites it), while show reads by thread alone. Every missing key is named in one
// refusal, with where the thread may come from when it is the one missing and the configuration file
// the run read, or that it read none and where one is looked for, so the operator learns in one run
// what to write where.
func supervisorConfigured(e *Env, cfg *Config, seat supervisorSection, register bool) bool {
	var missing []string
	if seat.TaskID == "" {
		missing = append(missing, "task_id")
	}
	if register && seat.HostID == "" {
		missing = append(missing, "host_id")
	}
	if register && seat.SettingsFile == "" {
		missing = append(missing, "settings_file")
	}
	if len(missing) == 0 {
		return true
	}
	keys := make([]string, len(missing))
	for i, key := range missing {
		keys[i] = supervisorSectionKey + "." + key
	}
	supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: %s: the configuration sets no %s\n", supervisorUnconfigured, strings.Join(keys, ", ")))
	if seat.TaskID == "" {
		supervisorNote(e, fmt.Sprintf("crw manage supervisor: the management thread is --task, else CODEX_THREAD_ID, else %s.task_id\n", supervisorSectionKey))
	}
	supervisorNote(e, "crw manage supervisor: "+supervisorConfigWhere(cfg)+"\n")
	return false
}

// supervisorConfigWhere says which configuration file a run read, or that it read none and where one
// is looked for. A path with no file there is named, since that is the place a file would be read.
func supervisorConfigWhere(cfg *Config) string {
	if cfg != nil && cfg.loaded {
		return fmt.Sprintf("the configuration file this run read is %s; the values go in its %s object", cfg.path, supervisorSectionKey)
	}
	where := "no configuration file was read"
	if cfg != nil && cfg.path != "" {
		where += fmt.Sprintf(" (there is none at %s)", cfg.path)
	}
	return where + "; a file is looked for at CRW_CONFIG, else crw/config.json below ${XDG_CONFIG_HOME:-$HOME/.config}, " +
		"and crw manage config [--config FILE] reports the file a run reads"
}

// supervisorHostValueOption is one command-line option a run may no longer pass: every host value
// is a host fact and comes from the configuration, so a command line that carries one is refused
// before the relay is called rather than silently overriding the configuration.
var supervisorHostValueOption = map[string]bool{"--cwd": true, "--settings-file": true}

// supervisorHostValueOptionNamed reports the removed option an argument carries, in either the
// separate form ("--cwd" with its value in the next argument) or the assigned form ("--cwd=DIR").
// Both are the same host value on the command line, so both get the same refusal and the same note
// rather than the assigned form falling through to the generic unexpected-argument message.
func supervisorHostValueOptionNamed(arg string) (string, bool) {
	name := arg
	if i := strings.IndexByte(arg, '='); i >= 0 {
		name = arg[:i]
	}
	if supervisorHostValueOption[name] {
		return name, true
	}
	return "", false
}

// supervisorRegisterArgs reads register's own arguments, and the order is the contract: an option
// that names a host value is refused first, with the note that says where host values come from,
// even when -h or --help is present; then a help request prints the usage and stops the run there;
// then --task and --host (separate or assigned form) name the seat's thread and host; anything left
// is an unexpected argument. Every refusal happens here, before any relay call, so a run that passes
// a removed option neither binds nor records anything. help is reported separately from the status,
// because a run that asked for the usage must not fall through to the bind: the caller returns
// before the configuration is even read.
func supervisorRegisterArgs(e *Env, args []string) (task, host string, help bool, code int) {
	for _, arg := range args {
		if _, removed := supervisorHostValueOptionNamed(arg); removed {
			supervisorNote(e, supervisorRegisterUsage+"\n")
			supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: %s\n", supervisorHostValuesNote))
			return "", "", false, usageExit
		}
	}
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return "", "", true, supervisorUsageWrite(e, supervisorRegisterUsage)
		}
	}
	code = supervisorValueArgs(e, supervisorRegisterUsage, args, map[string]*string{"--task": &task, "--host": &host})
	return task, host, false, code
}

// supervisorValueArgs reads options that each carry one value, in the separate ("--task T") or the
// assigned ("--task=T") form, into values. An option it does not know, an option with no value or a
// value that is blank is an unexpected argument, refused with the usage before any relay call.
func supervisorValueArgs(e *Env, usage string, args []string, values map[string]*string) int {
	for i := 0; i < len(args); i++ {
		name, value, assigned := strings.Cut(args[i], "=")
		target, known := values[name]
		if known && !assigned && i+1 < len(args) {
			i++
			value, assigned = args[i], true
		}
		if !known || !assigned || strings.TrimSpace(value) == "" {
			supervisorNote(e, usage+"\n")
			supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: unexpected argument %q\n", args[i]))
			return usageExit
		}
		*target = strings.TrimSpace(value)
	}
	return 0
}

// supervisorShowArgs reads show's own arguments: a help request prints the usage and stops the run,
// then --task names the thread to read. show reads by thread alone, so it takes no host.
func supervisorShowArgs(e *Env, args []string) (task string, help bool, code int) {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return "", true, supervisorUsageWrite(e, supervisorShowUsage)
		}
	}
	code = supervisorValueArgs(e, supervisorShowUsage, args, map[string]*string{"--task": &task})
	return task, false, code
}

// supervisorRegister is crw manage supervisor register: the calling thread's management seat first
// (linkage-bind --role supervisor --scope-kind management --scope <thread>; a thread that inherited
// a seat by handover is answered with that seat, and a thread that held the store seat lays it down
// with a record), the settings pair second (CRW-1333). A refused bind is reported as the relay's own
// refusal and the pair is not recorded.
func supervisorRegister(ctx context.Context, e *Env, args []string) int {
	task, host, help, code := supervisorRegisterArgs(e, args)
	if help || code != 0 {
		return code
	}
	cfg := supervisorConfig(e)
	section, code := supervisorSectionOf(e, cfg)
	if code != 0 {
		return code
	}
	section = supervisorSeat(e, section, task, host)
	if !supervisorConfigured(e, cfg, section, true) {
		return usageExit
	}
	bind := []string{"linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", section.TaskID,
		"--task", section.TaskID, "--host", section.HostID}
	if section.Cwd != "" {
		bind = append(bind, "--cwd", section.Cwd)
	}
	bound, code, err := e.Relay(ctx, cfg, bind...)
	if err != nil {
		return supervisorRelayFailure(e, err)
	}
	if code != 0 {
		return supervisorRelayStatus(e, bound, code)
	}
	record := []string{"settings-record", "--task", section.TaskID, "--role", "supervisor",
		"--settings", "@" + section.SettingsFile, "--source", supervisorSource}
	recorded, code, err := e.Relay(ctx, cfg, record...)
	if err != nil {
		return supervisorRelayFailure(e, err)
	}
	if code != 0 {
		// The bind stands and the pair does not. Re-running register converges: the seat keeps one
		// live owner, so the bind is a no-op and only the pair is written.
		supervisorNote(e, "crw manage supervisor: the management seat was recorded and the pair was not; re-running register completes the pair\n")
		return supervisorRelayStatus(e, recorded, code)
	}
	boundValue, ok := supervisorAnswer(bound)
	if !ok {
		return supervisorUnreadableAnswer(e, "linkage-bind", bound)
	}
	supervisorWarnStaleCwd(e, boundValue, section.Cwd)
	recordedValue, ok := supervisorAnswer(recorded)
	if !ok {
		return supervisorUnreadableAnswer(e, "settings-record", recorded)
	}
	if err := supervisorWrite(e.Stdout, supervisorRecord{OK: true, TaskID: section.TaskID,
		Binding: boundValue, Settings: recordedValue}); err != nil {
		return supervisorWriteFailure(e, err)
	}
	return 0
}

// supervisorWarnStaleCwd names a binding that kept a working directory other than the one this run
// asked for. The relay treats a live same-host binding for the same task as already present and
// applies no update, so a re-registration with a new cwd succeeds without moving the endpoint; the
// binding is still the recorded seat, so this is reported rather than refused, and the caller sees
// the binding's own cwd in the answer. A binding whose cwd matches, or a run that named no cwd, is
// silent.
func supervisorWarnStaleCwd(e *Env, binding json.RawMessage, wanted string) {
	if wanted == "" {
		return
	}
	var record struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(binding, &record); err != nil {
		return
	}
	if record.Cwd != "" && record.Cwd != wanted {
		supervisorNote(e, fmt.Sprintf("crw manage supervisor: the live binding kept cwd %q and this run asked for %q; the seat keeps one owner and does not move its endpoint, so the binding still names %q\n",
			record.Cwd, wanted, record.Cwd))
	}
}

// supervisorShow is crw manage supervisor show: the recorded settings and the recorded store
// binding as one JSON object, for the thread --task names, else the calling thread
// (CODEX_THREAD_ID), else the section's task_id. Both reads take the thread alone, so show needs no
// host and no settings file. A relay read that fails is passed through with the relay's own stdout
// and its own exit status, so a refusal (exit 2) stays distinguishable from a host failure (exit 3).
func supervisorShow(ctx context.Context, e *Env, args []string) int {
	task, help, code := supervisorShowArgs(e, args)
	if help || code != 0 {
		return code
	}
	cfg := supervisorConfig(e)
	section, code := supervisorSectionOf(e, cfg)
	if code != 0 {
		return code
	}
	section = supervisorSeat(e, section, task, "")
	if !supervisorConfigured(e, cfg, section, false) {
		return usageExit
	}
	settings, code, err := e.Relay(ctx, cfg, "settings-show", "--task", section.TaskID)
	if err != nil {
		return supervisorRelayFailure(e, err)
	}
	if code != 0 {
		return supervisorRelayStatus(e, settings, code)
	}
	linkage, code, err := e.Relay(ctx, cfg, "linkage-up", "--task", section.TaskID)
	if err != nil {
		return supervisorRelayFailure(e, err)
	}
	if code != 0 {
		return supervisorRelayStatus(e, linkage, code)
	}
	settingsValue, ok := supervisorAnswer(settings)
	if !ok {
		return supervisorUnreadableAnswer(e, "settings-show", settings)
	}
	answer, ok := supervisorLinkageOf(linkage)
	if !ok {
		// A store that could not be read is not a store with no binding: the reader answers
		// readable:false and exits 0, so reporting null here would claim the store said there is
		// nothing. Pass the answer through as a host failure instead.
		return supervisorPassThrough(e, linkage, supervisorExitHost)
	}
	if answer.State != "resolved" && answer.State != "unregistered" {
		// A walk that contends is not an absence, and a state this command does not know is not
		// one either: both are refused with the relay's own state and contention in detail, so a
		// caller never reads a contested seat as an unbound one.
		reason := "binding_state_unknown"
		if answer.State == "ambiguous" || len(supervisorLiveContention(answer.Contention)) > 0 {
			reason = "binding_ambiguous"
		}
		return supervisorRefuse(e, reason, linkage)
	}
	if len(supervisorLiveContention(answer.Contention)) > 0 {
		// A resolved or unregistered walk that carries a live conflict is still contested: the
		// relay leaves the state resolved only when the contention it found is not one of the two
		// competing ones, so the live entries are what decides this, not the state label.
		return supervisorRefuse(e, "binding_ambiguous", linkage)
	}
	if err := supervisorWrite(e.Stdout, supervisorReport{OK: true, TaskID: section.TaskID,
		Settings: settingsValue, Binding: supervisorStoreBinding(answer.Levels),
		Refusals: supervisorRefusalRecords(answer.Contention)}); err != nil {
		return supervisorWriteFailure(e, err)
	}
	return 0
}

// supervisorLevel is one level of a linkage-up answer.
type supervisorLevel struct {
	ScopeKind string          `json:"scopeKind"`
	ScopeKey  string          `json:"scopeKey"`
	Owner     json.RawMessage `json:"owner"`
}

// supervisorLinkage is the linkage-up answer as this command reads it: the state the walk settled
// on, whether the store could be read at all, the levels it holds and the contention it found.
// Reading the state and the contention is the point: an answer whose state is ambiguous, or which
// carries a live contention entry, is a contested binding rather than an absent one, and the levels
// of such an answer are empty. The contention array also carries past refusal records, which are
// not conflicts; supervisorContentionIsLive tells the two apart.
type supervisorLinkage struct {
	State      string            `json:"state"`
	Readable   bool              `json:"readable"`
	Levels     []supervisorLevel `json:"levels"`
	Contention []json.RawMessage `json:"contention"`
}

// supervisorContentionIsLive reports whether one contention entry is a conflict the relay found
// now rather than a past refusal record. The relay's own reading is truthiness of the entry's
// "contention" key (internal/relay/delivery/service.go ResolveRecipient with its truthy helper):
// an entry with a non-empty "contention" value is live, and one without the key, with an empty
// string, or with any other falsy value is not. A live entry names one of competing_owners,
// competing_parents, instruction_conflict, owner_drift or scope_cycle; a past refusal record from
// the linkage_conflicts table carries no such key and is kept forever, so treating one of those as
// live would make show fail for good after a single refused competitor. An entry this command
// cannot read at all is not a live conflict it can name, so it is read as a refusal and rides
// along rather than being dropped.
func supervisorContentionIsLive(entry json.RawMessage) bool {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(entry, &item); err != nil {
		return false
	}
	value, ok := item["contention"]
	if !ok {
		return false
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	return supervisorTruthy(decoded)
}

// supervisorTruthy is the relay's truthy() over a decoded JSON value
// (internal/relay/delivery/transport.go): nil, false, "", an empty object, an empty array and zero
// are false, and everything else is true. It is the same test the relay applies to a contention
// entry's "contention" key, so this command reads live contention exactly as the relay does.
func supervisorTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	case float64:
		return typed != 0
	}
	return true
}

// supervisorLiveContention is the entries of a linkage-up answer's contention array that are a live
// conflict, in the relay's own order.
func supervisorLiveContention(contention []json.RawMessage) []json.RawMessage {
	var live []json.RawMessage
	for _, entry := range contention {
		if supervisorContentionIsLive(entry) {
			live = append(live, entry)
		}
	}
	return live
}

// supervisorRefusalRecords is the entries of a linkage-up answer's contention array that are past
// refusal records rather than live conflicts, in the relay's own order. They are not a failure, so
// they are reported rather than refused, and the array is always present (empty when there are
// none).
func supervisorRefusalRecords(contention []json.RawMessage) json.RawMessage {
	records := []json.RawMessage{}
	for _, entry := range contention {
		if !supervisorContentionIsLive(entry) {
			records = append(records, entry)
		}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return json.RawMessage("[]")
	}
	return json.RawMessage(data)
}

// supervisorLinkageOf reads the linkage-up answer. ok is false when the answer is not JSON or says
// the store could not be read (readable:false), which is a failure rather than an absence.
func supervisorLinkageOf(linkage []byte) (supervisorLinkage, bool) {
	var answer supervisorLinkage
	if err := json.Unmarshal(linkage, &answer); err != nil {
		return supervisorLinkage{}, false
	}
	return answer, answer.Readable
}

// supervisorStoreBinding is the supervisor seat binding out of a linkage-up answer's levels: the
// owner of the level whose scope is the store seat or a management seat (CRW-1333). A task with no
// such level is null, because a store that answered and holds no binding for the task has answered.
func supervisorStoreBinding(levels []supervisorLevel) json.RawMessage {
	for _, level := range levels {
		seat := level.ScopeKind == "store" && level.ScopeKey == "store" || level.ScopeKind == "management"
		if seat && len(level.Owner) > 0 && string(level.Owner) != "null" {
			return level.Owner
		}
	}
	return json.RawMessage("null")
}

// supervisorAnswer is a relay answer the report embeds, spliced in unchanged. ok is false when the
// answer is empty or not one JSON value, which is an unreadable answer rather than an absence: a
// report must not turn a command that answered nothing usable into a success.
func supervisorAnswer(data []byte) (json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return nil, false
	}
	return json.RawMessage(trimmed), true
}

// supervisorRelayStatus is what a relay command that ran and failed means here: the relay's own
// refusal is this command's refusal, and any other failure is a host failure, so a caller can tell
// a contested seat from a store it could not read. The relay's own stdout is passed through either
// way, and a settings-record refusal after a successful bind is not rewritten.
func supervisorRelayStatus(e *Env, stdout []byte, code int) int {
	if code == contract.ExitRefused {
		return supervisorPassThrough(e, stdout, supervisorExitRefused)
	}
	return supervisorPassThrough(e, stdout, supervisorExitHost)
}

// supervisorUnreadableAnswer reports a relay command that exited 0 with an answer this command
// cannot read, as a host failure, keeping the bytes for the caller.
func supervisorUnreadableAnswer(e *Env, command string, stdout []byte) int {
	supervisorNote(e, fmt.Sprintf("crw manage supervisor: the %s answer is not one JSON value\n", command))
	return supervisorPassThrough(e, stdout, supervisorExitHost)
}

// supervisorPassThrough writes the relay's own stdout unchanged, so a caller sees the relay's
// refusal rather than a rewritten one, and returns the exit status the run ends with. A write that
// fails is a failure of the run rather than of the pass-through: the answer never left, so the
// command reports it and ends with exit 1 instead of the status it was about to return.
func supervisorPassThrough(e *Env, stdout []byte, code int) int {
	if len(stdout) == 0 {
		return code
	}
	if err := supervisorOutputWrite(e.Stdout, stdout); err != nil {
		return supervisorWriteFailure(e, err)
	}
	return code
}

// supervisorRefuse writes this command's own refusal: the reason and the relay's linkage-up answer
// whole, so the state and the contention a caller reads are the relay's own bytes rather than a
// rewrite of them.
func supervisorRefuse(e *Env, reason string, linkage []byte) int {
	detail, ok := supervisorAnswer(linkage)
	if !ok {
		detail = json.RawMessage("null")
	}
	if err := supervisorWrite(e.Stdout, supervisorRefusal{OK: false, Reason: reason, Detail: detail}); err != nil {
		return supervisorWriteFailure(e, err)
	}
	return supervisorExitRefused
}

// supervisorUsageWrite writes the usage lines a help request prints and reports a write that failed,
// so a help request whose usage never reached the caller ends with exit 1 rather than reading as a
// success. The message names the usage rather than the report: the JSON report this command writes
// is a different output and keeps its own failure message (supervisorWriteFailure).
func supervisorUsageWrite(e *Env, lines ...string) int {
	for _, line := range lines {
		if err := supervisorOutputWrite(e.Stdout, []byte(line+"\n")); err != nil {
			supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: write the usage: %v\n", err))
			return 1
		}
	}
	return 0
}

// supervisorWrite writes one JSON value and a newline, and reports a write that failed so a command
// whose report never left can end with a failure rather than a silent success.
func supervisorWrite(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		// The value could not be encoded, so what is written is the encode failure itself: the
		// caller must still learn that its report was not the one it meant to send, so the error is
		// returned whether or not that fallback line reached the writer.
		if werr := supervisorOutputWrite(w, []byte(fmt.Sprintf("{\"ok\":false,\"reason\":\"encode_failed\",\"detail\":%q}\n", err.Error()))); werr != nil {
			return werr
		}
		return err
	}
	return supervisorOutputWrite(w, append(data, '\n'))
}

// supervisorOutputWrite is the one place this command's stdout output is written, so a caller can
// tell a report that left from one that did not. os.File.Write on fd 1 or 2 turns an EPIPE into a
// fatal runtime SIGPIPE (os/file_unix.go epipecheck), so a run whose stdout is a pipe nobody reads
// would die on a signal before any error branch could report it; a direct syscall keeps EPIPE an
// ordinary error, the way internal/relay/job/envelope.go writeHookOutput does for the hook output.
// Any other writer, and any file on another descriptor, is written with io.Writer as before. The
// process's signal policy is left alone either way.
func supervisorOutputWrite(w io.Writer, body []byte) error {
	if file, ok := w.(*os.File); ok && file.Fd() <= 2 {
		fd := int(file.Fd())
		for len(body) > 0 {
			n, err := syscall.Write(fd, body)
			if n > 0 {
				body = body[n:]
			}
			if err == syscall.EINTR {
				continue
			}
			if err != nil {
				return err
			}
			if n == 0 {
				// A write of nothing that reported no error would spin here, so it is reported as
				// the failure it is rather than retried forever.
				return io.ErrShortWrite
			}
		}
		return nil
	}
	_, err := w.Write(body)
	return err
}

// supervisorNote writes one diagnostic to stderr. A diagnostic is best-effort: it must never take
// the run's exit status with it, so a stderr that cannot be written to is not reported again and
// the caller still returns the status it decided on. It goes through supervisorOutputWrite for the
// same reason the report does, and this matters when a shell merges the streams ("cmd 2>&1 | ..."):
// os.File.Write on fd 2 would raise SIGPIPE and end the process before the caller could return the
// exit 1 the failure path promises.
func supervisorNote(e *Env, text string) {
	_ = supervisorOutputWrite(e.Stderr, []byte(text))
}

// supervisorWriteFailure reports a report that could not be written, as a failure of the run: the
// caller sees the write failure on stderr and the run ends with exit 1.
func supervisorWriteFailure(e *Env, err error) int {
	supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: write output: %v\n", err))
	return 1
}

// supervisorRelayFailure reports a relay command the helper could not carry out at all, as a host
// failure (exit 3). A command that ran and was refused is not this: its own status is passed
// through instead.
func supervisorRelayFailure(e *Env, err error) int {
	supervisorNote(e, fmt.Sprintf("crw manage supervisor: error: %v\n", err))
	return supervisorExitHost
}
