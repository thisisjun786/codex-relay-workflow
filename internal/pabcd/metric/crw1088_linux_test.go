//go:build linux

package metric

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-1088: ReadObjectiveKind with a valid explicit kind file returns it without opening the metrics ledger; only a session with
// no explicit kind reads the ledger to infer one. The opens are counted by the kernel (inotify IN_OPEN on the ledger file).
func TestCRW1088AnExplicitKindDoesNotOpenTheLedger(t *testing.T) {
	cwd := t.TempDir()
	if _, err := RecordObjectiveMetric(cwd, RecordInput{SessionID: "s", MetricName: "score", Value: 1}); err != nil {
		t.Fatal(err)
	}
	metricsMust(t, WriteObjectiveKind(cwd, "s", Satisfy))

	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(fd) })
	// The close events separate two sequential opens, which the kernel would otherwise merge into one unread event.
	if _, err := unix.InotifyAddWatch(fd, metricsPath(cwd), unix.IN_OPEN|unix.IN_CLOSE_WRITE|unix.IN_CLOSE_NOWRITE); err != nil {
		t.Fatal(err)
	}
	opens := func() int {
		n, buf := 0, make([]byte, 64*1024)
		for {
			got, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EAGAIN) {
				return n
			}
			if err != nil {
				t.Fatal(err)
			}
			for off := 0; off+unix.SizeofInotifyEvent <= got; {
				var ev unix.InotifyEvent
				if err := binary.Read(bytes.NewReader(buf[off:off+unix.SizeofInotifyEvent]), binary.NativeEndian, &ev); err != nil {
					t.Fatal(err)
				}
				if ev.Mask&unix.IN_OPEN != 0 {
					n++
				}
				off += unix.SizeofInotifyEvent + int(ev.Len)
			}
		}
	}

	if got := ReadObjectiveKind(cwd, "s"); got != Satisfy {
		t.Fatalf("explicit kind: %q, want %q", got, Satisfy)
	}
	if n := opens(); n != 0 {
		t.Errorf("an explicit kind opened the ledger %d times, want 0", n)
	}
	// Without an explicit kind the ledger is read once; this session has no row there, so the kind is satisfy.
	if got := ReadObjectiveKind(cwd, "no-kind-file"); got != Satisfy {
		t.Fatalf("a session without rows: %q, want %q", got, Satisfy)
	}
	if n := opens(); n != 1 {
		t.Errorf("an inferred kind opened the ledger %d times, want 1", n)
	}
}
