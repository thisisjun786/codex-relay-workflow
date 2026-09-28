package service

import (
	"strings"
	"testing"
)

func Test29ServiceBoundedRunConsole(t *testing.T) {
	for _, tail := range [][]string{{"--max-segments", "0"}, {"--deadline-monotonic", "0"}, {"--deadline", "0"}, {"--deadline", "nan"}, {"--segment-seconds", "0"}, {"--deadline", "1", "--deadline-monotonic", "1"}} {
		t.Run(strings.Join(tail, "_"), func(t *testing.T) {
			home := t.TempDir()
			var want capture
			var wf map[string]string
			for _, python := range []bool{true, false} {
				if r := invoke(t, home, python, "service", "enable"); r.Code != 0 {
					t.Fatal(r)
				}
				args := append([]string{"--socket", home + "/socket", "service", "run", "--allow-isolated-scope"}, tail...)
				r := invoke(t, home, python, args...)
				state := files(t, home)
				if python {
					want = r
					wf = state
					resetRuntime(t, home)
				} else {
					compare(t, want, r)
					for name, value := range wf {
						if value != state[name] {
							t.Fatalf("%s\nPython %s\nGo %s", name, value, state[name])
						}
					}
				}
			}
		})
	}
}
