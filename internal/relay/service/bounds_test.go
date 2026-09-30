package service

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Test29ServiceBoundedRunConsole runs an enabled service with a spent or refused bound in both
// runtimes over the same home; Python's answer and files are recorded (pythonHalf).
func Test29ServiceBoundedRunConsole(t *testing.T) {
	for _, tail := range [][]string{{"--max-segments", "0"}, {"--deadline-monotonic", "0"}, {"--deadline", "0"}, {"--deadline", "nan"}, {"--segment-seconds", "0"}, {"--deadline", "1", "--deadline-monotonic", "1"}} {
		t.Run(strings.Join(tail, "_"), func(t *testing.T) {
			home := t.TempDir()
			args := append([]string{"--socket", home + "/socket", "service", "run", "--allow-isolated-scope"}, tail...)
			var want consoleAnswer
			pythonHalf(t, home, "python", true, &want, func() (any, error) {
				if r := invoke(t, home, true, "service", "enable"); r.Code != 0 {
					t.Fatal(r)
				}
				r := invoke(t, home, true, args...)
				return consoleAnswer{pythonCapture(r), files(t, home, testsupport.Python)}, nil
			})
			if r := invoke(t, home, false, "service", "enable"); r.Code != 0 {
				t.Fatal(r)
			}
			r := invoke(t, home, false, args...)
			state := files(t, home, testsupport.Go)
			compare(t, want.Capture, r)
			for name, value := range want.Files {
				if value != state[name] {
					t.Fatalf("%s\nPython %s\nGo %s", name, value, state[name])
				}
			}
		})
	}
}
