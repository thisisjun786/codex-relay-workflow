//go:build dev

// Package trialledger is `crw-dev trial-ledger`: a finished live trial's intervention ledger,
// graded (scripts/trial_startup.py's ledger mode, ported by property). It reads the trial root
// the start record names and nothing else: no relay, host record or capture, so a trial stays
// gradable after the installation it ran against is upgraded or removed (docs/live-trial.md).
// Preparation and the window are separated by timestamp rather than by what a line calls
// itself, and a window with any intervention inside it is not an uninterrupted result.
//
// The document is byte-compatible with the Python ledger's (source live-trial-startup), so a
// Python-era grade and a Go-era grade of the same trial compare. It prints the document and
// exits 0 when every judgment passed, 1 when one failed; a trial it cannot grade is a refusal
// document and exit 2.
package trialledger

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

const (
	source         = "live-trial-startup"
	checkerVersion = 1
	preparation    = "preparation"
	window         = "window"
	note           = "classification is by timestamp and by nothing else, and a window with any intervention inside it is not an uninterrupted result. What this cannot see is an intervention nobody wrote down."
)

// The record versions a finished trial can still be graded at: ledger grading reads the trial
// root and the window, and version 2 changed neither.
var ledgerRecordVersions = []any{int64(1), int64(2)}

var ledgerKinds = []string{"segment_start", "segment_end", "window_open", "window_close", "intervention", "dispatch"}

type object = contract.OrderedObject

func o(fields ...any) object {
	out := make(object, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		out = append(out, contract.Field{Key: fields[i].(string), Value: fields[i+1]})
	}
	return out
}

// refusal is a run that cannot be made at all: it prints why and produces no grade.
type refusal struct {
	reason string
	detail object
}

func (r *refusal) Error() string { return r.reason }

func refuse(reason string, detail ...any) *refusal { return &refusal{reason, o(detail...)} }

func (r *refusal) record() object {
	return o("source", source, "checkerVersion", checkerVersion, "refused", r.reason, "detail", r.detail)
}

// clock is the grading's own time; tests pin it.
var clock = time.Now

// stampNow is how the Python grader writes the time it graded at: ISO 8601 in UTC with a Z,
// microseconds only when there are any.
func stampNow() string {
	now := clock().UTC()
	if now.Nanosecond()/1000 == 0 {
		return now.Format("2006-01-02T15:04:05Z")
	}
	return now.Format("2006-01-02T15:04:05.000000Z")
}

const usage = "usage: crw-dev trial-ledger [-h] --start START"

const help = `Grade a finished live trial's intervention ledger (docs/live-trial.md).

  --start START  the start record under the private trial root

Exit 0 when every judgment passed, 1 when one failed, 2 when the trial cannot be graded.`

// Run is `crw-dev trial-ledger --start <start.json>`.
func Run(args []string, stdout, stderr io.Writer) int {
	usageError := func(message string) int {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev trial-ledger: error: "+message)
		return 2
	}
	var start *string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			fmt.Fprintln(stdout, usage+"\n\n"+help)
			return 0
		case arg == "--start":
			if i+1 >= len(args) || (strings.HasPrefix(args[i+1], "-") && args[i+1] != "-") {
				return usageError("argument --start: expected one argument")
			}
			i++
			value := args[i]
			start = &value
		case strings.HasPrefix(arg, "--start="):
			value := strings.TrimPrefix(arg, "--start=")
			start = &value
		default:
			return usageError("unrecognized arguments: " + strings.Join(args[i:], " "))
		}
	}
	if start == nil {
		return usageError("the following arguments are required: --start")
	}
	document, failed, err := Grade(*start)
	if err != nil {
		var refused *refusal
		if !errors.As(err, &refused) {
			refused = refuse("this run raised before it could report", "exception", exceptionName(err), "detail", exceptionText(err), "raisedAt", nil)
		}
		fmt.Fprintln(stdout, evidence.DumpsIndent(refused.record(), 2, true, true))
		return 2
	}
	fmt.Fprintln(stdout, evidence.DumpsIndent(document, 2, true, true))
	if failed {
		return 1
	}
	return 0
}

// exceptionText is str(error): a Python exception's message without its class.
func exceptionText(err error) string {
	var python *evidence.PythonError
	if errors.As(err, &python) {
		return python.Detail
	}
	return err.Error()
}

func exceptionName(err error) string {
	var python *evidence.PythonError
	if errors.As(err, &python) {
		return python.Class
	}
	return "RuntimeError"
}

// Grade reads the start record, grades its trial's ledger and returns the document and whether
// a judgment in it failed. A trial it cannot grade is a *refusal error.
func Grade(startPath string) (document object, failed bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			if python, ok := p.(*evidence.PythonError); ok {
				err = python
			} else {
				err = fmt.Errorf("%v", p)
			}
		}
	}()
	record, ledgerPath, err := loadStart(startPath)
	if err != nil {
		return nil, false, err
	}
	document, err = report(record, ledgerPath)
	if err != nil {
		return nil, false, err
	}
	counted := judgments(document, "")
	var failing []any
	for _, j := range counted {
		if !j.value {
			failing = append(failing, j.at)
		}
	}
	if failing == nil {
		failing = []any{}
	}
	document = append(document, contract.Field{Key: "judgmentsCounted", Value: len(counted)}, contract.Field{Key: "judgmentsThatFailed", Value: failing})
	return document, len(failing) > 0, nil
}

// loadStart reads the record, or refuses. Grading a ledger uses the trial root, the window and
// nothing else.
func loadStart(value string) (object, string, error) {
	start, err := absolute(value, "--start")
	if err != nil {
		return nil, "", err
	}
	state, detail, body := readJSON(start, "the start record")
	if state != present {
		return nil, "", refuse("the start record could not be read", "path", start, "state", state, "detail", detail)
	}
	record, ok := body.(object)
	if !ok {
		return nil, "", refuse("the start record is not a JSON object", "path", start)
	}
	if get(record, "source") != "live-trial-start" {
		return nil, "", refuse("this file does not stamp itself as a live trial start record", "path", start, "source", get(record, "source"))
	}
	version := get(record, "recordVersion")
	if !slices.ContainsFunc(ledgerRecordVersions, func(v any) bool { return evidence.Equal(version, v) }) {
		return nil, "", refuse("unsupported record version", "path", start, "recordVersion", version, "supported", ledgerRecordVersions)
	}
	root, err := absolute(get(record, "trialRoot"), "trialRoot")
	if err != nil {
		return nil, "", err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, "", refuse("the trial root is not a directory", "trialRoot", root)
	}
	if worktree := gitWorktreeOf(root); worktree != "" {
		return nil, "", refuse("the trial root is inside a git worktree, and operational state never lives inside a repository", "trialRoot", root, "worktree", worktree)
	}
	if !within(start, root) {
		return nil, "", refuse("the start record itself is outside the trial root", "path", start, "trialRoot", root)
	}
	// The ledger is a private trial record like the captures, read by path, so it is confined
	// the same way rather than followed wherever a link points.
	ledger := join(root, "ledger.jsonl")
	if !within(ledger, root) {
		return nil, "", refuse("the ledger resolves outside the trial root", "path", ledger, "trialRoot", root)
	}
	for _, item := range []string{start, ledger} {
		if nested := gitWorktreeOf(item); nested != "" {
			return nil, "", refuse("a private trial record is inside a git worktree", "path", item, "worktree", nested)
		}
	}
	return record, ledger, nil
}

func get(o object, key string) any { return evidence.Get(o, key) }

// field is the value at a path of object keys, or missing.
func field(value any, path ...string) (any, bool) {
	for _, key := range path {
		o, ok := value.(object)
		if !ok {
			return nil, false
		}
		if value, ok = evidence.Lookup(o, key); !ok {
			return nil, false
		}
	}
	return value, true
}

func absolute(value any, what string) (string, error) {
	s, ok := value.(string)
	if !ok || !strings.HasPrefix(s, "/") {
		return "", refuse(what+" must be an absolute path", "value", value)
	}
	if strings.IndexByte(s, 0) >= 0 {
		return "", refuse(what+" holds a NUL byte, which no path can carry", "value", s)
	}
	return pathlibForm(s), nil
}

// entry is one ledger line with its time and line number.
type entry struct {
	body object
	at   time.Time
	line int
}

func (e entry) get(key string) any { return get(e.body, key) }

// moment is one timestamp, or a refusal naming what could not be read as one.
func moment(value any, what string) (time.Time, error) {
	s, ok := value.(string)
	if !ok || pyStrip(s) == "" {
		return time.Time{}, refuse(what+" is not a timestamp", "value", value)
	}
	text := pyStrip(s)
	if strings.HasSuffix(text, "Z") {
		text = text[:len(text)-1] + "+00:00"
	}
	at, aware, err := fromISOFormat(text)
	if err != nil {
		return time.Time{}, refuse(what+" is not an ISO-8601 timestamp", "value", value, "detail", err.Error())
	}
	if !aware {
		return time.Time{}, refuse(what+" does not name a UTC offset, and a time without one is not a moment this can place", "value", value)
	}
	return at, nil
}

type segment struct {
	name                       string
	opensAt, closesAt, outcome any
	from, to                   *time.Time
	interventions              []object
}

// report grades the ledger: preparation and the window, separated by timestamp.
func report(record object, path string) (object, error) {
	state, _, text := readText(path)
	if state != present {
		return nil, refuse("the ledger could not be read", "path", path, "state", state)
	}
	var entries []entry
	for number, line := range pySplitLines(text) {
		number++
		if pyStrip(line) == "" {
			continue
		}
		value, err := pyload.Loads([]byte(line))
		if python, deep := pyload.Recursion(err); deep {
			panic(python) // `except ValueError` does not catch a RecursionError
		}
		if err != nil {
			return nil, refuse("a ledger line is not JSON", "line", number, "detail", err.Error())
		}
		body, isObject := value.(object)
		if !isObject || !member(get(body, "kind"), ledgerKinds) {
			var kind any
			if isObject {
				kind = get(body, "kind")
			}
			return nil, refuse("a ledger line carries no known kind", "line", number, "kind", kind)
		}
		if segmentName, has := evidence.Lookup(body, "segment"); has {
			if _, text := segmentName.(string); !text {
				return nil, refuse("a ledger line's segment has to be written as text", "line", number, "found", evidence.Dumps(segmentName, false, false, true))
			}
		}
		at, err := moment(get(body, "at"), "a ledger line's at")
		if err != nil {
			return nil, err
		}
		if at.After(clock()) {
			// The ledger is appended as things happen, so a line dated after the moment it is
			// graded did not happen.
			return nil, refuse("a ledger line is dated after the time it is being graded", "line", number, "at", get(body, "at"), "now", stampNow())
		}
		entries = append(entries, entry{body, at, number})
	}
	of := func(kind string) []entry {
		var found []entry
		for _, e := range entries {
			if e.get("kind") == kind {
				found = append(found, e)
			}
		}
		return found
	}
	opens, closes, dispatches := of("window_open"), of("window_close"), of("dispatch")
	if len(opens) > 1 || len(closes) > 1 {
		return nil, refuse("a trial has one window", "opened", len(opens), "closed", len(closes))
	}
	if len(opens) == 0 || len(closes) == 0 {
		return nil, refuse("the window is not bounded in this ledger", "opened", len(opens), "closed", len(closes))
	}
	// The window is the interval the dispatch opened, not one declared ahead of it.
	if len(dispatches) != 1 {
		return nil, refuse("a trial has one dispatch, and the window opens at it", "dispatched", len(dispatches))
	}
	if !dispatches[0].at.Equal(opens[0].at) {
		return nil, refuse("the window does not open at the dispatch, so the interval between them is measured as preparation", "dispatchedAt", dispatches[0].get("at"), "opensAt", opens[0].get("at"))
	}
	if !evidence.Truthy(opens[0].get("segment")) || !evidence.Equal(opens[0].get("segment"), closes[0].get("segment")) {
		return nil, refuse("the window's open and close name different segments", "opensAt", opens[0].get("at"), "opensSegment", opens[0].get("segment"), "closesAt", closes[0].get("at"), "closesSegment", closes[0].get("segment"))
	}
	opened, closed := opens[0].at, closes[0].at
	if !closed.After(opened) {
		// A point interval holds none of the round trip the trial exists to measure.
		return nil, refuse("this window has no duration to measure", "opensAt", opens[0].get("at"), "closesAt", closes[0].get("at"))
	}
	if closed.After(clock()) {
		return nil, refuse("this window has not closed yet, so there is nothing to grade", "closesAt", closes[0].get("at"), "now", stampNow())
	}
	// The record declares the window and the ledger records it: they have to be the same one.
	for _, bound := range []struct {
		key   string
		event entry
	}{{"opensAt", opens[0]}, {"closesAt", closes[0]}} {
		declared, has := field(record, "window", bound.key)
		if !has || declared == nil {
			return nil, refuse("the start record does not declare window." + bound.key)
		}
		at, err := moment(declared, "window."+bound.key)
		if err != nil {
			return nil, err
		}
		if !at.Equal(bound.event.at) {
			return nil, refuse("the ledger's window does not match the one the record declares", "field", bound.key, "declared", declared, "ledger", bound.event.get("at"), "line", bound.event.line)
		}
	}

	// Segments are intervals, built from their own timestamps and then checked for intersection.
	byName := map[string]*segment{}
	var order []*segment
	for _, e := range entries {
		kind := e.get("kind")
		if kind != "segment_start" && kind != "segment_end" {
			continue
		}
		name, _ := e.get("segment").(string)
		if name == "" {
			return nil, refuse("a segment boundary names no segment", "line", e.line)
		}
		found := byName[name]
		if found == nil {
			found = &segment{name: name}
			byName[name] = found
			order = append(order, found)
		}
		at := e.at
		if kind == "segment_start" {
			if found.from != nil {
				return nil, refuse("a segment starts twice", "segment", name, "line", e.line)
			}
			found.from, found.opensAt = &at, e.get("at")
		} else {
			if found.to != nil {
				return nil, refuse("a segment ends twice", "segment", name, "line", e.line)
			}
			found.to, found.closesAt, found.outcome = &at, e.get("at"), e.get("outcome")
		}
	}
	ordered := slices.Clone(order)
	slices.SortStableFunc(ordered, func(a, b *segment) int {
		switch {
		case a.from == nil && b.from == nil:
			return 0
		case a.from == nil:
			return 1
		case b.from == nil:
			return -1
		}
		return a.from.Compare(*b.from)
	})
	for _, s := range ordered {
		switch {
		case s.from == nil:
			return nil, refuse("a segment ends without starting", "segment", s.name)
		case s.to == nil:
			return nil, refuse("a segment never closes, so what it attempted was never recorded", "segment", s.name, "opensAt", s.opensAt)
		case s.outcome != "failed" && s.outcome != "succeeded":
			return nil, refuse("a segment closes without saying whether it failed or succeeded", "segment", s.name, "outcome", s.outcome)
		case s.to.Before(*s.from):
			return nil, refuse("a segment closes before it opens", "segment", s.name)
		}
	}
	for i := 0; i+1 < len(ordered); i++ {
		first, second := ordered[i], ordered[i+1]
		if !second.from.After(*first.to) {
			return nil, refuse("two segments overlap in time", "earlier", first.name, "later", second.name, "earlierClosesAt", first.closesAt, "laterOpensAt", second.opensAt)
		}
	}
	for _, s := range ordered {
		// A preparation segment running through the window means preparation was still going on.
		if !s.from.After(closed) && !opened.After(*s.to) {
			return nil, refuse("a preparation segment overlaps the trial window", "segment", s.name, "opensAt", s.opensAt, "closesAt", s.closesAt, "windowOpensAt", opens[0].get("at"), "windowClosesAt", closes[0].get("at"))
		}
	}

	var prepared, inside []any
	for _, e := range entries {
		if e.get("kind") != "intervention" {
			continue
		}
		computed := preparation
		if !e.at.Before(opened) && !e.at.After(closed) {
			computed = window
		}
		if claimed := e.get("claimed"); claimed != nil && !evidence.Equal(claimed, computed) {
			return nil, refuse("a ledger line's claimed class disagrees with its own timestamp", "line", e.line, "at", e.get("at"), "claimed", claimed, "computed", computed)
		}
		item := o("at", e.get("at"), "actor", e.get("actor"), "target", e.get("target"), "action", e.get("action"), "class", computed, "line", e.line)
		// The operator's own words travel into the report as words, never as structure a
		// judgment walk could read a verdict from.
		for _, name := range []string{"at", "actor", "target", "action"} {
			if _, text := get(item, name).(string); !text {
				return nil, refuse("a ledger line's "+name+" has to be written as text", "line", e.line, "found", evidence.Dumps(get(item, name), false, false, true))
			}
		}
		if computed == window {
			inside = append(inside, item)
			continue
		}
		prepared = append(prepared, item)
		for _, s := range ordered {
			if !s.from.After(e.at) && !e.at.After(*s.to) {
				s.interventions = append(s.interventions, item)
			}
		}
	}

	// Corroboration is compared, not accepted.
	provenance, compared := "declared", []any{}
	var corroboration any
	if supplied, has := field(record, "window", "corroboration"); has && supplied != nil {
		times, isObject := supplied.(object)
		if !isObject {
			return nil, refuse("window.corroboration is not an object of times", "corroboration", supplied)
		}
		kept := object{}
		for _, bound := range []struct {
			key      string
			declared any
		}{{"opensAt", opens[0].get("at")}, {"closesAt", closes[0].get("at")}} {
			given := get(times, bound.key)
			if given == nil {
				continue
			}
			at, err := moment(given, "window.corroboration."+bound.key)
			if err != nil {
				return nil, err
			}
			declared, err := moment(bound.declared, bound.key)
			if err != nil {
				return nil, err
			}
			if !at.Equal(declared) {
				return nil, refuse("a corroborating time disagrees with the window it corroborates", "field", bound.key, "corroborating", given, "declared", bound.declared)
			}
			compared = append(compared, bound.key)
			kept = append(kept, contract.Field{Key: bound.key, Value: given})
		}
		if len(compared) == 0 {
			return nil, refuse("window.corroboration names no time to compare")
		}
		provenance, corroboration = "corroborated", kept
	}
	segments, failedSegments := []any{}, []any{}
	for _, s := range ordered {
		segments = append(segments, o("name", s.name, "opensAt", s.opensAt, "closesAt", s.closesAt, "outcome", s.outcome, "interventions", len(s.interventions)))
		if s.outcome == "failed" {
			failedSegments = append(failedSegments, s.name)
		}
	}
	clean := len(inside) == 0
	return o(
		"source", source, "checkerVersion", checkerVersion, "ledger", path,
		"preparation", o("interventions", len(prepared), "entries", orEmpty(prepared), "segments", segments, "failedSegments", failedSegments),
		"window", o("opensAt", opens[0].get("at"), "closesAt", closes[0].get("at"), "provenance", provenance, "corroborated", compared, "corroboration", corroboration, "dispatchedAt", dispatches[0].get("at"), "interventions", len(inside), "entries", orEmpty(inside), "windowIsClean", clean, "passed", clean),
		"note", note,
	), nil
}

func orEmpty(items []any) []any {
	if items == nil {
		return []any{}
	}
	return items
}

func member(v any, set []string) bool {
	s, ok := v.(string)
	return ok && slices.Contains(set, s)
}

type judgment struct {
	at    string
	value bool
}

// judgments is every field named passed or met, found by walking what was assembled rather than
// from a list of the kinds that produce judgments.
func judgments(node any, path string) []judgment {
	var found []judgment
	switch v := node.(type) {
	case object:
		for _, f := range v {
			here := f.Key
			if path != "" {
				here = path + "." + f.Key
			}
			if b, ok := f.Value.(bool); ok && (f.Key == "passed" || f.Key == "met") {
				found = append(found, judgment{here, b})
				continue
			}
			found = append(found, judgments(f.Value, here)...)
		}
	case []any:
		for i, item := range v {
			found = append(found, judgments(item, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return found
}
