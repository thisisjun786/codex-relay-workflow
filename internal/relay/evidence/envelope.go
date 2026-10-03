package evidence

import "github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"

// The part of envelope.py the supervisor channel reads: how an envelope field states that it is
// absent, and how such a field is shown in a rendered message.

const (
	Inherited     = "inherited"
	Unknown       = "unknown"
	NotApplicable = "not_applicable"
)

func IsAbsent(value any) bool {
	o, ok := Object(value)
	if !ok {
		return false
	}
	r := o.Get("absent")
	return pyvalue.ItemEqual(r, Inherited) || pyvalue.ItemEqual(r, Unknown) || pyvalue.ItemEqual(r, NotApplicable)
}
func Shown(value any) string {
	if IsAbsent(value) {
		o := Dict(value, false)
		if pyvalue.Truthy(o["detail"]) {
			return "<" + pyvalue.Str(o["absent"]) + ": " + pyvalue.Str(o["detail"]) + ">"
		}
		return "<" + pyvalue.Str(o["absent"]) + ">"
	}
	if value == nil {
		return ""
	}
	return pyvalue.Str(value)
}
