package crwconfig

import (
	"fmt"
	"strings"
)

// ToolRoots are the roots a tool the product starts is told about: the build cache, the module
// cache and the build work directory of the go tool. An empty field is a root nothing configured,
// and the tool keeps its own default for it; the product passes the tool nothing.
type ToolRoots struct {
	BuildCache, ModuleCache, BuildWork string
}

// ToolRoots is the configured tool roots of this file: a root counts only when its path came from
// the configuration, because these roots have no default of their own.
func (f *File) ToolRoots() ToolRoots {
	var t ToolRoots
	for name, field := range map[string]*string{RootBuildCache: &t.BuildCache, RootModuleCache: &t.ModuleCache, RootBuildWork: &t.BuildWork} {
		if root := f.roots[name]; root.Set() {
			*field = root.Path
		}
	}
	return t
}

// toolRootNames are the roots ToolRoots carries, for a check limited to a tool command's roots.
var toolRootNames = []string{RootBuildCache, RootModuleCache, RootBuildWork}

// LoadToolRoots reads the configuration the way every command does and returns its tool roots. A
// tool root that fails a structural check is refused with the check's own words, so a product
// command does not start a tool against a cache it cannot use; a warning never refuses. A host with
// no file and no tool root gets the zero value.
func LoadToolRoots(getenv func(string) string, flagPath string) (ToolRoots, error) {
	file, err := Load(getenv, flagPath)
	if err != nil {
		return ToolRoots{}, err
	}
	tools := file.ToolRoots()
	if tools == (ToolRoots{}) {
		return tools, nil
	}
	if err := file.Refusal(getenv, toolRootNames...); err != nil {
		return ToolRoots{}, err
	}
	return tools, nil
}

// Env is the environment entries for the configured roots, GOCACHE, GOMODCACHE and GOTMPDIR in
// that order, and none for a root that is unset.
func (t ToolRoots) Env() []string {
	var env []string
	for _, entry := range []struct{ name, value string }{{"GOCACHE", t.BuildCache}, {"GOMODCACHE", t.ModuleCache}, {"GOTMPDIR", t.BuildWork}} {
		if entry.value != "" {
			env = append(env, entry.name+"="+entry.value)
		}
	}
	return env
}

// Apply returns env with each configured entry in place of any entry of the same name, appended at
// the end, and every other entry untouched and in order. With nothing configured it returns env
// as it is. The slice it is given is never changed.
func (t ToolRoots) Apply(env []string) []string {
	entries := t.Env()
	if len(entries) == 0 {
		return env
	}
	replaced := map[string]bool{}
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		replaced[name] = true
	}
	out := make([]string, 0, len(env)+len(entries))
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); !replaced[name] {
			out = append(out, kv)
		}
	}
	return append(out, entries...)
}

// String names the configured roots, for a message.
func (t ToolRoots) String() string {
	return fmt.Sprintf("build_cache=%q module_cache=%q build_work=%q", t.BuildCache, t.ModuleCache, t.BuildWork)
}
