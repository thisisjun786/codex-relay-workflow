package doctor

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// Surface is one row of the closed retention-scan surface (docs/port/cutover.md "Retention
// scan surface"). Adding a surface is a plan change, not a scan option.
type Surface struct {
	Row  int
	Name string
}

// Surfaces is the closed list, in the document's order.
var Surfaces = []Surface{
	{1, "Stop-event claims without an outcome"},
	{2, "hook journal rows younger than twice the longest configured hook timeout"},
	{3, "alive processes recorded in daemon.json"},
	{4, "crw-*.json settings records"},
	{5, "cached plugin wiring command strings"},
	{6, "alive holders of managed-start locks"},
	{7, "resumable Codex threads"},
	{8, "the crw-stop-hook.py launcher copy"},
	{9, "hooks.json command strings"},
	{10, "config.toml mcp_servers commands"},
	{11, "the owned pointer's target"},
}

// RetentionOptions are the inputs of one scan.
type RetentionOptions struct {
	Env         scope.Env
	CodexHome   string
	Destination string
	StateRoot   string
	Now         func() time.Time
	// Proc is the procfs root (a seam for tests); "" is /proc.
	Proc string
}

// DefaultHookTimeout is the plugin's declared Stop hook timeout, used when nothing configures one.
const DefaultHookTimeout = 10.0

// SettingsKeys are the keys of a crw-*.json record that name something to execute.
var SettingsKeys = []string{"relayExecutable", "bridgeExecutable", "adapterEntryPoint", "adapterInterpreter", "interpreterPath", "command", "args"}

type scan struct {
	o          RetentionOptions
	pointer    string
	references []any
	holds      []any
	unreadable []string
	surfaces   []any
	timeouts   []float64
}

func (s *scan) reference(row int, source, fieldName string, e Executable, extra ...record.Object) {
	o := Object{{Key: "row", Value: int64(row)}, {Key: "surface", Value: Surfaces[row-1].Name}, {Key: "source", Value: source}, {Key: "field", Value: fieldName}}
	o = append(o, e.Object()...)
	for _, more := range extra {
		o = append(o, more...)
	}
	s.references = append(s.references, o)
}

func (s *scan) hold(row int, source string, fields record.Object) {
	o := Object{{Key: "row", Value: int64(row)}, {Key: "surface", Value: Surfaces[row-1].Name}, {Key: "source", Value: source}}
	s.holds = append(s.holds, append(o, fields...))
}

func (s *scan) surface(row int, scanned bool, examined int, detail string) {
	s.surfaces = append(s.surfaces, Object{{Key: "row", Value: int64(row)}, {Key: "surface", Value: Surfaces[row-1].Name}, {Key: "scanned", Value: scanned}, {Key: "examined", Value: int64(examined)}, {Key: "detail", Value: detail}})
}

func (s *scan) unread(what string, err error) {
	s.unreadable = append(s.unreadable, what+": "+store.PythonOSError(err))
}

// RetentionScan is `crw doctor retention-scan --json`: every reference to a Python interpreter,
// venv or .py path that a live or resumable task could still spawn, found by resolving each
// executable reference (through the owned pointer and every link) and classifying what it
// resolves to, never by matching text; and every live hold (a Stop-event claim without an
// outcome, a recent journal row). It writes nothing.
func RetentionScan(ctx context.Context, o RetentionOptions) Object {
	if o.CodexHome == "" {
		o.CodexHome = CodexHome(o.Env)
	}
	if o.Destination == "" {
		o.Destination = DefaultDestination(o.Env)
	}
	if o.StateRoot == "" {
		o.StateRoot = scope.DefaultStateRoot(o.Env)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Proc == "" {
		o.Proc = "/proc"
	}
	s := &scan{o: o, pointer: pointer.Path(o.Destination)}
	settings := s.settingsRecords() // row 4, and the journal root and timeout row 2 needs
	s.pluginCache()                 // row 5, before row 2 so its hook timeouts count
	s.userHooks()                   // row 9, likewise
	s.claims()                      // row 1
	s.journal(settings)             // row 2
	daemonPids := s.daemons()       // row 3
	s.lockHolders(daemonPids)       // row 6
	s.surface(7, false, 0, "Codex threads are listed through the App Server (crw bridge list_threads) and judged against the host's turn-command cache lifetime, which todo 43 records on codex-cli 0.154.0; this command reads neither, so the scan is incomplete until todo 43 adds this row")
	s.launcherCopy()  // row 8
	s.configToml(ctx) // row 10
	s.pointerTarget() // row 11
	byRow := func(list []any) func(i, j int) bool {
		return func(i, j int) bool {
			return record.Get(list[i].(Object), "row").(int64) < record.Get(list[j].(Object), "row").(int64)
		}
	}
	sort.SliceStable(s.surfaces, byRow(s.surfaces))
	sort.SliceStable(s.references, byRow(s.references))
	sort.SliceStable(s.holds, byRow(s.holds))
	var unscanned []any
	for _, one := range s.surfaces {
		if record.Get(one.(Object), "scanned") == false {
			unscanned = append(unscanned, "row "+scope.PyStr(record.Get(one.(Object), "row"))+": "+scope.PyStr(record.Get(one.(Object), "detail")))
		}
	}
	longest := DefaultHookTimeout
	for _, t := range s.timeouts {
		longest = max(longest, t)
	}
	clear := len(s.references) == 0 && len(s.holds) == 0 && len(unscanned) == 0 && len(s.unreadable) == 0
	return Object{
		{Key: "command", Value: "doctor retention-scan"},
		{Key: "scanVersion", Value: int64(1)},
		{Key: "codexHome", Value: o.CodexHome},
		{Key: "stateRoot", Value: o.StateRoot},
		{Key: "pointer", Value: s.pointer},
		{Key: "longestHookTimeoutSeconds", Value: longest},
		{Key: "pythonReferences", Value: nonNil(s.references)},
		{Key: "liveHolds", Value: nonNil(s.holds)},
		{Key: "surfaces", Value: nonNil(s.surfaces)},
		{Key: "unscanned", Value: nonNil(unscanned)},
		{Key: "unreadable", Value: strs(s.unreadable)},
		{Key: "clear", Value: clear},
		{Key: "note", Value: "A Python interpreter, venv, .py entry point or python3 -c launcher that any live or resumable task can still spawn is retained (docs/port/cutover.md Retention). References are found by resolving each executable through the owned pointer and every link and classifying what it resolves to, never by matching text. A live hold is a turn in flight whose command this scan cannot see. Removal waits until clear is true: no reference, no hold, no unscanned row and nothing unreadable."},
	}
}

func nonNil(values []any) []any {
	if values == nil {
		return []any{}
	}
	return values
}

func (s *scan) readJSON(path, what string) (Object, bool) {
	read := reading.ReadJSON(path, what, nil, nil)
	switch {
	case read.State == reading.Absent:
		return nil, false
	case !read.OK():
		s.unreadable = append(s.unreadable, path+": "+read.Detail)
		return nil, false
	}
	value, ok := read.Value.(Object)
	if !ok {
		s.unreadable = append(s.unreadable, path+": not a JSON object")
	}
	return value, ok
}

// settingsRecords is row 4. It returns the completion-hook settings for row 2.
func (s *scan) settingsRecords() Object {
	matches, err := filepath.Glob(filepath.Join(globEscape(s.o.CodexHome), "crw-*.json"))
	if err != nil {
		s.unread(s.o.CodexHome, err)
	}
	sort.Strings(matches)
	var completion Object
	for _, path := range matches {
		value, ok := s.readJSON(path, filepath.Base(path))
		if !ok {
			continue
		}
		if filepath.Base(path) == "crw-completion-hook.json" {
			completion = value
			if seconds, ok := number(record.Get(value, "timeoutSeconds")); ok {
				s.timeouts = append(s.timeouts, seconds)
			}
		}
		for _, key := range SettingsKeys {
			for i, text := range words(record.Get(value, key)) {
				name := key
				if _, list := record.Get(value, key).([]any); list {
					name = key + "[" + strconv.Itoa(i) + "]"
				}
				if e := Classify(text, "", s.pointer); e.Python {
					s.reference(4, path, name, e)
				}
			}
		}
	}
	s.surface(4, true, len(matches), "every crw-*.json record in "+s.o.CodexHome+", each executable resolved through links")
	return completion
}

// words is a string value, or each string in a list value.
func words(v any) []string {
	switch value := v.(type) {
	case string:
		if value != "" {
			return []string{value}
		}
	case []any:
		var out []string
		for _, item := range value {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func globEscape(path string) string {
	var b strings.Builder
	for _, r := range path {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// commandWords classifies the words of one command line: a word naming a Python interpreter,
// a .py path, or a path (or a first word found on PATH) that resolves to Python is a reference.
func (s *scan) commandWords(row int, source, fieldName, line, base string, first bool) {
	for i, word := range ShellWords(line) {
		if strings.ContainsAny(word, "\n") {
			continue // a -c program, not something executed by name
		}
		var e Executable
		switch {
		case strings.Contains(word, "$"):
			if !strings.HasSuffix(word, ".py") {
				continue
			}
			e = Executable{Value: word, Kind: KindPythonScript, Python: true, Detail: "names a .py file through a variable this scan does not expand"}
		case strings.Contains(word, "/") || strings.HasSuffix(word, ".py"):
			e = Classify(word, base, s.pointer)
		case PythonName(word):
			e = Executable{Value: word, Kind: KindPythonInterpreter, Python: true, Detail: "names a Python interpreter"}
			if found, err := lookPath(word, s.o.Env.Get("PATH")); err == nil {
				e.Resolves = found
			}
		case i == 0 && first:
			found, err := lookPath(word, s.o.Env.Get("PATH"))
			if err != nil {
				continue
			}
			e = Classify(found, "", s.pointer)
			e.Value = word
		default:
			continue
		}
		if e.Python {
			s.reference(row, source, fieldName, e)
		}
	}
}

func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

// hookCommands walks a hooks document ({"hooks": {Event: [{"hooks": [{"command", "timeout"}]}]}}).
func (s *scan) hookCommands(row int, path string, document Object, base string) int {
	count := 0
	events, _ := record.Get(document, "hooks").(Object)
	for _, event := range events {
		groups, _ := event.Value.([]any)
		for g, group := range groups {
			hooks, _ := record.Get(asObject(group), "hooks").([]any)
			for h, hook := range hooks {
				one := asObject(hook)
				command, _ := record.Get(one, "command").(string)
				if command == "" {
					continue
				}
				count++
				if seconds, ok := number(record.Get(one, "timeout")); ok && event.Key == "Stop" {
					s.timeouts = append(s.timeouts, seconds)
				}
				s.commandWords(row, path, "hooks."+event.Key+"["+strconv.Itoa(g)+"].hooks["+strconv.Itoa(h)+"].command", command, base, true)
			}
		}
	}
	return count
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

// mcpServers walks an MCP document ({"mcpServers": {name: {"command", "args"}}}).
func (s *scan) mcpServers(row int, path string, servers Object, base string) int {
	for _, server := range servers {
		entry := asObject(server.Value)
		if command, ok := record.Get(entry, "command").(string); ok && command != "" {
			s.commandWords(row, path, "mcpServers."+server.Key+".command", command, base, true)
		}
		for i, arg := range words(record.Get(entry, "args")) {
			s.commandWords(row, path, "mcpServers."+server.Key+".args["+strconv.Itoa(i)+"]", arg, base, false)
		}
	}
	return len(servers)
}

// pluginCache is row 5: every cached version's hook and MCP declarations.
func (s *scan) pluginCache() {
	root := filepath.Join(s.o.CodexHome, "plugins", "cache", "crw", "crw")
	versions, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		s.surface(5, true, 0, "no cached plugin version exists at "+root)
		return
	}
	if err != nil {
		s.unread(root, err)
		s.surface(5, true, 0, "the plugin cache could not be listed")
		return
	}
	count := 0
	for _, version := range versions {
		base := filepath.Join(root, version.Name())
		hooks, _ := filepath.Glob(filepath.Join(globEscape(base), "wiring", "hooks", "*.json"))
		sort.Strings(hooks)
		for _, path := range hooks {
			if document, ok := s.readJSON(path, "a cached hook declaration"); ok {
				count += s.hookCommands(5, path, document, base)
			}
		}
		for _, path := range []string{filepath.Join(base, "wiring", "mcp.json"), filepath.Join(base, ".mcp.json")} {
			if document, ok := s.readJSON(path, "a cached MCP declaration"); ok {
				servers, _ := record.Get(document, "mcpServers").(Object)
				count += s.mcpServers(5, path, servers, base)
			}
		}
	}
	s.surface(5, true, count, "every hook and MCP command declared by a cached plugin version under "+root)
}

// userHooks is row 9.
func (s *scan) userHooks() {
	path := filepath.Join(s.o.CodexHome, "hooks.json")
	count := 0
	if document, ok := s.readJSON(path, "hooks.json"); ok {
		count = s.hookCommands(9, path, document, s.o.CodexHome)
	}
	s.surface(9, true, count, "every command in "+path)
}

// claims is row 1: a Stop-event claim whose outcome file is absent is a turn whose hook may
// still be running (or died), so whatever its command was is held.
func (s *scan) claims() {
	directory := filepath.Join(s.o.CodexHome, "crw-completion-hook", "stop-events")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		s.surface(1, true, 0, "no Stop-event claims exist at "+directory)
		return
	}
	if err != nil {
		s.unread(directory, err)
		s.surface(1, true, 0, "the claims could not be listed")
		return
	}
	count := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		claim, ok := s.readJSON(path, "a Stop-event claim")
		if !ok {
			continue
		}
		count++
		key, _ := record.Get(claim, "eventKey").(string)
		if key == "" {
			key = strings.TrimSuffix(entry.Name(), ".json")
		}
		claimed := asObject(record.Get(claim, "claimedBy"))
		root, _ := record.Get(claimed, "journalRoot").(string)
		fields := record.Object{{Key: "eventKey", Value: key}, {Key: "claimedAt", Value: record.Get(claim, "claimedAt")}, {Key: "pid", Value: record.Get(claimed, "pid")}}
		if root == "" {
			s.hold(1, path, append(fields, record.Object{{Key: "outcome", Value: nil}, {Key: "detail", Value: "the claim names no journal root, so its outcome cannot be looked for"}}...))
			continue
		}
		outcome := filepath.Join(root, "accepted", key+".outcome.json")
		_, err := os.Lstat(outcome)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			s.hold(1, path, append(fields, record.Object{{Key: "outcome", Value: outcome}, {Key: "detail", Value: "the claim has no outcome file, so the turn that claimed it may still be running its hook command"}}...))
		default:
			s.unread(outcome, err)
		}
	}
	s.surface(1, true, count, "every claim in "+directory+", each judged by <claimedBy.journalRoot>/accepted/<eventKey>.outcome.json")
}

// journal is row 2: a row younger than twice the longest configured hook timeout is a turn
// whose hook may still be running.
func (s *scan) journal(settings Object) {
	root, _ := record.Get(settings, "journalRoot").(string)
	if root == "" {
		root = filepath.Join(s.o.CodexHome, "crw-completion-hook", "journal")
	}
	longest := DefaultHookTimeout
	for _, t := range s.timeouts {
		longest = max(longest, t)
	}
	window := time.Duration(2 * longest * float64(time.Second))
	now := s.o.Now().UTC()
	count := 0
	seen := map[string]bool{}
	for _, day := range []time.Time{now, now.Add(-window)} {
		name := day.Format("20060102")
		if seen[name] {
			continue
		}
		seen[name] = true
		directory := filepath.Join(root, name)
		entries, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			s.unread(directory, err)
			continue
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(directory, entry.Name())
			info, err := entry.Info()
			if err != nil {
				s.unread(path, err)
				continue
			}
			written := info.ModTime()
			row := reading.ReadJSON(path, "a journal row", nil, nil)
			if value, ok := row.Value.(Object); ok {
				if at, ok := record.Get(value, "at").(string); ok {
					if parsed, err := time.Parse("2006-01-02T15:04:05Z", at); err == nil {
						written = parsed
					}
				}
			}
			count++
			if now.Sub(written) < window {
				var configuration any
				if value, ok := row.Value.(Object); ok {
					configuration = record.Get(value, "configuration")
				}
				s.hold(2, path, record.Object{{Key: "at", Value: written.UTC().Format("2006-01-02T15:04:05Z")}, {Key: "configuration", Value: configuration}, {Key: "detail", Value: "a Stop hook ran within the last " + strconv.FormatFloat(window.Seconds(), 'f', -1, 64) + " s, so the turn that ran it may still hold its command"}})
			}
		}
	}
	s.surface(2, true, count, "journal rows under "+root+" for today and the day the window reaches back to")
}

// stateDirectories are the relay state root, every directory under it, and an explicit
// CODEX_SESSION_RELAY_STATE.
func (s *scan) stateDirectories() []string {
	directories := []string{s.o.StateRoot}
	if entries, err := os.ReadDir(s.o.StateRoot); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				directories = append(directories, filepath.Join(s.o.StateRoot, entry.Name()))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		s.unread(s.o.StateRoot, err)
	}
	if explicit := s.o.Env.Get(scope.StateEnv); explicit != "" && !contains(directories, explicit) {
		directories = append(directories, explicit)
	}
	return directories
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// process is what an alive pid is running: its executable and argv, and whether that is Python.
func (s *scan) process(pid int) (Executable, []any, bool) {
	base := filepath.Join(s.o.Proc, strconv.Itoa(pid))
	exe, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return Executable{}, nil, false
	}
	raw, _ := os.ReadFile(filepath.Join(base, "cmdline"))
	var argv []any
	var python bool
	for i, word := range strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00") {
		if word == "" {
			continue
		}
		argv = append(argv, word)
		if (i == 0 && PythonName(word)) || strings.HasSuffix(word, ".py") {
			python = true
		}
	}
	e := Executable{Value: exe, Resolves: exe, Kind: KindNative, Detail: "the process's executable"}
	if PythonName(exe) || python {
		e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a process running a Python interpreter or a .py program"
	}
	return e, argv, true
}

// alive is whether pid is the process a record describes: same start time (and boot).
func alive(pid int, ticks any, boot any) bool {
	if pid <= 0 {
		return false
	}
	now := service.StartTicks(pid)
	if now == nil {
		return false
	}
	if ticks != nil && scope.PyStr(ticks) != scope.PyStr(now) {
		return false
	}
	if boot != nil && service.BootID() != nil && scope.PyStr(boot) != scope.PyStr(service.BootID()) {
		return false
	}
	return true
}

// daemons is row 3. It returns every alive recorded pid for row 6 to exclude.
func (s *scan) daemons() map[int]bool {
	pids := map[int]bool{}
	count := 0
	for _, directory := range s.stateDirectories() {
		path := filepath.Join(directory, "daemon.json")
		document, ok := s.readJSON(path, "daemon.json")
		if !ok {
			continue
		}
		count++
		for _, key := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
			n, ok := number(record.Get(document, key[0]))
			if !ok || !alive(int(n), record.Get(document, key[1]), record.Get(document, "bootId")) {
				continue
			}
			pid := int(n)
			pids[pid] = true
			e, argv, ok := s.process(pid)
			if ok && e.Python {
				s.reference(3, path, key[0], e, record.Object{{Key: "pid", Value: int64(pid)}, {Key: "argv", Value: nonNil(argv)}})
			}
		}
	}
	s.surface(3, true, count, "daemon.json in the relay state root and every scope under it; a pid counts only while its start time matches the record")
	return pids
}

// lockHolders is row 6: /proc/locks holders of every managed-start lock, less row 3's pids.
func (s *scan) lockHolders(exclude map[int]bool) {
	type inode struct {
		major, minor uint32
		ino          uint64
	}
	files := map[inode]string{}
	for _, directory := range s.stateDirectories() {
		matches, _ := filepath.Glob(filepath.Join(globEscape(directory), "managed-start-*.lock"))
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil {
				s.unread(path, err)
				continue
			}
			sys, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			files[inode{unix.Major(uint64(sys.Dev)), unix.Minor(uint64(sys.Dev)), sys.Ino}] = path
		}
	}
	if len(files) == 0 {
		s.surface(6, true, 0, "no managed-start lock exists under the relay state root")
		return
	}
	locks, err := os.Open(filepath.Join(s.o.Proc, "locks"))
	if err != nil {
		s.unread(filepath.Join(s.o.Proc, "locks"), err)
		s.surface(6, false, len(files), "the kernel's lock table could not be read, so the holders of "+strconv.Itoa(len(files))+" managed-start locks are unknown")
		return
	}
	defer locks.Close()
	holders := map[int]string{}
	lines := bufio.NewScanner(locks)
	for lines.Scan() {
		fieldsList := strings.Fields(lines.Text())
		if len(fieldsList) < 6 || fieldsList[1] == "->" || fieldsList[1] != "FLOCK" {
			continue
		}
		pid, err := strconv.Atoi(fieldsList[4])
		if err != nil {
			continue
		}
		parts := strings.Split(fieldsList[5], ":")
		if len(parts) != 3 {
			continue
		}
		major, _ := strconv.ParseUint(parts[0], 16, 32)
		minor, _ := strconv.ParseUint(parts[1], 16, 32)
		ino, _ := strconv.ParseUint(parts[2], 10, 64)
		if path, ok := files[inode{uint32(major), uint32(minor), ino}]; ok {
			holders[pid] = path
		}
	}
	pids := make([]int, 0, len(holders))
	for pid := range holders {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if exclude[pid] {
			continue
		}
		if e, argv, ok := s.process(pid); ok && e.Python {
			s.reference(6, holders[pid], "holder", e, record.Object{{Key: "pid", Value: int64(pid)}, {Key: "argv", Value: nonNil(argv)}})
		}
	}
	s.surface(6, true, len(files), "the /proc/locks holders of every managed-start lock, less the pids row 3 reports")
}

// launcherCopy is row 8: <CODEX_HOME>/crw-stop-hook.py is a .py launcher by definition.
func (s *scan) launcherCopy() {
	path := filepath.Join(s.o.CodexHome, "crw-stop-hook.py")
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		s.reference(8, path, "file", Classify(path, "", s.pointer))
		s.surface(8, true, 1, "the launcher copy the cached Python Stop bootstrap falls back to")
	case errors.Is(err, os.ErrNotExist):
		s.surface(8, true, 0, "no launcher copy exists at "+path)
	default:
		s.unread(path, err)
		s.surface(8, true, 0, "whether a launcher copy exists could not be established")
	}
}

// configToml is row 10: the command and args of every mcp_servers table.
func (s *scan) configToml(_ context.Context) {
	path := filepath.Join(s.o.CodexHome, "config.toml")
	read := reading.ReadText(path, "config.toml")
	switch {
	case read.State == reading.Absent:
		s.surface(10, true, 0, "no config.toml exists at "+path)
		return
	case !read.OK():
		s.unreadable = append(s.unreadable, path+": "+read.Detail)
		s.surface(10, true, 0, "config.toml could not be read")
		return
	}
	var document map[string]any
	if _, err := toml.Decode(read.Value.(string), &document); err != nil {
		s.unreadable = append(s.unreadable, path+": "+err.Error())
		s.surface(10, true, 0, "config.toml could not be parsed")
		return
	}
	servers, _ := document["mcp_servers"].(map[string]any)
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, _ := servers[name].(map[string]any)
		if command, ok := entry["command"].(string); ok && command != "" {
			s.commandWords(10, path, "mcp_servers."+name+".command", command, s.o.CodexHome, true)
		}
		if args, ok := entry["args"].([]any); ok {
			for i, arg := range args {
				if text, ok := arg.(string); ok {
					s.commandWords(10, path, "mcp_servers."+name+".args["+strconv.Itoa(i)+"]", text, s.o.CodexHome, false)
				}
			}
		}
	}
	s.surface(10, true, len(names), "every mcp_servers table in "+path)
}

// pointerTarget is row 11: the owned pointer naming a venv is itself a reference.
func (s *scan) pointerTarget() {
	read := pointer.Read(s.pointer)
	switch read.State {
	case pointer.Unreachable:
		s.unreadable = append(s.unreadable, s.pointer+": "+read.Detail)
		s.surface(11, true, 0, read.Detail)
	case pointer.Link:
		if e := Classify(s.pointer, "", s.pointer); e.Python {
			s.reference(11, s.pointer, "target", e)
		}
		s.surface(11, true, 1, read.Detail)
	default:
		s.surface(11, true, 0, read.Detail)
	}
}

// ShellWords splits a command line the way sh would split its words: single quotes literal,
// double quotes with backslash escapes, a backslash escaping outside quotes. Expansions are
// left as written.
func ShellWords(line string) []string {
	var out []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'':
			inWord = true
			end := strings.IndexByte(line[i+1:], '\'')
			if end < 0 {
				word.WriteString(line[i+1:])
				i = len(line)
				continue
			}
			word.WriteString(line[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) && strings.IndexByte("\"\\$`\n", line[i+1]) >= 0 {
					i++
				}
				word.WriteByte(line[i])
			}
		case c == '\\' && i+1 < len(line):
			inWord = true
			i++
			word.WriteByte(line[i])
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|':
			if inWord {
				out = append(out, word.String())
				word.Reset()
				inWord = false
			}
		default:
			inWord = true
			word.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, word.String())
	}
	return out
}
