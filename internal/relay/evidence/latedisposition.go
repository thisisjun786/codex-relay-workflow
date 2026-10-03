package evidence

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// A review thread that appears on the candidate's head after a child wrote its record is not in
// the record's threadsSeen, so the fresh reading reports it as a late finding and the record goes
// back to the child. When the thread is one the coordinator can judge itself, it answers, defers,
// resolves or refutes the thread and states that judgement in a dispositions document handed to
// merge-evidence --restate. A late thread with an entry for the head under restatement is no longer
// a late finding. The relay records the grade the coordinator gave and never reads it: whether a
// thread is minor enough to judge here is the coordinator's decision, not a rule of this package.

// The dispositions a coordinator can record for a late thread.
const (
	DispositionAnswered = "answered"
	DispositionBacklog  = "backlog"
	DispositionResolved = "resolved"
	DispositionRefuted  = "refuted"
)

// LateDispositions is the closed set of dispositions, in the order the documents state them.
var LateDispositions = []string{DispositionAnswered, DispositionBacklog, DispositionResolved, DispositionRefuted}

// LateDispositionsMember is the member of the dispositions document that holds the entries.
const LateDispositionsMember = "lateDispositions"

// LateDispositionFields are the members every entry states, all of them strings.
var LateDispositionFields = []string{"threadId", "disposition", "evidenceUrl", "head", "grade"}

// What the restatement says an entry did, and why an entry that did nothing did nothing.
const (
	LateApplied       = "applied"
	LateIgnored       = "ignored"
	LateOtherHead     = "other_head"
	LateNotLate       = "not_late"
	LateUnknownThread = "unknown_thread"
)

// LateEntry is one coordinator disposition of one late review thread on one head. Its strings are
// exactly what the document says: a thread id and a head are compared as written, and the grade is
// echoed as written.
type LateEntry struct{ ThreadID, Disposition, EvidenceURL, Head, Grade string }

// ReadLateDispositions reads a decoded dispositions document, an object whose lateDispositions
// member lists the entries. A document with any malformed entry is rejected whole and yields no
// entries, so a typo can never leave half of a judgement applied.
func ReadLateDispositions(document any) ([]LateEntry, []Problem) {
	var problems []Problem
	bad := func(detail string) { problems = append(problems, Problem{Code: Malformed, Detail: detail}) }
	o, isObject := Object(document)
	if !isObject {
		bad("the late-thread dispositions document is an object stating " + LateDispositionsMember + ", not " + quote.Kind(document))
		return nil, problems
	}
	member, present := o.Lookup(LateDispositionsMember)
	if !present {
		bad("the late-thread dispositions document does not state " + LateDispositionsMember)
		return nil, problems
	}
	items, isList := List(member)
	if !isList {
		bad(LateDispositionsMember + " is a list of entries stating " + strings.Join(LateDispositionFields, ", ") + ", not " + quote.Kind(member))
		return nil, problems
	}
	type pair struct{ thread, head string }
	firstAt := map[pair]int{}
	var entries []LateEntry
	for position, raw := range items {
		where := "late disposition entry " + strconv.Itoa(position)
		entryObject, isObject := Object(raw)
		if !isObject {
			bad(where + " is an object stating " + strings.Join(LateDispositionFields, ", ") + ", not " + quote.Kind(raw))
			continue
		}
		var entry LateEntry
		slots := map[string]*string{"threadId": &entry.ThreadID, "disposition": &entry.Disposition, "evidenceUrl": &entry.EvidenceURL, "head": &entry.Head, "grade": &entry.Grade}
		complete := true
		for _, field := range LateDispositionFields {
			value, present := entryObject.Lookup(field)
			text, isString := value.(string)
			switch {
			case !present:
				bad(where + " does not state " + field)
			case !isString:
				bad(where + " states " + field + " as " + quote.Kind(value) + ", not a string; coercing it would let two different values agree")
			case strings.TrimSpace(text) == "":
				bad(where + " states a blank " + field)
			default:
				*slots[field] = text
				continue
			}
			complete = false
		}
		if !complete {
			continue
		}
		if !slices.Contains(LateDispositions, entry.Disposition) {
			bad(where + " states disposition " + quote.Value(entry.Disposition) + ", which is not one of " + strings.Join(LateDispositions, ", "))
			continue
		}
		if u, err := url.Parse(entry.EvidenceURL); err != nil || u.Hostname() == "" || u.Scheme != "http" && u.Scheme != "https" {
			bad(where + " states evidenceUrl " + quote.Value(entry.EvidenceURL) + ", which is not an http or https URL; the evidence is something a reader can open")
			continue
		}
		if !shaPattern.MatchString(entry.Head) {
			bad(where + " states head " + quote.Value(entry.Head) + ", which is not a commit sha")
			continue
		}
		key := pair{entry.ThreadID, entry.Head}
		if earlier, repeated := firstAt[key]; repeated {
			bad(where + " repeats the disposition of entry " + strconv.Itoa(earlier) + ": thread " + quote.Value(entry.ThreadID) + " on head " + quote.Value(entry.Head) + ", and two entries for one thread on one head cannot both be the judgement")
			continue
		}
		firstAt[key] = position
		entries = append(entries, entry)
	}
	if len(problems) > 0 {
		return nil, problems
	}
	return entries, nil
}

// lateResults says what each entry did against the fresh reading: threads maps every review thread
// the fresh reading shows to whether the restated record listed it in threadsSeen. An entry that
// did nothing says why, in this order of precedence: it names another head, no such thread is
// shown, or the record already lists the thread.
func lateResults(entries []LateEntry, head string, threads map[string]bool) []any {
	results := make([]any, 0, len(entries))
	for _, e := range entries {
		effect, reason := LateApplied, ""
		listed, shown := threads[e.ThreadID]
		switch {
		case e.Head != head:
			effect, reason = LateIgnored, LateOtherHead
		case !shown:
			effect, reason = LateIgnored, LateUnknownThread
		case listed:
			effect, reason = LateIgnored, LateNotLate
		}
		result := map[string]any{"threadId": e.ThreadID, "disposition": e.Disposition, "evidenceUrl": e.EvidenceURL, "head": e.Head, "grade": e.Grade, "effect": effect}
		if reason != "" {
			result["reason"] = reason
		}
		results = append(results, result)
	}
	return results
}
