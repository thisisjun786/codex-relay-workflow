//go:build dev

package stopevents

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Whether a record is one the adapter's writers produce: the shape of each record kind, checked
// against the hook's own vocabulary (internal/relay/hook records.go) and EventKey. The reading
// in judge.go decides what an event's records say together; this file decides whether each one
// is a record at all.

type object = hook.Object

// The names the adapter gives the files it writes.
var (
	journalName = regexp.MustCompile(`^[0-9a-f]{32}\.json$`)
	journalDay  = regexp.MustCompile(`^[0-9]{8}$`)
	ledgerName  = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)
	outcomeName = regexp.MustCompile(`^[0-9a-f]{64}\.outcome\.json$`)
	stampShape  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

func has(o object, key string) bool {
	_, ok := o.Lookup(key)
	return ok
}

func asObject(v any) (object, bool) {
	o, ok := v.(object)
	return o, ok
}

// member is whether v is one of the strings in set: a value of another type is never in it.
func member(v any, set []string) bool {
	s, ok := v.(string)
	return ok && slices.Contains(set, s)
}

// valueKey keys a value a record holds: equal numbers alike whatever their spelling, and an
// array or object by its JSON text.
func valueKey(v any) string {
	switch v.(type) {
	case object, []any:
		return "json:" + pyjson.Dumps(v, pyjson.Options{})
	}
	return evidence.HashKey(v)
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

func isString(v any) bool {
	_, ok := v.(string)
	return ok
}

func isBool(v any) bool {
	_, ok := v.(bool)
	return ok
}

// exact is an integer the adapter writes, not a value that merely compares equal to it.
func exact(v any, n int64) bool {
	i, ok := evidence.PyInt(v)
	return ok && i == n
}

// count is a non-negative (zero) or positive integer of any size, never a bool (the hook's own
// predicate, which its native pre-scan row is read with too).
func count(v any, zero bool) bool { return hook.Count(v, zero) }

func fieldsExactly(v any, fields []string) bool {
	o, ok := asObject(v)
	if !ok || len(o) != len(fields) {
		return false
	}
	for _, f := range fields {
		if !has(o, f) {
			return false
		}
	}
	return true
}

func isAbs(p string) bool { return strings.HasPrefix(p, "/") }

// stamp is a real UTC second in the adapter's own format.
func stamp(v any) bool {
	s, ok := v.(string)
	if !ok || !stampShape.MatchString(s) {
		return false
	}
	at, err := time.Parse("2006-01-02T15:04:05Z", s)
	return err == nil && at.Year() >= 1
}

// day is a day directory's name that is a real date.
func day(name string) bool {
	if !journalDay.MatchString(name) {
		return false
	}
	at, err := time.Parse("20060102", name)
	return err == nil && at.Year() >= 1
}

// slot is a row's place as the adapter names it: a real day and a row name.
func slot(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	parts := strings.Split(s, "/")
	return len(parts) == 2 && day(parts[0]) && journalName.MatchString(parts[1])
}

// readRecord is a record as the reading finds it: its content, whether it is a readable regular
// file reached without following a link, and whether it holds exactly the writer's bytes.
func readRecord(p string) (any, bool, bool) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, false
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, false, false
	}
	body, err := pyload.Loads(raw)
	if err != nil {
		return nil, false, false
	}
	return body, true, bytes.Equal(raw, hook.RecordBytes(body))
}

func stderrKept(v any) bool {
	s, ok := v.(string)
	if !ok || !utf8.ValidString(s) {
		return false
	}
	return utf8.RuneCountInString(s) <= hook.StderrLimit
}

// callRecorded is whether a row's guard call is one the adapter records: a known ending and
// stdout reading, an exit code only from a process that exited, a signal only from one that was
// signalled, an errno only from one that never started.
func callRecorded(row object) bool {
	said, how := row.Get("stdoutReading"), row.Get("processEnding")
	if !member(said, hook.StdoutReadings) || !member(how, hook.ProcessEndings) {
		return false
	}
	code, signal, errno := row.Get("exitCode"), row.Get("signal"), row.Get("errno")
	switch how {
	case hook.Exited:
		if !count(code, true) || signal != nil || errno != nil {
			return false
		}
	case hook.Signalled:
		if code != nil || !count(signal, false) || errno != nil {
			return false
		}
	case hook.NotStarted:
		if code != nil || signal != nil || errno == nil {
			return false
		}
	default:
		if code != nil || signal != nil || errno != nil {
			return false
		}
	}
	return count(row.Get("guardElapsedMs"), true) && stderrKept(row.Get("guardStderr"))
}

// couldAnswer is a call that could have produced an answer: a verdict from a clean exit.
func couldAnswer(row object) bool {
	return row.Get("processEnding") == hook.Exited && row.Get("stdoutReading") == hook.SaidAVerdict && exact(row.Get("exitCode"), 0)
}

// outcomeOf is the one outcome a recorded call reaches, when the verdict itself is not needed.
func outcomeOf(how, said, code any) string {
	switch how {
	case hook.NotStarted:
		return hook.GuardUnreachable
	case hook.TimedOut:
		return "guard_timed_out"
	case hook.Signalled:
		return "guard_signalled"
	}
	is := func(n int64) bool { return evidence.HashKey(code) == evidence.HashKey(n) }
	switch said {
	case hook.SaidSomethingUnreadable:
		return "guard_output_unreadable"
	case hook.SaidAnErrorRecord:
		switch {
		case is(2):
			return "guard_refused"
		case is(3):
			return "guard_host_error"
		case is(4):
			return "guard_usage_error"
		}
		return "guard_ended_unexpectedly"
	case hook.SaidNothing:
		switch {
		case is(2):
			return "guard_rejected_the_call"
		case is(0):
			return "guard_said_nothing"
		}
		return "guard_ended_unexpectedly"
	}
	return "guard_ended_unexpectedly"
}

// outcomeFollows is whether a guard outcome is the one its recorded call reaches. The verdict is
// not recorded, so a verdict from a clean exit may have been answered or incomplete.
func outcomeFollows(row object) bool {
	if !callRecorded(row) {
		return false
	}
	outcome := row.Get("adapterOutcome")
	if couldAnswer(row) {
		return outcome == "guard_verdict_incomplete" || outcome == hook.GuardAnswered
	}
	return outcome == outcomeOf(row.Get("processEnding"), row.Get("stdoutReading"), row.Get("exitCode"))
}

func anyAnswerField(row object) bool {
	for _, f := range hook.AnswerFields {
		if has(row, f) {
			return true
		}
	}
	return false
}

func allFields(row object, fields []string) bool {
	for _, f := range fields {
		if !has(row, f) {
			return false
		}
	}
	return true
}

func anyField(row object, fields []string) bool {
	for _, f := range fields {
		if has(row, f) {
			return true
		}
	}
	return false
}

// faultPrefixWritten is whether a faulted row holds a prefix of what the adapter records, in its
// order: the guard marked as asked, then the call, then an answer.
func faultPrefixWritten(row object) bool {
	called := row.Get("processEnding") != nil
	answered := row.Get("guardDecision") != nil || row.Get("guardState") != nil || row.Get("assignmentId") != nil || row.Get("guardRecordedAs") != nil || anyAnswerField(row)
	if called {
		if row.Get("guardInvoked") != true || !allFields(row, hook.GuardCallFields) || !callRecorded(row) {
			return false
		}
	} else if row.Get("stdoutReading") != nil || anyField(row, hook.GuardCallFields) {
		return false
	}
	if answered {
		return called && allFields(row, hook.AnswerFields) && member(row.Get("guardDecision"), hook.Decisions) && couldAnswer(row)
	}
	return true
}

// guardResultWritten is whether a record's guard result is one the adapter writes: only an
// answer carries a decision, and it holds exactly when it blocks.
func guardResultWritten(record object) bool {
	outcome, decision := record.Get("adapterOutcome"), record.Get("guardDecision")
	state, held := record.Get("guardState"), record.Get("held")
	h, ok := held.(bool)
	if !ok || !(state == nil || isString(state)) {
		return false
	}
	if outcome == hook.GuardAnswered {
		return member(decision, hook.Decisions) && h == (decision == hook.Block)
	}
	if h {
		return false
	}
	if outcome == hook.AdapterFaulted {
		return decision == nil || member(decision, hook.Decisions)
	}
	return decision == nil && state == nil && record.Get("assignmentId") == nil && record.Get("guardRecordedAs") == nil
}

func settledPath(v any, untried bool) bool {
	s, ok := v.(string)
	return ok && isAbs(s) && s == store.Normpath(s) && (untried || hook.PathTheSystemTakes(s))
}

// rowFieldsWritten is whether a row carries every field the adapter writes on the path its
// outcome names, and nothing else.
func rowFieldsWritten(row object) bool {
	if !allFields(row, hook.RowFields) || row.Get("event") != "Stop" {
		return false
	}
	outcome, acceptance, detail := row.Get("adapterOutcome"), row.Get("acceptance"), row.Get("detail")
	allowed := slices.Concat(hook.RowFields, hook.PayloadFields, hook.GuardCallFields, hook.AnswerFields, hook.SettledFields)
	if outcome == hook.AdapterFaulted {
		allowed = append(allowed, hook.FaultFields...)
	}
	for _, f := range row {
		if !slices.Contains(allowed, f.Key) {
			return false
		}
	}
	if row.Get("journalledAs") != nil {
		return false
	}
	if identity, ok := asObject(row.Get("eventIdentity")); ok {
		for _, f := range identity {
			if !slices.Contains(hook.IdentityFields, f.Key) {
				return false
			}
		}
	}
	if !settledPath(row.Get("configuration"), member(outcome, hook.SettingsUntried)) || !count(row.Get("elapsedMs"), true) || !(detail == nil || isString(detail)) {
		return false
	}
	read := allFields(row, hook.PayloadFields)
	if anyField(row, hook.PayloadFields) && !read {
		return false
	}
	mode := row.Get("guardMode")
	if read != member(mode, []string{hook.Observe, hook.Hold}) || (mode != nil && !read) {
		return false
	}
	if acceptance == nil && !hook.NativePrescanUnreachable(row) && ((read && outcome != hook.AdapterFaulted) || row.Get("identityScanMs") != nil) {
		return false
	}
	if member(outcome, slices.Concat(hook.BeforeTheGuard, []string{hook.DuplicateInvocation, hook.ArbitrationFailed})) && !isString(detail) {
		return false
	}
	if member(outcome, []string{hook.DuplicateInvocation, hook.ArbitrationFailed}) {
		fixed := map[string]string{hook.DuplicateInvocation: hook.DuplicateDetail, hook.ArbitrationFailed: hook.UnarbitratedDetail}
		if detail != fixed[outcome.(string)] {
			return false
		}
	}
	if member(outcome, hook.FromTheGuard) && (detail == nil) != member(row.Get("processEnding"), []string{hook.Exited, hook.Signalled}) {
		return false
	}
	if outcome == hook.AdapterFaulted {
		if !isString(row.Get("fault")) {
			return false
		}
	} else if !allFields(row, hook.SettledFields) {
		return false
	}
	switch {
	case outcome == hook.AdapterFaulted:
		if !faultPrefixWritten(row) {
			return false
		}
	case member(outcome, hook.FromTheGuard):
		if !allFields(row, hook.GuardCallFields) || !outcomeFollows(row) {
			return false
		}
	case anyField(row, hook.GuardCallFields):
		return false
	}
	if outcome == hook.GuardAnswered {
		if !allFields(row, hook.AnswerFields) {
			return false
		}
	} else if outcome != hook.AdapterFaulted && anyAnswerField(row) {
		return false
	}
	if !(mode == nil || member(mode, []string{hook.Observe, hook.Hold})) {
		return false
	}
	if acceptance != nil {
		identity, ok := asObject(row.Get("eventIdentity"))
		if !read || !ok || !allFields(identity, hook.IdentityFields) || !count(row.Get("identityScanMs"), true) || !count(identity.Get("scannedBytes"), true) || !count(identity.Get("scannedLines"), true) {
			return false
		}
		if p := identity.Get("transcriptPath"); !(p == nil || isString(p)) {
			return false
		}
	}
	return true
}

// acceptances is every acceptance a row can carry besides null (an invocation that reached no
// event), and outcomesOf the outcomes each one ends in besides a fault.
var acceptances = []string{hook.Unestablished, hook.Accepted, hook.Unclaimable, hook.ClaimFailed, hook.Duplicate, hook.Unarbitrated}

func outcomesOf(acceptance any) []string {
	switch acceptance {
	case nil:
		return hook.BeforeTheGuard
	case hook.Duplicate:
		return []string{hook.DuplicateInvocation}
	case hook.Unarbitrated:
		return []string{hook.ArbitrationFailed}
	}
	return hook.FromTheGuard
}

// rowShape is whether a version-2 row is one the adapter writes. A row that names an event needs
// its session, turn and key, and they hash to the key; one that could not be identified carries
// only what its reason had recorded; one that reached no event carries none of them.
func rowShape(row object) bool {
	if !stamp(row.Get("at")) || !rowFieldsWritten(row) {
		return false
	}
	if hook.NativePrescanUnreachable(row) {
		return true
	}
	acceptance, key := row.Get("acceptance"), row.Get("eventKey")
	outcome, asked := row.Get("adapterOutcome"), row.Get("guardInvoked")
	if !(acceptance == nil || member(acceptance, acceptances)) || !isBool(asked) || !guardResultWritten(row) {
		return false
	}
	switch {
	case outcome == hook.AdapterFaulted:
		if acceptance == hook.Duplicate || acceptance == hook.Unarbitrated {
			return false
		}
	case !member(outcome, outcomesOf(acceptance)):
		return false
	case member(outcome, hook.FromTheGuard):
		if asked != true || !isString(row.Get("processEnding")) {
			return false
		}
	case (asked == true && acceptance != hook.Duplicate) || row.Get("processEnding") != nil || row.Get("stdoutReading") != nil:
		// A duplicate that says it asked is left to the verdict, which reads it FALSE.
		return false
	}
	k, keyed := key.(string)
	keyed = keyed && ledgerName.MatchString(k+".json")
	named := nonEmptyString(row.Get("sessionId")) && nonEmptyString(row.Get("turnId"))
	identity, isIdentity := asObject(row.Get("eventIdentity"))
	switch acceptance {
	case hook.Accepted, hook.Duplicate, hook.Unclaimable, hook.ClaimFailed, hook.Unarbitrated:
		if !keyed || !named || !isIdentity || identity.Get("established") != true || identity.Get("reason") != nil {
			return false
		}
		transcript, ok := identity.Get("transcriptPath").(string)
		if !ok || !isAbs(transcript) || !hook.PathTheSystemTakes(transcript) || !isBool(row.Get("stopHookActive")) || !nonEmptyString(identity.Get("answerItem")) {
			return false
		}
		if k != hook.EventKey(row.Get("sessionId"), row.Get("turnId"), row.Get("stopHookActive"), identity.Get("answerItem")) {
			return false
		}
		where := row.Get("acceptedAs")
		switch acceptance {
		case hook.Accepted:
			return where == hook.LedgerDirectory+"/"+k+".json"
		case hook.Duplicate:
			return where == hook.LedgerDirectory+"/"+k+".json" || where == strings.Join(hook.HostLedgerParts, "/")+"/"+k+".json"
		}
		return where == nil
	case hook.Unestablished:
		if key != nil || !isIdentity || identity.Get("established") != false || !member(identity.Get("reason"), hook.UnestablishedReasons) {
			return false
		}
		reason := identity.Get("reason").(string)
		transcript := identity.Get("transcriptPath")
		if slices.Contains(hook.PathlessReasons, reason) {
			if transcript != nil {
				return false
			}
		} else if p, ok := transcript.(string); !ok || isAbs(p) != (reason != hook.TranscriptPathRelative) {
			return false
		} else if !slices.Contains(hook.PathUntriedReasons, reason) && !hook.PathTheSystemTakes(p) {
			return false
		}
		if reason == hook.SessionMismatch {
			return nonEmptyString(identity.Get("answerItem"))
		}
		return identity.Get("answerItem") == nil
	}
	return key == nil && row.Get("eventIdentity") == nil
}

// hostLedgerNamed is a host ledger as the adapter names one: absolute, normalized, ending in the
// ledger's own two parts, and one the system took.
func hostLedgerNamed(v any) bool {
	s, ok := v.(string)
	if !ok || !isAbs(s) || s != store.Normpath(s) || !hook.PathTheSystemTakes(s) {
		return false
	}
	parts := strings.Split(s, "/")
	n := len(hook.HostLedgerParts)
	return len(parts) > n && slices.Equal(parts[len(parts)-n:], hook.HostLedgerParts)
}

// ledgerShape is whether a claim (outcome false) or an outcome carries every field its kind is
// written with and names its key.
func ledgerShape(v any, key string, outcome bool) bool {
	body, ok := asObject(v)
	if !ok || body.Get("eventKey") != key || !exact(body.Get("ledgerVersion"), hook.LedgerVersion) {
		return false
	}
	if !nonEmptyString(body.Get("sessionId")) || !nonEmptyString(body.Get("turnId")) {
		return false
	}
	if outcome {
		ended := body.Get("adapterOutcome")
		row := body.Get("attemptRow")
		return fieldsExactly(body, hook.OutcomeFields) && stamp(body.Get("at")) && (member(ended, hook.FromTheGuard) || ended == hook.AdapterFaulted) && member(body.Get("journalPolicy"), hook.JournalPolicies) && guardResultWritten(body) && (row == nil || slot(row))
	}
	claimedBy, _ := asObject(body.Get("claimedBy"))
	return fieldsExactly(body, hook.ClaimFields) && fieldsExactly(body.Get("claimedBy"), hook.ClaimedByFields) && stamp(body.Get("claimedAt")) && isBool(body.Get("stopHookActive")) && nonEmptyString(body.Get("answerItem")) && slot(claimedBy.Get("attemptRow")) && count(claimedBy.Get("pid"), false) && hostLedgerNamed(claimedBy.Get("hostLedger")) && key == hook.EventKey(body.Get("sessionId"), body.Get("turnId"), body.Get("stopHookActive"), body.Get("answerItem"))
}

// hostShape is whether a host file carries every field it is written with, under its own key.
func hostShape(v any, key string) bool {
	body, ok := asObject(v)
	if !ok || body.Get("eventKey") != key || !exact(body.Get("ledgerVersion"), hook.LedgerVersion) {
		return false
	}
	for _, f := range []string{"sessionId", "turnId", "answerItem"} {
		if !nonEmptyString(body.Get(f)) {
			return false
		}
	}
	if !stamp(body.Get("claimedAt")) {
		return false
	}
	claimedBy, _ := asObject(body.Get("claimedBy"))
	root := claimedBy.Get("journalRoot")
	rootOK := root == nil
	if s, ok := root.(string); ok {
		rootOK = isAbs(s)
	}
	return fieldsExactly(body, hook.ClaimFields) && fieldsExactly(body.Get("claimedBy"), hook.HostClaimedByFields) && isBool(body.Get("stopHookActive")) && slot(claimedBy.Get("attemptRow")) && count(claimedBy.Get("pid"), false) && rootOK && key == hook.EventKey(body.Get("sessionId"), body.Get("turnId"), body.Get("stopHookActive"), body.Get("answerItem"))
}
