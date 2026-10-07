package policystore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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

// Change is one proposed edit: exactly one of the five kinds, with only its own fields set. A
// request carrying a field of another kind is refused rather than partly applied. The JSON keys are
// the lower-case spellings the API documents, not Go field names.
type Change struct {
	Kind    string   `json:"kind"`
	Role    string   `json:"role,omitempty"`
	Pairs   []Pair   `json:"pairs,omitempty"`
	Model   string   `json:"model,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
	ID      string   `json:"id,omitempty"`
	Effort  string   `json:"effort,omitempty"`
	CWD     []string `json:"cwd,omitempty"`
	// conflict is why the request's two effort spellings could not be reconciled, or "" when they
	// could. It is unexported so it is not part of the wire shape: Check turns it into a refused
	// change, which keeps a disagreement a check answer rather than a bad request.
	conflict string
}

// changeWire is a change as JSON carries it. It exists so Change can decode the two effort
// spellings through the same resolver Pair uses without recursing into its own UnmarshalJSON. The
// spellings are raw so an absent key is distinguishable from one supplied empty.
type changeWire struct {
	Kind            string          `json:"kind"`
	Role            string          `json:"role,omitempty"`
	Pairs           []Pair          `json:"pairs,omitempty"`
	Model           string          `json:"model,omitempty"`
	Efforts         []string        `json:"efforts,omitempty"`
	ID              string          `json:"id,omitempty"`
	ReasoningEffort json.RawMessage `json:"reasoningEffort"`
	Effort          json.RawMessage `json:"effort"`
	CWD             []string        `json:"cwd,omitempty"`
}

// UnmarshalJSON reads a change from the wire, resolving its effort through the same alias resolver
// Pair uses. The contract names the field effort and the policy document names it reasoningEffort;
// a request that uses both with different values is recorded as a conflict and refused by Check.
func (c *Change) UnmarshalJSON(raw []byte) error {
	var wire changeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	reasoningEffort, reasoningPresent, err := effortField(wire.ReasoningEffort)
	if err != nil {
		return err
	}
	effort, effortPresent, err := effortField(wire.Effort)
	if err != nil {
		return err
	}
	resolved, conflict := effortAlias(reasoningEffort, reasoningPresent, effort, effortPresent)
	*c = Change{Kind: wire.Kind, Role: wire.Role, Pairs: wire.Pairs, Model: wire.Model,
		Efforts: wire.Efforts, ID: wire.ID, Effort: resolved, CWD: wire.CWD, conflict: conflict}
	return nil
}

// CheckResult is what a check answers. It carries the file's current digest and whether the
// caller's expected digest still matches, plus every way the candidate was refused and the list of
// fields the change moved.
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
	if err := singleKind(change); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	// A request whose two effort spellings disagree is refused here, before the file is read: the
	// disagreement is about the request, not about the document it would be applied to.
	if change.conflict != "" {
		result.Errors = append(result.Errors, change.conflict)
		return result
	}
	for _, pair := range change.Pairs {
		if pair.conflict != "" {
			result.Errors = append(result.Errors, pair.conflict)
			return result
		}
	}
	document, err := decode(raw)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	original, err := execution.FromBytes(raw, "the execution policy")
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	// The snapshot is taken before the edit, because apply writes through the decoded document
	// (pyjson.Object.Set assigns in place) and would otherwise leave nothing to compare against.
	before := snapshotSections(document)
	updated, moved, err := apply(document, change)
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	encoded := []byte(encode(updated))
	candidate, err := execution.FromBytes(encoded, "the candidate execution policy")
	if err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	// A change may not move the policy's mode. An absent allowed key is presence_only, which allows
	// every model the bridge's own parser accepts, so a change that turns an allowlist into a
	// presence_only document widens what this host allows without saying so. The comparison is on
	// the parser's own reading of both documents, not on a projection of them.
	if original.Mode() != candidate.Mode() {
		result.Errors = append(result.Errors, "the change turns this policy from "+original.Mode()+" into "+candidate.Mode()+", which changes what this host allows; a change may not move the policy's mode")
		return result
	}
	if err := onlyTheTargetMoved(before, updated, change); err != nil {
		result.Errors = append(result.Errors, err.Error())
		return result
	}
	result.Valid = true
	result.Diff = moved
	return result
}

// singleKind refuses a request that populated fields belonging to a kind other than the one it
// names. A change is one edit, so an unread field would otherwise be approved without being judged.
func singleKind(change Change) error {
	allowed := map[string]bool{}
	switch change.Kind {
	case KindSetRolePairs:
		allowed["Role"], allowed["Pairs"] = true, true
	case KindSetAllowed:
		allowed["Model"], allowed["Efforts"] = true, true
	case KindRemoveAllowed:
		allowed["Model"] = true
	case KindSetException:
		allowed["ID"], allowed["Role"], allowed["Model"], allowed["Effort"], allowed["CWD"] = true, true, true, true, true
	case KindRemoveException:
		allowed["ID"] = true
	default:
		return fmt.Errorf("unknown change %q", change.Kind)
	}
	set := map[string]bool{}
	if change.Role != "" {
		set["Role"] = true
	}
	if len(change.Pairs) > 0 {
		set["Pairs"] = true
	}
	if change.Model != "" {
		set["Model"] = true
	}
	if len(change.Efforts) > 0 {
		set["Efforts"] = true
	}
	if change.ID != "" {
		set["ID"] = true
	}
	if change.Effort != "" {
		set["Effort"] = true
	}
	if len(change.CWD) > 0 {
		set["CWD"] = true
	}
	var stray []string
	for field := range set {
		if !allowed[field] {
			stray = append(stray, field)
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		return fmt.Errorf("change %q does not take %v; a change is one edit", change.Kind, stray)
	}
	return nil
}

// encode renders a candidate document the way the file is written: two-space indentation and one
// trailing newline. It is the only place a candidate document's bytes are produced.
func encode(document pyjson.Object) string {
	return pyjson.Dumps(document, pyjson.Options{Indent: 2, Unicode: true}) + "\n"
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

// applyRolePairs sets one role's pairs. Every other key the role declares (its mcp profiles above
// all) is carried across, so a pair change cannot silently drop them.
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
	existing, _ := roles.Get(change.Role).(pyjson.Object)
	entry := pyjson.Object{}
	for _, field := range existing {
		if field.Key == "model" || field.Key == "pairs" || field.Key == "reasoningEffort" || field.Key == "autoCompactTokenLimit" {
			continue
		}
		entry = append(entry, field)
	}
	// The limit belongs to a pair, and this change rebuilds the whole list. Every limit the role
	// declares today is read here, so a pair the change keeps keeps its limit and a legacy single
	// declaration's limit follows its one pair into the list rather than being left beside it,
	// where the parser refuses it. A change may still state a limit of its own for any pair.
	declared := declaredLimits(existing)
	if len(change.Pairs) == 1 {
		entry = append(entry, pyjson.Field{Key: "model", Value: change.Pairs[0].Model}, pyjson.Field{Key: "reasoningEffort", Value: change.Pairs[0].Effort})
		if limit, ok := limitFor(change.Pairs[0], declared); ok {
			entry = append(entry, pyjson.Field{Key: "autoCompactTokenLimit", Value: limit})
		}
	} else {
		list := make([]any, 0, len(change.Pairs))
		for _, pair := range change.Pairs {
			built := pyjson.Object{{Key: "model", Value: pair.Model}, {Key: "reasoningEffort", Value: pair.Effort}}
			if limit, ok := limitFor(pair, declared); ok {
				built = append(built, pyjson.Field{Key: "autoCompactTokenLimit", Value: limit})
			}
			list = append(list, built)
		}
		entry = append(entry, pyjson.Field{Key: "pairs", Value: list})
	}
	return document.Set("roles", roles.Set(change.Role, entry)), []string{"roles." + change.Role}, nil
}

// declaredLimits is every autoCompactTokenLimit a role entry declares today: the limit of each pair
// it lists, and the limit beside a legacy single declaration, which belongs to that one pair. The
// key is the pair's own model and effort in two nested maps, so no delimiter inside either name can
// make two different pairs look like one.
func declaredLimits(entry pyjson.Object) map[string]map[string]any {
	limits := map[string]map[string]any{}
	record := func(pair pyjson.Object) {
		limit, present := pair.Lookup("autoCompactTokenLimit")
		if !present {
			return
		}
		model, _ := pair.Lookup("model")
		effort, _ := pair.Lookup("reasoningEffort")
		byEffort := limits[pyvalue.Str(model)]
		if byEffort == nil {
			byEffort = map[string]any{}
			limits[pyvalue.Str(model)] = byEffort
		}
		byEffort[pyvalue.Str(effort)] = limit
	}
	if listed, ok := entry.Get("pairs").([]any); ok {
		for _, item := range listed {
			if pair, ok := item.(pyjson.Object); ok {
				record(pair)
			}
		}
		return limits
	}
	record(entry)
	return limits
}

// limitFor is the limit a rebuilt pair carries: the one the change states for it, or the one the
// document already declared for that same model and effort. The second value is false when neither
// has one, and then no key is written at all, so a pair without a limit is written the way it
// always was rather than gaining an explicit null.
func limitFor(pair Pair, declared map[string]map[string]any) (any, bool) {
	if pair.AutoCompactTokenLimit != 0 {
		return pair.AutoCompactTokenLimit, true
	}
	byEffort, ok := declared[pair.Model]
	if !ok {
		return nil, false
	}
	limit, ok := byEffort[pair.Effort]
	return limit, ok
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
// moves nothing, so it is refused rather than reported as a change. Removing the last one leaves an
// empty list: the key is kept so the bridge's own parser judges the candidate as it stands, and an
// empty list is what that parser refuses. Dropping the key would instead turn the document into a
// presence_only one, which widens what this host allows.
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

// applySetException adds or replaces one exception. The reason an existing entry records is kept,
// because a check that silently dropped it would hide the loss.
func applySetException(document pyjson.Object, change Change) (pyjson.Object, []string, error) {
	if change.ID == "" {
		return document, nil, errors.New("an exception needs an id")
	}
	section, _ := document.Get("exceptions").(pyjson.Object)
	if section == nil {
		section = pyjson.Object{}
	}
	existing, _ := section.Get(change.ID).(pyjson.Object)
	entry := pyjson.Object{}
	if existing != nil {
		if reason, declared := existing.Lookup("reason"); declared {
			entry = append(entry, pyjson.Field{Key: "reason", Value: reason})
		}
	}
	// The role scope is carried across, not dropped: an exception whose scope vanished would be
	// checked against any cited role, which widens the authorization the operator recorded.
	role := change.Role
	if role == "" && existing != nil {
		role, _ = existing.Get("role").(string)
	}
	if role != "" {
		entry = append(entry, pyjson.Field{Key: "role", Value: role})
	}
	entry = append(entry, pyjson.Field{Key: "model", Value: change.Model}, pyjson.Field{Key: "reasoningEffort", Value: change.Effort})
	roots := make([]any, 0, len(change.CWD))
	for _, root := range change.CWD {
		roots = append(roots, root)
	}
	entry = append(entry, pyjson.Field{Key: "cwd", Value: roots})
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

// sections is the document before the edit: each of the three declared sections as canonical JSON,
// and every other top-level key. It is taken before apply, because apply assigns through the
// decoded document in place.
type sections struct {
	values map[string]string
	others []string
}

// snapshotSections records what a comparison needs: the three sections the changes may name, and
// the other top-level keys, which no change may drop.
func snapshotSections(document pyjson.Object) sections {
	snapshot := sections{values: map[string]string{}}
	for _, key := range []string{"roles", "allowed", "exceptions"} {
		snapshot.values[key] = canonical(document.Get(key))
	}
	for _, field := range document {
		switch field.Key {
		case "roles", "allowed", "exceptions":
			continue
		}
		snapshot.others = append(snapshot.others, field.Key)
	}
	return snapshot
}

// canonical is a value as the comparisons spell it: compact, keys sorted, so two spellings of the
// same document compare equal and any real difference does not.
func canonical(value any) string {
	return pyjson.Dumps(value, pyjson.Options{Compact: true, SortKeys: true, Unicode: true})
}

// onlyTheTargetMoved refuses a candidate that changed anything but the section the change names.
// Every other section is compared with its snapshot and a dropped top-level key is refused, so a
// normalisation or an unrelated edit the writer introduced is caught here rather than silently
// accepted.
func onlyTheTargetMoved(before sections, after pyjson.Object, change Change) error {
	prefix := changePrefix(change)
	for _, key := range []string{"roles", "allowed", "exceptions"} {
		if key == prefix {
			continue
		}
		if before.values[key] != canonical(after.Get(key)) {
			return fmt.Errorf("the change moved %s, which it does not name", key)
		}
	}
	for _, key := range before.others {
		if _, present := after.Lookup(key); !present {
			return fmt.Errorf("the change dropped the top-level key %q", key)
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
