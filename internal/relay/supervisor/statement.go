package supervisor

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Statement identity (CRW-1500, plan-r3 section 1). A statement is what the work says, not the
// packet bytes: a message re-addressed to a moved seat, sent by a rotated sender, scoped to a moved
// project or composed at a later clock says what it said before. The statement key reads the
// obligation's columns (obligation_id, event_id, submission_no) and the allowlisted statement fields
// of the packet. A field not on the list is routing or reinterpretation and never makes a new
// statement. The key is pure: the stored side is computed from the stored row, the fresh side from
// the packet staging would write now plus the columns it would write with it.
//
// A direct message's statement is what its sender gave (CRW-1558). The channel adds two values that
// are not the sender's: the default evidence pointer, which names the executable and the state
// directory that composed the packet, and report-up's attribution line above the sender's text. The
// packet records which values the channel generated in its origin object (composeDirect), and a row
// that carries a readable origin is compared through it (the origin reading): the digest of the
// sender's whole text stands for the body and "channel" for a generated pointer. A row staged before
// the record existed is compared the fields way on both sides (sameDirectStatement).

// statementEnvelopeFields are the envelope fields a statement keeps. sender, recipient (taskId,
// role, scopeKind), layer, scope, observedAt, evidence, relationRevision and reach are dropped.
var statementEnvelopeFields = []string{"version", "direction", "kind", "purpose", "messageId", "relationId", "subject", "basis",
	"decision", "answerOwedBy", "correlationId", "replyTo"}

// statementTopFields are the top-level packet fields a statement keeps, besides the envelope. The
// top-level evidence is kept only for a direct message (statementFields).
var statementTopFields = []string{"version", "issue", "generation", "criteriaDigest", "policy", "callback", "artifact", "body", "activation"}

// statementColumns are the columns of a supervisor_messages row a statement key reads besides the
// packet: the obligation and its kind, and the raising event and work-report submission. A new
// event or a new submission is a new statement whatever the packet says.
type statementColumns struct {
	ObligationID   string
	ObligationKind string
	EventID        sql.NullString
	SubmissionNo   sql.NullInt64
}

// rowStatementColumns are the statement columns of a stored row.
func rowStatementColumns(row store.SupervisorMessagesRow) statementColumns {
	return statementColumns{ObligationID: row.ObligationID, ObligationKind: row.ObligationKind, EventID: row.EventID, SubmissionNo: row.SubmissionNo}
}

// obligationStatementColumns are the columns StageWithReading writes for obligation o when the
// newest work report of its event is submission (0 when there is none): the event id from the
// obligation's basis, and the submission when there is one.
func obligationStatementColumns(o Obligation, submission int64) statementColumns {
	eventID, _ := o.Basis["eventId"].(string)
	columns := statementColumns{ObligationID: o.ID, ObligationKind: o.Kind, EventID: nullString(eventID)}
	if submission != 0 {
		columns.SubmissionNo = sql.NullInt64{Int64: submission, Valid: true}
	}
	return columns
}

// directStatementColumns are the columns StageDirect writes for m: its subject is the obligation,
// and a direct message has no event and no submission.
func directStatementColumns(m DirectMessage) statementColumns {
	return statementColumns{ObligationID: m.Subject, ObligationKind: m.Kind}
}

// statementFields is the statement part of a decoded packet, as a new map (the packet is not
// changed). direct says which evidence rule applies: a direct message's top-level evidence is kept as
// it is stored (the origin reading then replaces a pointer the channel generated, which names the
// executable and state directory that composed it); an obligation packet's evidence is built by the
// channel from the event id or the live project and is dropped. An envelope that is not an object is
// kept as it is, so a packet this reader does not understand is compared whole there.
func statementFields(packet map[string]any, direct bool) map[string]any {
	out := map[string]any{}
	for _, name := range statementTopFields {
		if value, ok := packet[name]; ok {
			out[name] = value
		}
	}
	if direct {
		if value, ok := packet["evidence"]; ok {
			out["evidence"] = value
		}
	}
	if envelope, ok := packet["envelope"]; ok {
		region, isObject := envelope.(map[string]any)
		if !isObject {
			out["envelope"] = envelope
		} else {
			kept := map[string]any{}
			for _, name := range statementEnvelopeFields {
				if value, ok := region[name]; ok {
					kept[name] = value
				}
			}
			out["envelope"] = kept
		}
	}
	return out
}

// statementReading is how the statement fields of a packet are read.
type statementReading int

const (
	// readingFields reads the allowlisted fields as the packet holds them.
	readingFields statementReading = iota
	// readingOrigin reads them through a direct packet's origin record: the body is the digest of
	// the sender's whole text, and the evidence is the sender's list or, when the channel generated
	// the pointer, the string "channel". A sender's evidence is always a JSON array, so the string
	// equals no sender list, not even one whose only item is the default pointer's exact text.
	readingOrigin
)

// directOrigin is the origin record of a direct packet (composeDirect).
type directOrigin struct {
	evidence string // "sender" or "channel"
	text     any    // "sha256:<hex>" of the sender's whole trimmed text, or nil
}

// readDirectOrigin is the origin record of a decoded packet, when it has a readable one: an object
// whose evidence is exactly "sender" or "channel" and whose text and preface are each null or a string.
func readDirectOrigin(packet map[string]any) (directOrigin, bool) {
	record, ok := packet["origin"].(map[string]any)
	if !ok {
		return directOrigin{}, false
	}
	evidence, _ := record["evidence"].(string)
	if evidence != "sender" && evidence != "channel" {
		return directOrigin{}, false
	}
	for _, name := range []string{"text", "preface"} {
		switch record[name].(type) {
		case nil, string:
		default:
			return directOrigin{}, false
		}
	}
	return directOrigin{evidence: evidence, text: record["text"]}, true
}

// statementKey is the statement key of a packet already decoded the way decodeStatementPacket
// decodes (objects as map[string]any, numbers as json.Number), read the way reading names: sha256
// over the canonical encoding of the obligation id, the event id, the submission and the statement
// fields. The four parts are one sorted-key JSON object, so no value can run into its neighbour.
// Which evidence rule applies is read from the obligation kind (IsDirectKind), for a stored row and a
// fresh packet alike. The origin reading refuses a packet without a readable origin.
func statementKey(columns statementColumns, decoded any, reading statementReading) (string, error) {
	packet, ok := decoded.(map[string]any)
	if !ok {
		return "", errors.New("a statement is read from a packet object, not " + pyvalue.TypeName(decoded))
	}
	var eventID, submission any
	if columns.EventID.Valid {
		eventID = columns.EventID.String
	}
	if columns.SubmissionNo.Valid {
		submission = json.Number(strconv.FormatInt(columns.SubmissionNo.Int64, 10))
	}
	fields := statementFields(packet, IsDirectKind(columns.ObligationKind))
	if reading == readingOrigin {
		origin, ok := readDirectOrigin(packet)
		if !ok {
			return "", errors.New("a statement read through its origin needs a readable origin record")
		}
		fields["body"] = origin.text
		if origin.evidence == "channel" {
			fields["evidence"] = "channel"
		}
	}
	whole := map[string]any{"obligationId": columns.ObligationID, "eventId": eventID, "submissionNo": submission,
		"statement": fields}
	encoded, err := pyjson.Encode(whole, pyjson.Options{SortKeys: true, Unicode: true, Normalize: true})
	if err != nil {
		return "", err
	}
	return pyvalue.SHA256Hex(string(encoded)), nil
}

// rowStatementKey is the statement key of a stored supervisor_messages row: its columns and the
// packet as it was stored.
func rowStatementKey(row store.SupervisorMessagesRow) (string, error) {
	return storedStatementKey(rowStatementColumns(row), row.Packet)
}

// storedStatementKey is the statement key of a stored packet encoding with the columns stored beside it.
func storedStatementKey(columns statementColumns, packet string) (string, error) {
	decoded, err := decodeStatementPacket([]byte(packet))
	if err != nil {
		return "", err
	}
	return statementKey(columns, decoded, readingFields)
}

// freshStatementKey is the statement key of a freshly composed packet with the columns staging
// would write with it. The packet is read through its JSON encoding, so a fresh []string and a
// stored array, or a fresh int64 and a stored number, are the same value.
func freshStatementKey(columns statementColumns, p Packet) (string, error) {
	decoded, err := decodeFreshPacket(p)
	if err != nil {
		return "", err
	}
	return statementKey(columns, decoded, readingFields)
}

// decodeFreshPacket is a freshly composed packet read back through its JSON encoding.
func decodeFreshPacket(p Packet) (any, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	return decodeStatementPacket(raw)
}

// directBodyMayBeCut reports whether a stored direct body may be the cut of a longer text: the cut
// keeps at most maxDirectBody bytes and moves back to a character start, so a cut body is never
// shorter than maxDirectBody less one character's continuation bytes.
func directBodyMayBeCut(packet map[string]any) bool {
	body, ok := packet["body"].(string)
	return ok && len(body) >= maxDirectBody-(utf8.UTFMax-1)
}

// sameDirectStatement reports whether the direct packet p, staged now with columns, says what the
// stored row says. The stored row decides the reading. A row with a readable origin record is read
// through it on both sides; the fresh packet is composed by this build, so its origin is always
// readable. Any other row (one staged before the record existed, or one whose record is unreadable)
// is read the fields way on both sides, except that a row whose stored body may be a cut cannot prove
// what the cut dropped: it is never the same statement, so its park is released once, and the
// rewrite that goes with the release records the origin.
func sameDirectStatement(row store.SupervisorMessagesRow, columns statementColumns, p Packet) (bool, error) {
	stored, err := decodeStatementPacket([]byte(row.Packet))
	if err != nil {
		return false, err
	}
	fresh, err := decodeFreshPacket(p)
	if err != nil {
		return false, err
	}
	reading := readingFields
	if packet, ok := stored.(map[string]any); ok && IsDirectKind(row.ObligationKind) {
		if _, readable := readDirectOrigin(packet); readable {
			reading = readingOrigin
		} else if directBodyMayBeCut(packet) {
			return false, nil
		}
	}
	storedKey, err := statementKey(rowStatementColumns(row), stored, reading)
	if err != nil {
		return false, err
	}
	freshKey, err := statementKey(columns, fresh, reading)
	if err != nil {
		return false, err
	}
	return storedKey == freshKey, nil
}

// decodeStatementPacket decodes one JSON packet with numbers kept as written.
func decodeStatementPacket(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("a stored packet is one JSON value, and this one has more after it")
	}
	return value, nil
}
