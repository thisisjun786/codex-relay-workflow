//go:build dev

package stopevents

import (
	"bytes"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// Whether a record is one the adapter's writers produce: the shape of each record kind, checked
// against the hook's own vocabulary (internal/relay/hook records.go) and EventKey. The reading
// in judge.go decides what an event's records say together; this file decides whether each one
// is a record at all.

type object = hook.Object

// The names the adapter gives the files it writes. A Python regular expression's $ also matches
// before one trailing newline, so pyMatch accepts that too: the reading classifies such a name
// the way the Python reader did.
var (
	journalName = regexp.MustCompile(`^[0-9a-f]{32}\.json$`)
	journalDay  = regexp.MustCompile(`^[0-9]{8}$`)
	ledgerName  = regexp.MustCompile(`^[0-9a-f]{64}\.json$`)
	outcomeName = regexp.MustCompile(`^[0-9a-f]{64}\.outcome\.json$`)
	stampShape  = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

func pyMatch(re *regexp.Regexp, s string) bool {
	return re.MatchString(s) || (strings.HasSuffix(s, "\n") && re.MatchString(s[:len(s)-1]))
}

func get(o object, key string) any { return evidence.Get(o, key) }

func has(o object, key string) bool {
	_, ok := evidence.Lookup(o, key)
	return ok
}

func asObject(v any) (object, bool) {
	o, ok := v.(object)
	return o, ok
}

// member is Python's `value in (strings...)`: equality, so a value of another type is never in.
func member(v any, set []string) bool {
	s, ok := v.(string)
	return ok && slices.Contains(set, s)
}

// dictKey hashes a value used as (or inside, as) a dict key. An unhashable one raises the
// TypeError Python 3.14, the relay host's interpreter, raises there.
func dictKey(as string, v any) string {
	switch v.(type) {
	case object, []any:
		name := pyvalue.TypeName(v)
		if as == "" {
			as = name
		}
		panic(&evidence.PythonError{Class: "TypeError", Detail: "cannot use '" + as + "' as a dict key (unhashable type: '" + name + "')"})
	}
	return evidence.HashKey(v)
}

// keyIn is `value in dict`: the value is hashed first, so an unhashable one raises TypeError.
func keyIn(v any, keys ...any) bool {
	hashed := dictKey("", v)
	for _, k := range keys {
		if evidence.HashKey(k) == hashed {
			return true
		}
	}
	return false
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

// normpath is posixpath.normpath, which keeps a leading "//" that path.Clean drops.
func normpath(p string) string {
	cleaned := path.Clean(p)
	if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
		return "/" + cleaned
	}
	return cleaned
}

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
	if !pyMatch(journalDay, name) || strings.HasSuffix(name, "\n") {
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
	return len(parts) == 2 && day(parts[0]) && pyMatch(journalName, parts[1])
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
	said, how := get(row, "stdoutReading"), get(row, "processEnding")
	if !member(said, hook.StdoutReadings) || !member(how, hook.ProcessEndings) {
		return false
	}
	code, signal, errno := get(row, "exitCode"), get(row, "signal"), get(row, "errno")
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
	return count(get(row, "guardElapsedMs"), true) && stderrKept(get(row, "guardStderr"))
}

// couldAnswer is a call that could have produced an answer: a verdict from a clean exit.
func couldAnswer(row object) bool {
	return get(row, "processEnding") == hook.Exited && get(row, "stdoutReading") == hook.SaidAVerdict && exact(get(row, "exitCode"), 0)
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
	outcome := get(row, "adapterOutcome")
	if couldAnswer(row) {
		return outcome == "guard_verdict_incomplete" || outcome == hook.GuardAnswered
	}
	return outcome == outcomeOf(get(row, "processEnding"), get(row, "stdoutReading"), get(row, "exitCode"))
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
	called := get(row, "processEnding") != nil
	answered := get(row, "guardDecision") != nil || get(row, "guardState") != nil || get(row, "assignmentId") != nil || get(row, "guardRecordedAs") != nil || anyAnswerField(row)
	if called {
		if get(row, "guardInvoked") != true || !allFields(row, hook.GuardCallFields) || !callRecorded(row) {
			return false
		}
	} else if get(row, "stdoutReading") != nil || anyField(row, hook.GuardCallFields) {
		return false
	}
	if answered {
		return called && allFields(row, hook.AnswerFields) && member(get(row, "guardDecision"), hook.Decisions) && couldAnswer(row)
	}
	return true
}

// guardResultWritten is whether a record's guard result is one the adapter writes: only an
// answer carries a decision, and it holds exactly when it blocks.
func guardResultWritten(record object) bool {
	outcome, decision := get(record, "adapterOutcome"), get(record, "guardDecision")
	state, held := get(record, "guardState"), get(record, "held")
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
	return decision == nil && state == nil && get(record, "assignmentId") == nil && get(record, "guardRecordedAs") == nil
}

func settledPath(v any, untried bool) bool {
	s, ok := v.(string)
	return ok && isAbs(s) && s == normpath(s) && (untried || hook.PathTheSystemTakes(s))
}

// rowFieldsWritten is whether a row carries every field the adapter writes on the path its
// outcome names, and nothing else.
func rowFieldsWritten(row object) bool {
	if !allFields(row, hook.RowFields) || get(row, "event") != "Stop" {
		return false
	}
	outcome, acceptance, detail := get(row, "adapterOutcome"), get(row, "acceptance"), get(row, "detail")
	allowed := slices.Concat(hook.RowFields, hook.PayloadFields, hook.GuardCallFields, hook.AnswerFields, hook.SettledFields)
	if outcome == hook.AdapterFaulted {
		allowed = append(allowed, hook.FaultFields...)
	}
	for _, f := range row {
		if !slices.Contains(allowed, f.Key) {
			return false
		}
	}
	if get(row, "journalledAs") != nil {
		return false
	}
	if identity, ok := asObject(get(row, "eventIdentity")); ok {
		for _, f := range identity {
			if !slices.Contains(hook.IdentityFields, f.Key) {
				return false
			}
		}
	}
	if !settledPath(get(row, "configuration"), member(outcome, hook.SettingsUntried)) || !count(get(row, "elapsedMs"), true) || !(detail == nil || isString(detail)) {
		return false
	}
	read := allFields(row, hook.PayloadFields)
	if anyField(row, hook.PayloadFields) && !read {
		return false
	}
	mode := get(row, "guardMode")
	if read != member(mode, []string{hook.Observe, hook.Hold}) || (mode != nil && !read) {
		return false
	}
	if acceptance == nil && !hook.NativePrescanUnreachable(row) && ((read && outcome != hook.AdapterFaulted) || get(row, "identityScanMs") != nil) {
		return false
	}
	if member(outcome, slices.Concat(hook.BeforeTheGuard, []string{hook.DuplicateInvocation, hook.ArbitrationFailed})) && !isString(detail) {
		return false
	}
	if keyIn(outcome, hook.DuplicateInvocation, hook.ArbitrationFailed) {
		fixed := map[string]string{hook.DuplicateInvocation: hook.DuplicateDetail, hook.ArbitrationFailed: hook.UnarbitratedDetail}
		if detail != fixed[outcome.(string)] {
			return false
		}
	}
	if member(outcome, hook.FromTheGuard) && (detail == nil) != member(get(row, "processEnding"), []string{hook.Exited, hook.Signalled}) {
		return false
	}
	if outcome == hook.AdapterFaulted {
		if !isString(get(row, "fault")) {
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
		identity, ok := asObject(get(row, "eventIdentity"))
		if !read || !ok || !allFields(identity, hook.IdentityFields) || !count(get(row, "identityScanMs"), true) || !count(get(identity, "scannedBytes"), true) || !count(get(identity, "scannedLines"), true) {
			return false
		}
		if p := get(identity, "transcriptPath"); !(p == nil || isString(p)) {
			return false
		}
	}
	return true
}

// acceptances is every acceptance a row can carry, None (an invocation that reached no event)
// among them, and the outcomes each one ends in besides a fault.
var acceptances = []any{nil, hook.Unestablished, hook.Accepted, hook.Unclaimable, hook.ClaimFailed, hook.Duplicate, hook.Unarbitrated}

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
	if !stamp(get(row, "at")) || !rowFieldsWritten(row) {
		return false
	}
	if hook.NativePrescanUnreachable(row) {
		return true
	}
	acceptance, key := get(row, "acceptance"), get(row, "eventKey")
	outcome, asked := get(row, "adapterOutcome"), get(row, "guardInvoked")
	if !keyIn(acceptance, acceptances...) || !isBool(asked) || !guardResultWritten(row) {
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
		if asked != true || !isString(get(row, "processEnding")) {
			return false
		}
	case (asked == true && acceptance != hook.Duplicate) || get(row, "processEnding") != nil || get(row, "stdoutReading") != nil:
		// A duplicate that says it asked is left to the verdict, which reads it FALSE.
		return false
	}
	k, keyed := key.(string)
	keyed = keyed && pyMatch(ledgerName, k+".json")
	named := nonEmptyString(get(row, "sessionId")) && nonEmptyString(get(row, "turnId"))
	identity, isIdentity := asObject(get(row, "eventIdentity"))
	switch acceptance {
	case hook.Accepted, hook.Duplicate, hook.Unclaimable, hook.ClaimFailed, hook.Unarbitrated:
		if !keyed || !named || !isIdentity || get(identity, "established") != true || get(identity, "reason") != nil {
			return false
		}
		transcript, ok := get(identity, "transcriptPath").(string)
		if !ok || !isAbs(transcript) || !hook.PathTheSystemTakes(transcript) || !isBool(get(row, "stopHookActive")) || !nonEmptyString(get(identity, "answerItem")) {
			return false
		}
		if k != hook.EventKey(get(row, "sessionId"), get(row, "turnId"), get(row, "stopHookActive"), get(identity, "answerItem")) {
			return false
		}
		where := get(row, "acceptedAs")
		switch acceptance {
		case hook.Accepted:
			return where == hook.LedgerDirectory+"/"+k+".json"
		case hook.Duplicate:
			return where == hook.LedgerDirectory+"/"+k+".json" || where == strings.Join(hook.HostLedgerParts, "/")+"/"+k+".json"
		}
		return where == nil
	case hook.Unestablished:
		if key != nil || !isIdentity || get(identity, "established") != false || !member(get(identity, "reason"), hook.UnestablishedReasons) {
			return false
		}
		reason := get(identity, "reason").(string)
		transcript := get(identity, "transcriptPath")
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
			return nonEmptyString(get(identity, "answerItem"))
		}
		return get(identity, "answerItem") == nil
	}
	return key == nil && get(row, "eventIdentity") == nil
}

// hostLedgerNamed is a host ledger as the adapter names one: absolute, normalized, ending in the
// ledger's own two parts, and one the system took.
func hostLedgerNamed(v any) bool {
	s, ok := v.(string)
	if !ok || !isAbs(s) || s != normpath(s) || !hook.PathTheSystemTakes(s) {
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
	if !ok || get(body, "eventKey") != key || !exact(get(body, "ledgerVersion"), hook.LedgerVersion) {
		return false
	}
	if !nonEmptyString(get(body, "sessionId")) || !nonEmptyString(get(body, "turnId")) {
		return false
	}
	if outcome {
		ended := get(body, "adapterOutcome")
		row := get(body, "attemptRow")
		return fieldsExactly(body, hook.OutcomeFields) && stamp(get(body, "at")) && (member(ended, hook.FromTheGuard) || ended == hook.AdapterFaulted) && member(get(body, "journalPolicy"), hook.JournalPolicies) && guardResultWritten(body) && (row == nil || slot(row))
	}
	claimedBy, _ := asObject(get(body, "claimedBy"))
	return fieldsExactly(body, hook.ClaimFields) && fieldsExactly(get(body, "claimedBy"), hook.ClaimedByFields) && stamp(get(body, "claimedAt")) && isBool(get(body, "stopHookActive")) && nonEmptyString(get(body, "answerItem")) && slot(get(claimedBy, "attemptRow")) && count(get(claimedBy, "pid"), false) && hostLedgerNamed(get(claimedBy, "hostLedger")) && key == hook.EventKey(get(body, "sessionId"), get(body, "turnId"), get(body, "stopHookActive"), get(body, "answerItem"))
}

// hostShape is whether a host file carries every field it is written with, under its own key.
func hostShape(v any, key string) bool {
	body, ok := asObject(v)
	if !ok || get(body, "eventKey") != key || !exact(get(body, "ledgerVersion"), hook.LedgerVersion) {
		return false
	}
	for _, f := range []string{"sessionId", "turnId", "answerItem"} {
		if !nonEmptyString(get(body, f)) {
			return false
		}
	}
	if !stamp(get(body, "claimedAt")) {
		return false
	}
	claimedBy, _ := asObject(get(body, "claimedBy"))
	root := get(claimedBy, "journalRoot")
	rootOK := root == nil
	if s, ok := root.(string); ok {
		rootOK = isAbs(s)
	}
	return fieldsExactly(body, hook.ClaimFields) && fieldsExactly(get(body, "claimedBy"), hook.HostClaimedByFields) && isBool(get(body, "stopHookActive")) && slot(get(claimedBy, "attemptRow")) && count(get(claimedBy, "pid"), false) && rootOK && key == hook.EventKey(get(body, "sessionId"), get(body, "turnId"), get(body, "stopHookActive"), get(body, "answerItem"))
}
