package skill

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func pythonAttribute(value any, name string) error {
	return &evidence.PythonError{Class: "AttributeError", Detail: fmt.Sprintf("%s object has no attribute %s", pyvalue.Repr(pyvalue.TypeName(value)), pyvalue.Repr(name))}
}

func pythonNotIterable(value any, membership bool) error {
	detail := fmt.Sprintf("%s object is not iterable", pyvalue.Repr(pyvalue.TypeName(value)))
	if membership {
		detail = fmt.Sprintf("argument of type %s is not iterable", pyvalue.Repr(pyvalue.TypeName(value)))
	}
	return &evidence.PythonError{Class: "TypeError", Detail: detail}
}
