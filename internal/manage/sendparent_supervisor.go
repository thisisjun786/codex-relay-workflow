package manage

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// sendParentRoleUsage is the usage line of the role options.
const sendParentRoleUsage = "       crw manage send-parent --role supervisor --thread T --message-file F --logical-id ID [--report ID] [--initiative I] [--state DIR] [--socket PATH] [--queue]"

// supervisorReply is the one JSON object `send-parent --role supervisor` answers with. The default role answers {class, requestId,
// logicalId} and nothing else; this answer adds the role, the observation of the thread when it kept the notice in the supervisor queue,
// and why.
type supervisorReply struct {
	Class       string `json:"class"`
	RequestID   string `json:"requestId"`
	LogicalID   string `json:"logicalId"`
	Role        string `json:"role"`
	Observation string `json:"observation,omitempty"`
	Reason      string `json:"reason,omitempty"`
	// Queue is the state of the notice in the supervisor queue: queued (written now), pending, sent or refused (it was there already).
	Queue string `json:"queue,omitempty"`
}

func sendParentSupervisorReport(e *Env, r supervisorReply) int {
	r.Role = supervisorRole
	data, err := json.Marshal(r)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	switch r.Class {
	case deliverClassAccepted, sendParentClassQueued:
		return sendParentExitAccepted
	case deliverClassUnknown:
		return sendParentExitUnknown
	default:
		return sendParentExitRefused
	}
}

// sendParentSupervisor is `send-parent --role supervisor`: a notice for the initiative lead's thread. The thread is the task of its
// supervision binding, whose recorded authorization gives the settings of the send. Before anything is sent the thread's active turn is
// read (the probe the pump uses): a thread that is idle is sent the notice with the role supervisor; a thread that is busy, or that is
// not loaded (notLoaded: the delivery core sends nothing to such a thread and answers unknown, so the send is not even tried), gets the
// notice in the supervisor queue, where the pump delivers it when the thread can take it. Nothing of the parent path changes.
func sendParentSupervisor(ctx context.Context, e *Env, cfg *Config, parsed sendParentArgs, logicalID string, text []byte) int {
	// the relay the report was written in (flow report-up --state S --socket P) is the one the binding is read in and the delivery is
	// told to; the notice remembers it for the pump
	cfg = withRelay(cfg, parsed.relayState, parsed.relaySocket)
	n := supervisorNotice{LogicalID: logicalID, Thread: parsed.thread, BindingTask: parsed.thread, ReportID: parsed.report, Initiative: parsed.initiative,
		RelayState: parsed.relayState, RelaySocket: parsed.relaySocket, Text: string(text), CreatedAt: deliverNow(e)}
	queue := func(reason, observation string, loadRetry int) int {
		n.LoadRetry = loadRetry
		state, err := supervisorQueueWrite(cfg, n)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
			return sendParentExitRefused
		}
		class := sendParentClassQueued
		if state == "refused" {
			class = deliverClassRefused
		}
		return sendParentSupervisorReport(e, supervisorReply{Class: class, LogicalID: logicalID, Observation: observation, Reason: reason, Queue: state})
	}
	if parsed.queue {
		return queue("", "", 0)
	}
	if err := supervisorQueueSafe(cfg, parsed.thread, logicalID); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	// the notice may be in the queue already (an earlier call queued it): it is the pump's, and it is not sent a second time from here
	state, found, err := supervisorQueueFind(cfg, n)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	if found && (state == "pending" || state == "sent") {
		return sendParentSupervisorReport(e, supervisorReply{Class: map[string]string{"pending": sendParentClassQueued, "sent": deliverClassAccepted}[state], LogicalID: logicalID, Queue: state})
	}
	settings, err := supervisorSettingsRead(ctx, e, cfg, parsed.thread)
	if err != nil {
		// no authorization to read: nothing is sent, and the notice waits for a binding that records one
		fmt.Fprintf(e.Stderr, "crw manage send-parent: settings_unavailable: %v\n", err)
		return queue("settings_unavailable", "", 0)
	}
	observation, _, err := pumpQueueObserve(ctx, e, cfg, parsed.thread)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: the active-turn probe failed: %v\n", err)
		return queue("probe_failed", "", 0)
	}
	if observation != "idle" {
		return queue("", observation, 0)
	}
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: logicalID, Thread: parsed.thread, Text: n.Text, Role: supervisorRole, Settings: settings})
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
	}
	if supervisorUnloaded(out) {
		// the thread unloaded between the probe and the send: the bridge's refusal is the last defence, and the notice is queued with
		// the ordinal of the next attempt
		reply := supervisorReply{Class: deliverClassRefused, RequestID: out.RequestID, LogicalID: logicalID, Reason: unloadedPairCode}
		if state, qerr := supervisorQueueWrite(cfg, withLoadRetry(n, 1)); qerr != nil {
			fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", qerr)
		} else {
			reply.Queue = state
		}
		return sendParentSupervisorReport(e, reply)
	}
	class := out.Class
	if class == "" {
		class = deliverClassRefused
	}
	return sendParentSupervisorReport(e, supervisorReply{Class: class, RequestID: out.RequestID, LogicalID: logicalID})
}

func withLoadRetry(n supervisorNotice, retry int) supervisorNotice {
	n.LoadRetry = retry
	return n
}

// sendParentSummaryQueue is `send-parent --queue` for the notice of a plan summary entry: written once. The notice is pending, in a batch
// the pump pinned, in sent/ (delivered) or refused already, and the answer says which; only when the queue holds none of it is it written.
func sendParentSummaryQueue(e *Env, cfg *Config, parsed sendParentArgs, logicalID string, text []byte) int {
	if err := sendParentQueueSafe(cfg, parsed.thread, logicalID); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	state, requestID, err := summaryQueueState(cfg, parsed.thread, logicalID)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	if state == "" {
		dest := crwconfig.JoinRoot(cfg.StateDir, sendParentQueueDir, parsed.thread, logicalID+".txt")
		if err := deliverWriteAtomic(dest, text); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
			return sendParentExitRefused
		}
		state = summaryStatePending
	}
	class := sendParentClassQueued
	switch state {
	case summaryStateSent:
		class = deliverClassAccepted
	case summaryStateRefused:
		class = deliverClassRefused
	}
	data, _ := json.Marshal(map[string]any{"class": class, "requestId": requestID, "logicalId": logicalID, "state": state})
	fmt.Fprintf(e.Stdout, "%s\n", data)
	if class == deliverClassRefused {
		return sendParentExitRefused
	}
	return sendParentExitAccepted
}
