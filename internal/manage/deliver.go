package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// The bridge's response contract, allowlisted: every value these names do not carry is unknown,
// because a receipt this code cannot place is a delivery nobody has evidence for. Acceptance is
// dispatch and never application, so an accepted record says received and never applied.
//
// This file is the classification and the outbox ledger only. The bridge session and the delivery
// function that drives them live in deliver_send.go (CRW-795).
const (
	deliverClassAccepted = "accepted"
	deliverClassRefused  = "refused"
	deliverClassUnknown  = "unknown"

	deliverStatePending  = "pending"
	deliverStateAccepted = "accepted"
	deliverStateRefused  = "refused"
	deliverStateUnknown  = "unknown"

	deliverToolSend  = "send_message_to_thread"
	deliverToolSteer = "steer_thread"

	deliverExcerptLimit = 600

	// The bridge's ledger refuses a longer request id, and a retry appends a suffix to the
	// logical id, so a logical id must leave that room.
	deliverRequestIDLimit  = 128
	deliverRetrySuffixRoom = 3
)

// Message is one logical message to deliver.
type Message struct {
	LogicalID string
	Thread    string
	Text      string
	Role      string
	Settings  coreSettings
}

// Outcome is what a delivery settled on: its class, the request id it stands under, and the
// receipt it was read from.
type Outcome struct {
	Class     string
	RequestID string
	Receipt   map[string]any
}

// deliverRecord is one logical message's outbox record, written before the send so a process that
// dies between the write and the call leaves evidence of the attempt.
type deliverRecord struct {
	LogicalID     string           `json:"logical_id"`
	RequestID     string           `json:"request_id"`
	Tool          string           `json:"tool"`
	TargetThread  string           `json:"target_thread"`
	MessageSHA256 string           `json:"message_sha256"`
	CreatedAt     string           `json:"created_at"`
	State         string           `json:"state"`
	Received      bool             `json:"received"`
	Applied       bool             `json:"applied"`
	Attempts      []deliverAttempt `json:"attempts"`
}

// deliverAttempt is one classified try at a request id.
type deliverAttempt struct {
	At             string `json:"at"`
	Class          string `json:"class"`
	ReceiptExcerpt string `json:"receipt_excerpt"`
}

// deliverReply is one tool call's answer: a transport failure, a tool error, or a receipt.
type deliverReply struct {
	Text    string
	Payload map[string]any
	IsError bool
	Err     error
}

// deliverClassify places one reply on the allowlist. Refused is read before accepted so a
// contradictory receipt fails closed, and an error member alone never makes a receipt refused:
// the bridge records an undetermined outcome without one, so an error beside it is still
// undetermined.
func deliverClassify(tool string, reply deliverReply) string {
	if reply.Err != nil {
		return deliverClassUnknown
	}
	// An isError result is not a refusal. The bridge writes the operation with Ledger.Begin before
	// any turn/start or turn/steer and the final receipt with Ledger.Save afterwards, so a failure
	// while saving that receipt arrives as an isError result after the host already took the
	// message. Validation and lookup errors come before Begin and are told apart by get_operation
	// on the request id, never by this text.
	if reply.IsError || strings.HasPrefix(strings.TrimSpace(reply.Text), "Error") {
		return deliverClassUnknown
	}
	if reply.Payload == nil {
		return deliverClassUnknown
	}
	delivery, _ := reply.Payload["delivery"].(string)
	status, _ := reply.Payload["status"].(string)
	// Uncertainty is read before acceptance: an undetermined status stays unknown whatever the
	// delivery field claims, because a receipt this code cannot place is a delivery nobody has
	// evidence for.
	if status == "outcome_unknown" || status == "in_progress_or_unknown" || delivery == "outcome_unknown" {
		return deliverClassUnknown
	}
	switch {
	case delivery == "not_delivered" || delivery == "rejected":
		return deliverClassRefused
	case status == "refused" || status == "rejected" || status == "not_attempted":
		return deliverClassRefused
	case deliverRefusalCodes[deliverRPCErrorCode(reply.Payload)]:
		return deliverClassRefused
	case tool == deliverToolSend && status == "accepted" && delivery == "turn_started":
		return deliverClassAccepted
	case tool == deliverToolSteer && status == "accepted" && delivery == "accepted_not_applied":
		return deliverClassAccepted
	default:
		return deliverClassUnknown
	}
}

// deliverRefusalCodes are the bridge's definitive no-delivery failures: each settles as status
// failed with an rpcError code and no delivery field, and each means the instruction landed
// nowhere, so the sender may re-read the thread and choose the now-valid delivery path.
//
// steered_turn_mismatch is deliberately absent: a turn took that steer, just not the guarded one,
// so the instruction may have landed and the outcome stays unknown.
var deliverRefusalCodes = map[string]bool{
	"thread_idle":            true,
	"thread_not_loaded":      true,
	"thread_system_error":    true,
	"expected_turn_mismatch": true,
}

// deliverRPCErrorCode is the rpcError code a failed receipt carries, or the empty string.
func deliverRPCErrorCode(payload map[string]any) string {
	rpc, _ := payload["rpcError"].(map[string]any)
	code, _ := rpc["code"].(string)
	return code
}

// deliverRetryable is the one refusal that may be sent again under a new request id: no turn left
// the process, so nothing on the host is holding the message.
func deliverRetryable(reply deliverReply) bool {
	delivery, _ := reply.Payload["delivery"].(string)
	return reply.Err == nil && delivery == "not_delivered"
}

// deliverExcerpt is the minimum reconciliation evidence kept with an attempt. A cut lands on a
// character boundary, so a receipt holding a multi-byte character is not left with a broken rune.
func deliverExcerpt(reply deliverReply) string {
	text := reply.Text
	if reply.Err != nil {
		text = reply.Err.Error()
	}
	if len(text) <= deliverExcerptLimit {
		return text
	}
	cut := deliverExcerptLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// deliverNewRecord is the record written before the first send. The request id is chosen here and
// does not change for this logical message except under the retry rule.
func deliverNewRecord(e *Env, m Message) deliverRecord {
	sum := sha256.Sum256([]byte(m.Text))
	return deliverRecord{
		LogicalID:     m.LogicalID,
		RequestID:     m.LogicalID,
		TargetThread:  m.Thread,
		MessageSHA256: hex.EncodeToString(sum[:]),
		CreatedAt:     deliverNow(e),
		State:         deliverStatePending,
	}
}

// deliverNow is the clock a record is stamped with.
func deliverNow(e *Env) string {
	now := time.Now
	if e != nil && e.Now != nil {
		now = e.Now
	}
	return now().UTC().Format(time.RFC3339)
}

// deliverPathComponent refuses a name that would not stay one component under the state directory.
// The logical id names the outbox file and the thread names the queue directory, both arriving
// from a command line, so a separator, a parent reference or an over-long name would either
// escape the state directory or make the retry id the bridge refuses.
func deliverPathComponent(name, what string) error {
	return deliverPathComponentLimit(name, what, deliverRequestIDLimit-deliverRetrySuffixRoom)
}

// deliverPathComponentLimit is the same check against a caller's own length bound, for a name that
// is never used as a retry id.
func deliverPathComponentLimit(name, what string, limit int) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("crw manage: %s %q is not a usable name", what, name)
	}
	if strings.ContainsRune(name, 0x2f) || strings.ContainsRune(name, 0x5c) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("crw manage: %s %q must not contain a path separator", what, name)
	}
	if len(name) > limit {
		return fmt.Errorf("crw manage: %s %q is longer than %d characters", what, name, limit)
	}
	return nil
}

// deliverOutboxPath is where one logical message's record lives.
func deliverOutboxPath(cfg *Config, logicalID string) string {
	return filepath.Join(cfg.StateDir, "outbox", logicalID+".json")
}

// deliverLoad reads one record. A record that is not there is not an error: it is the first
// attempt at that logical message.
func deliverLoad(cfg *Config, logicalID string) (deliverRecord, bool, error) {
	if err := deliverPathComponent(logicalID, "logical id"); err != nil {
		return deliverRecord{}, false, err
	}
	raw, err := os.ReadFile(deliverOutboxPath(cfg, logicalID))
	if err != nil {
		if os.IsNotExist(err) {
			return deliverRecord{}, false, nil
		}
		return deliverRecord{}, false, fmt.Errorf("crw manage: read the outbox record: %w", err)
	}
	var record deliverRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return deliverRecord{}, false, fmt.Errorf("crw manage: decode the outbox record of %s: %w", logicalID, err)
	}
	return record, true, nil
}

// deliverSave writes one record atomically. The logical id is checked here rather than left to the
// caller, so no record is written for a name that would not stay one component.
func deliverSave(cfg *Config, record deliverRecord) error {
	if err := deliverPathComponent(record.LogicalID, "logical id"); err != nil {
		return err
	}
	if err := deliverOutboxDirSafe(cfg); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("crw manage: encode the outbox record: %w", err)
	}
	return deliverWriteAtomic(deliverOutboxPath(cfg, record.LogicalID), append(raw, 0x0a))
}

// deliverWriteAtomic writes data to path through a private temporary file in the same directory,
// synced before the rename that commits it, then syncs the directory so the entry is durable. A
// private name per writer matters because two processes can save one logical message at once: a
// shared temporary path would let one rename the inode the other still has open.
func deliverWriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("crw manage: the outbox directory: %w", err)
	}
	file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("crw manage: the outbox temporary file: %w", err)
	}
	temporary := file.Name()
	fail := func(step string, err error) error {
		file.Close()
		os.Remove(temporary)
		return fmt.Errorf("crw manage: %s: %w", step, err)
	}
	if _, err := file.Write(data); err != nil {
		return fail("write the outbox record", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync the outbox record", err)
	}
	if err := file.Close(); err != nil {
		return fail("close the outbox record", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fail("commit the outbox record", err)
	}
	return deliverSyncDir(dir)
}

// deliverSyncDir makes a directory entry durable. Syncing the record alone is not enough: until
// the directory is synced the rename can be lost, and a message that was sent would read as one
// that was never attempted.
func deliverSyncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("crw manage: open the outbox directory: %w", err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("crw manage: sync the outbox directory: %w", err)
	}
	return nil
}

// deliverOutboxDirSafe refuses an outbox directory that is a symlink, so a link planted in its
// place cannot redirect a ledger write outside the state directory.
func deliverOutboxDirSafe(cfg *Config) error {
	info, err := os.Lstat(filepath.Join(cfg.StateDir, "outbox"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("crw manage: read the outbox directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("crw manage: the outbox directory is a symlink; refusing to write through it")
	}
	return nil
}
