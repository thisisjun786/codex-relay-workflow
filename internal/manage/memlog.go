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
	Argv0 string
	// Args is the command line as the kernel kept it, one string per argument. The
	// recorded line is joined from it, so a value the kernel holds as one argument - a
	// passphrase with a space in it, for instance - stays one token and is masked whole.
	Args []string
	Cmd  string
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
	// The name the process was started with is the first NUL-separated field of the
	// command line, kept beside the joined line because a substring search over the whole
	// line cannot tell the program from an argument that happens to spell the same word.
	args := memlogSplitCmdline(cmdline)
	argv0 := ""
	if len(args) > 0 {
		argv0 = filepath.Base(args[0])
	}
	return memlogProc{PID: pid, PPID: ppid, RSSKB: rss, Argv0: argv0, Args: args,
		Cmd: strings.Join(args, " ")}, nil
}

// memlogSplitCmdline splits a NUL-separated /proc/<pid>/cmdline into its arguments. The
// kernel ends the file with a NUL, and a kernel thread has none at all.
func memlogSplitCmdline(cmdline []byte) []string {
	trimmed := strings.TrimSuffix(string(cmdline), "\x00")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\x00")
}

// memlogGroupRule is one rule of the list: the first rule that claims a process names its
// group. Match lists command-line substrings, Argv0 lists the names a process may have
// been started with, DescendantsOf names another group. A rule with Match or Argv0 claims
// a process itself; a rule with only DescendantsOf claims one below a member of that group.
type memlogGroupRule struct {
	Name          string   `json:"name"`
	Match         []string `json:"match"`
	Argv0         []string `json:"argv0"`
	DescendantsOf string   `json:"descendants_of"`
}

// memlogRules is the group list the command runs with: the configuration's memlog section
// when it carries one, the built-in list otherwise. An absent section or an absent groups
// key means the defaults; a section that is present but malformed, an explicitly null list
// included, is an error rather than a silent fallback to the defaults, because a configured
// list is meant to decide the assignment and a run that quietly ignores it reports groups
// the operator did not ask for. An empty list is a real configuration: it replaces the
// built-in list whole, so every process is other.
func memlogRules(cfg *Config) ([]memlogGroupRule, error) {
	var section struct {
		Groups json.RawMessage `json:"groups"`
	}
	if err := cfg.Section("memlog", &section); err != nil {
		return nil, err
	}
	if len(section.Groups) == 0 {
		return memlogDefaultGroups(), nil
	}
	if string(section.Groups) == "null" {
		return nil, errors.New("groups is null; give a list of rules or leave the key out")
	}
	var rules []memlogGroupRule
	if err := json.Unmarshal(section.Groups, &rules); err != nil {
		return nil, fmt.Errorf("groups: %w", err)
	}
	return rules, nil
}

// memlogDefaultGroups is the built-in list, with no private path or host name in it.
func memlogDefaultGroups() []memlogGroupRule {
	return []memlogGroupRule{
		{Name: "app_server", Match: []string{"app-server"}},
		{Name: "mcp_helpers", DescendantsOf: "app_server"},
		{Name: "go", Argv0: []string{"go"}, Match: []string{"/go/", "go-build", ".test"}},
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
		if len(rule.Match) > 0 || len(rule.Argv0) > 0 {
			if memlogRuleClaims(p, rule) {
				return rule.Name
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

// memlogRuleClaims reports whether the rule claims the process: by the name it was started
// with, exactly, or by one of the command-line substrings.
func memlogRuleClaims(p memlogProc, rule memlogGroupRule) bool {
	if p.Argv0 != "" {
		for _, name := range rule.Argv0 {
			if name != "" && p.Argv0 == name {
				return true
			}
		}
	}
	for _, substring := range rule.Match {
		if substring != "" && strings.Contains(p.Cmd, substring) {
			return true
		}
	}
	return false
}

// memlogRedactNames are the argument-name fragments that mark a value as a credential.
var memlogRedactNames = []string{"token", "secret", "password", "passwd",
	"api_key", "api-key", "apikey", "auth", "credential", "private_key"}

// memlogRedacted is what a masked value reads as.
const memlogRedacted = "***"

// memlogRedactCommand masks the value of every credential-shaped argument of a process and
// then cuts the joined line to memlogCommandLimit runes. It works on the arguments the
// kernel kept apart, not on the joined line, so a value that carries a space is masked
// whole. The group assignment keeps the unmasked arguments, so a mask never changes which
// group a process is counted in.
func memlogRedactCommand(args []string) string {
	tokens := append([]string(nil), args...)
	for i := 0; i < len(tokens); i++ {
		name, _, hasValue := strings.Cut(tokens[i], "=")
		switch {
		case hasValue:
			if memlogNamesACredential(name) {
				tokens[i] = name + "=" + memlogRedacted
			}
		case strings.HasPrefix(name, "-") && memlogNamesACredential(name) && i+1 < len(tokens):
			tokens[i+1] = memlogRedacted
			i++
		}
	}
	return memlogShorten(strings.Join(tokens, " "))
}

// memlogShorten keeps the first memlogCommandLimit runes of a line.
func memlogShorten(line string) string {
	if runes := []rune(line); len(runes) > memlogCommandLimit {
		return string(runes[:memlogCommandLimit])
	}
	return line
}

// memlogNamesACredential reports whether an argument name holds one of the fragments.
func memlogNamesACredential(name string) bool {
	lower := strings.ToLower(name)
	for _, fragment := range memlogRedactNames {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
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
		top = append(top, memlogTopEntry{PID: p.PID, RSSKB: p.RSSKB, Group: groups[p.PID],
			Cmd: memlogRedactCommand(p.Args)})
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
	rules, err := memlogRules(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage memlog: error: the memlog section: %v\n", err)
		return 1
	}
	dir := filepath.Join(cfg.StateDir, "memlog")
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
