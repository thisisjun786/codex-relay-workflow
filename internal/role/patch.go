package role

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Opt is a patch field: absent (the zero value), null (Set with a nil Value) or a value. A value of the wrong JSON type, which a request
// body can carry, is Set with a nil Value (so it behaves as null where the oracle's check does) and keeps its raw JSON in bad: the
// oracle prints it with String() only in the check that refuses it, and that conversion can throw, so it is made there and not here.
type Opt[T any] struct {
	Set   bool
	Value *T
	bad   json.RawMessage
}

func Some[T any](v T) Opt[T] { return Opt[T]{Set: true, Value: &v} }
func Null[T any]() Opt[T]    { return Opt[T]{Set: true} }

// UnmarshalJSON marks the field present, so an absent member stays unset and a JSON null is Null. It never fails: a value that does not
// decode into T is kept as wrong-typed.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	*o = Opt[T]{Set: true}
	if string(b) == "null" {
		return nil
	}
	v := new(T)
	if err := json.Unmarshal(b, v); err != nil {
		o.bad = slices.Clone(b)
		return nil
	}
	o.Value = v
	return nil
}

// shown is how the oracle's message prints the value: String() of what the caller sent.
func (o Opt[T]) shown() (string, error) {
	switch {
	case o.bad != nil:
		return jsString(o.bad)
	case o.Value == nil:
		return "null", nil
	}
	return fmt.Sprint(*o.Value), nil
}

// pick is over when it is set, else base.
func pick[T any](base, over Opt[T]) Opt[T] {
	if over.Set {
		return over
	}
	return base
}

// FallbackPatch changes a role's fallback member by member.
type FallbackPatch struct {
	Model  Opt[string]     `json:"model"`
	Effort Opt[EffortName] `json:"effort"`
}

// UnmarshalJSON reads the members by their exact names, as JavaScript does (encoding/json would match "Model" to "model"), the last of
// a repeated name winning. A value that is not an object is an error, which Opt keeps as a wrong-typed fallback.
func (f *FallbackPatch) UnmarshalJSON(b []byte) error {
	m, ok := members(b)
	if !ok {
		return errNotObject
	}
	*f = FallbackPatch{}
	for key, dst := range map[string]json.Unmarshaler{"model": &f.Model, "effort": &f.Effort} {
		if raw, ok := m[key]; ok {
			_ = dst.UnmarshalJSON(raw) // an Opt never fails
		}
	}
	return nil
}

// RolePatch is a partial update of one role: an absent field stays, Null clears the model, effort or prompt override, and a null
// Fallback removes the fallback.
type RolePatch struct {
	Mode           Opt[RoleMode]      `json:"mode"`
	Model          Opt[string]        `json:"model"`
	Effort         Opt[EffortName]    `json:"effort"`
	PromptOverride Opt[string]        `json:"promptOverride"`
	Fallback       Opt[FallbackPatch] `json:"fallback"`
}

const effortHint = "(must be one of low/medium/high/xhigh or null)"

// Validate is validateRolePatch: the first refusal in the oracle's order, or nil.
func Validate(p RolePatch) error {
	if m := p.Mode; m.Set && (m.bad != nil || m.Value == nil || (*m.Value != ModeDefault && *m.Value != ModeModel)) {
		mode, err := m.shown()
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid mode \"%s\" (must be \"default\" or \"model\")", mode)
	}
	model, modelMode := p.Model.Value, p.Mode.Value != nil && *p.Mode.Value == ModeModel
	if modelMode && (model == nil || *model == "") {
		return errors.New("mode \"model\" requires a non-empty model id")
	}
	if e := p.Effort; e.bad != nil || (e.Value != nil && !validEffort(*e.Value)) {
		effort, err := e.shown()
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid effort \"%s\" %s", effort, effortHint)
	}
	if p.PromptOverride.bad != nil {
		return errors.New("promptOverride must be a string or null")
	}
	if p.Fallback.bad != nil {
		return errors.New("fallback must be an object or null")
	}
	f := p.Fallback.Value
	if f == nil {
		return nil
	}
	if m := f.Model; m.Set && (m.Value == nil || text.Trim(*m.Value) == "") {
		return errors.New("fallback requires a non-empty model id")
	}
	if e := f.Effort; e.bad != nil || (e.Value != nil && !validEffort(*e.Value)) {
		effort, err := e.shown()
		if err != nil {
			return err
		}
		return fmt.Errorf("invalid fallback effort \"%s\" %s", effort, effortHint)
	}
	if modelMode && model != nil && f.Model.Value != nil && *f.Model.Value == *model {
		return errors.New("fallback model must differ from the primary model")
	}
	return nil
}

// mergeFallback applies a fallback patch to the stored fallback: absent keeps it, null removes it, an object changes the members it
// names (a model it does not name is the stored one, or empty, which Validate then refuses).
func mergeFallback(current *RoleFallback, patch Opt[FallbackPatch]) *RoleFallback {
	if !patch.Set {
		return current
	} else if patch.Value == nil {
		return nil
	}
	var merged RoleFallback
	if current != nil {
		merged = *current
	}
	if m := patch.Value.Model.Value; m != nil {
		merged.Model = *m
	}
	if patch.Value.Effort.Set {
		merged.Effort = patch.Value.Effort.Value
	}
	return &merged
}

func fallbackOpt(f *RoleFallback) Opt[FallbackPatch] {
	if f == nil {
		return Null[FallbackPatch]()
	}
	return Some(FallbackPatch{Model: Some(f.Model), Effort: Opt[EffortName]{Set: true, Value: f.Effort}})
}

// patch is the role as a patch that sets every field, which is what the oracle validates after merging.
func (c RoleConfig) patch() RolePatch {
	return RolePatch{Mode: Some(c.Mode), Model: Opt[string]{Set: true, Value: c.Model}, Effort: Opt[EffortName]{Set: true, Value: c.Effort},
		PromptOverride: Opt[string]{Set: true, Value: c.PromptOverride}, Fallback: fallbackOpt(c.Fallback)}
}

// config is the role a validated all-fields patch describes.
func (p RolePatch) config() RoleConfig {
	c := RoleConfig{Mode: *p.Mode.Value, Model: p.Model.Value, Effort: p.Effort.Value, PromptOverride: p.PromptOverride.Value}
	if f := p.Fallback.Value; f != nil {
		c.Fallback = &RoleFallback{Model: *f.Model.Value, Effort: f.Effort.Value}
	}
	return c
}
