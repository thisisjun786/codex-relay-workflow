package service

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Test29ServiceBoundedRunConsole runs an enabled service with a spent or refused bound and checks
// its answer and files against the golden, which began as the retained Python console's.
func Test29ServiceBoundedRunConsole(t *testing.T) {
	for _, tail := range [][]string{{"--max-segments", "0"}, {"--deadline-monotonic", "0"}, {"--deadline", "0"}, {"--deadline", "nan"}, {"--segment-seconds", "0"}, {"--deadline", "1", "--deadline-monotonic", "1"}} {
		t.Run(strings.Join(tail, "_"), func(t *testing.T) {
			home := t.TempDir()
			args := append([]string{"--socket", home + "/socket", "service", "run", "--allow-isolated-scope"}, tail...)
			if r := invoke(t, home, false, "service", "enable"); r.Code != 0 {
				t.Fatal(r)
			}
			r := invoke(t, home, false, args...)
			checkAnswer(t, home, "answer", consoleAnswer{normalizedCapture(r), files(t, home, testsupport.Go)})
		})
	}
}
