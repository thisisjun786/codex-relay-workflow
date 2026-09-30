package hook

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RouteGuard reads the owner's answer within the fence's JSON nesting budget: socket_guard's
// json.loads takes 9998 nested containers, so an answer that deep is the owner's, and raises
// RecursionError from 9999, which leaves socket_guard for cli.main to report as a host error
// (the CLI's rows in guard_route_test.go compare those with the live fence). A 9998-deep answer
// is not compared there: both runtimes would print some 200 MB of it.
func TestRouteGuard_reads_an_answer_within_the_fences_nesting_budget(t *testing.T) {
	for _, row := range []struct {
		name, reply, err string
	}{
		{"9998 nested arrays", strings.Repeat("[", 9998) + strings.Repeat("]", 9998) + "\n", ""},
		{"9998 nested objects", strings.Repeat("{\"a\": ", 9997) + "{}" + strings.Repeat("}", 9997) + "\n", ""},
		{"9999 nested arrays", strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON array from a unicode string"},
		{"9999 nested objects", strings.Repeat("{\"a\": ", 9998) + "{}" + strings.Repeat("}", 9998) + "\n", "RecursionError: maximum recursion depth exceeded while decoding a JSON object from a unicode string"},
	} {
		t.Run(row.name, func(t *testing.T) {
			// The owner's directory, which only this user may write (_trusted_guard_peer).
			state := t.TempDir()
			if err := os.Chmod(state, 0o700); err != nil {
				t.Fatal(err)
			}
			address, release, err := ControlAddress(filepath.Join(state, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", address)
			release()
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadString('\n')
				_, _ = io.WriteString(conn, row.reply)
			}()
			routed, err := RouteGuard(context.Background(), state, Object{}, GuardOptions{Root: t.TempDir(), Mode: Observe})
			switch {
			case row.err != "":
				if err == nil || err.Error() != row.err {
					t.Fatalf("got %v; want the error %q", err, row.err)
				}
			case err != nil:
				t.Fatal(err)
			case routed == nil || !routed.Readable || routed.Code != 0 || routed.Answer == nil:
				t.Fatal("an answer 9998 deep is the owner's, read as the fence reads it")
			}
		})
	}
}
