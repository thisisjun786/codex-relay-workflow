package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
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
	lateVerdictFixture(t, home)
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
	// Compare the actual output with Python's block from the same real guard fixture:
	// answered_deadline.py python, run where this hook ran, answers its stdout and the hold and
	// observation files its guard published, recorded (pyoracle). Reservations and observation
	// files remain real, not mocked.
	paths, err := filepath.Glob(filepath.Join(home, "markers", "*", "*", "hook", "s", "t", "*.json"))
	if err != nil || len(paths) != 2 {
		t.Fatal(paths, err)
	}
	files := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.Base(path)] = string(raw)
	}
	if _, ok := files["hold.json"]; !ok {
		t.Fatal(paths)
	}
	raw := pyoracle.Answer(t, "python", func() ([]byte, error) {
		return pythonScript(t, nil, []byte(output), "testdata/answered_deadline.py", home, "python")
	}, pyoracle.Substitute(home, "<HOME>"))
	var python struct {
		Stdout string            `json:"stdout"`
		Files  map[string]string `json:"files"`
	}
	if err := json.Unmarshal(raw, &python); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if output == "" || python.Stdout != output || !maps.Equal(python.Files, files) {
		t.Fatalf("go %q %v\npython %q %v", output, files, python.Stdout, python.Files)
	}
}
