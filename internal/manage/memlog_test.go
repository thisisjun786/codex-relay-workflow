package manage

import (
	"context"
	"encoding/json"
	"fmt"
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
func memlogEnv(now func() time.Time, out, errOut *strings.Builder) *Env {
	return &Env{Stdin: strings.NewReader(""), Stdout: out, Stderr: errOut, Getenv: os.Getenv, Now: now}
}

// memlogState points the homes at a temporary tree, so no test reaches a real one.
func memlogState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	return home
}

// C1: when the UTC date changes, the next sample goes to a new file.
func TestMemlogWritesANewFileWhenTheUTCDateChanges(t *testing.T) {
	home := memlogState(t)
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
	if code := memlogRunWith(ctx, env, coreDefaults(env), []string{"--interval", "1ms"}, memlogNewProcSampler(root)); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	dir := filepath.Join(home, ".local", "state", "crw", "manage", "memlog")
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

// C2: --once writes one line and exits; a second run appends a second line.
func TestMemlogOnceWritesOneLineAndExits(t *testing.T) {
	home := memlogState(t)
	root := t.TempDir()
	memlogWriteTree(t, root, []memlogProcSpec{{pid: 9, ppid: 1, rss: 4096, cmd: "/usr/bin/thing"}})
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for run := 1; run <= 2; run++ {
		var out, errOut strings.Builder
		env := memlogEnv(func() time.Time { return at }, &out, &errOut)
		if code := memlogRunWith(context.Background(), env, coreDefaults(env), []string{"--once"}, memlogNewProcSampler(root)); code != 0 {
			t.Fatalf("run %d: exit %d: %s", run, code, errOut.String())
		}
		if out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("run %d: stdout %q, stderr %q", run, out.String(), errOut.String())
		}
	}
	data, err := os.ReadFile(filepath.Join(home, ".local", "state", "crw", "manage", "memlog", "20261006.jsonl"))
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
	home := memlogState(t)
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
		cfg := coreDefaults(env)
		cfg.StateDir = filepath.Join(home, "state")
		if code := memlogRunWith(context.Background(), env, cfg, tc.args, memlogNewProcSampler(t.TempDir())); code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.name, code, tc.code)
		}
		if got := out.String() + errOut.String(); !strings.Contains(got, "usage: crw manage memlog") {
			t.Errorf("%s: the usage is missing from %q", tc.name, got)
		}
	}
}
