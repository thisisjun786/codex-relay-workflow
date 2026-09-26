package delivery

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Management intent (intent.py): what a coordinator publishes before the task it is about
// exists. The assignment state is DERIVED from which facts exist, never stored.

// BindingWindowMinutes is BINDING_WINDOW_MINUTES.
const BindingWindowMinutes = 30

// Derived assignment states and create-once outcomes (intent.py).
const (
	RelationshipRegistered = "relationship_registered"
	IdentityBound          = "identity_bound"
	AmbiguousIdentity      = "ambiguous_identity"
	IntentExpired          = "intent_expired"
	CreationUnknown        = "creation_unknown"
	CreationAccepted       = "creation_accepted"
	IntentDeclared         = "intent_declared"

	Bound     = "bound"
	Unchanged = "unchanged"
	Conflict  = "conflict"

	DispatchCurrent = "current"
	DispatchStale   = "stale"
	DispatchAbsent  = "absent"
)

// Refusal reasons the marker writers raise that the delivery package did not already name.
const (
	RelationshipConflict = "relationship_conflict"
	UnboundGeneration    = "unbound_generation"
	OutcomeInconsistent  = "outcome_inconsistent"
)

// AttemptOutcomes is ATTEMPT_OUTCOMES.
var AttemptOutcomes = []string{"accepted", "unknown", "failed"}

// DispositionOutcomes is DISPOSITION_OUTCOMES: exhaustive, not illustrative.
var DispositionOutcomes = []string{"in_progress", "blocked_needs_input", "interrupted", "failed", "ready_for_review"}

// intentFields is INTENT_FIELDS: everything an intent means, which is everything but when.
var intentFields = []string{"dispatchRequestIdHash", "issueKey", "workspace", "dbPath", "criteriaSource", "baselineRevision", "authorizedSettings"}

var singleKeys = []string{"intent", "bound", "relationship"}
var listedKeys = []string{"attempts", "claims", "conflicts", "resolutions"}

// identityFields is IDENTITY_FIELDS: every identity slot a fact may carry, in Python's order.
var identityFields = map[string][]string{
	"intent":       {"dispatchRequestIdHash", "dbPath"},
	"bound":        {"sessionId", "taskId"},
	"relationship": {"relationshipId"},
	"attempts":     {"taskId", "outcome"},
	"claims":       {"sessionId", "dispatchRequestId"},
	"conflicts":    {"attemptedSessionId", "attemptedTaskId"},
	"resolutions":  {"chosenTaskId", "chosenSessionId"},
}

// numberFields is NUMBER_FIELDS: a present value must be a positive integer.
var numberFields = map[string][]string{"relationship": {"executionGeneration"}}

var isoOffset = regexp.MustCompile(`([+-]\d{2}):?(\d{2})(?::?(\d{2}))?$`)

// Moment is intent.moment: datetime.fromisoformat, "Z" read as UTC and a naive stamp as UTC;
// nil for a value that is not a timestamp.
func Moment(value any) *time.Time {
	text, ok := value.(string)
	if !ok {
		if value == nil {
			return nil
		}
		text = pyStr(value)
	}
	text = strings.Replace(text, "Z", "+00:00", 1)
	zone := time.UTC
	if m := isoOffset.FindStringSubmatchIndex(text); m != nil && len(text) > 10 {
		sign, hh, mm := text[m[2]], text[m[2]+1:m[3]], text[m[4]:m[5]]
		ss := "00"
		if m[6] >= 0 {
			ss = text[m[6]:m[7]]
		}
		h, _ := strconv.Atoi(hh)
		mi, _ := strconv.Atoi(mm)
		se, _ := strconv.Atoi(ss)
		offset := h*3600 + mi*60 + se
		if sign == '-' {
			offset = -offset
		}
		zone = time.FixedZone("", offset)
		text = text[:m[0]]
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, text, zone); err == nil {
			return &t
		}
	}
	return nil
}

// ---------------------------------------------------------------- shape

// Malformed is malformed: the first published record that is not the shape a fact must be.
func Malformed(marker Obj) string {
	for _, key := range singleKeys {
		if v, ok := get(marker, key); ok {
			if _, record := v.(Obj); !record {
				return key
			}
		}
	}
	for _, key := range listedKeys {
		v, ok := get(marker, key)
		if !ok {
			continue
		}
		items, list := v.([]any)
		if !list {
			return key
		}
		for _, item := range items {
			if _, record := item.(Obj); !record {
				return key
			}
		}
	}
	for _, key := range append(append([]string{}, singleKeys...), listedKeys...) {
		v, ok := get(marker, key)
		if !ok {
			continue
		}
		items := []any{v}
		if list, isList := v.([]any); isList {
			items = list
		}
		for _, raw := range items {
			item := raw.(Obj)
			for _, field := range identityFields[key] {
				if value, present := get(item, field); present {
					if _, text := value.(string); !text {
						return key + "." + field
					}
				}
			}
			for _, field := range numberFields[key] {
				value, present := get(item, field)
				if !present {
					continue
				}
				if n, integer := value.(int64); !integer || n < 1 {
					return key + "." + field
				}
			}
			if key == "resolutions" {
				if entries, present := get(item, "adjudicated"); present {
					list, isList := entries.([]any)
					if !isList {
						return key + ".adjudicated"
					}
					for _, entry := range list {
						if _, record := entry.(Obj); !record {
							return key + ".adjudicated"
						}
					}
				}
			}
		}
	}
	return ""
}

// MalformedCounters is malformed_counters: a count that is not a count is corruption.
func MalformedCounters(counters any) string {
	if counters == nil {
		return ""
	}
	record, ok := counters.(Obj)
	if !ok {
		return "counters"
	}
	for _, f := range record {
		if n, integer := f.Value.(int64); !integer || n < 0 {
			return "counters." + f.Key
		}
	}
	return ""
}

// ---------------------------------------------------------------- coverage

func objects(items []any) []Obj {
	out := make([]Obj, 0, len(items))
	for _, item := range items {
		record, _ := item.(Obj)
		out = append(out, record)
	}
	return out
}

func resolutionsOf(marker Obj) []Obj { return objects(markerFactList(marker, "resolutions")) }

// FactCovered is covered: this exact fact, by identity AND digest, adjudicated by a resolution.
func FactCovered(fact Obj, resolutions []Obj) bool {
	factID := fieldOf(fact, "factId")
	if !Named(factID) {
		return false
	}
	digest := FactDigest(fact)
	for _, resolution := range resolutions {
		entries, _ := fieldOf(resolution, "adjudicated").([]any)
		for _, raw := range entries {
			entry, ok := raw.(Obj)
			if !ok {
				continue
			}
			if fieldOf(entry, "factId") == factID && fieldOf(entry, "digest") == digest {
				return true
			}
		}
	}
	return false
}

// Claimant is claimant: the session a claim belongs to, from the path that authorised the write,
// and only when the body names that same session. nil when it owns nothing.
func Claimant(claim Obj) any {
	parts := strings.Split(pyStrOr(fieldOf(claim, "factId")), "/")
	if len(parts) != 3 || parts[0] != "claims" || parts[2] != claimFile {
		return nil
	}
	owner := parts[1]
	if owner == "." || owner == ".." || !Named(owner) {
		return nil
	}
	body := fieldOf(claim, "sessionId")
	if !Named(body) || body != owner {
		return nil
	}
	return owner
}

// pyStrOr is str(value or ""): Python's falsy values read as the empty string.
func pyStrOr(value any) string {
	if !truthy(value) {
		return ""
	}
	return pyStr(value)
}

func acceptedTasks(marker Obj) map[any]bool {
	tasks := map[any]bool{}
	for _, attempt := range objects(markerFactList(marker, "attempts")) {
		if fieldOf(attempt, "outcome") == "accepted" && Named(fieldOf(attempt, "taskId")) {
			tasks[fieldOf(attempt, "taskId")] = true
		}
	}
	return tasks
}

func competingFacts(marker Obj) []Obj {
	bound := markerFact(marker, "bound")
	var facts []Obj
	for _, attempt := range objects(markerFactList(marker, "attempts")) {
		if fieldOf(attempt, "outcome") == "accepted" && Named(fieldOf(attempt, "taskId")) && !SameIdentity(fieldOf(attempt, "taskId"), fieldOf(bound, "taskId")) {
			facts = append(facts, attempt)
		}
	}
	for _, claim := range objects(markerFactList(marker, "claims")) {
		if !SameIdentity(Claimant(claim), fieldOf(bound, "sessionId")) {
			facts = append(facts, claim)
		}
	}
	return append(facts, objects(markerFactList(marker, "conflicts"))...)
}

func ambiguityResolved(marker Obj) bool {
	var resolutions []Obj
	for _, r := range resolutionsOf(marker) {
		if Named(fieldOf(r, "chosenTaskId")) && Named(fieldOf(r, "chosenSessionId")) {
			resolutions = append(resolutions, r)
		}
	}
	if len(resolutions) == 0 {
		return false
	}
	pairs := map[[2]any]bool{}
	for _, r := range resolutions {
		pairs[[2]any{fieldOf(r, "chosenTaskId"), fieldOf(r, "chosenSessionId")}] = true
	}
	if len(pairs) != 1 {
		return false
	}
	var chosen [2]any
	for pair := range pairs {
		chosen = pair
	}
	sessions := map[any]bool{}
	for _, claim := range objects(markerFactList(marker, "claims")) {
		if owner := Claimant(claim); owner != nil {
			sessions[owner] = true
		}
	}
	if !acceptedTasks(marker)[chosen[0]] || !sessions[chosen[1]] {
		return false
	}
	var facts []Obj
	for _, attempt := range objects(markerFactList(marker, "attempts")) {
		if fieldOf(attempt, "outcome") == "accepted" && Named(fieldOf(attempt, "taskId")) {
			facts = append(facts, attempt)
		}
	}
	facts = append(facts, objects(markerFactList(marker, "claims"))...)
	for _, fact := range facts {
		if !FactCovered(fact, resolutions) {
			return false
		}
	}
	return true
}

// IdentityContested is identity_contested: a competing fact after a bind that no resolution
// naming the BOUND identity has adjudicated.
func IdentityContested(marker Obj) bool {
	bound := markerFact(marker, "bound")
	if len(bound) == 0 {
		return false
	}
	var applicable []Obj
	for _, r := range resolutionsOf(marker) {
		if SameIdentity(fieldOf(r, "chosenSessionId"), fieldOf(bound, "sessionId")) && SameIdentity(fieldOf(r, "chosenTaskId"), fieldOf(bound, "taskId")) {
			applicable = append(applicable, r)
		}
	}
	for _, fact := range competingFacts(marker) {
		if !FactCovered(fact, applicable) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- state

// DeriveAssignmentState is derive_assignment_state: the contract's precedence table, in order.
func DeriveAssignmentState(marker Obj, now any) string {
	intent := markerFact(marker, "intent")
	attempts := objects(markerFactList(marker, "attempts"))
	claims := markerFactList(marker, "claims")
	if len(markerFact(marker, "bound")) > 0 {
		if Named(fieldOf(markerFact(marker, "relationship"), "relationshipId")) {
			return RelationshipRegistered
		}
		return IdentityBound
	}
	resolvedAmbiguity := ambiguityResolved(marker)
	accepted, unknown := false, false
	for _, a := range attempts {
		switch fieldOf(a, "outcome") {
		case "accepted":
			accepted = true
		case "unknown":
			unknown = true
		}
	}
	if !resolvedAmbiguity && (len(acceptedTasks(marker)) > 1 || len(claims) > 1) {
		return AmbiguousIdentity
	}
	anchor, seen := Moment(fieldOf(intent, "declaredAt")), Moment(now)
	if anchor != nil && seen != nil && seen.After(anchor.Add(BindingWindowMinutes*time.Minute)) {
		return IntentExpired
	}
	if !resolvedAmbiguity && !accepted && unknown {
		return CreationUnknown
	}
	if accepted {
		return CreationAccepted
	}
	return IntentDeclared
}

// Why a session's claim does not correlate (intent.py labels).
const (
	ClaimAbsent              = "claim_absent"
	ClaimDispatchUnnamed     = "claim_dispatch_unnamed"
	ClaimDispatchMismatch    = "claim_dispatch_mismatch"
	IntentDispatchUnnamed    = "intent_dispatch_unnamed"
	IntentAssignmentMismatch = "intent_assignment_mismatch"
)

func ownClaim(marker Obj, sessionID any) Obj {
	for _, claim := range objects(markerFactList(marker, "claims")) {
		if SameIdentity(Claimant(claim), sessionID) {
			return claim
		}
	}
	return nil
}

// CorrelationProblem is correlation_problem: which condition this session's claim fails, or "".
func CorrelationProblem(marker Obj, sessionID, assignment any) string {
	claim := ownClaim(marker, sessionID)
	if len(claim) == 0 {
		return ClaimAbsent
	}
	presented := fieldOf(claim, "dispatchRequestId")
	if !Named(presented) {
		return ClaimDispatchUnnamed
	}
	declared := fieldOf(markerFact(marker, "intent"), "dispatchRequestIdHash")
	if !Named(declared) {
		return IntentDispatchUnnamed
	}
	if Named(assignment) && !SameIdentity(declared, assignment) {
		return IntentAssignmentMismatch
	}
	if SameIdentity(AssignmentID(presented.(string)), declared) {
		return ""
	}
	return ClaimDispatchMismatch
}

// Correlated is correlated: correlation_problem read as a yes or no.
func Correlated(marker Obj, sessionID, assignment any) bool {
	return CorrelationProblem(marker, sessionID, assignment) == ""
}

// ClaimedDispatch is claimed_dispatch: the dispatch this session claimed, where that correlates.
func ClaimedDispatch(marker Obj, sessionID, assignment any) any {
	if CorrelationProblem(marker, sessionID, assignment) != "" {
		return nil
	}
	presented := fieldOf(ownClaim(marker, sessionID), "dispatchRequestId")
	if Named(presented) {
		return presented
	}
	return nil
}

// ---------------------------------------------------------------- selection

func obstructedClaim(marker Obj, sessionID any) Obj {
	for _, raw := range markerFactList(marker, "claims") {
		claim, ok := raw.(Obj)
		if !ok || !SameIdentity(Claimant(claim), sessionID) {
			continue
		}
		if value, present := get(claim, "dispatchRequestId"); present {
			if _, text := value.(string); !text {
				return claim
			}
		}
	}
	return nil
}

func selectingClaim(marker Obj, sessionID any, assignment string) Obj {
	for _, raw := range markerFactList(marker, "claims") {
		claim, ok := raw.(Obj)
		if !ok || !SameIdentity(Claimant(claim), sessionID) {
			continue
		}
		presented := fieldOf(claim, "dispatchRequestId")
		if !Named(presented) || !SameIdentity(AssignmentID(presented.(string)), assignment) {
			continue
		}
		intentValue, _ := get(marker, "intent")
		intent, readable := intentValue.(Obj)
		if readable {
			for _, field := range identityFields["intent"] {
				if value, present := get(intent, field); present {
					if _, text := value.(string); !text {
						readable = false
					}
				}
			}
		}
		var declared any
		if readable {
			declared = fieldOf(intent, "dispatchRequestIdHash")
		}
		if Named(declared) && !SameIdentity(declared, assignment) {
			continue
		}
		return claim
	}
	return nil
}

type candidate struct {
	directory string
	facts     Obj
	problems  []string
}

// recencyLess is _recency's ordering: a readable declaration above none, then the instant, then
// the assignment id.
func recencyLess(a, b candidate) bool {
	moment := func(c candidate) *time.Time {
		if intent, ok := fieldOfAny(c.facts, "intent").(Obj); ok {
			return Moment(fieldOf(intent, "declaredAt"))
		}
		return nil
	}
	ma, mb := moment(a), moment(b)
	if (ma != nil) != (mb != nil) {
		return ma == nil
	}
	if ma != nil && !ma.Equal(*mb) {
		return ma.Before(*mb)
	}
	return filepath.Base(a.directory) < filepath.Base(b.directory)
}

func fieldOfAny(o Obj, key string) any { v, _ := get(o, key); return v }

// SelectAssignment is select_assignment: which assignment under this workspace a session's turn
// is about. A claim naming THIS assignment is consulted before recency. It returns ("", nil,
// ["workspace"]) when the workspace could not be listed and ("", nil, []) when nothing is
// selectable.
func SelectAssignment(root, workspace string, sessionID any) (string, Obj, []string, error) {
	listed, readable, err := ListAssignments(root, workspace)
	if err != nil {
		return "", nil, nil, err
	}
	if !readable {
		return "", nil, []string{"workspace"}, nil
	}
	var candidates []candidate
	for _, directory := range listed {
		facts, problems := ReadAssignment(directory)
		_, hasIntent := get(facts, "intent")
		unreadableIntent := false
		for _, p := range problems {
			if p == "intent" {
				unreadableIntent = true
			}
		}
		if !hasIntent && !unreadableIntent {
			continue
		}
		candidates = append(candidates, candidate{directory, facts, problems})
	}
	if len(candidates) == 0 {
		return "", nil, []string{}, nil
	}
	var claimed []candidate
	for _, c := range candidates {
		if selectingClaim(c.facts, sessionID, filepath.Base(c.directory)) != nil || obstructedClaim(c.facts, sessionID) != nil {
			claimed = append(claimed, c)
		}
	}
	pool := candidates
	if len(claimed) > 0 {
		pool = claimed
	}
	best := pool[0]
	for _, c := range pool[1:] {
		if recencyLess(best, c) {
			best = c
		}
	}
	return best.directory, best.facts, best.problems, nil
}

// ---------------------------------------------------------------- writing

func registrationError(reason, detail string) error {
	return &store.RefusedError{Reason: reason, Detail: detail}
}

func checkedAssignment(value any) (string, error) {
	if !ValidAssignment(value) {
		return "", registrationError(UnknownGeneration, "an assignment id is the hex sha256 of a dispatch request id, not "+pyReprValue(value))
	}
	return value.(string), nil
}

func checkedIdentity(value any, what string) (string, error) {
	if !ValidSegment(value) {
		return "", registrationError(UnboundGeneration, "a "+what+" becomes a directory name, so it cannot be empty, . or .., or contain a path separator: "+pyReprValue(value))
	}
	return value.(string), nil
}

func assignmentDirectory(root, workspace string, assignment any) (string, error) {
	checked, err := checkedAssignment(assignment)
	if err != nil {
		return "", err
	}
	return AssignmentDir(root, workspace, checked)
}

// pyEqual is Python == over decoded JSON values: numbers by value (a bool is 0 or 1), objects
// by key set.
func pyEqual(a, b any) bool {
	number := func(v any) (float64, bool) {
		switch n := v.(type) {
		case bool:
			if n {
				return 1, true
			}
			return 0, true
		case int64:
			return float64(n), true
		case int:
			return float64(n), true
		case float64:
			return n, true
		}
		return 0, false
	}
	if x, ok := number(a); ok {
		y, ok := number(b)
		return ok && x == y && !math.IsNaN(x)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case Obj:
		y, ok := b.(Obj)
		if !ok || len(x) != len(y) {
			return false
		}
		for _, f := range x {
			other, present := get(y, f.Key)
			if !present || !pyEqual(f.Value, other) {
				return false
			}
		}
		return true
	}
	return false
}

// publishOrCompare is _publish_or_compare: whether losing was a replay or a contradiction.
func publishOrCompare(target string, payload Obj, fields []string, root string, since []string) (string, error) {
	outcome, err := Publish(target, payload, root)
	if err != nil || outcome == Published {
		return outcome, err
	}
	value, status := readFact(target)
	existing, isRecord := value.(Obj)
	if status != factPresent || !isRecord {
		return Conflict, nil
	}
	for _, f := range fields {
		if !pyEqual(fieldOf(existing, f), fieldOf(payload, f)) {
			return Conflict, nil
		}
	}
	for _, f := range since {
		if stored, present := get(existing, f); present && !pyEqual(stored, fieldOf(payload, f)) {
			return Conflict, nil
		}
	}
	return Unchanged, nil
}

// nextIndex is _next_index: the next free slot for a numbered fact, and whether it was read.
func nextIndex(directory, kind string) (int, bool) {
	entries, readable := listing(filepath.Join(directory, kind), "*.json", "")
	if !readable {
		return 0, false
	}
	next := 0
	for _, entry := range entries {
		name := filepath.Base(entry)
		if strings.HasPrefix(name, ".") {
			continue
		}
		if n, err := strconv.Atoi(stem(name)); err == nil && n >= 0 && strings.Trim(stem(name), "0123456789") == "" && n+1 > next {
			next = n + 1
		}
	}
	return next, true
}

// publishNumbered is _publish_numbered: the next numbered fact, re-listing on every loss.
func publishNumbered(directory, kind string, payload Obj, root string) (string, error) {
	for range 64 {
		index, readable := nextIndex(directory, kind)
		if !readable {
			return "", registrationError(RelationshipConflict, "the "+kind+" directory cannot be read, so no slot can be allocated in it")
		}
		outcome, err := Publish(filepath.Join(directory, kind, strconv.Itoa(index)+".json"), payload, root)
		if err != nil {
			return "", err
		}
		if outcome == Published {
			return kind + "/" + strconv.Itoa(index), nil
		}
	}
	return "", registrationError(RelationshipConflict, "could not allocate a free "+kind+" slot after 64 attempts")
}

// IntentDeclaration is declare_intent's keyword arguments.
type IntentDeclaration struct {
	Workspace, DispatchRequestID, IssueKey, DeclaredAt string
	CriteriaSource, BaselineRevision, AuthorizedSettings, DBPath any
}

// DeclareIntent is declare_intent: the fact that exists BEFORE the task does. The dispatch
// request id is stored only as its sha256.
func DeclareIntent(root string, d IntentDeclaration) (Obj, error) {
	if !Named(d.DispatchRequestID) {
		return nil, registrationError(UnboundGeneration, "an intent needs an exact dispatch request id")
	}
	assignment := AssignmentID(d.DispatchRequestID)
	directory, err := assignmentDirectory(root, d.Workspace, assignment)
	if err != nil {
		return nil, err
	}
	workspace, err := resolved(d.Workspace)
	if err != nil {
		return nil, err
	}
	payload := Obj{{Key: "dispatchRequestIdHash", Value: assignment}, {Key: "issueKey", Value: d.IssueKey}, {Key: "workspace", Value: workspace},
		{Key: "criteriaSource", Value: d.CriteriaSource}, {Key: "baselineRevision", Value: d.BaselineRevision}, {Key: "authorizedSettings", Value: d.AuthorizedSettings}, {Key: "declaredAt", Value: d.DeclaredAt}}
	if truthy(d.DBPath) {
		payload = append(payload, F{Key: "dbPath", Value: pyStr(d.DBPath)})
	}
	outcome, err := publishOrCompare(filepath.Join(directory, "intent.json"), payload, intentFields, root, nil)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "assignmentDir", Value: directory}, {Key: "outcome", Value: outcome}, {Key: "declaredAt", Value: d.DeclaredAt}}, nil
}

// RecordAttempt is record_attempt: what the creation call returned (accepted, unknown, failed).
func RecordAttempt(root, workspace string, assignment any, outcome, at string, taskID any) (Obj, error) {
	valid := false
	for _, o := range AttemptOutcomes {
		valid = valid || o == outcome
	}
	if !valid {
		return nil, registrationError(UnknownGeneration, "an attempt outcome is one of "+strings.Join(AttemptOutcomes, ", ")+", not "+store.PyRepr(outcome))
	}
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	payload := Obj{{Key: "outcome", Value: outcome}, {Key: "at", Value: at}}
	if taskID != nil {
		payload = append(payload, F{Key: "taskId", Value: taskID})
	}
	factID, err := publishNumbered(directory, "attempts", payload, root)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "factId", Value: factID}, {Key: "outcome", Value: outcome}}, nil
}

// BindIdentity is bind: the intent bound to the real native task id, atomic and idempotent.
// A losing different identity is RECORDED as a conflict, never swallowed.
func BindIdentity(root, workspace string, assignment any, sessionID, taskID any, at string) (Obj, error) {
	if !Named(sessionID) || !Named(taskID) {
		return nil, registrationError(UnboundGeneration, "a bind needs an exact session id and task id; a record naming nothing binds nothing")
	}
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	outcome, err := Publish(filepath.Join(directory, "bound.json"), Obj{{Key: "sessionId", Value: sessionID}, {Key: "taskId", Value: taskID}, {Key: "at", Value: at}}, root)
	if err != nil {
		return nil, err
	}
	if outcome == Published {
		return Obj{{Key: "assignmentId", Value: assignment}, {Key: "outcome", Value: Bound}, {Key: "sessionId", Value: sessionID}, {Key: "taskId", Value: taskID}}, nil
	}
	marker, unreadable := ReadAssignment(directory)
	winnerValue, _ := get(marker, "bound")
	winner, ok := winnerValue.(Obj)
	if !ok {
		labels := unreadable
		if len(labels) == 0 {
			labels = []string{"bound"}
		}
		return nil, registrationError(UnboundGeneration, "a bind already exists here and cannot be read: "+strings.Join(labels, ", "))
	}
	if SameIdentity(fieldOf(winner, "sessionId"), sessionID) && SameIdentity(fieldOf(winner, "taskId"), taskID) {
		return Obj{{Key: "assignmentId", Value: assignment}, {Key: "outcome", Value: Unchanged}, {Key: "sessionId", Value: sessionID}, {Key: "taskId", Value: taskID}}, nil
	}
	factID, err := publishNumbered(directory, "conflicts", Obj{{Key: "attemptedSessionId", Value: sessionID}, {Key: "attemptedTaskId", Value: taskID}, {Key: "loserProcess", Value: strconv.Itoa(os.Getpid())}, {Key: "at", Value: at}}, root)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "outcome", Value: Conflict}, {Key: "factId", Value: factID}, {Key: "boundSessionId", Value: fieldOf(winner, "sessionId")}, {Key: "boundTaskId", Value: fieldOf(winner, "taskId")}}, nil
}

// generationState is _generation_state: (state, generation) for one dispatch on a connection the
// caller owns; state "" when the read itself failed.
func generationState(ctx context.Context, conn *sql.Conn, relationshipID, dispatchRequestID string) (string, any) {
	var opened, current any
	err := conn.QueryRowContext(ctx, "SELECT g.execution_generation AS opened, r.execution_generation AS current  FROM generations g  JOIN relationships r ON r.relationship_id = g.relationship_id WHERE g.relationship_id = ? AND g.dispatch_request_id = ?", relationshipID, dispatchRequestID).Scan(&opened, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return DispatchAbsent, nil
	}
	if err != nil {
		return "", nil
	}
	if !pyEqual(opened, current) {
		return DispatchStale, current
	}
	return DispatchCurrent, current
}

// RegisterRelationship is register_relationship: which relay relationship this assignment was
// registered as, confirmed against the relay store and published under ONE hold on its write
// lock (store.RegistrationHold). dbPath nil is Python's None.
func RegisterRelationship(ctx context.Context, root, workspace string, assignment any, relationshipID, dispatchRequestID, at string, dbPath any) (Obj, error) {
	if !Named(relationshipID) {
		return nil, registrationError(UnregisteredRelationship, "a registration needs an exact relationship id")
	}
	if AssignmentID(dispatchRequestID) != assignment {
		return nil, registrationError(RelationshipConflict, "relationship "+relationshipID+" was dispatched under a different request id, so it does not belong to assignment "+pyStr(assignment))
	}
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	refuseUnheld := func(unavailable string) error {
		return registrationError(UnregisteredRelationship, "the relay store could not be held for this registration, so it cannot be confirmed that relationship "+relationshipID+" is still this assignment's at the moment the registration lands ("+unavailable+"); it is refused rather than published on the caller's word")
	}
	path, isPath := dbPath.(string)
	if !isPath {
		// Path(None) raises TypeError, which registration_hold answers as an unreadable path.
		return nil, refuseUnheld("the relay store path " + pyReprValue(dbPath) + " could not be read as a path")
	}
	var result Obj
	err = store.RegistrationHold(ctx, path, func(held *sql.Conn, unavailable string) error {
		if held == nil {
			return refuseUnheld(unavailable)
		}
		state, generation := generationState(ctx, held, relationshipID, dispatchRequestID)
		switch state {
		case "":
			return registrationError(UnregisteredRelationship, "the relay store could not be read, so it cannot be confirmed that relationship "+relationshipID+" belongs to this assignment; registration is refused rather than taken on the caller's word")
		case DispatchStale:
			return registrationError(StaleGeneration, "this assignment's dispatch request id opened an earlier generation of relationship "+relationshipID+", which has since advanced, so registering it would attribute the current generation's receipts to a superseded assignment")
		case DispatchAbsent:
			return registrationError(RelationshipConflict, "the relay has no generation of relationship "+relationshipID+" opened under this assignment's dispatch request id, so it is not this assignment's relationship")
		}
		outcome, err := publishOrCompare(filepath.Join(directory, "relationship.json"), Obj{{Key: "relationshipId", Value: relationshipID}, {Key: "executionGeneration", Value: generation}, {Key: "at", Value: at}}, []string{"relationshipId"}, root, []string{"executionGeneration"})
		if err != nil {
			return err
		}
		result = Obj{{Key: "assignmentId", Value: assignment}, {Key: "relationshipId", Value: relationshipID}, {Key: "executionGeneration", Value: generation}, {Key: "outcome", Value: outcome}}
		return nil
	})
	return result, err
}

// PublishResolution is publish_resolution: adjudicate named evidence; resolutions accumulate.
func PublishResolution(root, workspace string, assignment, chosenTaskID, chosenSessionID, reason any, at string, adjudicated []Obj) (Obj, error) {
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	entries := make([]any, len(adjudicated))
	for i, entry := range adjudicated {
		entries[i] = Obj{{Key: "factId", Value: fieldOf(entry, "factId")}, {Key: "digest", Value: fieldOf(entry, "digest")}}
	}
	factID, err := publishNumbered(directory, "resolutions", Obj{{Key: "chosenTaskId", Value: chosenTaskID}, {Key: "chosenSessionId", Value: chosenSessionID}, {Key: "reason", Value: reason}, {Key: "at", Value: at}, {Key: "adjudicated", Value: entries}}, root)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "factId", Value: factID}, {Key: "adjudicated", Value: int64(len(entries))}}, nil
}

// PublishClaim is publish_claim: the child's own assertion that it is this assignment's session,
// refused where the dispatch it names does not hash to the assignment.
func PublishClaim(root, workspace string, assignment, sessionID any, dispatchRequestID string, firstTurnID any, at string) (Obj, error) {
	session, err := checkedIdentity(sessionID, "session id")
	if err != nil {
		return nil, err
	}
	if AssignmentID(dispatchRequestID) != assignment {
		return nil, registrationError(RelationshipConflict, "this claim names dispatch request id "+dispatchRequestID+", which does not hash to assignment "+pyStr(assignment)+", so it claims a different assignment")
	}
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	outcome, err := publishOrCompare(filepath.Join(directory, "claims", session, claimFile), Obj{{Key: "dispatchRequestId", Value: dispatchRequestID}, {Key: "sessionId", Value: session}, {Key: "firstTurnId", Value: firstTurnID}, {Key: "at", Value: at}}, []string{"dispatchRequestId", "sessionId"}, root, nil)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "sessionId", Value: session}, {Key: "outcome", Value: outcome}}, nil
}

// PublishDisposition is publish_disposition: what this turn declared, at the path its Stop
// identity derives, from the exhaustive vocabulary only.
func PublishDisposition(root, workspace string, assignment, sessionID, turnID any, outcome, at string) (Obj, error) {
	session, err := checkedIdentity(sessionID, "session id")
	if err != nil {
		return nil, err
	}
	turn, err := checkedIdentity(turnID, "turn id")
	if err != nil {
		return nil, err
	}
	known := false
	for _, o := range DispositionOutcomes {
		known = known || o == outcome
	}
	if !known {
		return nil, registrationError(OutcomeInconsistent, "a disposition outcome is one of "+strings.Join(DispositionOutcomes, ", ")+", not "+store.PyRepr(outcome))
	}
	directory, err := assignmentDirectory(root, workspace, assignment)
	if err != nil {
		return nil, err
	}
	published, err := publishOrCompare(filepath.Join(directory, "dispositions", session, turn+".json"), Obj{{Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "outcome", Value: outcome}, {Key: "at", Value: at}}, []string{"sessionId", "turnId", "outcome"}, root, nil)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "assignmentId", Value: assignment}, {Key: "sessionId", Value: session}, {Key: "turnId", Value: turn}, {Key: "outcome", Value: outcome}, {Key: "published", Value: published}}, nil
}

