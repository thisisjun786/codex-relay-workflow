package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/metric"
)

// CRW-632: the CLI entry points of the writers that waited for a lock without a deadline take the
// invocation's context, as RunMetricCLIContext already did for the ingest. These cases hold each lock
// from their own descriptor and cancel the context: the entry point must return the context's own error
// (so the harness row answers Interrupted and prints nothing) and write nothing. They are identity and
// propagation checks, not evidence of where the wait was: the settle only gives the call time to reach
// the lock, and cancelling earlier gives the same error. A build that ignores ctx blocks and fails the
// 5 s bound.

// crw632Hold locks path exclusively, as a directory when dir is true, and returns the release.
func crw632Hold(t *testing.T, path string, dir bool) func() {
	t.Helper()
	var f *os.File
	var err error
	if dir {
		if err = os.MkdirAll(path, 0o777); err != nil {
			t.Fatal(err)
		}
		f, err = os.Open(path)
	} else {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { f.Close() }) }
	t.Cleanup(release)
	return release
}

// crw632Run cancels ctx once the call has had time to reach the held lock and returns its error.
func crw632Run(t *testing.T, call func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the call was still waiting 5 s after its context ended")
		return nil
	}
}

func TestRunMetricCLIContextEndsTheWriterWaitsWithTheContext(t *testing.T) {
	metrics, kinds := filepath.Join(crwdir.DirName, metric.MetricsFile), filepath.Join(crwdir.DirName, metric.ObjectiveKindDir)
	for _, tc := range []struct {
		name string
		args []string
		hold func(t *testing.T, cwd string) func()
		// wrote reports whether the writer added anything: the record and candidate cases compare the
		// bytes of the file the holder created, the kind case the session file it would have made.
		wrote func(cwd string) bool
	}{
		{"record", []string{"record", "--session", "s1", "--name", "n", "--value", "1"},
			func(t *testing.T, cwd string) func() { return crw632Hold(t, filepath.Join(cwd, metrics), false) },
			func(cwd string) bool {
				raw, err := os.ReadFile(filepath.Join(cwd, metrics))
				return err == nil && len(raw) != 0
			}},
		{"kind", []string{"kind", "--session", "s1", "maximize"},
			func(t *testing.T, cwd string) func() { return crw632Hold(t, filepath.Join(cwd, kinds), true) },
			func(cwd string) bool { _, err := os.Lstat(filepath.Join(cwd, kinds, "s1.json")); return err == nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			tc.hold(t, cwd)
			err := crw632Run(t, func(ctx context.Context) error {
				_, err := RunMetricCLIContext(ctx, tc.args, cwd, "")
				return err
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the %s writer cancelled in its lock wait returned %v, want an error that is context.Canceled", tc.name, err)
			}
			if tc.wrote(cwd) {
				t.Fatalf("the cancelled %s writer wrote its file", tc.name)
			}
		})
	}
}

func TestRunDivergenceCliContextEndsTheWriterWaitsWithTheContext(t *testing.T) {
	divergence := filepath.Join(crwdir.DirName, metric.DivergenceDir)
	for _, tc := range []struct {
		name  string
		args  []string
		hold  func(t *testing.T, cwd string) func()
		wrote func(cwd string) bool
	}{
		{"mode", []string{"mode", "on", "--session", "s1"},
			func(t *testing.T, cwd string) func() { return crw632Hold(t, filepath.Join(cwd, divergence), true) },
			func(cwd string) bool {
				_, err := os.Lstat(filepath.Join(cwd, divergence, "s1.mode.json"))
				return err == nil
			}},
		{"candidate add", []string{"candidate", "add", "--session", "s1", "--kind", "strong-1", "--title", "t", "--rationale", "r", "--source", "u"},
			func(t *testing.T, cwd string) func() {
				return crw632Hold(t, filepath.Join(cwd, divergence, metric.CandidatesFile), false)
			},
			func(cwd string) bool {
				raw, err := os.ReadFile(filepath.Join(cwd, divergence, metric.CandidatesFile))
				return err == nil && len(raw) != 0
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			tc.hold(t, cwd)
			var result DivergenceCliResult
			err := crw632Run(t, func(ctx context.Context) error {
				var err error
				result, err = RunDivergenceCliContext(ctx, tc.args, cwd)
				return err
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("divergence %s cancelled in its lock wait returned %v, want an error that is context.Canceled", tc.name, err)
			}
			if result.Output != "" || result.Code != 0 {
				t.Fatalf("the cancelled divergence %s answered %q with code %d; want the zero result the row answers Interrupted with", tc.name, result.Output, result.Code)
			}
			if tc.wrote(cwd) {
				t.Fatalf("the cancelled divergence %s wrote its file", tc.name)
			}
		})
	}
}
