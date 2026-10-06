package policystore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// The five changes a caller may propose. Exactly one is applied per request, and nothing outside
// its target may move.
const (
	KindSetRolePairs    = "setRolePairs"
	KindSetAllowed      = "setAllowed"
	KindRemoveAllowed   = "removeAllowed"
	KindSetException    = "setException"
	KindRemoveException = "removeException"
)

// Change is one proposed edit: exactly one of the five kinds, with only its own fields set.
type Change struct {
	Kind    string
	Role    string
	Pairs   []Pair
	Model   string
	Efforts []string
	ID      string
	Effort  string
	CWD     []string
}

// CheckResult is what a check answers. It carries the file's current digest and whether the
// caller's expected digest still matches, plus every way the candidate was refused and the list
// of fields the change moved.
type CheckResult struct {
	Valid         bool
	Errors        []string
	CurrentDigest string
	Stale         bool
	Diff          []string
}

// Check applies one change to a copy of the document and judges the result with the bridge's own
// parser. It writes nothing: the bytes it reads are the caller's, the document it edits is in
// memory, and no file, record, backup or lock is created.
func Check(raw []byte, expectedDigest string, change Change) CheckResult {
	sum := sha256.Sum256(raw)
	current := hex.EncodeToString(sum[:])
	result := CheckResult{CurrentDigest: current, Stale: expectedDigest != current}
	document, err := decode(raw)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	if _, err := execution.FromBytes(raw, "the execution policy"); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	updated, moved, err := apply(document, change)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	encoded := []byte(pyjson.Dumps(updated, pyjson.Options{Indent: 2, Unicode: true}) + "\n")
	if _, err := execution.FromBytes(encoded, "the candidate execution policy"); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	if err := onlyTheTargetMoved(document, updated, change); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	result.Valid = true
	result.Diff = moved
	return result
}

// apply edits a copy of the document for the change.
func apply(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	switch change.Kind {
	case KindSetRolePairs:
		return applyRolePairs(document, change)
	case KindSetAllowed:
		return applyAllowed(document, change)
	case KindRemoveAllowed:
		return applyRemoveAllowed(document, change)
	case KindSetException:
		return applySetException(document, change)
	case KindRemoveException:
		return applyRemoveException(document, change)
	default:
		return document, nil, fmt.Errorf("unknown change %q", change.Kind)
	}
}

// applyRolePairs sets one role's pairs. A supervisor cannot carry a model, so its write is refused
// before anything is edited.
func applyRolePairs(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	if change.Role == execution.Supervisor {
		return document, nil, errors.New("role 'supervisor' cannot declare a model or a pair: its model and effort are the user's own selection, so its expectation is the recorded authorization")
	}
	if change.Role != execution.Parent && change.Role != execution.Child {
		return document, nil, fmt.Errorf("%q is not a role; supported are child, parent and supervisor", change.Role)
	}
	if len(change.Pairs) == 0 {
		return document, nil, errors.New("pairs must be a non-empty list of {model, reasoningEffort}")
	}
	roles, _ := document.Get("roles").(pyjson.Object)
	if roles == nil {
		roles = pyjson.Object{}
	}
	entry := pyjson.Object{}
	if len(change.Pairs) == 1 {
		entry = append(entry, pyjson.Field{Key: "model", Value: change.Pairs[0].Model}, pyjson.Field{Key: "reasoningEffort", Value: change.Pairs[0].Effort})
	} else {
		list := make([]any, 0, len(change.Pairs))
		for _, pair := range change.Pairs {
			list = append(list, pyjson.Object{{Key: "model", Value: pair.Model}, {Key: "reasoningEffort", Value: pair.Effort}})
		}
		entry = append(entry, pyjson.Field{Key: "pairs", Value: list})
	}
	updated := document.Set("roles", roles.Set(change.Role, entry))
	return updated, []string{"roles." + change.Role}, nil
}

// applyAllowed sets one model's approved efforts.
func applyAllowed(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	if change.Model == "" {
		return document, nil, errors.New("a change must name a model")
	}
	if len(change.Efforts) == 0 {
		return document, nil, errors.New("efforts must be a non-empty list")
	}
	entries, _ := document.Get("allowed").([]any)
	list := make([]any, 0, len(entries)+1)
	replaced := false
	for _, item := range entries {
		entry, ok := item.(pyjson.Object)
		if !ok {
			list = append(list, item)
			continue
		}
		if model, _ := entry.Get("model").(string); model == change.Model {
			list = append(list, allowedEntry(change.Model, change.Efforts))
			replaced = true
			continue
		}
		list = append(list, entry)
	}
	if !replaced {
		list = append(list, allowedEntry(change.Model, change.Efforts))
	}
	return document.Set("allowed", list), []string{"allowed." + change.Model}, nil
}

// allowedEntry is one allowlist row: the model and its sorted efforts.
func allowedEntry(model string, efforts []string) pyjson.Object {
	names := append([]string(nil), efforts...)
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, name := range names {
		values = append(values, name)
	}
	return pyjson.Object{{Key: "model", Value: model}, {Key: "efforts", Value: values}}
}

// applyRemoveAllowed removes one model from the allowlist. Removing a model that is not listed
// moves nothing, so it is refused rather than reported as a change.
func applyRemoveAllowed(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	entries, _ := document.Get("allowed").([]any)
	list := make([]any, 0, len(entries))
	removed := false
	for _, item := range entries {
		if entry, ok := item.(pyjson.Object); ok {
			if model, _ := entry.Get("model").(string); model == change.Model {
				removed = true
				continue
			}
		}
		list = append(list, item)
	}
	if !removed {
		return document, nil, fmt.Errorf("model %q is not in this file's allowed list", change.Model)
	}
	return document.Set("allowed", list), []string{"allowed." + change.Model}, nil
}

// applySetException adds or replaces one exception.
func applySetException(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	if change.ID == "" {
		return document, nil, errors.New("an exception needs an id")
	}
	entry := pyjson.Object{}
	if change.Role != "" {
		entry = append(entry, pyjson.Field{Key: "role", Value: change.Role})
	}
	entry = append(entry, pyjson.Field{Key: "model", Value: change.Model}, pyjson.Field{Key: "reasoningEffort", Value: change.Effort})
	roots := make([]any, 0, len(change.CWD))
	for _, root := range change.CWD {
		roots = append(roots, root)
	}
	entry = append(entry, pyjson.Field{Key: "cwd", Value: roots})
	section, _ := document.Get("exceptions").(pyjson.Object)
	if section == nil {
		section = pyjson.Object{}
	}
	return document.Set("exceptions", section.Set(change.ID, entry)), []string{"exceptions." + change.ID}, nil
}

// applyRemoveException removes one exception. Removing one that is not declared moves nothing, so
// it is refused.
func applyRemoveException(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	section, _ := document.Get("exceptions").(pyjson.Object)
	if section == nil {
		return document, nil, fmt.Errorf("exception %q is not declared in this file", change.ID)
	}
	if _, declared := section.Lookup(change.ID); !declared {
		return document, nil, fmt.Errorf("exception %q is not declared in this file", change.ID)
	}
	remaining := make(pyjson.Object, 0, len(section))
	for _, field := range section {
		if field.Key == change.ID {
			continue
		}
		remaining = append(remaining, field)
	}
	return document.Set("exceptions", remaining), []string{"exceptions." + change.ID}, nil
}

// onlyTheTargetMoved refuses a candidate that changed anything but the section the change names.
// It compares every other top-level section and refuses a dropped top-level key, so a
// normalisation the writer would introduce is caught here rather than silently accepted.
func onlyTheTargetMoved(before, after pyjson.Object, change Change) error {
	prefix := changePrefix(change)
	for _, key := range []string{"roles", "allowed", "exceptions"} {
		if key == prefix {
			continue
		}
		one := pyjson.Dumps(before.Get(key), pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
		two := pyjson.Dumps(after.Get(key), pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
		if one != two {
			return fmt.Errorf("the change moved %s, which it does not name", key)
		}
	}
	for _, field := range before {
		if field.Key == "roles" || field.Key == "allowed" || field.Key == "exceptions" {
			continue
		}
		if _, present := after.Lookup(field.Key); !present {
			return fmt.Errorf("the change dropped the top-level key %q", field.Key)
		}
	}
	return nil
}

// changePrefix is the top-level section a change names.
func changePrefix(change Change) string {
	switch change.Kind {
	case KindSetRolePairs:
		return "roles"
	case KindSetAllowed, KindRemoveAllowed:
		return "allowed"
	case KindSetException, KindRemoveException:
		return "exceptions"
	}
	return ""
}
