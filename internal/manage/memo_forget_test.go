package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// memoForgetFakeProcess makes this test binary act as the fake crw for the relay helper Run
// starts, and returns the file that records one line of JSON arguments per call. It mirrors
// supervisorFakeProcess: Run fills Executable from os.Executable(), which is this test binary,
// and TestMain routes a re-executed binary carrying CRW_MANAGE_TEST_FAKE=1 to coreFakeMain.
// doctorState is the state path the fake doctor answer names and exit the status it ends with.
func memoForgetFakeProcess(t *testing.T, doctorState string, exit int) (record string) {
	t.Helper()
	record = filepath.Join(t.TempDir(), "argv.jsonl")
	t.Setenv(coreFakeEnv, "1")
	t.Setenv(coreFakeRecordEnv, record)
	t.Setenv(coreFakeDoctorEnv, doctorState)
	t.Setenv(coreFakeExitEnv, strconv.Itoa(exit))
	return record
}

// memoForgetClear drops every entry of the three Env-keyed memos, so the test starts from a
// known size whatever the tests before it left behind: a test that drives a command handler
// directly instead of Run resolves the relay state through an Env that nothing ever forgets.
func memoForgetClear() {
	relayHelperMemoMu.Lock()
	relayHelperMemo = map[*Env]string{}
	relayHelperMemoMu.Unlock()
	coreConfigMemoMu.Lock()
	coreConfigMemo = map[*Env]coreConfigState{}
	coreConfigMemoMu.Unlock()
	branchAlwaysMemoMu.Lock()
	branchAlwaysMemo = map[*Env]bool{}
	branchAlwaysMemoMu.Unlock()
}

// memoForgetSizes is the size of each Env-keyed memo, read under its own mutex.
func memoForgetSizes() (relayHelper, coreConfig, branchAlways int) {
	relayHelperMemoMu.Lock()
	relayHelper = len(relayHelperMemo)
	relayHelperMemoMu.Unlock()
	coreConfigMemoMu.Lock()
	coreConfig = len(coreConfigMemo)
	coreConfigMemoMu.Unlock()
	branchAlwaysMemoMu.Lock()
	branchAlways = len(branchAlwaysMemo)
	branchAlwaysMemoMu.Unlock()
	return relayHelper, coreConfig, branchAlways
}

// memoForgetDoctorCalls is how many doctor calls the fake crw recorded.
func memoForgetDoctorCalls(t *testing.T, record string) int {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		if len(argv) > 0 && argv[len(argv)-1] == "doctor" {
			count++
		}
	}
	return count
}

// memoForgetRun is one crw manage invocation through the registry, so the test proves the
// dispatch and the deferred cleanup and not only a handler.
func memoForgetRun(args ...string) int {
	var out, errOut strings.Builder
	return Run(context.Background(), args, strings.NewReader(""), &out, &errOut)
}

// Run forgets every Env-keyed memo of its invocation when it returns, so a process that calls
// Run in a loop - the crw gui run-state screen polls it every five seconds - keeps no entry per
// call. With no relay.state configured each of five Runs resolves the state through the doctor
// answer, which is the path that fills relayHelperMemo; before the fix that memo holds five.
func TestMemoForgetRunLeavesNoEntry(t *testing.T) {
	coreTempHome(t)
	record := memoForgetFakeProcess(t, filepath.Join(t.TempDir(), "relay-state"), 0)
	memoForgetClear()

	for i := 0; i < 5; i++ {
		if code := memoForgetRun("relay-read"); code != relayReadStoreExit {
			t.Fatalf("run %d exited %d, want %d", i, code, relayReadStoreExit)
		}
	}
	if got := memoForgetDoctorCalls(t, record); got != 5 {
		t.Fatalf("the fake doctor was asked %d times, want 5: the Runs did not resolve the relay state through the memo", got)
	}
	relayHelper, coreConfig, branchAlways := memoForgetSizes()
	if relayHelper != 0 || coreConfig != 0 || branchAlways != 0 {
		t.Errorf("after five Runs the memos hold relayHelper=%d coreConfig=%d branchAlways=%d entries, want 0 each",
			relayHelper, coreConfig, branchAlways)
	}
}

// The memo still answers within one Run: a command that resolves the relay state twice asks the
// fake doctor once, so forgetting at the end of a Run does not turn the per-Env memoization off.
func TestMemoForgetRunStillAsksDoctorOncePerRun(t *testing.T) {
	coreTempHome(t)
	record := memoForgetFakeProcess(t, filepath.Join(t.TempDir(), "relay-state"), 0)
	memoForgetClear()

	saved := coreRegistry
	defer func() { coreRegistry = saved }()
	Register(Command{Name: "memo-forget-probe", Summary: "resolve the relay state twice", Run: func(ctx context.Context, e *Env, _ []string) int {
		cfg := coreDefaults(e)
		for i := 0; i < 2; i++ {
			if _, err := e.relayHelperState(ctx, cfg); err != nil {
				t.Errorf("resolve the relay state: %v", err)
				return 1
			}
		}
		return 0
	}})
	if code := memoForgetRun("memo-forget-probe"); code != 0 {
		t.Fatalf("the probe exited %d, want 0", code)
	}
	if got := memoForgetDoctorCalls(t, record); got != 1 {
		t.Errorf("one Run that resolved the relay state twice asked the fake doctor %d times, want 1", got)
	}
}
