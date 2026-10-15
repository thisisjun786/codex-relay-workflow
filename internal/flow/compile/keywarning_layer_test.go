package compile

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestKeyBindingWarningNamesTheChild (CRW-1478): the project key warnings name the 자식 (the relay parent, whose live
// binding holds the project key) and carry no bare role word; the rule ids are unchanged.
func TestKeyBindingWarningNamesTheChild(t *testing.T) {
	for _, tc := range []struct {
		name, rule, want string
		b                Binding
	}{
		{"no binding", RuleProjectKeyBound, "holds no live 자식 binding", Binding{Thread: "t"}},
		{"another key", RuleProjectKeyBound, "is the live 자식 of project x:y:1", Binding{Thread: "t", Scopes: []string{"x:y:1"}}},
		{"unreadable", RuleProjectKeyUnchecked, "the live 자식 bindings of thread t cannot be read", Binding{Thread: "t", Err: errors.New("no store")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := KeyWarning("o:r:3", "o/r#3", tc.b, nil)
			if !ok || w.Rule != tc.rule {
				t.Fatalf("warning %+v (%v), want rule %s", w, ok, tc.rule)
			}
			if !strings.Contains(w.Detail, tc.want) {
				t.Errorf("the warning does not name %q: %s", tc.want, w.Detail)
			}
			if tc.rule == RuleProjectKeyBound && !strings.Contains(w.Detail, "the scope_key of the orchestrator's 자식 binding") {
				t.Errorf("the warning does not name the 자식 binding's scope_key: %s", w.Detail)
			}
			if hits := layername.Find(w.Detail); len(hits) != 0 {
				t.Errorf("bare role words %v in %q", hits, w.Detail)
			}
		})
	}
}
