package cli_test

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// CRW-1443: the refusals and warnings decision-raise and decision-answer write for a person name the
// session layer (부모, 자식), never the relay role id written as a bare word. The flag values a person
// types (--authority parent=ref, the parent-class decision) stay as they are. layername.Find (the
// matcher the ci ratchet shares) must find no bare role word in any of them.

func decisionTextHasNoRoleWords(t *testing.T, what, text, names string) {
	t.Helper()
	if !strings.Contains(text, names) {
		t.Errorf("%s %q does not name %q", what, text, names)
	}
	if hits := layername.Find(text); len(hits) > 0 {
		t.Errorf("%s %q names a role id where a person reads a layer (write 부모 / 자식): %v", what, text, hits)
	}
}

func TestDecisionRefusalTextNamesLayers(t *testing.T) {
	t.Run("store-scope seat, one live binding", func(t *testing.T) {
		state := authWorld(t)
		unmanaged, _ := authRaise(t, state, "P-C", "Which window does P-C take?", "delegated-management")
		value := crw737Refused(t, authAnswer(t, state, unmanaged, authSeatA, "delegated-management"), "authority_unverified")
		detail, _ := value["detail"].(string)
		decisionTextHasNoRoleWords(t, "the store-scope refusal", detail, "store-scope 부모 binding")
	})
	t.Run("store-scope seat, no live binding", func(t *testing.T) {
		state := crw737Store(t)
		decision := review818Raise(t, state, "Which window does the host update take?")
		value := crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
			"--by", "task-sup", "--via", "dots", "--authority", "user"), "authority_unverified")
		detail, _ := value["detail"].(string)
		decisionTextHasNoRoleWords(t, "the store-scope refusal with no binding", detail, "store-scope 부모 binding")
	})
	t.Run("a via that reads the 부모's report", func(t *testing.T) {
		state := crw737Store(t)
		review818SeedSupervisor(t, state, "task-sup")
		decision := review818Raise(t, state, "Which window does the host update take?")
		value := crw737Refused(t, crw737Run(t, "--state", state, "decision-answer", "--decision", decision, "--option", "now",
			"--by", "task-sup", "--via", "supervisor-readback", "--authority", "user"), "bad_invocation")
		detail, _ := value["detail"].(string)
		// The via the person typed is echoed back as the value it is; the sentence around it is judged.
		typed := `--via "supervisor-readback"`
		if !strings.Contains(detail, typed) {
			t.Fatalf("the via refusal %q does not echo %s", detail, typed)
		}
		decisionTextHasNoRoleWords(t, "the via refusal", strings.Replace(detail, typed, "--via", 1), "the 부모's report")
		if strings.Contains(detail, "자식") {
			t.Errorf("the via refusal %q names the 자식; the supervisor readback is the 부모's report", detail)
		}
	})
	t.Run("a parent-class raise without its ref", func(t *testing.T) {
		state := crw737Store(t)
		value := crw737Refused(t, crw737Run(t, "--state", state, "decision-raise", "--kind", "policy",
			"--context", "Which window does the host update take?", "--option", "now=now:at once",
			"--option", "later=later:deferred", "--origin-project", "PRJ-A", "--source", "report=1",
			"--authority", "parent"), "bad_invocation")
		detail, _ := value["detail"].(string)
		decisionTextHasNoRoleWords(t, "the parent-class refusal", detail, "--authority parent=ref names the parent (자식) authority")
	})
	t.Run("the fold warning of a parent-class raise", func(t *testing.T) {
		question := "Open the release window for the shared module?"
		state := authWorld(t)
		authRaise(t, state, "P-A", question, "user")
		_, raised := authRaise(t, state, "P-C", question, "parent="+authOwnerC)
		warning, _ := raised["foldWarning"].(map[string]any)
		detail, _ := warning["detail"].(string)
		decisionTextHasNoRoleWords(t, "the fold warning", detail, "the raiser's own authority answers (the parent-class rule)")
	})
}
