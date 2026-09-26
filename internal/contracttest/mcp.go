package contracttest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// runMCP is the `mcp` family of contract/README.md (contract/runner/mcp.py): the built crw is
// started as `crw bridge --socket <fake> --state-dir <case>/state` and driven over stdio by a
// real MCP client, against a fake App Server on a real unix socket.
//
// run.launcher starts it the way the plugin launcher does (plugins/crw/wiring/crw_bridge_mcp.py,
// replaced by a native launcher in todo 34): from the installed package directory with only
// HOME and PATH, the policy handed over through the two variables the launcher sets from its
// record. The launcher itself is not under test here; the bridge's behaviour under that bare
// environment is.
func runMCP(t *testing.T, scenario Scenario) (map[string]any, error) {
	t.Helper()
	binary, err := crwBinary()
	if err != nil {
		return nil, err
	}
	home := t.TempDir()
	if err := writeFiles(home, scenario.Given["files"]); err != nil {
		return nil, err
	}
	config := asObject(scenario.Given["host"])
	jsonCursors := config["cursor_format"] == "json_turns"
	resolvedConfig, err := resolve(withoutKey(config, "cursor_format"), nil, home)
	if err != nil {
		return nil, err
	}
	threadOrder, err := objectKeyOrder(scenario.Path, "given", "host", "threads")
	if err != nil {
		return nil, err
	}
	host := startMCPHost(t, asObject(resolvedConfig), threadOrder, jsonCursors)
	socket := host.server.SocketPath
	if scenario.Run["socket_alias"] == true {
		alias := filepath.Join(filepath.Dir(socket), "alias.sock")
		if err := os.Symlink(socket, alias); err != nil {
			return nil, err
		}
		socket = alias
	}
	state := filepath.Join(home, "state")
	command := func(socketPath string) (*exec.Cmd, error) {
		cmd := commandFor(binary, socketPath, state, home)
		if env, ok := scenario.Run["env"].(map[string]any); ok {
			resolved, err := resolve(env, nil, home)
			if err != nil {
				return nil, err
			}
			for key, value := range asObject(resolved) {
				cmd.Env = append(cmd.Env, key+"="+fmt.Sprint(value))
			}
		}
		if scenario.Run["launcher"] == true {
			if err := launcherEnvironment(cmd, home, scenario.Given["policy"]); err != nil {
				return nil, err
			}
		}
		return cmd, nil
	}

	if scenario.Run["process"] == true {
		cmd, err := command(socket)
		if err != nil {
			return nil, err
		}
		var stderr bytes.Buffer
		cmd.Stdin, cmd.Stderr = bytes.NewReader(nil), &stderr
		exit := 0
		if err := runBounded(cmd, 60*time.Second); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			exit = exitErr.ExitCode()
		}
		_, statErr := os.Stat(state)
		return map[string]any{"exit": float64(exit), "stderr": stderr.String(), "state_exists": statErr == nil, "calls": host.calls(t)}, nil
	}

	sessions, err := mcpSessions(scenario.Run)
	if err != nil {
		return nil, err
	}
	results := []any{}
	named := map[string]any{}
	tools := map[string]any{}
	for index, steps := range sessions {
		if index > 0 {
			socket = host.server.SocketPath // a restart reaches the endpoint by its real name
		}
		cmd, err := command(socket)
		if err != nil {
			return nil, err
		}
		if tools, err = driveSession(t, cmd, steps, host, home, named, &results); err != nil {
			return nil, err
		}
	}
	return map[string]any{"exit": float64(0), "tools": tools, "results": results, "calls": host.calls(t)}, nil
}

// objectKeyOrder is the order the file at path writes the keys of the object reached by keys,
// or nil when there is no such object.
func objectKeyOrder(path string, keys ...string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return nil, nil
		}
		if raw = object[key]; raw == nil {
			return nil, nil
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil
	}
	order := []string{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		order = append(order, token.(string))
		var skip json.RawMessage
		if err := decoder.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// commandFor is `crw bridge --socket <socket> --state-dir <state>` run in home under an
// environment isolated to home.
func commandFor(binary, socket, state, home string) *exec.Cmd {
	cmd := exec.Command(binary, "bridge", "--socket", socket, "--state-dir", state)
	cmd.Dir = home
	cmd.Env = isolatedEnv(home)
	return cmd
}

func withoutKey(object map[string]any, key string) map[string]any {
	out := shallow(object)
	delete(out, key)
	return out
}

// isolatedEnv is the process environment with HOME, XDG_* and CODEX_HOME moved under home, so
// no test process can reach the live state.
func isolatedEnv(home string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := cutEnv(entry)
		switch key {
		case "HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CODEX_HOME", execution.EnvPolicy, execution.EnvDigest:
			continue
		}
		env = append(env, entry)
	}
	for key, dir := range map[string]string{"HOME": "", "XDG_STATE_HOME": "xdg-state", "XDG_DATA_HOME": "xdg-data", "XDG_CONFIG_HOME": "xdg-config", "XDG_CACHE_HOME": "xdg-cache", "CODEX_HOME": "codex-home"} {
		env = append(env, key+"="+filepath.Join(home, dir))
	}
	return env
}

func cutEnv(entry string) (string, string, bool) {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return entry, "", false
}

// launcherEnvironment is what crw_bridge_mcp.py hands the bridge: the host's bare environment
// (HOME moved to a user home that holds no record, PATH) started in the installed package, plus
// the policy path and digest a version-2 record names.
func launcherEnvironment(cmd *exec.Cmd, home string, policy any) error {
	pkg := filepath.Join(home, "codex-home", "plugins", "cache", "crw", "crw", "0.0.0")
	userHome := filepath.Join(home, "user-home")
	for _, dir := range []string{pkg, userHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	cmd.Dir = pkg
	cmd.Env = []string{"HOME=" + userHome, "PATH=" + os.Getenv("PATH")}
	if policy == nil {
		return nil
	}
	resolved, err := resolve(policy, nil, home)
	if err != nil {
		return err
	}
	data, err := pythonDumps(resolved)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "execution-policy.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	cmd.Env = append(cmd.Env, execution.EnvPolicy+"="+path, execution.EnvDigest+"="+hex.EncodeToString(digest[:]))
	return nil
}

// pythonDumps is json.dumps(value) with its default separators, which is what the Python
// runner hashes. Key order is Go's sorted order; the digest only has to match the bytes written.
func pythonDumps(value any) ([]byte, error) {
	switch v := value.(type) {
	case map[string]any:
		out := []byte{'{'}
		keys := slices.Sorted(maps.Keys(v))
		for i, key := range keys {
			if i > 0 {
				out = append(out, ", "...)
			}
			encodedKey, _ := json.Marshal(key)
			item, err := pythonDumps(v[key])
			if err != nil {
				return nil, err
			}
			out = append(append(append(out, encodedKey...), ": "...), item...)
		}
		return append(out, '}'), nil
	case []any:
		out := []byte{'['}
		for i, element := range v {
			if i > 0 {
				out = append(out, ", "...)
			}
			item, err := pythonDumps(element)
			if err != nil {
				return nil, err
			}
			out = append(out, item...)
		}
		return append(out, ']'), nil
	default:
		return json.Marshal(v)
	}
}

func runBounded(cmd *exec.Cmd, limit time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("crw bridge did not exit within %s", limit)
	}
}

func mcpSessions(run map[string]any) ([][]map[string]any, error) {
	raw, ok := run["sessions"].([]any)
	if !ok {
		steps, _ := run["steps"].([]any)
		raw = []any{steps}
	}
	sessions := [][]map[string]any{}
	for _, session := range raw {
		items, _ := session.([]any)
		steps := []map[string]any{}
		for i, item := range items {
			step, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%w: mcp step %d is not an object", ErrFixture, i)
			}
			steps = append(steps, step)
		}
		sessions = append(sessions, steps)
	}
	return sessions, nil
}

// driveSession starts one server process, lists its tools and runs the steps in order. The
// process's stderr is kept and reported if the session fails.
func driveSession(t *testing.T, cmd *exec.Cmd, steps []map[string]any, host *mcpHost, home string, named map[string]any, results *[]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "crw-contracttest", Version: "0"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w (stderr %s)", err, stderr.String())
	}
	defer func() { _ = session.Close() }()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	tools := map[string]any{}
	for _, tool := range listed.Tools {
		schema, err := jsonValue(tool.InputSchema)
		if err != nil {
			return nil, err
		}
		tools[tool.Name] = schema
	}
	for _, step := range steps {
		if set, ok := step["host_set"].(map[string]any); ok {
			path, _ := set["path"].([]any)
			value, err := resolve(set["value"], named, home)
			if err != nil {
				return nil, err
			}
			if err := host.set(path, value); err != nil {
				return nil, err
			}
			continue
		}
		arguments, err := resolve(valueOr(step["arguments"], map[string]any{}), named, home)
		if err != nil {
			return nil, err
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: stringValue(step["tool"]), Arguments: arguments})
		if err != nil {
			return nil, fmt.Errorf("tools/call %v: %w (stderr %s)", step["tool"], err, stderr.String())
		}
		observed, err := observeCall(result)
		if err != nil {
			return nil, err
		}
		*results = append(*results, observed)
		id, ok := step["id"].(string)
		if !ok {
			id = strconv.Itoa(len(*results) - 1)
		}
		named[id] = observed
	}
	return tools, nil
}

// observeCall is the Python runner's {"error", "content", "structured"} view of one result.
func observeCall(result *sdk.CallToolResult) (map[string]any, error) {
	content := []any{}
	for _, part := range result.Content {
		value, err := jsonValue(part)
		if err != nil {
			return nil, err
		}
		content = append(content, value)
	}
	structured, err := jsonValue(result.StructuredContent)
	if err != nil {
		return nil, err
	}
	return map[string]any{"error": result.IsError, "content": content, "structured": structured}, nil
}

func jsonValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out any
	return out, json.Unmarshal(raw, &out)
}
