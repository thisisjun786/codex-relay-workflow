package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file drives one delivery over the bridge: the MCP stdio session, the get_operation
// reconciliation of an unsettled record, and the retry rule. The classification and the outbox
// ledger it writes through live in deliver.go.
const (
	deliverToolActive    = "get_active_turn"
	deliverToolOperation = "get_operation"

	// The number of times a not_delivered refusal may be sent again under a new request id.
	deliverRetryLimit = 2

	// The protocol revision the session pins: below the SDK's stateless revision, so the
	// initialize then notifications/initialized handshake this tool is specified against runs.
	deliverProtocol = "2025-11-25"

	// The bridge reads its execution policy from the environment of the process it is started as.
	deliverPolicyEnv = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

	// The refusal a delivery reports when no execution policy is configured: a bridge without one
	// authorizes nothing, so nothing is sent and no process is started.
	deliverPolicyUnconfigured = "bridge_policy_unconfigured"
)

// deliverBridge is one bridge session, started for a single delivery and closed with it.
type deliverBridge struct{ session *mcp.ClientSession }

// Deliver sends one logical message over the configured bridge and records what it settled on.
// An unknown outcome is never recorded as sent and never removed: it is reconciled with the
// bridge's own receipt before anything else, and it is never turned into a failure and resent
// under a new request id. Only not_delivered retries, under a new request id, at most twice.
//
// The order matters. The policy is checked and the record is written before the bridge process
// exists, so a process that dies at any point leaves the evidence of what it was about to do. A
// record that is already settled as accepted is answered from the record itself: the same request
// id stays one accepted attempt and nothing is sent again.
func Deliver(ctx context.Context, e *Env, cfg *Config, m Message) (Outcome, error) {
	if cfg == nil {
		cfg = coreDefaults(e)
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, fmt.Errorf("crw manage: the delivery was cancelled before it started: %w", err)
	}
	if cfg.Bridge.ExecutionPolicy == "" {
		return Outcome{Class: deliverClassRefused, RequestID: m.LogicalID},
			fmt.Errorf("crw manage: %s: bridge.execution_policy is not set", deliverPolicyUnconfigured)
	}
	if m.LogicalID == "" || m.Thread == "" || m.Text == "" {
		return Outcome{}, errors.New("crw manage: a delivery needs a logical id, a thread and a text")
	}
	if !utf8.ValidString(m.Text) {
		// JSON carries text as UTF-8, so an invalid byte would reach the bridge as a replacement
		// character and the recipient would read a different message than the caller wrote.
		return Outcome{}, errors.New("crw manage: the message text is not valid UTF-8")
	}
	if err := deliverPathComponent(m.LogicalID, "logical id"); err != nil {
		return Outcome{}, err
	}
	record, known, err := deliverLoad(cfg, m.LogicalID)
	if err != nil {
		return Outcome{}, err
	}
	if !known {
		record = deliverNewRecord(e, m)
		if err := deliverSave(cfg, record); err != nil {
			return Outcome{}, err
		}
	} else if record.State == deliverStateAccepted {
		// The logical message is already dispatched. A replay is answered from the record, so
		// the same request id stays one accepted attempt and nothing is sent again.
		return Outcome{Class: deliverClassAccepted, RequestID: record.RequestID, Receipt: map[string]any{"status": "accepted", "replayed": true}}, nil
	}
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		record.State = deliverStateRefused
		record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassRefused, ReceiptExcerpt: deliverExcerpt(deliverReply{Err: err})})
		if saveErr := deliverSave(cfg, record); saveErr != nil {
			return Outcome{Class: deliverClassRefused, RequestID: record.RequestID},
				fmt.Errorf("%w (and the ledger write failed: %v)", err, saveErr)
		}
		return Outcome{Class: deliverClassRefused, RequestID: record.RequestID}, err
	}
	defer bridge.close()
	if known {
		// The record is unsettled: read the bridge's own receipt before anything is sent, so an
		// attempt that may already have gone out is never duplicated under a fresh id.
		accepted, proceed := deliverReconcile(ctx, bridge, record.RequestID)
		switch {
		case accepted:
			record.State, record.Received, record.Applied = deliverStateAccepted, true, false
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassAccepted, ReceiptExcerpt: "reconciled with get_operation"})
			return deliverSettled(cfg, record, Outcome{Class: deliverClassAccepted, RequestID: record.RequestID})
		case !proceed:
			record.State = deliverStateUnknown
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassUnknown, ReceiptExcerpt: "get_operation left the attempt undetermined; nothing was sent"})
			return deliverSettled(cfg, record, Outcome{Class: deliverClassUnknown, RequestID: record.RequestID})
		}
	}
	requestID := record.RequestID
	for attempt := 0; ; attempt++ {
		reply, tool := deliverAttemptSend(ctx, bridge, m, requestID)
		class := deliverClassify(tool, reply)
		record.Tool, record.RequestID = tool, requestID
		record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: class, ReceiptExcerpt: deliverExcerpt(reply)})
		switch class {
		case deliverClassAccepted:
			record.State, record.Received, record.Applied = deliverStateAccepted, true, false
			return deliverSettled(cfg, record, Outcome{Class: class, RequestID: requestID, Receipt: reply.Payload})
		case deliverClassRefused:
			record.State = deliverStateRefused
			if !deliverRetryable(reply) || attempt >= deliverRetryLimit {
				return deliverSettled(cfg, record, Outcome{Class: class, RequestID: requestID, Receipt: reply.Payload})
			}
			// The retry goes out under a new request id, and the ledger names that id before the
			// retry does: a process that dies during it then reconciles the id it actually
			// attempted, instead of resending the message under the id the bridge already
			// answered not_delivered for.
			requestID = fmt.Sprintf("%s-r%d", m.LogicalID, attempt+1)
			record.RequestID = requestID
			if err := deliverSave(cfg, record); err != nil {
				return Outcome{Class: class, RequestID: requestID, Receipt: reply.Payload}, err
			}
			continue
		default:
			record.State = deliverStateUnknown
			return deliverSettled(cfg, record, Outcome{Class: deliverClassUnknown, RequestID: requestID, Receipt: reply.Payload})
		}
	}
}

// deliverSettled writes the record a settled outcome produced. A write that fails is reported
// rather than dropped: the class still describes what happened to the message, and the caller
// must know that the evidence on disk did not take the update.
func deliverSettled(cfg *Config, record deliverRecord, outcome Outcome) (Outcome, error) {
	if err := deliverSave(cfg, record); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// deliverReconcile reads the bridge's receipt for an unsettled request id. accepted is true when
// the receipt reads accepted. proceed is true only when the receipt says not_attempted, the one
// answer that makes sending again under the same request id correct. Everything else, an error
// included, leaves the attempt undetermined and sends nothing.
func deliverReconcile(ctx context.Context, b *deliverBridge, requestID string) (accepted, proceed bool) {
	reply := b.call(ctx, deliverToolOperation, map[string]any{"request_id": requestID})
	if reply.Err != nil || reply.Payload == nil {
		return false, false
	}
	status, _ := reply.Payload["status"].(string)
	delivery, _ := reply.Payload["delivery"].(string)
	switch {
	case status == "accepted" || delivery == "turn_started" || delivery == "accepted_not_applied":
		return true, false
	case status == "not_attempted":
		return false, true
	default:
		return false, false
	}
}

// deliverAttemptSend reads the thread's active turn and steers it, or starts a turn on a thread
// that is not active. The tool it used comes back with the reply, because the two carry different
// accepted receipts.
func deliverAttemptSend(ctx context.Context, b *deliverBridge, m Message, requestID string) (deliverReply, string) {
	turn := b.call(ctx, deliverToolActive, map[string]any{"thread_id": m.Thread})
	if turn.Err != nil {
		return turn, deliverToolActive
	}
	if turn.Payload != nil {
		observation, _ := turn.Payload["observation"].(string)
		turnID, _ := turn.Payload["activeTurnId"].(string)
		if observation == "active" && turnID != "" {
			return b.call(ctx, deliverToolSteer, map[string]any{"request_id": requestID, "thread_id": m.Thread, "expected_turn_id": turnID, "message": m.Text}), deliverToolSteer
		}
	}
	args := map[string]any{"request_id": requestID, "thread_id": m.Thread, "message": m.Text}
	if m.Settings.Model != "" || m.Settings.ReasoningEffort != "" {
		args["expected_settings"] = m.Settings
	}
	if m.Role != "" {
		args["role"] = m.Role
	}
	return b.call(ctx, deliverToolSend, args), deliverToolSend
}

// deliverDial starts the configured bridge and opens one MCP session over its stdio. A configured
// bridge binary is used as it stands; without one the running executable is started in its bridge
// mode. The execution policy rides in the child's environment, where the bridge reads it.
func deliverDial(ctx context.Context, e *Env, cfg *Config) (*deliverBridge, error) {
	command := cfg.Bridge.Binary
	var args []string
	if command == "" {
		command = e.Executable
		args = append(args, "bridge")
	}
	if command == "" {
		return nil, errors.New("crw manage: no bridge binary is configured and the running executable is unknown")
	}
	cmd := exec.Command(command, args...)
	cmd.Env = append(os.Environ(), deliverPolicyEnv+"="+cfg.Bridge.ExecutionPolicy)
	client := mcp.NewClient(&mcp.Implementation{Name: "crw-manage", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, &mcp.ClientSessionOptions{ProtocolVersion: deliverProtocol})
	if err != nil {
		return nil, fmt.Errorf("crw manage: start the bridge: %w", err)
	}
	return &deliverBridge{session: session}, nil
}

// call runs one tool. A call that never got an answer is returned as a transport failure, so the
// caller can tell it apart from one the bridge refused.
func (b *deliverBridge) call(ctx context.Context, tool string, args map[string]any) deliverReply {
	result, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return deliverReply{Err: err}
	}
	reply := deliverReply{IsError: result.IsError}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			reply.Text = text.Text
			break
		}
	}
	if reply.Text != "" {
		var payload map[string]any
		if err := json.Unmarshal([]byte(reply.Text), &payload); err == nil {
			reply.Payload = payload
		}
	}
	return reply
}

// close ends the session, which closes the child's input and lets it exit.
func (b *deliverBridge) close() { _ = b.session.Close() }
