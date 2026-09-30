package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tickAnswer is the daemon's console answer and its tables.
type tickAnswer struct {
	Capture capture `json:"capture"`
	Tables  string  `json:"tables"`
}

func Test29EmptyTickTableParity(t *testing.T) {
	home := t.TempDir()
	args := []string{"--socket", home + "/socket", "daemon", "--max-ticks", "1", "--allow-isolated-scope"}
	got := invoke(t, home, args...)
	checkAnswer(t, home, "answer", tickAnswer{normalizedCapture(got), tables(t, home)})
}
func Test29LaunchPolicyPersistence(t *testing.T) {
	home := t.TempDir()
	policy := filepath.Join(home, "execution.json")
	raw := `{"roles":{"parent":{"model":"test-model","reasoningEffort":"high"},"child":{"model":"test-model","reasoningEffort":"high"}}}`
	if err := os.WriteFile(policy, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	steps := [][]string{{"service", "declare", "--execution-policy", policy}, {"service", "status"}, {"service", "declare", "--forget-execution-policy"}}
	var answers []consoleAnswer
	for _, args := range steps {
		result := invoke(t, home, args...)
		if result.Code != 0 {
			t.Fatalf("%s refused: %+v", strings.Join(args, " "), result)
		}
		answers = append(answers, consoleAnswer{normalizedCapture(result), files(t, home)})
	}
	checkAnswer(t, home, "answer", answers)
}
