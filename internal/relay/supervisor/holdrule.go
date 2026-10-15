package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// AcknowledgeCommand is the name an acknowledgement's supervisor_message_superseded journal row
// gives, under the detail key "by", as the one who parked the message. A superseded hold whose newest
// such row names it is an acknowledged park; any other (or none) is a hold the channel derived.
const AcknowledgeCommand = "supervisor-acknowledge"

// holdKind is which hold of the rule a message carries. The rule governs the hold word
// superseded_by_report; its kind is read from the message and its journal (holdKindOf), never stored
// apart.
type holdKind string

const (
	holdNone         holdKind = "none"         // no hold
	holdOther        holdKind = "other"        // another hold word (hierarchy_unresolved, a cap word): its own release rule
	holdDerived      holdKind = "derived"      // superseded_by_report the channel set itself (holdSuperseded, holdObsoleteClaim)
	holdAcknowledged holdKind = "acknowledged" // superseded_by_report supervisor-acknowledge set
	holdNotice       holdKind = "notice"       // superseded_by_report on a fault notice (its park, or one derived at its send)
)

// holdEntry is the path that asks the rule.
type holdEntry string

const (
	entryAcknowledge holdEntry = "acknowledge" // supervisor-acknowledge parks a message
	entryNoticePark  holdEntry = "notice_park" // the fault deliverer parks its notice (NoticeChannel.Park)
	entryDerive      holdEntry = "derive"      // the channel finds a claimed message obsolete (holdSuperseded, holdObsoleteClaim)
	entryDefer       holdEntry = "defer"       // the channel defers a message it read with no hold (deferMessage, deferBusy)
	entryRestage     holdEntry = "restage"     // the message's obligation is staged again: by hand, by the daemon's pass or attempt, by its direct sender, or a notice's reservation
	entryReaddress   holdEntry = "readdress"   // staging rewrites the message's endpoints to what the live hierarchy names
	entrySendByID    holdEntry = "send_by_id"  // an attempt of this message named by id (supervisor-send --message, the fault deliverer's send of its notice)
	entryClaim       holdEntry = "claim"       // the channel claims the message for a send
	entrySettle      holdEntry = "settle"      // an attempt settles the message (sent, withheld, deferred under a cap)
)

// holdChange is what the asking path knows about the message against what would be staged now.
type holdChange struct {
	Entry holdEntry
	// SameStatement: what would be staged now says what the message says (statementKey equal). Seat,
	// scope, sender and observation time are not part of a statement.
	SameStatement bool
	// SameRecipient: the live hierarchy names the recipient the message is addressed to.
	SameRecipient bool
	// SameSender: the live hierarchy names the sender session the message carries.
	SameSender bool
	// SetWord is the hold word a defer or a settle asks to write, "" for none.
	SetWord string
}

type holdVerdict string

const (
	holdSet     holdVerdict = "set"     // write the hold the entry asks for (for defer and settle: SetWord)
	holdKeep    holdVerdict = "keep"    // write nothing to the hold
	holdRelease holdVerdict = "release" // clear the hold
	holdRefuse  holdVerdict = "refuse"  // the entry does not go on: it writes nothing
)

const (
	// acknowledgedKeptReason is the answer for an acknowledgement's park that a staging of the same
	// statement, or a re-address of the sender only, keeps.
	acknowledgedKeptReason = "this fact is already staged and its recipient's acknowledgement parked it; staging the same statement again leaves it parked"
	// sendByIDDriftReason is the answer for a send by id whose endpoints the live hierarchy moved: a
	// send by id never re-addresses a message.
	sendByIDDriftReason = "relation_owner_drift: the hierarchy no longer names this message's sender and recipient, and a send by id does not re-address it; stage it again (crw relay supervisor-stage), then send it"
)

// holdDecision is the hold rule of the supervisor channel (CRW-1500): the one place that answers
// whether a hold is set, kept or released, and whether a path that must not touch a held message goes
// on. Every path that writes hold_reason asks it and writes only what it answers.
//
//	entry                          | none          | other   | derived | acknowledged                 | notice
//	acknowledge, notice_park, derive | set         | refuse  | refuse  | refuse                       | refuse
//	defer                          | set SetWord   | refuse  | refuse  | refuse                       | refuse
//	claim                          | keep          | refuse  | refuse  | refuse                       | refuse
//	settle                         | set SetWord, keep when "" | keep | keep | keep                    | keep
//	readdress, recipient moved     | keep          | release | release | release                      | release
//	readdress, sender moved only   | keep          | release | release | keep                         | release
//	restage, recipient moved       | keep          | keep    | release | release                      | release
//	restage, same recipient        | keep          | keep    | ifOwed  | SameStatement ? keep : ifOwed | release
//	send_by_id, an endpoint moved  | keep (relation_owner_drift: stage it again)
//	send_by_id, same endpoints     | keep          | keep    | ifOwed  | ifOwed                       | ifOwed
//
// ifOwed releases when owed answers true and keeps when it answers false; an error keeps the hold and
// is returned. owed is asked only in an ifOwed cell, and may restate the message (refreshProposal),
// as the release it allows would. A readdress that moved neither endpoint is answered as a sender-only
// move. An entry or a kind the rule does not know is refused with an error.
func holdDecision(held holdKind, change holdChange, owed func() (bool, error)) (holdVerdict, string, error) {
	switch held {
	case holdNone, holdOther, holdDerived, holdAcknowledged, holdNotice:
	default:
		return holdRefuse, "", fmt.Errorf("hold rule: unknown hold kind %q", held)
	}
	ifOwed := func(why string) (holdVerdict, string, error) {
		if owed == nil {
			return holdKeep, "", fmt.Errorf("hold rule: %s asks whether the message is owed, and no answer was given", change.Entry)
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
	switch change.Entry {
	case entryAcknowledge, entryNoticePark, entryDerive:
		if held == holdNone {
			return holdSet, "the message carries no hold", nil
		}
		return holdRefuse, "the message already carries a hold", nil
	case entryDefer:
		if held == holdNone {
			return holdSet, "the message carries no hold, so it is deferred", nil
		}
		return holdRefuse, "the message carries a hold, so it is not deferred", nil
	case entryClaim:
		if held == holdNone {
			return holdKeep, "the message carries no hold, so it is claimable", nil
		}
		return holdRefuse, "the message carries a hold, so it is not claimable", nil
	case entrySettle:
		if held == holdNone {
			if change.SetWord == "" {
				return holdKeep, "the message carries no hold and the settle asks for none", nil
			}
			return holdSet, "the message carries no hold, so the settle writes the one it asks for", nil
		}
		return holdKeep, "the message already carries a hold, and a settle keeps it", nil
	case entryReaddress:
		if held == holdNone {
			return holdKeep, "the message carries no hold", nil
		}
		if change.SameRecipient && held == holdAcknowledged {
			return holdKeep, acknowledgedKeptReason, nil
		}
		if change.SameRecipient {
			return holdRelease, "the hierarchy names another sender session, so the message is re-addressed", nil
		}
		return holdRelease, "the hierarchy names another recipient, so the message is re-addressed", nil
	case entryRestage:
		switch {
		case held == holdNone:
			return holdKeep, "the message carries no hold", nil
		case held == holdOther:
			return holdKeep, "the hold is not superseded_by_report; its own rule releases it", nil
		case !change.SameRecipient:
			return holdRelease, "the hierarchy names another recipient, so the message is re-addressed", nil
		}
		switch held {
		case holdDerived:
			return ifOwed("what the message is for is owed through it again")
		case holdAcknowledged:
			if change.SameStatement {
				return holdKeep, acknowledgedKeptReason, nil
			}
			return ifOwed("a new statement of what the message is for")
		}
		return holdRelease, "its notification is reserved again", nil
	case entrySendByID:
		if !change.SameRecipient || !change.SameSender {
			return holdKeep, sendByIDDriftReason, nil
		}
		switch held {
		case holdNone:
			return holdKeep, "the message carries no hold", nil
		case holdOther:
			return holdKeep, "the hold is not superseded_by_report; its own rule releases it", nil
		case holdNotice:
			return ifOwed("the notice is sent by its deliverer")
		}
		return ifOwed("the message is sent by id")
	}
	return holdRefuse, "", fmt.Errorf("hold rule: unknown entry %q", change.Entry)
}

// holdKindOf is the kind of the hold a stored message carries: from its hold word, its obligation
// kind and the newest supervisor_message_superseded journal row of it. That row is the one the write
// setting the current hold journaled, because superseded_by_report is set only from no hold and each
// such set journals exactly one row (a release journals supervisor_message_reopened). The row names
// AcknowledgeCommand under "by" for an acknowledgement's park; a missing row, or one without that key
// (every row an older build wrote), reads as a derived hold. It reads through s.Q(ctx), so inside a
// transaction it reads that transaction.
func holdKindOf(ctx context.Context, s *store.Store, row store.SupervisorMessagesRow) (holdKind, error) {
	hold := ""
	if row.HoldReason.Valid {
		hold = row.HoldReason.String
	}
	switch {
	case hold == "":
		return holdNone, nil
	case hold != store.SupervisorHoldSuperseded:
		return holdOther, nil
	case row.ObligationKind == store.SupervisorNoticeObligationKind:
		return holdNotice, nil
	}
	var detail sql.NullString
	err := s.Q(ctx).QueryRowContext(ctx, "SELECT detail FROM journal WHERE kind='supervisor_message_superseded' AND subject=? ORDER BY seq DESC LIMIT 1", row.MessageID).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return holdDerived, nil
	}
	if err != nil {
		return "", err
	}
	var parked struct {
		By string `json:"by"`
	}
	if json.Unmarshal([]byte(detail.String), &parked) == nil && parked.By == AcknowledgeCommand {
		return holdAcknowledged, nil
	}
	return holdDerived, nil
}
