package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// SupervisorSend is one notice for an initiative lead's thread.
type SupervisorSend struct {
	Thread, LogicalID, Text string
	ReportID, Initiative    string
	// Kind is the report kind, Run the run it is about, Project the project the recipient was resolved for and Subject the report's
	// one line: what the supervisor channel stages the report under (RelaySender).
	Kind, Run, Project, Subject string
}

// ParentSend is one notice for the orchestrator thread (a run's escalation, its completion digest, the summary that is owed). Queue
// writes it where the pump collects it and sends nothing: a run cannot prove that a send wakes the thread, so it queues unless it can.
type ParentSend struct {
	Thread, LogicalID, Text string
	Queue                   bool
	// Kind is the direct kind of the notice on the supervisor channel (flow_escalation, flow_completion; "" for one the channel has no
	// kind for, the summary notice), Run the run it is about and Project the project of the run's plan (RelaySender).
	Kind, Run, Project string
}

// SendResult is what `crw manage send-parent` settled on.
type SendResult struct {
	// Class is accepted, queued, refused or unknown.
	Class     string
	RequestID string
	// Observation is the state of the thread when the notice was queued instead of sent (notLoaded, active, ...).
	Observation string
	// Reason is why a notice was queued or refused (settings_unavailable, unverified_pair_for_unloaded_thread, ...).
	Reason string
	// State is where a summary notice stands in the queue (pending, in_flight, sent, refused).
	State string
	// Recipient is the thread the notice was addressed to when that is not the thread asked for: the level above the project a
	// message staged through the supervisor channel goes to (RelaySender); "" for crw manage send-parent, which sends to the thread
	// it was given.
	Recipient string
}

// Sender delivers the notices of the upward half of a run through crw manage send-parent.
type Sender interface {
	SendSupervisor(ctx context.Context, s SupervisorSend) (SendResult, error)
	SendParent(ctx context.Context, p ParentSend) (SendResult, error)
}

// ManageSender runs `crw manage send-parent`. Program is the crw executable; Run replaces the process start (tests).
type ManageSender struct {
	Program string
	// State and Socket are the relay the reports are written in, when the operator selected one (flow report-up --state S --socket P, a
	// resume of a run in that store): a supervisor notice names it, so the command reads the binding's authorization and the pump tells the
	// flow rows there. Empty: the configuration's relay.
	State, Socket string
	// Dir holds the message files; it must be absolute. Empty: tmploc's temporary directory.
	Dir string
	Run func(ctx context.Context, program string, args ...string) (stdout []byte, exit int, err error)
}

func (m ManageSender) run(ctx context.Context, text string, args []string) (SendResult, error) {
	var f *os.File
	var err error
	switch dir := m.Dir; {
	case dir == "":
		f, err = tmploc.CreateTemp("crw-notice-*.txt")
	case !filepath.IsAbs(dir):
		err = fmt.Errorf("temp_parent_not_absolute: the message file directory %q; give an absolute directory", dir)
	default:
		f, err = os.CreateTemp(dir, "crw-notice-*.txt")
	}
	if err != nil {
		return SendResult{}, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return SendResult{}, err
	}
	if err := f.Close(); err != nil {
		return SendResult{}, err
	}
	argv := append([]string{"manage", "send-parent", "--message-file", filepath.Clean(name)}, args...)
	var out []byte
	var code int
	if m.Run != nil {
		out, code, err = m.Run(ctx, m.Program, argv...)
	} else {
		cmd := exec.CommandContext(ctx, m.Program, argv...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		out = stdout.Bytes()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code, err = exit.ExitCode(), nil
			} else {
				return SendResult{}, err
			}
			if len(bytes.TrimSpace(out)) == 0 {
				return SendResult{}, fmt.Errorf("crw manage send-parent: exit %d: %s", code, strings.TrimSpace(stderr.String()))
			}
		}
	}
	if err != nil {
		return SendResult{}, err
	}
	// exit 0 (accepted or queued), 1 (refused) and 4 (unknown) all answer with the one JSON object; anything else is no answer
	var reply struct {
		Class       string `json:"class"`
		RequestID   string `json:"requestId"`
		Observation string `json:"observation"`
		Reason      string `json:"reason"`
		State       string `json:"state"`
		Queue       string `json:"queue"`
	}
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &reply); jerr != nil || reply.Class == "" {
		return SendResult{}, fmt.Errorf("crw manage send-parent: exit %d with no answer: %s", code, strings.TrimSpace(string(out)))
	}
	return SendResult{Class: reply.Class, RequestID: reply.RequestID, Observation: reply.Observation, Reason: reply.Reason, State: firstText(reply.State, reply.Queue)}, nil
}

// SendSupervisor implements Sender: send-parent --role supervisor.
func (m ManageSender) SendSupervisor(ctx context.Context, s SupervisorSend) (SendResult, error) {
	args := []string{"--role", "supervisor", "--thread", s.Thread, "--logical-id", s.LogicalID}
	if m.State != "" {
		args = append(args, "--state", m.State)
	}
	if m.Socket != "" {
		args = append(args, "--socket", m.Socket)
	}
	if s.ReportID != "" {
		args = append(args, "--report", s.ReportID)
	}
	if s.Initiative != "" {
		args = append(args, "--initiative", s.Initiative)
	}
	return m.run(ctx, s.Text, args)
}

// SendParent implements Sender: send-parent with the parent role, queued when asked.
func (m ManageSender) SendParent(ctx context.Context, p ParentSend) (SendResult, error) {
	args := []string{"--thread", p.Thread, "--logical-id", p.LogicalID}
	if p.Queue {
		args = append(args, "--queue")
	}
	return m.run(ctx, p.Text, args)
}

func firstText(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}
