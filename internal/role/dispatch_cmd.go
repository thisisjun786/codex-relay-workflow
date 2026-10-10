package role

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// DispatchHelp is the usage dispatch --help prints: the stdin form and one line per action.
func DispatchHelp() string {
	return strings.Join([]string{
		"usage: crw role helper dispatch < request.json",
		"Reads one JSON object (at most 64 KiB) on stdin and prints one JSON answer; a refusal prints {\"error\":...} and exits 1.",
		"It records the managed fallback ledger of .crw/dispatches and never starts a model or a native call.", "",
		`  {"action":"start","sessionId":"<main-id>","dispatchId":"<task-id>","role":"<role>"}`,
		`  {"action":"claim","sessionId":"<main-id>","dispatchId":"<task-id>","attemptId":"<attempt-id>"}`,
		`  {"action":"report","outcome":"created","sessionId":"<main-id>","dispatchId":"<task-id>","attemptId":"<attempt-id>","agentId":"<child-id>"}`,
		`  {"action":"report","outcome":"complete|failed|task_failed|unavailable|stopped", ...the ids, "agentId", "executionState", "reconciliation"...}`,
		`  {"action":"status","sessionId":"<main-id>","dispatchId":"<task-id>"}`, "",
		"The protocol is the crw-pabcd skill's delegation reference (Configured first fallback).",
	}, "\n")
}

// DispatchCommand ports fallback-dispatch-cli.ts:25-34,40-45. Help (--help, -h or help) prints DispatchHelp and any other
// argument is refused, both before stdin is read (CRW-1117); the dispatch path consumes one bounded JSON value on stdin.
func DispatchCommand(args []string, in io.Reader, out io.Writer, env host.LookupEnv) int {
	if len(args) > 0 {
		if slices.ContainsFunc(args, helperCLIHelpToken) || args[0] == "help" {
			if _, err := fmt.Fprintln(out, DispatchHelp()); err != nil {
				return 1
			}
			return 0
		}
		if encoded, err := Stringify(map[string]string{"error": "dispatch takes no arguments (got '" + args[0] + "'); send one JSON request on stdin, see crw role helper dispatch --help"}, ""); err == nil {
			fmt.Fprintln(out, string(encoded))
		}
		return 1
	}
	data, err := io.ReadAll(io.LimitReader(in, dispatchMaxInput+1))
	if err == nil && len(data) > dispatchMaxInput {
		err = errors.New("dispatch input exceeds 64 KiB")
	}
	var input any
	if err == nil {
		err = json.Unmarshal(data, &input)
	}
	var result DispatchResult
	if err == nil {
		var cwd string
		cwd, err = os.Getwd()
		if err == nil {
			result, err = dispatchChecked(context.Background(), cwd, input, env)
		}
	}
	var answer any = result
	code := 0
	if err != nil {
		answer = map[string]string{"error": err.Error()}
		code = 1
	}
	encoded, err := Stringify(answer, "")
	if err != nil {
		return 1
	}
	if _, err := fmt.Fprintln(out, string(encoded)); err != nil {
		return 1
	}
	return code
}

// dispatchChecked runs one dispatch input, opening the read-only App Server host only for a report
// that reads the child's newest turn: a stopped close, and a failed or task_failed report, whose
// handoff waits for the recorded child to be seen ending. Every other input, the created report
// included, runs exactly as it does without a host and never dials. A nil
// OpenDispatchHost (the library default), a nil host from it, or a failed dial all pass nil, so the
// command falls back to the native-database path it had before the host existed. The connection
// closes when the command ends.
func dispatchChecked(ctx context.Context, cwd string, input any, env host.LookupEnv) (DispatchResult, error) {
	if !dispatchWantsHost(input) || OpenDispatchHost == nil {
		return CheckedDispatch(ctx, cwd, input, env, nil)
	}
	h, closeHost, err := OpenDispatchHost(env)
	if closeHost != nil {
		defer closeHost()
	}
	if err != nil {
		h = nil
	}
	return CheckedDispatch(ctx, cwd, input, env, h)
}

// dispatchWantsHost reports whether the input is a report the host could help: a report whose outcome
// is stopped, failed or task_failed and that names the record it changes. An input that cannot be read
// as a dispatch record, or that omits the session, dispatch or attempt id, is not one: the report cannot
// proceed on it and CheckedDispatch produces the canonical refusal without a host, so nothing dials.
func dispatchWantsHost(input any) bool {
	b, err := dispatchRecord(input)
	if err != nil {
		return false
	}
	if !dispatchIs(b["action"], "report") || !dispatchIs(b["outcome"], "stopped") && !dispatchIs(b["outcome"], "failed") && !dispatchIs(b["outcome"], "task_failed") {
		return false
	}
	for _, key := range []string{"sessionId", "dispatchId", "attemptId"} {
		if value, _ := b[key].(string); value == "" {
			return false
		}
	}
	return true
}
