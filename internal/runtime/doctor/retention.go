package doctor

import (
	"bufio"
	"context"
	"errors"
	"math"
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

// unresolved lists a reference whose target this scan could not establish: it may be Python,
// so it is never dropped.
func (s *scan) unresolved(row int, source, fieldName, value, detail string) {
	s.unreadable = append(s.unreadable, source+": row "+strconv.Itoa(row)+" "+fieldName+" "+strconv.Quote(value)+": "+detail)
}

// judge files one classified reference: a Python one is a reference, and one whose target
// could not be read is unreadable (a Python one that could not be read is both).
func (s *scan) judge(row int, source, fieldName string, e Executable, extra ...record.Object) {
	if e.Python {
		s.reference(row, source, fieldName, e, extra...)
	}
	if e.Kind == KindUnreadable {
		s.unresolved(row, source, fieldName, e.Value, e.Detail)
	}
}

// expander is what a reference may name through the shell: ~ and $HOME from the scan's
// environment, $CODEX_HOME as the scan reads it, and ${PLUGIN_ROOT} for a command declared by
// a cached plugin version (that version's directory).
func (s *scan) expander(pluginRoot string) Expander {
	vars := map[string]string{"HOME": s.o.Env.Get("HOME"), "CODEX_HOME": s.o.CodexHome}
	if pluginRoot != "" {
		vars["PLUGIN_ROOT"] = pluginRoot
	}
	return Expander{Vars: vars, Path: s.o.Env.Get("PATH")}
}

// literal is a value that no shell reads (a crw-*.json field, an MCP command or argument) as
// a word: ~, $HOME, $CODEX_HOME and ${PLUGIN_ROOT} expanded over its text (Expander).
func (s *scan) literal(text, pluginRoot string) shellWord {
	value, missing := s.expander(pluginRoot).Expand(text)
	return shellWord{Written: text, Value: value, Missing: missing}
}

// word classifies one reference: a word whose expansions could not all be made is reported as
// a Python reference when it names Python anyway, else as unreadable.
// sourced judges the file as a shell's program (sh FILE, . FILE) whatever its #! line says. A
// value that is not an absolute path with no base to resolve it against (a crw-*.json record's)
// is never resolved against the scan's own working directory: the program that reads it runs
// elsewhere, so it is a Python reference when it names Python and unreadable otherwise. It
// returns false when the word is left unjudged.
func (s *scan) word(row int, source, fieldName string, w shellWord, base, pluginRoot string, sourced bool) (Executable, bool) {
	word, expanded, missing := w.Written, w.Value, w.Missing
	if missing != "" {
		if strings.HasSuffix(word, ".py") || PythonName(word) {
			return Executable{Value: word, Kind: KindPythonScript, Python: true, Detail: "names a Python file through " + missing + ", an expansion this scan does not make"}, true
		}
		s.unresolved(row, source, fieldName, word, "names "+missing+", an expansion this scan does not make, so what it runs is unknown")
		return Executable{}, false
	}
	if base == "" && !filepath.IsAbs(expanded) {
		switch {
		case strings.HasSuffix(expanded, ".py"):
			return Executable{Value: word, Kind: KindPythonScript, Python: true, Detail: "names a Python file by a relative path"}, true
		case PythonName(expanded):
			return Executable{Value: word, Kind: KindPythonInterpreter, Python: true, Detail: "names a Python interpreter by a relative path"}, true
		}
		s.unresolved(row, source, fieldName, word, "is not an absolute path: it resolves in whatever directory the program reading it runs in (the Stop and bridge launchers accept only an absolute path), so this scan does not resolve it against its own")
		return Executable{}, false
	}
	c := Classifier{Pointer: s.pointer, Expand: s.expander(pluginRoot)}
	var e Executable
	if sourced {
		e = c.sourced(expanded, base, 0)
	} else {
		e = c.Classify(expanded, base)
	}
	e.Value = word
	return e, true
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
				if key == "args" {
					s.argvWord(4, path, name, s.literal(text, ""), "", "", roleArgument)
				} else if e, ok := s.word(4, path, name, s.literal(text, ""), "", "", false); ok {
					s.judge(4, path, name, e)
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

// commandWords reads one hook command, which Codex runs through a shell, as a shell program
// (shellWalker): every word in a command position is judged as a command, every other word as
// an argument, and every construct the reader cannot place is unreadable.
func (s *scan) commandWords(row int, source, fieldName, line, base, pluginRoot string) {
	w := &shellWalker{
		expand: s.expander(pluginRoot),
		visit: func(word shellWord, role int) bool {
			s.argvWord(row, source, fieldName, word, base, pluginRoot, role)
			return true
		},
		unreadable: func(value, detail string) { s.unresolved(row, source, fieldName, value, detail) },
	}
	w.walk(line, 0)
}

// argvWord classifies one word as it reaches exec, in its role (roleCommand, roleArgument or
// roleScript): a word naming a Python interpreter, a .py path, or a path that resolves to
// Python is a reference. Its expansions are made (~, $HOME, $CODEX_HOME and, for a cached
// plugin version, ${PLUGIN_ROOT}); a word needing any other expansion is unreadable. A bare command word is looked up on PATH, and one that no PATH directory holds as
// an executable file is unreadable: what it runs is unknown. A bare argument is looked up too,
// and reported when it is a Python program, because a runner (sudo, xargs, uv run) may execute
// it.
func (s *scan) argvWord(row int, source, fieldName string, w shellWord, base, pluginRoot string, role int) {
	word, expanded, missing := w.Written, w.Value, w.Missing
	if strings.ContainsAny(word, "\n") {
		return // a -c program, not something executed by name
	}
	var e Executable
	switch {
	case role == roleScript || missing != "" || strings.Contains(expanded, "/") || strings.HasSuffix(expanded, ".py"):
		var ok bool
		if e, ok = s.word(row, source, fieldName, w, base, pluginRoot, role == roleScript); !ok {
			return
		}
	case PythonName(expanded):
		e = Executable{Value: word, Kind: KindPythonInterpreter, Python: true, Detail: "names a Python interpreter"}
		if found, err := lookPath(expanded, s.o.Env.Get("PATH")); err == nil {
			e.Resolves = found
		}
	default:
		found, err := lookPath(expanded, s.o.Env.Get("PATH"))
		switch {
		case err == nil:
			e = Classifier{Pointer: s.pointer, Expand: s.expander(pluginRoot)}.Classify(found, "")
			e.Value = word
			if role != roleCommand && !e.Python {
				return
			}
		case role == roleCommand:
			s.unresolved(row, source, fieldName, word, "names a command that no directory on the scan's PATH ("+s.o.Env.Get("PATH")+") holds as an executable file, so what it runs is unknown")
			return
		default:
			return
		}
	}
	s.judge(row, source, fieldName, e)
}

// lookPath is the file a PATH lookup finds for name. A relative PATH directory (or an empty
// one, which means the current directory) resolves where the program runs, not where the scan
// runs, so it is not searched: a name only such a directory holds is not found.
func lookPath(name, path string) (string, error) {
	for _, dir := range filepath.SplitList(path) {
		if !filepath.IsAbs(dir) {
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
func (s *scan) hookCommands(row int, path string, document Object, base, pluginRoot string) int {
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
				s.commandWords(row, path, "hooks."+event.Key+"["+strconv.Itoa(g)+"].hooks["+strconv.Itoa(h)+"].command", command, base, pluginRoot)
			}
		}
	}
	return count
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

// mcpServers walks an MCP document ({"mcpServers": {name: {"command", "args"}}}). Codex starts
// an MCP server without a shell: command is one executable path and each args entry one argv
// word, so neither is split into words (a path holding a space is one path).
func (s *scan) mcpServers(row int, path string, servers Object, base, pluginRoot string) int {
	for _, server := range servers {
		entry := asObject(server.Value)
		if command, ok := record.Get(entry, "command").(string); ok && command != "" {
			s.argvWord(row, path, "mcpServers."+server.Key+".command", s.literal(command, pluginRoot), base, pluginRoot, roleCommand)
		}
		for i, arg := range words(record.Get(entry, "args")) {
			s.argvWord(row, path, "mcpServers."+server.Key+".args["+strconv.Itoa(i)+"]", s.literal(arg, pluginRoot), base, pluginRoot, roleArgument)
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
				count += s.hookCommands(5, path, document, base, base)
			}
		}
		for _, path := range []string{filepath.Join(base, "wiring", "mcp.json"), filepath.Join(base, ".mcp.json")} {
			if document, ok := s.readJSON(path, "a cached MCP declaration"); ok {
				servers, _ := record.Get(document, "mcpServers").(Object)
				count += s.mcpServers(5, path, servers, base, base)
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
		count = s.hookCommands(9, path, document, s.o.CodexHome, "")
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
	longest := s.longestTimeout()
	if math.IsNaN(longest) {
		s.surface(2, false, 0, "a configured Stop hook timeout is NaN, so no window can be computed and no journal row under "+root+" was judged")
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
	days, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		s.surface(2, true, 0, "no journal exists at "+root)
		return
	}
	if err != nil {
		s.unread(root, err)
		s.surface(2, true, 0, "the journal's day directories could not be listed")
		return
	}
	count := 0
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
	if unbounded {
		s.surface(2, true, count, "journal rows in every day directory under "+root+": the window is unbounded")
		return
	}
	s.surface(2, true, count, "journal rows under "+root+" in every day directory from "+since.Format("20060102")+" on")
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
		longest = max(longest, t)
	}
	return longest
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
	if PythonName(exe) || python {
		e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a process running a Python interpreter or a .py program"
		return e, argv, nil
	}
	if cmdErr != nil {
		return e, argv, gone(cmdErr)
	}
	return e, argv, nil
}

// inspect files what an alive pid runs: a Python process is a reference, one whose exe or
// cmdline could not be read is unreadable, one that exited meanwhile is nothing.
func (s *scan) inspect(row int, source, fieldName string, pid int) {
	e, argv, err := s.process(pid)
	switch {
	case errors.Is(err, errGone):
	case err != nil:
		s.unreadable = append(s.unreadable, source+": row "+strconv.Itoa(row)+" "+fieldName+" "+strconv.Itoa(pid)+": what the process runs could not be read: "+store.PythonOSError(err))
	case e.Python:
		s.reference(row, source, fieldName, e, record.Object{{Key: "pid", Value: int64(pid)}, {Key: "argv", Value: nonNil(argv)}})
	}
}

// daemons is row 3. It returns every alive recorded pid for row 6 to exclude.
func (s *scan) daemons() map[int]bool {
	pids := map[int]bool{}
	count, unknown := 0, 0
	for _, directory := range s.stateDirectories() {
		path := filepath.Join(directory, "daemon.json")
		document, ok := s.readJSON(path, "daemon.json")
		if !ok {
			continue
		}
		count++
		for _, key := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
			n, ok := number(record.Get(document, key[0]))
			if !ok {
				continue
			}
			pid := int(n)
			alive, err := s.alive(pid, record.Get(document, key[1]), record.Get(document, "bootId"))
			if err != nil {
				unknown++
				s.unreadable = append(s.unreadable, path+": row 3 "+key[0]+" "+strconv.Itoa(pid)+": whether it is alive could not be read: "+store.PythonOSError(err))
				continue
			}
			if !alive {
				continue
			}
			pids[pid] = true
			s.inspect(3, path, key[0], pid)
		}
	}
	if unknown > 0 {
		s.surface(3, false, count, "whether "+strconv.Itoa(unknown)+" recorded pids are alive could not be read from "+s.o.Proc+" (its process table, or the boot id a record names), so whether they are alive, and what they run, is unknown")
		return pids
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
	// A read that fails part way leaves every line after it unread, and a holder may be on one
	// of them: the holders found so far are still judged, and the row is not scanned.
	partial := lines.Err()
	if partial != nil {
		s.unread(filepath.Join(s.o.Proc, "locks"), partial)
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
		s.inspect(6, holders[pid], "holder", pid)
	}
	if partial != nil {
		s.surface(6, false, len(files), "the kernel's lock table could not be read to its end, so the holders of "+strconv.Itoa(len(files))+" managed-start locks are unknown")
		return
	}
	s.surface(6, true, len(files), "the /proc/locks holders of every managed-start lock, less the pids row 3 reports")
}

// launcherCopy is row 8: <CODEX_HOME>/crw-stop-hook.py is a .py launcher by definition.
func (s *scan) launcherCopy() {
	path := filepath.Join(s.o.CodexHome, "crw-stop-hook.py")
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		e := Classify(path, "", s.pointer)
		e.Python = true // a .py launcher by definition, whatever it now holds
		s.judge(8, path, "file", e)
		s.surface(8, true, 1, "the launcher copy the cached Python Stop bootstrap falls back to")
	case errors.Is(err, os.ErrNotExist):
		s.surface(8, true, 0, "no launcher copy exists at "+path)
	default:
		s.unread(path, err)
		s.surface(8, true, 0, "whether a launcher copy exists could not be established")
	}
}

// configToml is row 10: the command and args of every mcp_servers table, each one argv word as
// Codex passes it to exec (mcpServers).
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
			s.argvWord(10, path, "mcp_servers."+name+".command", s.literal(command, ""), s.o.CodexHome, "", roleCommand)
		}
		if args, ok := entry["args"].([]any); ok {
			for i, arg := range args {
				if text, ok := arg.(string); ok {
					s.argvWord(10, path, "mcp_servers."+name+".args["+strconv.Itoa(i)+"]", s.literal(text, ""), s.o.CodexHome, "", roleArgument)
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
		s.judge(11, s.pointer, "target", Classify(s.pointer, "", s.pointer))
		s.surface(11, true, 1, read.Detail)
	default:
		s.surface(11, true, 0, read.Detail)
	}
}
