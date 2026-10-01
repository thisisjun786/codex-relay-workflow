package doctor

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// surfaces names the registrations a host reads, by the row numbers the retired retention scan
// gave them (decision 59): `crw install remove` names each registration it finds by its row
// and this name, so both are kept as they were. Row 8, the crw-stop-hook.py launcher copy, is
// retired (decision 67).
var surfaces = map[int]string{
	4:  "crw-*.json settings records",
	5:  "cached plugin wiring command strings",
	9:  "hooks.json command strings",
	10: "config.toml mcp_servers commands",
}

// ScanOptions are the inputs of one reading of the host's registrations or relay records.
type ScanOptions struct {
	Env         scope.Env
	CodexHome   string
	Destination string
	StateRoot   string
	// Proc is the procfs root (a seam for tests); "" is /proc.
	Proc string
	// ScopeRegistry is the relay's production scope registry (a seam for tests); "" is
	// <home>/.codex-session-relay/scopes, home taken from the passwd entry as the relay takes it.
	ScopeRegistry string
	// Foreign keeps what another program's registration in hooks.json or config.toml could not be
	// read or judged, which RegisteredMatching otherwise drops (shared; a seam for the tests of the
	// grammar, which read such registrations).
	Foreign bool
}

// scopeDirEnv overrides the relay's scope registry (the relay's service.ScopeEnv).
const scopeDirEnv = "CODEX_SESSION_RELAY_SCOPE_DIR"

// settingsKeys are the keys of a crw-*.json record that name something to execute.
var settingsKeys = []string{"relayExecutable", "bridgeExecutable", "interpreterPath", "command", "args"}

// scan is one reading of the host: the registrations it reads (RegisteredMatching) or the relay
// records it keeps (RecordedDaemons). Everything it could not read
// or judge is in unreadable, never dropped.
type scan struct {
	o          ScanOptions
	pointer    string
	unreadable []string
	// stopSettings are the settings documents the Stop registrations read.
	stopSettings []stopSettings
	// states are the relay state directories whose daemon.json is read, registries the scope
	// registries whose claims name more of them, and scopeClaims the registries' records.
	// relayScope narrows registries to the one the relay resolves in the scan's environment.
	states      []string
	registries  []string
	scopeClaims []scopeClaim
	relayScope  bool
	// observe, when set, receives every path the registration rows classify, with the row,
	// source and field naming it (RegisteredMatching).
	observe func(row int, source, field, path string, e Executable)
	// destination is the destination resolved, when that differs from how it is spelled.
	destination string
	// ours is set while one registration of rows 9 and 10 is judged, once anything it names is
	// CRW's (crwWord, crwPath); see shared.
	ours bool
}

// stopSettings is the settings document one Stop registration reads: path, or why it cannot be
// established.
type stopSettings struct {
	source, field, path, problem string
}

func (s *scan) unread(what string, err error) {
	s.unreadable = append(s.unreadable, what+": "+store.PythonOSError(err))
}

// unresolved lists a reference whose target this reading could not establish: what it runs is
// unknown, so it is never dropped.
func (s *scan) unresolved(row int, source, fieldName, value, detail string) {
	s.unreadable = append(s.unreadable, source+": row "+strconv.Itoa(row)+" "+fieldName+" "+strconv.Quote(value)+": "+detail)
}

// verdict files one classified reference: one whose target could not be read is unreadable.
func (s *scan) verdict(row int, source, fieldName string, e Executable) {
	if e.Kind == KindUnreadable {
		s.unresolved(row, source, fieldName, e.Value, e.Detail)
	}
}

// expander is what a hook command may name through the shell: ~ and $HOME from the reading's
// environment, $CODEX_HOME as the reading takes it, and ${PLUGIN_ROOT} (${CLAUDE_PLUGIN_ROOT})
// for a command declared by a cached plugin version (that version's directory). Its PATH is
// the reading's.
func (s *scan) expander(pluginRoot string) Expander {
	vars := map[string]string{"HOME": s.o.Env.Get("HOME"), "CODEX_HOME": s.o.CodexHome}
	if pluginRoot != "" {
		vars["PLUGIN_ROOT"], vars["CLAUDE_PLUGIN_ROOT"] = pluginRoot, pluginRoot
	}
	return Expander{Vars: vars, Path: s.o.Env.Get("PATH")}
}

// judge is an argvJudge whose reports are filed under one row, source and field. Every word it
// reports or runs is also asked whether it is CRW's (shared).
func (s *scan) judge(row int, source, field, cwd string, x Expander) argvJudge {
	c := s.classifier(row, source, field, x)
	return argvJudge{c: c, cwd: cwd, report: func(word string, e Executable) {
		s.ours = s.ours || s.crwWord(word)
		e.Value = word
		s.verdict(row, source, field, e)
	}, seen: func(argv []shellWord, _ Executable) {
		for _, w := range argv {
			s.ours = s.ours || s.crwWord(w.Written) || s.crwWord(w.Value)
		}
	}}
}

// classifier is the host's Classifier for one row, source and field, passing every path it
// classifies to observe when the reading has one.
func (s *scan) classifier(row int, source, field string, x Expander) Classifier {
	c := Classifier{Pointer: s.pointer, Expand: x}
	c.Observe = func(path string, e Executable) {
		s.ours = s.ours || s.crwPath(path) || s.crwPath(e.Resolves)
		if s.observe != nil {
			s.observe(row, source, field, path, e)
		}
	}
	return c
}

// crwNames are the programs a CRW runtime holds: crw, its compatibility links, and the link a
// runtime installed before decision 66 still carries.
var crwNames = map[string]bool{"crw": true, definition.Relay: true, definition.Bridge: true, retiredHookLink: true}

// crwPath is whether a path is in the destination, as written or resolved.
func (s *scan) crwPath(path string) bool {
	if path == "" {
		return false
	}
	for _, root := range []string{s.o.Destination, s.destination} {
		if root != "" && (path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	return crwNames[filepath.Base(path)]
}

// crwWord is whether a word as a registration writes it names a CRW program: one of crwNames, or
// a path in the destination, spelled from HOME as $HOME, ${HOME} or ~ too, since a word this
// reading cannot expand is judged by its spelling.
func (s *scan) crwWord(word string) bool {
	if word == "" {
		return false
	}
	if crwNames[filepath.Base(word)] || strings.Contains(word, s.o.Destination) || s.destination != "" && strings.Contains(word, s.destination) {
		return true
	}
	if home := s.o.Env.Get("HOME"); home != "" && strings.HasPrefix(s.o.Destination, home+"/") {
		rest := strings.TrimPrefix(s.o.Destination, home)
		for _, spelling := range []string{"$HOME", "${HOME}", "~"} {
			if strings.Contains(word, spelling+rest) {
				return true
			}
		}
	}
	return false
}

// shared judges one registration of the host-wide surfaces other programs share, hooks.json
// (row 9) and config.toml's mcp_servers (row 10), and keeps what it could not read or judge only
// when the registration is CRW's: when a word it names or runs, a path it classifies, or where
// that path resolves is a CRW program or lies in the destination. Another program's registration
// that this reading cannot judge (a hook running an interpreter's script, a server started over
// ssh) leaves a CRW runtime unused, so it never holds a removal back (decision 68). What
// it finds inside the directory is kept whoever registered it.
func (s *scan) shared(judge func()) {
	if s.o.Foreign {
		judge()
		return
	}
	mark, stops := len(s.unreadable), len(s.stopSettings)
	s.ours = false
	judge()
	if !s.ours {
		s.unreadable, s.stopSettings = s.unreadable[:mark], s.stopSettings[:stops]
	}
	s.ours = false
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

// settingsRecords is row 4. The Codex home is listed explicitly: one that cannot be listed is
// unreadable, never read as holding no record.
func (s *scan) settingsRecords() {
	matches, err := listNamed(s.o.CodexHome, func(name string) bool {
		return strings.HasPrefix(name, "crw-") && strings.HasSuffix(name, ".json")
	})
	if err != nil {
		s.unread(s.o.CodexHome, err)
		return
	}
	for _, path := range matches {
		s.settingsRecord(path)
	}
}

// settingsRecord reads one crw-*.json record as row 4 reads it: every value of settingsKeys
// resolved through links and classified.
func (s *scan) settingsRecord(path string) {
	value, ok := s.readJSON(path, filepath.Base(path))
	if !ok {
		return
	}
	for _, key := range settingsKeys {
		values, ok := texts(record.Get(value, key))
		if !ok {
			s.malformed(4, path, key, "the value is "+pyvalue.TypeName(record.Get(value, key))+", not a string or a list of strings")
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
			// an absolute path, so one that is not is never resolved against the reading's own
			// directory (argvJudge.path); a file neither native nor #! is not this reading's to judge.
			j := s.judge(4, path, name, "", s.expander(""))
			if key == "args" {
				j.argument(literal(text))
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
// order. A relative or empty directory resolves where the program runs, not where the reading
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
func (s *scan) hookCommands(row int, path string, document Object, pluginRoot string) {
	// bad lists an entry that is not a hook declaration; one that may be a Stop hook also leaves
	// the settings it reads unknown.
	bad := func(field, what string, stop bool) {
		s.malformed(row, path, field, what)
		if stop {
			s.stopSettings = append(s.stopSettings, stopSettings{source: path, field: field, problem: "the Stop declaration is malformed"})
		}
	}
	declared, present := record.Lookup(document, "hooks")
	events, ok := declared.(Object)
	if present && !ok {
		bad("hooks", "hooks is "+pyvalue.TypeName(declared)+", not an object of events", true)
	}
	for _, event := range events {
		stop := event.Key == "Stop"
		groups, ok := event.Value.([]any)
		if !ok {
			bad("hooks."+event.Key, "the event is "+pyvalue.TypeName(event.Value)+", not a list of groups", stop)
			continue
		}
		for g, group := range groups {
			at := "hooks." + event.Key + "[" + strconv.Itoa(g) + "]"
			hooks, ok := record.Get(asObject(group), "hooks").([]any)
			if !ok {
				bad(at+".hooks", "the group's hooks are "+pyvalue.TypeName(record.Get(asObject(group), "hooks"))+", not a list", stop)
				continue
			}
			for h, hook := range hooks {
				field := at + ".hooks[" + strconv.Itoa(h) + "].command"
				one, isObject := hook.(Object)
				command, isText := record.Get(one, "command").(string)
				kind, typed := record.Lookup(one, "type")
				timeout, timed := record.Lookup(one, "timeout")
				_, isNumber := number(timeout)
				switch {
				case !isObject:
					bad(field, "the hook is "+pyvalue.TypeName(hook)+", not an object", stop)
					continue
				case !isText || (typed && kind != "command"):
					bad(field, "the hook's command is "+pyvalue.TypeName(record.Get(one, "command"))+" and its type "+scope.PyStr(kind)+", not a command string", stop)
					continue
				case timed && !isNumber:
					bad(field, "the hook's timeout is "+pyvalue.TypeName(timeout)+", not a number", stop)
				}
				judge := func() {
					j := s.judge(row, path, field, "", s.expander(pluginRoot))
					if !stop {
						j.program(command)
						return
					}
					calls, unknown := readStopCommand(j, command)
					for _, call := range calls {
						s.stopSettings = append(s.stopSettings, s.settingsOf(path, field, call))
					}
					if unknown != "" {
						s.stopSettings = append(s.stopSettings, stopSettings{source: path, field: field, problem: unknown})
					}
				}
				if row == 9 {
					s.shared(judge)
				} else {
					judge() // row 5, the CRW plugin's own cache
				}
			}
		}
	}
}

func asObject(v any) Object {
	o, _ := v.(Object)
	return o
}

// server judges one MCP server declaration (rows 5 and 10). Codex execs command with args, with
// no shell and no expansion, in cwd, with env over the variables it passes on (HOME and PATH;
// not CODEX_HOME or PLUGIN_ROOT, docs/plugin-packaging.md): a relative command or argument
// resolves against cwd, and is unreadable when cwd is not one this reading can place; a bare
// command is found on env's PATH, else the reading's. versionDir is a cached plugin version's
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
	judge := func() {
		for _, w := range argv {
			s.ours = s.ours || s.crwWord(w.Value)
		}
		j := s.judge(row, source, field, cwd, x)
		defer j.recovered(strings.Join(append([]string{command}, args...), " "))
		j.argv(argv, false)
	}
	if row == 10 {
		s.shared(judge)
	} else {
		judge() // row 5, the CRW plugin's own cache
	}
}

// pluginCache is row 5: every cached version's hook and MCP declarations. Codex loads only a
// version directory, so an entry that is not one (a link to one included) declares nothing.
func (s *scan) pluginCache() {
	root := filepath.Join(s.o.CodexHome, "plugins", "cache", "crw", "crw")
	versions, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.unread(root, err)
		return
	}
	for _, version := range versions {
		base := filepath.Join(root, version.Name())
		if info, err := os.Stat(base); err != nil || !info.IsDir() {
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				s.unread(base, err)
			}
			continue
		}
		directory := filepath.Join(base, "wiring", "hooks")
		hooks, err := listNamed(directory, func(name string) bool { return strings.HasSuffix(name, ".json") })
		if err != nil {
			s.unread(directory, err)
		}
		for _, path := range hooks {
			if document, ok := s.readJSON(path, "a cached hook declaration"); ok {
				s.hookCommands(5, path, document, base)
			}
		}
		for _, path := range []string{filepath.Join(base, "wiring", "mcp.json"), filepath.Join(base, ".mcp.json")} {
			if document, ok := s.readJSON(path, "a cached MCP declaration"); ok {
				declared, present := record.Lookup(document, "mcpServers")
				servers, ok := declared.(Object)
				if present && !ok {
					s.malformed(5, path, "mcpServers", "mcpServers is "+pyvalue.TypeName(declared)+", not an object of servers")
				}
				for _, server := range servers {
					entry := asObject(server.Value)
					s.server(5, path, "mcpServers."+server.Key, func(key string) any { return record.Get(entry, key) }, base)
				}
			}
		}
	}
}

// userHooks is row 9.
func (s *scan) userHooks() {
	path := filepath.Join(s.o.CodexHome, "hooks.json")
	if document, ok := s.readJSON(path, "hooks.json"); ok {
		s.hookCommands(9, path, document, "")
	}
}

// retiredHookLink is the link a runtime installed before decision 66 carries beside crw; that
// runtime's crw, started under it, is its Stop hook and takes its settings as the next word.
const retiredHookLink = "crw-completion-hook"

// stopAdapterCall is one command of a Stop command that runs the Stop hook, found by the word
// naming it: crw hook, or an older runtime's crw-completion-hook link; the word after it is the
// settings a runtime installed before decision 66 reads, and --plugin-launch (or no word) the
// default settings.
type stopAdapterCall struct {
	argv     []shellWord
	at       int       // the word naming the hook: the link, or crw of crw hook
	crwHook  bool      // crw hook
	launcher bool      // crw hook --plugin-launch
	settings shellWord // the word after the hook (after crw hook); Written "" when there is none
}

// stopAdapterIn is the Stop adapter one command runs, if any.
func stopAdapterIn(argv []shellWord) (stopAdapterCall, bool) {
	for i, w := range argv {
		base := filepath.Base(w.Written)
		call := stopAdapterCall{argv: argv, at: i}
		next := i + 1
		if base == "crw" && i+1 < len(argv) && argv[i+1].Value == "hook" {
			call.crwHook, next = true, i+2
		} else if base != retiredHookLink {
			continue
		}
		if next < len(argv) {
			call.settings = argv[next]
		}
		call.launcher = call.settings.Value == "--plugin-launch"
		return call, true
	}
	return stopAdapterCall{}, false
}

// readStopCommand is the one reader of a Stop command, for the registration readings (which
// settings it reads) and the doctor (which hook it runs): the program is read through the
// grammar and judge (j, whose own reports still reach its report), and each command it runs
// that runs a Stop adapter is returned. unknown says why that list cannot be complete: the
// command holds a word or construct the grammar cannot judge, or runs a script that may run the
// adapter itself.
func readStopCommand(j argvJudge, command string) (calls []stopAdapterCall, unknown string) {
	report, seen := j.report, j.seen
	j.report = func(word string, e Executable) {
		if e.Kind == KindUnreadable && unknown == "" {
			unknown = "it holds " + strconv.Quote(word) + ", which this scan cannot judge"
		}
		if report != nil {
			report(word, e)
		}
	}
	j.seen = func(argv []shellWord, command Executable) {
		if seen != nil {
			seen(argv, command)
		}
		if call, ok := stopAdapterIn(argv); ok {
			calls = append(calls, call)
			return
		}
		if command.Kind == KindScript {
			unknown = "it runs " + strconv.Quote(argv[0].Written) + ", a script that may run the adapter with settings of its own"
		}
	}
	j.program(command)
	return calls, unknown
}

// settingsOf is the settings document one Stop hook call reads: the default settings for crw
// hook --plugin-launch and for a call naming none, and otherwise the word after the hook, which a
// runtime installed before decision 66 reads (a later one reads nothing then, so reading it
// only ever finds more).
func (s *scan) settingsOf(source, field string, call stopAdapterCall) stopSettings {
	named := call.settings
	one := stopSettings{source: source, field: field, path: filepath.Join(s.o.CodexHome, "crw-completion-hook.json")}
	switch {
	case call.launcher:
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

// scopeClaim is one record of the relay's scope registry.
type scopeClaim struct {
	path     string
	document Object
}

// stateDirectories settles every relay state directory whose daemon.json is read: the state root
// and each directory under it (a link to one included), CODEX_SESSION_RELAY_STATE made absolute
// as the relay makes it, and each stateDir the relay's scope registry records. The registry (the
// production one under the passwd entry's home, and CODEX_SESSION_RELAY_SCOPE_DIR) is where
// every daemon claims its scope whatever environment started it, so a daemon started with
// another --state is found there. With relayScope set, and CODEX_SESSION_RELAY_SCOPE_DIR set,
// that override is the only registry read, as it is the only one a relay started in this
// environment reads or claims its scope in (service.ResolveScope). Anything that cannot be
// listed, read or made absolute is unreadable.
func (s *scan) stateDirectories() {
	unknown := func(what, detail string) {
		s.unreadable = append(s.unreadable, what+": rows 3 and 6: "+detail+", so the relay state directories are not all known")
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
	registries := []string{s.o.ScopeRegistry, absolute(scopeDirEnv)}
	switch {
	case s.relayScope && s.o.Env.Get(scopeDirEnv) != "":
		// The override alone; one that cannot be made absolute was listed unknown above.
		registries = registries[1:]
	case registries[0] == "":
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
		s.registries = append(s.registries, registry)
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

// errGone is a process that exited (or became a zombie) while the reading looked at it.
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
// (and boot). An error other than errGone is a process table the reading could not read.
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

// readable is whether what an alive pid runs can be read: its executable and its command line.
// One that cannot be, of a process still there, is an error: what it runs is unknown.
func (s *scan) readable(pid int) error {
	base := filepath.Join(s.o.Proc, strconv.Itoa(pid))
	gone := func(err error) error {
		if _, statErr := s.procStat(pid); errors.Is(statErr, errGone) {
			return errGone
		}
		return err
	}
	if _, err := os.Readlink(filepath.Join(base, "exe")); err != nil {
		return gone(err)
	}
	if _, err := os.ReadFile(filepath.Join(base, "cmdline")); err != nil {
		return gone(err)
	}
	return nil
}

// daemons reads daemon.json in every state directory and every scope registry claim, and
// returns every alive recorded pid.
func (s *scan) daemons() map[int]bool {
	pids := map[int]bool{}
	records := append([]scopeClaim(nil), s.scopeClaims...)
	for _, directory := range s.states {
		path := filepath.Join(directory, "daemon.json")
		if document, ok := s.readJSON(path, "daemon.json"); ok {
			records = append(records, scopeClaim{path, document})
		}
	}
	for _, one := range records {
		for _, key := range [][2]string{{"pid", "startTicks"}, {"workerPid", "workerStartTicks"}} {
			// Absent or null records no process (a stopped daemon, no worker); anything but a
			// positive integer pid_t names a process this reading cannot find.
			raw := record.Get(one.document, key[0])
			n, ok := raw.(int64)
			if raw == nil {
				continue
			} else if !ok || n < 1 || n > math.MaxInt32 {
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+": "+pyvalue.TypeName(raw)+" "+scope.PyStr(raw)+" is not a pid, so the process the record names, and what it runs, is unknown")
				continue
			}
			pid := int(n)
			alive, err := s.alive(pid, record.Get(one.document, key[1]), record.Get(one.document, "bootId"))
			if err != nil {
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+" "+strconv.Itoa(pid)+": whether it is alive could not be read: "+store.PythonOSError(err))
				continue
			}
			if !alive {
				continue
			}
			pids[pid] = true
			if err := s.readable(pid); err != nil && !errors.Is(err, errGone) {
				s.unreadable = append(s.unreadable, one.path+": row 3 "+key[0]+" "+strconv.Itoa(pid)+": what the process runs could not be read: "+store.PythonOSError(err))
			}
		}
	}
	return pids
}

// configToml is row 10: every mcp_servers table, judged as Codex starts it (server).
func (s *scan) configToml(_ context.Context) {
	path := filepath.Join(s.o.CodexHome, "config.toml")
	read := reading.ReadText(path, "config.toml")
	switch {
	case read.State == reading.Absent:
		return
	case !read.OK():
		s.unreadable = append(s.unreadable, path+": "+read.Detail)
		return
	}
	var document map[string]any
	if _, err := toml.Decode(read.Value.(string), &document); err != nil {
		s.unreadable = append(s.unreadable, path+": "+err.Error())
		return
	}
	servers, ok := document["mcp_servers"].(map[string]any)
	if declared, present := document["mcp_servers"]; present && !ok {
		s.malformed(10, path, "mcp_servers", "mcp_servers is "+pyvalue.TypeName(declared)+", not a table of servers")
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
}
