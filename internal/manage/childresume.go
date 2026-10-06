package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// resumeUsage is the one line crw manage child-resume prints for its help and its refusals.
const resumeUsage = "usage: crw manage child-resume --relationship R --message-file F [--dry-run]"

// The refusal reasons child-resume reports. A refusal is the reason plus, where it has one, the
// host's or the relay's own detail, and the exit status resumeExit gives it.
const (
	resumeSettingsUnavailable   = "settings_unavailable"
	resumeDisabledServersUnset  = "disabled_servers_unset"
	resumeAssignmentUnavailable = "assignment_unavailable"
	resumeThreadActive          = "thread_active"
	resumeSettingsMismatch      = "settings_mismatch"
	resumeMCPNotDisabled        = "mcp_not_disabled"
)

// resumeSettingsModes maps the sandbox type a record carries to the mode thread/resume takes.
var resumeSettingsModes = map[string]string{"readOnly": "read-only", "workspaceWrite": "workspace-write", "dangerFullAccess": "danger-full-access"}

// resumeOptions is one run of child-resume.
type resumeOptions struct {
	relationship string
	message      string
	dryRun       bool
}

// resumeSettings is the authorized record child-resume resumes the child with.
type resumeSettings struct {
	Model                 string
	ReasoningEffort       string
	CWD                   string
	ApprovalPolicy        string
	RuntimeWorkspaceRoots []string
	Sandbox               map[string]any
}

// resumeReport is what a run writes: the identifiers and settings the child came up with, the
// servers that were verified stopped, and the admit-turn line the parent runs next.
type resumeReport struct {
	OK         bool              `json:"ok"`
	Thread     string            `json:"thread"`
	TurnID     string            `json:"turnId"`
	Generation int64             `json:"generation"`
	Model      string            `json:"model"`
	Effort     string            `json:"effort"`
	Servers    map[string]string `json:"servers"`
	AdmitTurn  string            `json:"admitTurn,omitempty"`
	DryRun     bool              `json:"dryRun,omitempty"`
}

// resumeFailure is a refusal: the reason and, where the host or the relay gave one, its detail.
type resumeFailure struct {
	Reason string
	Detail string
}

func (f *resumeFailure) Error() string {
	if f.Detail == "" {
		return f.Reason
	}
	return f.Reason + ": " + f.Detail
}

// resumeFailureDoc is how a refusal is written: one JSON object on stdout, as host-read and
// child-check report theirs.
type resumeFailureDoc struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// resumeExit is the status a refusal ends with: the recorded settings or the stopped-server list
// are what the operator must fix (2), a disagreement with the host about what the child runs is
// not something this command may resolve (4), and everything else is the relay or the host (3).
func resumeExit(f *resumeFailure) int {
	switch f.Reason {
	case resumeSettingsUnavailable, resumeDisabledServersUnset:
		return usageExit
	case resumeSettingsMismatch, resumeMCPNotDisabled:
		return 4
	default:
		return 3
	}
}

// resumeAssignment reads the child task id, the generation and the parent task id the
// relationship stands on. It is one read of the relay; a relay that ran and failed, an answer
// that is not JSON, and an answer naming no child or no generation are all
// assignment_unavailable.
func resumeAssignment(ctx context.Context, e *Env, cfg *Config, relationship string) (child, parent string, generation int64, err error) {
	stdout, code, err := e.Relay(ctx, cfg, "assignment-show", "--relationship", relationship)
	if err != nil {
		return "", "", 0, &resumeFailure{Reason: resumeAssignmentUnavailable, Detail: err.Error()}
	}
	if code != 0 {
		return "", "", 0, &resumeFailure{Reason: resumeAssignmentUnavailable, Detail: "the relay exited with status " + fmt.Sprint(code) + ": " + resumeTrim(stdout)}
	}
	var answer struct {
		ChildTaskID         string `json:"childTaskId"`
		ParentTaskID        string `json:"parentTaskId"`
		ExecutionGeneration int64  `json:"executionGeneration"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return "", "", 0, &resumeFailure{Reason: resumeAssignmentUnavailable, Detail: "the relay answer: " + err.Error()}
	}
	if answer.ChildTaskID == "" || answer.ExecutionGeneration < 1 {
		return "", "", 0, &resumeFailure{Reason: resumeAssignmentUnavailable, Detail: "the relay named no child task and no generation for this relationship"}
	}
	return answer.ChildTaskID, answer.ParentTaskID, answer.ExecutionGeneration, nil
}

// resumeRecorded reads the child's authorized settings. A relay that ran and failed, an answer
// that is not JSON, and a record missing any of the settings a resume carries are all
// settings_unavailable: the recorded settings are what the operator re-records, and a resume
// with a host default instead of one of them is what this command exists to prevent.
func resumeRecorded(ctx context.Context, e *Env, cfg *Config, child string) (*resumeSettings, error) {
	stdout, code, err := e.Relay(ctx, cfg, "settings-show", "--task", child)
	if err != nil {
		return nil, &resumeFailure{Reason: resumeSettingsUnavailable, Detail: err.Error()}
	}
	if code != 0 {
		return nil, &resumeFailure{Reason: resumeSettingsUnavailable, Detail: "the relay exited with status " + fmt.Sprint(code) + ": " + resumeTrim(stdout)}
	}
	var answer struct {
		Settings resumeSettings `json:"settings"`
	}
	if err := json.Unmarshal(stdout, &answer); err != nil {
		return nil, &resumeFailure{Reason: resumeSettingsUnavailable, Detail: "the relay answer: " + err.Error()}
	}
	settings := answer.Settings
	missing := []string{}
	for name, value := range map[string]string{
		"model": settings.Model, "reasoningEffort": settings.ReasoningEffort,
		"cwd": settings.CWD, "approvalPolicy": settings.ApprovalPolicy,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(settings.RuntimeWorkspaceRoots) == 0 {
		missing = append(missing, "runtimeWorkspaceRoots")
	}
	if settings.Sandbox == nil {
		missing = append(missing, "sandbox")
	}
	if len(missing) > 0 {
		return nil, &resumeFailure{Reason: resumeSettingsUnavailable, Detail: "the recorded settings are missing " + strings.Join(missing, ", ")}
	}
	return &settings, nil
}

// resumeSandboxMode is the thread/resume mode the recorded sandbox takes. A type this command
// cannot name is settings_unavailable, because resuming with a mode the record does not carry is
// what the issue refuses.
func resumeSandboxMode(sandbox map[string]any) (string, error) {
	kind, _ := sandbox["type"].(string)
	mode := resumeSettingsModes[kind]
	if mode == "" {
		return "", &resumeFailure{Reason: resumeSettingsUnavailable, Detail: "the recorded sandbox type " + quote.Value(sandbox["type"]) + " has no thread/resume mode"}
	}
	return mode, nil
}

// resumeMCPConfig is the config a resume carries for the servers this command stops.
func resumeMCPConfig(disabled []string) map[string]any {
	off := map[string]any{}
	for _, name := range disabled {
		off[name] = map[string]any{"enabled": false}
	}
	return off
}

// resumeRun is one child-resume: two relay reads, then one App Server connection carrying
// thread/read, thread/resume, mcpServerStatus/list and turn/start. Nothing is sent until the
// thread is not active and the settings and the servers agree with the record; --dry-run stops
// after the read and the settings comparison.
func resumeRun(ctx context.Context, e *Env, cfg *Config, opts resumeOptions) (*resumeReport, error) {
	child, parent, generation, err := resumeAssignment(ctx, e, cfg, opts.relationship)
	if err != nil {
		return nil, err
	}
	settings, err := resumeRecorded(ctx, e, cfg, child)
	if err != nil {
		return nil, err
	}
	disabled, err := childCheckDisabled(cfg, nil)
	if err != nil {
		return nil, &resumeFailure{Reason: resumeDisabledServersUnset, Detail: err.Error()}
	}
	if len(disabled) == 0 {
		return nil, &resumeFailure{Reason: resumeDisabledServersUnset, Detail: "the config section child_check names no disabled_servers"}
	}
	mode, err := resumeSandboxMode(settings.Sandbox)
	if err != nil {
		return nil, err
	}
	report := &resumeReport{OK: true, Thread: child, Generation: generation, Model: settings.Model,
		Effort: settings.ReasoningEffort, Servers: map[string]string{}, DryRun: opts.dryRun}
	client := appserver.New(cfg.Relay.Socket, appserver.DefaultBounds)
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return nil, &resumeFailure{Reason: string(hostReadUnreachable), Detail: err.Error()}
	}
	call := func(method string, params map[string]any) (json.RawMessage, error) {
		result, err := client.Call(ctx, method, params)
		if err != nil {
			return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: method + ": " + err.Error()}
		}
		return result, nil
	}
	// 1. thread/read: a thread that is active is not one this command may start a turn on.
	raw, err := call("thread/read", map[string]any{"threadId": child, "includeTurns": false})
	if err != nil {
		return nil, err
	}
	var read struct {
		Thread struct {
			Model, ReasoningEffort string
			Status                 struct{ Type string }
		}
	}
	if err := json.Unmarshal(raw, &read); err != nil {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "thread/read result: " + err.Error()}
	}
	if read.Thread.Status.Type == "active" {
		return nil, &resumeFailure{Reason: resumeThreadActive, Detail: "the thread is active; nothing was sent"}
	}
	if opts.dryRun {
		// The settings comparison the dry run is for, against what the thread reports now.
		if mismatch := resumeCompare(settings, read.Thread.Model, read.Thread.ReasoningEffort); mismatch != "" {
			return nil, &resumeFailure{Reason: resumeSettingsMismatch, Detail: mismatch + "; the dry run sent nothing"}
		}
		return report, nil
	}
	// 2. thread/resume with the recorded settings and the servers switched off.
	resumed, err := call("thread/resume", map[string]any{
		"threadId": child, "excludeTurns": true, "model": settings.Model, "cwd": settings.CWD,
		"sandbox": mode, "approvalPolicy": settings.ApprovalPolicy,
		"runtimeWorkspaceRoots": settings.RuntimeWorkspaceRoots,
		"config":                map[string]any{"model_reasoning_effort": settings.ReasoningEffort, "mcp_servers": resumeMCPConfig(disabled)},
	})
	if err != nil {
		return nil, err
	}
	var resumedSettings struct{ Model, ReasoningEffort string }
	if err := json.Unmarshal(resumed, &resumedSettings); err != nil {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "thread/resume result: " + err.Error()}
	}
	if mismatch := resumeCompare(settings, resumedSettings.Model, resumedSettings.ReasoningEffort); mismatch != "" {
		return nil, &resumeFailure{Reason: resumeSettingsMismatch, Detail: mismatch + "; no turn was started"}
	}
	// 3. mcpServerStatus/list: every server this command stopped must read as disabled.
	listed, err := call("mcpServerStatus/list", map[string]any{"threadId": child, "detail": "toolsAndAuthOnly", "limit": 500})
	if err != nil {
		return nil, err
	}
	var status struct {
		Data       []struct{ Name, RuntimeStatus string }
		NextCursor any
	}
	if err := json.Unmarshal(listed, &status); err != nil {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "mcpServerStatus/list result: " + err.Error()}
	}
	if status.NextCursor != nil && status.NextCursor != "" {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "mcpServerStatus/list answer is paged; the server list is incomplete"}
	}
	seen := map[string]string{}
	for _, row := range status.Data {
		seen[row.Name] = row.RuntimeStatus
	}
	for _, name := range disabled {
		state, ok := seen[name]
		report.Servers[name] = state
		if !ok {
			return nil, &resumeFailure{Reason: resumeMCPNotDisabled, Detail: "the host lists no server " + quote.Value(name) + "; no turn was started"}
		}
		if state != "disabled" {
			return nil, &resumeFailure{Reason: resumeMCPNotDisabled, Detail: "the server " + quote.Value(name) + " reads " + quote.Value(state) + "; no turn was started"}
		}
	}
	// 4. turn/start with the message file's text, unchanged.
	started, err := call("turn/start", map[string]any{"threadId": child,
		"input": []map[string]any{{"type": "text", "text": opts.message}}})
	if err != nil {
		return nil, err
	}
	var turn struct{ Turn struct{ ID string } }
	if err := json.Unmarshal(started, &turn); err != nil {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "turn/start result: " + err.Error()}
	}
	if turn.Turn.ID == "" {
		return nil, &resumeFailure{Reason: string(hostReadHostError), Detail: "turn/start answered with no turn id"}
	}
	report.TurnID = turn.Turn.ID
	report.AdmitTurn = resumeAdmitTurn(cfg, opts.relationship, generation, turn.Turn.ID, parent)
	return report, nil
}

// resumeCompare is the difference between the record and what the host reports, or "" when they
// agree. A host that reports nothing is a difference too: an answer that does not confirm the
// recorded model and effort is not a confirmation.
func resumeCompare(settings *resumeSettings, model, effort string) string {
	switch {
	case model != settings.Model && effort != settings.ReasoningEffort:
		return "the host reports model " + quote.Value(model) + " and effort " + quote.Value(effort) + " while the record is " + quote.Value(settings.Model) + " and " + quote.Value(settings.ReasoningEffort)
	case model != settings.Model:
		return "the host reports model " + quote.Value(model) + " while the record is " + quote.Value(settings.Model)
	case effort != settings.ReasoningEffort:
		return "the host reports effort " + quote.Value(effort) + " while the record is " + quote.Value(settings.ReasoningEffort)
	}
	return ""
}

// resumeAdmitTurn is the line the parent runs next: the relay's own admission of the turn this
// command started, with the state and the socket the relay helper resolved. A value the
// configuration did not name is left out, and the helper resolves it again.
func resumeAdmitTurn(cfg *Config, relationship string, generation int64, turn, actor string) string {
	argv := []string{"crw", "relay"}
	if cfg.Relay.State != "" {
		argv = append(argv, "--state", cfg.Relay.State)
	}
	if cfg.Relay.Socket != "" {
		argv = append(argv, "--socket", cfg.Relay.Socket)
	}
	argv = append(argv, "admit-turn", "--relationship", relationship, "--generation", fmt.Sprint(generation), "--turn", turn)
	if actor != "" {
		argv = append(argv, "--actor", actor)
	}
	quoted := make([]string, len(argv))
	for i, one := range argv {
		quoted[i] = quote.Shell(one)
	}
	return strings.Join(quoted, " ")
}

// resumeTrim is a command's output as a detail: one line, bounded, so a refusal carries what the
// command said without carrying all of it.
func resumeTrim(out []byte) string {
	text := strings.TrimSpace(string(out))
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	if len(text) > 500 {
		text = text[:500]
	}
	if text == "" {
		return "no output"
	}
	return text
}

// resumeParse reads the command line: --relationship and --message-file each take a value and
// --dry-run is a switch. Anything else, and a value that is missing, is an error.
func resumeParse(args []string) (values map[string]string, dryRun, help bool, err error) {
	values = map[string]string{}
	allowed := map[string]bool{"relationship": true, "message-file": true}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			return values, false, true, nil
		case args[i] == "--dry-run":
			dryRun = true
			continue
		}
		name := strings.TrimPrefix(args[i], "--")
		if name == args[i] || !allowed[name] {
			return nil, false, false, fmt.Errorf("unexpected argument %q", args[i])
		}
		if i+1 >= len(args) {
			return nil, false, false, fmt.Errorf("option %q needs a value", args[i])
		}
		i++
		values[name] = args[i]
	}
	return values, dryRun, false, nil
}

var resumeCommand = Command{Name: "child-resume", Summary: "bring a notLoaded child back up under its recorded settings and start one turn", Run: resumeRunCommand}

func init() { Register(resumeCommand) }

// resumeRunCommand is crw manage child-resume: 0 a turn was started (or a dry run agreed),
// 2 the command line or the record is what to fix, 3 the relay or the host, 4 a disagreement
// this command will not resolve by sending anyway.
func resumeRunCommand(ctx context.Context, e *Env, args []string) int {
	values, dryRun, help, err := resumeParse(args)
	if help {
		fmt.Fprintln(e.Stdout, resumeUsage)
		return 0
	}
	if err == nil && (values["relationship"] == "" || values["message-file"] == "") {
		err = errors.New("--relationship and --message-file are required")
	}
	var message []byte
	if err == nil {
		message, err = os.ReadFile(values["message-file"])
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, resumeUsage)
		fmt.Fprintf(e.Stderr, "crw manage child-resume: error: %v\n", err)
		return usageExit
	}
	report, err := resumeRun(ctx, e, coreDefaults(e), resumeOptions{relationship: values["relationship"], message: string(message), dryRun: dryRun})
	if err != nil {
		var failure *resumeFailure
		if errors.As(err, &failure) {
			hostReadWrite(e.Stdout, resumeFailureDoc{Reason: failure.Reason, Detail: failure.Detail})
			fmt.Fprintf(e.Stderr, "crw manage child-resume: error: %s\n", failure.Error())
			return resumeExit(failure)
		}
		fmt.Fprintf(e.Stderr, "crw manage child-resume: error: %v\n", err)
		return 1
	}
	hostReadWrite(e.Stdout, report)
	return 0
}
