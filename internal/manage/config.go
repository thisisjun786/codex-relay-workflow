package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
)

// coreSourceDefault is where a value no input supplied came from; a later issue that
// reads the crw configuration file names the file and the key instead.
const coreSourceDefault = "default"

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

// Config is the configuration of a management session, with the defaults filled. A key
// this type does not name stays in the document and is read with Section.
type Config struct {
	ManagementThread string            `json:"management_thread"`
	Parents          map[string]string `json:"parents"`
	Repository       string            `json:"repository"`
	Relay            coreRelay         `json:"relay"`
	Bridge           coreBridge        `json:"bridge"`
	StateDir         string            `json:"state_dir"`
	Settings         coreSettingsBlock `json:"settings"`

	raw map[string]json.RawMessage // the document verbatim, for Section
}

// Section decodes the top-level key name of the configuration document into v. A key
// the document does not carry leaves v untouched and reports no error, so a subcommand
// reads its own section without this file knowing about it.
func (c *Config) Section(name string, v any) error {
	raw, ok := c.raw[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// coreDefaults is the configuration of a session with nothing configured.
func coreDefaults(e *Env) *Config {
	return &Config{
		Parents:  map[string]string{},
		Relay:    coreRelay{Socket: filepath.Join(coreHomeDir(e, "CODEX_HOME", ".codex"), "app-server-control", "app-server-control.sock")},
		StateDir: filepath.Join(coreHomeDir(e, "XDG_STATE_HOME", ".local/state"), "crw", "manage"),
		raw:      map[string]json.RawMessage{},
	}
}

// coreHomeDir is a per-user directory: the variable's value when it is set, else name
// below HOME.
func coreHomeDir(e *Env, variable, name string) string {
	if dir := e.Getenv(variable); dir != "" {
		return dir
	}
	return filepath.Join(e.Getenv("HOME"), name)
}

// coreConfigReport is what crw manage config writes: the configuration with the
// defaults filled, and where each defaulted value came from.
func coreConfigReport(c *Config) map[string]any {
	return map[string]any{
		"management_thread": c.ManagementThread,
		"parents":           c.Parents,
		"repository":        c.Repository,
		"relay":             c.Relay,
		"bridge":            c.Bridge,
		"state_dir":         c.StateDir,
		"settings":          c.Settings,
		"sources": map[string]string{
			"relay.socket":  coreSourceDefault,
			"state_dir":     coreSourceDefault,
			"bridge.binary": coreSourceDefault,
		},
	}
}

// coreConfigCommand is crw manage config.
var coreConfigCommand = Command{Name: "config", Summary: "print the configuration with its defaults filled", Run: coreRunConfig}

func init() { Register(coreConfigCommand) }

// coreRunConfig is crw manage config: the configuration as JSON on stdout. This issue
// reads no configuration file, so every value is the default.
func coreRunConfig(_ context.Context, e *Env, args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(e.Stderr, "usage: crw manage config")
		fmt.Fprintf(e.Stderr, "crw manage config: error: unexpected argument %q\n", args[0])
		return usageExit
	}
	data, err := json.MarshalIndent(coreConfigReport(coreDefaults(e)), "", "  ")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage config: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	return 0
}
