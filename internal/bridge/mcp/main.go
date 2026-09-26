package mcp

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// PackageVersion is codex_thread_bridge.__version__, which --version prints.
const PackageVersion = "0.1.0"

const description = "STDIO MCP entry point. No daemon startup or client configuration changes."

// Defaults are server.py main()'s: the socket under CODEX_HOME (or ~/.codex) and the ledger
// under XDG_STATE_HOME (or ~/.local/state). They are computed from env and never opened here.
func Defaults(env map[string]string) (socket, state string) {
	home := env["HOME"]
	codexHome, ok := env["CODEX_HOME"]
	if !ok {
		codexHome = filepath.Join(home, ".codex")
	}
	stateHome, ok := env["XDG_STATE_HOME"]
	if !ok {
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(codexHome, "app-server-control", "app-server-control.sock"), filepath.Join(stateHome, "codex-thread-bridge")
}

// Environ turns os.Environ-shaped entries into a map.
func Environ(entries []string) map[string]string {
	env := map[string]string{}
	for _, entry := range entries {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	return env
}

// Main is `crw bridge` (and `codex-thread-bridge`): server.py main(). stdout carries MCP frames
// and nothing else; usage, refusals and logs go to stderr. It returns the process exit code.
func Main(ctx context.Context, args []string, env map[string]string, stdin io.ReadCloser, stdout io.WriteCloser, stderr io.Writer) int {
	socket, state := Defaults(env)
	flags := flag.NewFlagSet("crw bridge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&socket, "socket", socket, "Existing App Server Unix WebSocket socket")
	flags.StringVar(&state, "state-dir", state, "Private durable operation ledger (keep across restarts)")
	flags.StringVar(&state, "state", state, "Alias of --state-dir")
	version := flags.Bool("version", false, "show program's version number and exit")
	flags.Usage = func() {
		fmt.Fprintf(stderr, "usage: crw bridge [-h] [--version] [--socket SOCKET] [--state-dir STATE_DIR]\n\n%s\n\noptions:\n", description)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw bridge: unrecognized arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *version {
		// argparse's version action prints to stdout; no protocol has started.
		fmt.Fprintln(stdout, PackageVersion)
		return 0
	}
	// Loaded before the ledger is opened and before any tool exists: a configured policy that
	// cannot be used stops the server instead of degrading to presence-only.
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	canonical, store, err := ledger.Endpoint(expand(socket, env), expand(state, env))
	if err != nil {
		fmt.Fprintln(stderr, "crw bridge:", err)
		return 1
	}
	defer store.Close()
	client := appserver.New(canonical, appserver.DefaultBounds)
	defer client.Close()
	server := NewServer(bridge.New(client, store, policy), PackageVersion, stderr)
	if err := server.Run(ctx, pythonTransport{&sdk.IOTransport{Reader: stdin, Writer: stdout}}); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fmt.Fprintln(stderr, "crw bridge:", err)
		return 1
	}
	return 0
}

// expand is Path.expanduser for a leading "~".
func expand(path string, env map[string]string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		return filepath.Join(env["HOME"], strings.TrimPrefix(path, "~"))
	}
	return path
}

// Run is Main over this process's own streams and environment.
func Run(ctx context.Context, args []string) int {
	return Main(ctx, args, Environ(os.Environ()), os.Stdin, os.Stdout, os.Stderr)
}
