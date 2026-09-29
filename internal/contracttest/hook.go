package contracttest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// runHook drives the real binary and a control.sock peer. The peer supplies the
// fixture's guard result, not the adapter decision or its filesystem effects.
func runHook(t *testing.T, s Scenario) (map[string]any, error) {
	if s.Domain != "hook" {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotPorted, s.Domain, s.Kind)
	}
	relay, hasRelay := s.Given["relay"].(map[string]any)
	steps, err := cliSteps(s.Run)
	if err != nil {
		return nil, err
	}
	for _, step := range steps {
		if step["kind"] == "mutate" {
			return nil, fmt.Errorf("%w: stop-events record mutation", ErrNotPorted)
		}
	}
	bin, err := crwBinary()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "crw-hc-")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Error(err)
		}
	})
	expand := func(v string) string { return strings.ReplaceAll(v, "${HOME}", home) }
	config := map[string]any{"configVersion": 1, "event": "Stop", "relayExecutable": home + "/codex-session-relay", "markerRoot": home + "/marker", "dbPath": nil, "mode": "observe", "timeoutSeconds": float64(5), "journalRoot": home + "/journal", "journalPolicy": "every_invocation", "installedBy": "CRW-37", "isolationAssertedBy": nil}
	if hasRelay {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' %s\nexit %d\n", shellSingleQuote(fmt.Sprint(relay["stdout"])), int(number(relay["exit"])))
		if err = os.WriteFile(home+"/codex-session-relay", []byte(script), 0o700); err != nil {
			return nil, err
		}
	}
	if overrides, ok := s.Given["settings_overrides"].(map[string]any); ok {
		for k, v := range overrides {
			if str, ok := v.(string); ok {
				v = expand(str)
			}
			config[k] = v
		}
	}
	// Literal Python fixture paths (e.g. /tmp/relay.sqlite) were only argv to
	// a fake subprocess. Native RPC needs a private owner directory instead of
	// binding a shared /tmp/control.sock. Preserve the legacy argv observation.
	legacyDB := config["dbPath"]
	if db, ok := legacyDB.(string); ok && db != "" && s.Kind != "status" && !strings.HasPrefix(db, home+string(filepath.Separator)) {
		config["dbPath"] = filepath.Join(home, "fixture-state", filepath.Base(db))
	}
	if s.Kind == "stop" && config["owner"] == nil {
		config["owner"] = "plugin"
		config["adapterInterpreter"] = "/retained/python"
		config["adapterEntryPoint"] = "/retained/stopadapter.py"
	}
	settings := filepath.Join(home, "crw-completion-hook.json")
	if s.Given["settings"] != false {
		raw, _ := json.Marshal(config)
		if err = os.WriteFile(settings, raw, 0600); err != nil {
			return nil, err
		}
	}
	if err = writeHookFiles(home, s.Given["files"]); err != nil {
		return nil, err
	}
	if err = writeHookSymlinks(home, s.Given["symlinks"]); err != nil {
		return nil, err
	}
	if err = applyHookModes(home, s.Given["modes"]); err != nil {
		return nil, err
	}
	env := os.Environ()
	for _, kv := range []string{"HOME=" + home, "CODEX_HOME=" + home, "XDG_STATE_HOME=" + home + "/xdg", "CODEX_SESSION_RELAY_STATE=" + home + "/state"} {
		key, _, _ := strings.Cut(kv, "=")
		env = slices.DeleteFunc(env, func(v string) bool { return strings.HasPrefix(v, key+"=") })
		env = append(env, kv)
	}
	calls := []any{}
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var listener net.Listener
	var workers sync.WaitGroup
	var serverErr error
	// Published before stdin is delivered, so the post-claim request can kill its
	// owner without racing process startup or relying on a sleep.
	firstProcess := make(chan *os.Process, 1)
	if hasRelay && s.Kind != "status" {
		state := home + "/state"
		if db, ok := config["dbPath"].(string); ok && db != "" {
			state = filepath.Dir(db)
		}
		if err = os.MkdirAll(state, 0700); err != nil {
			return nil, err
		}
		listener, err = net.Listen("unix", state+"/control.sock")
		if err != nil {
			return nil, err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if p := recover(); p != nil {
					mu.Lock()
					serverErr = fmt.Errorf("control peer panic: %v", p)
					mu.Unlock()
				}
			}()
			for {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer conn.Close()
					defer func() {
						if p := recover(); p != nil {
							mu.Lock()
							serverErr = fmt.Errorf("control peer panic: %v", p)
							mu.Unlock()
						}
					}()
					_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
					raw, e := bufio.NewReader(conn).ReadBytes('\n')
					if e != nil {
						return
					}
					var request map[string]any
					if e = json.Unmarshal(raw, &request); e != nil {
						mu.Lock()
						serverErr = e
						mu.Unlock()
						return
					}
					params, ok := request["params"].(map[string]any)
					if !ok {
						mu.Lock()
						serverErr = fmt.Errorf("missing params")
						mu.Unlock()
						return
					}
					stdin, _ := json.Marshal(params["stopInput"])
					argv := []any{"guard-evaluate", "--marker-root", params["markerRoot"]}
					if db := params["dbPath"]; db != nil { // The native route pins a DB; only expose a legacy --db-path if the fixture configured it.
						if overrides, ok := s.Given["settings_overrides"].(map[string]any); ok && overrides["dbPath"] != nil {
							argv = append(argv, "--db-path", legacyDB)
						}
					}
					if params["mode"] == "hold" {
						argv = append(argv, "--mode", "hold")
					}
					mu.Lock()
					calls = append(calls, map[string]any{"argv": argv, "stdin": string(stdin)})
					callNumber := len(calls)
					mu.Unlock()
					if truthy(relay["die_first"]) && callNumber == 1 {
						select {
						case process := <-firstProcess:
							if e := process.Kill(); e != nil {
								mu.Lock()
								serverErr = e
								mu.Unlock()
							}
						case <-ctx.Done():
							mu.Lock()
							serverErr = ctx.Err()
							mu.Unlock()
						}
						return
					}
					if number(relay["exit"]) == 2 && relay["stdout"] == "" {
						_, _ = io.WriteString(conn, "{\"protocol\":1,\"requestRejected\":true}\n")
						return
					}
					if number(relay["delay"]) > 0 {
						_, _ = io.Copy(io.Discard, conn)
						return
					}
					response, _ := relay["stdout"].(string)
					_, _ = io.WriteString(conn, response+"\n")
				}()
			}
		}()
	}
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
		workers.Wait()
	}()
	if s.Kind == "status" {
		status := hook.Status(ctx, home, envMap(env), "Stop")
		return map[string]any{"exit": float64(0), "stdout": "", "stderr": "", "stdout_json": nil, "status": status, "files": expectedFiles(home, s.Expect.Files), "observed": map[string]any{}}, nil
	}
	outcomes := []any{}
	started := time.Now()
	for stepIndex, step := range steps {
		if step["kind"] == "verify" {
			// The per-event judge over what the steps before it wrote (crw-dev stop-events).
			outcome, err := runVerify(ctx, step, env, expand)
			if err != nil {
				return nil, err
			}
			outcomes = append(outcomes, outcome)
			continue
		}
		payload := step["stdin"]
		if payload == nil {
			payload = map[string]any{"cwd": "/tmp/workspace", "hook_event_name": "Stop", "last_assistant_message": "I finished the task.", "model": "test-model", "permission_mode": "default", "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688", "stop_hook_active": false, "transcript_path": "/tmp/transcript.jsonl", "turn_id": "turn-1"}
		}
		if index, ok := step["transcript_stop"].(float64); ok {
			root, _ := Root()
			raw, e := os.ReadFile(filepath.Join(root, "packages/codex-session-relay/tests/fixtures/stop_event_r1.json"))
			if e != nil {
				return nil, e
			}
			var fixture struct {
				TranscriptLines []string
				Stops           []struct {
					Payload     map[string]any
					LinesAtStop int
				}
			}
			if e = json.Unmarshal(raw, &fixture); e != nil {
				return nil, e
			}
			stop := fixture.Stops[int(index)]
			path := home + "/transcript.jsonl"
			if e = os.WriteFile(path, []byte(strings.Join(fixture.TranscriptLines[:stop.LinesAtStop], "\n")+"\n"), 0600); e != nil {
				return nil, e
			}
			stop.Payload["transcript_path"] = path
			stop.Payload["cwd"] = home
			payload = stop.Payload
		}
		var raw []byte
		if str, ok := payload.(string); ok {
			raw = []byte(expand(str))
		} else {
			raw, _ = json.Marshal(payload)
		}
		count := max(1, int(number(step["concurrency"])))
		type running struct {
			cmd      *exec.Cmd
			out, err bytes.Buffer
		}
		processes := make([]*running, 0, count)
		for range count {
			args := []string{"hook"}
			if values, ok := step["argv"].([]any); ok {
				for _, arg := range values {
					args = append(args, expand(arg.(string)))
				}
			}
			if named, ok := step["settings"].(string); ok {
				args = append(args, expand(named))
			}
			p := &running{cmd: exec.CommandContext(ctx, bin, args...)}
			p.cmd.Env = env
			p.cmd.Stdin = bytes.NewReader(raw)
			p.cmd.Stdout = &p.out
			p.cmd.Stderr = &p.err
			if e := p.cmd.Start(); e != nil {
				return nil, e
			}
			if truthy(relay["die_first"]) && stepIndex == 0 && len(processes) == 0 {
				firstProcess <- p.cmd.Process
			}
			processes = append(processes, p)
		}
		for _, p := range processes {
			e := p.cmd.Wait()
			code := 0
			var signal any
			if e != nil {
				var exit *exec.ExitError
				if !errors.As(e, &exit) {
					return nil, e
				}
				code = exit.ExitCode()
				if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					signal = float64(status.Signal())
					code = -int(status.Signal())
				}
			}
			var parsed any
			if p.out.Len() > 0 {
				_ = json.Unmarshal(p.out.Bytes(), &parsed)
			}
			outcomes = append(outcomes, map[string]any{"exit": float64(code), "stdout": p.out.String(), "stderr": p.err.String(), "stdout_json": parsed, "timeout": false, "signal": signal})
		}
	}
	if listener != nil {
		_ = listener.Close()
		listener = nil
	}
	workers.Wait()
	if serverErr != nil {
		return nil, serverErr
	}
	actual := map[string]any{}
	for k, v := range outcomes[len(outcomes)-1].(map[string]any) {
		actual[k] = v
	}
	actual["outcomes"] = outcomes
	actual["elapsed"] = time.Since(started).Seconds()
	actual["calls"] = calls
	actual["call"] = nil
	if len(calls) > 0 {
		actual["call"] = calls[0]
	}
	rows, rowFiles, claims, ledgerOutcomes := []any{}, []any{}, []any{}, []any{}
	acceptances := []string{}
	paths, _ := filepath.Glob(home + "/journal/[0-9]*/*.json")
	for _, p := range paths {
		raw, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		var row map[string]any
		if e = json.Unmarshal(raw, &row); e != nil {
			return nil, e
		}
		rows = append(rows, row)
		acceptances = append(acceptances, fmt.Sprint(row["acceptance"]))
		info, e := os.Stat(p)
		if e != nil {
			return nil, e
		}
		rowFiles = append(rowFiles, map[string]any{"name": filepath.Base(p), "day": filepath.Base(filepath.Dir(p)), "mode": float64(info.Mode().Perm())})
	}
	paths, _ = filepath.Glob(home + "/journal/accepted/*.json")
	for _, p := range paths {
		raw, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		var value any
		if e = json.Unmarshal(raw, &value); e != nil {
			return nil, e
		}
		if strings.HasSuffix(p, ".outcome.json") {
			ledgerOutcomes = append(ledgerOutcomes, value)
		} else {
			claims = append(claims, value)
		}
	}
	slices.Sort(acceptances)
	accepts := make([]any, len(acceptances))
	for i, v := range acceptances {
		accepts[i] = v
	}
	hostClaims := map[string]any{}
	paths, _ = filepath.Glob(home + "/crw-completion-hook/stop-events/*.json")
	for _, p := range paths {
		raw, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		var value any
		if e = json.Unmarshal(raw, &value); e != nil {
			return nil, e
		}
		hostClaims[filepath.Base(p)] = value
	}
	actual["host_claims"] = hostClaims
	actual["home"] = home
	actual["rows"] = rows
	actual["row_files"] = rowFiles
	actual["claims"] = claims
	actual["ledger_outcomes"] = ledgerOutcomes
	actual["acceptances"] = accepts
	actual["files"] = map[string]any{}
	actual["observed"] = map[string]any{}
	if truthy(s.Run["status"]) {
		actual["status"] = hook.Status(ctx, home, envMap(env), "Stop")
	}
	return actual, nil
}
func number(v any) float64 { n, _ := v.(float64); return n }

// runVerify runs `crw-dev stop-events` with a verify step's argv, as the Python runner runs
// scripts/stop_events.py.
func runVerify(ctx context.Context, step map[string]any, env []string, expand func(string) string) (map[string]any, error) {
	bin, err := crwDevBinary()
	if err != nil {
		return nil, err
	}
	args := []string{"stop-events"}
	if values, ok := step["argv"].([]any); ok {
		for _, arg := range values {
			args = append(args, expand(arg.(string)))
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	code := 0
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return nil, err
		}
		code = exit.ExitCode()
	}
	var parsed any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("crw-dev stop-events printed no reading (exit %d): %w\n%s", code, err, errs.String())
	}
	return map[string]any{"exit": float64(code), "stdout": out.String(), "stderr": errs.String(), "stdout_json": parsed, "timeout": false, "signal": nil}, nil
}

func writeHookFiles(home string, files any) error {
	entries, _ := files.(map[string]any)
	python, err := exec.LookPath("python3")
	if err != nil {
		return err
	}
	root, err := Root()
	if err != nil {
		return err
	}
	entry := filepath.Join(root, "scripts", "completion_hook.py")
	for name, contents := range entries {
		text, ok := contents.(string)
		if !ok {
			return fmt.Errorf("%w: given.files[%q] is not text", ErrFixture, name)
		}
		text = strings.NewReplacer("${HOME}", home, "${PYTHON}", python, "${ENTRY}", entry).Replace(text)
		target := filepath.Join(home, name)
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err = os.WriteFile(target, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func writeHookSymlinks(home string, links any) error {
	entries, _ := links.(map[string]any)
	for name, value := range entries {
		target, ok := value.(string)
		if !ok {
			return fmt.Errorf("%w: given.symlinks[%q] is not text", ErrFixture, name)
		}
		path := filepath.Join(home, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(strings.ReplaceAll(target, "${HOME}", home), path); err != nil {
			return err
		}
	}
	return nil
}

func applyHookModes(home string, modes any) error {
	entries, _ := modes.(map[string]any)
	for name, value := range entries {
		if err := os.Chmod(filepath.Join(home, name), os.FileMode(number(value))); err != nil {
			return err
		}
	}
	return nil
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func expectedFiles(home string, files map[string]bool) map[string]any {
	out := map[string]any{}
	for name := range files {
		_, err := os.Lstat(filepath.Join(home, name))
		out[name] = err == nil
	}
	return out
}
