package evidence

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
	r := Get(o, "absent")
	return Equal(r, Inherited) || Equal(r, Unknown) || Equal(r, NotApplicable)
}
func Shown(value any) string {
	if IsAbsent(value) {
		o := Dict(value, false)
		if Truthy(o["detail"]) {
			return "<" + Text(o["absent"]) + ": " + Text(o["detail"]) + ">"
		}
		return "<" + Text(o["absent"]) + ">"
	}
	if value == nil {
		return ""
	}
	return Text(value)
}
