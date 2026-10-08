package execution

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Role names and expectations from roles.py.
const (
	Supervisor = "supervisor"
	Parent     = "parent"
	Child      = "child"
	Pair       = "pair"
	Record     = "record"
)

// roleNames is roles.ROLES in declaration order; checks that iterate roles follow it.
var roleNames = [...]string{Supervisor, Parent, Child}

const supportedRoles = `"child", "parent", "supervisor"`

// RolePair is one model and reasoning effort a role may run on. A request matches a pair only when
// both halves are equal: an effort name belongs to the model beside it, so two models that both
// offer "xhigh" do not thereby offer the same thing.
//
// AutoCompactTokenLimit is the pair's optional model_auto_compact_token_limit. It travels in the
// thread/start and thread/resume config, and the host never reports it back, so it is a property of
// the pair's execution and not part of what a request is judged against: the pair's identity stays
// model and effort, and a nil limit leaves the host's own auto-compaction threshold in place.
type RolePair struct {
	Model, Effort         string
	AutoCompactTokenLimit *int64
}

// Role is roles.RoleExpectation. Pairs is every pair the role may run on, in the order the policy
// file declares them (the legacy model and reasoningEffort keys declare one); it is empty for a
// supervisor, whose model is the user's own selection.
type Role struct {
	Pairs       []RolePair
	Expectation string
	// MCP is the role's MCP profiles; nil when the role declares none.
	MCP *MCPProfiles
}

// pairFor is the declared pair whose model and effort a request states. The limit is not part of the
// identity: two entries that name the same model and effort are the same pair, and the declaration's
// limit travels with whichever entry matched.
func (r Role) pairFor(model, effort string) (RolePair, bool) {
	for _, pair := range r.Pairs {
		if pair.Model == model && pair.Effort == effort {
			return pair, true
		}
	}
	return RolePair{}, false
}

// Allows is whether a request stating model and effort is one of the role's pairs.
func (r Role) Allows(model, effort string) bool {
	_, ok := r.pairFor(model, effort)
	return ok
}

// pairValues is the pairs as the policy description and a refusal carry them.
func (r Role) pairValues() []any {
	values := make([]any, len(r.Pairs))
	for i, pair := range r.Pairs {
		values[i] = map[string]any{"model": pair.Model, "reasoningEffort": pair.Effort}
	}
	return values
}

// receipt describes the role. A role with one pair, and a supervisor, are described exactly as
// they always were, so a policy file written for one pair per role reads the same. A role with
// several pairs lists them under "pairs" and leaves model and reasoningEffort for the pair a
// request matched (nil in a description, and when an exception answered instead of the role).
func (r Role) receipt(name string, matched *RolePair) map[string]any {
	if len(r.Pairs) > 1 {
		entry := map[string]any{"role": name, "expectation": r.Expectation, "model": nil, "reasoningEffort": nil, "pairs": r.pairValues()}
		if matched != nil {
			entry["model"], entry["reasoningEffort"] = matched.Model, matched.Effort
		}
		return entry
	}
	var single RolePair
	if len(r.Pairs) == 1 {
		single = r.Pairs[0]
	}
	return map[string]any{"role": name, "expectation": r.Expectation, "model": nullable(single.Model), "reasoningEffort": nullable(single.Effort)}
}

// mismatch is the refusal for a request that is none of the role's pairs. A role with one pair
// reports the half that differs, model first, as it always has. A role with several names no
// single half, because the request fails as a whole, so it lists every pair.
func (r Role) mismatch(name, model, effort string) *Refusal {
	if len(r.Pairs) == 1 {
		authorized := r.Pairs[0]
		for _, f := range [...]struct{ name, requested, authorized string }{{"model", model, authorized.Model}, {"reasoning_effort", effort, authorized.Effort}} {
			if f.requested != f.authorized {
				return &Refusal{Code: RoleMismatch, Field: f.name, Requested: f.requested, Allowed: []any{f.authorized}, Detail: fmt.Sprintf("role %s runs %s %s on this host", repr(name), f.name, repr(f.authorized))}
			}
		}
	}
	declared := make([]string, len(r.Pairs))
	for i, pair := range r.Pairs {
		declared[i] = repr(pair.Model) + " at " + repr(pair.Effort)
	}
	return &Refusal{Code: RoleMismatch, Field: "pair", Requested: map[string]any{"model": model, "reasoningEffort": effort}, Allowed: r.pairValues(),
		Detail: fmt.Sprintf("role %s runs one of these pairs on this host: %s; this request states %s at %s", repr(name), strings.Join(declared, ", "), repr(model), repr(effort))}
}

func isRole(value any) bool {
	for _, name := range roleNames {
		if value == any(name) {
			return true
		}
	}
	return false
}

// parseRoles mirrors roles.parse: absent means no role is enforced on this host.
func parseRoles(declared any) (map[string]Role, error) {
	parsed := map[string]Role{}
	if declared == nil {
		return parsed, nil
	}
	section, ok := declared.(object)
	if !ok {
		return nil, &PolicyError{"roles must be a JSON object keyed by role id"}
	}
	for _, name := range keys(section) {
		if !isRole(name) {
			return nil, &PolicyError{fmt.Sprintf("%s is not a role; supported are %s", repr(name), supportedRoles)}
		}
		entry, ok := section.Get(name).(object)
		if !ok {
			return nil, &PolicyError{fmt.Sprintf("role %s must be an object", repr(name))}
		}
		if err := only(entry, []string{"autoCompactTokenLimit", "expectation", "mcp", "model", "pairs", "reasoningEffort"}, "role "+repr(name)); err != nil {
			return nil, err
		}
		role, err := parseRole(name, entry)
		if err != nil {
			return nil, err
		}
		if has(entry, "mcp") {
			if name == Supervisor {
				return nil, &PolicyError{"role 'supervisor' cannot declare \"mcp\": a supervisor's thread is the user's own, so no profile switches anything off in it"}
			}
			if role.MCP, err = parseMCP(name, entry.Get("mcp")); err != nil {
				return nil, err
			}
		}
		parsed[name] = role
	}
	return parsed, nil
}

func parseRole(name string, entry object) (Role, error) {
	var expectation any = Pair
	if name == Supervisor {
		expectation = Record
	}
	if has(entry, "expectation") {
		expectation = entry.Get("expectation")
	}
	if expectation != any(Pair) && expectation != any(Record) {
		return Role{}, &PolicyError{fmt.Sprintf("role %s expectation must be 'pair' or 'record'", repr(name))}
	}
	if name == Supervisor && expectation != any(Record) {
		return Role{}, &PolicyError{"role 'supervisor' expectation must be 'record': its model and effort are the user's own selection, not something the policy pins"}
	}
	if name != Supervisor && expectation != any(Pair) {
		return Role{}, &PolicyError{fmt.Sprintf("role %s expectation must be 'pair': only 'supervisor' defers to the recorded authorization, and letting another role do so would exempt it from the role check", repr(name))}
	}
	if name == Supervisor {
		if named := present(entry, "autoCompactTokenLimit", "model", "pairs", "reasoningEffort"); len(named) > 0 {
			return Role{}, &PolicyError{fmt.Sprintf("role 'supervisor' cannot declare %s: its model and effort are the user's own selection, so its expectation is the recorded authorization and it carries no pair for a limit to belong to", repr(named))}
		}
		return Role{Expectation: Record}, nil
	}
	if has(entry, "pairs") {
		if named := present(entry, "autoCompactTokenLimit", "model", "reasoningEffort"); len(named) > 0 {
			return Role{}, &PolicyError{fmt.Sprintf("role %s declares both pairs and %s; state one pair as model, reasoningEffort and autoCompactTokenLimit, or several as pairs", repr(name), repr(named))}
		}
		pairs, err := parsePairs(name, entry.Get("pairs"))
		if err != nil {
			return Role{}, err
		}
		return Role{Pairs: pairs, Expectation: Pair}, nil
	}
	if absent := absent(entry, "model", "reasoningEffort"); len(absent) > 0 {
		return Role{}, &PolicyError{fmt.Sprintf("role %s is missing %s", repr(name), repr(absent))}
	}
	model, err := identifier(entry.Get("model"), "the model of role "+repr(name), Maximum)
	if err != nil {
		return Role{}, err
	}
	effort, err := identifier(entry.Get("reasoningEffort"), "the effort of role "+repr(name), Maximum)
	if err != nil {
		return Role{}, err
	}
	var limit *int64
	if has(entry, "autoCompactTokenLimit") {
		if limit, err = autoCompactTokenLimit(entry.Get("autoCompactTokenLimit"), "role "+repr(name)); err != nil {
			return Role{}, err
		}
	}
	return Role{Pairs: []RolePair{{Model: model, Effort: effort, AutoCompactTokenLimit: limit}}, Expectation: Pair}, nil
}

// parsePairs reads the pairs list of a role: a non-empty list of {model, reasoningEffort} objects,
// none repeated, each optionally carrying an autoCompactTokenLimit.
func parsePairs(name string, declared any) ([]RolePair, error) {
	list, ok := declared.([]any)
	if !ok || len(list) == 0 {
		return nil, &PolicyError{fmt.Sprintf("pairs of role %s must be a non-empty list of {model, reasoningEffort}", repr(name))}
	}
	where := "a pair of role " + repr(name)
	pairs := make([]RolePair, 0, len(list))
	for _, item := range list {
		entry, ok := item.(object)
		if !ok {
			return nil, &PolicyError{where + " must be an object"}
		}
		if err := only(entry, []string{"autoCompactTokenLimit", "model", "reasoningEffort"}, where); err != nil {
			return nil, err
		}
		if absent := absent(entry, "model", "reasoningEffort"); len(absent) > 0 {
			return nil, &PolicyError{fmt.Sprintf("%s is missing %s", where, repr(absent))}
		}
		model, err := identifier(entry.Get("model"), "the model of "+where, Maximum)
		if err != nil {
			return nil, err
		}
		effort, err := identifier(entry.Get("reasoningEffort"), "the effort of "+where, Maximum)
		if err != nil {
			return nil, err
		}
		var limit *int64
		if has(entry, "autoCompactTokenLimit") {
			if limit, err = autoCompactTokenLimit(entry.Get("autoCompactTokenLimit"), where); err != nil {
				return nil, err
			}
		}
		if _, repeated := (Role{Pairs: pairs}).pairFor(model, effort); repeated {
			return nil, &PolicyError{fmt.Sprintf("role %s lists the pair %s at %s twice", repr(name), repr(model), repr(effort))}
		}
		pairs = append(pairs, RolePair{Model: model, Effort: effort, AutoCompactTokenLimit: limit})
	}
	return pairs, nil
}

// autoCompactTokenLimit reads a pair's optional model_auto_compact_token_limit. It must be a
// positive integer: a string, a boolean, a fraction or an exponent, zero and a negative value are
// all refused while the policy is read, through the same PolicyError every other unusable policy
// value is refused with. The field is new, so nothing it refuses used to load.
func autoCompactTokenLimit(value any, where string) (*int64, error) {
	spelled, ok := value.(json.Number)
	if !ok || strings.ContainsAny(string(spelled), ".eE") {
		return nil, &PolicyError{where + " must state autoCompactTokenLimit as a positive integer"}
	}
	parsed, err := strconv.ParseInt(string(spelled), 10, 64)
	if err != nil || parsed <= 0 {
		return nil, &PolicyError{where + " must state autoCompactTokenLimit as a positive integer"}
	}
	return &parsed, nil
}
