package contracttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	relaycli "github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runCLI is the `cli` family of contract/README.md: each step spawns `crw relay --state
// <case>/state <argv>` as a real process under an isolated HOME/XDG/CODEX_HOME.
func runCLI(t *testing.T, scenario Scenario) (map[string]any, error) {
	t.Helper()
	if scenario.Domain == "sqlite-ddl" {
		return runSQLite(t, scenario)
	}
	// given.sql_seed goes through the real relay Store and given.host through the relay's
	// App Server socket; neither exists in Go yet.
	for _, key := range []string{"sql_seed", "host"} {
		if _, present := scenario.Given[key]; present {
			return nil, fmt.Errorf("%w: %s/cli given.%s", ErrNotPorted, scenario.Domain, key)
		}
	}
	binary, err := crwBinary()
	if err != nil {
		return nil, err
	}
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if err := writeFiles(home, scenario.Given["files"]); err != nil {
		return nil, err
	}
	if script, ok := scenario.Given["sql"].(string); ok {
		if err := seedSQL(state, script); err != nil {
			return nil, err
		}
	}
	steps, err := cliSteps(scenario.Run)
	if err != nil {
		return nil, err
	}
	// A scenario that drives a relay command the Go build has not registered yet is not ported:
	// it is counted as a skip (a failure under CRW_CONTRACT_STRICT=1), never run against usage.
	// relaycli.Registered is the union of both gates: the delivery commands (todo 21) and the
	// diagnostics commands of internal/relay/cli (todo 20: doctor, status, show, store-*).
	for _, step := range steps {
		if argv, _ := step["argv"].([]any); len(argv) > 0 {
			if name, ok := argv[0].(string); ok && !relaycli.Registered(name) {
				return nil, fmt.Errorf("%w: %s/cli command %q", ErrNotPorted, scenario.Domain, name)
			}
		}
	}
	results := map[string]any{}
	var last map[string]any
	for index, step := range steps {
		last, err = runStep(binary, home, step, results)
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", index, err)
		}
		id, ok := step["id"].(string)
		if !ok {
			id = strconv.Itoa(index)
		}
		results[id] = last
	}
	actual := map[string]any{"steps": results}
	for key, value := range last {
		actual[key] = value
	}
	files := map[string]any{}
	for name := range scenario.Expect.Files {
		_, err := os.Lstat(filepath.Join(home, name))
		files[name] = err == nil
	}
	actual["files"] = files
	observed := map[string]any{}
	for _, name := range scenario.Expect.Observe {
		if observed[name], err = observe(filepath.Join(home, name)); err != nil {
			return nil, err
		}
	}
	actual["observed"] = observed
	if len(scenario.Expect.Queries) > 0 {
		if actual["sql"], err = querySQL(state, scenario.Expect.Queries); err != nil {
			return nil, err
		}
	}
	return actual, nil
}

func cliSteps(run map[string]any) ([]map[string]any, error) {
	raw, ok := run["steps"].([]any)
	if !ok {
		return []map[string]any{run}, nil
	}
	steps := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		step, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: run.steps[%d] is not an object", ErrFixture, i)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

func runStep(binary, home string, step, results map[string]any) (map[string]any, error) {
	resolved, err := resolve(step, results, home)
	if err != nil {
		return nil, err
	}
	step = resolved.(map[string]any)
	argv, err := stringList(step["argv"])
	if err != nil {
		return nil, fmt.Errorf("argv: %w", err)
	}
	timeout := 60 * time.Second
	if seconds, ok := step["timeout"].(float64); ok {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append([]string{"relay", "--state", filepath.Join(home, "state")}, argv...)...)
	command.Dir = home
	if cwd, ok := step["cwd"].(string); ok {
		command.Dir = cwd
	}
	command.Env = os.Environ()
	if env, ok := step["env"].(map[string]any); ok {
		for key, value := range env {
			command.Env = append(command.Env, key+"="+fmt.Sprint(value))
		}
	}
	for key, dir := range map[string]string{
		"HOME": "", "XDG_STATE_HOME": "xdg-state", "XDG_DATA_HOME": "xdg-data",
		"XDG_CONFIG_HOME": "xdg-config", "CODEX_HOME": "codex-home", "CODEX_SESSION_RELAY_SCOPE_DIR": "scopes",
	} {
		command.Env = append(command.Env, key+"="+filepath.Join(home, dir))
	}
	switch stdin := step["stdin"].(type) {
	case nil:
	case string:
		command.Stdin = strings.NewReader(stdin)
	default:
		encoded, err := json.Marshal(stdin)
		if err != nil {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		command.Stdin = bytes.NewReader(encoded)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	exit := 0
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || ctx.Err() != nil {
			return nil, fmt.Errorf("crw %v: %w", argv, errors.Join(err, ctx.Err()))
		}
		exit = exitErr.ExitCode()
	}
	var parsed any
	if text := strings.TrimSpace(stdout.String()); text != "" {
		if json.Unmarshal([]byte(text), &parsed) != nil {
			parsed = nil // non-JSON stdout is observed as stdout_json null, as in the Python runner
		}
	}
	return map[string]any{"exit": float64(exit), "stdout": stdout.String(), "stderr": stderr.String(), "stdout_json": parsed}, nil
}

// resolve expands {"$step": id, "path": [...]} references and ${HOME} in strings.
func resolve(value any, results map[string]any, home string) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		if id, ok := v["$step"].(string); ok {
			result, present := results[id]
			if !present {
				return nil, fmt.Errorf("%w: reference to unknown step %q", ErrFixture, id)
			}
			path, _ := v["path"].([]any)
			return lookup(result, path)
		}
		out := make(map[string]any, len(v))
		for key, item := range v {
			resolved, err := resolve(item, results, home)
			if err != nil {
				return nil, err
			}
			out[key] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			resolved, err := resolve(item, results, home)
			if err != nil {
				return nil, err
			}
			out[i] = resolved
		}
		return out, nil
	case string:
		return strings.ReplaceAll(v, "${HOME}", home), nil
	default:
		return value, nil
	}
}

func stringList(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: expected an array, got %T", ErrFixture, value)
	}
	out := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%w: element %d is %T, not a string", ErrFixture, i, item)
		}
		out[i] = text
	}
	return out, nil
}

func writeFiles(home string, files any) error {
	entries, _ := files.(map[string]any)
	for name, contents := range entries {
		text, ok := contents.(string)
		if !ok {
			return fmt.Errorf("%w: given.files[%q] is not text", ErrFixture, name)
		}
		target := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("given.files: %w", err)
		}
		if err := os.WriteFile(target, []byte(strings.ReplaceAll(text, "${HOME}", home)), 0o644); err != nil {
			return fmt.Errorf("given.files: %w", err)
		}
	}
	return nil
}
