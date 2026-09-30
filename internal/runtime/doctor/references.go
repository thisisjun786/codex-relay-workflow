package doctor

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// RegisteredMatching is every path the host's registrations name that inside counts, and
// everything CRW's registrations hold that could not be read or judged (in hooks.json and
// config.toml, only an entry that is CRW's counts, decision 68). The registrations are every
// crw-*.json settings record (row 4: the Stop settings' relayExecutable, the bridge record's
// bridgeExecutable), the cached plugin declarations (row 5), hooks.json (row 9) and config.toml's
// mcp_servers (row 10), and the settings document each Stop command there reads when it is not
// one of row 4's; the row numbers are the retired retention scan's (decision 59), and row 8,
// the Python launcher copy, is retired (decision 67). inside answers, for a path a registration names (absolute, as written) and what it resolves to (""
// when it could not be resolved), the path it counts as, or "" when it does not count; each
// interpreter and script a reference is followed through is asked too. What the host starts from
// one of these is started afresh by each new session, so no process table shows it between
// sessions. It writes nothing.
func RegisteredMatching(ctx context.Context, o ScanOptions, inside func(path, resolves string) string) (found []any, unreadable []string) {
	if o.CodexHome == "" {
		o.CodexHome = CodexHome(o.Env)
	}
	if o.Destination == "" {
		o.Destination = DefaultDestination(o.Env)
	}
	seen := map[string]bool{}
	s := &scan{o: o, pointer: pointer.Path(o.Destination)}
	if resolved, err := filepath.EvalSymlinks(o.Destination); err == nil && resolved != o.Destination {
		s.destination = resolved
	}
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
			{Key: "row", Value: int64(row)}, {Key: "surface", Value: surfaces[row]}, {Key: "source", Value: source},
			{Key: "field", Value: field}, {Key: "names", Value: path}, {Key: "inside", Value: names}, {Key: "kind", Value: e.Kind},
		})
	}
	s.settingsRecords() // row 4
	s.pluginCache()     // row 5, and the settings its Stop commands read
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

// DaemonRecords is what RecordedDaemons read and found.
type DaemonRecords struct {
	// Registries are the scope registries whose claims were read, and States the relay state
	// directories whose daemon.json was read, in the order read.
	Registries, States []string
	// Alive are the recorded pids alive in this process table.
	Alive []int
	// Unreadable is everything that could not be read, which leaves a recorded daemon unknown.
	Unreadable []string
}

// RecordedDaemons is, for a caller that removes a runtime, every relay
// daemon and worker recorded in a daemon.json (the relay state root, every scope under it,
// $CODEX_SESSION_RELAY_STATE, each state directory the relay's scope registry records, and the
// extra state directories given) or a scope registry claim that is alive - its pid present in
// this process table with the start time and boot id the record gives - and everything that
// could not be read, which leaves a recorded daemon unknown. A record written on another boot, or
// whose pid is gone or now another process, names nothing alive; a process in another PID
// namespace or on another host is not in this table at all, so it is not seen. It writes nothing.
//
// The scope registry is the one the relay resolves in o.Env: $CODEX_SESSION_RELAY_SCOPE_DIR alone
// when it is set, as a relay started there reads and claims its scope in that one and no other,
// and otherwise the production one (o.ScopeRegistry, or under the passwd entry's home). A caller
// that removes a runtime finds a process running out of it in the process table, whichever
// registry recorded it.
func RecordedDaemons(o ScanOptions, states ...string) DaemonRecords {
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
	s := &scan{o: o, pointer: pointer.Path(o.Destination), relayScope: true}
	s.stateDirectories()
	for _, state := range states {
		if filepath.IsAbs(state) && !contains(s.states, state) {
			s.states = append(s.states, state)
		}
	}
	var alive []int
	for pid := range s.daemons() {
		alive = append(alive, pid)
	}
	sort.Ints(alive)
	return DaemonRecords{Registries: s.registries, States: s.states, Alive: alive, Unreadable: s.unreadable}
}
