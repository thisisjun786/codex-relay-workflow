package doctor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
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
	// ScopeRegistry is the relay's production scope registry (a seam for tests); "" is
	// <home>/.codex-session-relay/scopes, home taken from the passwd entry as the relay takes it.
	ScopeRegistry string
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
	timeouts   []stopTimeout
	// stopSettings are the settings documents the retained Stop registrations read (row 2),
	// and claimRoots the journal roots Stop-event claims name.
	stopSettings []stopSettings
	claimRoots   [][2]string // (claim file, journalRoot)
	// listed is how many unreadable entries the rows surfaced so far account for.
	listed int
	// states are the relay state directories rows 3 and 6 read, scopeClaims the scope registry's
	// records row 3 judges, and statesUnknown how many sources of either could not be read.
	states        []string
	scopeClaims   []scopeClaim
	statesUnknown int
	// observe, when set, receives every path the registration rows classify, with the row,
	// source and field naming it (RegisteredInside).
	observe func(row int, source, field, path string, e Executable)
}

// stopTimeout is one configured Stop hook timeout, in seconds, and where it is configured.
type stopTimeout struct {
	seconds float64
	source  string
}

// stopSettings is the settings document one Stop registration reads: path, or why it cannot be
// established.
type stopSettings struct {
	source, field, path, problem string
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

// surface records one row. The rows are read in turn and each surfaces once, so every unreadable
// entry listed since the previous row surfaced is this row's: a row with any is not scanned,
// whatever its reader concluded, because something it should judge was not judged.
func (s *scan) surface(row int, scanned bool, examined int, detail string) {
	if unjudged := len(s.unreadable) - s.listed; unjudged > 0 && scanned {
		scanned, detail = false, strconv.Itoa(unjudged)+" of what this row reads could not be read or judged (see unreadable); "+detail
	}
	s.listed = len(s.unreadable)
	s.surfaces = append(s.surfaces, Object{{Key: "row", Value: int64(row)}, {Key: "surface", Value: Surfaces[row-1].Name}, {Key: "scanned", Value: scanned}, {Key: "examined", Value: int64(examined)}, {Key: "detail", Value: detail}})
}

func (s *scan) unread(what string, err error) {
	s.unreadable = append(s.unreadable, what+": "+store.PythonOSError(err))
}

// unresolved lists a reference whose target this scan could not establish: it may be Python,
// so it is never dropped.
func (s *scan) unresolved(row int, source, fieldName, value, detail string) {
	s.unreadable = append(s.unreadable, source+": row "+strconv.Itoa(row)+" "+fieldName+" "+strconv.Quote(value)+": "+detail)
}

// verdict files one classified reference: a Python one is a reference, and one whose target
// could not be read is unreadable (a Python one that could not be read is both).
func (s *scan) verdict(row int, source, fieldName string, e Executable, extra ...record.Object) {
	if e.Python {
		s.reference(row, source, fieldName, e, extra...)
	}
	if e.Kind == KindUnreadable {
		s.unresolved(row, source, fieldName, e.Value, e.Detail)
	}
}

// expander is what a hook command may name through the shell: ~ and $HOME from the scan's
// environment, $CODEX_HOME as the scan reads it, and ${PLUGIN_ROOT} (${CLAUDE_PLUGIN_ROOT})
// for a command declared by a cached plugin version (that version's directory). Its PATH is
// the scan's.
func (s *scan) expander(pluginRoot string) Expander {
	vars := map[string]string{"HOME": s.o.Env.Get("HOME"), "CODEX_HOME": s.o.CodexHome}
	if pluginRoot != "" {
		vars["PLUGIN_ROOT"], vars["CLAUDE_PLUGIN_ROOT"] = pluginRoot, pluginRoot
	}
	return Expander{Vars: vars, Path: s.o.Env.Get("PATH")}
}

// judge is an argvJudge whose reports are filed under one row, source and field.
func (s *scan) judge(row int, source, field, cwd string, x Expander) argvJudge {
	return argvJudge{c: s.classifier(row, source, field, x), cwd: cwd, report: func(word string, e Executable) {
		e.Value = word
		s.verdict(row, source, field, e)
	}}
}

// classifier is the host's Classifier for one row, source and field, passing every path it
// classifies to observe when the scan has one.
func (s *scan) classifier(row int, source, field string, x Expander) Classifier {
	c := Classifier{Pointer: s.pointer, Expand: x}
	if s.observe != nil {
		c.Observe = func(path string, e Executable) { s.observe(row, source, field, path, e) }
	}
	return c
}

// absolute is a path the relay or the hook reads from its environment, made as they make it
// (expanduser, then absolute), or why that cannot be done here: ~user, and a relative path,
// which resolves against the working directory of the program that reads it.
func (s *scan) absolute(value string) (string, string) {
	if home := s.o.Env.Get("HOME"); home != "" && (value == "~" || strings.HasPrefix(value, "~/")) {
		value = home + value[1:]
	}
	if !filepath.IsAbs(value) {
		return "", "is not an absolute path once ~ is expanded, so it resolves against the working directory of the program that reads it"
	}
	return value, ""
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
	s.settingsRecords()       // row 4, and the settings timeout row 2 needs
	s.pluginCache()           // row 5, before row 2 so its hook timeouts count
	s.userHooks()             // row 9, likewise
	s.claims()                // row 1
	s.journal()               // row 2
	s.stateDirectories()      // rows 3 and 6
	daemonPids := s.daemons() // row 3
	s.lockHolders(daemonPids) // row 6
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
	longest := s.longestTimeout()
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

// listNamed lists directory for the names match accepts, sorted: nil and no error when the
// directory does not exist, and the listing error when it cannot be read, which a caller
// reports rather than reading as an empty directory.
func listNamed(directory string, match func(string) bool) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		if match(entry.Name()) {
			paths = append(paths, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// settingsRecords is row 4. The Codex home is listed explicitly: one that cannot be listed
// leaves the row unscanned, never scanned with no record found.
func (s *scan) settingsRecords() {
	matches, err := listNamed(s.o.CodexHome, func(name string) bool {
		return strings.HasPrefix(name, "crw-") && strings.HasSuffix(name, ".json")
	})
	if err != nil {
		s.unread(s.o.CodexHome, err)
		s.surface(4, false, 0, "the Codex home "+s.o.CodexHome+" could not be listed, so its crw-*.json records are unknown")
		return
	}
	for _, path := range matches {
		s.settingsRecord(path)
	}
	s.surface(4, true, len(matches), "every crw-*.json record in "+s.o.CodexHome+", each executable resolved through links")
}

// settingsRecord reads one crw-*.json record as row 4 reads it: every value of SettingsKeys
// resolved through links and classified.
func (s *scan) settingsRecord(path string) {
	value, ok := s.readJSON(path, filepath.Base(path))
	if !ok {
		return
	}
	if filepath.Base(path) == "crw-completion-hook.json" {
		if seconds, ok := number(record.Get(value, "timeoutSeconds")); ok {
			s.timeouts = append(s.timeouts, stopTimeout{seconds, path + " timeoutSeconds"})
		}
	}
	for _, key := range SettingsKeys {
		values, ok := texts(record.Get(value, key))
		if !ok {
			s.malformed(4, path, key, "the value is "+scope.TypeName(record.Get(value, key))+", not a string or a list of strings")
		}
		for i, text := range values {
			name := key
			if _, list := record.Get(value, key).([]any); list {
				name = key + "[" + strconv.Itoa(i) + "]"
			}
			if text == "" {
				continue // a launcher reads an empty value as unset
			}
			// The Stop and bridge launchers run a value as written, with no shell, and accept only
			// an absolute path, so one that is not is never resolved against the scan's own
			// directory (argvJudge.path); a file neither native nor #! is not this scan's to judge.
			j := s.judge(4, path, name, "", s.expander(""))
			if key == "args" {
				j.argument(literal(text), false)
			} else if target, ok := j.path(literal(text), false); ok {
				e := j.c.classify(target, "", 0)
				if e.Kind == KindOther {
					e.Kind = KindUnreadable
				}
				j.report(text, e)
			}
		}
	}
}

// texts is a string value, or each item of a list of strings, and whether the value is one of
// those (absent is none); anything else is malformed, which a caller lists rather than reads as
// nothing.
func texts(v any) ([]string, bool) {
	switch value := v.(type) {
	case nil:
		return nil, true
	case string:
		return []string{value}, true
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, true
	}
	return nil, false
}

// malformed lists a declaration or record entry that is not what its reader takes: what it
// runs, or which settings it reads, is unknown.
func (s *scan) malformed(row int, source, field, what string) {
	s.unreadable = append(s.unreadable, source+": row "+strconv.Itoa(row)+" "+field+": "+what+", which is not what the host reads there, so what it runs is unknown")
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

// errRelativePath is a PATH lookup that meets a relative or empty directory (the working
// directory) before any match: what the name finds depends on where the program runs.
var errRelativePath = errors.New("a relative or empty PATH directory comes first, so what the name finds depends on the working directory")

// lookPath is the file a PATH lookup finds for name, searching as the shell and exec do, in
// order. A relative or empty directory resolves where the program runs, not where the scan
// runs, so meeting one before a match is errRelativePath, never a skip to a later directory.
func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
			return "", errRelativePath
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

// hookCommands walks a hooks document ({"hooks": {Event: [{"hooks": [{"command", "timeout"}]}]}}).
// A hook command runs through a shell in the session's workspace, so it is judged as a program
// (argvJudge) with no working directory: a relative word in it is unreadable.
func (s *scan) hookCommands(row int, path string, document Object, pluginRoot string) int {
	count := 0
	// bad lists an entry that is not a hook declaration; one that may be a Stop hook also leaves
	// the settings it reads unknown (row 2).
	bad := func(field, what string, stop bool) {
		s.malformed(row, path, field, what)
		if stop {
			s.stopSettings = append(s.stopSettings, stopSettings{source: path, field: field, problem: "the Stop declaration is malformed"})
		}
	}
	declared, present := record.Lookup(document, "hooks")
	events, ok := declared.(Object)
	if present && !ok {
		bad("hooks", "hooks is "+scope.TypeName(declared)+", not an object of events", true)
	}
	for _, event := range events {
		stop := event.Key == "Stop"
		groups, ok := event.Value.([]any)
		if !ok {
			bad("hooks."+event.Key, "the event is "+scope.TypeName(event.Value)+", not a list of groups", stop)
			continue
		}
		for g, group := range groups {
			at := "hooks." + event.Key + "[" + strconv.Itoa(g) + "]"
			hooks, ok := record.Get(asObject(group), "hooks").([]any)
			if !ok {
				bad(at+".hooks", "the group's hooks are "+scope.TypeName(record.Get(asObject(group), "hooks"))+", not a list", stop)
				continue
			}
			for h, hook := range hooks {
				field := at + ".hooks[" + strconv.Itoa(h) + "].command"
				one, isObject := hook.(Object)
				command, isText := record.Get(one, "command").(string)
				kind, typed := record.Lookup(one, "type")
				timeout, timed := record.Lookup(one, "timeout")
				seconds, isNumber := number(timeout)
				switch {
				case !isObject:
					bad(field, "the hook is "+scope.TypeName(hook)+", not an object", stop)
					continue
				case !isText || (typed && kind != "command"):
					bad(field, "the hook's command is "+scope.TypeName(record.Get(one, "command"))+" and its type "+scope.PyStr(kind)+", not a command string", stop)
					continue
				case timed && !isNumber:
					bad(field, "the hook's timeout is "+scope.TypeName(timeout)+", not a number", stop) // a Stop hook's window cannot be computed
				case stop && timed:
					s.timeouts = append(s.timeouts, stopTimeout{seconds, path + " " + strings.TrimSuffix(field, ".command") + ".timeout"})
				}
				count++
				j := s.judge(row, path, field, "", s.expander(pluginRoot))
				if !stop {
					j.program(command)
					continue
				}
				calls, unknown := readStopCommand(j, command)
				for _, call := range calls {
					s.stopSettings = append(s.stopSettings, s.settingsOf(path, field, call))
				}
				if unknown != "" {
					s.stopSettings = append(s.stopSettings, stopSettings{source: path, field: field, problem: unknown})
				}
			}
		}
	}
	return count
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

// server judges one MCP server declaration (rows 5 and 10). Codex execs command with args, with
// no shell and no expansion, in cwd, with env over the variables it passes on (HOME and PATH;
// not CODEX_HOME or PLUGIN_ROOT, docs/plugin-packaging.md): a relative command or argument
// resolves against cwd, and is unreadable when cwd is not one this scan can place; a bare
// command is found on env's PATH, else the scan's. versionDir is a cached plugin version's
// directory, which a ./ or ${PLUGIN_ROOT} cwd names.
func (s *scan) server(row int, source, field string, get func(string) any, versionDir string) {
	command, isText := get("command").(string)
	args, argsOK := texts(get("args"))
	if _, isList := get("args").([]any); get("args") != nil && !isList {
		argsOK = false
	}
	cwd, cwdOK := get("cwd").(string)
	env := map[string]string{}
	envOK := true
	each := func(key string, v any) {
		text, ok := v.(string)
		env[key], envOK = text, envOK && ok
	}
	switch declared := get("env").(type) {
	case nil:
	case Object:
		for _, f := range declared {
			each(f.Key, f.Value)
		}
	case map[string]any:
		for k, v := range declared {
			each(k, v)
		}
	default:
		envOK = false
	}
	switch {
	case get("command") == nil && get("url") != nil:
		return // a server Codex reaches over HTTP starts nothing here
	case !isText || command == "" || !argsOK || (!cwdOK && get("cwd") != nil) || !envOK:
		s.malformed(row, source, field, "its command, args, cwd or env is not a non-empty string, a list of strings, a string and an object of strings")
		return
	}
	x := Expander{Vars: map[string]string{"HOME": s.o.Env.Get("HOME")}, Path: s.o.Env.Get("PATH")}
	for _, name := range []string{"HOME", "CODEX_HOME"} {
		if v, ok := env[name]; ok {
			x.Vars[name] = v
		}
	}
	if v, ok := env["PATH"]; ok {
		x.Path = v
	}
	switch plugin := strings.TrimPrefix(cwd, "${PLUGIN_ROOT}"); {
	case filepath.IsAbs(cwd):
	case versionDir != "" && (cwd == "." || strings.HasPrefix(cwd, "./")):
		cwd = filepath.Join(versionDir, cwd)
	case versionDir != "" && plugin != cwd && (plugin == "" || strings.HasPrefix(plugin, "/")):
		cwd = versionDir + plugin
	default:
		cwd = ""
	}
	argv := []shellWord{literal(command)}
	for _, arg := range args {
		argv = append(argv, literal(arg))
	}
	j := s.judge(row, source, field, cwd, x)
	defer j.recovered(strings.Join(append([]string{command}, args...), " "))
	j.argv(argv, false)
}

// pluginCache is row 5: every cached version's hook and MCP declarations. Codex loads only a
// version directory, so an entry that is not one (a link to one included) declares nothing.
func (s *scan) pluginCache() {
	root := filepath.Join(s.o.CodexHome, "plugins", "cache", "crw", "crw")
	versions, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		s.surface(5, true, 0, "no cached plugin version exists at "+root)
		return
	}
	if err != nil {
		s.unread(root, err)
		s.surface(5, false, 0, "the plugin cache could not be listed")
		return
	}
	count, incomplete := 0, false
	for _, version := range versions {
		base := filepath.Join(root, version.Name())
		if info, err := os.Stat(base); err != nil || !info.IsDir() {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				s.unread(base, err)
				incomplete = true
			}
			continue
		}
		directory := filepath.Join(base, "wiring", "hooks")
		hooks, err := listNamed(directory, func(name string) bool { return strings.HasSuffix(name, ".json") })
		if err != nil {
			s.unread(directory, err)
			incomplete = true
		}
		for _, path := range hooks {
			if document, ok := s.readJSON(path, "a cached hook declaration"); ok {
				count += s.hookCommands(5, path, document, base)
			}
		}
		for _, path := range []string{filepath.Join(base, "wiring", "mcp.json"), filepath.Join(base, ".mcp.json")} {
			if document, ok := s.readJSON(path, "a cached MCP declaration"); ok {
				declared, present := record.Lookup(document, "mcpServers")
				servers, ok := declared.(Object)
				if present && !ok {
					s.malformed(5, path, "mcpServers", "mcpServers is "+scope.TypeName(declared)+", not an object of servers")
				}
				for _, server := range servers {
					entry := asObject(server.Value)
					s.server(5, path, "mcpServers."+server.Key, func(key string) any { return record.Get(entry, key) }, base)
				}
				count += len(servers)
			}
		}
	}
	if incomplete {
		s.surface(5, false, count, "a cached plugin version under "+root+", or its hook declarations, could not be read, so its hook commands are unknown")
		return
	}
	s.surface(5, true, count, "every hook and MCP command declared by a cached plugin version under "+root)
}

// userHooks is row 9.
func (s *scan) userHooks() {
	path := filepath.Join(s.o.CodexHome, "hooks.json")
	count := 0
	if document, ok := s.readJSON(path, "hooks.json"); ok {
		count = s.hookCommands(9, path, document, "")
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
		if root != "" {
			s.claimRoots = append(s.claimRoots, [2]string{path, root})
		}
		fields := record.Object{{Key: "eventKey", Value: key}, {Key: "claimedAt", Value: record.Get(claim, "claimedAt")}, {Key: "pid", Value: record.Get(claimed, "pid")}}
		switch {
		case root == "":
			s.hold(1, path, append(fields, record.Object{{Key: "outcome", Value: nil}, {Key: "detail", Value: "the claim names no journal root, so its outcome cannot be looked for"}}...))
			continue
		case !filepath.IsAbs(root): // the hooks write only absolute roots; this one resolves nowhere the scan can name
			s.hold(1, path, append(fields, record.Object{{Key: "outcome", Value: nil}, {Key: "detail", Value: "the claim's journal root " + strconv.Quote(root) + " is not an absolute path, so its outcome cannot be looked for"}}...))
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
// whose hook may still be running. Every journal root a retained Stop registration can write
// to is scanned (journalRoots), each within the same window.
func (s *scan) journal() {
	roots := s.journalRoots()
	s.nonFinite()
	listing := strings.Join(roots, ", ")
	longest := s.longestTimeout()
	if math.IsNaN(longest) {
		s.surface(2, false, 0, "a configured Stop hook timeout is NaN, so no window can be computed and no journal row under "+listing+" was judged")
		return
	}
	// A window a Duration cannot hold (2 x 4.6e9 s or more, or infinite) holds every row.
	seconds := 2 * longest
	unbounded := seconds >= float64(math.MaxInt64/int64(time.Second))
	var window time.Duration
	now := s.o.Now().UTC()
	since := time.Time{}
	if !unbounded {
		window = time.Duration(seconds * float64(time.Second))
		since = now.Add(-window)
	}
	count := 0
	for _, root := range roots {
		days, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			s.unread(root, err)
			continue
		}
		for _, day := range days {
			start, err := time.Parse("20060102", day.Name())
			if err != nil || day.Name() != start.Format("20060102") {
				continue // not a day directory (accepted/ holds the outcomes)
			}
			// Every day from the one the window reaches back into (floor(now - window)) on,
			// a future-dated one included.
			if !unbounded && !start.AddDate(0, 0, 1).After(since) {
				continue
			}
			directory := filepath.Join(root, day.Name())
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
				row := reading.ReadJSON(path, "a journal row", nil, nil)
				if row.State == reading.Absent {
					continue // removed since the directory was listed
				}
				count++
				written, configuration, problem := journalRow(row)
				if problem != "" {
					s.unreadable = append(s.unreadable, path+": row 2 journal row: "+problem+", so when its Stop hook ran, and whether the turn that ran it may still hold its command, is unknown")
					continue
				}
				if unbounded || now.Sub(written) < window {
					detail := "a Stop hook ran within the last " + strconv.FormatFloat(window.Seconds(), 'f', -1, 64) + " s, so the turn that ran it may still hold its command"
					if unbounded {
						detail = "the longest configured Stop hook timeout (" + strconv.FormatFloat(longest, 'g', -1, 64) + " s) gives a window no row is outside of, so the turn that ran it may still hold its command"
					}
					s.hold(2, path, record.Object{{Key: "at", Value: written.UTC().Format("2006-01-02T15:04:05Z")}, {Key: "configuration", Value: configuration}, {Key: "detail", Value: detail}})
				}
			}
		}
	}
	if unbounded {
		s.surface(2, true, count, "journal rows in every day directory under "+listing+": the window is unbounded")
		return
	}
	s.surface(2, true, count, "journal rows under "+listing+" in every day directory from "+since.Format("20060102")+" on")
}

// journalRoots is every distinct journal root row 2 reads, sorted: the root the default settings
// (<CODEX_HOME>/crw-completion-hook.json, which the packaged launcher and a registration naming
// no settings read) name, the root each settings document a Stop registration names as its
// argument names, the default root <CODEX_HOME>/crw-completion-hook/journal (always, and
// wherever a settings document names none) and every root a Stop-event claim names. Settings
// that cannot be established or read, and a journalRoot that is not an absolute path, are
// unreadable: the root they name is unknown.
func (s *scan) journalRoots() []string {
	found := map[string]bool{}
	add := func(root string) { found[filepath.Clean(root)] = true }
	fallback := filepath.Join(s.o.CodexHome, "crw-completion-hook", "journal")
	add(fallback)
	defaults := filepath.Join(s.o.CodexHome, "crw-completion-hook.json")
	settings := append([]stopSettings{{source: s.o.CodexHome, field: "the default settings", path: defaults}}, s.stopSettings...)
	read := map[string]bool{}
	for _, one := range settings {
		if one.problem != "" {
			s.unreadable = append(s.unreadable, one.source+": row 2 "+one.field+": the settings this Stop registration reads cannot be established ("+one.problem+"), so the journal root it writes to is unknown")
			continue
		}
		if read[one.path] {
			continue
		}
		read[one.path] = true
		document := reading.ReadJSON(one.path, "Stop settings", nil, nil)
		if document.State == reading.Absent {
			continue // no settings: the hook releases and journals nothing
		}
		value, ok := document.Value.(Object)
		if !document.OK() || !ok {
			detail := document.Detail
			if detail == "" {
				detail = "not a JSON object"
			}
			s.unreadable = append(s.unreadable, one.path+": row 2: the Stop settings could not be read ("+detail+"), so the journal root they name is unknown")
			continue
		}
		if seconds, ok := number(record.Get(value, "timeoutSeconds")); ok && one.path != defaults {
			s.timeouts = append(s.timeouts, stopTimeout{seconds, one.path + " timeoutSeconds"})
		}
		switch root := record.Get(value, "journalRoot").(type) {
		case nil:
			add(fallback)
		case string:
			if !filepath.IsAbs(root) {
				s.unreadable = append(s.unreadable, one.path+": row 2: journalRoot "+strconv.Quote(root)+" is not an absolute path, so where it resolves is unknown")
				continue
			}
			add(root)
		default:
			s.unreadable = append(s.unreadable, one.path+": row 2: journalRoot is "+scope.TypeName(root)+", not a path, so the journal root is unknown")
		}
	}
	for _, claim := range s.claimRoots {
		if filepath.IsAbs(claim[1]) {
			add(claim[1])
		} else {
			s.unreadable = append(s.unreadable, claim[0]+": row 2: journalRoot "+strconv.Quote(claim[1])+" is not an absolute path, so the journal root that turn wrote to is unknown")
		}
	}
	roots := make([]string, 0, len(found))
	for root := range found {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

// adapterEntries are the basenames of the programs that read Stop settings (completion_hook.py,
// the crw-completion-hook entry point), after which a registration names its settings.
var adapterEntries = map[string]bool{"completion_hook.py": true, definition.HookScript: true}

// launcherEntries are the packaged launcher and its copy, which read the default settings.
var launcherEntries = map[string]bool{"crw_stop_hook.py": true, "crw-stop-hook.py": true}

// settingsEnv is the settings override the adapter and the Go hook read when a registration
// names no settings (completion.configuration_path, internal/relay/hook/settings.go).
const settingsEnv = "CRW_COMPLETION_HOOK_CONFIG"

// stopAdapterCall is one command of a Stop command that runs a Stop adapter, found by the word
// naming it: the adapter's entry point (completion_hook.py, crw-completion-hook) or crw hook,
// which take their settings as the next word, or the packaged launcher (and crw hook
// --plugin-launch), which reads the default settings.
type stopAdapterCall struct {
	argv     []shellWord
	at       int       // the word naming the adapter: the entry point, the launcher, or crw of crw hook
	crwHook  bool      // crw hook
	launcher bool      // the packaged launcher or crw hook --plugin-launch
	settings shellWord // the word after the adapter (after crw hook); Written "" when there is none
}

// stopAdapterIn is the Stop adapter one command runs, if any.
func stopAdapterIn(argv []shellWord) (stopAdapterCall, bool) {
	for i, w := range argv {
		base := filepath.Base(w.Written)
		call := stopAdapterCall{argv: argv, at: i}
		next := i + 1
		if base == "crw" && i+1 < len(argv) && argv[i+1].Value == "hook" {
			call.crwHook, next = true, i+2
		} else if !adapterEntries[base] && !launcherEntries[base] {
			continue
		}
		if next < len(argv) {
			call.settings = argv[next]
		}
		call.launcher = launcherEntries[base] || call.settings.Value == "--plugin-launch"
		return call, true
	}
	return stopAdapterCall{}, false
}

// readStopCommand is the one reader of a Stop command, for the retention scan (row 2: which
// settings it reads) and the doctor (which hook it runs): the program is read through the scan's
// grammar and judge (j, whose own reports still reach its report), and each command it runs
// that runs a Stop adapter is returned. unknown says why that list cannot be complete: the
// command holds a word or construct the scan cannot judge, or runs a script that may run the
// adapter itself.
func readStopCommand(j argvJudge, command string) (calls []stopAdapterCall, unknown string) {
	report := j.report
	j.report = func(word string, e Executable) {
		if e.Kind == KindUnreadable && unknown == "" {
			unknown = "it holds " + strconv.Quote(word) + ", which this scan cannot judge"
		}
		if report != nil {
			report(word, e)
		}
	}
	j.seen = func(argv []shellWord, command Executable) {
		if call, ok := stopAdapterIn(argv); ok {
			calls = append(calls, call)
			return
		}
		if command.Kind == KindScript || command.Kind == KindPythonScript {
			unknown = "it runs " + strconv.Quote(argv[0].Written) + ", a script that may run the adapter with settings of its own"
		}
	}
	j.program(command)
	return calls, unknown
}

// settingsOf is the settings document one Stop adapter call reads (row 2): the word after the
// adapter's entry point or after crw hook, which the adapter and the Go hook take first; with no
// such word, $CRW_COMPLETION_HOOK_CONFIG as the scan's environment holds it, else the default
// settings; and the default settings for the packaged launcher and crw hook --plugin-launch.
func (s *scan) settingsOf(source, field string, call stopAdapterCall) stopSettings {
	named := call.settings
	one := stopSettings{source: source, field: field, path: filepath.Join(s.o.CodexHome, "crw-completion-hook.json")}
	switch override := s.o.Env.Get(settingsEnv); {
	case call.launcher:
	case named.Written == "" && override != "":
		if one.path, one.problem = s.absolute(override); one.problem != "" {
			one.problem = "$" + settingsEnv + " " + strconv.Quote(override) + " " + one.problem
		}
	case named.Written == "":
	case named.Missing != "":
		one.problem = "its settings argument " + strconv.Quote(named.Written) + " needs " + named.Missing + ", an expansion this scan does not make"
	case !filepath.IsAbs(named.Value):
		one.problem = "its settings argument " + strconv.Quote(named.Written) + " is a relative path, which resolves wherever the hook runs"
	default:
		one.path = named.Value
	}
	return one
}

// journalRow is when a journal row's Stop hook ran (its at: completion.now writes
// 2006-01-02T15:04:05Z, and any RFC 3339 time is read) and the settings it names, or why that
// cannot be established. The file's modification time is
// never a stand-in: a row that cannot be read or has no valid at is unreadable.
func journalRow(row reading.Reading) (time.Time, any, string) {
	if !row.OK() {
		return time.Time{}, nil, row.Detail
	}
	value, ok := row.Value.(Object)
	if !ok {
		return time.Time{}, nil, "the row is not a JSON object"
	}
	at, ok := record.Get(value, "at").(string)
	if !ok {
		return time.Time{}, nil, "the row's at is " + scope.TypeName(record.Get(value, "at")) + ", not a time"
	}
	written, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return time.Time{}, nil, "the row's at " + strconv.Quote(at) + " is not an RFC 3339 time"
	}
	return written, record.Get(value, "configuration"), ""
}

// longestTimeout is the longest configured Stop hook timeout, at least DefaultHookTimeout. A
// NaN timeout makes it NaN (max passes NaN through), which row 2 treats as no window at all.
func (s *scan) longestTimeout() float64 {
	longest := DefaultHookTimeout
	for _, t := range s.timeouts {
		longest = max(longest, t.seconds)
	}
	return longest
}

// nonFinite lists each configured Stop hook timeout that is not a finite number of seconds,
// naming the value as JSON spells it and where it is configured: it is why row 2's window is
// none (NaN) or unbounded (Infinity, which holds every row), so the row is not scanned.
func (s *scan) nonFinite() {
	for _, t := range s.timeouts {
		spelled := map[bool]string{true: "Infinity", false: "-Infinity"}[t.seconds > 0]
		switch {
		case math.IsNaN(t.seconds):
			spelled = "NaN"
		case !math.IsInf(t.seconds, 0):
			continue
		}
		s.unreadable = append(s.unreadable, t.source+": row 2: a Stop hook timeout of "+spelled+" is not a finite number of seconds, so the journal window it sets is none (NaN) or unbounded (every row is held)")
	}
}

// scopeClaim is one record of the relay's scope registry.
type scopeClaim struct {
	path     string
	document Object
}

// stateDirectories settles every relay state directory rows 3 and 6 read: the state root and
// each directory under it (a link to one included), CODEX_SESSION_RELAY_STATE made absolute as
// the relay makes it, and each stateDir the relay's scope registry records. The registry (the
// production one under the passwd entry's home, and CODEX_SESSION_RELAY_SCOPE_DIR) is where
// every daemon claims its scope whatever environment started it, so a daemon started with
// another --state is found there. Anything that cannot be listed, read or made absolute is
// unreadable and leaves rows 3 and 6 unscanned.
func (s *scan) stateDirectories() {
	unknown := func(what, detail string) {
		s.unreadable = append(s.unreadable, what+": rows 3 and 6: "+detail+", so the relay state directories are not all known")
		s.statesUnknown++
	}
	add := func(directory string) {
		if !contains(s.states, directory) {
			s.states = append(s.states, directory)
		}
	}
	if root := s.o.StateRoot; !filepath.IsAbs(root) {
		unknown(root, "the state root is not an absolute path")
	} else {
		add(root)
		entries, err := os.ReadDir(root)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			unknown(root, store.PythonOSError(err))
		}
		for _, entry := range entries {
			directory := filepath.Join(root, entry.Name())
			if info, err := os.Stat(directory); err == nil && info.IsDir() {
				add(directory)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				unknown(directory, store.PythonOSError(err))
			}
		}
	}
	absolute := func(name string) string {
		value := s.o.Env.Get(name)
		if value == "" {
			return ""
		}
		path, problem := s.absolute(value)
		if problem != "" {
			unknown("$"+name+" "+strconv.Quote(value), problem)
		}
		return path
	}
	if explicit := absolute(scope.StateEnv); explicit != "" {
		add(explicit)
	}
	registries := []string{s.o.ScopeRegistry, absolute("CODEX_SESSION_RELAY_SCOPE_DIR")}
	if registries[0] == "" {
		if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err != nil {
			unknown("the relay's scope registry", "the passwd entry whose home holds it could not be read: "+err.Error())
		} else {
			registries[0] = filepath.Join(u.HomeDir, ".codex-session-relay", "scopes")
		}
	}
	for i, registry := range registries {
		if registry == "" || (i == 1 && registry == registries[0]) {
			continue
		}
		paths, err := listNamed(registry, func(name string) bool { return strings.HasSuffix(name, ".json") })
		if err != nil {
			unknown(registry, store.PythonOSError(err))
		}
		for _, path := range paths {
			read := reading.ReadJSON(path, "a scope claim", nil, nil)
			document, ok := read.Value.(Object)
			switch {
			case read.State == reading.Absent:
				continue
			case !read.OK() || !ok:
				unknown(path, "the scope claim could not be read as a JSON object: "+read.Detail)
				continue
			}
			s.scopeClaims = append(s.scopeClaims, scopeClaim{path, document})
			if directory, _ := record.Get(document, "stateDir").(string); filepath.IsAbs(directory) {
				add(directory)
			} else if stated := record.Get(document, "stateDir"); stated != nil {
				unknown(path, "its stateDir "+scope.PyStr(stated)+" is not an absolute path")
			}
		}
	}
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

// errGone is a process that exited (or became a zombie) while the scan looked at it.
var errGone = errors.New("the process is gone")

// procStat is the fields of <proc>/<pid>/stat after the command name. A pid with no entry
// while the process table itself is there is gone; with no process table at all (darwin has
// none) nothing about it can be known.
func (s *scan) procStat(pid int) ([]string, error) {
	path := filepath.Join(s.o.Proc, strconv.Itoa(pid), "stat")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, tableErr := os.Stat(filepath.Join(s.o.Proc, "self")); tableErr != nil {
			return nil, errors.New("no process table (procfs) exists at " + s.o.Proc)
		}
		return nil, errGone
	}
	if err != nil {
		return nil, err
	}
	end := strings.LastIndex(string(raw), ") ")
	var fields []string
	if end >= 0 {
		fields = strings.Fields(string(raw)[end+2:])
	}
	if len(fields) < 20 {
		return nil, errors.New(path + " is not a process status line")
	}
	if state := fields[0]; state == "Z" || state == "X" || state == "x" {
		return nil, errGone
	}
	return fields, nil
}

// alive is whether pid is the process a record describes: running, with the same start time
// (and boot). An error other than errGone is a process table the scan could not read.
func (s *scan) alive(pid int, ticks any, boot any) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	fields, err := s.procStat(pid)
	if errors.Is(err, errGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return false, err
	}
	if ticks != nil && scope.PyStr(ticks) != scope.PyStr(now) {
		return false, nil
	}
	if boot != nil {
		// A record that names its boot is alive only in that boot. A boot id that cannot be
		// read establishes nothing: a pid and start time can repeat across boots.
		path := filepath.Join(s.o.Proc, "sys", "kernel", "random", "boot_id")
		raw, err := os.ReadFile(path)
		if err != nil {
			return false, errors.New("the current boot id could not be read to compare with the recorded one: " + store.PythonOSError(err))
		}
		if scope.PyStr(boot) != strings.TrimSpace(string(raw)) {
			return false, nil
		}
	}
	return true, nil
}

// process is what an alive pid is running: its executable and argv, and whether that is
// Python. An exe or cmdline that cannot be read, of a process still there, is an error: what
// it runs is unknown.
func (s *scan) process(pid int) (Executable, []any, error) {
	base := filepath.Join(s.o.Proc, strconv.Itoa(pid))
	gone := func(err error) error {
		if _, statErr := s.procStat(pid); errors.Is(statErr, errGone) {
			return errGone
		}
		return err
	}
	exe, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return Executable{}, nil, gone(err)
	}
	raw, cmdErr := os.ReadFile(filepath.Join(base, "cmdline"))
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
	if PythonName(exe) || python || pythonImage(filepath.Join(base, "exe")) {
		e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a process running a Python interpreter or a .py program"
		return e, argv, nil
	}
	if cmdErr != nil {
		return e, argv, gone(cmdErr)
	}
	return e, argv, nil
}

// daemons is row 3: daemon.json in every state directory and every scope registry claim. It
// returns every alive recorded pid for row 6 to exclude.
func (s *scan) daemons() map[int]bool {
	pids := map[int]bool{}
	count, unknown := 0, 0
	records := append([]scopeClaim(nil), s.scopeClaims...)
	for _, directory := range s.states {
		path := filepath.Join(directory, "daemon.json")
		if document, ok := s.readJSON(path, "daemon.json"); ok {
			records = append(records, scopeClaim{path, document})
		}
	}
	for _, one := range records {
		count++
		for _, key := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
			// Absent or null records no process (a stopped daemon, no worker); anything but a
			// positive integer pid_t names a process this scan cannot find.
			raw := record.Get(one.document, key[0])
			n, ok := raw.(int64)
			if raw == nil {
				continue
			} else if !ok || n < 1 || n > math.MaxInt32 {
				unknown++
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+": "+scope.TypeName(raw)+" "+scope.PyStr(raw)+" is not a pid, so the process the record names, and what it runs, is unknown")
				continue
			}
			pid := int(n)
			alive, err := s.alive(pid, record.Get(one.document, key[1]), record.Get(one.document, "bootId"))
			if err != nil {
				unknown++
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+" "+strconv.Itoa(pid)+": whether it is alive could not be read: "+store.PythonOSError(err))
				continue
			}
			if !alive {
				continue
			}
			pids[pid] = true
			switch e, argv, err := s.process(pid); {
			case errors.Is(err, errGone): // exited since it was found alive
			case err != nil:
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+" "+strconv.Itoa(pid)+": what the process runs could not be read: "+store.PythonOSError(err))
			case e.Python:
				s.reference(3, one.path, key[0], e, record.Object{{Key: "pid", Value: int64(pid)}, {Key: "argv", Value: nonNil(argv)}})
			}
		}
	}
	switch {
	case s.statesUnknown > 0:
		s.surface(3, false, count, "the relay state directories could not all be established (see unreadable), so a daemon recorded in one this scan did not read is unknown")
	case unknown > 0:
		s.surface(3, false, count, strconv.Itoa(unknown)+" recorded pids are not pids, or whether they are alive could not be read from "+s.o.Proc+" (its process table, or the boot id a record names), so whether they are alive, and what they run, is unknown")
	default:
		s.surface(3, true, count, "daemon.json in the relay state root, every scope under it, $CODEX_SESSION_RELAY_STATE and each state directory the relay's scope registry records, and each registry claim; a pid counts only while its start time matches the record")
	}
	return pids
}

// mount is one line of mountinfo: a mount point and its superblock's device as /proc/locks
// prints a device (major:minor in hex).
type mount struct{ point, device string }

// mounts reads <proc>/self/mountinfo.
func (s *scan) mounts() ([]mount, error) {
	raw, err := os.ReadFile(filepath.Join(s.o.Proc, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	var out []mount
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var id, parent, major, minor uint64
		var root, point string
		if _, err := fmt.Sscanf(line, "%d %d %d:%d %s %s", &id, &parent, &major, &minor, &root, &point); err != nil {
			return nil, errors.New("a mountinfo line this scan cannot read: " + strconv.Quote(line))
		}
		point, err := strconv.Unquote(`"` + strings.ReplaceAll(point, `"`, `\"`) + `"`) // \040 and the other octal escapes
		if err != nil {
			return nil, errors.New("a mountinfo mount point this scan cannot read: " + strconv.Quote(line))
		}
		out = append(out, mount{point, fmt.Sprintf("%02x:%02x", major, minor)})
	}
	return out, nil
}

// device is the device /proc/locks names a file's locks by: that of the mount holding it (the
// longest mount point containing its resolved path, the last one mounted where two share it).
// stat's st_dev is not it on btrfs, whose subvolumes stat under devices of their own.
func device(mounts []mount, path string) string {
	best, found := -1, ""
	for _, m := range mounts {
		if (path == m.point || m.point == "/" || strings.HasPrefix(path, m.point+"/")) && len(m.point) >= best {
			best, found = len(m.point), m.device
		}
	}
	return found
}

// lockHolders is row 6: the flock holders of every managed-start lock in the state
// directories, less row 3's pids. /proc/locks names a lock by its file's device and inode and
// by the pid that took it, not by whoever holds it now: a lock whose taker is 0 (not visible
// here), gone, or no longer has the file open is held by a process the table does not name, so
// it is unreadable and the row unscanned. A waiter (->) is judged as a holder.
func (s *scan) lockHolders(exclude map[int]bool) {
	type lock struct {
		path string
		stat *syscall.Stat_t
	}
	locks, unknown := map[string]lock{}, s.statesUnknown
	fail := func(what, detail string) {
		s.unreadable = append(s.unreadable, what+": row 6: "+detail)
		unknown++
	}
	var paths []string
	for _, directory := range s.states {
		matches, err := listNamed(directory, func(name string) bool {
			return strings.HasPrefix(name, "managed-start-") && strings.HasSuffix(name, ".lock")
		})
		if err != nil {
			s.unread(directory, err)
			unknown++
		}
		paths = append(paths, matches...)
	}
	mounts, err := s.mounts()
	if err != nil && len(paths) > 0 {
		fail(filepath.Join(s.o.Proc, "self", "mountinfo"), "the mount table could not be read, so no managed-start lock can be matched to the kernel's lock table: "+store.PythonOSError(err))
	}
	for _, path := range paths {
		info, statErr := os.Stat(path)
		resolved, resolveErr := record.Resolve(path)
		switch {
		case statErr != nil:
			fail(path, store.PythonOSError(statErr))
		case resolveErr != nil:
			fail(path, resolveErr.Error())
		case err == nil:
			sys, ok := info.Sys().(*syscall.Stat_t)
			on := device(mounts, resolved)
			if !ok || on == "" {
				fail(path, "the device the lock table names it by could not be established from its stat and the mount table")
				continue
			}
			locks[on+":"+strconv.FormatUint(uint64(sys.Ino), 10)] = lock{path, sys}
		}
	}
	examined := len(paths)
	surface := func(detail string) {
		if unknown > 0 {
			s.surface(6, false, examined, "the holders of the managed-start locks are not all known (see unreadable); "+detail)
			return
		}
		s.surface(6, true, examined, detail)
	}
	if examined == 0 {
		surface("no managed-start lock exists under the relay state directories")
		return
	}
	table := filepath.Join(s.o.Proc, "locks")
	file, err := os.Open(table)
	if err != nil {
		fail(table, "the kernel's lock table could not be read: "+store.PythonOSError(err))
		surface("the kernel's lock table could not be read")
		return
	}
	defer file.Close()
	holders := map[int]lock{}
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		f := strings.Fields(lines.Text())
		if len(f) > 1 && f[1] == "->" {
			f = append(f[:1:1], f[2:]...)
		}
		if len(f) < 6 || f[1] != "FLOCK" {
			continue
		}
		held, ok := locks[f[5]]
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(f[4]); err == nil && pid > 0 {
			holders[pid] = held
			continue
		}
		fail(held.path, "the lock table names its taker as pid "+f[4]+", not visible from here, so what holds it is unknown")
	}
	// A read that fails part way leaves every line after it unread, and a holder may be on one
	// of them: the holders found so far are still judged, and the row is not scanned.
	if err := lines.Err(); err != nil {
		fail(table, "the kernel's lock table could not be read to its end: "+store.PythonOSError(err))
	}
	pids := make([]int, 0, len(holders))
	for pid := range holders {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		held := holders[pid]
		if exclude[pid] {
			continue
		}
		e, argv, err := s.process(pid)
		switch {
		case errors.Is(err, errGone):
			fail(held.path, "pid "+strconv.Itoa(pid)+" took this lock and has exited, so a process that inherited it holds it, which the lock table does not name")
		case err != nil:
			fail(held.path, "what pid "+strconv.Itoa(pid)+" runs could not be read: "+store.PythonOSError(err))
		case e.Python:
			s.reference(6, held.path, "holder", e, record.Object{{Key: "pid", Value: int64(pid)}, {Key: "argv", Value: nonNil(argv)}})
		default:
			if open, err := s.holdsOpen(pid, held.stat); err != nil {
				fail(held.path, "whether pid "+strconv.Itoa(pid)+" still has the lock open could not be read: "+store.PythonOSError(err))
			} else if !open {
				fail(held.path, "pid "+strconv.Itoa(pid)+", which took this lock, no longer has it open, so the process holding it is one the lock table does not name")
			}
		}
	}
	surface("the /proc/locks holders of every managed-start lock, less the pids row 3 reports")
}

// holdsOpen reports whether pid has the file described by want open.
func (s *scan) holdsOpen(pid int, want *syscall.Stat_t) (bool, error) {
	directory := filepath.Join(s.o.Proc, strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if info, err := os.Stat(filepath.Join(directory, entry.Name())); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Dev == want.Dev && st.Ino == want.Ino {
				return true, nil
			}
		}
	}
	return false, nil
}

// launcherCopy is row 8: <CODEX_HOME>/crw-stop-hook.py is a .py launcher by definition.
func (s *scan) launcherCopy() {
	path := filepath.Join(s.o.CodexHome, "crw-stop-hook.py")
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		e := s.classifier(8, path, "file", Expander{}).Classify(path, "")
		e.Python = true // a .py launcher by definition, whatever it now holds
		s.verdict(8, path, "file", e)
		s.surface(8, true, 1, "the launcher copy the cached Python Stop bootstrap falls back to")
	case errors.Is(err, os.ErrNotExist):
		s.surface(8, true, 0, "no launcher copy exists at "+path)
	default:
		s.unread(path, err)
		s.surface(8, true, 0, "whether a launcher copy exists could not be established")
	}
}

// configToml is row 10: every mcp_servers table, judged as Codex starts it (server).
func (s *scan) configToml(_ context.Context) {
	path := filepath.Join(s.o.CodexHome, "config.toml")
	read := reading.ReadText(path, "config.toml")
	switch {
	case read.State == reading.Absent:
		s.surface(10, true, 0, "no config.toml exists at "+path)
		return
	case !read.OK():
		s.unreadable = append(s.unreadable, path+": "+read.Detail)
		s.surface(10, false, 0, "config.toml could not be read")
		return
	}
	var document map[string]any
	if _, err := toml.Decode(read.Value.(string), &document); err != nil {
		s.unreadable = append(s.unreadable, path+": "+err.Error())
		s.surface(10, false, 0, "config.toml could not be parsed")
		return
	}
	servers, ok := document["mcp_servers"].(map[string]any)
	if declared, present := document["mcp_servers"]; present && !ok {
		s.malformed(10, path, "mcp_servers", "mcp_servers is "+scope.TypeName(declared)+", not a table of servers")
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, _ := servers[name].(map[string]any)
		s.server(10, path, "mcp_servers."+name, func(key string) any { return entry[key] }, "")
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
		s.verdict(11, s.pointer, "target", Classify(s.pointer, "", s.pointer))
		s.surface(11, true, 1, read.Detail)
	default:
		s.surface(11, true, 0, read.Detail)
	}
}
