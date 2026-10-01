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

// RouteGuard reads the owner's answer nested as deep as routeDepth (9998 containers); an answer
// nested deeper is no readable answer, which the CLI reports as a host error.
func TestRouteGuard_reads_an_answer_within_its_nesting_cap(t *testing.T) {
	for _, row := range []struct {
		name     string
		reply    string
		readable bool
	}{
		{"9998 nested arrays", strings.Repeat("[", 9998) + strings.Repeat("]", 9998) + "\n", true},
		{"9998 nested objects", strings.Repeat("{\"a\": ", 9997) + "{}" + strings.Repeat("}", 9997) + "\n", true},
		{"9999 nested arrays", strings.Repeat("[", 9999) + strings.Repeat("]", 9999) + "\n", false},
		{"9999 nested objects", strings.Repeat("{\"a\": ", 9998) + "{}" + strings.Repeat("}", 9998) + "\n", false},
		{"brackets in a string", "[\"" + strings.Repeat("[", 10000) + "\\\"\"]\n", true},
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
			case err != nil:
				t.Fatal(err)
			case routed == nil:
				t.Fatal("no answer")
			case row.readable && (!routed.Readable || routed.Code != 0 || routed.Answer == nil):
				t.Fatalf("an answer within the nesting cap was not read: %+v", routed)
			case !row.readable && routed.Readable:
				t.Fatal("an answer past the nesting cap was read")
			}
		})
	}
}
