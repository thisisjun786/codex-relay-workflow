package doctor

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
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
	if o.CodexHome == "" {
		o.CodexHome = CodexHome(o.Env)
	}
	if o.Destination == "" {
		o.Destination = DefaultDestination(o.Env)
	}
	root, err := record.Resolve(directory)
	if err != nil {
		return nil, []string{directory + ": the directory could not be resolved: " + err.Error()}
	}
	spelled := filepath.Clean(directory)
	seen := map[string]bool{}
	s := &scan{o: o, pointer: pointer.Path(o.Destination)}
	s.observe = func(row int, source, field, path string, e Executable) {
		var names string
		switch {
		case filepath.IsAbs(path) && record.Within(filepath.Clean(path), spelled):
			names = filepath.Clean(path)
		case e.Resolves != "" && record.Within(e.Resolves, root):
			names = e.Resolves
		default:
			return
		}
		key := strconv.Itoa(row) + "\x00" + source + "\x00" + field + "\x00" + path
		if seen[key] {
			return
		}
		seen[key] = true
		inside = append(inside, Object{
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
	return inside, s.unreadable
}
