// registry.go is the Go form of CXC v0.2.40 bg-wake/src/registry.ts: the record of a background job, its reconciliation from the exit
// file and the pid (there is no watcher), and the choice of the completions that wake or are handed to a session. It sits on store.go.
//
// Behaviour is the oracle's; the differences are these. Every function takes the workspace the caller trusts and acts on its record,
// exit file and ledger, never on the cwd field of a record it has read (store.go's rule; the oracle wrote through rec.cwd). Time comes
// from a clock the caller passes, read where the oracle reads Date. A record is decoded by key and checked whole before any lifecycle
// step (CRW-1134): all thirteen keys of the oracle's record must be there with their types, a key this port does not name is kept and
// written back, and a file that fails the check is a broken record, which nothing changes and list, get, cancel and status report. Date.parse reads the standard spellings (a zone-less date-time as local time) and
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
	// StatusCancelRequested is a cancel that has signalled the job and not yet seen its process group stop (CRW-1155). It is not
	// terminal: Reconcile settles it cancelled once no process of the group is left, and a second cancel sends SIGKILL.
	StatusCancelRequested BgStatus = "cancellation-requested"
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

// recordKeys are the thirteen keys of a record, each with the test of its value: s text, s? text or null, n? a number or null, i? an
// integer or null, [s] an array of text, and st one of the statuses.
var recordKeys = []struct{ key, kind string }{{"id", "s"}, {"sessionId", "s?"}, {"adoptedBy", "s?"}, {"cwd", "s"}, {"command", "[s]"},
	{"note", "s?"}, {"pid", "i?"}, {"startToken", "s?"}, {"status", "st"}, {"exitCode", "n?"}, {"startedAt", "s"}, {"endedAt", "s?"},
	{"deliveredAt", "s?"}}

// checkRecord is the whole-record test (CRW-1134): the oracle's isRecord tested four keys, so a file without deliveredAt was taken as
// delivered, a fractional pid as a dead process and a number in command was joined by coercion. It names the first key that fails.
func checkRecord(m map[string]json.RawMessage) error {
	for _, k := range recordKeys {
		raw, ok := m[k.key]
		if !ok {
			return fmt.Errorf("no %q", k.key)
		}
		raw = bytes.TrimSpace(raw)
		if strings.HasSuffix(k.kind, "?") && string(raw) == "null" {
			continue
		}
		// json.Unmarshal reads a null into a string, a number or a slice element without an error, so a null is refused here for every
		// key that does not allow it, and a text must be a JSON string token.
		bad := len(raw) == 0 || string(raw) == "null"
		switch kind := strings.TrimSuffix(k.kind, "?"); {
		case bad:
		case kind == "s":
			var v string
			bad = raw[0] != '"' || json.Unmarshal(raw, &v) != nil
		case kind == "[s]":
			var v []json.RawMessage
			bad = raw[0] != '[' || json.Unmarshal(raw, &v) != nil
			for _, el := range v {
				if el = bytes.TrimSpace(el); len(el) == 0 || el[0] != '"' {
					bad = true
				}
			}
		case kind == "n":
			var v float64
			bad = raw[0] == '"' || json.Unmarshal(raw, &v) != nil
		case kind == "i":
			var v int
			bad = json.Unmarshal(raw, &v) != nil
		case kind == "st":
			var v BgStatus
			bad = json.Unmarshal(raw, &v) != nil || !slices.Contains([]BgStatus{StatusRunning, StatusComplete, StatusFailed, StatusCancelled, StatusCancelRequested}, v)
		}
		if bad {
			return fmt.Errorf("%q is %s", k.key, raw)
		}
	}
	return nil
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
func WriteRecord(ws string, rec BgRecord) error { return writeRecord(ws, rec, time.Now) }

// writeRecord reads the clock once for the temporary name, as atomicWrite reads Date.now, so a driven clock ticks as the oracle's does.
func writeRecord(ws string, rec BgRecord, clock func() time.Time) error {
	b, err := encode(rec)
	if err != nil {
		return err
	}
	return atomicWrite(ws, RecordPath(ws, rec.ID), string(b), os.Getpid(), clock().UnixMilli())
}

// errRecordGone is a record that is not there when a writer goes back to it under the lock.
const errRecordGone = sentinel("the record is gone")

// BrokenRecord is a file of the store, <id>.json, that does not read as a record: it cannot be read, is not a JSON object, fails the
// whole-record check or holds another id. Nothing changes such a file.
type BrokenRecord struct {
	ID     string
	Reason string
}

func (e BrokenRecord) Error() string { return "손상된 기록 " + e.ID + ": " + e.Reason }

// readRecord is the record of that id, errRecordGone when its file is missing, or a BrokenRecord.
func readRecord(ws, id string) (BgRecord, error) {
	raw, err := readText(RecordPath(ws, id))
	if errors.Is(err, os.ErrNotExist) {
		return BgRecord{}, errRecordGone
	}
	if err != nil {
		return BgRecord{}, BrokenRecord{id, "읽을 수 없음 (" + err.Error() + ")"}
	}
	var m map[string]json.RawMessage
	if !json.Valid([]byte(raw)) || json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return BgRecord{}, BrokenRecord{id, "JSON 객체가 아님"}
	}
	if err := checkRecord(m); err != nil {
		return BgRecord{}, BrokenRecord{id, err.Error()}
	}
	get := func(key string) json.RawMessage { raw := m[key]; delete(m, key); return raw }
	rec := BgRecord{ID: str(get("id")), SessionID: nullable[string](get("sessionId")), AdoptedBy: nullable[string](get("adoptedBy")), Cwd: str(get("cwd")),
		Note: nullable[string](get("note")), PID: nullable[int](get("pid")), StartToken: nullable[string](get("startToken")), Status: BgStatus(str(get("status"))),
		ExitCode: nullable[float64](get("exitCode")), StartedAt: str(get("startedAt")), EndedAt: nullable[string](get("endedAt")), DeliveredAt: nullable[string](get("deliveredAt"))}
	_ = json.Unmarshal(get("command"), &rec.Command)
	if rec.ID != id {
		return BgRecord{}, BrokenRecord{id, "다른 id를 담음 (" + rec.ID + ")"}
	}
	for _, key := range slices.Sorted(maps.Keys(m)) {
		rec.Extra = append(rec.Extra, Member{key, m[key]})
	}
	return rec, nil
}

// ReadRecord is the record of that id, or false when its file is missing or is a broken record (readRecord): the oracle returned a
// record under the id asked for whatever id it held and then wrote its corrections to the record file of the id it held.
func ReadRecord(ws, id string) (BgRecord, bool) {
	rec, err := readRecord(ws, id)
	return rec, err == nil
}

// BrokenRecords is every broken record of the store, in the order of ListRecordIDs.
func BrokenRecords(ws string) []BrokenRecord {
	var out []BrokenRecord
	for _, id := range ListRecordIDs(ws) {
		var broken BrokenRecord
		if _, err := readRecord(ws, id); errors.As(err, &broken) {
			out = append(out, broken)
		}
	}
	return out
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

// PidGone is whether no process has the pid (signal 0 answers ESRCH).
func PidGone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// ExitRead is what <id>.exit says: absent, pending (a body that is empty, has no number or is infinite: a write in progress) or known,
// with its Code.
type ExitRead struct {
	State string
	Code  float64
}

// readExitCode reads the exit file. Its body, trimmed, must be one whole integer with an optional sign and finite as a number; anything
// else is a write in progress (pending). The oracle took the integer prefix (parseInt), so a half-written "1" of "127", or "3x", was a
// final code (CRW-1134).
func readExitCode(ws, id string) ExitRead {
	raw, ok := ReadText(ExitPath(ws, id))
	if !ok {
		return ExitRead{State: "absent"}
	}
	body := text.Trim(raw)
	digits := strings.TrimLeft(body, "+-")
	if len(body)-len(digits) > 1 || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return ExitRead{State: "pending"}
	}
	code, err := strconv.ParseFloat(body, 64)
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
// fails is the error, as the oracle's throw was. The correction is written under the store lock over the record as it is on disk then
// (CRW-1155): a record another writer has moved on since it was read is returned as it is now and not written.
//
// A record whose cancel was requested is settled cancelled only once no process of its group is left: a leader that has ended says
// nothing of its descendants (CRW-1155). A reservation is judged by its age alone: no exit file is its own.
func Reconcile(ws string, rec BgRecord, clock func() time.Time) (BgRecord, error) {
	return reconcile(ws, rec, clock, false)
}

func reconcile(ws string, rec BgRecord, clock func() time.Time, held bool) (BgRecord, error) {
	stamp := clock().UTC().Format(isoLayout)
	nowMs := func() float64 { return float64(clock().UnixMilli()) }
	if rec.Status != StatusRunning && rec.Status != StatusCancelRequested {
		return rec, nil
	}
	next, event := rec, Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", nil}, {"detail", "watcher vanished"}}
	// A reservation (CRW-1155) is published before the stale files of a reused id are cleared, and its shell cannot run before the
	// launch has published its pid: an exit file beside it is the previous job's, so only its age settles it.
	exit := ExitRead{State: "absent"}
	if !isReservation(rec) {
		exit = readExitCode(ws, rec.ID)
	}
	switch {
	case exit.State == "known":
		code := exit.Code
		next.Status, next.ExitCode, event = StatusComplete, &code, Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", code}}
		if code != 0 {
			next.Status = StatusFailed
		}
		if next.EndedAt == nil {
			next.EndedAt = &stamp
		}
		return settle(ws, rec, next, event, clock, held)
	case exit.State == "pending":
		// The exit code is on the way, so stay running, but only for the grace window. A file whose mtime cannot be read is not stale,
		// and the pid test here never looks at the start token.
		mtime, ok := MtimeMs(ExitPath(ws, rec.ID))
		if !(ok && nowMs()-mtime > PendingExitGraceMs && (rec.PID == nil || !PidAlive(*rec.PID))) {
			return rec, nil
		}
	case rec.PID == nil: // the shell never spawned, so only age bounds the record
		started, ok := dateMs(rec.StartedAt)
		if !ok {
			// No pid, no exit file and no age: nothing will ever settle it, and no process can be named as its owner (CRW-1134).
			event = Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", nil}, {"detail", "startedAt unreadable"}}
			break
		}
		if nowMs()-float64(started) <= PendingExitGraceMs {
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
	return settle(ws, rec, next, event, clock, held)
}

// settle writes the corrected record and then its ledger row, each reading the clock once, over the record as it is on disk under the
// lock. A record that has moved on since rec was read is left as it is. A requested cancel becomes cancelled, and only when no process
// of its group is left.
func settle(ws string, rec, next BgRecord, event Event, clock func() time.Time, held bool) (BgRecord, error) {
	out, err := update(ws, rec.ID, clock, held, func(cur BgRecord) change {
		if cur.Status != rec.Status || !samePID(cur.PID, rec.PID) {
			return change{}
		}
		fixed := cur
		fixed.Status, fixed.ExitCode, fixed.EndedAt = next.Status, next.ExitCode, next.EndedAt
		if next.ExitCode == nil && !isReservation(cur) {
			// The shell may have published its code since the exit file was read: the code wins over "vanished" (CRW-1134).
			if exit := readExitCode(ws, rec.ID); exit.State == "known" {
				code := exit.Code
				fixed.Status, fixed.ExitCode, event = StatusComplete, &code, Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", code}}
				if code != 0 {
					fixed.Status = StatusFailed
				}
			}
		}
		if cur.Status == StatusCancelRequested {
			if cur.PID != nil && !groupEnded(*cur.PID) {
				return change{}
			}
			fixed.Status, event = StatusCancelled, Event{{"event", "cancelled"}, {"id", rec.ID}}
		}
		return change{next: fixed, event: event, write: true}
	})
	var broken BrokenRecord
	if errors.Is(err, errRecordGone) || errors.As(err, &broken) {
		return rec, nil // nothing to correct any more, or nothing this port may change
	}
	if err != nil {
		return rec, err
	}
	return out, nil
}

// samePID is whether two record pids are the same value.
func samePID(a, b *int) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// groupEnded is whether no process is left in the process group the job's shell leads (it starts one of its own). A zombie that has
// not been reaped still counts, so a reaper that is late keeps the group alive a moment longer, never the other way round.
func groupEnded(pid int) bool {
	return pid > 1 && pid <= math.MaxInt32 && errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

// ListRecords is every parseable record of the store, reconciled; other files are skipped (listRecords).
func ListRecords(ws string, clock func() time.Time) ([]BgRecord, error) {
	return listRecords(ws, clock, false)
}

func listRecords(ws string, clock func() time.Time, held bool) ([]BgRecord, error) {
	out := []BgRecord{}
	for _, id := range ListRecordIDs(ws) {
		if rec, ok := ReadRecord(ws, id); ok {
			fresh, err := reconcile(ws, rec, clock, held)
			if err != nil {
				return nil, err
			}
			out = append(out, fresh)
		}
	}
	return out, nil
}

// DisabledState is the "bg off" switch: whether it is off, the time the file holds, and Err when the file is there but does not read
// (disabledState). Only a missing file is on: the oracle's readTextOrNull answered null for every read error, so an unreadable switch
// (a directory, a file without read permission) let the wake run against "bg off" (CRW-1134). It is now off, with the error.
type DisabledState struct {
	Disabled bool
	Since    *string
	Err      error
}

// ReadDisabledState reads the switch file of the workspace.
func ReadDisabledState(ws string) DisabledState {
	raw, err := readText(DisabledPath(ws))
	if errors.Is(err, os.ErrNotExist) {
		return DisabledState{}
	}
	if err != nil {
		return DisabledState{Disabled: true, Err: err}
	}
	if since := text.Trim(raw); since != "" {
		return DisabledState{Disabled: true, Since: &since}
	}
	return DisabledState{Disabled: true}
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
// MarkDelivered once it has emitted them. A caller that must not hand a completion out twice selects and stamps under one store lock
// (deliver).
func SelectWake(ws string, sessionID *string, limit int, clock func() time.Time) ([]BgRecord, error) {
	return selectWake(ws, sessionID, limit, clock, false)
}

func selectWake(ws string, sessionID *string, limit int, clock func() time.Time, held bool) ([]BgRecord, error) {
	raw, _ := ReadText(EnabledAtPath(ws))
	gate := text.Trim(raw)
	recs, err := listRecords(ws, clock, held)
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
// oracle did. Each stamp is written over the record as it is on disk under the store lock (CRW-1092): a record that is no longer
// terminal and undelivered there is left alone, and an adoption written since it was selected is kept.
func MarkDelivered(ws string, recs []BgRecord, clock func() time.Time) []BgRecord {
	return markDelivered(ws, recs, clock, false)
}

func markDelivered(ws string, recs []BgRecord, clock func() time.Time, held bool) []BgRecord {
	stamp := clock().UTC().Format(isoLayout)
	stamped := []BgRecord{}
	for _, rec := range recs {
		wrote := false
		_, err := update(ws, rec.ID, clock, held, func(cur BgRecord) change {
			if !IsTerminal(cur.Status) || cur.DeliveredAt != nil {
				return change{}
			}
			next := cur
			next.DeliveredAt, wrote = &stamp, true
			return change{next: next, event: Event{{"event", "delivered"}, {"id", rec.ID}, {"sessionId", opt(cmp.Or(cur.AdoptedBy, cur.SessionID))}}, write: true}
		})
		if err == nil && wrote {
			stamped = append(stamped, rec)
		}
	}
	return stamped
}

// AdoptOrphans hands the undelivered completions of other sessions to this one, because after a restart the registering session is
// gone and nobody would be woken (adoptOrphans). Adoption is not delivery: a later Stop or UserPromptSubmit does that. Each adoption is
// written over the record as it is on disk under the store lock (CRW-1092), so a stamp written since the listing is never put back.
func AdoptOrphans(ws string, sessionID *string, clock func() time.Time) ([]BgRecord, error) {
	if sessionID == nil {
		return []BgRecord{}, nil
	}
	unlock, err := lockStore(ws)
	if errors.Is(err, os.ErrNotExist) {
		return []BgRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock()
	return adoptOrphans(ws, sessionID, clock, nil)
}

// adoptOrphans runs under the store lock. It adopts from recs when the caller has listed the store already, else it lists it.
func adoptOrphans(ws string, sessionID *string, clock func() time.Time, recs []BgRecord) ([]BgRecord, error) {
	adopted := []BgRecord{}
	if sessionID == nil {
		return adopted, nil
	}
	at := clock().UTC().Format(isoLayout)
	var err error
	if recs == nil {
		if recs, err = listRecords(ws, clock, true); err != nil {
			return adopted, err
		}
	}
	for _, rec := range recs {
		if !IsTerminal(rec.Status) || rec.DeliveredAt != nil || ownedBy(rec, *sessionID) {
			continue
		}
		var next BgRecord
		next, err = update(ws, rec.ID, clock, true, func(cur BgRecord) change {
			if !IsTerminal(cur.Status) || cur.DeliveredAt != nil || ownedBy(cur, *sessionID) {
				return change{}
			}
			next := cur
			next.AdoptedBy = sessionID
			return change{next: next, event: Event{{"event", "adopted"}, {"id", rec.ID}, {"sessionId", *sessionID}, {"at", at}}, write: true}
		})
		var broken BrokenRecord
		if errors.Is(err, errRecordGone) || errors.As(err, &broken) {
			err = nil
			continue
		}
		if err != nil {
			break
		}
		if next.AdoptedBy != nil && *next.AdoptedBy == *sessionID && next.DeliveredAt == nil {
			adopted = append(adopted, next)
		}
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

// deliver hands the session's due completions over (CRW-1092). Under the store lock it selects them from the records as they are now,
// renders them, emits the text and stamps them delivered only once the emission has succeeded, so a failed write leaves them pending
// for a later wake and a concurrent hook sees them stamped. render answers the text and the completions it describes: only those are
// stamped, and one the budget left out stays pending (CRW-1095). suppressed, when there is one, is asked under the lock too: a wake that
// was on before the wait for the lock may have been turned off by a writer that held it (CRW-1092). It returns the emitted text, ""
// when there was none.
func deliver(ws string, sessionID *string, clock func() time.Time, suppressed func() bool, render func([]BgRecord) (string, []BgRecord), emit func(string) error) (string, error) {
	unlock, err := lockStore(ws)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer unlock()
	if suppressed != nil && suppressed() {
		return "", nil
	}
	due, err := selectWake(ws, sessionID, WakeBatchLimit, clock, true)
	if err != nil || len(due) == 0 {
		return "", err
	}
	out, shown := render(due)
	if out == "" {
		return "", nil
	}
	if err := emit(out); err != nil {
		return "", err
	}
	markDelivered(ws, shown, clock, true)
	return out, nil
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
