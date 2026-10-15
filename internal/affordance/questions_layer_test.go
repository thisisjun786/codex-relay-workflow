package affordance

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// TestQuestionsTextNamesTheParent (CRW-1478): the questions text names the session above the project session as the
// 부모 (repository session) and carries no bare role word.
func TestQuestionsTextNamesTheParent(t *testing.T) {
	q := RenderQuestionAffordance()
	if !strings.Contains(q, "the session above the project session, the 부모 (repository session)") {
		t.Errorf("the questions text does not name the 부모: %q", q)
	}
	if hits := layername.Find(q); len(hits) != 0 {
		t.Errorf("bare role words %v in %q", hits, q)
	}
}
