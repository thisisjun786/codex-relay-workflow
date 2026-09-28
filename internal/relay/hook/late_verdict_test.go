package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An event-driven deadline: expiration happens at the exact journal write,
// not after a sleep that might race a slow CI worker's guard evaluation.
type journalDeadline struct {
	context.Context
	done chan struct{}
}

func (c *journalDeadline) Done() <-chan struct{} { return c.done }
func (c *journalDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func Test33LateVerdictPython(t *testing.T) {
	for _, edge := range []string{"bookkeeping", "guard_timeout"} {
		t.Run(edge, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			setup := exec.Command(python(t), "testdata/late_verdict.py", "setup", home)
			if out, err := setup.CombinedOutput(); err != nil {
				t.Fatalf("setup: %v %s", err, out)
			}
			payload, err := os.ReadFile(filepath.Join(home, "stop.json"))
			if err != nil {
				t.Fatal(err)
			}
			dbPath := filepath.Join(home, "state/relay.sqlite3")
			before, err := os.ReadFile(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			var answers []string
			for attempt := range 2 {
				done, closeHost := fakeControl(t, home, func(c net.Conn) error { _, err := io.Copy(io.Discard, c); return err })
				deadline := &journalDeadline{Context: context.Background(), done: make(chan struct{})}
				var guardCtx context.Context
				wrote := make(chan struct{})
				ctx := context.WithValue(deadline, claimWriteKey{}, claimWriteFunc(func(f *os.File, raw []byte) (int, error) {
					n, err := f.Write(raw)
					if attempt == 0 && edge == "bookkeeping" && strings.Contains(f.Name(), "/journal/") {
						close(deadline.done)
						// Observe propagation BEFORE returning the write. Thus both the
						// original post-journal check and the fixed path see expiry.
						<-guardCtx.Done()
						close(wrote)
					}
					return n, err
				}))
				var output bytes.Buffer
				code := runAdapter(ctx, nil, bytes.NewReader(payload), &output, time.Now(), func(ctx context.Context, stop Object, options GuardOptions) (Object, error) {
					guardCtx = ctx
					options.Now = "2026-01-01T00:00:00Z"
					verdict, err := Evaluate(ctx, stop, options)
					if err != nil {
						return nil, err
					}
					if attempt == 0 && get(verdict, "decision") != "block" {
						return nil, fmt.Errorf("first verdict: %v", verdict)
					}
					if attempt == 1 && get(verdict, "state") != "hold_in_flight" {
						return nil, fmt.Errorf("next Stop: %v", verdict)
					}
					if attempt == 0 && edge == "guard_timeout" {
						// Same boundary as Python's communicate TimeoutExpired AFTER
						// the real guard's hold and observation have been published.
						return nil, context.DeadlineExceeded
					}
					return verdict, nil
				})
				if code != 0 {
					t.Fatal(code)
				}
				if attempt == 0 && edge == "bookkeeping" {
					select {
					case <-wrote:
					case <-time.After(10 * time.Second):
						t.Fatal("journal write not reached")
					}
				}
				awaitHost(t, done)
				closeHost()
				answers = append(answers, output.String())
			}
			after, err := os.ReadFile(dbPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("relay DB changed: %v", err)
			}
			raw, err := json.Marshal(map[string]any{"stdout": answers, "exit": 0})
			if err != nil {
				t.Fatal(err)
			}
			writeTest(t, filepath.Join(home, "result.json"), raw)
			for _, module := range []string{"console", "script"} {
				cmd := exec.Command(python(t), "testdata/late_verdict.py", "compare", home, edge, module, testRoot)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", module, err, out)
				}
				t.Logf("%s", out)
			}
		})
	}
}
