package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// relayHelperTestEnv is an Env whose only filled seam is the executable the helper runs,
// which is all Relay reads.
func relayHelperTestEnv(exe string) *Env { return &Env{Executable: exe} }

// relayHelperScript writes a shell script that runs body and returns its path, so a test
// can pin the stdout, stderr and exit status the helper reports for a command that ran.
func relayHelperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// The helper runs the same executable's relay mode with the configured state and socket,
// and the argument order is fixed (C2).
func TestRelayRunsTheRelayWithTheConfiguredStateAndSocket(t *testing.T) {
	exe, record := coreFakeCRW(t, "", 0)
	cfg := &Config{Relay: coreRelay{State: "/s0", Socket: "/k0"}}
	stdout, code, err := relayHelperTestEnv(exe).Relay(context.Background(), cfg, "status", "--json")
	if err != nil || code != 0 || len(stdout) != 0 {
		t.Fatalf("Relay: stdout %q exit %d err %v", stdout, code, err)
	}
	coreCheckCalls(t, coreFakeCalls(t, record), [][]string{
		{"relay", "--state", "/s0", "--socket", "/k0", "status", "--json"},
	})
}

// An empty relay.state is resolved from the doctor answer's stateSelection.path and
// remembered for the Env, so a second call does not ask doctor again (C1).
func TestRelayResolvesAnEmptyStateOncePerEnv(t *testing.T) {
	exe, record := coreFakeCRW(t, "/s1", 0)
	e := relayHelperTestEnv(exe)
	cfg := &Config{Relay: coreRelay{Socket: "/k1"}}
	for _, args := range [][]string{{"status"}, {"show"}} {
		if _, code, err := e.Relay(context.Background(), cfg, args...); err != nil || code != 0 {
			t.Fatalf("Relay %q: exit %d err %v", args, code, err)
		}
	}
	coreCheckCalls(t, coreFakeCalls(t, record), [][]string{
		{"relay", "--socket", "/k1", "doctor"},
		{"relay", "--state", "/s1", "--socket", "/k1", "status"},
		{"relay", "--state", "/s1", "--socket", "/k1", "show"},
	})
}

// The remembered state belongs to one Env: a second Env resolves it again (C1).
func TestRelayResolvesTheStatePerEnv(t *testing.T) {
	exe, record := coreFakeCRW(t, "/s1", 0)
	cfg := &Config{Relay: coreRelay{Socket: "/k2"}}
	for i := 0; i < 2; i++ {
		if _, code, err := relayHelperTestEnv(exe).Relay(context.Background(), cfg, "status"); err != nil || code != 0 {
			t.Fatalf("Relay %d: exit %d err %v", i, code, err)
		}
	}
	coreCheckCalls(t, coreFakeCalls(t, record), [][]string{
		{"relay", "--socket", "/k2", "doctor"},
		{"relay", "--state", "/s1", "--socket", "/k2", "status"},
		{"relay", "--socket", "/k2", "doctor"},
		{"relay", "--state", "/s1", "--socket", "/k2", "status"},
	})
}

// A doctor that fails is relay_state_unresolved, and no result is reported (C3).
func TestRelayRefusesADoctorThatFails(t *testing.T) {
	exe, _ := coreFakeCRW(t, "", 3)
	stdout, code, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{Socket: "/k0"}}, "status")
	if !errors.Is(err, relayHelperErrStateUnresolved) {
		t.Fatalf("err = %v, want relay_state_unresolved", err)
	}
	if len(stdout) != 0 || code != 0 {
		t.Errorf("a refusal reported stdout %q exit %d", stdout, code)
	}
}

// A doctor that names no state directory is relay_state_unresolved (C3).
func TestRelayRefusesADoctorWithoutAState(t *testing.T) {
	exe, _ := coreFakeCRW(t, "", 0)
	if _, _, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{Socket: "/k0"}}, "status"); !errors.Is(err, relayHelperErrStateUnresolved) {
		t.Fatalf("err = %v, want relay_state_unresolved", err)
	}
}

// A doctor answer that is not JSON is relay_state_unresolved too, and still reports the
// stderr the doctor wrote (C3).
func TestRelayRefusesADoctorThatAnswersGarbage(t *testing.T) {
	exe := relayHelperScript(t, "printf '%s\n' 'not json'\nprintf '%s\n' 'doctor noise' 1>&2\nexit 0\n")
	_, _, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{Socket: "/k0"}}, "status")
	if !errors.Is(err, relayHelperErrStateUnresolved) {
		t.Fatalf("err = %v, want relay_state_unresolved", err)
	}
	if !strings.Contains(err.Error(), "doctor noise") {
		t.Errorf("the refusal dropped the doctor's stderr: %v", err)
	}
}

// A relay executable that cannot be run is relay_state_unresolved (C3).
func TestRelayRefusesADoctorItCannotRun(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "no-such-crw")
	if _, _, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{Socket: "/k0"}}, "status"); !errors.Is(err, relayHelperErrStateUnresolved) {
		t.Fatalf("err = %v, want relay_state_unresolved", err)
	}
}

// The exit status and stdout of a command that ran come back unchanged, with no error (C4).
func TestRelayReturnsTheExitStatusAndStdout(t *testing.T) {
	exe := relayHelperScript(t, "printf '%s\n' 'relay said hello'\nexit 3\n")
	stdout, code, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{State: "/s0", Socket: "/k0"}}, "status")
	if err != nil || code != 3 {
		t.Fatalf("exit %d err %v", code, err)
	}
	if string(stdout) != "relay said hello\n" {
		t.Errorf("stdout = %q, want %q", stdout, "relay said hello\n")
	}
}

// An error carries at most the first 2000 bytes of the command's stderr.
func TestRelayKeepsTheFirstTwoThousandBytesOfStderr(t *testing.T) {
	exe := relayHelperScript(t, "printf '%s' '"+strings.Repeat("q", 2500)+"' 1>&2\nexit 7\n")
	_, _, err := relayHelperTestEnv(exe).Relay(context.Background(), &Config{Relay: coreRelay{Socket: "/k0"}}, "status")
	if err == nil {
		t.Fatal("a failing doctor was accepted")
	}
	if got := strings.Count(err.Error(), "q"); got != 2000 {
		t.Errorf("the error kept %d stderr bytes, want 2000: %v", got, err)
	}
}

// A context that ends while the command runs is that context's error, and the answer the
// command had half written is not reported as a finished one (C4).
func TestRelayReportsAContextThatEnds(t *testing.T) {
	exe := relayHelperScript(t, "printf '%s\n' 'half an answer'\nexec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	stdout, code, err := relayHelperTestEnv(exe).Relay(ctx, &Config{Relay: coreRelay{State: "/s0", Socket: "/k0"}}, "status")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's error", err)
	}
	if len(stdout) != 0 || code != 0 {
		t.Errorf("a command that was cut off reported stdout %q exit %d", stdout, code)
	}
}

// A doctor whose context ends is relay_state_unresolved and still names the cancellation.
func TestRelayReportsADoctorWhoseContextEnds(t *testing.T) {
	exe := relayHelperScript(t, "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _, err := relayHelperTestEnv(exe).Relay(ctx, &Config{Relay: coreRelay{Socket: "/k0"}}, "status")
	if !errors.Is(err, relayHelperErrStateUnresolved) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want relay_state_unresolved naming the deadline", err)
	}
}
