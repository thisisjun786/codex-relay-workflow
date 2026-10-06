package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// childCheckOptions is the comparison crw manage child-check is asked to make.
type childCheckOptions struct {
	thread, model, effort string
	disabled              []string
}

// childCheckServersUnset is the refusal of a comparison with nothing to compare: no
// --disabled and no configured list. It is reported before any socket read, and a run that
// compares nothing never reports ok.
const childCheckServersUnset hostReadRefusal = "disabled_servers_unset"

// childCheckUsableStatuses are the thread statuses ok may be reported for: only a thread
// the host holds idle or active can be checked. notLoaded, systemError, an empty status and
// any other value are mismatches, so a run that could not observe the thread never reports
// ok.
var childCheckUsableStatuses = map[string]bool{"idle": true, "active": true}

// childCheckServersUnsetError is the refusal of a comparison with nothing to compare: no
// --disabled and no configured list. It is a type rather than a sentinel variable because
// every package-level name this issue adds starts with childCheck.
type childCheckServersUnsetError struct{}

func (childCheckServersUnsetError) Error() string { return string(childCheckServersUnset) }

type childCheckMatch struct {
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Match    bool   `json:"match"`
}

type childCheckReport struct {
	OK           bool              `json:"ok"`
	Thread       string            `json:"thread"`
	ThreadStatus string            `json:"threadStatus"`
	Model        childCheckMatch   `json:"model"`
	Effort       childCheckMatch   `json:"effort"`
	Servers      map[string]string `json:"servers"`
	NotDisabled  []string          `json:"notDisabled"`
	Missing      []string          `json:"missing"`
}

// childCheckDisabled is the servers to compare: the named ones, else the config section's.
func childCheckDisabled(cfg *Config, named []string) ([]string, error) {
	if len(named) > 0 {
		return named, nil
	}
	var section struct {
		DisabledServers []string `json:"disabled_servers"`
	}
	if err := cfg.Section("child_check", &section); err != nil {
		return nil, err
	}
	if len(section.DisabledServers) == 0 {
		return nil, childCheckServersUnsetError{}
	}
	return section.DisabledServers, nil
}

// childCheck compares the thread's model, effort, status and servers with what was asked.
func childCheck(ctx context.Context, cfg *Config, opts childCheckOptions) (*childCheckReport, error) {
	disabled, err := childCheckDisabled(cfg, opts.disabled)
	if err != nil {
		return nil, err
	}
	raw, err := HostRead(ctx, cfg, "thread/read", map[string]any{"threadId": opts.thread, "includeTurns": false})
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
		return nil, &hostReadError{reason: hostReadHostError, detail: "thread/read result: " + err.Error()}
	}
	status, err := childCheckServerStatus(ctx, cfg, opts.thread)
	if err != nil {
		return nil, err
	}
	report := &childCheckReport{
		Thread: opts.thread, ThreadStatus: read.Thread.Status.Type, Servers: map[string]string{},
		Model:       childCheckMatch{opts.model, read.Thread.Model, opts.model == read.Thread.Model},
		Effort:      childCheckMatch{opts.effort, read.Thread.ReasoningEffort, opts.effort == read.Thread.ReasoningEffort},
		NotDisabled: []string{}, Missing: []string{},
	}
	for _, name := range disabled {
		state, listed := status[name]
		report.Servers[name] = state
		switch {
		case !listed:
			report.Missing = append(report.Missing, name)
		case state != "disabled":
			report.NotDisabled = append(report.NotDisabled, name)
		}
	}
	report.OK = report.Model.Match && report.Effort.Match && report.ThreadStatus != "" &&
		childCheckUsableStatuses[report.ThreadStatus] && len(report.NotDisabled) == 0 && len(report.Missing) == 0
	return report, nil
}

// childCheckServerStatus reads the host's per-server MCP status, keyed by name; a paged answer is refused.
func childCheckServerStatus(ctx context.Context, cfg *Config, thread string) (map[string]string, error) {
	raw, err := HostRead(ctx, cfg, "mcpServerStatus/list", map[string]any{"threadId": thread, "detail": "toolsAndAuthOnly", "limit": 500})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data       []struct{ Name, RuntimeStatus string }
		NextCursor any
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, &hostReadError{reason: hostReadHostError, detail: "mcpServerStatus/list result: " + err.Error()}
	}
	// Any non-nil, non-empty cursor means paged, the reading internal/bridge makes of the answer.
	if answer.NextCursor != nil && answer.NextCursor != "" {
		return nil, &hostReadError{reason: hostReadHostError, detail: "mcpServerStatus/list answer is paged; the server list is incomplete"}
	}
	status := map[string]string{}
	for _, row := range answer.Data {
		status[row.Name] = row.RuntimeStatus
	}
	return status, nil
}

var childCheckCommand = Command{Name: "child-check", Summary: "compare a child thread's model, effort and MCP servers", Run: childCheckRunCommand}

func init() { Register(childCheckCommand) }

// childCheckRunCommand is crw manage child-check: exit 0 all match, 1 any difference, 3 a read failure.
func childCheckRunCommand(ctx context.Context, e *Env, args []string) int {
	const usage = "usage: crw manage child-check --thread T --model M --effort E [--disabled a,b,c]"
	values, help, err := hostReadParse(args, map[string]bool{"thread": true, "model": true, "effort": true, "disabled": true})
	if help {
		fmt.Fprintln(e.Stdout, usage)
		return 0
	}
	if err == nil && (values["thread"] == "" || values["model"] == "" || values["effort"] == "") {
		err = errors.New("--thread, --model and --effort are required")
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, usage)
		fmt.Fprintf(e.Stderr, "crw manage child-check: error: %v\n", err)
		return usageExit
	}
	opts := childCheckOptions{thread: values["thread"], model: values["model"], effort: values["effort"]}
	for _, name := range strings.Split(values["disabled"], ",") {
		if name = strings.TrimSpace(name); name != "" {
			opts.disabled = append(opts.disabled, name)
		}
	}
	report, err := childCheck(ctx, coreDefaults(e), opts)
	if err != nil {
		var unset childCheckServersUnsetError
		if errors.As(err, &unset) {
			// Nothing to compare: refuse by name before the socket is read, so a run that
			// checked nothing never reports ok.
			if werr := hostReadWrite(e.Stdout, hostReadFailure{Reason: childCheckServersUnset}); werr != nil {
				fmt.Fprintf(e.Stderr, "crw manage child-check: error: write output: %v\n", werr)
				return 1
			}
			return usageExit
		}
		reason, detail := hostReadReason(err)
		if werr := hostReadWrite(e.Stdout, hostReadFailure{Reason: reason, Detail: detail}); werr != nil {
			fmt.Fprintf(e.Stderr, "crw manage child-check: error: write output: %v\n", werr)
			return 1
		}
		return 3
	}
	if werr := hostReadWrite(e.Stdout, report); werr != nil {
		fmt.Fprintf(e.Stderr, "crw manage child-check: error: write output: %v\n", werr)
		return 1
	}
	if !report.OK {
		return 1
	}
	return 0
}
