package faults

import (
	"context"
	"database/sql"
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
	triggerFix        = "fix"
	triggerResolve    = "resolve"
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

var declaredClasses = map[string]classPolicy{
	"completion_mismatch":    {"completion", "the evidence the completion lacked being observed and reverified"},
	"completion_unverified":  {"completion", "a later reading that establishes the check either way"},
	"product_defect":         {"product", "a later reading of the same product, component and symptom that finds it gone, or the fix and reverification loop"},
	"product_expected":       {"product", "the expected state ending: the cancellation settled, the approval given, or the support added"},
	"project_needed":         {"planning", "the project being created and bound; the record stays as that project's creation record"},
	"unclassified_incident":  {"triage", "classification into a registered product, which re-files every stored incident there"},
	"delivery_refused":       {"delivery", "the delivery being sent or settling, or its newest withholding naming another reason"},
	"delivery_stalled":       {"delivery", "a delivery to the same recipient reaching dispatched"},
	"managed_start_failed":   {"managed_start", "the request recording an accepted receipt or attaching, or its newest creation-stage answer saying something else"},
	"observation_stalled":    {"observation", "a successful poll of the same anchor, or its turn settling"},
	"observation_unmeasured": {"reporting", "a later reading of the same turn that establishes something"},
	"record_sync_failed":     {"sync", "any synchronisation job on the same target confirming"},
	"report_omitted":         {"reporting", "a reading of the same turn that no longer says unreported"},
}

// classes are declarations imported in this process. Python starts with the seven
// fault-ledger classes and adds product classes only when routing imports projects.
var classes = func() map[string]classPolicy {
	out := map[string]classPolicy{}
	for _, name := range []string{"delivery_refused", "delivery_stalled", "managed_start_failed", "observation_stalled", "observation_unmeasured", "record_sync_failed", "report_omitted"} {
		out[name] = declaredClasses[name]
	}
	return out
}()

// Observation is faults.observation.
type Observation struct {
	Product, FaultClass, Severity string
	Signature                     map[string]any
	OccurrenceKey                 string
	Scope                         map[string]any
	Detail                        string
	Evidence                      []any
	Cleared                       bool
	ObservedAt                    any
}

func canonicalSignature(signature map[string]any) string { return dumps(signature, true) }

// FaultID is fault_id without a workspace.
func FaultID(product, class string, signature map[string]any) string {
	return FaultIDInWorkspace(product, class, signature, "")
}

// FaultIDInWorkspace preserves old identities when no workspace was given.
func FaultIDInWorkspace(product, class string, signature map[string]any, workspace string) string {
	text := product + "|" + class + "|" + canonicalSignature(signature)
	if workspace != "" {
		text += "|workspace=" + workspace
	}
	return sha256Hex(text)[:idWidth]
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

func encodeScopePart(part string) string {
	r := strings.NewReplacer("%", "%25", "|", "%7C", ":", "%3A", "@", "%40")
	return r.Replace(part)
}
func scopeKeyFor(product string, scope map[string]any) string {
	project := ""
	if p := scope["projectKey"]; named(p) {
		project = p.(string)
	}
	if workspace, ok := scope["workspace"].(string); ok {
		return "ws|" + encodeScopePart(product) + "|" + encodeScopePart(workspace) + "|" + encodeScopePart(project)
	}
	if project != "" {
		return product + ":" + project
	}
	return product
}

func boundedEvidence(evidence []any) ([]any, bool) {
	filtered := make([]any, 0, len(evidence))
	truncated := false
	for _, entry := range evidence {
		if _, ok := entry.(map[string]any); ok {
			filtered = append(filtered, entry)
		} else {
			truncated = true
		}
	}
	kept := filtered
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
	policy, ok := classLookup(o.FaultClass)
	if !ok {
		return fact{}, fmt.Errorf("fault_class_unregistered: '%s' is not a registered fault class, so nothing declares what would clear it", o.FaultClass)
	}
	if _, ok := severityRank[o.Severity]; !ok || !productName.MatchString(o.Product) || !named(o.OccurrenceKey) || len(o.Signature) == 0 {
		return fact{}, fmt.Errorf("fault_observation_malformed: malformed observation")
	}
	workspace, _ := o.Scope["workspace"].(string)
	if _, ok := o.Scope["workspace"]; ok && !named(o.Scope["workspace"]) {
		return fact{}, fmt.Errorf("fault_observation_malformed: a workspace is a non-blank string")
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
	return fact{Observation: o, id: FaultIDInWorkspace(o.Product, o.FaultClass, o.Signature, workspace), component: policy.component,
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
	return l.record(ctx, o, nil)
}

func (l *Ledger) record(ctx context.Context, o Observation, adoption *Adoption) (bool, error) {
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
			f.id = text(alias, "fault_id")
		} else if workspace, ok := f.Scope["workspace"].(string); ok {
			legacy := FaultID(f.Product, f.FaultClass, f.Signature)
			old, e := l.one(ctx, "SELECT scope FROM fault_ledger WHERE fault_id = ?", legacy)
			if e != nil {
				return e
			}
			if old != nil && loadsMap(text(old, "scope"))["workspace"] == workspace {
				if _, e := l.exec(ctx, "INSERT INTO fault_aliases (alias_id, fault_id, created_at) VALUES (?,?,?)", f.id, legacy, nowISO); e != nil {
					return e
				}
				f.id = legacy
			}
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
				return fmt.Errorf("fault_scope_conflict: scope key %s is already carried by product %s; one product's issues are never filed through another's key", f1Repr(f.scopeKey), f1Repr(text(other, "product")))
			}
			if _, err := l.exec(ctx, "INSERT INTO fault_ledger (fault_id, product, fault_class, component,  severity, signature, scope, scope_key, state, cycle, occurrence_count,  reopen_count, detail, suppression, first_seen_at, last_seen_at,  updated_at) VALUES (?,?,?,?,?,?,?,?,?,1,0,0,?,?,?,?,?)",
				f.id, f.Product, f.FaultClass, f.component, f.Severity, f.signature, scopeText, f.scopeKey, Observed, f.Detail, nil, nowISO, nowISO, nowISO); err != nil {
				return err
			}
		} else if f.scopeKey != text(existing, "scope_key") && f.Scope["workspace"] == loadsMap(text(existing, "scope"))["workspace"] {
			if err := l.rescope(ctx, existing, f.Scope, nowISO); err != nil {
				return err
			}
		}
		r, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", f.id)
		if err != nil {
			return err
		}
		if adoption != nil {
			if _, err := f2Adopt(ctx, l, r, adoption.ExternalRef, adoption.Scope, nowISO); err != nil {
				return err
			}
			r, err = l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", f.id)
			if err != nil {
				return err
			}
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
			oid, f.id, episode, storedKey, f.Severity, boolInt(f.Cleared), f.Detail, dumps(f.evidence, false), f.evidenceDigest, boolInt(f.truncated), f.ObservedAt, nowISO, now); err != nil {
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
		if threshold, ok := classThreshold(f.FaultClass); ok && severity == Degraded {
			f.threshold = threshold
		}
		f.publish = f.threshold != nil
		override, err := l.one(ctx, "SELECT threshold, window_seconds, reason, updated_at FROM fault_policies WHERE product = ? AND fault_class = ? AND severity = ?", f.Product, f.FaultClass, severity)
		if err != nil {
			return err
		}
		if override != nil {
			if override.Get("threshold") != nil {
				f.threshold = integer(override, "threshold")
				f.publish = true
			}
			if override.Get("window_seconds") != nil {
				f.window = override.Get("window_seconds").(float64)
			}
		}
		suppression, publish, err := l.suppression(ctx, f.id, f, severity, now, override)
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
		if next == Withdrawn && state != Withdrawn {
			if err := l.cancelUnissued(ctx, f.id, "the fault was withdrawn before anything landed", nowISO); err != nil {
				return err
			}
			if _, err := l.exec(ctx, "UPDATE fault_notifications SET state = ?, updated_at = ? WHERE fault_id = ? AND state = ? AND kind = ?", Withdrawn, nowISO, f.id, pending, blocking); err != nil {
				return err
			}
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
			next, cycle, severity, episode+boolInt(next == Withdrawn), count, boolInt(reopened), detail, suppression, nowISO, clearedAt, resolvedAt, nowISO, f.id); err != nil {
			return err
		}
		if trigger != "" {
			policy, _ := classLookup(f.FaultClass)
			if err := l.enqueue(ctx, f.id, trigger, nowISO, opened, create, policy.clears); err != nil {
				return err
			}
			reason, _, _ := strings.Cut(trigger, ":")
			if reason == triggerReopen {
				fresh, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", f.id)
				if err != nil {
					return err
				}
				if _, err = f2Update(ctx, l, fresh, triggerReopen, nil, nowISO); err != nil {
					return err
				}
			}
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

// cancelUnissued is Python's _cancel_where for every unissued publication of a fault.
// A claimed publication gives back only its current attempt and budget use.
func (l *Ledger) cancelUnissued(ctx context.Context, identifier, reason, now string) error {
	claimed := "SELECT p.publication_id FROM fault_publications p WHERE p.fault_id = ? AND p.state = ?"
	current := "SELECT MAX(a.attempt_id) FROM fault_publication_attempts a WHERE a.publication_id IN (" + claimed + ") GROUP BY a.publication_id"
	if _, err := l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product = (SELECT product FROM fault_ledger WHERE fault_id = ?) AND ref IN (SELECT a.publication_id || ':' || a.attempt_id FROM fault_publication_attempts a WHERE a.attempt_id IN ("+current+"))", identifier, identifier, "claimed"); err != nil {
		return err
	}
	if _, err := l.exec(ctx, "UPDATE fault_publication_attempts SET outcome = 'cancelled', ended = 1, ended_at = ? WHERE attempt_id IN ("+current+")", now, identifier, "claimed"); err != nil {
		return err
	}
	_, err := l.exec(ctx, "UPDATE fault_publications SET state = ?, claim_token = NULL, lease_owner = NULL, lease_until = NULL, last_error = ?, updated_at = ?, attempts = attempts - (CASE WHEN state = ? THEN 1 ELSE 0 END) WHERE fault_id = ? AND state IN (?,?,?)", cancelled, reason, now, "claimed", identifier, pending, "failed", "claimed")
	return err
}

func (l *Ledger) suppression(ctx context.Context, identifier string, f fact, severity string, now float64, override row) (string, bool, error) {
	source := ""
	if override != nil {
		source = " (product override: " + text(override, "reason") + ")"
	}
	if !f.publish {
		return dumps(map[string]any{"publish": false, "threshold": nil, "window": f.window, "counted": nil,
			"reason": fmt.Sprintf("a %s is recorded for an operator and never filed%s", severity, source)}, false), false, nil
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
	return dumps(map[string]any{"publish": publish, "threshold": threshold, "window": f.window, "counted": counted, "reason": reason + source}, false), publish, nil
}

// Prune removes old occurrence evidence without changing the timeline that decides
// suppression and duplicate observations.
func (l *Ledger) Prune(ctx context.Context, identifier string, keep int) (int64, error) {
	if keep < 1 {
		return 0, fmt.Errorf("fault_observation_malformed: keep at least one")
	}
	var removed int64
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fault, err := cFault(ctx, l, identifier)
		if err != nil {
			return err
		}
		identifier = text(fault, "fault_id")
		removed, err = l.exec(ctx, "DELETE FROM fault_occurrences WHERE fault_id = ? AND rowid NOT IN (SELECT rowid FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?)", identifier, identifier, keep)
		if err != nil {
			return err
		}
		_, err = l.exec(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'fault_prune',?,?)", l.Clock.ISO(), identifier, dumps(map[string]any{"kept": keep, "removed": removed}, false))
		return err
	})
	return removed, err
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
		ref, err := l.one(ctx, "SELECT external_ref FROM fault_adoptions WHERE fault_id = ? AND state = ?", identifier, pending)
		if err != nil {
			return err
		}
		if _, err = l.exec(ctx, "UPDATE fault_ledger SET external_ref = ?, updated_at = ? WHERE fault_id = ? AND external_ref IS NULL", ref.Get("external_ref"), now, identifier); err != nil {
			return err
		}
		if _, err = l.exec(ctx, "UPDATE fault_adoptions SET state = 'materialized', updated_at = ? WHERE fault_id = ?", now, identifier); err != nil {
			return err
		}
		if err = l.insertPublication(ctx, identifier, appendComment, triggerOpen, now, ""); err != nil {
			return err
		}
		fault, err := l.one(ctx, "SELECT product, scope_key FROM fault_ledger WHERE fault_id = ?", identifier)
		if err != nil {
			return err
		}
		target, err := l.one(ctx, "SELECT project_ref FROM fault_target_projects WHERE scope_key = ? AND product = ?", text(fault, "scope_key"), text(fault, "product"))
		if err != nil {
			return err
		}
		if target == nil || target.Get("project_ref") == nil {
			return dUnlinkOne(ctx, l, identifier, now)
		}
		return dRelinkOne(ctx, l, identifier, text(target, "project_ref"), now)
	}
	switch {
	case reason == triggerOpen && holder == "issue", reason != triggerOpen && holder == "issue":
		return l.insertPublication(ctx, identifier, appendComment, trigger, now, clears)
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

func (l *Ledger) enqueueWithRemediation(ctx context.Context, identifier, trigger, now, holder string, create row, remediationID string) error {
	if err := l.enqueue(ctx, identifier, trigger, now, holder, create, ""); err != nil {
		return err
	}
	publication := publicationID(identifier, appendComment, trigger)
	fault, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", identifier)
	if err != nil {
		return err
	}
	remediation, err := l.one(ctx, "SELECT * FROM fault_remediations WHERE remediation_id=?", remediationID)
	if err != nil {
		return err
	}
	occurrences, err := l.Store.All(ctx, "SELECT * FROM fault_occurrences WHERE fault_id=? ORDER BY rowid DESC LIMIT ?", identifier, renderedOccur)
	if err != nil {
		return err
	}
	summary := renderSummaryWithRemediation(fault, trigger, occurrences, publication, remediation)
	_, err = l.exec(ctx, "UPDATE fault_publications SET summary=? WHERE publication_id=?", summary, publication)
	return err
}

func (l *Ledger) insertPublication(ctx context.Context, identifier, kind, trigger, now, clears string) error {
	fault, err := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id = ?", identifier)
	if err != nil {
		return err
	}
	publication := publicationID(identifier, kind, trigger)
	var team any
	targetMode := "none"
	var project any
	if spec := kinds[kind]; spec.Target != "" {
		targetMode = spec.Target
		target, _, err := f1OwnedTarget(ctx, l, fault)
		if err != nil {
			return err
		}
		if target != nil {
			team = target.Get("tracker_ref")
			if spec.Target == "team+project" {
				project = target.Get("project_ref")
			}
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
		if _, err := l.exec(ctx, "UPDATE fault_publications SET state = ?, cycle = ?, tracker_ref = ?, external_ref = ?, summary = ?, identity_digest = ?, attempts = 0, next_attempt_at = NULL, claim_token = NULL, lease_owner = NULL, lease_until = NULL, issued_at = NULL, last_error = NULL, updated_at = ? WHERE publication_id = ?", pending, integer(fault, "cycle"), team, fault.Get("external_ref"), summary, digest, now, publication); err != nil {
			return err
		}
	default:
		return nil
	}
	_, err = l.exec(ctx, "INSERT INTO fault_publication_payloads (publication_id, project_ref, payload,  hold_reason, updated_at, target_mode) VALUES (?,?,?,?,?,?) ON CONFLICT(publication_id) DO UPDATE SET project_ref = excluded.project_ref, payload = excluded.payload,   hold_reason = excluded.hold_reason, updated_at = excluded.updated_at,   target_mode = excluded.target_mode",
		publication, project, nil, nil, now, targetMode)
	return err
}

func (l *Ledger) notify(ctx context.Context, identifier string, cycle int64, now string) error {
	return l.notifyKind(ctx, identifier, blocking, cycle, now)
}

func (l *Ledger) notifyKind(ctx context.Context, identifier, kind string, cycle int64, now string) error {
	fault, err := l.one(ctx, "SELECT product FROM fault_ledger WHERE fault_id = ?", identifier)
	if err != nil {
		return err
	}
	notification := sha256Hex(fmt.Sprintf("%s|%s|%d", identifier, kind, cycle))[:idWidth]
	_, err = l.exec(ctx, "INSERT INTO fault_notifications (notification_id, fault_id, product,  kind, reason, cycle, ref, state, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(notification_id) DO UPDATE SET state = excluded.state,   last_error = NULL, updated_at = excluded.updated_at WHERE fault_notifications.state = ?",
		notification, identifier, text(fault, "product"), kind, nil, cycle, nil, pending, now, now, Withdrawn)
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

// renderSummary is render_summary for publication triggers.
func renderSummaryWithRemediation(fault row, trigger string, occurrences []row, publication string, remediation row) string {
	summary := renderSummary(fault, trigger, occurrences, "", publication)
	needle := "\n\nevidence, as observed at the time:"
	line := "\n\nfix: " + text(remediation, "ref")
	if detail := text(remediation, "detail"); detail != "" {
		line += "\n  detail: " + detail
	}
	if strings.Contains(summary, needle) {
		return strings.Replace(summary, needle, line+needle, 1)
	}
	return strings.Replace(summary, "\n\nRecorded by codex-session-relay.", line+"\n\nRecorded by codex-session-relay.", 1)
}

func renderSummary(fault row, trigger string, occurrences []row, clears, publication string) string {
	lines := []string{fmt.Sprintf("[%s] %s: %s", text(fault, "product"), text(fault, "fault_class"), domain(fault)), ""}
	reason, _, _ := strings.Cut(trigger, ":")
	switch reason {
	case triggerOpen:
		lines = append(lines, fmt.Sprintf("The relay recorded a %s fault in its %s path and is filing it once.", text(fault, "severity"), text(fault, "component")))
	case triggerEscalate:
		lines = append(lines, fmt.Sprintf("This fault escalated to %s.", text(fault, "severity")))
	case "recur":
		lines = append(lines, "This fault happened again after a fix was recorded, so the fix did not hold and the record is open again.")
	case "reopen":
		lines = append(lines, fmt.Sprintf("This fault happened again after it was resolved. It is the same record, reopened for cycle %d.", integer(fault, "cycle")))
	case "fix":
		lines = append(lines, "A fix was recorded. This does not resolve the fault: a reverification recorded after the fix, with no occurrence after it, does.")
	case "resolve":
		lines = append(lines, "Resolved. A fix was recorded, a reverification was recorded after it, and nothing has been observed since.")
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
