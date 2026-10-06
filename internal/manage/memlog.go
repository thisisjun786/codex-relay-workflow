package manage

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	memlogDefaultInterval = 30 * time.Second
	memlogTopCount        = 10
	memlogCommandLimit    = 120
	memlogOtherGroup      = "other"
)

// memlogRecord is one memory sample, appended as one JSON line.
type memlogRecord struct {
	At             string           `json:"at"`
	MemAvailableKB int64            `json:"mem_available_kb"`
	SwapUsedKB     int64            `json:"swap_used_kb"`
	PSI            memlogPSI        `json:"psi"`
	Groups         map[string]int64 `json:"groups"`
	Top            []memlogTopEntry `json:"top"`
}

// memlogPSI is the memory pressure over the last ten and sixty seconds.
type memlogPSI struct {
	SomeAvg10 float64 `json:"some_avg10"`
	SomeAvg60 float64 `json:"some_avg60"`
	FullAvg10 float64 `json:"full_avg10"`
	FullAvg60 float64 `json:"full_avg60"`
}

// memlogTopEntry is one of the largest processes of a sample.
type memlogTopEntry struct {
	PID   int    `json:"pid"`
	RSSKB int64  `json:"rss_kb"`
	Group string `json:"group"`
	Cmd   string `json:"cmd"`
}

// memlogProc is one process as one pass over the tree read it.
type memlogProc struct {
	PID   int
	PPID  int
	RSSKB int64
	Cmd   string
}

// memlogSnapshot is one consistent pass over a procfs tree, before grouping.
type memlogSnapshot struct {
	MemAvailableKB int64
	SwapTotalKB    int64
	SwapFreeKB     int64
	PSI            memlogPSI
	Procs          []memlogProc
}

// memlogSampler reads one snapshot; a test passes a fake tree instead of /proc.
type memlogSampler interface {
	Snapshot() (memlogSnapshot, error)
}

// memlogProcTree reads meminfo, pressure/memory and one directory per process below root.
type memlogProcTree struct{ root string }

func memlogNewProcSampler(root string) memlogSampler { return memlogProcTree{root: root} }

// Snapshot reads the counters and every process; a process that vanishes is skipped.
func (p memlogProcTree) Snapshot() (memlogSnapshot, error) {
	data, err := os.ReadFile(filepath.Join(p.root, "meminfo"))
	if err != nil {
		return memlogSnapshot{}, err
	}
	meminfo := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if fields := strings.Fields(rest); len(fields) > 0 {
			if value, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
				meminfo[key] = value
			}
		}
	}
	snapshot := memlogSnapshot{MemAvailableKB: meminfo["MemAvailable"],
		SwapTotalKB: meminfo["SwapTotal"], SwapFreeKB: meminfo["SwapFree"]}
	pressure, err := os.ReadFile(filepath.Join(p.root, "pressure", "memory"))
	if err != nil {
		return memlogSnapshot{}, err
	}
	for _, line := range strings.Split(string(pressure), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		average := func(name string) float64 {
			for _, field := range fields[1:] {
				if key, value, ok := strings.Cut(field, "="); ok && key == name {
					parsed, _ := strconv.ParseFloat(value, 64)
					return parsed
				}
			}
			return 0
		}
		switch fields[0] {
		case "some":
			snapshot.PSI.SomeAvg10, snapshot.PSI.SomeAvg60 = average("avg10"), average("avg60")
		case "full":
			snapshot.PSI.FullAvg10, snapshot.PSI.FullAvg60 = average("avg10"), average("avg60")
		}
	}
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return memlogSnapshot{}, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if proc, err := memlogReadProc(filepath.Join(p.root, entry.Name()), pid); err == nil {
			snapshot.Procs = append(snapshot.Procs, proc)
		}
	}
	return snapshot, nil
}

// memlogReadProc reads one process: its parent, its memory and its command line.
func memlogReadProc(dir string, pid int) (memlogProc, error) {
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return memlogProc{}, err
	}
	status, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return memlogProc{}, err
	}
	cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return memlogProc{}, err
	}
	// The command name in parentheses may hold spaces and even a ")", so the parent is the
	// field after the last ")" of the line.
	close := strings.LastIndex(string(stat), ")")
	if close < 0 {
		return memlogProc{}, fmt.Errorf("the stat of %d has no command name", pid)
	}
	fields := strings.Fields(string(stat)[close+1:])
	if len(fields) < 2 {
		return memlogProc{}, fmt.Errorf("the stat of %d has no parent", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return memlogProc{}, err
	}
	var rss int64
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			if value := strings.Fields(strings.TrimPrefix(line, "VmRSS:")); len(value) > 0 {
				rss, _ = strconv.ParseInt(value[0], 10, 64)
			}
			break
		}
	}
	return memlogProc{PID: pid, PPID: ppid, RSSKB: rss,
		Cmd: strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))}, nil
}

// memlogGroupRule is one rule of the built-in list: the first rule that claims a process
// names its group; Match lists command-line substrings, DescendantsOf names another group.
type memlogGroupRule struct {
	Name          string
	Match         []string
	DescendantsOf string
}

// memlogDefaultGroups is the built-in list, with no private path or host name in it.
func memlogDefaultGroups() []memlogGroupRule {
	return []memlogGroupRule{
		{Name: "app_server", Match: []string{"app-server"}},
		{Name: "mcp_helpers", DescendantsOf: "app_server"},
		{Name: "go", Match: []string{"/go/", "go-build", ".test"}},
		{Name: "claude", Match: []string{"claude"}},
		{Name: "ocx", Match: []string{"ocx", "opencodex"}},
		{Name: "docker", Match: []string{"docker", "containerd"}},
	}
}

// memlogClassify names the group of every process; a process no rule claims is "other".
func memlogClassify(procs []memlogProc, rules []memlogGroupRule) map[int]string {
	byPID := map[int]memlogProc{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	out := map[int]string{}
	for pass := 0; pass <= len(rules)+1; pass++ {
		changed := false
		for _, p := range procs {
			if name := memlogGroupOf(p, rules, out, byPID); out[p.PID] != name {
				out[p.PID], changed = name, true
			}
		}
		if !changed {
			break
		}
	}
	return out
}

// memlogGroupOf is the first rule that claims the process.
func memlogGroupOf(p memlogProc, rules []memlogGroupRule, out map[int]string, byPID map[int]memlogProc) string {
	for _, rule := range rules {
		if rule.Name == "" {
			continue
		}
		if len(rule.Match) > 0 {
			for _, substring := range rule.Match {
				if substring != "" && strings.Contains(p.Cmd, substring) {
					return rule.Name
				}
			}
			continue
		}
		if rule.DescendantsOf == "" {
			continue
		}
		for pid, hops := p.PPID, 0; pid > 0 && hops < 64; hops++ {
			if out[pid] == rule.DescendantsOf {
				return rule.Name
			}
			pid = byPID[pid].PPID
		}
	}
	return memlogOtherGroup
}

// memlogBuildRecord turns one snapshot into the record: group sums and the ten largest.
func memlogBuildRecord(now time.Time, snapshot memlogSnapshot, rules []memlogGroupRule) memlogRecord {
	groups := memlogClassify(snapshot.Procs, rules)
	sums := map[string]int64{}
	for _, p := range snapshot.Procs {
		sums[groups[p.PID]] += p.RSSKB
	}
	ranked := append([]memlogProc(nil), snapshot.Procs...)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].RSSKB != ranked[j].RSSKB {
			return ranked[i].RSSKB > ranked[j].RSSKB
		}
		return ranked[i].PID < ranked[j].PID
	})
	if len(ranked) > memlogTopCount {
		ranked = ranked[:memlogTopCount]
	}
	top := make([]memlogTopEntry, 0, len(ranked))
	for _, p := range ranked {
		cmd := p.Cmd
		if runes := []rune(cmd); len(runes) > memlogCommandLimit {
			cmd = string(runes[:memlogCommandLimit])
		}
		top = append(top, memlogTopEntry{PID: p.PID, RSSKB: p.RSSKB, Group: groups[p.PID], Cmd: cmd})
	}
	return memlogRecord{At: now.UTC().Format(time.RFC3339Nano), MemAvailableKB: snapshot.MemAvailableKB,
		SwapUsedKB: snapshot.SwapTotalKB - snapshot.SwapFreeKB, PSI: snapshot.PSI, Groups: sums, Top: top}
}

// memlogAppend writes one record as one line of the file of the record's UTC date, so a
// date change opens a new file and the file name and the line never disagree.
func memlogAppend(dir string, now time.Time, record memlogRecord) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	line, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, now.UTC().Format("20060102")+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return "", err
	}
	return path, file.Close()
}

// memlogParseInterval reads a number of seconds or a duration such as 30s.
func memlogParseInterval(value string) (time.Duration, error) {
	interval, err := time.ParseDuration(value)
	if err != nil {
		seconds, secondsErr := strconv.ParseFloat(value, 64)
		if secondsErr != nil {
			return 0, fmt.Errorf("not a number of seconds or a duration: %q", value)
		}
		interval = time.Duration(seconds * float64(time.Second))
	}
	if interval <= 0 {
		return 0, fmt.Errorf("must be positive: %q", value)
	}
	return interval, nil
}

// memlogRunWith is crw manage memlog: it samples through sampler and appends one line per
// sample below the state directory, until --once ends it or ctx is cancelled.
func memlogRunWith(ctx context.Context, e *Env, cfg *Config, args []string, sampler memlogSampler) int {
	interval, once := memlogDefaultInterval, false
	usage := func(w io.Writer) {
		fmt.Fprintln(w, "usage: crw manage memlog [--interval S] [--once]")
		fmt.Fprintln(w, "  --interval S\tthe seconds between samples, a number or a duration such as 30s (default 30)")
		fmt.Fprintln(w, "  --once\tsample once, write one line and exit")
	}
	flags := flag.NewFlagSet("crw manage memlog", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	flags.Func("interval", "the seconds between samples", func(value string) error {
		parsed, err := memlogParseInterval(value)
		if err != nil {
			return err
		}
		interval = parsed
		return nil
	})
	flags.BoolVar(&once, "once", false, "sample once, write one line and exit")
	switch err := flags.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		usage(e.Stdout)
		return 0
	case err != nil:
		usage(e.Stderr)
		fmt.Fprintf(e.Stderr, "crw manage memlog: error: %v\n", err)
		return usageExit
	case flags.NArg() > 0:
		usage(e.Stderr)
		fmt.Fprintf(e.Stderr, "crw manage memlog: error: unexpected argument %q\n", flags.Arg(0))
		return usageExit
	}
	dir, rules := filepath.Join(cfg.StateDir, "memlog"), memlogDefaultGroups()
	for {
		snapshot, err := sampler.Snapshot()
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage memlog: error: %v\n", err)
			return 1
		}
		now := e.Now()
		if _, err := memlogAppend(dir, now, memlogBuildRecord(now, snapshot, rules)); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage memlog: error: %v\n", err)
			return 1
		}
		if once {
			return 0
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0
		case <-timer.C:
		}
	}
}
