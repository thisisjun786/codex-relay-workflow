package cli

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestControlRefusesAnUnknownLockHolder: a run whose lock is held but names no process (the record is empty or unreadable) is neither
// paused nor cancelled by a signal; a signal to pid 0 would go to the process group of the controller (CRW-1205 d1).
func TestControlRefusesAnUnknownLockHolder(t *testing.T) {
	for name, record := range map[string]string{"empty": "", "garbage": "not json\n", "pid zero": `{"pid":0}` + "\n", "pid one": `{"pid":1}` + "\n", "negative": `{"pid":-4}` + "\n"} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.newRun("run-anon", "thread-owner").close()
			td := w.deps()
			var sent []int
			td.Signal = func(pid int, sig syscall.Signal) error { sent = append(sent, pid); return nil }
			if err := os.MkdirAll(RunDir(w.state, "run-anon"), 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(lockPath(w.state, "run-anon"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(record); err != nil {
				t.Fatal(err)
			}
			for _, verb := range []string{"pause", "cancel"} {
				code, out, errOut := td.do(verb, "run-anon", "--state", w.state, "--wait", "0")
				if code == 0 || len(sent) != 0 || !strings.Contains(errOut, "holder") {
					t.Fatalf("%s: exit %d, signals to %v, out %q, err %q", verb, code, sent, out, errOut)
				}
			}
		})
	}
}
