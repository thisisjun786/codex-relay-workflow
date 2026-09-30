package hook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
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
			lateVerdictFixture(t, home)
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
			actual := lateVerdictSnapshot(t, home)
			if edge == "guard_timeout" {
				// Native cancellation diagnostic (decision 24), not a Python process group.
				rows, _ := evidence.List(get(actual, "rows"))
				rows[0] = withoutKeys(object(rows[0]), "detail")
			}
			// Both Stops' answers and the snapshot are the goldens, which began as what Python's
			// adapter, the console module and the script alike, answered and left through the same
			// edge in a home of its own (late_verdict.py compare).
			goldenDumps(t, "stdout", answers, true)
			goldenDumps(t, "snapshot", actual, true)
		})
	}
}

// lateVerdictFixture lays late_verdict.py setup's fixture out under home: the published
// markers, an empty store, the settings and the Stop.
func lateVerdictFixture(t *testing.T, home string) {
	t.Helper()
	layFixture(t, "late-verdict", home)
}

// lateVerdictSnapshot is late_verdict.py snapshot(): the journal rows (by guard state) and the
// guard's hold and observation records, without times, with home spelled <HOME>.
func lateVerdictSnapshot(t *testing.T, home string) Object {
	t.Helper()
	var normalize func(any) any
	normalize = func(v any) any {
		switch value := v.(type) {
		case Object:
			out := Object{}
			for _, field := range value {
				if !slices.Contains([]string{"at", "elapsedMs", "guardElapsedMs", "identityScanMs"}, field.Key) {
					out = append(out, Field{Key: field.Key, Value: normalize(field.Value)})
				}
			}
			return out
		case []any:
			out := make([]any, len(value))
			for i, item := range value {
				out[i] = normalize(item)
			}
			return out
		case string:
			return strings.ReplaceAll(value, home, "<HOME>")
		}
		return v
	}
	read := func(path string) any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value, err := Decode(raw)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return normalize(value)
	}
	paths, _ := filepath.Glob(filepath.Join(home, "journal", "*", "*.json"))
	rows := []any{}
	for _, path := range paths {
		rows = append(rows, read(path))
	}
	slices.SortStableFunc(rows, func(a, b any) int {
		state := func(v any) string { s, _ := get(object(v), "guardState").(string); return s }
		return strings.Compare(state(a), state(b))
	})
	records := Object{}
	paths, _ = filepath.Glob(filepath.Join(home, "markers", "*", "*", "hook", "s", "t", "*.json"))
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, Field{Key: filepath.Base(path), Value: Object{{Key: "value", Value: read(path)}, {Key: "mode", Value: int64(info.Mode().Perm())}}})
	}
	return Object{{Key: "rows", Value: rows}, {Key: "records", Value: records}}
}
