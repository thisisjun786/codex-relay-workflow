//go:build parity

package hook

import (
	"bytes"
	"io"
	"net"
	"os"
	"slices"
	"testing"
	"time"
)

// Wall-time acceptance is gated separately from the parity matrix because shared
// hosted runners cannot promise a 150ms scheduling window to a runnable process.
func Test33LatencyAcceptance(t *testing.T) {
	if os.Getenv("CRW_HOOK_LATENCY") != "1" {
		t.Skip("set CRW_HOOK_LATENCY=1 on an idle host")
	}
	home := hookHome(t, 5)
	_ = binary(t)
	samples := []float64{}
	for range 20 {
		cmd := hookCommand(t, home, `{"session_id":"s","turn_id":"t","stop_hook_active":false,"last_assistant_message":"done","transcript_path":"/not-read"}`)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		elapsed := float64(time.Since(start)) / float64(time.Millisecond)
		if err != nil || len(out) != 0 {
			t.Fatalf("%v %s", err, out)
		}
		samples = append(samples, elapsed)
	}
	rows := rowsAt(t, home)
	if len(rows) != 20 {
		t.Fatalf("latency must include 20 completed journal writes, got %d", len(rows))
	}
	for _, row := range rows {
		if row["adapterOutcome"] != "guard_unreachable" || row["held"] != false {
			t.Fatal(row)
		}
	}
	slices.Sort(samples)
	p95 := samples[18]
	if p95 >= 150 {
		t.Fatalf("no-socket p95 %.3f ms samples %v", p95, samples)
	}
	slow := hookHome(t, .4)
	done, _ := fakeControl(t, slow, func(conn net.Conn) error {
		if _, err := readFrame(conn); err != nil {
			return err
		}
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	cmd := hookCommand(t, slow, `{}`)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	elapsed := float64(time.Since(start)) / float64(time.Millisecond)
	awaitHost(t, done)
	if err != nil || out.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("%v %s %s", err, out.String(), stderr.String())
	}
	if elapsed > 500 {
		t.Fatalf("slow guard %.3f ms exceeds 400+100ms", elapsed)
	}
	t.Logf(`{"no_socket_p95_ms":%.3f,"slow_guard_exit_ms":%.3f,"budget_ms":400}`, p95, elapsed)
}
