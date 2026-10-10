package crwconfig

import (
	"fmt"
	"os"
	"os/exec"
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
// no file and no tool root gets the zero value, and so does a host whose default configuration
// location cannot be named at all (no HOME, no XDG_CONFIG_HOME, no CRW_CONFIG): nothing can have
// configured it.
func LoadToolRoots(getenv func(string) string, flagPath string) (ToolRoots, error) {
	file, err := loadOptional(getenv, flagPath)
	if err != nil || file == nil {
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

// loadOptional is Load, except that a default configuration location that cannot be named (the
// home is unknown, so the default path is relative) is no configuration at all, not an error: a
// product process that has no home cannot have been configured through one. A location the caller
// named (a flag, CRW_CONFIG, XDG_CONFIG_HOME) that is unusable is still an error. It returns nil
// for no configuration.
func loadOptional(getenv func(string) string, flagPath string) (*File, error) {
	if flagPath == "" && getenv("CRW_CONFIG") == "" && getenv("XDG_CONFIG_HOME") == "" {
		if _, _, err := locate(getenv, ""); err != nil {
			return nil, nil
		}
	}
	return load(getenv, flagPath, false)
}

// Prepare makes the directories a tool needs to find in place before it starts. The go tool creates
// its build cache itself and fetches into the module cache on demand, but it makes its work
// directory with os.MkdirTemp directly below GOTMPDIR and fails when that directory is missing, so
// a configured build_work that does not exist yet is created here, owner-only. The validator that
// accepted the missing directory stays read-only; this is the preparation step of a command that is
// about to start the tool. A root nothing configured is left alone.
func (t ToolRoots) Prepare() error {
	if t.BuildWork == "" {
		return nil
	}
	if err := os.MkdirAll(t.BuildWork, 0o700); err != nil {
		return fmt.Errorf("build_work %q: %v", t.BuildWork, err)
	}
	return nil
}

// ApplyTo prepares the roots and puts the configured entries into cmd's environment, on top of
// cmd.Env when it has one and of this process's environment when it has none. With nothing
// configured it touches nothing, so a command that inherits the environment keeps doing so.
func (t ToolRoots) ApplyTo(cmd *exec.Cmd) error {
	if len(t.Env()) == 0 {
		return nil
	}
	if err := t.Prepare(); err != nil {
		return err
	}
	base := cmd.Env
	if base == nil {
		base = os.Environ()
	}
	cmd.Env = t.Apply(base)
	return nil
}

// ConfigureCommand reads the tool roots the configuration names, the way LoadToolRoots does, and
// applies them to cmd (ApplyTo). It is the one call a place that starts a tool makes when it holds
// no tool roots of its own.
func ConfigureCommand(cmd *exec.Cmd, getenv func(string) string) error {
	tools, err := LoadToolRoots(getenv, "")
	if err != nil {
		return err
	}
	return tools.ApplyTo(cmd)
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

// SetEnv returns env with name set to value: an entry of that name is replaced in place, and one is
// appended when there is none. The slice it is given is never changed.
func SetEnv(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	set := false
	for _, kv := range env {
		if key, _, _ := strings.Cut(kv, "="); key == name {
			if !set {
				out = append(out, name+"="+value)
				set = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !set {
		out = append(out, name+"="+value)
	}
	return out
}

// ExplicitFile is the configuration file this environment selects through CRW_CONFIG or
// XDG_CONFIG_HOME, when that file exists. A process started somewhere else (a systemd user manager has
// neither variable of the shell a service was registered from) finds only the default location, so a
// registration that must keep the configuration it used names this file for the process it starts. A
// file at the default location, a missing file and an unusable (relative) location return "": the
// other process finds the default itself, or nothing configured it.
func ExplicitFile(getenv func(string) string) string {
	if getenv("CRW_CONFIG") == "" && getenv("XDG_CONFIG_HOME") == "" {
		return ""
	}
	path, _, err := locate(getenv, "")
	if err != nil {
		return ""
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return path
}
