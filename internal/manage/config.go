package manage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// coreConfigUsage is what crw manage config prints for -h.
const coreConfigUsage = "usage: crw manage config [--config <path>]"

// coreSettings is one settings block: the model and the reasoning effort.
type coreSettings struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

// coreSettingsBlock holds the management session's settings and the parent's.
type coreSettingsBlock struct {
	Management coreSettings `json:"management"`
	Parent     coreSettings `json:"parent"`
}

// coreRelay is the relay block: the state directory and the App Server socket.
type coreRelay struct {
	State  string `json:"state"`
	Socket string `json:"socket"`
}

// coreBridge is the bridge block: the bridge binary (empty means the same executable's
// bridge mode) and the execution policy.
type coreBridge struct {
	Binary          string `json:"binary"`
	ExecutionPolicy string `json:"execution_policy"`
}

// Config is the configuration of a management session, with the defaults filled: the
// manage object of the crw configuration file. A key of that object this type does not
// name stays in the document and is read with Section.
type Config struct {
	ManagementThread string            `json:"management_thread"`
	Parents          map[string]string `json:"parents"`
	Repository       string            `json:"repository"`
	Relay            coreRelay         `json:"relay"`
	Bridge           coreBridge        `json:"bridge"`
	StateDir         string            `json:"state_dir"`
	Settings         coreSettingsBlock `json:"settings"`

	path   string                     // the configuration file's path
	source crwconfig.Source           // where that path came from: flag, env or default
	raw    map[string]json.RawMessage // the manage object verbatim, for Section
}

// Section decodes the top-level key name of the manage object into v. A key the document
// does not carry leaves v untouched and reports no error, so a subcommand reads its own
// section without this file knowing about it.
func (c *Config) Section(name string, v any) error {
	raw, ok := c.raw[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// coreConfigState is one Env's configuration resolution: the configuration and, when the
// file could not be used, why. The configuration is always present, so a caller that runs
// before the refusal is reported still holds the defaults.
type coreConfigState struct {
	cfg *Config
	err error
}

// coreConfigMemoMu guards coreConfigMemo. It covers the map's own reads and writes and the
// one resolution an Env makes, so two Envs resolve in turn rather than reading one file at
// once; a file read is short and local.
var coreConfigMemoMu sync.Mutex

// coreConfigMemo is the configuration each Env resolved, remembered for the rest of that
// Env's life: Run builds one Env per invocation, so the map holds one live entry per run.
// It is keyed by Env because Env is declared in another issue's file and may not gain a
// field here.
var coreConfigMemo = map[*Env]coreConfigState{}

// coreDefaults is the configuration of a management session: the manage object of the crw
// configuration file with the defaults filled, or the defaults alone when no file is
// there. It is resolved once per Env and reused, so every subcommand of one invocation
// reads the same configuration. A file this product cannot use leaves the defaults in
// place; coreConfigError reports it and Run refuses before it dispatches.
func coreDefaults(e *Env) *Config { return coreConfigStateOf(e).cfg }

// coreConfigError is why the configuration file could not be used, or nil.
func coreConfigError(e *Env) error { return coreConfigStateOf(e).err }

// coreConfigStateOf is this Env's configuration, resolved on first use.
func coreConfigStateOf(e *Env) coreConfigState {
	coreConfigMemoMu.Lock()
	defer coreConfigMemoMu.Unlock()
	if state, ok := coreConfigMemo[e]; ok {
		return state
	}
	state := coreLoad(e, "")
	coreConfigMemo[e] = state
	return state
}

// coreLoad resolves the configuration file and reads its manage object. flagPath is the
// --config value when the command line carried one, empty otherwise; the location chain
// itself belongs to crwconfig (--config, then CRW_CONFIG, then the configuration home
// below ${XDG_CONFIG_HOME:-$HOME/.config}), so manage never looks for a file of its own.
func coreLoad(e *Env, flagPath string) coreConfigState {
	file, err := crwconfig.Load(e.Getenv, flagPath)
	if err != nil {
		return coreConfigState{cfg: coreDefaultConfig(e, nil), err: err}
	}
	path, source := file.Path()
	cfg := coreDefaultConfig(e, file)
	cfg.path, cfg.source = path, source
	manage, err := coreManageSection(file, path)
	if err != nil {
		return coreConfigState{cfg: cfg, err: err}
	}
	cfg.raw = manage
	for _, field := range []struct {
		key  string
		into any
	}{
		{"management_thread", &cfg.ManagementThread},
		{"parents", &cfg.Parents},
		{"repository", &cfg.Repository},
		{"relay", &cfg.Relay},
		{"bridge", &cfg.Bridge},
		{"state_dir", &cfg.StateDir},
		{"settings", &cfg.Settings},
	} {
		raw, ok := manage[field.key]
		if !ok || string(bytes.TrimSpace(raw)) == "null" {
			continue
		}
		if err := json.Unmarshal(raw, field.into); err != nil {
			return coreConfigState{cfg: cfg, err: fmt.Errorf("%s: manage.%s: %v", path, field.key, err)}
		}
	}
	// A key that is there but empty is not a value: the install layout's defaults stand.
	if cfg.StateDir == "" {
		cfg.StateDir = coreManageStateRoot(e, file)
	}
	if cfg.Relay.Socket == "" {
		cfg.Relay.Socket = coreSocketPath(e)
	}
	return coreConfigState{cfg: cfg}
}

// coreLoadFile is coreLoad as a caller that wants the configuration and the refusal apart.
func coreLoadFile(e *Env, flagPath string) (*Config, error) {
	state := coreLoad(e, flagPath)
	return state.cfg, state.err
}

// coreManageSection is the configuration document's top-level manage object, verbatim. A
// file without the key keeps the defaults; a key that is there must be a JSON object, and
// null, an array, a string or a number is not one.
func coreManageSection(file *crwconfig.File, path string) (map[string]json.RawMessage, error) {
	var raw json.RawMessage
	if err := file.Section("manage", &raw); err != nil {
		return nil, fmt.Errorf("%s: manage: %v", path, err)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%s: manage is not a JSON object", path)
	}
	manage := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &manage); err != nil {
		return nil, fmt.Errorf("%s: manage is not a JSON object: %v", path, err)
	}
	return manage, nil
}

// coreDefaultConfig is the configuration with no file, or with a file that names nothing:
// the state directory crwconfig resolves for manage_state, and the App Server socket below
// the Codex home.
func coreDefaultConfig(e *Env, file *crwconfig.File) *Config {
	return &Config{
		Parents:  map[string]string{},
		Relay:    coreRelay{Socket: coreSocketPath(e)},
		StateDir: coreManageStateRoot(e, file),
		raw:      map[string]json.RawMessage{},
	}
}

// coreSocketPath is the App Server socket's default.
func coreSocketPath(e *Env) string {
	return filepath.Join(coreHomeDir(e, "CODEX_HOME", ".codex"), "app-server-control", "app-server-control.sock")
}

// coreManageStateRoot is the state directory's default: crwconfig's manage_state root,
// which follows XDG_STATE_HOME and the paths overrides the file itself carries. A file
// that could not be read falls back to the roots the environment alone resolves.
func coreManageStateRoot(e *Env, file *crwconfig.File) string {
	if file != nil {
		return file.Roots()[crwconfig.RootManage].Path
	}
	roots, err := crwconfig.Resolve(e.Getenv, nil)
	if err != nil {
		return filepath.Join(coreHomeDir(e, "XDG_STATE_HOME", ".local/state"), "crw", "manage")
	}
	return roots[crwconfig.RootManage].Path
}

// coreHomeDir is a per-user directory: the variable's value when it is set, else name
// below HOME.
func coreHomeDir(e *Env, variable, name string) string {
	if dir := e.Getenv(variable); dir != "" {
		return dir
	}
	return filepath.Join(e.Getenv("HOME"), name)
}

// coreConfigReport is what crw manage config writes: the configuration with the defaults
// filled, and the file it came from with where that path came from.
func coreConfigReport(c *Config) map[string]any {
	return map[string]any{
		"management_thread": c.ManagementThread,
		"parents":           c.Parents,
		"repository":        c.Repository,
		"relay":             c.Relay,
		"bridge":            c.Bridge,
		"state_dir":         c.StateDir,
		"settings":          c.Settings,
		"config": map[string]string{
			"path":   c.path,
			"source": string(c.source),
		},
	}
}

// coreConfigCommand is crw manage config.
var coreConfigCommand = Command{Name: "config", Summary: "print the configuration with its defaults filled", Run: coreRunConfig}

func init() { Register(coreConfigCommand) }

// coreRunConfig is crw manage config [--config <path>]: the configuration as JSON on
// stdout, with the file it came from and where that path came from. A configuration this
// product cannot use is exit 2, and an output write that fails is exit 1, because a
// truncated report must not read as one.
func coreRunConfig(_ context.Context, e *Env, args []string) int {
	flagPath := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config":
			if i+1 >= len(args) {
				fmt.Fprintln(e.Stderr, coreConfigUsage)
				fmt.Fprintln(e.Stderr, "crw manage config: error: argument --config: expected one argument")
				return usageExit
			}
			i++
			flagPath = args[i]
			if flagPath == "" {
				fmt.Fprintln(e.Stderr, coreConfigUsage)
				fmt.Fprintln(e.Stderr, "crw manage config: error: argument --config: expected a non-empty path")
				return usageExit
			}
		case strings.HasPrefix(args[i], "--config="):
			flagPath = strings.TrimPrefix(args[i], "--config=")
			if flagPath == "" {
				fmt.Fprintln(e.Stderr, coreConfigUsage)
				fmt.Fprintln(e.Stderr, "crw manage config: error: argument --config: expected a non-empty path")
				return usageExit
			}
		case args[i] == "-h" || args[i] == "--help":
			fmt.Fprintln(e.Stdout, coreConfigUsage)
			return 0
		default:
			fmt.Fprintln(e.Stderr, coreConfigUsage)
			fmt.Fprintf(e.Stderr, "crw manage config: error: unexpected argument %q\n", args[i])
			return usageExit
		}
	}
	// --config names the file itself, so the default location is not even read: the same
	// precedence crwconfig applies, where the flag wins before CRW_CONFIG and the
	// configuration home are looked at. Without one, the file every other subcommand reads
	// is the one reported.
	state := coreConfigStateOf(e)
	if flagPath != "" {
		state = coreLoad(e, flagPath)
	}
	if state.err != nil {
		fmt.Fprintf(e.Stderr, "crw manage config: error: %v\n", state.err)
		return usageExit
	}
	data, err := json.MarshalIndent(coreConfigReport(state.cfg), "", "  ")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage config: error: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(e.Stdout, "%s\n", data); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage config: error: write output: %v\n", err)
		return 1
	}
	return 0
}
