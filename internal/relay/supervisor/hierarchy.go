package supervisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Subset ported for todo 24; todo 26 owns and extends the linkage walk.
// Linkage supplies the live hierarchy, never the relationship's frozen recipient.
type Linkage interface {
	Up(context.Context, string) (map[string]any, error)
}

type Channel struct {
	Store             *store.Store
	Linkage           Linkage
	Program           string
	Socket            string
	Settings          *delivery.TaskSettings
	SettingsLoader    func(context.Context, string) (*delivery.TaskSettings, error)
	beforeTransport   func()
	beforeClaimRead   func(context.Context)
	skipPreflightRate bool
	beforeStageLock   func()
	clockISO          func() string
}

type Resolution struct {
	Sender             string `json:"sender"`
	Recipient          string `json:"recipient"`
	RecipientScopeKind string `json:"recipientScopeKind"`
	ProjectKey         string `json:"projectKey"`
	InitiativeKey      string `json:"initiativeKey"`
	Source             string `json:"source"`
}

type Refusal struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

func (r Refusal) Error() string { return r.Detail }

func (c *Channel) Resolve(ctx context.Context, relationshipID string) (Resolution, error) {
	if key, ok := strings.CutPrefix(relationshipID, prefixProject); ok {
		return c.resolveNoticeProject(ctx, key)
	}
	if key, ok := strings.CutPrefix(relationshipID, prefixInitiative); ok {
		return c.resolveInitiative(ctx, key)
	}
	if id, ok := strings.CutPrefix(relationshipID, prefixRun); ok {
		return c.resolveRun(ctx, id)
	}
	reading, err := c.hierarchyReading(ctx, relationshipID)
	if err != nil {
		return Resolution{}, err
	}
	return resolveReading(reading, "relationship "+pyvalue.StrRepr(relationshipID))
}

// scopeKindStore is the store scope's kind: the one supervisor seat no Linear level owns, which
// CRW-450 made bindable (a supervisor's own scope is an initiative; the store seat is the one
// exception). resolveReading recognises it so a recipient read from the seat is recorded as
// coming from the store instead of being given an initiative key it does not have.
const scopeKindStore = "store"

// scopeKindInitiative is the ordinary recipient's kind: a supervisor bound at the initiative level.
// resolveReading records whichever kind the level the recipient came from carries, so a resolution
// never has to be re-derived from the initiative key. Both arms below set it, and a resolution is
// only returned when Recipient is non-empty, so the kind is always set on success.
const scopeKindInitiative = "initiative"

// hierarchyReading is the hierarchy a resolution is decided from: the linkage walk, with the
// store-scope supervisor folded in as a level of its own when the walk named no initiative
// supervisor. The folded level keeps the seat's own scope kind, and that is the record of the
// substitution: the recipient came from the store scope, not from an initiative.
func (c *Channel) hierarchyReading(ctx context.Context, relationshipID string) (map[string]any, error) {
	reading, err := c.Linkage.Up(ctx, relationshipID)
	if err != nil {
		return nil, err
	}
	return c.withStoreSeat(ctx, reading, "relationship "+pyvalue.StrRepr(relationshipID))
}

// withStoreSeat folds the store seat into a readable walk that named a project owner and no level
// above it. The order is the decided one (CRW-1314 §2): the management seat whose execution edge
// stands directly above the project first, the initiative execution owner (the legacy lead) second,
// the store-scope supervisor third, and unregistered_scope when none exists. A walk that names an
// initiative supervisor or a management level above the project, or whose project owner is missing,
// is handed back untouched, so a seat that could not answer for a report the level above owns never
// decides one; a management level whose seat has no live owner is resolveLevels' refusal, not a fold.
func (c *Channel) withStoreSeat(ctx context.Context, reading map[string]any, where string) (map[string]any, error) {
	if c.Store == nil || reading["readable"] != true {
		return reading, nil
	}
	levels, _ := reading["levels"].([]any)
	project, initiative, managed := false, false, false
	previous := ""
	for _, raw := range levels {
		level, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch level["scopeKind"] {
		case "project":
			project = levelOwner(level) != ""
		case "initiative":
			initiative = levelOwner(level) != ""
		case scopeKindManagement:
			managed = managed || previous == "project"
		}
		previous, _ = level["scopeKind"].(string)
	}
	if initiative || managed || !project {
		return reading, nil
	}
	held, err := (&registry.Registry{Store: c.Store}).StoreScopeSupervisor(ctx)
	if err != nil {
		return unreadableReading(err), nil
	}
	if len(held) > 1 {
		candidates := make([]any, len(held))
		for i, one := range held {
			candidates[i] = one.Get("taskId")
		}
		return nil, Refusal{"duplicate_scope_owner", "the store scope has more than one live supervisor above " + where + "; this sender will not choose between them: " + pythonRepr(candidates)}
	}
	if len(held) == 0 {
		return reading, nil
	}
	seat := held[0]
	folded := make([]any, 0, len(levels)+1)
	folded = append(folded, levels...)
	folded = append(folded, map[string]any{"scopeKind": seat.Get("scopeKind"), "scopeKey": seat.Get("scopeKey"),
		"owner": map[string]any{"taskId": seat.Get("taskId")}, "depth": len(levels)})
	out := make(map[string]any, len(reading)+1)
	for key, value := range reading {
		out[key] = value
	}
	out["levels"] = folded
	return out, nil
}

// unreadableReading is the walk's own shape for a store that did not answer: state unreadable,
// which resolveReading names relation_unreadable. The seat read answers the same way, because a
// caller that records only named refusals (ReportHolds, staging) would otherwise abort on a raw
// database error instead of holding the report.
func unreadableReading(err error) map[string]any {
	return map[string]any{"state": "unreadable", "readable": false, "levels": []any{},
		"gaps": []any{}, "contention": []any{}, "detail": store.StoredSQLiteError(err)}
}

// levelOwner reads the task a linkage level names, whether the walk wrote the owner as a task id
// or as the object it is recorded in.
func levelOwner(level map[string]any) string {
	switch value := level["owner"].(type) {
	case string:
		return value
	case map[string]any:
		owner, _ := value["taskId"].(string)
		return owner
	}
	return ""
}

// resolveReading decides a report's sender and recipient from the walk above it. A management level
// directly above the project (CRW-1314: the management session bound the project owner) is the
// recipient of the project's reports. A management level above an initiative (CRW-1333) is not: in
// that walk the initiative stands between, so the lead stays the recipient.
func resolveReading(reading map[string]any, where string) (Resolution, error) {
	if err := settledReading(reading, where); err != nil {
		return Resolution{}, err
	}
	return resolveLevels(reading, where, Resolution{Source: "linkage"})
}

// settledReading refuses a reading that cannot decide a recipient: unreadable, ambiguous, or
// carrying a live contention (owner drift, competing parents, an instruction conflict).
func settledReading(reading map[string]any, where string) error {
	if reading["readable"] != true {
		return Refusal{"relation_unreadable", "the linkage could not be read for " + where + ", so who the level above is is unknown; nothing is staged and the obligation stays exactly where it was"}
	}
	if reading["state"] == "ambiguous" {
		return Refusal{"duplicate_scope_owner", "the linkage reports more than one candidate above " + where + "; this sender will not choose between them: " + pythonRepr(reading["contention"])}
	}
	if items, ok := reading["contention"].([]any); ok {
		live := make([]any, 0, len(items))
		drift := false
		for _, v := range items {
			item, ok := v.(map[string]any)
			if !ok || item["contention"] == nil {
				continue
			}
			live = append(live, item)
			if item["contention"] == "owner_drift" {
				drift = true
			}
		}
		if len(live) > 0 {
			reason := "link_conflict"
			if drift {
				reason = "relation_owner_drift"
			}
			return Refusal{reason, "the hierarchy above " + where + " is not settled: " + pythonRepr(live) + ". A report waits for it to settle rather than being filed with whichever candidate happens to match"}
		}
	}
	return nil
}

// resolveLevels is resolveReading after the reading was found settled.
func resolveLevels(reading map[string]any, where string, result Resolution) (Resolution, error) {
	refuse := func(reason, detail string) (Resolution, error) { return Resolution{}, Refusal{reason, detail} }
	managedBy, managed := "", false
	if levels, ok := reading["levels"].([]any); ok {
		previous := ""
		for _, v := range levels {
			level, ok := v.(map[string]any)
			if !ok {
				continue
			}
			owner := levelOwner(level)
			key, _ := level["scopeKey"].(string)
			kind, _ := level["scopeKind"].(string)
			switch kind {
			case "project":
				result.ProjectKey, result.Sender = key, owner
			case "initiative":
				result.InitiativeKey, result.Recipient, result.RecipientScopeKind = key, owner, scopeKindInitiative
			case scopeKindManagement:
				// Only the seat directly above the project; one above an initiative is the lead's.
				if previous == "project" {
					managedBy, managed = key, true
					result.Recipient, result.InitiativeKey, result.RecipientScopeKind = owner, "", scopeKindManagement
				}
			case scopeKindStore:
				// The seat is not an initiative's. A walk that named an initiative level nobody
				// holds still records that level's key, so the key is cleared with the recipient:
				// a resolution must not pair a store recipient with an initiative that supervises
				// nothing, and a later change to that non-owning initiative must not read as the
				// recipient moving.
				result.Recipient, result.InitiativeKey, result.RecipientScopeKind = owner, "", scopeKindStore
			}
			previous = kind
		}
	}
	gaps := pythonRepr(reading["gaps"])
	if result.Sender == "" {
		return refuse("unregistered_scope", where+" has no live project owner, so there is nobody whose report this would be; gaps "+gaps)
	}
	if result.Recipient == "" && managed {
		return refuse("unregistered_scope", "the management seat "+pyvalue.StrRepr(managedBy)+" above the project of "+where+
			" has no live owner, so there is nobody to tell; the obligation stays standing and waits for the seat's handover")
	}
	if result.Recipient == "" {
		return refuse("unregistered_scope", "no initiative supervises the project above "+where+", so there is nobody to report to; gaps "+gaps+". The obligation stays standing, which is the difference between having nowhere to send a report and not owing one")
	}
	return result, nil
}

// pythonRepr preserves the linkage walk's preferred map field order in visible refusals.
// Unlike evidence.Repr, it cannot sort these map fields alphabetically.
func pythonRepr(value any) string {
	switch v := value.(type) {
	case nil, string, bool:
		return pyvalue.Repr(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pythonRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		// Input map order is discarded at the Go boundary. The linkage walk builds its
		// fields in Python's insertion order; retain that order in visible diagnostics.
		preferred := []string{"contention", "gap", "scopeKind", "scopeKey", "directiveId", "fromScopeKey", "digest", "candidates", "linkId", "recorded", "live", "reason", "at"}
		keys := make([]string, 0, len(v))
		for _, key := range preferred {
			if _, ok := v[key]; ok {
				keys = append(keys, key)
			}
		}
		var extra []string
		for key := range v {
			found := false
			for _, known := range preferred {
				if key == known {
					found = true
					break
				}
			}
			if !found {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		keys = append(keys, extra...)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = pyvalue.StrRepr(key) + ": " + pythonRepr(v[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// Ensure the caller's expectation never silently overrides the current hierarchy.
func (c *Channel) ResolveRecipient(ctx context.Context, relationshipID, expected string) (Resolution, error) {
	resolved, err := c.Resolve(ctx, relationshipID)
	if err != nil {
		return Resolution{}, err
	}
	if expected != "" && expected != resolved.Recipient {
		return Resolution{}, Refusal{"recipient_not_authorized", fmt.Sprintf("the caller named %q and the linkage says project %q is supervised by %q; a disagreement about who the level above is is the finding, not something to resolve by picking one", expected, resolved.ProjectKey, resolved.Recipient)}
	}
	return resolved, nil
}
