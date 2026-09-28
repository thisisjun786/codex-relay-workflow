package hook

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

type gatedAnswer struct {
	started, release chan struct{}
	bytes.Buffer
}

func (w *gatedAnswer) Write(p []byte) (int, error) {
	close(w.started)
	<-w.release
	return w.Buffer.Write(p)
}

// Prevent io.WriteString from calling the embedded Buffer.WriteString directly.
func (w *gatedAnswer) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func Test33AnsweredDeadlinePython(t *testing.T) {
	home := hookHome(t, 5)
	t.Setenv("CODEX_HOME", home)
	cmd := exec.Command(python(t), "testdata/late_verdict.py", "setup", home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	payload, err := os.ReadFile(filepath.Join(home, "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	var output string
	synctest.Test(t, func(t *testing.T) {
		deadline := &journalDeadline{Context: context.Background(), done: make(chan struct{})}
		ctx := context.WithValue(deadline, beforeEmitKey{}, func(ctx context.Context) {
			// Evaluate and verdict validation have completed. Expire the exact
			// context used by finish before it attempts to emit anything.
			close(deadline.done)
			<-ctx.Done()
			if ctx.Err() != context.DeadlineExceeded {
				t.Error(ctx.Err())
			}
		})
		done, _ := fakeControl(t, home, func(net.Conn) error { return nil })
		writer := &gatedAnswer{started: make(chan struct{}), release: make(chan struct{})}
		returned := make(chan int, 1)
		go func() {
			returned <- runAdapter(ctx, nil, bytes.NewReader(payload), writer, time.Now(), func(ctx context.Context, stop Object, options GuardOptions) (Object, error) {
				options.Now = "2026-01-01T00:00:00Z"
				return Evaluate(ctx, stop, options)
			})
		}()
		select {
		case <-writer.started:
		case <-time.After(10 * time.Second):
			t.Fatal("accepted block never reached stdout")
		}
		// Drain runnable work without timing luck. The old bounded write leaves
		// its writer pending but returns from Run, allowing main to exit first.
		synctest.Wait()
		returnedEarly := false
		select {
		case <-returned:
			returnedEarly = true
		default:
		}
		close(writer.release)
		synctest.Wait()
		if !returnedEarly {
			if code := <-returned; code != 0 {
				t.Fatal(code)
			}
		}
		awaitHost(t, done)
		output = writer.String()
		if returnedEarly {
			t.Error("Run returned before the accepted block was written")
		}
	})
	// Compare the actual output with Python's block from the same real guard
	// fixture. Reservations and observation files remain real, not mocked.
	cmd = exec.Command(python(t), "testdata/answered_deadline.py", home)
	cmd.Stdin = bytes.NewBufferString(output)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Python comparison: %v\n%s", err, out)
	} else {
		t.Logf("%s", out)
	}
}
