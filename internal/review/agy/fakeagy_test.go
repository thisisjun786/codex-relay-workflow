package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fake agy is this test binary started again. Config.Binary is a small shell script that sets CRW_FAKE_SPEC to the JSON file of a fakeSpec and execs the
// test binary, so the spec reaches the fake after the runner's scrub has run, not through it. The fake records what it saw in spec.Record, writes agy's log,
// then answers as the spec says.

type fakeSpec struct {
	Stdout, Stderr string
	Exit           int
	Kill           bool          // die by SIGKILL after answering instead of exiting
	LogLength      int           // the promptLength to log: 0 the bytes read from stdin, -1 no line
	NoLabel        bool          // log no served-model line
	NoStdin        bool          // never read stdin
	Sleep          time.Duration // before answering, after the log and the record are written
	Child          bool          // start a child in the same process group that sleeps a minute
	ChildPipes     bool          // the child also holds stdout and stderr open
	Flood          int           // write this many bytes to stdout instead of Stdout
	Record         string
}

type fakeRecord struct {
	Args        []string
	Stdin       []byte
	StdinIsPipe bool
	Env         []string
	Fds         []string // what the fake had open (Linux only)
	Cwd         string
	CwdEntries  []string
	Schema      string
	Pid, Pgid   int
	Child       int
	Start, End  int64
}

func TestMain(m *testing.M) {
	switch {
	case os.Getenv("CRW_FAKE_CHILD") != "":
		time.Sleep(time.Minute)
	case os.Getenv("CRW_FAKE_SPEC") != "":
		fakeAgy()
	default:
		os.Exit(m.Run())
	}
}

func argAfter(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func fakeAgy() {
	var sp fakeSpec
	spec, err := os.ReadFile(os.Getenv("CRW_FAKE_SPEC"))
	if err == nil {
		err = json.Unmarshal(spec, &sp)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(99)
	}
	rec := fakeRecord{Args: os.Args[1:], Env: os.Environ(), Pid: os.Getpid(), Pgid: syscall.Getpgrp(), Start: time.Now().UnixNano()}
	rec.Cwd, _ = os.Getwd()
	if entries, err := os.ReadDir("."); err == nil {
		for _, e := range entries {
			rec.CwdEntries = append(rec.CwdEntries, e.Name())
		}
	}
	if fds, err := os.ReadDir("/proc/self/fd"); err == nil {
		for _, fd := range fds {
			if target, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil {
				rec.Fds = append(rec.Fds, target)
			}
		}
	}
	if st, err := os.Stdin.Stat(); err == nil {
		rec.StdinIsPipe = st.Mode()&os.ModeNamedPipe != 0
	}
	if !sp.NoStdin {
		rec.Stdin, _ = io.ReadAll(os.Stdin)
	}
	if path := argAfter(rec.Args, "--json-schema"); path != "" {
		b, _ := os.ReadFile(path)
		rec.Schema = string(b)
	}
	if sp.Child {
		c := exec.Command(os.Args[0])
		c.Env = []string{"CRW_FAKE_CHILD=1"}
		if sp.ChildPipes {
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
		}
		if c.Start() == nil {
			rec.Child = c.Process.Pid
		}
	}
	if path := argAfter(rec.Args, "--log-file"); path != "" {
		// The start-up lines are R0's: agy says it is not logged in on every call and then logs itself in silently.
		log := "I1003 19:21:46.000000       1 printmode.go:150] You are not logged into Antigravity\nI1003 19:21:46.100000       1 printmode.go:160] Print mode: not authenticated, trying silent auth\n" +
			"I1003 19:21:46.900000       1 printmode.go:170] Print mode: silent auth succeeded\n"
		if n := loggedLength(sp.LogLength, len(rec.Stdin)); n >= 0 {
			log += fmt.Sprintf("I1003 19:21:47.093603       1 printmode.go:202] Print mode: starting (promptLength=%d, model=%q, conversationID=\"\")\n", n, argAfter(rec.Args, "--model"))
		}
		if !sp.NoLabel {
			log += "I1003 19:21:48.546335       1 model_config_manager.go:327] Propagating selected model override to backend: label=\"Gemini 3.8 Flash (High)\"\n"
		}
		_ = os.WriteFile(path, []byte(log), 0o600)
	}
	save := func() { // atomically, so a test that sees the file sees all of it
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(sp.Record+".tmp", b, 0o600)
		_ = os.Rename(sp.Record+".tmp", sp.Record)
	}
	save()
	time.Sleep(sp.Sleep)
	rec.End = time.Now().UnixNano()
	save()
	if sp.Flood > 0 {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), sp.Flood))
	} else {
		_, _ = os.Stdout.WriteString(sp.Stdout)
	}
	_, _ = os.Stderr.WriteString(sp.Stderr)
	if sp.Kill {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	os.Exit(sp.Exit)
}

// loggedLength is the promptLength the fake logs: the bytes it read when the spec says 0, the spec's value otherwise (-1 logs no line).
func loggedLength(logLength, read int) int {
	if logLength == 0 {
		return read
	}
	return logLength
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// fakeCfg is a Config that runs the fake as the spec says, with its lock, working root and record in a temporary directory and a one minute time limit. The
// returned function reads the record.
func fakeCfg(t *testing.T, sp fakeSpec) (Config, func() fakeRecord) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sp.Record = filepath.Join(dir, "record.json")
	spec, err := json.Marshal(sp)
	if err != nil {
		t.Fatal(err)
	}
	specPath, binary := filepath.Join(dir, "spec.json"), filepath.Join(dir, "agy")
	script := fmt.Sprintf("#!/bin/sh\nCRW_FAKE_SPEC=%s exec %s \"$@\"\n", shellQuote(specPath), shellQuote(exe))
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Binary: binary, LockPath: filepath.Join(dir, "agy.lock"), LockWait: 10 * time.Second, WorkRoot: filepath.Join(dir, "work"),
		TimeLimitFloor: time.Minute, TimeLimitCeiling: time.Minute}
	return cfg, func() fakeRecord { return readRecord(t, sp.Record) }
}

// recordPath is where the fake of a fakeCfg Config writes its record, which exists once the fake has started.
func recordPath(cfg Config) string { return filepath.Join(filepath.Dir(cfg.LockPath), "record.json") }

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	var rec fakeRecord
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &rec)
	}
	if err != nil {
		t.Fatalf("the fake left no record: %v", err)
	}
	return rec
}

func try(cfg Config, req Request) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return Run(ctx, cfg, req)
}

func run(t *testing.T, cfg Config, req Request) Result {
	t.Helper()
	res, err := try(cfg, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// zombie is true for a process that has died and that nobody has reaped yet (Linux only).
func zombie(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	return err == nil && bytes.Contains(stat, []byte(") Z "))
}

// gone waits for a process to be gone; a zombie that nobody has reaped yet counts as gone.
func gone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if zombie(pid) {
			return
		}
	}
	t.Errorf("process %d is still running", pid)
}
