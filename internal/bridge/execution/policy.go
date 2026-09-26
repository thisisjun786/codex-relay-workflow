package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// FromBytes is ExecutionPolicy.from_bytes: parse bytes already read from source, refusing
// repeated keys at every depth, and record their SHA-256 as the policy digest.
func FromBytes(raw []byte, source string) (Policy, error) {
	data, err := decode(raw)
	if err != nil {
		var policyErr *PolicyError
		if errors.As(err, &policyErr) {
			return Policy{}, err
		}
		return Policy{}, &PolicyError{fmt.Sprintf("%s is not valid JSON: %v", source, err)}
	}
	sum := sha256.Sum256(raw)
	return fromMapping(data, hex.EncodeToString(sum[:]))
}

// fromMapping is ExecutionPolicy.from_mapping over a decoded document.
func fromMapping(value any, digest string) (Policy, error) {
	data, ok := value.(*object)
	if !ok {
		return Policy{}, &PolicyError{"the execution policy must be a JSON object"}
	}
	if err := only(data, []string{"allowed", "exceptions", "roles"}, "the execution policy"); err != nil {
		return Policy{}, err
	}
	roles, err := parseRoles(data.values["roles"])
	if err != nil {
		return Policy{}, err
	}
	allowed, err := parseAllowed(data.values["allowed"], len(roles) > 0)
	if err != nil {
		return Policy{}, err
	}
	if allowed != nil {
		for _, name := range roleNames {
			role, declared := roles[name]
			if !declared || role.Expectation != Pair {
				continue
			}
			if !slices.Contains(allowed[role.Model], role.Effort) {
				return Policy{}, &PolicyError{fmt.Sprintf("role %s is declared to run %s at %s, which this file's allowed list does not approve; no such task could be created, so the two sections disagree rather than one narrowing the other", repr(name), repr(role.Model), repr(role.Effort))}
			}
		}
	}
	declared := any(&object{values: map[string]any{}})
	if data.has("exceptions") {
		declared = data.values["exceptions"]
	}
	exceptions, err := parseExceptions(declared)
	if err != nil {
		return Policy{}, err
	}
	return Policy{allowed: allowed, roles: roles, exceptions: exceptions, digest: digest}, nil
}

func parseAllowed(entries any, rolesDeclared bool) (map[string][]string, error) {
	if entries == nil && rolesDeclared {
		return nil, nil
	}
	list, ok := entries.([]any)
	if !ok || len(list) == 0 {
		return nil, &PolicyError{"allowed must be a non-empty list of {model, efforts}"}
	}
	allowed := map[string][]string{}
	for _, item := range list {
		entry, ok := item.(*object)
		if !ok {
			return nil, &PolicyError{"each allowed entry must be an object"}
		}
		if err := only(entry, []string{"efforts", "model"}, "an allowed entry"); err != nil {
			return nil, err
		}
		if len(entry.keys) != 2 {
			return nil, &PolicyError{"each allowed entry needs both model and efforts"}
		}
		model, err := identifier(entry.values["model"], "an allowed model", Maximum)
		if err != nil {
			return nil, err
		}
		if _, listed := allowed[model]; listed {
			return nil, &PolicyError{fmt.Sprintf("model %s is listed twice", repr(model))}
		}
		where := "efforts for " + repr(model)
		efforts, ok := entry.values["efforts"].([]any)
		if !ok || len(efforts) == 0 {
			return nil, &PolicyError{where + " must be a non-empty list of efforts"}
		}
		set := []string{}
		for _, effort := range efforts {
			name, err := identifier(effort, where, Maximum)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(set, name) {
				set = append(set, name)
			}
		}
		slices.Sort(set)
		allowed[model] = set
	}
	return allowed, nil
}

func parseExceptions(value any) (map[string]exception, error) {
	declared, ok := value.(*object)
	if !ok {
		return nil, &PolicyError{"exceptions must be an object"}
	}
	exceptions := map[string]exception{}
	for _, name := range declared.keys {
		if _, err := identifier(name, "an exception id", ExceptionIDMaximum); err != nil {
			return nil, err
		}
		label := "exception " + repr(name)
		entry, ok := declared.values[name].(*object)
		if !ok {
			return nil, &PolicyError{label + " must be an object"}
		}
		if err := only(entry, []string{"cwd", "model", "reason", "reasoningEffort", "role"}, label); err != nil {
			return nil, err
		}
		if absent := entry.absent("cwd", "model", "reasoningEffort"); len(absent) > 0 {
			return nil, &PolicyError{fmt.Sprintf("%s is missing %s", label, repr(absent))}
		}
		scoped := entry.values["role"]
		if scoped != nil && !isRole(scoped) {
			return nil, &PolicyError{fmt.Sprintf("%s role %s is not a role; supported are %s", label, repr(scoped), supportedRoles)}
		}
		roots, err := parseRoots(entry.values["cwd"], name, label)
		if err != nil {
			return nil, err
		}
		model, err := identifier(entry.values["model"], "the model of "+label, Maximum)
		if err != nil {
			return nil, err
		}
		effort, err := identifier(entry.values["reasoningEffort"], "the effort of "+label, Maximum)
		if err != nil {
			return nil, err
		}
		role, _ := scoped.(string)
		exceptions[name] = exception{model: model, effort: effort, role: role, cwd: roots}
	}
	return exceptions, nil
}

func parseRoots(value any, name, label string) ([]string, error) {
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, &PolicyError{label + " needs at least one cwd"}
	}
	roots := make([]string, 0, len(list))
	for _, item := range list {
		root, err := identifier(item, "a cwd of exception "+repr(name), Maximum)
		if err != nil {
			return nil, err
		}
		if strings.ContainsRune(root, 0) {
			return nil, &PolicyError{fmt.Sprintf("%s cwd %s cannot be resolved: lstat: embedded null character in path", label, repr(root))}
		}
		canonical, err := resolve(root)
		if err != nil {
			return nil, &PolicyError{fmt.Sprintf("%s cwd %s cannot be resolved: %v", label, repr(root), err)}
		}
		if !filepath.IsAbs(root) || canonical != root {
			return nil, &PolicyError{fmt.Sprintf("%s cwd %s must be canonical and absolute", label, repr(root))}
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// resolve is pathlib's non-strict Path.resolve: symlinks in the existing prefix are resolved
// and a missing remainder is kept as written.
func resolve(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) || absolute == "/" {
		return "", err
	}
	parent, err := resolve(filepath.Dir(absolute))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func only(entry *object, supported []string, where string) error {
	var unknown []string
	for _, k := range entry.keys {
		if !slices.Contains(supported, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	return &PolicyError{fmt.Sprintf("%s has unknown keys %s; supported are %s", where, repr(unknown), repr(supported))}
}

func identifier(value any, where string, maximum int) (string, error) {
	s, ok := value.(string)
	if !ok || !text(s, maximum) {
		return "", &PolicyError{where + " must be a non-empty string"}
	}
	return s, nil
}
