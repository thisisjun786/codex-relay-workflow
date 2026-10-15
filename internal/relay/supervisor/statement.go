package supervisor

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

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
// changed). direct says which evidence rule applies: a direct message's top-level evidence is
// content its sender wrote (the default supervisor-show --message <id> is constant per id) and is
// kept; an obligation packet's evidence is built by the channel from the event id or the live
// project and is dropped. An envelope that is not an object is kept as it is, so a packet this reader
// does not understand is compared whole there.
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
		if body, ok := out["body"].(string); ok {
			out["body"] = strings.TrimPrefix(body, proxyReporterNoticePattern.FindString(body))
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

// ProxyReporterNotice is the line report-up writes ahead of a proxy report's text: the report is
// made by reporter, a thread that is not the initiative's lead, and sent in the name of lead. It is
// the reporting command's notice, not what the report says: statementFields strips it from a direct
// message's body, so a report made again after the lead moved (the notice naming the new lead), or by
// the lead itself, is the same statement.
func ProxyReporterNotice(reporter, lead string) string {
	return "Reported by thread " + reporter + ", which is not the initiative's lead (" + lead + ").\n\n"
}

// proxyReporterNoticePattern matches ProxyReporterNotice at the start of a body, for any reporter and
// lead.
var proxyReporterNoticePattern = regexp.MustCompile(`^Reported by thread [^\n]*, which is not the initiative's lead \([^\n]*\)\.\n\n`)

// defaultEvidenceCommand is the tail of the evidence pointer composeDirect writes when the sender
// gives none: the supervisor-show line for the message. What precedes it (the program the staging
// process was invoked as, and --state with its store directory) is how that process ran, not what the
// message says.
const defaultEvidenceCommand = "supervisor-show" + " --message " // a command line, built in two parts so no part reads as running text

// shellWord is one word as quote.Shell writes it: safe characters, or a single-quoted run with its
// quotes escaped, or several such pieces joined.
const shellWord = `(?:[A-Za-z0-9@%+=:,./_-]+|'(?:[^']|'"'"')*')+`

// isDefaultEvidence is whether text is the pointer composeDirect writes for messageID when its sender
// gives no evidence: exactly programCommand's shape, one program word (with relay after it for the
// multi-call crw), --state and one directory word, then the supervisor-show line. A pointer of any
// other shape (another command, a wrapper such as ssh, a different message) is the sender's own.
func isDefaultEvidence(text, messageID string) bool {
	pattern := `^` + shellWord + `(?: relay)? --state ` + shellWord + ` ` + regexp.QuoteMeta(defaultEvidenceCommand+messageID) + `$`
	matched, err := regexp.MatchString(pattern, text)
	return err == nil && matched
}

// statementEvidence is a direct message's top-level evidence as a statement reads it when the
// staging asks for the default evidence reading (statementReading.defaultEvidence): composeDirect
// writes its default pointer only as the one item of an evidence list the sender left empty, so a list
// of exactly that one pointer is read as the supervisor-show line alone, whatever program and store
// spelled it. Any other list is kept as written.
func statementEvidence(value any, messageID string) any {
	items, ok := value.([]any)
	if !ok || messageID == "" || len(items) != 1 {
		return value
	}
	if text, ok := items[0].(string); ok && isDefaultEvidence(text, messageID) {
		return []any{defaultEvidenceCommand + messageID}
	}
	return value
}

// statementReading says how a comparison of two statements reads them, the same for both sides.
//   - defaultEvidence: the message being staged now carries no evidence of its sender's, so its
//     evidence is the channel's default pointer, and a stored list of one default-shaped pointer is
//     read as the supervisor-show line (statementEvidence). When the sender gives evidence it is
//     compared as written on both sides, so a pointer the sender moved (another store, another host)
//     is a new statement.
//   - masked: sender session ids read as one placeholder wherever a kept string names them.
type statementReading struct {
	defaultEvidence bool
	masked          []string
}

// statementKey is the statement key of a packet already decoded the way decodeStatementPacket
// decodes (objects as map[string]any, numbers as json.Number): sha256 over the canonical encoding
// of the obligation id, the event id, the submission and the statement fields. The four parts are
// one sorted-key JSON object, so no value can run into its neighbour. Which evidence rule applies is
// read from the obligation kind (IsDirectKind), for a stored row and a fresh packet alike. reading
// (statementReading) is how a direct message's comparison reads both sides (directHoldReleases); the
// zero reading compares the statement fields as they are.
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
	direct := IsDirectKind(columns.ObligationKind)
	statement := statementFields(packet, direct)
	if value, ok := statement["evidence"]; ok && direct && reading.defaultEvidence {
		messageID := ""
		if region, ok := packet["envelope"].(map[string]any); ok {
			messageID, _ = region["messageId"].(string)
		}
		statement["evidence"] = statementEvidence(value, messageID)
	}
	if masked := reading.masked; len(masked) > 0 {
		// The longest text first, so a sender id that holds another (01lead2 and 01lead) is masked whole.
		masked = append([]string(nil), masked...)
		sort.SliceStable(masked, func(i, j int) bool { return len(masked[i]) > len(masked[j]) })
		statement = maskStatement(statement, masked).(map[string]any)
	}
	whole := map[string]any{"obligationId": columns.ObligationID, "eventId": eventID, "submissionNo": submission,
		"statement": statement}
	encoded, err := pyjson.Encode(whole, pyjson.Options{SortKeys: true, Unicode: true, Normalize: true})
	if err != nil {
		return "", err
	}
	return pyvalue.SHA256Hex(string(encoded)), nil
}

// rowStatementKey is the statement key of a stored supervisor_messages row: its columns and the
// packet as it was stored.
func rowStatementKey(row store.SupervisorMessagesRow) (string, error) {
	return storedStatementKey(rowStatementColumns(row), row.Packet, statementReading{})
}

// storedStatementKey is the statement key of a stored packet encoding with the columns stored beside it.
func storedStatementKey(columns statementColumns, packet string, reading statementReading) (string, error) {
	decoded, err := decodeStatementPacket([]byte(packet))
	if err != nil {
		return "", err
	}
	return statementKey(columns, decoded, reading)
}

// freshStatementKey is the statement key of a freshly composed packet with the columns staging
// would write with it. The packet is read through its JSON encoding, so a fresh []string and a
// stored array, or a fresh int64 and a stored number, are the same value.
func freshStatementKey(columns statementColumns, p Packet, reading statementReading) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	decoded, err := decodeStatementPacket(raw)
	if err != nil {
		return "", err
	}
	return statementKey(columns, decoded, reading)
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

// maskStatement is value with every occurrence of a masked text in its strings (keys and values)
// replaced by one placeholder, in the order given. An empty text masks nothing.
func maskStatement(value any, masked []string) any {
	switch v := value.(type) {
	case string:
		for _, text := range masked {
			if text != "" {
				v = strings.ReplaceAll(v, text, "\x00sender\x00")
			}
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = maskStatement(item, masked)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[maskStatement(key, masked).(string)] = maskStatement(item, masked)
		}
		return out
	}
	return value
}
