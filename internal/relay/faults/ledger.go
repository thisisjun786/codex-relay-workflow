package faults

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Words and bounds (faults.py). Subset ported for todo 21; todo 22 owns this.
const (
	SchemaObservation = "fault-observation/1"
	idWidth           = 32
	Notice            = "notice"
	Degraded          = "degraded"
	Broken            = "broken"
	Observed          = "observed"
	Open              = "open"
	FixPending        = "fix_pending"
	Resolved          = "resolved"
	Withdrawn         = "withdrawn"
	pending           = "pending"
	cancelled         = "cancelled"
	openRecord        = "open_record"
	appendComment     = "append_comment"
	blocking          = "blocking"
	triggerOpen       = "open"
	triggerEscalate   = "escalate"
	triggerRecur      = "recur"
	triggerReopen     = "reopen"
	occurrence        = "occurrence"
	clearedKind       = "cleared"
	maxEvidence       = 8
	maxEvidenceBytes  = 4096
	renderedOccur     = 3
	defaultWindow     = 21600.0
)

var severityRank = map[string]int{Notice: 0, Degraded: 1, Broken: 2}

var productName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type classPolicy struct{ component, clears string }

// classes are the registered classes this subset can record (register_class).
var classes = map[string]classPolicy{
	"delivery_stalled": {"delivery", "a delivery to the same recipient reaching dispatched"},
}

// ErrNotPorted marks a ledger path todo 22 ports; this subset refuses it rather than guessing.
var ErrNotPorted = errors.New("faults: not ported in the todo 21 subset (todo 22 owns this)")

// Observation is faults.observation.
type Observation struct {
	Product, FaultClass, Severity string
	Signature                     map[string]any
	OccurrenceKey                 string
	Scope                         map[string]any
	Detail                        string
	Evidence                      []any
	Cleared                       bool
}

func canonicalSignature(signature map[string]any) string { return dumps(signature, true) }

// FaultID is fault_id without a workspace.
func FaultID(product, class string, signature map[string]any) string {
	return sha256Hex(product + "|" + class + "|" + canonicalSignature(signature))[:idWidth]
}

func occurrenceID(identifier, key string, episode int64, cleared bool) string {
	direction := "active"
	if cleared {
		direction = "cleared"
	}
	return sha256Hex(fmt.Sprintf("%s|%d|%s|%s", identifier, episode, direction, key))[:idWidth]
}

func publicationID(identifier, kind, trigger string) string {
	return sha256Hex(identifier + "|" + kind + "|" + trigger)[:idWidth]
}

func identityDigest(identifier, kind, trigger string, cycle int64) string {
	return sha256Hex(fmt.Sprintf("%s|%s|%s|%d", identifier, kind, trigger, cycle))
}

func evidenceDigest(evidence []any) string { return sha256Hex(dumps(evidence, true)) }

// scopeKeyFor is scope_key_for without a workspace.
func scopeKeyFor(product string, scope map[string]any) string {
	if p := scope["projectKey"]; named(p) {
		return product + ":" + p.(string)
	}
	return product
}

func boundedEvidence(evidence []any) ([]any, bool) {
	kept := evidence
	truncated := false
	if len(kept) > maxEvidence {
		kept, truncated = kept[:maxEvidence], true
	}
	kept = append([]any(nil), kept...)
	for len(kept) > 0 && len(dumpsASCIIDefault(kept)) > maxEvidenceBytes {
		kept = kept[:len(kept)-1]
		truncated = true
	}
	return kept, truncated
}

// dumpsASCIIDefault is json.dumps(kept, ensure_ascii=False) (insertion order, as Python measures
// it); the byte length of a sorted rendering is the same, which is all it is used for.
func dumpsASCIIDefault(v any) string { return dumps(v, false) }

type fact struct {
	Observation
	id, component, signature, scopeKey, evidenceDigest string
	evidence                                           []any
	truncated                                          bool
	threshold                                          any
	window                                             float64
	publish                                            bool
}

func readObservation(o Observation) (fact, error) {
	policy, ok := classes[o.FaultClass]
	if !ok {
		return fact{}, fmt.Errorf("%w: fault class %q", ErrNotPorted, o.FaultClass)
	}
	if _, ok := severityRank[o.Severity]; !ok || !productName.MatchString(o.Product) || !named(o.OccurrenceKey) || len(o.Signature) == 0 {
		return fact{}, fmt.Errorf("faults: malformed observation")
	}
	if w, ok := o.Scope["workspace"]; ok && w != nil {
		return fact{}, fmt.Errorf("%w: a workspace scope", ErrNotPorted)
	}
	scope := map[string]any{}
	for k, v := range o.Scope {
		if v != nil {
			scope[k] = v
		}
	}
	o.Scope = scope
	evidence, truncated := boundedEvidence(o.Evidence)
	threshold := map[string]any{Broken: int64(1), Degraded: int64(3), Notice: nil}[o.Severity]
	return fact{Observation: o, id: FaultID(o.Product, o.FaultClass, o.Signature), component: policy.component,
		signature: canonicalSignature(o.Signature), scopeKey: scopeKeyFor(o.Product, scope), evidence: evidence,
		evidenceDigest: evidenceDigest(evidence), truncated: truncated, threshold: threshold, window: defaultWindow, publish: threshold != nil}, nil
}

// Clock is the ledger's clock (Python's injected clock).
type Clock interface {
	Now() float64
	ISO() string
}

// Ledger is faults.FaultLedger.
type Ledger struct {
	Store *store.Store
	Clock Clock
}

type row = store.Row

func text(r row, name string) string {
	switch v := r.Get(name).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func integer(r row, name string) int64 {
	switch v := r.Get(name).(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func (l *Ledger) one(ctx context.Context, query string, args ...any) (row, error) {
	return l.Store.One(ctx, query, args...)
}

func (l *Ledger) exec(ctx context.Context, query string, args ...any) (int64, error) {
	result, err := l.Store.Q(ctx).ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Record is FaultLedger.record without adoption: one observation converged on its fault, at
// most one write queued. The answer carries recorded and state; the rest is todo 22's surface.
func (l *Ledger) Record(ctx context.Context, o Observation) (bool, error) {
	f, err := readObservation(o)
	if err != nil {
		return false, err
	}
	nowISO, now := l.Clock.ISO(), l.Clock.Now()
	recorded := false
	err = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		alias, err := l.one(ctx, "SELECT fault_id FROM fault_aliases WHERE alias_id = ?", f.id)
		if err != nil {
			return err
		}
		if alias != nil {
			return fmt.Errorf("%w: an aliased fault", ErrNotPorted)
		}
		existing, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", f.id)
		if err != nil {
			return err
		}
		if existing == nil && f.Cleared {
			return nil
		}
		scopeText := dumps(f.Scope, false)
		if existing == nil {
			other, err := l.one(ctx, "SELECT fault_id, product FROM fault_ledger WHERE scope_key = ? AND product != ? LIMIT 1", f.scopeKey, f.Product)
			if err == nil && other == nil {
				other, err = l.one(ctx, "SELECT product FROM fault_target_projects WHERE scope_key = ? AND product != ?", f.scopeKey, f.Product)
			}
			if err != nil {
				return err
			}
			if other != nil {
				return fmt.Errorf("fault_scope_conflict: scope key %s is already carried by product %s", f.scopeKey, text(other, "product"))
			}
			if _, err := l.exec(ctx, "INSERT INTO fault_ledger (fault_id, product, fault_class, component,  severity, signature, scope, scope_key, state, cycle, occurrence_count,  reopen_count, detail, suppression, first_seen_at, last_seen_at,  updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,0,0,?,?,?,?,?)",
				f.id, f.Product, f.FaultClass, f.component, f.Severity, f.signature, scopeText, f.scopeKey, Observed, f.Detail, nil, nowISO, nowISO, nowISO); err != nil {
				return err
			}
		} else if f.scopeKey != text(existing, "scope_key") || scopeText != text(existing, "scope") {
			return fmt.Errorf("%w: rescoping a fault", ErrNotPorted)
		}
		r, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", f.id)
		if err != nil {
			return err
		}
		state := text(r, "state")
		if f.Cleared && (state == Withdrawn || state == Resolved) {
			return nil
		}
		episode := integer(r, "episode")
		if !f.Cleared && r.Get("cleared_at") != nil {
			episode++
		}
		occurrenceKey := f.OccurrenceKey
		oid := occurrenceID(f.id, occurrenceKey, episode, f.Cleared)
		seen, err := l.one(ctx, "SELECT 1 FROM fault_timeline WHERE fault_id = ? AND ref_id = ?", f.id, oid)
		if err != nil {
			return err
		}
		storedKey := occurrenceKey
		if f.Cleared {
			storedKey += "#cleared"
		}
		if _, err := l.exec(ctx, "INSERT OR IGNORE INTO fault_occurrences (occurrence_id, fault_id, episode,  occurrence_key, severity, cleared, detail, evidence, evidence_digest,  truncated, observed_at, recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			oid, f.id, episode, storedKey, f.Severity, boolInt(f.Cleared), f.Detail, dumps(f.evidence, false), f.evidenceDigest, boolInt(f.truncated), nil, nowISO, now); err != nil {
			return err
		}
		if seen != nil {
			return nil
		}
		kind := occurrence
		if f.Cleared {
			kind = clearedKind
		}
		if _, err := l.exec(ctx, "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)", f.id, integer(r, "cycle"), kind, oid, f.Detail, nowISO, now); err != nil {
			return err
		}
		count := integer(r, "occurrence_count")
		if !f.Cleared {
			count++
		}
		severity := text(r, "severity")
		if severityRank[f.Severity] > severityRank[severity] {
			severity = f.Severity
		}
		escalated := severity != text(r, "severity")
		f.threshold = map[string]any{Broken: int64(1), Degraded: int64(3), Notice: nil}[severity]
		f.publish = f.threshold != nil
		override, err := l.one(ctx, "SELECT threshold, window_seconds, reason, updated_at FROM fault_policies WHERE product = ? AND fault_class = ? AND severity = ?", f.Product, f.FaultClass, severity)
		if err != nil {
			return err
		}
		if override != nil {
			return fmt.Errorf("%w: a policy override", ErrNotPorted)
		}
		suppression, publish, err := l.suppression(ctx, f.id, f, severity, now)
		if err != nil {
			return err
		}
		opened, create, err := issueSlot(ctx, l, r)
		if err != nil {
			return err
		}
		landed, err := l.one(ctx, "SELECT 1 FROM fault_publications WHERE fault_id = ? AND state IN (?,?,?) LIMIT 1", f.id, "issued", "uncertain", "confirmed")
		if err != nil {
			return err
		}
		next, cycle, reopened, trigger := transition(state, integer(r, "cycle"), f.Cleared, publish, escalated, severity, landed != nil, opened != "")
		if next == Withdrawn || reopened || next == Resolved {
			return fmt.Errorf("%w: a %s transition", ErrNotPorted, next)
		}
		detail := f.Detail
		if detail == "" {
			detail = text(r, "detail")
		}
		var clearedAt any
		if f.Cleared {
			clearedAt = nowISO
		}
		var resolvedAt any
		if next == Resolved {
			resolvedAt = r.Get("resolved_at")
		}
		if _, err := l.exec(ctx, "UPDATE fault_ledger SET state = ?, cycle = ?, severity = ?, episode = ?,  occurrence_count = ?, reopen_count = reopen_count + ?, detail = ?,  suppression = ?, last_seen_at = ?, cleared_at = ?, resolved_at = ?,  updated_at = ? WHERE fault_id = ?",
			next, cycle, severity, episode, count, 0, detail, suppression, nowISO, clearedAt, resolvedAt, nowISO, f.id); err != nil {
			return err
		}
		if trigger != "" {
			if err := l.enqueue(ctx, f.id, trigger, nowISO, opened, create, classes[f.FaultClass].clears); err != nil {
				return err
			}
			reason, _, _ := strings.Cut(trigger, ":")
			if (reason == triggerOpen || reason == triggerReopen) && severity == Broken {
				if err := l.notify(ctx, f.id, cycle, nowISO); err != nil {
					return err
				}
			}
		}
		recorded = true
		return nil
	})
	return recorded, err
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (l *Ledger) suppression(ctx context.Context, identifier string, f fact, severity string, now float64) (string, bool, error) {
	if !f.publish {
		return dumps(map[string]any{"publish": false, "threshold": nil, "window": f.window, "counted": nil,
			"reason": fmt.Sprintf("a %s is recorded for an operator and never filed", severity)}, false), false, nil
	}
	r, err := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_timeline WHERE fault_id = ? AND kind = ? AND recorded_ts >= ?", identifier, occurrence, now-f.window)
	if err != nil {
		return "", false, err
	}
	counted := integer(r, "n")
	threshold := f.threshold.(int64)
	publish := counted >= threshold
	reason := fmt.Sprintf("%d observation(s) inside %ds is under the threshold of %d", counted, int64(f.window), threshold)
	if publish {
		reason = fmt.Sprintf("%d observation(s) inside %ds reached the threshold of %d", counted, int64(f.window), threshold)
	}
	return dumps(map[string]any{"publish": publish, "threshold": threshold, "window": f.window, "counted": counted, "reason": reason}, false), publish, nil
}

// issueSlot is _issue_slot: "issue", "create", or "" when free (then create may be a cancelled
// create row).
func issueSlot(ctx context.Context, l *Ledger, fault row) (string, row, error) {
	if text(fault, "external_ref") != "" {
		return "issue", nil, nil
	}
	create, err := l.one(ctx, "SELECT * FROM fault_publications WHERE fault_id = ? AND kind = ?", text(fault, "fault_id"), openRecord)
	if err != nil {
		return "", nil, err
	}
	if create != nil && text(create, "state") != cancelled {
		return "create", create, nil
	}
	return "", create, nil
}

// transition is _transition.
func transition(state string, cycle int64, cleared, publishable, escalated bool, severity string, landed, opened bool) (string, int64, bool, string) {
	if cleared {
		if landed || state == Resolved || state == Withdrawn {
			return state, cycle, false, ""
		}
		return Withdrawn, cycle, false, ""
	}
	if state == Withdrawn {
		state = Observed
	}
	switch state {
	case Resolved:
		if !opened {
			if publishable {
				return Open, cycle + 1, true, triggerOpen
			}
			return Open, cycle + 1, true, ""
		}
		return Open, cycle + 1, true, fmt.Sprintf("%s:%d", triggerReopen, cycle+1)
	case FixPending:
		if !opened {
			if publishable {
				return Open, cycle, false, triggerOpen
			}
			return Open, cycle, false, ""
		}
		return Open, cycle, false, fmt.Sprintf("%s:%d", triggerRecur, cycle)
	case Open:
		if !opened && publishable {
			return Open, cycle, false, triggerOpen
		}
		if escalated {
			return Open, cycle, false, triggerEscalate + ":" + severity
		}
		return Open, cycle, false, ""
	}
	if publishable {
		return Open, cycle, false, triggerOpen
	}
	return Observed, cycle, false, ""
}

// enqueue is _enqueue for a fault with no adoption and no issue: the one open_record, then
// comments (which need an issue this subset never has, so they are not queued).
func (l *Ledger) enqueue(ctx context.Context, identifier, trigger, now, holder string, create row, clears string) error {
	adoption, err := l.one(ctx, "SELECT 1 FROM fault_adoptions WHERE fault_id = ? AND state = ?", identifier, pending)
	if err != nil {
		return err
	}
	reason, _, _ := strings.Cut(trigger, ":")
	if reason == triggerOpen && adoption != nil {
		return fmt.Errorf("%w: materializing an adoption", ErrNotPorted)
	}
	switch {
	case reason == triggerOpen && holder == "issue", reason != triggerOpen && holder == "issue":
		return fmt.Errorf("%w: a comment on an owned issue", ErrNotPorted)
	case reason == triggerOpen && holder == "create":
		return nil
	case reason == triggerOpen:
		return l.insertPublication(ctx, identifier, openRecord, triggerOpen, now, clears)
	case holder == "":
		return nil
	}
	// A live create holds the slot and the fault owns no issue yet: a comment is queued, and
	// its target mode is none (KINDS[append_comment]).
	return l.insertPublication(ctx, identifier, appendComment, trigger, now, clears)
}

func (l *Ledger) insertPublication(ctx context.Context, identifier, kind, trigger, now, clears string) error {
	fault, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", identifier)
	if err != nil {
		return err
	}
	publication := publicationID(identifier, kind, trigger)
	var team any
	targetMode := "none"
	if kind == openRecord {
		targetMode = "team+project"
		target, err := l.one(ctx, "SELECT t.tracker_ref, p.project_ref, p.product FROM fault_targets t LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key WHERE t.scope_key = ?", text(fault, "scope_key"))
		if err != nil {
			return err
		}
		if target != nil {
			return fmt.Errorf("%w: a configured fault target", ErrNotPorted)
		}
	}
	occurrences, err := l.Store.All(ctx, "SELECT * FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?", identifier, renderedOccur)
	if err != nil {
		return err
	}
	summary := renderSummary(fault, trigger, occurrences, clears, publication)
	existing, err := l.one(ctx, "SELECT state FROM fault_publications WHERE publication_id = ?", publication)
	if err != nil {
		return err
	}
	digest := identityDigest(identifier, kind, trigger, integer(fault, "cycle"))
	switch {
	case existing == nil:
		if _, err := l.exec(ctx, "INSERT INTO fault_publications (publication_id, fault_id, kind, trigger_key,  cycle, tracker_ref, external_ref, summary, identity_digest, state, attempts,  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?)",
			publication, identifier, kind, trigger, integer(fault, "cycle"), team, fault.Get("external_ref"), summary, digest, pending, now, now); err != nil {
			return err
		}
	case text(existing, "state") == cancelled:
		return fmt.Errorf("%w: reviving a cancelled publication", ErrNotPorted)
	default:
		return nil
	}
	_, err = l.exec(ctx, "INSERT INTO fault_publication_payloads (publication_id, project_ref, payload,  hold_reason, updated_at, target_mode) VALUES (?,?,?,?,?,?) ON CONFLICT(publication_id) DO UPDATE SET project_ref = excluded.project_ref, payload = excluded.payload,   hold_reason = excluded.hold_reason, updated_at = excluded.updated_at,   target_mode = excluded.target_mode",
		publication, nil, nil, nil, now, targetMode)
	return err
}

func (l *Ledger) notify(ctx context.Context, identifier string, cycle int64, now string) error {
	fault, err := l.one(ctx, "SELECT product FROM fault_ledger WHERE fault_id = ?", identifier)
	if err != nil {
		return err
	}
	notification := sha256Hex(fmt.Sprintf("%s|%s|%d", identifier, blocking, cycle))[:idWidth]
	_, err = l.exec(ctx, "INSERT INTO fault_notifications (notification_id, fault_id, product,  kind, reason, cycle, ref, state, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(notification_id) DO UPDATE SET state = excluded.state,   last_error = NULL, updated_at = excluded.updated_at WHERE fault_notifications.state = ?",
		notification, identifier, text(fault, "product"), blocking, nil, cycle, nil, pending, now, now, Withdrawn)
	return err
}

func domain(fault row) string {
	signature, err := loads(text(fault, "signature"))
	m, ok := signature.(map[string]any)
	if err != nil || !ok {
		return text(fault, "signature")
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + pyStr(m[k])
	}
	return strings.Join(parts, " ")
}

func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

// renderSummary is render_summary for the open and escalate triggers this subset queues.
func renderSummary(fault row, trigger string, occurrences []row, clears, publication string) string {
	lines := []string{fmt.Sprintf("[%s] %s: %s", text(fault, "product"), text(fault, "fault_class"), domain(fault)), ""}
	reason, _, _ := strings.Cut(trigger, ":")
	switch reason {
	case triggerOpen:
		lines = append(lines, fmt.Sprintf("The relay recorded a %s fault in its %s path and is filing it once.", text(fault, "severity"), text(fault, "component")))
	case triggerEscalate:
		lines = append(lines, fmt.Sprintf("This fault escalated to %s.", text(fault, "severity")))
	}
	lines = append(lines, "", "fault: "+text(fault, "fault_id"),
		fmt.Sprintf("class: %s  component: %s  severity: %s", text(fault, "fault_class"), text(fault, "component"), text(fault, "severity")),
		"domain: "+domain(fault),
		fmt.Sprintf("observed: %d occurrence(s), first %s, most recent %s", integer(fault, "occurrence_count"), text(fault, "first_seen_at"), text(fault, "last_seen_at")))
	if clears != "" {
		lines = append(lines, "clears when: "+clears)
	}
	if d := text(fault, "detail"); d != "" {
		lines = append(lines, "detail: "+d)
	}
	if len(occurrences) > 0 {
		lines = append(lines, "", "evidence, as observed at the time:")
		for _, entry := range occurrences {
			lines = append(lines, fmt.Sprintf("- %s %s", text(entry, "recorded_at"), text(entry, "occurrence_key")))
			evidence, _ := loads(text(entry, "evidence"))
			items, _ := evidence.([]any)
			for _, item := range items {
				lines = append(lines, "    "+dumps(item, false))
			}
			if integer(entry, "truncated") != 0 {
				lines = append(lines, "    (evidence truncated at the recorded bound)")
			}
		}
	}
	lines = append(lines, "", "Recorded by codex-session-relay. The occurrence count counts observations, not incidents: a source that overwrites its own history is observed once per sweep, not once per failure.")
	if publication != "" {
		lines = append(lines, "publication: "+publication)
	}
	return strings.Join(lines, "\n")
}
