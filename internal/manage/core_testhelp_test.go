package manage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The fake crw is this test binary started again, so a test can pin the exact argument
// order a helper gives it: coreFakeCRW writes a shell script that sets the variables
// below and execs the test binary, and TestMain runs coreFakeMain when it sees them.

const (
	coreFakeEnv       = "CRW_MANAGE_TEST_FAKE"
	coreFakeRecordEnv = "CRW_MANAGE_TEST_RECORD"
	coreFakeDoctorEnv = "CRW_MANAGE_TEST_DOCTOR_STATE"
	coreFakeExitEnv   = "CRW_MANAGE_TEST_EXIT"
)

func TestMain(m *testing.M) {
	if os.Getenv(coreFakeEnv) != "" {
		coreFakeMain()
		return
	}
	os.Exit(m.Run())
}

// coreFakeMain is this test binary acting as the fake crw: it appends its own arguments
// to the record file, answers doctor, and exits with the status it was given.
func coreFakeMain() {
	argv := os.Args[1:]
	if path := os.Getenv(coreFakeRecordEnv); path != "" {
		line, err := json.Marshal(argv)
		if err != nil {
			coreFakeFail(err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			coreFakeFail(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			f.Close()
			coreFakeFail(err)
		}
		if err := f.Close(); err != nil {
			coreFakeFail(err)
		}
	}
	if len(argv) > 0 && argv[len(argv)-1] == "doctor" {
		answer, err := json.Marshal(map[string]any{"stateSelection": map[string]any{"path": os.Getenv(coreFakeDoctorEnv)}})
		if err != nil {
			coreFakeFail(err)
		}
		fmt.Printf("%s\n", answer)
	}
	if code := os.Getenv(coreFakeExitEnv); code != "" {
		n, err := strconv.Atoi(code)
		if err != nil {
			coreFakeFail(err)
		}
		os.Exit(n)
	}
}

func coreFakeFail(err error) {
	fmt.Fprintln(os.Stderr, "fake crw:", err)
	os.Exit(90)
}

// coreFakeCRW writes the fake crw and returns its path and the file that records one
// line of JSON arguments per call. doctorState is the state path its doctor answer
// reports and exit the status every call ends with.
func coreFakeCRW(t *testing.T, doctorState string, exit int) (exe, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "argv.jsonl")
	exe = filepath.Join(dir, "crw")
	script := "#!/bin/sh\n" +
		coreFakeEnv + "=1 " +
		coreFakeRecordEnv + "=" + coreShellQuote(record) + " " +
		coreFakeDoctorEnv + "=" + coreShellQuote(doctorState) + " " +
		coreFakeExitEnv + "=" + coreShellQuote(strconv.Itoa(exit)) + " " +
		"exec " + coreShellQuote(os.Args[0]) + " \"$@\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func coreShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func coreFakeCalls(t *testing.T, record string) [][]string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatalf("record %q: %v", line, err)
		}
		calls = append(calls, argv)
	}
	return calls
}

func coreCheckCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("the fake saw %d calls %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// coreTempHome points HOME, CODEX_HOME and CRW_HOME at a fresh temporary tree, clears
// XDG_STATE_HOME so a test can see the HOME fallback, and returns the three roots, so
// no test reaches the real ones.
func coreTempHome(t *testing.T) (home, codexHome, crwHome string) {
	t.Helper()
	home = t.TempDir()
	codexHome = filepath.Join(home, "codex")
	crwHome = filepath.Join(home, "crw")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CRW_HOME", crwHome)
	t.Setenv("XDG_STATE_HOME", "")
	return home, codexHome, crwHome
}

// The fake the helper builds runs and records the arguments it was given; a later
// node's tests pin their own helper's calls with it.
func TestCoreFakeCRWRecordsTheArgumentsItIsGiven(t *testing.T) {
	exe, record := coreFakeCRW(t, "", 0)
	if err := exec.Command(exe, "relay", "--state", "/s", "status").Run(); err != nil {
		t.Fatalf("the fake: %v", err)
	}
	coreCheckCalls(t, coreFakeCalls(t, record), [][]string{{"relay", "--state", "/s", "status"}})
}
