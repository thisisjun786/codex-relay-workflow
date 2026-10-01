package supervisor

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
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
	Sender        string `json:"sender"`
	Recipient     string `json:"recipient"`
	ProjectKey    string `json:"projectKey"`
	InitiativeKey string `json:"initiativeKey"`
	Source        string `json:"source"`
}

type Refusal struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

func (r Refusal) Error() string { return r.Detail }

func (c *Channel) Resolve(ctx context.Context, relationshipID string) (Resolution, error) {
	if key, ok := strings.CutPrefix(relationshipID, "project:"); ok {
		return c.resolveNoticeProject(ctx, key)
	}
	reading, err := c.Linkage.Up(ctx, relationshipID)
	if err != nil {
		return Resolution{}, err
	}
	return resolveReading(reading, "relationship "+pyvalue.StrRepr(relationshipID))
}

func resolveReading(reading map[string]any, where string) (Resolution, error) {
	refuse := func(reason, detail string) (Resolution, error) { return Resolution{}, Refusal{reason, detail} }
	if reading["readable"] != true {
		return refuse("relation_unreadable", "the linkage could not be read for "+where+", so who the level above is is unknown; nothing is staged and the obligation stays exactly where it was")
	}
	if reading["state"] == "ambiguous" {
		return refuse("duplicate_scope_owner", "the linkage reports more than one candidate above "+where+"; this sender will not choose between them: "+pythonRepr(reading["contention"]))
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
			return refuse(reason, "the hierarchy above "+where+" is not settled: "+pythonRepr(live)+". A report waits for it to settle rather than being filed with whichever candidate happens to match")
		}
	}
	result := Resolution{Source: "linkage"}
	if levels, ok := reading["levels"].([]any); ok {
		for _, v := range levels {
			level, ok := v.(map[string]any)
			if !ok {
				continue
			}
			owner := ""
			switch value := level["owner"].(type) {
			case string:
				owner = value
			case map[string]any:
				owner, _ = value["taskId"].(string)
			}
			key, _ := level["scopeKey"].(string)
			switch level["scopeKind"] {
			case "project":
				result.ProjectKey, result.Sender = key, owner
			case "initiative":
				result.InitiativeKey, result.Recipient = key, owner
			}
		}
	}
	gaps := pythonRepr(reading["gaps"])
	if result.Sender == "" {
		return refuse("unregistered_scope", where+" has no live project owner, so there is nobody whose report this would be; gaps "+gaps)
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
