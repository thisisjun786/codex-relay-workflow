package execution

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

const (
	Missing             = "execution_setting_missing"
	Invalid             = "execution_setting_invalid"
	NotAllowed          = "execution_not_allowed"
	ExceptionUnknown    = "execution_exception_unknown"
	ExceptionOutOfScope = "execution_exception_out_of_scope"
	RoleUnknown         = "execution_role_unknown"
	RoleMismatch        = "execution_role_mismatch"
	PolicyUnreadable    = "execution_policy_unreadable"
)
const (
	EnvPolicy = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"
	EnvDigest = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST"
)

// Maximum is execution.py MAXIMUM; ExceptionIDMaximum is EXCEPTION_ID_MAXIMUM. Both count
// characters (Python len), not bytes.
const (
	Maximum            = 500
	ExceptionIDMaximum = 128
)

// Limits is execution.py LIMITS, carried on every authorization receipt.
const Limits = "This is what the bridge authorized and transmitted. A host reporting the same values has recorded the request; it is not evidence that a provider served this model or honoured this effort. An allowlist covers requests made through this bridge only."

// Refusal is execution.py ExecutionRefused: decided before any RPC. Requested and Allowed
// entries are nil where Python carries None.
type Refusal struct {
	Code, Field string
	Requested   any
	Allowed     []any
	Detail      string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Detail }

// PolicyError is execution.py ExecutionPolicyError: the configured file cannot be used.
type PolicyError struct{ Detail string }

func (e *PolicyError) Error() string { return PolicyUnreadable + ": " + e.Detail }

type exception struct {
	model, effort, role string
	cwd                 []string
}

// Policy is what this host allows. The zero value is Python's PRESENCE_ONLY.
type Policy struct {
	allowed    map[string][]string
	roles      map[string]Role
	exceptions map[string]exception
	digest     string
}

// Input is one authorization request. Model and Effort are the raw argument values: nil is
// "not supplied", and anything else that is not a non-empty string is invalid, as in Python.
// For CWD, Exception and Role an empty string stands for Python's None.
type Input struct {
	Model, Effort        any
	CWD, Exception, Role string
}

// Optional maps a Go string whose zero value means "not supplied" to an Input value.
func Optional(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type Authorized struct {
	Model, Effort, Provenance string
	Receipt                   map[string]any
}

func (p Policy) Mode() string {
	if p.allowed == nil {
		return "presence_only"
	}
	return "allowlist"
}

func (p Policy) Summary() map[string]any {
	roles := map[string]any{}
	for name, role := range p.roles {
		roles[name] = role.receipt(name)
	}
	return map[string]any{"mode": p.Mode(), "digest": nullable(p.digest), "roles": roles}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func stated(value any, field string) (string, error) {
	if value == nil {
		return "", &Refusal{Code: Missing, Field: field, Detail: field + " was not supplied; this bridge never inherits the host's configured default"}
	}
	s, ok := value.(string)
	if !ok || !text(s, Maximum) {
		return "", &Refusal{Code: Invalid, Field: field, Requested: value, Detail: fmt.Sprintf("%s must be a non-empty string of at most %d characters", field, Maximum)}
	}
	return s, nil
}

func text(s string, maximum int) bool {
	return !isBlank(s) && utf8.RuneCountInString(s) <= maximum
}

// Authorize mirrors ExecutionPolicy.authorize: presence, then exception, then role, then allowlist.
func (p Policy) Authorize(in Input) (Authorized, error) {
	model, err := stated(in.Model, "model")
	if err != nil {
		return Authorized{}, err
	}
	effort, err := stated(in.Effort, "reasoning_effort")
	if err != nil {
		return Authorized{}, err
	}
	role, declared := p.roles[in.Role]
	provenance := "unverified"
	var overriddenBy any
	if in.Exception != "" {
		if err := p.exceptionCovers(in, model, effort); err != nil {
			return Authorized{}, err
		}
		overriddenBy, provenance = in.Exception, "exception"
	} else if in.Role != "" {
		if !declared {
			return Authorized{}, &Refusal{Code: RoleUnknown, Field: "role", Requested: in.Role, Allowed: anys(sortedKeys(p.roles)), Detail: "no such role is declared in this host's execution policy; this bridge declares no pair of its own for any role"}
		}
		if role.Expectation == Pair {
			for _, f := range [...]struct{ name, requested, authorized string }{{"model", model, role.Model}, {"reasoning_effort", effort, role.Effort}} {
				if f.requested != f.authorized {
					return Authorized{}, &Refusal{Code: RoleMismatch, Field: f.name, Requested: f.requested, Allowed: []any{f.authorized}, Detail: fmt.Sprintf("role %s runs %s %s on this host", repr(in.Role), f.name, repr(f.authorized))}
				}
			}
			provenance = "role_pair"
		}
	}
	if in.Exception == "" && p.allowed != nil {
		efforts, ok := p.allowed[model]
		if !ok {
			return Authorized{}, &Refusal{Code: NotAllowed, Field: "model", Requested: model, Allowed: anys(sortedKeys(p.allowed)), Detail: "this model is not in the execution policy configured on this host"}
		}
		if !slices.Contains(efforts, effort) {
			return Authorized{}, &Refusal{Code: NotAllowed, Field: "reasoning_effort", Requested: effort, Allowed: anys(efforts), Detail: fmt.Sprintf("%s is approved only at %s", repr(model), repr(efforts))}
		}
	}
	receipt := map[string]any{"mode": p.Mode(), "digest": nullable(p.digest), "exception": nullable(in.Exception), "role": nullable(in.Role), "model": model, "reasoningEffort": effort, "limits": Limits}
	if in.Role != "" {
		expectation := map[string]any{"role": in.Role, "expectation": nil, "model": nil, "reasoningEffort": nil}
		if declared {
			expectation = role.receipt(in.Role)
		}
		expectation["overriddenBy"] = overriddenBy
		receipt["roleExpectation"] = expectation
	}
	return Authorized{model, effort, provenance, receipt}, nil
}

func (p Policy) exceptionCovers(in Input, model, effort string) error {
	entry, ok := p.exceptions[in.Exception]
	if !ok {
		return &Refusal{Code: ExceptionUnknown, Field: "policy_exception", Requested: in.Exception, Detail: "no exception with this id is declared in this host's execution policy"}
	}
	name := repr(in.Exception)
	if entry.role != in.Role {
		return &Refusal{Code: ExceptionOutOfScope, Field: "policy_exception", Requested: nullable(in.Role), Allowed: []any{nullable(entry.role)}, Detail: fmt.Sprintf("exception %s is declared for role %s and this request cites %s", name, repr(nullable(entry.role)), repr(nullable(in.Role)))}
	}
	if in.CWD == "" {
		return &Refusal{Code: ExceptionOutOfScope, Field: "cwd", Allowed: anys(entry.cwd), Detail: fmt.Sprintf("exception %s is bound to directories, so a request citing it must also state its cwd; it covers %s", name, repr(entry.cwd))}
	}
	if !slices.Contains(entry.cwd, in.CWD) {
		return &Refusal{Code: ExceptionOutOfScope, Field: "policy_exception", Requested: in.CWD, Allowed: anys(entry.cwd), Detail: fmt.Sprintf("exception %s applies only to %s", name, repr(entry.cwd))}
	}
	for _, f := range [...]struct{ name, requested, authorized string }{{"model", model, entry.model}, {"reasoning_effort", effort, entry.effort}} {
		if f.requested != f.authorized {
			return &Refusal{Code: NotAllowed, Field: f.name, Requested: f.requested, Allowed: []any{f.authorized}, Detail: fmt.Sprintf("exception %s authorizes %s %s only", name, f.name, repr(f.authorized))}
		}
	}
	return nil
}

func anys(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
