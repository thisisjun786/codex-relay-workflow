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

// Sweeper derives the delivery faults of one store.
type Sweeper struct {
	Store *store.Store
	// Current is delivery.supersession_reason(...) is None: whether a delivery still says
	// something current (the send path's own rule, owned by the delivery package).
	Current func(ctx context.Context, eventID string) (bool, error)
	// MaxAttempts is the RetryPolicy's max_attempts.
	MaxAttempts  int64
	Installation Installation
	// Now stamps the cursors, as faultsweep._now reads the wall clock.
	Now func() string
}

// Batch is one sweep's answer for the classes this subset derives.
type Batch struct {
	Observations []Observation
	Clears       []Observation
	positions    map[string]any
}

func pick(values ...any) []any { return values }

// Sweep is faultsweep.sweep for delivery_stalled (its hold page and its retry page) and the
// clears recovered() derives for that class.
func (sw *Sweeper) Sweep(ctx context.Context, product string) (Batch, error) {
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
	derived := append(append([]Observation(nil), held.observations...), retries.observations...)
	clears, recoveredCursor, err := sw.recovered(ctx, product, cursors["recovered"])
	if err != nil {
		return Batch{}, err
	}
	active := map[string]bool{}
	for _, o := range derived {
		active[o.FaultClass+"|"+canonicalSignature(o.Signature)] = true
	}
	var kept []Observation
	for _, c := range clears {
		if !active[c.FaultClass+"|"+canonicalSignature(c.Signature)] {
			kept = append(kept, c)
		}
	}
	positions := map[string]any{"delivery_stalled": held.cursor, "delivery_retrying": retries.cursor, "recovered": recoveredCursor}
	return Batch{Observations: derived, Clears: kept, positions: positions}, nil
}

// RecordAll is record_all: every observation recorded, then the cursors advanced.
func (sw *Sweeper) RecordAll(ctx context.Context, ledger *Ledger, batch Batch) error {
	for _, o := range append(append([]Observation(nil), batch.Observations...), batch.Clears...) {
		if _, err := ledger.Record(ctx, o); err != nil {
			return err
		}
	}
	return sw.writeCursors(ctx, batch.positions)
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
		for _, source := range []string{"delivery_stalled", "delivery_retrying", "recovered"} {
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
			if m, ok := v.(map[string]any); ok {
				at, until = m["at"], m["until"]
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
		at, until = asInt(at), asInt(until)
	}
	if until == nil {
		at = nil
	}
	return at, until, nil
}

func asInt(v any) any {
	switch n := v.(type) {
	case json.Number:
		i, _ := n.Int64()
		return i
	case float64:
		return int64(n)
	}
	return v
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
	base := os.Getenv("XDG_STATE_HOME")
	root := filepath.Join(os.Getenv("HOME"), ".local", "state")
	if base != "" {
		root = base
	}
	path := filepath.Join(root, "codex-relay-workflow", "host-record.json")
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
		if real, err := filepath.EvalSymlinks(location); e != nil && err == nil && real == here {
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
	return answer(revision, nil, nil)
}

func (sw *Sweeper) current(ctx context.Context, event string, cache map[string]bool) (bool, error) {
	if v, ok := cache[event]; ok {
		return v, nil
	}
	v, err := sw.Current(ctx, event)
	cache[event] = v
	return v, err
}

// settingsState refuses what _settings_items would read a settings hold for: the settings
// reading is the delivery/assignment port's, and the fault evidence for it todo 22's.
func settingsState(state string) error {
	if state == "withheld_pre_send" || state == "inbox_only" {
		return fmt.Errorf("%w: settings-hold evidence for a %s delivery", ErrNotPorted, state)
	}
	return nil
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
		if err := settingsState(text(r, "state")); err != nil {
			return page{}, err
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
		obs = append(obs, Observation{Product: product, FaultClass: "delivery_stalled", Severity: severity,
			Signature: map[string]any{"recipient": recipient, "attemptState": r.Get("last_state")}, OccurrenceKey: occurrenceKey, Scope: scope,
			Detail: fmt.Sprintf("a delivery to %s is held: %s", recipient, hold),
			Evidence: []any{evidence("row", "deliveries:"+text(r, "event_id"), map[string]any{"state": r.Get("state"), "holdReason": r.Get("hold_reason"), "attemptCount": r.Get("attempt_count"), "lastAttemptState": r.Get("last_state"), "relationship": r.Get("relationship_id")}),
				sw.facts("the delivery reaches "+recipient, fmt.Sprintf("held (%s) after %d attempts; the last settled attempt ended %s", hold, integer(r, "attempt_count"), pyStr(r.Get("last_state"))),
					"the recipient is not given this delivery while the hold stands", []any{"read from settled attempts only; one still in flight is not counted"},
					map[string]any{"event": r.Get("event_id"), "relationship": r.Get("relationship_id"), "generation": r.Get("generation"), "turn": r.Get("turn")})}})
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
		if cause != nil || text(r, "delivery_state") == "withheld_pre_send" {
			return page{}, fmt.Errorf("%w: settings evidence on an attempt", ErrNotPorted)
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
		obs = append(obs, Observation{Product: product, FaultClass: "delivery_stalled", Severity: severity,
			Signature: map[string]any{"recipient": recipient, "attemptState": state}, OccurrenceKey: "delivery:" + text(r, "request_id"), Scope: scope,
			Detail: fmt.Sprintf("an attempt to deliver to %s ended %s", recipient, state),
			Evidence: []any{evidence("row", "attempts:"+text(r, "request_id"), map[string]any{"attemptState": state, "deliveryState": r.Get("delivery_state"), "holdReason": r.Get("hold_reason"), "attemptCount": r.Get("attempt_count"), "event": r.Get("event_id")}),
				sw.facts("the attempt reaches "+recipient, "the attempt ended "+state, "the delivery is retried and has not reached its recipient", []any{"one occurrence per settled failed attempt; attempts in flight are not read"},
					map[string]any{"event": r.Get("event_id"), "relationship": r.Get("relationship_id"), "generation": r.Get("generation"), "turn": r.Get("turn")})}})
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

// recovered is recovered() for delivery_stalled: open faults of the class whose own source no
// longer produces them, asked of each fault directly (an exact existence query).
func (sw *Sweeper) recovered(ctx context.Context, product string, cursor any) ([]Observation, any, error) {
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(fault_id) FROM fault_ledger", false)
	if err != nil || until == nil {
		return nil, nil, err
	}
	afterText, _ := after.(string)
	rows, err := sw.Store.All(ctx, "SELECT fault_id, fault_class, signature, cycle, scope FROM fault_ledger WHERE product = ? AND state IN (?,?,?) AND cleared_at IS NULL   AND fault_class IN (?,?,?,?,?)   AND fault_id > ? AND fault_id <= ? ORDER BY fault_id LIMIT ?",
		product, Observed, Open, FixPending, "delivery_stalled", "record_sync_failed", "observation_stalled", "delivery_refused", "managed_start_failed", afterText, until, sweepLimit)
	if err != nil {
		return nil, nil, err
	}
	var clears []Observation
	for _, r := range rows {
		if text(r, "fault_class") != "delivery_stalled" {
			continue
		}
		signature := loadsMap(text(r, "signature"))
		present, err := sw.stillPresent(ctx, signature)
		if err != nil {
			return nil, nil, err
		}
		if present {
			continue
		}
		last, err := sw.Store.One(ctx, "SELECT occurrence_id FROM fault_occurrences WHERE fault_id = ? AND cleared = 0 ORDER BY rowid DESC LIMIT 1", text(r, "fault_id"))
		if err != nil {
			return nil, nil, err
		}
		key := fmt.Sprintf("cleared:after:%d", integer(r, "cycle"))
		if last != nil {
			key = "cleared:after:" + text(last, "occurrence_id")
		}
		clears = append(clears, Observation{Product: product, FaultClass: "delivery_stalled", Severity: Notice, Signature: signature, OccurrenceKey: key,
			Scope: loadsMap(text(r, "scope")), Cleared: true, Detail: "this sweep read the source and no longer derives this fault",
			Evidence: []any{evidence("sweep", "delivery_stalled", map[string]any{"derived": false})}})
	}
	var next any
	if len(rows) >= sweepLimit {
		next = map[string]any{"at": rows[len(rows)-1].Get("fault_id"), "until": until}
	}
	return clears, next, nil
}

// stillPresent is still_present for delivery_stalled via _first_current. The overtaken-delivery
// memo (fault_overtaken_deliveries) is written as Python writes it.
func (sw *Sweeper) stillPresent(ctx context.Context, signature map[string]any) (bool, error) {
	if signature["attemptState"] == busyAttempt {
		return false, nil
	}
	state := signature["attemptState"]
	query := "SELECT d.event_id FROM deliveries d WHERE d.recipient_task_id = ? AND d.state NOT IN (?,?,?)   AND " + notSuperseded + "   AND ((d.hold_reason IS NOT NULL AND d.hold_reason != ?         AND COALESCE((SELECT a.state FROM attempts a WHERE a.event_id = d.event_id                     AND a.internal_state = 'settled'                   ORDER BY a.attempt_no DESC LIMIT 1), '') = COALESCE(?, ''))        OR EXISTS (SELECT 1 FROM attempts a2 WHERE a2.event_id = d.event_id                     AND a2.internal_state = 'settled' AND a2.state = ?)) AND NOT EXISTS (SELECT 1 FROM fault_overtaken_deliveries o                  WHERE o.event_id = d.event_id) AND d.event_id > ? ORDER BY d.event_id LIMIT ?"
	after, checked := "", 0
	var overtaken []string
	for {
		rows, err := sw.Store.All(ctx, query, append(append(pick(signature["recipient"]), settledDelivery...), busyCap, state, state, after, sweepLimit)...)
		if err != nil {
			return false, err
		}
		for _, r := range rows {
			if checked >= presentChecks {
				return false, fmt.Errorf("%w: a presence check past %d deliveries", ErrNotPorted, presentChecks)
			}
			checked++
			ok, err := sw.Current(ctx, text(r, "event_id"))
			if err != nil {
				return false, err
			}
			if ok {
				return true, sw.noteOvertaken(ctx, overtaken)
			}
			overtaken = append(overtaken, text(r, "event_id"))
		}
		if len(rows) < sweepLimit {
			return false, sw.noteOvertaken(ctx, overtaken)
		}
		after = text(rows[len(rows)-1], "event_id")
	}
}

func (sw *Sweeper) noteOvertaken(ctx context.Context, events []string) error {
	if len(events) == 0 {
		return nil
	}
	return fmt.Errorf("%w: noting overtaken deliveries", ErrNotPorted)
}

// WallClockISO is faultsweep._now.
func WallClockISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
