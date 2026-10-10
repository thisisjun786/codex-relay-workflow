package interview

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CRW-1133. Nothing CRW tells an agent may say that a requested token limit is to be left out of create_goal.
// The freeze handoff and the skills that carry goal rules are checked for the phrases the oracle's guard used.
var dropBudgetPhrases = regexp.MustCompile(`(?i)objective\s+only|no\s+token_budget|omit\s+token_budget|denies\s+budgeted|without\s+(a\s+)?token_budget|drop\s+(the\s+)?token_budget|remove\s+(the\s+)?token_budget`)

func TestFreezeGoalHandoffDoesNotTellTheAgentToDropTheBudget(t *testing.T) {
	if m := dropBudgetPhrases.FindString(GoalActivationDirective); m != "" {
		t.Errorf("the freeze handoff says %q", m)
	}
	for _, want := range []string{"token_budget only when the user named a token limit", "exactly that value", "never choose one yourself", "unlimited", "capability conflict"} {
		if !strings.Contains(GoalActivationDirective, want) {
			t.Errorf("the freeze handoff lacks %q", want)
		}
	}
}

func TestSkillsDoNotTellTheAgentToDropTheBudget(t *testing.T) {
	root := filepath.Join("..", "..", "..", "plugins", "crw", "skills")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if dropBudgetPhrases.MatchString(line) && strings.Contains(strings.ToLower(line), "goal") {
				t.Errorf("%s:%d tells the agent to leave the budget out: %s", path, i+1, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The durable goalplan reference carries the rule a HOTL goal is created under.
	raw, err := os.ReadFile(filepath.Join(root, "crw-loop", "references", "durable-goalplan.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"token_budget", "capability conflict"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("durable-goalplan.md lacks %q", want)
		}
	}
}
