package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// singleFlightDoctor is a fake crw whose doctor is gated by files in dir: the nth doctor call
// appends to doctor.calls, waits for the file gate<n>, then fails when fail<n> exists and
// otherwise answers the state /s<n>. Every other relay call is recorded in other.calls. The
// tests open a gate to let a doctor finish, so they decide which calls overlap.
type singleFlightDoctor struct {
	t   *testing.T
	dir string
	exe string
}

func newSingleFlightDoctor(t *testing.T) *singleFlightDoctor {
	t.Helper()
	d := &singleFlightDoctor{t: t, dir: t.TempDir()}
	q := coreShellQuote(d.dir)
	d.exe = relayHelperScript(t, "D="+q+"\n"+
		"last=\nfor a in \"$@\"; do last=$a; done\n"+
		"if [ \"$last\" = doctor ]; then\n"+
		"  echo x >> \"$D/doctor.calls\"\n"+
		"  n=$(wc -l < \"$D/doctor.calls\" | tr -d ' ')\n"+
		"  while [ ! -e \"$D/gate$n\" ]; do [ -d \"$D\" ] || exit 9; sleep 0.02; done\n"+
		"  if [ -e \"$D/fail$n\" ]; then echo boom 1>&2; exit 3; fi\n"+
		"  printf '{\"stateSelection\":{\"path\":\"/s%s\"}}\\n' \"$n\"\n"+
		"  exit 0\n"+
		"fi\n"+
		"printf '%s\\n' \"$*\" >> \"$D/other.calls\"\n")
	// A test that fails early must not leave a doctor polling for a gate that never opens.
	t.Cleanup(func() {
		for n := 1; n <= 8; n++ {
			d.open(n)
		}
	})
	return d
}

func (d *singleFlightDoctor) open(n int) {
	name := filepath.Join(d.dir, "gate"+string(rune('0'+n)))
	if f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
}

func (d *singleFlightDoctor) fail(n int) {
	d.t.Helper()
	if err := os.WriteFile(filepath.Join(d.dir, "fail"+string(rune('0'+n))), nil, 0o600); err != nil {
		d.t.Fatal(err)
	}
}

func (d *singleFlightDoctor) doctorCalls() int { return d.lines("doctor.calls") }

func (d *singleFlightDoctor) lines(name string) int {
	data, err := os.ReadFile(filepath.Join(d.dir, name))
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func (d *singleFlightDoctor) others() []string {
	data, _ := os.ReadFile(filepath.Join(d.dir, "other.calls"))
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), "\n", " \n "))
}

// waitDoctors waits until the fake has started n doctor calls.
func (d *singleFlightDoctor) waitDoctors(n int) {
	d.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for d.doctorCalls() < n {
		if time.Now().After(deadline) {
			d.t.Fatalf("only %d doctor calls started, want %d", d.doctorCalls(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type singleFlightResult struct {
	stdout []byte
	code   int
	err    error
}

func singleFlightRelay(e *Env, ctx context.Context, cfg *Config, arg string) <-chan singleFlightResult {
	out := make(chan singleFlightResult, 1)
	go func() {
		stdout, code, err := e.Relay(ctx, cfg, arg)
		out <- singleFlightResult{stdout, code, err}
	}()
	return out
}

func singleFlightWait(t *testing.T, ch <-chan singleFlightResult) singleFlightResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("a Relay call did not return")
		return singleFlightResult{}
	}
}

// settle is how long a test lets a second call run before it checks that it started no doctor.
const singleFlightSettle = 400 * time.Millisecond

// Two first calls that overlap on one Env share one doctor: the second waits for the first's
// answer, so both run with the same state and the doctor runs once.
func TestRelaySingleFlightOverlappingFirstCallsAskDoctorOnce(t *testing.T) {
	d := newSingleFlightDoctor(t)
	e := relayHelperTestEnv(d.exe)
	cfg := &Config{Relay: coreRelay{Socket: "/k"}}
	a := singleFlightRelay(e, context.Background(), cfg, "alpha")
	d.waitDoctors(1)
	b := singleFlightRelay(e, context.Background(), cfg, "beta")
	time.Sleep(singleFlightSettle)
	if got := d.doctorCalls(); got != 1 {
		t.Fatalf("while the first doctor ran, %d doctors had started, want 1", got)
	}
	d.open(1)
	d.open(2)
	for name, ch := range map[string]<-chan singleFlightResult{"alpha": a, "beta": b} {
		if r := singleFlightWait(t, ch); r.err != nil || r.code != 0 {
			t.Fatalf("Relay %s: exit %d err %v", name, r.code, r.err)
		}
	}
	if got := d.doctorCalls(); got != 1 {
		t.Errorf("the doctor ran %d times, want 1", got)
	}
	var states []string
	lines := strings.Split(strings.TrimSpace(readFileString(t, filepath.Join(d.dir, "other.calls"))), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "--state" {
			t.Fatalf("a relay call without the state flag: %q", line)
		}
		states = append(states, fields[2])
	}
	if len(states) != 2 || states[0] != "/s1" || states[1] != "/s1" {
		t.Errorf("the two calls ran with states %v, want both /s1", states)
	}
	// A later call reuses the one answer.
	if r := singleFlightWait(t, singleFlightRelay(e, context.Background(), cfg, "gamma")); r.err != nil {
		t.Fatal(r.err)
	}
	if got := d.doctorCalls(); got != 1 {
		t.Errorf("a call after the resolution asked the doctor again: %d calls", got)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The first caller's context ending does not reach the caller that waited: it asks the doctor
// itself, with its own context, and remembers that answer.
func TestRelaySingleFlightFirstCallersCancellationDoesNotReachTheWaiter(t *testing.T) {
	d := newSingleFlightDoctor(t)
	e := relayHelperTestEnv(d.exe)
	cfg := &Config{Relay: coreRelay{Socket: "/k"}}
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	a := singleFlightRelay(e, ctxA, cfg, "alpha")
	d.waitDoctors(1)
	b := singleFlightRelay(e, context.Background(), cfg, "beta")
	time.Sleep(singleFlightSettle)
	if got := d.doctorCalls(); got != 1 {
		t.Fatalf("while the first doctor ran, %d doctors had started, want 1", got)
	}
	cancelA()
	ra := singleFlightWait(t, a)
	if !relayHelperIsUnresolved(ra.err) || !errors.Is(ra.err, context.Canceled) {
		t.Fatalf("the cancelled caller got %v, want relay_state_unresolved naming the cancellation", ra.err)
	}
	d.waitDoctors(2)
	d.open(2)
	rb := singleFlightWait(t, b)
	if rb.err != nil {
		t.Fatalf("the waiter inherited the first caller's cancellation: %v", rb.err)
	}
	if r := singleFlightWait(t, singleFlightRelay(e, context.Background(), cfg, "gamma")); r.err != nil {
		t.Fatal(r.err)
	}
	if got := d.doctorCalls(); got != 2 {
		t.Errorf("the doctor ran %d times, want 2 (the cancelled one and the waiter's own)", got)
	}
	if !strings.Contains(readFileString(t, filepath.Join(d.dir, "other.calls")), "--state /s2 --socket /k beta") {
		t.Errorf("the waiter did not run with the state its own doctor answered: %q", readFileString(t, filepath.Join(d.dir, "other.calls")))
	}
}

// A failed resolution is not remembered: the next call asks the doctor again. A caller that
// waited on the failed doctor is told the same failure and does not start a second doctor.
func TestRelaySingleFlightFailureIsNotRemembered(t *testing.T) {
	d := newSingleFlightDoctor(t)
	d.fail(1)
	e := relayHelperTestEnv(d.exe)
	cfg := &Config{Relay: coreRelay{Socket: "/k"}}
	a := singleFlightRelay(e, context.Background(), cfg, "alpha")
	d.waitDoctors(1)
	b := singleFlightRelay(e, context.Background(), cfg, "beta")
	time.Sleep(singleFlightSettle)
	d.open(1)
	for name, ch := range map[string]<-chan singleFlightResult{"alpha": a, "beta": b} {
		if r := singleFlightWait(t, ch); !relayHelperIsUnresolved(r.err) {
			t.Fatalf("Relay %s: err %v, want relay_state_unresolved", name, r.err)
		}
	}
	if got := d.doctorCalls(); got != 1 {
		t.Fatalf("the overlapping callers ran %d doctors, want the one failed doctor", got)
	}
	d.open(2)
	if r := singleFlightWait(t, singleFlightRelay(e, context.Background(), cfg, "gamma")); r.err != nil {
		t.Fatalf("a call after the failure: %v", r.err)
	}
	if got := d.doctorCalls(); got != 2 {
		t.Errorf("the failure was remembered: %d doctor calls, want 2", got)
	}
}

// Forgetting an Env drops a resolution that is still running, and the resolution that ends
// afterwards does not bring the entry back: the next call asks the doctor again.
func TestRelaySingleFlightForgetDropsARunningResolution(t *testing.T) {
	memoForgetClear()
	d := newSingleFlightDoctor(t)
	e := relayHelperTestEnv(d.exe)
	cfg := &Config{Relay: coreRelay{Socket: "/k"}}
	a := singleFlightRelay(e, context.Background(), cfg, "alpha")
	d.waitDoctors(1)
	relayHelperForget(e)
	if relayHelper, _, _ := memoForgetSizes(); relayHelper != 0 {
		t.Fatalf("after Forget the memo holds %d entries, want 0", relayHelper)
	}
	d.open(1)
	if r := singleFlightWait(t, a); r.err != nil {
		t.Fatalf("the running call: %v", r.err)
	}
	if relayHelper, _, _ := memoForgetSizes(); relayHelper != 0 {
		t.Errorf("a resolution that ended after Forget left %d memo entries, want 0", relayHelper)
	}
	d.open(2)
	if r := singleFlightWait(t, singleFlightRelay(e, context.Background(), cfg, "beta")); r.err != nil {
		t.Fatal(r.err)
	}
	if got := d.doctorCalls(); got != 2 {
		t.Errorf("a late resolution revived the entry: %d doctor calls, want 2", got)
	}
	relayHelperForget(e)
}

// A configured relay.state never asks a doctor, whatever overlaps.
func TestRelaySingleFlightConfiguredStateAsksNoDoctor(t *testing.T) {
	d := newSingleFlightDoctor(t)
	e := relayHelperTestEnv(d.exe)
	cfg := &Config{Relay: coreRelay{State: "/s0", Socket: "/k"}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := e.Relay(context.Background(), cfg, "status"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := d.doctorCalls(); got != 0 {
		t.Errorf("a configured state asked the doctor %d times", got)
	}
}
