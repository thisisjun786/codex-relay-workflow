package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// holdKind is which hold of the rule a message carries. The rule governs the hold word
// superseded_by_report only; its kind is read from the message and its journal, never stored apart.
type holdKind string

const (
	holdNone         holdKind = "none"         // no hold
	holdOther        holdKind = "other"        // another hold word (hierarchy_unresolved, a cap): its own release rule, not this one
	holdDerived      holdKind = "derived"      // superseded_by_report the channel set itself (holdSuperseded, holdObsoleteClaim)
	holdAcknowledged holdKind = "acknowledged" // superseded_by_report supervisor-acknowledge set (Acknowledge)
	holdNotice       holdKind = "notice"       // superseded_by_report on a fault notice (its park, or one derived at its send)
)

// holdEntry is the path that asks.
type holdEntry string

const (
	entryAcknowledge holdEntry = "acknowledge" // supervisor-acknowledge parks a message
	entryNoticePark  holdEntry = "notice_park" // the fault deliverer parks its notice (NoticeChannel.Park)
	entryDerive      holdEntry = "derive"      // the channel finds a claimed message obsolete (claim, transport start)
	entryRestage     holdEntry = "restage"     // the message's obligation is staged again: by hand, by the daemon pass, by its direct sender, or a notice's reservation
	entrySendByID    holdEntry = "send_by_id"  // an attempt of this message named by id (supervisor-send --message, the fault deliverer's send of its notice)
)

// holdChange is what the asking path knows about the message against what is staged now.
type holdChange struct {
	Entry holdEntry
	// SameStatement: what would be staged now is what the message carries: the same event and
	// submission and, for an event's obligation, the packet it composes now with the stored sender and
	// observation time is the stored packet (a work report corrected in place changes it).
	SameStatement bool
	// SameRecipient: the live hierarchy names the recipient the message is addressed to. A different
	// sender session (a rotated or replaced session above the work) with the same recipient is the same.
	SameRecipient bool
}

type holdVerdict string

const (
	holdSet     holdVerdict = "set"
	holdKeep    holdVerdict = "keep"
	holdRelease holdVerdict = "release"
	holdRefuse  holdVerdict = "refuse"
)

// holdDecision is the hold rule of the supervisor channel (CRW-1486), the one place a
// superseded_by_report hold is set, kept or released; every path that sets or clears that hold asks it
// and writes only what it answers.
//
// Who sets it. The channel sets its derived hold on a message it claimed whose obligation moved
// (entryDerive). supervisor-acknowledge parks a message (entryAcknowledge). The fault deliverer parks
// its notice (entryNoticePark). Each sets the hold only on a message that carries none, and each
// journals one row: supervisor_message_superseded (the channel's; the acknowledgement's names
// supervisor-acknowledge) or supervisor_notice_parked. The kind of a hold is read from that: a fault
// notice's is the notice kind, otherwise the newest supervisor_message_superseded row of the message
// tells an acknowledgement from a derived hold.
//
// Who releases it, and when. A message staged again (entryRestage: by hand, by the daemon's pass,
// after a restart, by its direct sender, or a fault notice reserved again) or sent by id
// (entrySendByID):
//   - any kind is released when the live hierarchy names another recipient (the message is
//     re-addressed);
//   - a derived hold is released when what the message is for is owed through it again, and kept
//     otherwise;
//   - an acknowledgement's park is kept while the statement is the same, whoever stages it (the daemon
//     pass, a repeat, another sender session, a restart); it is released by a new statement that is
//     still owed (another event or submission, a work report corrected in place so the packet differs,
//     a direct report with other text), and by a send of it by id while it is still owed;
//   - a fault notice's park is released when its notification is reserved and staged again, or by its
//     deliverer's send while it is owed.
//
// Another hold word (holdOther) is not this rule's: it is kept here, and released by its own path
// (reopenAddressed, the recipient's wake). owed is asked only where the answer depends on it, and may
// restate the message (refreshProposal), as the release it allows would.
func holdDecision(held holdKind, change holdChange, owed func() (bool, error)) (holdVerdict, string, error) {
	switch change.Entry {
	case entryAcknowledge, entryNoticePark, entryDerive:
		if held == holdNone {
			return holdSet, "the message carries no hold", nil
		}
		return holdRefuse, "the message already carries a hold", nil
	}
	if held == holdNone {
		return holdKeep, "the message carries no hold", nil
	}
	if held == holdOther {
		return holdKeep, "the hold is not superseded_by_report; its own rule releases it", nil
	}
	if !change.SameRecipient {
		return holdRelease, "the hierarchy names another recipient, so the message is re-addressed", nil
	}
	ifOwed := func(why string) (holdVerdict, string, error) {
		if owed == nil {
			return holdRelease, why, nil
		}
		ok, err := owed()
		if err != nil {
			return holdKeep, "", err
		}
		if !ok {
			return holdKeep, "what the message is for is not owed through it any more", nil
		}
		return holdRelease, why, nil
	}
	switch held {
	case holdDerived:
		return ifOwed("what the message is for is owed through it again")
	case holdAcknowledged:
		if change.Entry == entrySendByID {
			return ifOwed("the message is sent by id")
		}
		if change.SameStatement {
			return holdKeep, acknowledgedKeptReason, nil
		}
		return ifOwed("a new statement of what the message is for")
	case holdNotice:
		if change.Entry == entrySendByID {
			return ifOwed("the notice is sent by its deliverer")
		}
		return holdRelease, "its notification is reserved again", nil
	}
	return holdKeep, "", nil
}

// acknowledgedKeptReason is the answer for a park an acknowledgement set that a staging of the same
// statement keeps.
const acknowledgedKeptReason = "this fact is already staged and its recipient's acknowledgement parked it; staging the same statement again leaves it parked"

// holdKindOf is the kind of the hold a message carries: its hold word, its obligation kind, and the
// newest supervisor_message_superseded journal row of it (an acknowledgement's names
// AcknowledgeCommand). It reads the journal only.
func holdKindOf(ctx context.Context, s *store.Store, messageID, hold, obligationKind string) (holdKind, error) {
	switch {
	case hold == "":
		return holdNone, nil
	case hold != store.SupervisorHoldSuperseded:
		return holdOther, nil
	case obligationKind == store.SupervisorNoticeObligationKind:
		return holdNotice, nil
	}
	var detail string
	err := s.Q(ctx).QueryRowContext(ctx, "SELECT detail FROM journal WHERE kind='supervisor_message_superseded' AND subject=? ORDER BY rowid DESC LIMIT 1", messageID).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return holdDerived, nil
	}
	if err != nil {
		return "", err
	}
	var parked struct {
		By string `json:"by"`
	}
	if json.Unmarshal([]byte(detail), &parked) == nil && parked.By == AcknowledgeCommand {
		return holdAcknowledged, nil
	}
	return holdDerived, nil
}

// rowHoldKind is holdKindOf for a stored message row.
func (c *Channel) rowHoldKind(ctx context.Context, row store.SupervisorMessagesRow) (holdKind, error) {
	return holdKindOf(ctx, c.Store, row.MessageID, row.HoldReason.String, row.ObligationKind)
}
