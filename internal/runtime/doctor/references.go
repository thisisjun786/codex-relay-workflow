package doctor

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// RegisteredInside is every path the host's registrations name inside directory, and everything
// those registrations hold that could not be read or judged. The registrations are the retention
// scan's registration rows, read by the scan's own readers: every crw-*.json settings record
// (row 4: the Stop settings' relayExecutable, adapterEntryPoint and adapterInterpreter, the
// bridge record's bridgeExecutable), the cached plugin declarations (row 5), the
// crw-stop-hook.py launcher copy (row 8), hooks.json (row 9) and config.toml's mcp_servers
// (row 10), and the settings document each Stop command there reads when it is not one of
// row 4's. A path counts when it lies inside directory as written or once every link (the owned
// pointer included) is followed, and so does each interpreter and script a reference is followed
// through. What the host starts from one of these is started afresh by each new session, so no
// process table shows it between sessions. It writes nothing.
func RegisteredInside(ctx context.Context, o RetentionOptions, directory string) (inside []any, unreadable []string) {
	root, err := record.Resolve(directory)
	if err != nil {
		return nil, []string{directory + ": the directory could not be resolved: " + err.Error()}
	}
	spelled := filepath.Clean(directory)
	return RegisteredMatching(ctx, o, func(path, resolves string) string {
		switch {
		case filepath.IsAbs(path) && record.Within(filepath.Clean(path), spelled):
			return filepath.Clean(path)
		case resolves != "" && record.Within(resolves, root):
			return resolves
		}
		return ""
	})
}

// RegisteredMatching is RegisteredInside with the question put by the caller: inside answers,
// for a path a registration names (absolute, as written) and what it resolves to ("" when it
// could not be resolved), the path it counts as inside, or "" when it does not count. A caller
// that knows a directory by its file identity rather than its spelling asks through it.
func RegisteredMatching(ctx context.Context, o RetentionOptions, inside func(path, resolves string) string) (found []any, unreadable []string) {
	if o.CodexHome == "" {
		o.CodexHome = CodexHome(o.Env)
	}
	if o.Destination == "" {
		o.Destination = DefaultDestination(o.Env)
	}
	seen := map[string]bool{}
	s := &scan{o: o, pointer: pointer.Path(o.Destination)}
	s.observe = func(row int, source, field, path string, e Executable) {
		names := inside(path, e.Resolves)
		if names == "" {
			return
		}
		key := strconv.Itoa(row) + "\x00" + source + "\x00" + field + "\x00" + path
		if seen[key] {
			return
		}
		seen[key] = true
		found = append(found, Object{
			{Key: "row", Value: int64(row)}, {Key: "surface", Value: Surfaces[row-1].Name}, {Key: "source", Value: source},
			{Key: "field", Value: field}, {Key: "names", Value: path}, {Key: "inside", Value: names}, {Key: "kind", Value: e.Kind},
		})
	}
	s.settingsRecords() // row 4
	s.pluginCache()     // row 5, and the settings its Stop commands read
	s.launcherCopy()    // row 8
	s.userHooks()       // row 9, and the settings its Stop commands read
	s.configToml(ctx)   // row 10
	// Row 4 read every crw-*.json record in the Codex home; a Stop command that reads another
	// document has it read the same way.
	read := map[string]bool{}
	for _, settings := range s.stopSettings {
		name := filepath.Base(settings.path)
		switch {
		case settings.problem != "":
			s.unreadable = append(s.unreadable, settings.source+": "+settings.field+": which settings this Stop command reads could not be established: "+settings.problem)
		case read[settings.path] || filepath.Dir(settings.path) == filepath.Clean(o.CodexHome) && strings.HasPrefix(name, "crw-") && strings.HasSuffix(name, ".json"):
		default:
			read[settings.path] = true
			s.settingsRecord(settings.path)
		}
	}
	return found, s.unreadable
}

// RecordedDaemons is the retention scan's row 3 for a caller that removes a runtime: every relay
// daemon and worker recorded in a daemon.json (the relay state root, every scope under it,
// $CODEX_SESSION_RELAY_STATE, each state directory the relay's scope registry records, and the
// extra state directories given) or a scope registry claim that is alive - its pid present in
// this process table with the start time and boot id the record gives - and everything that
// could not be read, which leaves a recorded daemon unknown. A record written on another boot, or
// whose pid is gone or now another process, names nothing alive; a process in another PID
// namespace or on another host is not in this table at all, so it is not seen. It writes nothing.
func RecordedDaemons(o RetentionOptions, states ...string) (alive []int, unreadable []string) {
	if o.CodexHome == "" {
		o.CodexHome = CodexHome(o.Env)
	}
	if o.Destination == "" {
		o.Destination = DefaultDestination(o.Env)
	}
	if o.StateRoot == "" {
		o.StateRoot = scope.DefaultStateRoot(o.Env)
	}
	if o.Proc == "" {
		o.Proc = "/proc"
	}
	s := &scan{o: o, pointer: pointer.Path(o.Destination)}
	s.stateDirectories()
	for _, state := range states {
		if filepath.IsAbs(state) && !contains(s.states, state) {
			s.states = append(s.states, state)
		}
	}
	for pid := range s.daemons() {
		alive = append(alive, pid)
	}
	sort.Ints(alive)
	return alive, s.unreadable
}
