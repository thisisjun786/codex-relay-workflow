package sync

import "slices"

func wireValue(v any) any {
	switch x := v.(type) {
	case []string:
		a := make([]any, len(x))
		for i, s := range x {
			a[i] = s
		}
		return a
	case Obj:
		o := slices.Clone(x)
		for i := range o {
			o[i].Value = wireValue(o[i].Value)
		}
		return o
	case []any:
		a := slices.Clone(x)
		for i := range a {
			a[i] = wireValue(a[i])
		}
		return a
	}
	return v
}
