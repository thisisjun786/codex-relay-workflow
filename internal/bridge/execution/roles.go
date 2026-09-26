package execution

import "fmt"

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

const supportedRoles = "['child', 'parent', 'supervisor']"

// Role is roles.RoleExpectation. Model and Effort are empty for a supervisor.
type Role struct{ Model, Effort, Expectation string }

func (r Role) receipt(name string) map[string]any {
	return map[string]any{"role": name, "expectation": r.Expectation, "model": nullable(r.Model), "reasoningEffort": nullable(r.Effort)}
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
	section, ok := declared.(*object)
	if !ok {
		return nil, &PolicyError{"roles must be a JSON object keyed by role id"}
	}
	for _, name := range section.keys {
		if !isRole(name) {
			return nil, &PolicyError{fmt.Sprintf("%s is not a role; supported are %s", repr(name), supportedRoles)}
		}
		entry, ok := section.values[name].(*object)
		if !ok {
			return nil, &PolicyError{fmt.Sprintf("role %s must be an object", repr(name))}
		}
		if err := only(entry, []string{"expectation", "model", "reasoningEffort"}, "role "+repr(name)); err != nil {
			return nil, err
		}
		role, err := parseRole(name, entry)
		if err != nil {
			return nil, err
		}
		parsed[name] = role
	}
	return parsed, nil
}

func parseRole(name string, entry *object) (Role, error) {
	var expectation any = Pair
	if name == Supervisor {
		expectation = Record
	}
	if entry.has("expectation") {
		expectation = entry.values["expectation"]
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
		if named := entry.present("model", "reasoningEffort"); len(named) > 0 {
			return Role{}, &PolicyError{fmt.Sprintf("role 'supervisor' cannot declare %s: its model and effort are the user's own selection, so its expectation is the recorded authorization", repr(named))}
		}
		return Role{Expectation: Record}, nil
	}
	if absent := entry.absent("model", "reasoningEffort"); len(absent) > 0 {
		return Role{}, &PolicyError{fmt.Sprintf("role %s is missing %s", repr(name), repr(absent))}
	}
	model, err := identifier(entry.values["model"], "the model of role "+repr(name), Maximum)
	if err != nil {
		return Role{}, err
	}
	effort, err := identifier(entry.values["reasoningEffort"], "the effort of role "+repr(name), Maximum)
	if err != nil {
		return Role{}, err
	}
	return Role{Model: model, Effort: effort, Expectation: Pair}, nil
}
