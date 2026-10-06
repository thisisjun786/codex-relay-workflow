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

// childCheckMatch is one expected/actual comparison.
type childCheckMatch struct {
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
	Match    bool   `json:"match"`
}

// childCheckReport is what crw manage child-check writes.
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

// childCheckDisabled is the servers to compare: the ones named on the command line, else the
// disabled_servers of the configuration's child_check section.
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
	return section.DisabledServers, nil
}

// childCheck reads the thread's model, effort and status and the per-server MCP status, and
// compares them with what was asked. A notLoaded thread never matches: the child is not loaded.
func childCheck(ctx context.Context, cfg *Config, opts childCheckOptions) (*childCheckReport, error) {
	disabled, err := childCheckDisabled(cfg, opts.disabled)
	if err != nil {
		return nil, err
	}
	raw, err := HostRead(ctx, cfg, "thread/read", map[string]any{"threadId": opts.thread, "includeTurns": false})
	if err != nil {
		return nil, err
	}
	// Field names match the JSON keys case-insensitively, so no tags are needed here.
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
	report.OK = report.Model.Match && report.Effort.Match && report.ThreadStatus != "notLoaded" &&
		len(report.NotDisabled) == 0 && len(report.Missing) == 0
	return report, nil
}

// childCheckServerStatus reads the host's per-server MCP status for the thread, keyed by name.
func childCheckServerStatus(ctx context.Context, cfg *Config, thread string) (map[string]string, error) {
	raw, err := HostRead(ctx, cfg, "mcpServerStatus/list", map[string]any{"threadId": thread, "detail": "toolsAndAuthOnly", "limit": 500})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Data []struct{ Name, RuntimeStatus string }
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, &hostReadError{reason: hostReadHostError, detail: "mcpServerStatus/list result: " + err.Error()}
	}
	status := map[string]string{}
	for _, row := range answer.Data {
		status[row.Name] = row.RuntimeStatus
	}
	return status, nil
}

var childCheckCommand = Command{Name: "child-check", Summary: "compare a child thread's model, effort and MCP servers", Run: childCheckRunCommand}

func init() { Register(childCheckCommand) }

// childCheckRunCommand is crw manage child-check: exit 0 when everything matches, 1 when anything
// differs, and 3 when a read fails.
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
		reason, detail := hostReadReason(err)
		hostReadWrite(e.Stdout, hostReadEnvelope{Reason: reason, Detail: detail})
		return 3
	}
	hostReadWrite(e.Stdout, report)
	if !report.OK {
		return 1
	}
	return 0
}
