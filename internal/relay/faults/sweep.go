package faults

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The delivery sources of faultsweep.sweep, and recorded()/record_all for them. Subset ported for
// todo 21; todo 22 owns this (the sync, observation, refusal, managed-start and reading sources,
// and every class's clear but delivery_stalled's, are its).

const (
	sweepLimit    = 32
	presentChecks = sweepLimit * 4
	busyCap       = "busy_cap"
	busyAttempt   = "deferred_busy"
	holdNamed     = "unknown_send_hold_named"
)

var (
	settledDelivery  = []any{"dispatched", "inbox_only", "superseded"}
	parentHolds      = []string{"host_lost_turn", "unknown_send_lost", "unknown_send_undecided"}
	unknownSendHolds = []string{"unknown_send_lost", "unknown_send_undecided"}
	commit           = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

const notSuperseded = "NOT EXISTS (SELECT 1 FROM delivery_supersession x WHERE x.event_id = d.event_id) AND NOT EXISTS (SELECT 1 FROM relationships sr WHERE sr.relationship_id = d.relationship_id AND sr.superseded_by IS NOT NULL)"

// Installation is faultsweep.INSTALLATION: the package, its version and the directory of the
// installed copy, which every observation's facts carry.
type Installation struct {
	Package, Version, Location string
}

// ExecutableInstallation is the running relay's own Installation (decision 34): package
// codex-session-relay at RelayPackageVersion, located at the directory of this executable with
// its links resolved, the directory crw install records as its Go install entry's location.
// The daemon's sweeper and the fault-sweep command both name it, as Python's name its
// installed PACKAGE_DIRECTORY.
func ExecutableInstallation() (Installation, error) {
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err != nil {
		return Installation{}, err
	}
	return Installation{Package: "codex-session-relay", Version: RelayPackageVersion, Location: filepath.Dir(executable)}, nil
}

// RelayPackageVersion is codex_session_relay.__version__, the version faultsweep.INSTALLATION
// records in every observation's facts. It follows the package, unlike ownership.PythonBuild,
// which names the fence release and never moves with a later bump.
const RelayPackageVersion = "0.2.0"

// Sweeper derives the delivery faults of one store.
type Sweeper struct {
	Store *store.Store
	// Current is delivery.supersession_reason(...) is None: whether a delivery still says
	// something current (the send path's own rule, owned by the delivery package).
	Current func(ctx context.Context, eventID string) (bool, error)
	// SupersessionReason preserves the send path's exact permanent verdict for memo rows.
	SupersessionReason func(ctx context.Context, eventID string) (string, error)
	// MaxAttempts is the RetryPolicy's max_attempts.
	MaxAttempts  int64
	Installation Installation
	// HostRecordPath is supplied by the caller; the sweeper never resolves host
	// state from the process environment.
	HostRecordPath string
	// Program names this relay in settings recovery commands; nil uses the installed binary.
	Program func() []string
	// Now stamps the cursors, as faultsweep._now reads the wall clock.
	Now func() string
	// ManagedObserver enables the relay's own settled-turn readings. Selection is
	// passed unchanged to the read-only reporting projection.
	ManagedObserver ManagedReadingObserver
	Selection       any
	// Workspace is the scope under which this store's source rows are judged.
	Workspace string
}

// Batch is the complete sweep answer, including the rotations to commit after recording.
type Batch struct {
	Observations    []Observation
	Clears          []Observation
	Gaps            []any
	CompleteSources []string
	Cursors         map[string]any
	ReadingsNext    any
	ReadingsTotal   int
	Limits          string
	positions       map[string]any
}

type RecordedSweep struct {
	Read, Recorded, Queued int
	Gaps                   []any
	Results                []any
}

func pick(values ...any) []any { return values }

// Sweep derives store sources and the supplied reporting readings on the same rotation.
func (sw *Sweeper) Sweep(ctx context.Context, product string) (Batch, error) {
	return sw.SweepReadings(ctx, product, "", nil, 0)
}
func (sw *Sweeper) SweepReadings(ctx context.Context, product, project string, readings []any, after int) (Batch, error) {
	if sw.Selection != nil && sw.ManagedObserver == nil {
		configured := *sw
		configured.ManagedObserver = ManagedOmittedObserver{}
		return configured.SweepReadings(ctx, product, project, readings, after)
	}
	cursors, err := sw.readCursors(ctx)
	if err != nil {
		return Batch{}, err
	}
	held, err := sw.deliveryFaults(ctx, product, cursors["delivery_stalled"])
	if err != nil {
		return Batch{}, err
	}
	retries, err := sw.retryFaults(ctx, product, cursors["delivery_retrying"])
	if err != nil {
		return Batch{}, err
	}
	managed, managedCursor, err := sw.ManagedStartFaults(ctx, product, cursors["managed_start_failed"])
	if err != nil {
		return Batch{}, err
	}
	derived := append([]Observation(nil), held.observations...)
	syncPage, err := sw.syncFaults(ctx, product, cursors["record_sync_failed"])
	if err != nil {
		return Batch{}, err
	}
	derived = append(derived, syncPage.observations...)
	observationPage, err := sw.observationFaults(ctx, product, cursors["observation_stalled"])
	if err != nil {
		return Batch{}, err
	}
	derived = append(derived, observationPage.observations...)
	derived = append(derived, retries.observations...)
	refusalPage, err := sw.refusalFaults(ctx, product, cursors["delivery_refused"])
	if err != nil {
		return Batch{}, err
	}
	derived = append(derived, refusalPage.observations...)
	derived = append(derived, managed...)
	if project != "" {
		for i := range derived {
			if _, ok := derived[i].Scope["projectKey"]; !ok {
				derived[i].Scope = copyMap(derived[i].Scope)
				derived[i].Scope["projectKey"] = project
			}
		}
	}
	readingObs, gaps, next, total, err := sw.readingFaults(ctx, product, project, readings, after)
	if err != nil {
		return Batch{}, err
	}
	var managedReadingCursor any
	if sw.ManagedObserver != nil {
		managedPage, e := sw.ManagedReadings(ctx, sw.Selection, sw.ManagedObserver, 8, cursors["managed_readings"], "")
		if e != nil {
			return Batch{}, e
		}
		managedReadingCursor = managedPage.Cursor
		gaps = append(gaps, managedPage.Gaps...)
		observations, readingGaps, _, _, e := sw.readingFaults(ctx, product, project, managedPage.Readings, 0)
		if e != nil {
			return Batch{}, e
		}
		derived = append(derived, observations...)
		gaps = append(gaps, readingGaps...)
	}
	derived = append(derived, readingObs...)
	if sw.Workspace != "" {
		for i := range derived {
			derived[i].Scope = copyMap(derived[i].Scope)
			derived[i].Scope["workspace"] = sw.Workspace
		}
	}
	clears, recoveredCursor, undetermined, err := sw.recovered(ctx, product, cursors["recovered"])
	if err != nil {
		return Batch{}, err
	}
	gaps = append(gaps, undetermined...)
	active := map[string]bool{}
	for _, o := range derived {
		if !o.Cleared {
			active[o.FaultClass+"|"+canonicalSignature(o.Signature)] = true
		}
	}
	filtered := derived[:0]
	for _, o := range derived {
		if !o.Cleared || !active[o.FaultClass+"|"+canonicalSignature(o.Signature)] {
			filtered = append(filtered, o)
		}
	}
	derived = filtered
	var kept []Observation
	for _, c := range clears {
		if !active[c.FaultClass+"|"+canonicalSignature(c.Signature)] {
			kept = append(kept, c)
		}
	}
	positions := map[string]any{"delivery_stalled": held.cursor, "delivery_retrying": retries.cursor, "managed_start_failed": managedCursor, "record_sync_failed": syncPage.cursor, "observation_stalled": observationPage.cursor, "delivery_refused": refusalPage.cursor, "recovered": recoveredCursor}
	if sw.ManagedObserver != nil {
		positions["managed_readings"] = managedReadingCursor
	}
	complete := []string{}
	for _, entry := range []struct {
		name string
		done bool
	}{{"delivery_stalled", held.complete && retries.complete}, {"record_sync_failed", syncPage.complete}, {"observation_stalled", observationPage.complete}, {"delivery_retrying", retries.complete}, {"delivery_refused", refusalPage.complete}, {"managed_start_failed", managedCursor == nil}} {
		if entry.done {
			complete = append(complete, entry.name)
		}
	}
	return Batch{Observations: derived, Clears: kept, Gaps: gaps, CompleteSources: complete, Cursors: positions, ReadingsNext: next, ReadingsTotal: total, Limits: "each source is read at most 32 rows per sweep, in rotations bounded by its upper key when the rotation started, so every rotation reaches the end; readings are reduced to the last per turn and read 32 at a time from readingsNext. A class this sweep does not derive is never cleared by its absence here", positions: positions}, nil
}

// RecordAll is record_all: every observation recorded, then the cursors advanced.
func (sw *Sweeper) RecordAll(ctx context.Context, ledger *Ledger, batch Batch) (RecordedSweep, error) {
	answer := RecordedSweep{Gaps: append([]any{}, batch.Gaps...), Results: []any{}}
	for _, o := range append(append([]Observation(nil), batch.Observations...), batch.Clears...) {
		workspace, _ := o.Scope["workspace"].(string)
		id := FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace)
		if alias, e := sw.Store.One(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", id); e != nil {
			return answer, e
		} else if alias != nil {
			id = text(alias, "fault_id")
		}
		before, e := sw.Store.One(ctx, "SELECT state FROM fault_publications WHERE fault_id=? AND state='pending' ORDER BY rowid DESC LIMIT 1", id)
		if e != nil {
			return answer, e
		}
		var recorded bool
		var err error
		if !productName.MatchString(o.Product) {
			err = fmt.Errorf("fault_observation_malformed: product %s is not a plain identifier (letters, digits, '.', '_', '-'); a ':' '@' or '|' would let one product's key read as another's", f1Repr(o.Product))
		} else {
			recorded, err = ledger.Record(ctx, o)
		}
		if err != nil {
			reason, _, _ := strings.Cut(strings.TrimPrefix(err.Error(), "transaction body: "), ":")
			if strings.HasPrefix(reason, "fault_") {
				answer.Gaps = append(answer.Gaps, map[string]any{"gap": "observation_refused", "faultClass": o.FaultClass, "reason": reason + ": " + strings.TrimPrefix(err.Error(), "transaction body: ")})
				continue
			}
			return answer, err
		}
		answer.Read++
		row, e := sw.Store.One(ctx, "SELECT state,cycle,severity,occurrence_count,suppression FROM fault_ledger WHERE fault_id=?", id)
		if e != nil {
			return answer, e
		}
		if row == nil && o.Cleared {
			answer.Results = append(answer.Results, map[string]any{"faultId": id, "recorded": false, "state": nil, "occurrenceCount": 0, "publication": nil, "reason": "a clearing observation for a fault that was never recorded"})
		} else if !recorded {
			answer.Results = append(answer.Results, map[string]any{"faultId": id, "recorded": false, "state": text(row, "state"), "occurrenceCount": integer(row, "occurrence_count"), "publication": nil, "reason": "this occurrence was already recorded in this episode"})
		} else {
			var publication any
			if before == nil {
				publication = publicationAnswer(ctx, ledger, id, true)
			}
			answer.Results = append(answer.Results, map[string]any{"faultId": id, "recorded": true, "state": text(row, "state"), "cycle": integer(row, "cycle"), "severity": text(row, "severity"), "occurrenceCount": integer(row, "occurrence_count"), "suppression": loadsMap(text(row, "suppression")), "publication": publication})
		}
		if recorded {
			answer.Recorded++
			if published, ok := answer.Results[len(answer.Results)-1].(map[string]any)["publication"].(map[string]any); ok && published["queued"] == true {
				answer.Queued++
			}
		}
	}
	if err := sw.writeCursors(ctx, batch.positions); err != nil {
		return answer, err
	}
	return answer, nil
}

func (sw *Sweeper) readCursors(ctx context.Context) (map[string]any, error) {
	rows, err := sw.Store.All(ctx, "SELECT source, position FROM fault_cursors")
	out := map[string]any{}
	for _, r := range rows {
		out[text(r, "source")] = r.Get("position")
	}
	return out, err
}

func (sw *Sweeper) writeCursors(ctx context.Context, positions map[string]any) error {
	now := sw.Now()
	return sw.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		sources := []string{"delivery_stalled", "record_sync_failed", "observation_stalled", "delivery_retrying", "delivery_refused", "managed_start_failed", "recovered"}
		if _, ok := positions["managed_readings"]; ok {
			sources = append(sources, "managed_readings")
		}
		for _, source := range sources {
			var stored any
			if p := positions[source]; p != nil {
				stored = dumps(p, false)
			}
			if _, err := sw.Store.Q(ctx).ExecContext(ctx, "INSERT INTO fault_cursors (source, position, pages, updated_at) VALUES (?,?,0,?) ON CONFLICT(source) DO UPDATE SET position = excluded.position,   pages = 0, updated_at = excluded.updated_at", source, stored, now); err != nil {
				return err
			}
		}
		return nil
	})
}

type page struct {
	observations []Observation
	cursor       any
	complete     bool
}

// rotation is _rotation for a text key, or an integer one.
func (sw *Sweeper) rotation(ctx context.Context, stored any, upper string, integerKey bool) (any, any, error) {
	var at, until any
	if s, ok := stored.(string); ok {
		if v, err := loads(s); err == nil {
			if m, ok := v.(map[string]any); ok && len(m) <= 2 {
				valid := true
				for key := range m {
					if key != "at" && key != "until" {
						valid = false
					}
				}
				if valid {
					at, until = m["at"], m["until"]
				} else {
					at = s
				}
			} else {
				at = s
			}
		} else {
			at = s
		}
	}
	if until == nil {
		r, err := sw.Store.One(ctx, upper)
		if err != nil {
			return nil, nil, err
		}
		if r != nil {
			until = r[0].Value
		}
	}
	if integerKey {
		convert := func(v any) (any, bool) {
			if v == nil {
				return nil, true
			}
			switch n := v.(type) {
			case int64:
				return n, true
			case float64:
				return int64(n), true
			case json.Number:
				i, e := n.Int64()
				return i, e == nil
			case string:
				i, e := strconv.ParseInt(n, 10, 64)
				return i, e == nil
			}
			return nil, false
		}
		var validAt, validUntil bool
		at, validAt = convert(at)
		until, validUntil = convert(until)
		if !validAt || !validUntil {
			at = nil
			r, e := sw.Store.One(ctx, upper)
			if e != nil {
				return nil, nil, e
			}
			until = nil
			if r != nil {
				until, _ = convert(r[0].Value)
			}
		}
	}
	if until == nil {
		at = nil
	}
	return at, until, nil
}

// anchorCursor orders relationship generations numerically while retaining a text cursor.
func anchorCursor(relationship string, generation int64) string {
	return fmt.Sprintf("%s:%020d", relationship, generation)
}

func pageOf(obs []Observation, rows []row, key string, after, until any) page {
	filled := len(rows) >= sweepLimit
	var cursor any
	if filled {
		cursor = map[string]any{"at": rows[len(rows)-1].Get(key), "until": until}
	}
	return page{obs, cursor, after == nil && !filled}
}

func (sw *Sweeper) scopeOf(ctx context.Context, rid any, cache map[string]map[string]any) (map[string]any, error) {
	id, _ := rid.(string)
	if strings.TrimSpace(id) == "" {
		return map[string]any{}, nil
	}
	if found, ok := cache[id]; ok {
		return copyMap(found), nil
	}
	r, err := sw.Store.One(ctx, "SELECT r.issue_key, s.project_key FROM relationships r  LEFT JOIN relationship_scope s ON s.relationship_id = r.relationship_id WHERE r.relationship_id = ?", id)
	if err != nil {
		return nil, err
	}
	found := map[string]any{}
	if r != nil {
		if p := text(r, "project_key"); p != "" {
			found["projectKey"] = p
		}
		if k := text(r, "issue_key"); k != "" {
			found["issueKey"] = k
		}
	}
	cache[id] = found
	return copyMap(found), nil
}

func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func evidence(kind, ref string, observed map[string]any) map[string]any {
	return map[string]any{"kind": kind, "ref": ref, "observed": observed}
}

// facts is _facts, with installation() read for this copy.
func (sw *Sweeper) facts(expected, actual, impact string, limits []any, subject map[string]any) map[string]any {
	installed := sw.installation()
	stated := []any{"the installed revision is not known to the relay; the package version and the location of the installed copy identify it"}
	if rev, ok := installed["revision"].(map[string]any); ok {
		stated = nil
		if rev["workingTreeClean"] == false {
			stated = []any{"installed from a working tree with uncommitted changes, so the recorded commit does not fully identify the installed bytes"}
		}
	}
	observed := map[string]any{"expected": expected, "actual": actual, "impact": impact, "installation": installed, "limits": append(limits, stated...)}
	for k, v := range subject {
		if v != nil {
			observed[k] = v
		}
	}
	return evidence("facts", "sweep", observed)
}

// installation is faultsweep.installation: the revision the installer's host record
// attributes to this copy, or why it is unknown.
func (sw *Sweeper) installation() map[string]any {
	path := sw.HostRecordPath
	answer := func(revision any, record any, reason any) map[string]any {
		return map[string]any{"package": sw.Installation.Package, "version": sw.Installation.Version, "location": sw.Installation.Location,
			"revision": revision, "revisionRecord": record, "revisionReason": reason}
	}
	unknown := func(reason string) map[string]any { return answer(nil, path, reason) }
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return unknown("no host record at " + path)
	case err != nil:
		return unknown(fmt.Sprintf("the host record at %s could not be read: OSError", path))
	}
	v, err := loads(string(raw))
	data, _ := v.(map[string]any)
	if err != nil {
		return unknown(fmt.Sprintf("the host record at %s is unreadable: JSONDecodeError", path))
	}
	if n, ok := data["recordVersion"].(json.Number); !ok || n.String() != "1" {
		return unknown("the host record is not record version 1")
	}
	components, _ := data["components"].(map[string]any)
	relay, _ := components["codex-session-relay"].(map[string]any)
	installs, ok := relay["installs"].([]any)
	if !ok {
		return unknown("the host record lists no codex-session-relay installs")
	}
	here, _ := filepath.EvalSymlinks(sw.Installation.Location)
	var entry map[string]any
	for _, raw := range installs {
		e, _ := raw.(map[string]any)
		location, _ := e["location"].(string)
		if real, err := filepath.EvalSymlinks(location); e != nil && err == nil && here != "" && real == here {
			entry = e
		}
	}
	if entry == nil {
		return unknown("the host record has no install entry for " + sw.Installation.Location)
	}
	source, ok := entry["source"].(map[string]any)
	if !ok {
		return unknown("this copy's install entry records no revision (entries written before the installer recorded one per install carry none; the next install records it)")
	}
	var bad []string
	for _, key := range []string{"repositoryCommit", "repositoryTree", "subdirectoryTree"} {
		if s, ok := source[key].(string); !ok || !commit.MatchString(s) {
			bad = append(bad, key)
		}
	}
	if _, ok := source["workingTreeClean"].(bool); !ok {
		bad = append(bad, "workingTreeClean")
	}
	if len(bad) > 0 {
		return unknown("this copy's install entry records an incomplete revision (" + strings.Join(bad, ", ") + " missing or malformed), which identifies nothing")
	}
	revision := map[string]any{"environment": entry["environment"], "integrity": entry["integrity"]}
	for _, key := range []string{"repositoryCommit", "repositoryTree", "subdirectoryTree", "workingTreeClean"} {
		revision[key] = source[key]
	}
	return answer(revision, path, nil)
}

func (sw *Sweeper) current(ctx context.Context, event string, cache map[string]bool) (bool, error) {
	if v, ok := cache[event]; ok {
		return v, nil
	}
	if sw.Current == nil {
		cache[event] = true
		return true, nil
	}
	v, err := sw.Current(ctx, event)
	cache[event] = v
	return v, err
}

func (sw *Sweeper) settingsItems(ctx context.Context, event, recipient, request string, own map[string]any, current bool) ([]any, string, error) {
	return sw.settingsEvidence(ctx, event, recipient, request, own, current)
}

func (sw *Sweeper) deliveryFaults(ctx context.Context, product string, cursor any) (page, error) {
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(event_id) FROM deliveries", false)
	if err != nil || until == nil {
		return page{complete: after == nil}, err
	}
	afterText, _ := after.(string)
	rows, err := sw.Store.All(ctx, "SELECT d.event_id, d.relationship_id, d.recipient_task_id, d.state, d.hold_reason,       d.attempt_count, e.execution_generation AS generation, e.turn_id AS turn,       (SELECT a.request_id FROM attempts a WHERE a.event_id = d.event_id          AND a.internal_state = 'settled'         ORDER BY a.attempt_no DESC LIMIT 1) AS last_request,       (SELECT a.state FROM attempts a WHERE a.event_id = d.event_id          AND a.internal_state = 'settled'         ORDER BY a.attempt_no DESC LIMIT 1) AS last_state  FROM deliveries d LEFT JOIN events e ON e.event_id = d.event_id WHERE d.hold_reason IS NOT NULL AND d.hold_reason != ? AND d.state NOT IN (?,?,?)   AND "+notSuperseded+"   AND d.event_id > ? AND d.event_id <= ? ORDER BY d.event_id LIMIT ?",
		append(append(pick(busyCap), settledDelivery...), afterText, until, sweepLimit)...)
	if err != nil {
		return page{}, err
	}
	var obs []Observation
	scopes := map[string]map[string]any{}
	cache := map[string]bool{}
	for _, r := range rows {
		ok, err := sw.current(ctx, text(r, "event_id"), cache)
		if err != nil {
			return page{}, err
		}
		if !ok {
			continue
		}
		hold := text(r, "hold_reason")
		capped := integer(r, "attempt_count") >= sw.MaxAttempts
		parked := contains(parentHolds, hold)
		key := text(r, "last_request")
		if key == "" {
			key = text(r, "event_id")
		}
		occurrenceKey := "delivery:" + key
		if contains(unknownSendHolds, hold) {
			occurrenceKey += ":held:" + hold
			if request := text(r, "last_request"); request != "" {
				named, err := sw.Store.One(ctx, "SELECT MAX(seq) AS seq FROM journal WHERE kind = ? AND subject = ?", holdNamed, request)
				if err != nil {
					return page{}, err
				}
				if named != nil && named.Get("seq") != nil {
					occurrenceKey += fmt.Sprintf(":%d", integer(named, "seq"))
				}
			}
		}
		severity := Degraded
		if capped || parked {
			severity = Broken
		}
		scope, err := sw.scopeOf(ctx, r.Get("relationship_id"), scopes)
		if err != nil {
			return page{}, err
		}
		recipient := text(r, "recipient_task_id")
		settings, cause, err := sw.settingsItems(ctx, text(r, "event_id"), recipient, "", nil, text(r, "state") == "withheld_pre_send" || text(r, "state") == "inbox_only")
		if err != nil {
			return page{}, err
		}
		detail := fmt.Sprintf("a delivery to %s is held: %s", recipient, hold)
		if cause != "" {
			detail += " after settings " + cause
		}
		obs = append(obs, Observation{Product: product, FaultClass: "delivery_stalled", Severity: severity,
			Signature: map[string]any{"recipient": recipient, "attemptState": r.Get("last_state")}, OccurrenceKey: occurrenceKey, Scope: scope,
			Detail: detail,
			Evidence: append(append([]any{evidence("row", "deliveries:"+text(r, "event_id"), map[string]any{"state": r.Get("state"), "holdReason": r.Get("hold_reason"), "attemptCount": r.Get("attempt_count"), "lastAttemptState": r.Get("last_state"), "relationship": r.Get("relationship_id")})}, settings...),
				sw.facts("the delivery reaches "+recipient, fmt.Sprintf("held (%s) after %d attempts; the last settled attempt ended %s", hold, integer(r, "attempt_count"), pyStr(r.Get("last_state"))),
					"the recipient is not given this delivery while the hold stands", []any{"read from settled attempts only; one still in flight is not counted"},
					map[string]any{"event": r.Get("event_id"), "relationship": r.Get("relationship_id"), "generation": r.Get("generation"), "turn": r.Get("turn")}))})
	}
	return pageOf(obs, rows, "event_id", after, until), nil
}

func (sw *Sweeper) retryFaults(ctx context.Context, product string, cursor any) (page, error) {
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(rowid) FROM attempts", true)
	if err != nil || until == nil {
		return page{complete: after == nil}, err
	}
	afterInt, _ := after.(int64)
	args := append(append([]any(nil), settledDelivery...), "dispatched", "inbox_only", busyAttempt, unknownSendHolds[0], unknownSendHolds[1], afterInt, until, sweepLimit)
	rows, err := sw.Store.All(ctx, "SELECT a.rowid AS seq, a.request_id, a.state AS attempt_state, a.event_id,       a.attempt_no,       (SELECT MAX(x.attempt_no) FROM attempts x WHERE x.event_id = a.event_id          AND x.internal_state = 'settled') AS latest_settled,       d.relationship_id, d.recipient_task_id, d.state AS delivery_state,       d.hold_reason, d.attempt_count, e.execution_generation AS generation,       e.turn_id AS turn  FROM attempts a JOIN deliveries d ON d.event_id = a.event_id  LEFT JOIN events e ON e.event_id = a.event_id WHERE d.state NOT IN (?,?,?)   AND a.internal_state = 'settled'   AND a.state IS NOT NULL AND a.state NOT IN (?,?,?)   AND NOT (COALESCE(d.hold_reason, '') IN (?,?) AND a.attempt_no = d.attempt_count)   AND "+notSuperseded+"   AND a.rowid > ? AND a.rowid <= ? ORDER BY a.rowid LIMIT ?", args...)
	if err != nil {
		return page{}, err
	}
	var obs []Observation
	scopes := map[string]map[string]any{}
	cache := map[string]bool{}
	for _, r := range rows {
		ok, err := sw.current(ctx, text(r, "event_id"), cache)
		if err != nil {
			return page{}, err
		}
		if !ok {
			continue
		}
		cause, err := sw.attemptSettingsCause(ctx, text(r, "event_id"), text(r, "request_id"))
		if err != nil {
			return page{}, err
		}
		settings, settingsReason, err := sw.settingsItems(ctx, text(r, "event_id"), text(r, "recipient_task_id"), text(r, "request_id"), cause, integer(r, "attempt_no") == integer(r, "latest_settled") && text(r, "delivery_state") == "withheld_pre_send")
		if err != nil {
			return page{}, err
		}
		hold := text(r, "hold_reason")
		severity := Degraded
		if hold != "" && hold != busyCap {
			severity = Broken
		}
		scope, err := sw.scopeOf(ctx, r.Get("relationship_id"), scopes)
		if err != nil {
			return page{}, err
		}
		recipient, state := text(r, "recipient_task_id"), text(r, "attempt_state")
		detail := fmt.Sprintf("an attempt to deliver to %s ended %s", recipient, state)
		if settingsReason != "" {
			detail += ": settings " + settingsReason
		}
		obs = append(obs, Observation{Product: product, FaultClass: "delivery_stalled", Severity: severity,
			Signature: map[string]any{"recipient": recipient, "attemptState": state}, OccurrenceKey: "delivery:" + text(r, "request_id"), Scope: scope,
			Detail: detail,
			Evidence: append(append([]any{evidence("row", "attempts:"+text(r, "request_id"), map[string]any{"attemptState": state, "deliveryState": r.Get("delivery_state"), "holdReason": r.Get("hold_reason"), "attemptCount": r.Get("attempt_count"), "event": r.Get("event_id")})}, settings...),
				sw.facts("the attempt reaches "+recipient, "the attempt ended "+state, "the delivery is retried and has not reached its recipient", []any{"one occurrence per settled failed attempt; attempts in flight are not read"},
					map[string]any{"event": r.Get("event_id"), "relationship": r.Get("relationship_id"), "generation": r.Get("generation"), "turn": r.Get("turn")}))})
	}
	return pageOf(obs, rows, "seq", after, until), nil
}

// attemptSettingsCause is _attempt_settings_cause.
func (sw *Sweeper) attemptSettingsCause(ctx context.Context, event, request string) (map[string]any, error) {
	var best row
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"SELECT seq, detail FROM journal WHERE subject = ? AND +kind = 'delivery_attempted'   AND (CASE WHEN json_valid(detail)        THEN json_extract(detail, '$.requestId') END) = ? ORDER BY seq DESC LIMIT 1", []any{event, request}},
		{"SELECT seq, detail FROM journal WHERE subject = ? AND +kind = 'reconciled' ORDER BY seq DESC LIMIT 1", []any{request}},
	} {
		r, err := sw.Store.One(ctx, q.sql, q.args...)
		if err != nil {
			return nil, err
		}
		if r != nil && (best == nil || integer(r, "seq") > integer(best, "seq")) {
			best = r
		}
	}
	if best == nil {
		return nil, nil
	}
	refusal, _ := loadsMap(text(best, "detail"))["settingsRefusal"].(map[string]any)
	if _, ok := refusal["reason"].(string); ok {
		return refusal, nil
	}
	return nil, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// recovered asks every derived fault's source directly; a bounded sweep page
// cannot establish absence for a fault past that page.
func (sw *Sweeper) recovered(ctx context.Context, product string, cursor any) ([]Observation, any, []any, error) {
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(fault_id) FROM fault_ledger", false)
	if err != nil || until == nil {
		return nil, nil, nil, err
	}
	afterText, _ := after.(string)
	rows, err := sw.Store.All(ctx, "SELECT fault_id, fault_class, signature, cycle, scope FROM fault_ledger WHERE product = ? AND state IN (?,?,?) AND cleared_at IS NULL   AND fault_class IN (?,?,?,?,?)   AND fault_id > ? AND fault_id <= ? ORDER BY fault_id LIMIT ?",
		product, Observed, Open, FixPending, "delivery_stalled", "record_sync_failed", "observation_stalled", "delivery_refused", "managed_start_failed", afterText, until, sweepLimit)
	if err != nil {
		return nil, nil, nil, err
	}
	var clears []Observation
	undeterminedGaps := []any{}
	for _, r := range rows {
		class := text(r, "fault_class")
		signature := loadsMap(text(r, "signature"))
		stored := loadsMap(text(r, "scope"))["workspace"]
		if stored != nil && stored != sw.Workspace {
			candidate := FaultIDInWorkspace(product, class, signature, sw.Workspace)
			alias, e := sw.Store.One(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id=?", candidate)
			if e != nil {
				return nil, nil, nil, e
			}
			if alias != nil {
				candidate = text(alias, "fault_id")
			}
			if candidate != text(r, "fault_id") {
				continue
			}
		}
		var present, undetermined bool
		switch class {
		case "managed_start_failed":
			present, err = sw.managedStillPresent(ctx, signature)
		case "delivery_stalled":
			present, undetermined, err = sw.stillPresent(ctx, signature)
		case "record_sync_failed", "observation_stalled", "delivery_refused":
			present, err = sw.derivedStillPresent(ctx, class, signature)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if undetermined {
			undeterminedGaps = append(undeterminedGaps, map[string]any{"gap": "presence_undetermined", "faultId": text(r, "fault_id"), "reason": "more than 128 deliveries to judge; the next sweep continues and nothing is cleared yet"})
			continue
		}
		if present {
			continue
		}
		last, err := sw.Store.One(ctx, "SELECT occurrence_id FROM fault_occurrences WHERE fault_id = ? AND cleared = 0 ORDER BY rowid DESC LIMIT 1", text(r, "fault_id"))
		if err != nil {
			return nil, nil, nil, err
		}
		key := fmt.Sprintf("cleared:after:%d", integer(r, "cycle"))
		if last != nil {
			key = "cleared:after:" + text(last, "occurrence_id")
		}
		clears = append(clears, Observation{Product: product, FaultClass: class, Severity: Notice, Signature: signature, OccurrenceKey: key,
			Scope: loadsMap(text(r, "scope")), Cleared: true, Detail: "this sweep read the source and no longer derives this fault",
			Evidence: []any{evidence("sweep", class, map[string]any{"derived": false})}})
	}
	var next any
	if len(rows) >= sweepLimit {
		next = map[string]any{"at": rows[len(rows)-1].Get("fault_id"), "until": until}
	}
	return clears, next, undeterminedGaps, nil
}

// stillPresent is still_present for delivery_stalled via _first_current. The overtaken-delivery
// memo (fault_overtaken_deliveries) is written as Python writes it.
func (sw *Sweeper) stillPresent(ctx context.Context, signature map[string]any) (bool, bool, error) {
	if signature["attemptState"] == busyAttempt {
		return false, false, nil
	}
	state := signature["attemptState"]
	query := "SELECT d.event_id FROM deliveries d WHERE d.recipient_task_id = ? AND d.state NOT IN (?,?,?)   AND " + notSuperseded + "   AND ((d.hold_reason IS NOT NULL AND d.hold_reason != ?         AND COALESCE((SELECT a.state FROM attempts a WHERE a.event_id = d.event_id                     AND a.internal_state = 'settled'                   ORDER BY a.attempt_no DESC LIMIT 1), '') = COALESCE(?, ''))        OR EXISTS (SELECT 1 FROM attempts a2 WHERE a2.event_id = d.event_id                     AND a2.internal_state = 'settled' AND a2.state = ?)) AND NOT EXISTS (SELECT 1 FROM fault_overtaken_deliveries o                  WHERE o.event_id = d.event_id) AND d.event_id > ? ORDER BY d.event_id LIMIT ?"
	after, checked := "", 0
	var overtaken []string
	for {
		rows, err := sw.Store.All(ctx, query, append(append(pick(signature["recipient"]), settledDelivery...), busyCap, state, state, after, sweepLimit)...)
		if err != nil {
			return false, false, err
		}
		for _, r := range rows {
			if checked >= presentChecks {
				return false, true, sw.noteOvertaken(ctx, overtaken)
			}
			checked++
			if sw.SupersessionReason == nil {
				return true, false, sw.noteOvertaken(ctx, overtaken)
			}
			reason, err := sw.SupersessionReason(ctx, text(r, "event_id"))
			if err != nil {
				return false, false, err
			}
			if reason == "" {
				return true, false, sw.noteOvertaken(ctx, overtaken)
			}
			overtaken = append(overtaken, text(r, "event_id")+"\x00"+reason)
		}
		if len(rows) < sweepLimit {
			return false, false, sw.noteOvertaken(ctx, overtaken)
		}
		after = text(rows[len(rows)-1], "event_id")
	}
}

func (sw *Sweeper) noteOvertaken(ctx context.Context, events []string) error {
	if len(events) == 0 {
		return nil
	}
	return sw.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		for _, entry := range events {
			event, reason, _ := strings.Cut(entry, "\x00")
			if _, err := sw.Store.Q(ctx).ExecContext(ctx, "INSERT OR IGNORE INTO fault_overtaken_deliveries (event_id, reason, noted_at) VALUES (?,?,?)", event, reason, sw.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// WallClockISO is faultsweep._now.
func WallClockISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
