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
// This file is the classification and the outbox ledger only. The bridge session and the
// delivery function that drives them live in deliver_send.go (CRW-795).
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
)

// Message is one logical message to deliver: the id that identifies it across attempts, the
// thread it goes to, its text, the role the recipient is resumed under, and the settings a
// resume states.
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

// deliverRecord is one logical message's outbox record. It is written before the send, so a
// process that dies between the write and the call leaves evidence of the attempt.
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
// the bridge records an undetermined outcome without one, and an error beside it is still
// undetermined.
func deliverClassify(tool string, reply deliverReply) string {
	if reply.Err != nil {
		return deliverClassUnknown
	}
	if reply.IsError || strings.HasPrefix(strings.TrimSpace(reply.Text), "Error") {
		return deliverClassRefused
	}
	if reply.Payload == nil {
		return deliverClassUnknown
	}
	delivery, _ := reply.Payload["delivery"].(string)
	status, _ := reply.Payload["status"].(string)
	switch {
	case delivery == "not_delivered" || delivery == "rejected":
		return deliverClassRefused
	case status == "refused" || status == "rejected" || status == "not_attempted":
		return deliverClassRefused
	case tool == deliverToolSend && delivery == "turn_started":
		return deliverClassAccepted
	case tool == deliverToolSteer && delivery == "accepted_not_applied":
		return deliverClassAccepted
	default:
		return deliverClassUnknown
	}
}

// deliverRetryable is the one refusal that may be sent again under a new request id: no turn
// left the process, so nothing on the host is holding the message.
func deliverRetryable(reply deliverReply) bool {
	delivery, _ := reply.Payload["delivery"].(string)
	return reply.Err == nil && delivery == "not_delivered"
}

// deliverExcerpt is the minimum reconciliation evidence kept with an attempt. A cut lands on a
// character boundary, so a receipt holding a multi-byte character is kept whole rather than left
// with a broken rune that would read as corruption in the ledger.
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

// deliverNewRecord is the record written before the first send. The request id is chosen here
// and does not change for this logical message except under the retry rule.
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

// deliverPathComponent refuses a name that would not stay a single component under the state
// directory. The logical id names the outbox file and the thread names the queue directory, and
// both arrive from a command line, so a separator or a parent reference in either one would let
// a delivery write outside the state directory. Everything this code derives itself (a sha256
// prefix, or a logical id plus the -r<n> retry suffix) is a safe component already.
func deliverPathComponent(name, what string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("crw manage: %s %q is not a usable name", what, name)
	}
	if strings.ContainsRune(name, 0x2f) || strings.ContainsRune(name, 0x5c) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("crw manage: %s %q must not contain a path separator", what, name)
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

// deliverSave writes one record atomically, so a reader never sees half of it and a crash leaves
// either the old record or the new one. The logical id is checked here rather than left to the
// caller, so no record is written for a name that would not stay one component.
func deliverSave(cfg *Config, record deliverRecord) error {
	if err := deliverPathComponent(record.LogicalID, "logical id"); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("crw manage: encode the outbox record: %w", err)
	}
	return deliverWriteAtomic(deliverOutboxPath(cfg, record.LogicalID), append(raw, 0x0a))
}

// deliverWriteAtomic writes data to path through a temporary file in the same directory, synced
// before the rename that commits it.
func deliverWriteAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("crw manage: the outbox directory: %w", err)
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("crw manage: the outbox temporary file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("crw manage: write the outbox record: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("crw manage: sync the outbox record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("crw manage: close the outbox record: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("crw manage: commit the outbox record: %w", err)
	}
	return nil
}
