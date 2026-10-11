package hook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// wakeRecorder is the wake function of a served Stop: the threads it was told, in order.
type wakeRecorder struct {
	mu      sync.Mutex
	threads []string
}

func (w *wakeRecorder) wake(thread string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.threads = append(w.threads, thread)
}

func (w *wakeRecorder) told() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.threads...)
}

// serveStop lays the control-default fixture <name> (a managed child's Stop and the owner state that
// answers it), serves its request through HandleControlWake with wake and returns the verdict the
// owner wrote and the session id the Stop carried. edit, when set, changes the request's stop input
// before it is sent.
func serveStop(t *testing.T, name string, wake func(string), edit func(stop map[string]any)) (verdict map[string]any, session string) {
	t.Helper()
	// The fixture's receipt digests the artifact paths, so it is laid out under the root of the test
	// that owns it (Test33ControlDefaultStorePython).
	home := hookHomeAt(t, canonicalRootNamed(t, "Test33ControlDefaultStorePython/"+name), 5)
	raw, _ := layFixture(t, "control-default-"+name, home)
	var fixture struct{ State string }
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SESSION_RELAY_MARKER_ROOT", filepath.Join(home, "markers"))
	request, err := os.ReadFile(filepath.Join(home, "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err = json.Unmarshal(request, &frame); err != nil {
		t.Fatal(err)
	}
	stop := frame["params"].(map[string]any)["stopInput"].(map[string]any)
	if edit != nil {
		edit(stop)
	}
	session, _ = stop["session_id"].(string)
	if request, err = json.Marshal(frame); err != nil {
		t.Fatal(err)
	}
	owner, peer := socketPair(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- HandleControlWake(ctx, owner, fixture.State, wake) }()
	if err = peer.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.Write(append(request, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = json.NewDecoder(bytes.NewReader(line)).Decode(&verdict); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	return verdict, session
}

// A Stop that ends a managed child's turn (the guard selected the assignment and released the turn)
// tells the wake the child's thread id, once.
func TestWakeFromManagedChildStop(t *testing.T) {
	var recorder wakeRecorder
	verdict, session := serveStop(t, "default_receipted", recorder.wake, nil)
	if verdict["decision"] != "release" || verdict["assignmentId"] == nil || session == "" {
		t.Fatalf("fixture is not a released managed Stop: %v session=%q", verdict, session)
	}
	if got := recorder.told(); len(got) != 1 || got[0] != session {
		t.Fatalf("wake told %v, want the child's thread %q once", got, session)
	}
}

// A Stop the guard holds open (decision block) continues the turn: nothing ended, nobody is woken.
func TestWakeNotSentForAHeldStop(t *testing.T) {
	var recorder wakeRecorder
	verdict, _ := serveStop(t, "default_missing_receipt", recorder.wake, nil)
	if verdict["decision"] != "block" {
		t.Fatalf("fixture is not a held Stop: %v", verdict)
	}
	if got := recorder.told(); len(got) != 0 {
		t.Fatalf("a held Stop woke %v", got)
	}
}

// A Stop outside every assignment is not the relay's turn: nobody is woken.
func TestWakeNotSentForAnUnmanagedStop(t *testing.T) {
	var recorder wakeRecorder
	verdict, _ := serveStop(t, "default_receipted", recorder.wake, func(stop map[string]any) {
		stop["cwd"] = filepath.Join(t.TempDir(), "elsewhere")
	})
	if verdict["assignmentId"] != nil {
		t.Fatalf("the Stop still selected an assignment: %v", verdict)
	}
	if got := recorder.told(); len(got) != 0 {
		t.Fatalf("an unmanaged Stop woke %v", got)
	}
}

// HandleControl, the entry that has no wake, answers exactly as before.
func TestWakeIsOptionalForHandleControl(t *testing.T) {
	verdict, _ := serveStop(t, "default_receipted", nil, nil)
	if verdict["decision"] != "release" {
		t.Fatalf("%v", verdict)
	}
}

// End to end across the control handler and the daemon loop: the Stop of a managed child, served by
// HandleControlWake, wakes the loop, whose next tick runs about a second later (the debounce) instead of after
// the poll interval, which is thirty seconds here, and is told the child's thread.
func TestWakeRunsTickWithoutPollInterval(t *testing.T) {
	waker := daemon.NewWaker()
	var session string
	var told [][]string
	tick := func(ctx context.Context) (daemon.Report, error) {
		told = append(told, daemon.Woken(ctx))
		if len(told) == 1 {
			// The child's turn ends while the daemon is between ticks: its Stop reaches the control handler.
			_, session = serveStop(t, "default_receipted", waker.Wake, nil)
		}
		return daemon.Report{}, nil
	}
	two := 2
	began := time.Now()
	reports, err := daemon.RunWoken(t.Context(), tick, delivery.SystemClock{}, 30, &two, nil, nil, waker)
	took := time.Since(began)
	if err != nil || len(reports) != 2 {
		t.Fatalf("%d ticks, %v", len(reports), err)
	}
	if took > 15*time.Second {
		t.Fatalf("the woken tick came after %v, not before the 30 s poll interval", took)
	}
	if len(told[0]) != 0 || !slices.Equal(told[1], []string{session}) || session == "" {
		t.Fatalf("ticks were told %v, want none and then the child's thread %q", told, session)
	}
}
