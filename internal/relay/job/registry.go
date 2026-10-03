// registry.go is the Go form of CXC v0.2.40 bg-wake/src/registry.ts: the record of a background job, its reconciliation from the exit
// file and the pid (there is no watcher), and the choice of the completions that wake or are handed to a session. It sits on store.go.
//
// Behaviour is the oracle's; the differences are these. Every function takes the workspace the caller trusts and acts on its record,
// exit file and ledger, never on the cwd field of a record it has read (store.go's rule; the oracle wrote through rec.cwd). Time comes
// from a clock the caller passes, read where the oracle reads Date. A record is decoded by key: isRecord's four tests are the oracle's,
// a key this port does not name is kept and written back, and any other field that is absent or of another type reads as null (a record
// the oracle writes has all thirteen keys, typed). Date.parse reads the standard spellings (a zone-less date-time as local time) and
// answers NaN for what V8's legacy parser adds, and the sort by endedAt compares bytes where the oracle used localeCompare, which
// agrees for the UTC stamps the oracle writes.

package job

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// BgStatus is the state of a job.
type BgStatus string

const (
	StatusRunning   BgStatus = "running"
	StatusComplete  BgStatus = "complete"
	StatusFailed    BgStatus = "failed"
	StatusCancelled BgStatus = "cancelled"
)

const (
	WakeBatchLimit     = 5            // the most completions handed over in one wake
	PendingExitGraceMs = 15_000       // how long a half-written exit file is tolerated before the job is called failed
	EnvVar             = "CRW_BGWAKE" // the environment kill switch of the wake (CXC_BGWAKE)
)

// BgRecord is <id>.json in the oracle's key order, then the keys this port does not name; a nil pointer is null. StartToken is the
// process start fingerprint, so a recycled pid is not taken for our shell. ExitCode is a JavaScript number.
type BgRecord struct {
	ID          string
	SessionID   *string
	AdoptedBy   *string
	Cwd         string
	Command     []string
	Note        *string
	PID         *int
	StartToken  *string
	Status      BgStatus
	ExitCode    *float64
	StartedAt   string
	EndedAt     *string
	DeliveredAt *string
	Extra       []Member
}

// isRecord is the oracle's test of a parsed record: id, cwd and status are text and command is an array. A null is not text.
func isRecord(m map[string]json.RawMessage) bool {
	is := func(key string, first byte) bool { v := m[key]; return len(v) > 0 && v[0] == first }
	return is("id", '"') && is("cwd", '"') && is("command", '[') && is("status", '"')
}

// opt is a pointer as a record or ledger value: nil is null.
func opt[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// encode is JSON.stringify(rec, null, 2) and a newline: the compact object of store.go indented, which is the same text for a flat
// object and arrays of strings.
func encode(rec BgRecord) ([]byte, error) {
	compact, err := object(append([]Member{{"id", rec.ID}, {"sessionId", opt(rec.SessionID)}, {"adoptedBy", opt(rec.AdoptedBy)}, {"cwd", rec.Cwd},
		{"command", rec.Command}, {"note", opt(rec.Note)}, {"pid", opt(rec.PID)}, {"startToken", opt(rec.StartToken)}, {"status", string(rec.Status)},
		{"exitCode", opt(rec.ExitCode)}, {"startedAt", rec.StartedAt}, {"endedAt", opt(rec.EndedAt)}, {"deliveredAt", opt(rec.DeliveredAt)}}, rec.Extra...), 0)
	var out bytes.Buffer
	if err == nil {
		err = json.Indent(&out, compact, "", "  ")
	}
	return append(out.Bytes(), '\n'), err
}

// WriteRecord publishes the record under its id in the workspace's store (writeRecord).
func WriteRecord(ws string, rec BgRecord) error {
	b, err := encode(rec)
	if err != nil {
		return err
	}
	return AtomicWrite(ws, RecordPath(ws, rec.ID), string(b))
}

// ReadRecord is the record of that id, or false when its file is missing, is not a JSON object or fails isRecord (readRecord).
func ReadRecord(ws, id string) (BgRecord, bool) {
	raw, ok := ReadJSON(RecordPath(ws, id))
	var m map[string]json.RawMessage
	if !ok || json.Unmarshal(raw, &m) != nil || !isRecord(m) {
		return BgRecord{}, false
	}
	get := func(key string) json.RawMessage { raw := m[key]; delete(m, key); return raw }
	rec := BgRecord{ID: str(get("id")), SessionID: nullable[string](get("sessionId")), AdoptedBy: nullable[string](get("adoptedBy")), Cwd: str(get("cwd")),
		Note: nullable[string](get("note")), PID: nullable[int](get("pid")), StartToken: nullable[string](get("startToken")), Status: BgStatus(str(get("status"))),
		ExitCode: nullable[float64](get("exitCode")), StartedAt: str(get("startedAt")), EndedAt: nullable[string](get("endedAt")), DeliveredAt: nullable[string](get("deliveredAt"))}
	_ = json.Unmarshal(get("command"), &rec.Command)
	for _, key := range slices.Sorted(maps.Keys(m)) {
		rec.Extra = append(rec.Extra, Member{key, m[key]})
	}
	return rec, true
}

// nullable is the JSON value as a pointer, nil for null and for a value of another type than T; str is a string, empty for any other.
func nullable[T any](raw json.RawMessage) *T {
	var v T
	if string(raw) == "null" || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return &v
}

func str(raw json.RawMessage) (s string) { _ = json.Unmarshal(raw, &s); return s }

// RecordExists is whether <id>.json is there (recordExists).
func RecordExists(ws, id string) bool {
	_, err := os.Stat(RecordPath(ws, id))
	return err == nil
}

// ProcessStartToken is what "ps -o lstart= -p <pid>" prints, a fingerprint stable for the life of a pid; a failed ps or an empty answer
// is none (processStartToken).
func ProcessStartToken(pid int) (string, bool) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	token := text.Trim(decodeUTF8(out))
	if err != nil || token == "" {
		return "", false
	}
	return token, true
}

// PidAlive is whether signal 0 reaches the pid; EPERM means it exists and belongs to someone else, which still counts (pidAlive).
func PidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ExitRead is what <id>.exit says: absent, pending (a body that is empty, has no number or is infinite: a write in progress) or known,
// with its Code.
type ExitRead struct {
	State string
	Code  float64
}

// readExitCode reads the exit file; like parseInt it takes the sign and digits the body starts with, so "3x" is 3, and like
// Number.isFinite it leaves an infinite one pending.
func readExitCode(ws, id string) ExitRead {
	raw, ok := ReadText(ExitPath(ws, id))
	if !ok {
		return ExitRead{State: "absent"}
	}
	body, end := text.Trim(raw), 0
	if body != "" && (body[0] == '+' || body[0] == '-') {
		end++
	}
	for end < len(body) && body[end] >= '0' && body[end] <= '9' {
		end++
	}
	code, err := strconv.ParseFloat(body[:end], 64)
	if err != nil || math.IsInf(code, 0) {
		return ExitRead{State: "pending"}
	}
	return ExitRead{State: "known", Code: code}
}

// dateMs is Date.parse in whole milliseconds for the spellings the standard format has: a zone, or none on a date alone, means UTC and
// none on a date-time means local time.
func dateMs(s string) (int64, bool) {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02", "2006-01", "2006"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// Reconcile brings a running record up to date and persists the correction (reconcile). The exit file is authoritative, because a
// recycled pid can look alive. The clock is read at the call, for the stamp, and again where the oracle reads Date.now; a write that
// fails is the error, as the oracle's throw was.
func Reconcile(ws string, rec BgRecord, clock func() time.Time) (BgRecord, error) {
	stamp := clock().UTC().Format(isoLayout)
	nowMs := func() float64 { return float64(clock().UnixMilli()) }
	if rec.Status != StatusRunning {
		return rec, nil
	}
	next, event := rec, Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", nil}, {"detail", "watcher vanished"}}
	switch exit := readExitCode(ws, rec.ID); {
	case exit.State == "known":
		code := exit.Code
		next.Status, next.ExitCode, event = StatusComplete, &code, Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", code}}
		if code != 0 {
			next.Status = StatusFailed
		}
		if next.EndedAt == nil {
			next.EndedAt = &stamp
		}
		return settle(ws, rec, next, event)
	case exit.State == "pending":
		// The exit code is on the way, so stay running, but only for the grace window. A file whose mtime cannot be read is not stale,
		// and the pid test here never looks at the start token.
		mtime, ok := MtimeMs(ExitPath(ws, rec.ID))
		if !(ok && nowMs()-mtime > PendingExitGraceMs && (rec.PID == nil || !PidAlive(*rec.PID))) {
			return rec, nil
		}
	case rec.PID == nil: // the shell never spawned, so only age bounds the record
		started, ok := dateMs(rec.StartedAt)
		if !ok || nowMs()-float64(started) <= PendingExitGraceMs {
			return rec, nil
		}
	default:
		alive, matches := PidAlive(*rec.PID), rec.StartToken == nil
		if !matches {
			token, ok := ProcessStartToken(*rec.PID)
			matches = ok && token == *rec.StartToken
		}
		if alive && matches {
			return rec, nil
		}
	}
	// Dead, or another process owns the pid: the shell went away without an exit file, so the outcome is unknown.
	next.Status, next.ExitCode, next.EndedAt = StatusFailed, nil, &stamp
	return settle(ws, rec, next, event)
}

// settle writes the corrected record and then its ledger row.
func settle(ws string, rec, next BgRecord, event Event) (BgRecord, error) {
	if err := WriteRecord(ws, next); err != nil {
		return rec, err
	}
	AppendLedger(ws, event)
	return next, nil
}

// ListRecords is every parseable record of the store, reconciled; other files are skipped (listRecords).
func ListRecords(ws string, clock func() time.Time) ([]BgRecord, error) {
	out := []BgRecord{}
	for _, id := range ListRecordIDs(ws) {
		if rec, ok := ReadRecord(ws, id); ok {
			fresh, err := Reconcile(ws, rec, clock)
			if err != nil {
				return nil, err
			}
			out = append(out, fresh)
		}
	}
	return out, nil
}

// DisabledState is the "bg off" switch: whether its file is there, and the time the file holds (disabledState).
type DisabledState struct {
	Disabled bool
	Since    *string
}

// ReadDisabledState reads the switch file of the workspace.
func ReadDisabledState(ws string) DisabledState {
	raw, ok := ReadText(DisabledPath(ws))
	if since := text.Trim(raw); ok && since != "" {
		return DisabledState{Disabled: true, Since: &since}
	}
	return DisabledState{Disabled: ok}
}

// EnvDisabled is the environment kill switch; it reaches only sessions started after it was exported (envDisabled).
func EnvDisabled(getenv func(string) string) bool {
	switch strings.ToLower(text.Trim(getenv(EnvVar))) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// WakeSuppressed is whether either switch is off (wakeSuppressed).
func WakeSuppressed(ws string, getenv func(string) string) bool {
	return EnvDisabled(getenv) || ReadDisabledState(ws).Disabled
}

// IsTerminal is whether the job is over (isTerminal).
func IsTerminal(status BgStatus) bool {
	return status == StatusComplete || status == StatusFailed || status == StatusCancelled
}

// ownedBy is whether the session registered the job or adopted it.
func ownedBy(rec BgRecord, sessionID string) bool {
	return rec.SessionID != nil && *rec.SessionID == sessionID || rec.AdoptedBy != nil && *rec.AdoptedBy == sessionID
}

// SelectWake is the completions to hand to the session, oldest first, at most limit: terminal, undelivered, the session's own or
// adopted, and finished after the "bg on" time (selectWake). A job that ends in the millisecond of "bg on" ended in the off window, so
// the strings compare with <=, in UTF-16 order as JavaScript does. The records are not changed; the caller stamps them with
// MarkDelivered once it has emitted them.
func SelectWake(ws string, sessionID *string, limit int, clock func() time.Time) ([]BgRecord, error) {
	raw, _ := ReadText(EnabledAtPath(ws))
	gate := text.Trim(raw)
	recs, err := ListRecords(ws, clock)
	if err != nil {
		return nil, err
	}
	var due []BgRecord
	for _, rec := range recs {
		switch {
		case !IsTerminal(rec.Status) || rec.DeliveredAt != nil || sessionID == nil || !ownedBy(rec, *sessionID):
		case gate != "" && rec.EndedAt != nil && slices.Compare(utf16.Encode([]rune(*rec.EndedAt)), utf16.Encode([]rune(gate))) <= 0:
		default:
			due = append(due, rec)
		}
	}
	ended := func(r BgRecord) string { return *cmp.Or(r.EndedAt, new(string)) }
	slices.SortStableFunc(due, func(a, b BgRecord) int { return strings.Compare(ended(a), ended(b)) })
	return due[:min(len(due), max(0, limit))], nil
}

// MarkDelivered stamps each record independently: one whose write fails stays undelivered for a later wake, which costs less than a
// dropped completion (markDelivered). It returns the records it stamped as they were passed in, with their old deliveredAt, as the
// oracle did.
func MarkDelivered(ws string, recs []BgRecord, clock func() time.Time) []BgRecord {
	stamp := clock().UTC().Format(isoLayout)
	stamped := []BgRecord{}
	for _, rec := range recs {
		next := rec
		next.DeliveredAt = &stamp
		if WriteRecord(ws, next) != nil {
			continue
		}
		AppendLedger(ws, Event{{"event", "delivered"}, {"id", rec.ID}, {"sessionId", opt(cmp.Or(rec.AdoptedBy, rec.SessionID))}})
		stamped = append(stamped, rec)
	}
	return stamped
}

// AdoptOrphans hands the undelivered completions of other sessions to this one, because after a restart the registering session is
// gone and nobody would be woken (adoptOrphans). Adoption is not delivery: a later Stop or UserPromptSubmit does that.
func AdoptOrphans(ws string, sessionID *string, clock func() time.Time) ([]BgRecord, error) {
	at := clock().UTC().Format(isoLayout)
	adopted := []BgRecord{}
	if sessionID == nil {
		return adopted, nil
	}
	recs, err := ListRecords(ws, clock)
	for _, rec := range recs {
		if !IsTerminal(rec.Status) || rec.DeliveredAt != nil || ownedBy(rec, *sessionID) {
			continue
		}
		next := rec
		next.AdoptedBy = sessionID
		if err = WriteRecord(ws, next); err != nil {
			break
		}
		AppendLedger(ws, Event{{"event", "adopted"}, {"id", rec.ID}, {"sessionId", *sessionID}, {"at", at}})
		adopted = append(adopted, next)
	}
	return adopted, err
}

// HasAnyTask counts the parseable records the session owns, not the .json files, so a directory of junk or of another session's jobs
// stays quiet (hasAnyTask). A nil session counts every record.
func HasAnyTask(ws string, sessionID *string, clock func() time.Time) (bool, error) {
	recs, err := ListRecords(ws, clock)
	if err != nil || sessionID == nil {
		return len(recs) > 0, err
	}
	return slices.ContainsFunc(recs, func(r BgRecord) bool { return ownedBy(r, *sessionID) }), nil
}

// DurationLabel is "running", "?" when the stamps do not read or run backwards, "<s>s" under a minute and "<m>m<ss>s" after it.
func DurationLabel(rec BgRecord) string {
	if rec.EndedAt == nil {
		return "running"
	}
	end, endOK := dateMs(*rec.EndedAt)
	start, startOK := dateMs(rec.StartedAt)
	if !endOK || !startOK || end < start {
		return "?"
	}
	s := int64(math.Floor(float64(end-start)/1000 + 0.5)) // Math.round
	if s < 60 {
		return strconv.FormatInt(s, 10) + "s"
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// DescribeRecord is the line a wake and a listing print for a job (describeRecord).
func DescribeRecord(rec BgRecord) string {
	code := "exit ?"
	if rec.ExitCode != nil {
		n, _ := value(*rec.ExitCode, 0) // the number as JavaScript spells it
		code = "exit " + string(n)
	}
	return "- " + rec.ID + " (" + string(rec.Status) + ", " + code + ", " + DurationLabel(rec) + ") — " + strings.Join(rec.Command, " ")
}
