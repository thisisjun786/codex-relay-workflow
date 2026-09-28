package skill

import (
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

func pythonAttribute(value any, name string) error {
	return &evidence.PythonError{Class: "AttributeError", Detail: fmt.Sprintf("%s object has no attribute %s", evidence.Repr(evidence.TypeName(value)), evidence.Repr(name))}
}

func pythonNotIterable(value any, membership bool) error {
	detail := fmt.Sprintf("%s object is not iterable", evidence.Repr(evidence.TypeName(value)))
	if membership {
		detail = fmt.Sprintf("argument of type %s is not iterable", evidence.Repr(evidence.TypeName(value)))
	}
	return &evidence.PythonError{Class: "TypeError", Detail: detail}
}
