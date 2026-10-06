package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// supervisorUnconfigured is the refusal of a run whose configuration names no management thread or
// no host. The relay is never called: there is nothing to bind and nothing to look up.
const supervisorUnconfigured = "supervisor_unconfigured"

// supervisorSource is the --source the pair record carries. It names the command that wrote it, so
// a later reader can tell a record this command made from one another surface made.
const supervisorSource = "crw manage supervisor register"

// supervisorExitRefused is the status of a run the relay refused. The refusal itself is the relay's
// own stdout, passed through unchanged.
const supervisorExitRefused = 1

// supervisorExitHost is the status of a run the relay helper could not carry out at all: the state
// directory could not be resolved, or the relay could not be started.
const supervisorExitHost = 3

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

// supervisorConfig is the configuration a run reads. It is a variable so a test can supply the
// supervisor section before the configuration-file loader lands; the command otherwise reads the
// package's own configuration, whose document carries no section at this baseline. A host with no
// section therefore reads every value as empty, which is the specified refusal rather than a
// failure.
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
}

// supervisorRecord is crw manage supervisor register: the two relay answers, spliced in unchanged.
// binding is the linkage-bind answer and settings is the settings-record answer whole.
type supervisorRecord struct {
	OK       bool            `json:"ok"`
	TaskID   string          `json:"taskId"`
	Binding  json.RawMessage `json:"binding"`
	Settings json.RawMessage `json:"settings"`
}

const (
	supervisorUsage         = "usage: crw manage supervisor {register,show} ..."
	supervisorRegisterUsage = "usage: crw manage supervisor register [--cwd DIR] [--settings-file FILE]"
	supervisorShowUsage     = "usage: crw manage supervisor show"
)

var supervisorCommand = Command{
	Name:    "supervisor",
	Summary: "bind the management thread as the store supervisor and read the pair back",
	Run:     supervisorRun,
}

func init() { Register(supervisorCommand) }

// supervisorRun is crw manage supervisor.
func supervisorRun(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, supervisorUsage)
		fmt.Fprintln(e.Stderr, "crw manage supervisor: error: the following arguments are required: command")
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, supervisorUsage)
		fmt.Fprintln(e.Stdout, "  register\tbind the management thread as the store supervisor and record its pair")
		fmt.Fprintln(e.Stdout, "  show\t\tprint the recorded binding and the recorded settings")
		return 0
	case "register":
		return supervisorRegister(ctx, e, args[1:])
	case "show":
		return supervisorShow(ctx, e, args[1:])
	}
	fmt.Fprintln(e.Stderr, supervisorUsage)
	fmt.Fprintf(e.Stderr, "crw manage supervisor: error: invalid command %q (choose from 'register', 'show')\n", args[0])
	return usageExit
}

// supervisorSectionOf reads the supervisor section of cfg. A section the document does not carry
// leaves every value empty and reports no error, which supervisorConfigured then refuses.
func supervisorSectionOf(e *Env, cfg *Config) (supervisorSection, int) {
	var section supervisorSection
	if err := cfg.Section("supervisor", &section); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage supervisor: error: the supervisor section is not readable: %v\n", err)
		return section, usageExit
	}
	return section, 0
}

// supervisorConfigured refuses a run whose configuration names no thread or no host, before any
// relay call: the pair record and the binding both need both values.
func supervisorConfigured(e *Env, section supervisorSection) bool {
	if section.TaskID != "" && section.HostID != "" {
		return true
	}
	fmt.Fprintf(e.Stderr, "crw manage supervisor: error: %s\n", supervisorUnconfigured)
	return false
}

// supervisorRegister is crw manage supervisor register: the store-scope supervisor binding first,
// the settings pair second. A refused bind is reported as the relay's own refusal and the pair is
// not recorded.
func supervisorRegister(ctx context.Context, e *Env, args []string) int {
	values, help, err := hostReadParse(args, map[string]bool{"cwd": true, "settings-file": true})
	if help {
		fmt.Fprintln(e.Stdout, supervisorRegisterUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, supervisorRegisterUsage)
		fmt.Fprintf(e.Stderr, "crw manage supervisor: error: %v\n", err)
		return usageExit
	}
	cfg := supervisorConfig(e)
	section, code := supervisorSectionOf(e, cfg)
	if code != 0 {
		return code
	}
	if values["cwd"] != "" {
		section.Cwd = values["cwd"]
	}
	if values["settings-file"] != "" {
		section.SettingsFile = values["settings-file"]
	}
	if !supervisorConfigured(e, section) {
		return usageExit
	}
	if section.SettingsFile == "" {
		fmt.Fprintln(e.Stderr, supervisorRegisterUsage)
		fmt.Fprintln(e.Stderr, "crw manage supervisor: error: the supervisor section names no settings_file, so the pair record has nothing to cite")
		return usageExit
	}
	bind := []string{"linkage-bind", "--role", "supervisor", "--scope-kind", "store", "--scope", "store",
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
		fmt.Fprintf(e.Stderr, "crw manage supervisor: the store binding was recorded and the pair was not; re-running register completes the pair\n")
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
	supervisorWrite(e.Stdout, supervisorRecord{OK: true, TaskID: section.TaskID,
		Binding: boundValue, Settings: recordedValue})
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
		fmt.Fprintf(e.Stderr, "crw manage supervisor: the live binding kept cwd %q and this run asked for %q; the seat keeps one owner and does not move its endpoint, so the binding still names %q\n",
			record.Cwd, wanted, record.Cwd)
	}
}

// supervisorShow is crw manage supervisor show: the recorded settings and the recorded store
// binding as one JSON object. A relay read that fails is passed through with the relay's own
// stdout and its own exit status, so a refusal (exit 2) stays distinguishable from a host failure
// (exit 3).
func supervisorShow(ctx context.Context, e *Env, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(e.Stderr, supervisorShowUsage)
		fmt.Fprintf(e.Stderr, "crw manage supervisor: error: unexpected argument %q\n", args[0])
		return usageExit
	}
	cfg := supervisorConfig(e)
	section, code := supervisorSectionOf(e, cfg)
	if code != 0 {
		return code
	}
	if !supervisorConfigured(e, section) {
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
	levels, ok := supervisorLevels(linkage)
	if !ok {
		// A store that could not be read is not a store with no binding: the reader answers
		// readable:false and exits 0, so reporting null here would claim the store said there is
		// nothing. Pass the answer through as a host failure instead.
		supervisorPassThrough(e, linkage)
		return supervisorExitHost
	}
	supervisorWrite(e.Stdout, supervisorReport{OK: true, TaskID: section.TaskID,
		Settings: settingsValue, Binding: supervisorStoreBinding(levels)})
	return 0
}

// supervisorLevel is one level of a linkage-up answer.
type supervisorLevel struct {
	ScopeKind string          `json:"scopeKind"`
	ScopeKey  string          `json:"scopeKey"`
	Owner     json.RawMessage `json:"owner"`
}

// supervisorLevels reads the levels of a linkage-up answer. ok is false when the answer is not JSON
// or says the store could not be read (readable:false), which is a failure rather than an absence.
func supervisorLevels(linkage []byte) ([]supervisorLevel, bool) {
	var answer struct {
		Readable bool              `json:"readable"`
		Levels   []supervisorLevel `json:"levels"`
	}
	if err := json.Unmarshal(linkage, &answer); err != nil {
		return nil, false
	}
	return answer.Levels, answer.Readable
}

// supervisorStoreBinding is the store-scope supervisor binding out of a linkage-up answer's levels:
// the owner of the level whose scope is the store seat. A task with no such level is null, because
// a store that answered and holds no binding for the task has answered.
func supervisorStoreBinding(levels []supervisorLevel) json.RawMessage {
	for _, level := range levels {
		if level.ScopeKind == "store" && level.ScopeKey == "store" && len(level.Owner) > 0 && string(level.Owner) != "null" {
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
	supervisorPassThrough(e, stdout)
	if code == contract.ExitRefused {
		return supervisorExitRefused
	}
	return supervisorExitHost
}

// supervisorUnreadableAnswer reports a relay command that exited 0 with an answer this command
// cannot read, as a host failure, keeping the bytes for the caller.
func supervisorUnreadableAnswer(e *Env, command string, stdout []byte) int {
	supervisorPassThrough(e, stdout)
	fmt.Fprintf(e.Stderr, "crw manage supervisor: the %s answer is not one JSON value\n", command)
	return supervisorExitHost
}

// supervisorPassThrough writes the relay's own stdout unchanged, so a caller sees the relay's
// refusal rather than a rewritten one.
func supervisorPassThrough(e *Env, stdout []byte) {
	if len(stdout) == 0 {
		return
	}
	if _, err := e.Stdout.Write(stdout); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage supervisor: error: %v\n", err)
	}
}

// supervisorWrite writes one JSON value and a newline.
func supervisorWrite(w io.Writer, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(w, "{\"ok\":false,\"reason\":\"encode_failed\",\"detail\":%q}\n", err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", data)
}

// supervisorRelayFailure reports a relay command the helper could not carry out at all, as a host
// failure (exit 3). A command that ran and was refused is not this: its own status is passed
// through instead.
func supervisorRelayFailure(e *Env, err error) int {
	fmt.Fprintf(e.Stderr, "crw manage supervisor: error: %v\n", err)
	return supervisorExitHost
}
