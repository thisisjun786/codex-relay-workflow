package manage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"golang.org/x/sys/unix"
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

	// The bridge's answer when it holds no record of a request id at all: the refusal happened
	// before the operation was written, so nothing was sent.
	deliverUnknownRequestID = "Unknown request_id"

	// The bridge's own input bounds: a thread longer than 128 characters or a message longer than
	// 100,000 is refused before it writes the operation, so the driver refuses it locally instead of
	// recording an attempt nobody could have made.
	deliverThreadLimit  = 128
	deliverMessageLimit = 100000
	deliverLockDir      = "locks"
)

// deliverBridge is one bridge session, started for a single delivery and closed with it.
type deliverBridge struct{ session *mcp.ClientSession }

// deliverMessageSHA256 is the digest of one message's text, the value the record keeps so a
// logical id cannot silently stand for two different messages.
func deliverMessageSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum)
}

// deliverRetryCount is the retry ordinal a request id carries, so a restart continues the
// sequence the record left instead of minting an id the bridge already answered for.
func deliverRetryCount(logicalID, requestID string) int {
	suffix, ok := strings.CutPrefix(requestID, logicalID+"-r")
	if !ok {
		return 0
	}
	// Only a plain positive ordinal counts. A logical id that itself ends in -r<n> leaves a suffix
	// this does not recognise, and reading it as an ordinal would skip retries rather than repeat
	// one, which is the harmless direction.
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 1 || suffix != strconv.Itoa(n) {
		return 0
	}
	return n
}

// deliverLock holds one logical id for the length of a delivery. Two processes that both read
// the same pending record and both find the bridge holding nothing would otherwise both send,
// so the second one is refused while the first is still working. The kernel releases the lock
// when the process ends, so a crash cannot leave it held.
func deliverLock(cfg *Config, logicalID string) (func(), error) {
	if err := deliverPathComponent(logicalID, "logical id"); err != nil {
		return nil, err
	}
	dir := crwconfig.JoinRoot(cfg.StateDir, deliverLockDir)
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("crw manage: the lock directory is a symlink; refusing to lock through it")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("crw manage: the lock directory: %w", err)
	}
	file, err := os.OpenFile(crwconfig.JoinRoot(dir, logicalID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("crw manage: the delivery lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("crw manage: another delivery of %s is in flight", logicalID)
		}
		return nil, fmt.Errorf("crw manage: lock the delivery: %w", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}, nil
}

// Deliver sends one logical message over the configured bridge and records what it settled on.
// An unknown outcome is never recorded as sent and never removed: it is reconciled with the
// bridge's own receipt before anything else, and it is never turned into a failure and resent
// under a new request id. Only not_delivered retries, under a new request id, at most twice.
//
// The order matters. The policy is checked and the record is written before the bridge process
// exists, so a process that dies at any point leaves the evidence of what it was about to do. A
// record that is already settled -- accepted or refused -- is answered from the record itself: the
// same request id stays one attempt, nothing is sent again, and a terminal refusal never decays
// into an undetermined outcome on a replay.
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
	if !utf8.ValidString(m.LogicalID) {
		// The logical id names the outbox file and travels as the request id, so a value that is
		// not valid UTF-8 would make the file name and the id the bridge sees disagree.
		return Outcome{}, errors.New("crw manage: the logical id is not valid UTF-8")
	}
	if !utf8.ValidString(m.Text) {
		// JSON carries text as UTF-8, so an invalid byte would reach the bridge as a replacement
		// character and the recipient would read a different message than the caller wrote.
		return Outcome{}, errors.New("crw manage: the message text is not valid UTF-8")
	}
	if err := deliverPathComponent(m.LogicalID, "logical id"); err != nil {
		return Outcome{}, err
	}
	// The bridge refuses these before it records an operation, so the message could never be
	// delivered and the record would only stay undetermined forever.
	if len(m.Thread) > deliverThreadLimit {
		return Outcome{Class: deliverClassRefused, RequestID: m.LogicalID},
			fmt.Errorf("crw manage: the thread is longer than %d characters", deliverThreadLimit)
	}
	if len(m.Text) > deliverMessageLimit {
		return Outcome{Class: deliverClassRefused, RequestID: m.LogicalID},
			fmt.Errorf("crw manage: the message is longer than %d characters", deliverMessageLimit)
	}
	// One logical id is delivered by one process at a time. Without this, two processes can both
	// read the same pending record, both find the bridge holds nothing, and both send.
	release, err := deliverLock(cfg, m.LogicalID)
	if err != nil {
		return Outcome{}, err
	}
	defer release()
	record, known, err := deliverLoad(cfg, m.LogicalID)
	if err != nil {
		return Outcome{}, err
	}
	if !known {
		record = deliverNewRecord(e, m)
		if err := deliverSave(cfg, record); err != nil {
			return Outcome{}, err
		}
	} else {
		// The record belongs to one logical message. A caller that reuses the id for different text or
		// a different thread is refused rather than told the other message's outcome.
		if record.TargetThread != m.Thread || record.MessageSHA256 != deliverMessageSHA256(m.Text) {
			return Outcome{Class: deliverClassRefused, RequestID: m.LogicalID},
				fmt.Errorf("crw manage: the logical id %q already belongs to another message", m.LogicalID)
		}
		// A settled record is answered from the record itself, so the same request id stays one
		// attempt and nothing is sent again. An accepted message is already dispatched; a refused
		// one is terminal, and dialling the bridge for it would only let a definitive refusal decay
		// into an undetermined outcome on every replay.
		switch record.State {
		case deliverStateAccepted:
			return Outcome{Class: deliverClassAccepted, RequestID: record.RequestID, Receipt: map[string]any{"status": "accepted", "replayed": true}}, nil
		case deliverStateRefused:
			return Outcome{Class: deliverClassRefused, RequestID: record.RequestID, Receipt: map[string]any{"status": "refused", "replayed": true}}, nil
		}
	}
	bridge, err := deliverDial(ctx, e, cfg)
	if err != nil {
		// A bridge that could not be started refused nothing: it never saw the message. The record
		// keeps the state it was written with, so a later attempt reconciles it against a bridge that
		// may by then be reachable, instead of reading a refusal the bridge never made.
		return Outcome{Class: deliverClassUnknown, RequestID: record.RequestID}, err
	}
	defer bridge.close()
	requestID := record.RequestID
	// The retry ordinal comes from the record, so a restart continues the sequence it left rather
	// than minting an id the bridge already holds a receipt for.
	retries := deliverRetryCount(m.LogicalID, record.RequestID)
	if known {
		// The record is unsettled: read the bridge's own receipt before anything is sent, so an
		// attempt that may already have gone out is never duplicated under a fresh id.
		switch verdict := deliverReconcile(ctx, bridge, record); verdict {
		case deliverReconcileAccepted:
			record.State, record.Received, record.Applied = deliverStateAccepted, true, false
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassAccepted, ReceiptExcerpt: "reconciled with get_operation"})
			return deliverSettled(cfg, record, Outcome{Class: deliverClassAccepted, RequestID: record.RequestID})
		case deliverReconcileRefused:
			// The bridge settled this id as a refusal before the dispatch, so nothing was sent and the
			// message is not sent again under it.
			record.State = deliverStateRefused
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassRefused, ReceiptExcerpt: "get_operation: refused before the dispatch"})
			return deliverSettled(cfg, record, Outcome{Class: deliverClassRefused, RequestID: record.RequestID})
		case deliverReconcileResendSame, deliverReconcileResendNew:
			// Nothing was sent. An id the bridge recorded and answered not_attempted for is reused,
			// because that makes the attempt rather than replaying an answer; an id it never recorded,
			// or one it answered not_delivered for, takes a new id under the same retry cap. The record
			// stays unsettled until that attempt settles, so a process that dies during it reconciles.
			excerpt := "get_operation: nothing was sent; resending under the same request id"
			if verdict == deliverReconcileResendNew {
				if retries >= deliverRetryLimit {
					record.State = deliverStateRefused
					record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassRefused, ReceiptExcerpt: "get_operation: nothing was sent; the retry cap is spent"})
					return deliverSettled(cfg, record, Outcome{Class: deliverClassRefused, RequestID: record.RequestID})
				}
				retries++
				requestID = deliverRetryRequestID(m.LogicalID, retries)
				record.RequestID = requestID
				excerpt = "get_operation: nothing was sent; resending under a new request id"
			}
			record.State = deliverStateUnknown
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassRefused, ReceiptExcerpt: excerpt})
			if err := deliverSave(cfg, record); err != nil {
				return Outcome{Class: deliverClassRefused, RequestID: requestID}, err
			}
		default:
			record.State = deliverStateUnknown
			record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: deliverClassUnknown, ReceiptExcerpt: "get_operation left the attempt undetermined; nothing was sent"})
			return deliverSettled(cfg, record, Outcome{Class: deliverClassUnknown, RequestID: record.RequestID})
		}
	}
	for {
		reply, tool := deliverAttemptSend(ctx, bridge, m, requestID)
		class := deliverClassify(tool, reply)
		record.Tool, record.RequestID = tool, requestID
		record.Attempts = append(record.Attempts, deliverAttempt{At: deliverNow(e), Class: class, ReceiptExcerpt: deliverExcerpt(reply)})
		switch class {
		case deliverClassAccepted:
			record.State, record.Received, record.Applied = deliverStateAccepted, true, false
			return deliverSettled(cfg, record, Outcome{Class: class, RequestID: requestID, Receipt: reply.Payload})
		case deliverClassRefused:
			// A live not_attempted receipt is a refusal under the issue's contract, and only a
			// not_delivered refusal is retried, so it settles here. A record that reconciles to
			// not_attempted is a different case and reuses its id before any send.
			if !deliverRetryable(reply) || retries >= deliverRetryLimit {
				record.State = deliverStateRefused
				return deliverSettled(cfg, record, Outcome{Class: class, RequestID: requestID, Receipt: reply.Payload})
			}
			// The retry goes out under a new request id, and the ledger names that id before the
			// retry does: a process that dies during it then reconciles the id it actually
			// attempted, instead of resending the message under the id the bridge already
			// answered not_delivered for. The record stays unsettled until that retry settles,
			// because nothing has answered for the new id yet.
			retries++
			requestID = deliverRetryRequestID(m.LogicalID, retries)
			record.RequestID, record.State = requestID, deliverStateUnknown
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

// deliverReconcileVerdict is what the bridge's own receipt for an unsettled request id settles.
type deliverReconcileVerdict int

const (
	// deliverReconcileUnsettled: the bridge said nothing this code can place, so nothing is sent.
	deliverReconcileUnsettled deliverReconcileVerdict = iota
	// deliverReconcileAccepted: the bridge holds the operation; acceptance is never application.
	deliverReconcileAccepted
	// deliverReconcileRefused: the bridge refused it before the dispatch; it is not sent again.
	deliverReconcileRefused
	// deliverReconcileResendSame: the bridge recorded the id and nothing was sent, so reusing the id
	// makes the attempt instead of replaying an answer.
	deliverReconcileResendSame
	// deliverReconcileResendNew: the bridge holds nothing under this id -- it never recorded it, or
	// it recorded that nothing was delivered -- so a new id may carry the message.
	deliverReconcileResendNew
)

// deliverReconcile reads the bridge's receipt for an unsettled record and places it on the same
// allowlist as a live reply, so an accepted, refused or undetermined answer means the same thing
// whichever tool first returned it. A lookup the bridge answers with its own "never recorded"
// verdict settles as a pre-dispatch refusal: the bridge writes the operation before any turn/start
// or turn/steer, so an id it does not hold proves nothing was sent.
func deliverReconcile(ctx context.Context, b *deliverBridge, record deliverRecord) deliverReconcileVerdict {
	reply := b.call(ctx, deliverToolOperation, map[string]any{"request_id": record.RequestID})
	if deliverUnknownRequestIDReply(reply) {
		return deliverReconcileResendNew
	}
	switch deliverClassify(deliverReceiptTool(reply.Payload), reply) {
	case deliverClassAccepted:
		return deliverReconcileAccepted
	case deliverClassRefused:
		if status, _ := reply.Payload["status"].(string); status == "not_attempted" {
			return deliverReconcileResendSame
		}
		if deliverRetryable(reply) {
			return deliverReconcileResendNew
		}
		return deliverReconcileRefused
	default:
		return deliverReconcileUnsettled
	}
}

// deliverUnknownRequestIDReply reports whether a get_operation answer is the bridge's own verdict
// that it holds no record of the request id. It matches the end of the tool error the bridge
// formats, so a longer message that merely mentions the words does not qualify.
func deliverUnknownRequestIDReply(reply deliverReply) bool {
	if reply.Err != nil || reply.Payload != nil {
		return false
	}
	text := strings.TrimSpace(reply.Text)
	if !reply.IsError && !strings.HasPrefix(text, "Error") {
		return false
	}
	return strings.HasSuffix(text, ": "+deliverUnknownRequestID)
}

// deliverReceiptTool names the tool a receipt read back from get_operation belongs to, so the
// allowlist accepts it exactly as it accepted the reply the tool first returned.
func deliverReceiptTool(payload map[string]any) string {
	if delivery, _ := payload["delivery"].(string); delivery == "accepted_not_applied" {
		return deliverToolSteer
	}
	return deliverToolSend
}

// deliverRetryRequestID is the request id a retry goes out under. The bridge reads a new id for
// the same logical message as one more attempt, which is what a not_delivered refusal and a
// refusal the bridge never recorded both leave room for.
func deliverRetryRequestID(logicalID string, retry int) string {
	return fmt.Sprintf("%s-r%d", logicalID, retry)
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
		switch {
		case observation == "active" && turnID != "":
			return b.call(ctx, deliverToolSteer, map[string]any{"request_id": requestID, "thread_id": m.Thread, "expected_turn_id": turnID, "message": m.Text}), deliverToolSteer
		case observation == "idle":
			// The only observation a turn may be started on. The bridge reports its two
			// disagreements when the status and the newest turn conflict and directs the caller to
			// read again; starting a turn then could put the message in the wrong turn.
		default:
			return deliverReply{Err: fmt.Errorf("crw manage: the thread is not confirmed idle (observation %q); nothing was sent", observation)}, deliverToolActive
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
