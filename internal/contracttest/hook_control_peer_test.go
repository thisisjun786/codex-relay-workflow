package contracttest

import (
	"sync"
	"testing"
	"time"
)

// The control peer's accept loop must never read a listener the run has already cleared. The CI
// failure of go-product (test-3) was exactly that: the loop sat between Accept calls when runHook
// cleared the listener variable, so the next Accept read the nil interface and panicked with
// "invalid memory address or nil pointer dereference". Reading the variable on the wrong side of
// that clear needs the loop to be preempted at one point, which is why the failure came once and
// then passed on a rerun.
//
// This test forces that interleaving with the peer's own seam: the loop is held after it has
// accepted the hook's connection and released only from the teardown that has just cleared the
// variable, so the Accept that follows is the one the CI run made. The two sides are ordered by the
// seam's channel, never by a sleep.
func TestHookControlPeerDoesNotReadTheClearedListener(t *testing.T) {
	scenarios, err := Load("hook")
	if err != nil {
		t.Fatal(err)
	}
	var scenario Scenario
	for _, candidate := range scenarios {
		if candidate.ID == "test_stop_adapter__test_the_same_stop_replayed_is_accepted_once_and_asked_about_once" {
			scenario = candidate
			break
		}
	}
	if scenario.ID == "" {
		t.Fatal("fixture missing")
	}
	accepted := make(chan struct{})
	released := make(chan struct{})
	var acceptedOnce, releasedOnce sync.Once
	hookPeerStep = func(step string) {
		switch step {
		case "accept":
			// Only the first acceptance is held: the fixture makes exactly one control call, and a
			// later connection must not block on a release the teardown has already sent.
			acceptedOnce.Do(func() {
				close(accepted)
				<-released
			})
		case "clear":
			releasedOnce.Do(func() { close(released) })
		}
	}
	t.Cleanup(func() { hookPeerStep = func(string) {} })

	type run struct {
		actual map[string]any
		err    error
	}
	done := make(chan run, 1)
	go func() {
		actual, err := runHook(t, scenario)
		done <- run{actual: actual, err: err}
	}()
	select {
	case <-accepted:
	case <-time.After(30 * time.Second):
		t.Fatal("the control peer never accepted the hook's connection")
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("the control peer failed after the run cleared its listener: %v", result.err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not finish after the control peer accepted the hook's connection")
	}
}
