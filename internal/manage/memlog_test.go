package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// memlogProcSpec is one process of a fake /proc tree.
type memlogProcSpec struct {
	pid, ppid int
	rss       int64
	cmd       string
}

// memlogWriteTree writes a fake /proc tree: the two host files and one directory per
// process, so a test points the sampler at a tree instead of the host's /proc.
func memlogWriteTree(t *testing.T, root string, procs []memlogProcSpec) {
	t.Helper()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "meminfo"), "MemAvailable: 1000000 kB\nSwapTotal: 2000 kB\nSwapFree: 500 kB\n")
	write(filepath.Join(root, "pressure", "memory"), "some avg10=1.50 avg60=0.75 avg300=0.10 total=42\nfull avg10=0.25 avg60=0.05 avg300=0.01 total=7\n")
	for _, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		// The command name carries a ")" on purpose: the parent is the field after the last one.
		write(filepath.Join(dir, "stat"), fmt.Sprintf("%d (pr)oc) S %d 1 1 0\n", p.pid, p.ppid))
		write(filepath.Join(dir, "status"), fmt.Sprintf("Name:\tproc\nVmRSS:\t%d kB\n", p.rss))
		write(filepath.Join(dir, "cmdline"), strings.ReplaceAll(p.cmd, " ", "\x00")+"\x00")
	}
}

// memlogClock answers the given instants in order and cancels after the last one.
type memlogClock struct {
	times  []time.Time
	cancel context.CancelFunc
}

func (c *memlogClock) now() time.Time {
	next := c.times[0]
	if len(c.times) > 1 {
		c.times = c.times[1:]
		return next
	}
	if c.cancel != nil {
		c.cancel()
	}
	return next
}

// memlogEnv is the Env a test runs the command with.
func memlogEnv(now func() time.Time, out, errOut io.Writer) *Env {
	return &Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errOut, Getenv: os.Getenv, Now: now}
}

// memlogFixedClock answers the same instant every time it is asked.
func memlogFixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

// memlogState points HOME, CODEX_HOME, CRW_HOME and every XDG directory at a fresh
// temporary tree, so no test reaches a real home or a real XDG directory an outer
// environment may have inherited. It returns that home and the configuration the
// environment resolves to, whose StateDir is where the samples land.
func memlogState(t *testing.T) (home string, cfg *Config) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return home, coreDefaults(memlogEnv(time.Now, io.Discard, io.Discard))
}

// memlogSampleDir is where crw manage memlog writes below a state directory.
func memlogSampleDir(stateDir string) string { return filepath.Join(stateDir, "memlog") }

// memlogRecordFile reads the single dated file of a sample directory and returns its one
// record and the line it was written as.
func memlogRecordFile(t *testing.T, dir string) (memlogRecord, string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the sample directory holds %d files, want 1: %v", len(entries), entries)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var record memlogRecord
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &record) != nil {
		t.Fatalf("%s holds %q, want one record line", entries[0].Name(), lines)
	}
	return record, lines[0]
}

// memlogRunOnce runs crw manage memlog --once over a fake tree and returns the record it
// wrote and the line it wrote.
func memlogRunOnce(t *testing.T, cfg *Config, root string, at time.Time) (memlogRecord, string) {
	t.Helper()
	var out, errOut strings.Builder
	env := memlogEnv(memlogFixedClock(at), &out, &errOut)
	if code := memlogRunWith(context.Background(), env, cfg, []string{"--once"}, memlogNewProcSampler(root)); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	return memlogRecordFile(t, memlogSampleDir(cfg.StateDir))
}

// C1: a fake /proc tree pins the counters, the pressure, the group assignment (the App
// Server's descendants included) and the ten largest processes.
func TestMemlogPinsTheSampleOfAFakeProcTree(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{
		{pid: 100, ppid: 1, rss: 300000, cmd: "/usr/bin/codex app-server --socket x"},
		{pid: 101, ppid: 100, rss: 200000, cmd: "node mcp-helper.js"},
		{pid: 102, ppid: 101, rss: 100000, cmd: "/bin/sh -c true"},
		{pid: 110, ppid: 1, rss: 90000, cmd: "/usr/local/go/pkg/tool/linux_amd64/compile -p x"},
		{pid: 120, ppid: 1, rss: 80000, cmd: "/usr/local/bin/claude --resume"},
		{pid: 130, ppid: 1, rss: 70000, cmd: "ocx serve"},
		{pid: 140, ppid: 1, rss: 60000, cmd: "dockerd --host=unix://x"},
		{pid: 200, ppid: 1, rss: 50000, cmd: "/usr/bin/other-a"},
		{pid: 201, ppid: 1, rss: 40000, cmd: "/usr/bin/other-b"},
		{pid: 202, ppid: 1, rss: 30000, cmd: "/usr/bin/other-c"},
		{pid: 203, ppid: 1, rss: 20000, cmd: "/usr/bin/other-d"},
		{pid: 204, ppid: 1, rss: 10000, cmd: "/usr/bin/other-e"},
		{pid: 205, ppid: 1, rss: 9000, cmd: "/usr/bin/other-f"},
		{pid: 206, ppid: 1, rss: 8000, cmd: "/usr/bin/other-g"},
	})
	record, line := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &keys); err != nil {
		t.Fatalf("the line is not JSON: %v\n%s", err, line)
	}
	for _, key := range []string{"at", "mem_available_kb", "swap_used_kb", "psi", "groups", "top"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("the line has no %q key: %s", key, line)
		}
	}
	if record.At != "2026-10-06T12:00:00Z" || record.MemAvailableKB != 1000000 || record.SwapUsedKB != 1500 {
		t.Errorf("at, mem_available_kb, swap_used_kb = %q %d %d", record.At, record.MemAvailableKB, record.SwapUsedKB)
	}
	if record.PSI != (memlogPSI{SomeAvg10: 1.5, SomeAvg60: 0.75, FullAvg10: 0.25, FullAvg60: 0.05}) {
		t.Errorf("psi = %+v", record.PSI)
	}
	want := map[string]int64{"app_server": 300000, "mcp_helpers": 300000, "go": 90000,
		"claude": 80000, "ocx": 70000, "docker": 60000, "other": 167000}
	if !maps.Equal(record.Groups, want) {
		t.Errorf("groups = %v, want %v", record.Groups, want)
	}
	if len(record.Top) != 10 {
		t.Fatalf("top holds %d entries, want 10: %v", len(record.Top), record.Top)
	}
	for i, pid := range []int{100, 101, 102, 110, 120, 130, 140, 200, 201, 202} {
		if record.Top[i].PID != pid {
			t.Errorf("top[%d].pid = %d, want %d", i, record.Top[i].PID, pid)
		}
	}
	for i, group := range []string{"app_server", "mcp_helpers", "mcp_helpers", "go", "claude",
		"ocx", "docker", "other", "other", "other"} {
		if record.Top[i].Group != group {
			t.Errorf("top[%d].group = %q, want %q", i, record.Top[i].Group, group)
		}
	}
	if record.Top[0].Cmd != "/usr/bin/codex app-server --socket x" || record.Top[0].RSSKB != 300000 {
		t.Errorf("top[0] = %+v", record.Top[0])
	}
}

// C2: the memlog section's ordered groups replace the built-in list whole, and the first
// rule that claims a process wins.
func TestMemlogTakesTheConfiguredGroups(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{
		{pid: 5, ppid: 1, rss: 100, cmd: "/usr/bin/mysvc --serve"},
		{pid: 6, ppid: 1, rss: 200, cmd: "/usr/bin/other"},
	})
	cfg.raw = map[string]json.RawMessage{
		"memlog": json.RawMessage(`{"groups":[{"name":"custom","match":["mysvc"]},{"name":"fallback","match":["mysvc","/usr/bin"]}]}`),
	}
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	want := map[string]int64{"custom": 100, "fallback": 200}
	if !maps.Equal(record.Groups, want) {
		t.Errorf("groups = %v, want %v", record.Groups, want)
	}
}

// C4: the go group is matched by the name the process was started with, not by the word
// go anywhere in the command line, and rule order still puts a go under the App Server in
// the App Server's descendant group.
func TestMemlogMatchesTheGoGroupByArgv0(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{
		{pid: 10, ppid: 1, rss: 100, cmd: "go test ./..."},
		{pid: 11, ppid: 1, rss: 200, cmd: "/usr/bin/go test ./..."},
		{pid: 12, ppid: 1, rss: 400, cmd: "mongod --dbpath /data/go"},
		{pid: 13, ppid: 1, rss: 800, cmd: "echo go"},
		{pid: 20, ppid: 1, rss: 1000, cmd: "/usr/bin/codex app-server --socket x"},
		{pid: 21, ppid: 20, rss: 2000, cmd: "go test ./..."},
	})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	want := map[string]int64{"go": 300, "other": 1200, "app_server": 1000, "mcp_helpers": 2000}
	if !maps.Equal(record.Groups, want) {
		t.Errorf("groups = %v, want %v", record.Groups, want)
	}
	if len(record.Top) != 6 {
		t.Fatalf("top holds %d entries, want 6: %v", len(record.Top), record.Top)
	}
	for i, group := range []string{"mcp_helpers", "app_server", "other", "other", "go", "go"} {
		if record.Top[i].Group != group {
			t.Errorf("top[%d].group = %q, want %q", i, record.Top[i].Group, group)
		}
	}
}

// C3: the recorded cmd masks a credential-shaped value in each of the three shapes the
// issue names.
func TestMemlogRedactsCredentialsInTheRecordedCommand(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{
		{pid: 7, ppid: 1, rss: 4096, cmd: "/usr/bin/mysvc --api-key sk-x TOKEN=abc --password=hunter2"},
	})
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if len(record.Top) != 1 {
		t.Fatalf("top holds %d entries, want 1", len(record.Top))
	}
	cmd := record.Top[0].Cmd
	for _, secret := range []string{"sk-x", "abc", "hunter2"} {
		if strings.Contains(cmd, secret) {
			t.Errorf("the recorded cmd %q still carries %q", cmd, secret)
		}
	}
	for _, masked := range []string{"--api-key ***", "TOKEN=***", "--password=***"} {
		if !strings.Contains(cmd, masked) {
			t.Errorf("the recorded cmd %q does not mask %q", cmd, masked)
		}
	}
	if !strings.Contains(cmd, "/usr/bin/mysvc") {
		t.Errorf("the recorded cmd %q lost its program name", cmd)
	}
}

// C3: each of the three argument shapes the issue names is masked: NAME=VALUE,
// --NAME=VALUE, and the token after a bare --NAME or -NAME. The mask looks at the
// argument name only, so an unrelated argument and a bare name with no value are kept.
func TestMemlogRedactsEachCredentialShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		want string
	}{
		{"name=value", "/usr/bin/svc TOKEN=abc", "/usr/bin/svc TOKEN=***"},
		{"long name=value", "/usr/bin/svc --api-key=sk-x", "/usr/bin/svc --api-key=***"},
		{"long name then value", "/usr/bin/svc --password hunter2", "/usr/bin/svc --password ***"},
		{"short name then value", "/usr/bin/svc -token plain-y", "/usr/bin/svc -token ***"},
		{"mixed case name", "/usr/bin/svc Api_Key=sk-z", "/usr/bin/svc Api_Key=***"},
		{"unrelated arguments are kept", "/usr/bin/svc --socket /run/x --dbpath /data/go",
			"/usr/bin/svc --socket /run/x --dbpath /data/go"},
		{"a bare name with no value", "/usr/bin/svc --password", "/usr/bin/svc --password"},
		{"no argument at all", "/usr/bin/svc", "/usr/bin/svc"},
	} {
		if got := memlogRedactCommand(tc.cmd); got != tc.want {
			t.Errorf("%s: %q became %q, want %q", tc.name, tc.cmd, got, tc.want)
		}
	}
}

// C3: the mask is applied before the 120-rune cut, so a long value cannot push the mask
// out of the record and the masked line still fits the limit.
func TestMemlogRedactsBeforeItCuts(t *testing.T) {
	cmd := "/usr/bin/svc --token " + strings.Repeat("s", 400) + " --socket x"
	got := memlogRedactCommand(cmd)
	if runes := []rune(got); len(runes) > memlogCommandLimit {
		t.Errorf("the masked line is %d runes, want at most %d", len(runes), memlogCommandLimit)
	}
	if strings.Contains(got, "ssss") {
		t.Errorf("the masked line still carries the value: %q", got)
	}
	if !strings.HasPrefix(got, "/usr/bin/svc --token ***") {
		t.Errorf("the masked line reads %q", got)
	}
}

// C3: the group assignment reads the original command line, so a group that matches a
// value the record masks still claims the process.
func TestMemlogClassifiesOnTheOriginalCommandLine(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 8, ppid: 1, rss: 4096, cmd: "/usr/bin/thing --token hunter2"}})
	cfg.raw = map[string]json.RawMessage{
		"memlog": json.RawMessage(`{"groups":[{"name":"secret_holder","match":["hunter2"]}]}`),
	}
	record, _ := memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	if want := (map[string]int64{"secret_holder": 4096}); !maps.Equal(record.Groups, want) {
		t.Errorf("groups = %v, want %v", record.Groups, want)
	}
	if len(record.Top) != 1 || record.Top[0].Cmd != "/usr/bin/thing --token ***" {
		t.Errorf("top = %+v", record.Top)
	}
}

// C5: an XDG_STATE_HOME inherited from outside the test keeps its bytes, and the sample
// goes to the state path the test pointed the environment at.
func TestMemlogKeepsAnInheritedXDGStateHomeUntouched(t *testing.T) {
	external := t.TempDir() // stands in for an XDG_STATE_HOME inherited from the host
	t.Setenv("XDG_STATE_HOME", external)
	_, cfg := memlogState(t)
	externalFile := filepath.Join(external, "crw", "manage", "memlog", "20261006.jsonl")
	if err := os.MkdirAll(filepath.Dir(externalFile), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte("seed\n")
	if err := os.WriteFile(externalFile, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 9, ppid: 1, rss: 4096, cmd: "/usr/bin/thing"}})
	memlogRunOnce(t, cfg, root, time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC))
	after, err := os.ReadFile(externalFile)
	if err != nil {
		t.Fatalf("the file under the inherited state directory: %v", err)
	}
	if !bytes.Equal(seed, after) {
		t.Errorf("the file under the inherited state directory grew from %d to %d bytes", len(seed), len(after))
	}
	if _, err := os.Stat(filepath.Join(memlogSampleDir(cfg.StateDir), "20261006.jsonl")); err != nil {
		t.Errorf("the sample is not under the test state path: %v", err)
	}
}

// A memlog section that is present but malformed is refused instead of silently falling
// back to the built-in groups.
func TestMemlogRefusesAMalformedSection(t *testing.T) {
	_, cfg := memlogState(t)
	cfg.raw = map[string]json.RawMessage{"memlog": json.RawMessage(`{"groups":"nope"}`)}
	root := t.TempDir()
	memlogWriteTree(t, root, nil)
	var out, errOut strings.Builder
	env := memlogEnv(memlogFixedClock(time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)), &out, &errOut)
	if code := memlogRunWith(context.Background(), env, cfg, []string{"--once"}, memlogNewProcSampler(root)); code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "memlog section") {
		t.Errorf("stderr %q does not name the section", errOut.String())
	}
	if _, err := os.Stat(memlogSampleDir(cfg.StateDir)); !os.IsNotExist(err) {
		t.Errorf("the refused run wrote a sample directory: %v", err)
	}
}

// C1: when the UTC date changes, the next sample goes to a new file.
func TestMemlogWritesANewFileWhenTheUTCDateChanges(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &memlogClock{cancel: cancel, times: []time.Time{
		time.Date(2026, 10, 6, 23, 59, 59, 0, time.UTC),
		time.Date(2026, 10, 7, 0, 0, 1, 0, time.UTC),
	}}
	var out, errOut strings.Builder
	env := memlogEnv(clock.now, &out, &errOut)
	if code := memlogRunWith(ctx, env, cfg, []string{"--interval", "1ms"}, memlogNewProcSampler(root)); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	dir := memlogSampleDir(cfg.StateDir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("the directory holds %v (%v), want the two dated files", entries, err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		var record memlogRecord
		if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &record) != nil {
			t.Fatalf("%s holds %q", entry.Name(), lines)
		}
		if want := entry.Name()[:4] + "-" + entry.Name()[4:6] + "-" + entry.Name()[6:8]; !strings.HasPrefix(record.At, want) {
			t.Errorf("%s holds a line stamped %q", entry.Name(), record.At)
		}
	}
}

// C1: --once writes one line and exits; a second run appends a second line.
func TestMemlogOnceWritesOneLineAndExits(t *testing.T) {
	_, cfg := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 9, ppid: 1, rss: 4096, cmd: "/usr/bin/thing"}})
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for run := 1; run <= 2; run++ {
		var out, errOut strings.Builder
		env := memlogEnv(memlogFixedClock(at), &out, &errOut)
		if code := memlogRunWith(context.Background(), env, cfg, []string{"--once"}, memlogNewProcSampler(root)); code != 0 {
			t.Fatalf("run %d: exit %d: %s", run, code, errOut.String())
		}
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("run %d: stdout %q, stderr %q", run, out.String(), errOut.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(memlogSampleDir(cfg.StateDir), "20261006.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the file holds %q, want the two lines of the two runs", lines)
	}
	var record memlogRecord
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	if record.At != "2026-10-06T12:00:00Z" || record.MemAvailableKB != 1000000 || record.SwapUsedKB != 1500 ||
		record.PSI.SomeAvg10 != 1.5 || record.PSI.FullAvg60 != 0.05 || len(record.Top) != 1 ||
		record.Top[0].RSSKB != 4096 || record.Top[0].Cmd != "/usr/bin/thing" {
		t.Errorf("the line is %+v", record)
	}
}

// --interval takes seconds and a duration, refuses a non-positive value, an unknown flag
// and a stray argument are usage errors, and --help writes the usage.
func TestMemlogFlagsAndUsage(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"30", 30 * time.Second}, {"1m", time.Minute}, {"0.5", 500 * time.Millisecond}} {
		if got, err := memlogParseInterval(tc.value); err != nil || got != tc.want {
			t.Errorf("--interval %s = %v (%v), want %v", tc.value, got, err, tc.want)
		}
	}
	for _, value := range []string{"0", "-1", "nope"} {
		if got, err := memlogParseInterval(value); err == nil {
			t.Errorf("--interval %s was accepted as %v", value, got)
		}
	}
	_, cfg := memlogState(t)
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"unknown flag", []string{"--bogus"}, usageExit},
		{"stray argument", []string{"30"}, usageExit},
		{"help", []string{"--help"}, 0},
	} {
		var out, errOut strings.Builder
		env := memlogEnv(time.Now, &out, &errOut)
		if code := memlogRunWith(context.Background(), env, cfg, tc.args, memlogNewProcSampler(t.TempDir())); code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.code)
		}
		if got := out.String() + errOut.String(); !strings.Contains(got, "usage: crw manage memlog") {
			t.Errorf("%s: the usage is missing from %q", tc.name, got)
		}
	}
}
