package role

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// DispatchCommand ports fallback-dispatch-cli.ts:25-34,40-45. Trailing argv
// remains ignored; the dispatch path consumes one bounded JSON value on stdin.
func DispatchCommand(_ []string, in io.Reader, out io.Writer, env host.LookupEnv) int {
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

// dispatchChecked runs one dispatch input, opening the read-only App Server host only for a stopped
// close. That is the one action whose check reads the child's newest turn; every other input,
// the created report included, runs exactly as it does without a host and never dials. A nil
// OpenDispatchHost (the library default), a nil host from it, or a failed dial all pass nil, so the
// command falls back to the native-database path it had before the host existed. The connection
// closes when the command ends.
func dispatchChecked(ctx context.Context, cwd string, input any, env host.LookupEnv) (DispatchResult, error) {
	if !dispatchIsStoppedReport(input) || OpenDispatchHost == nil {
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

// dispatchIsStoppedReport reports whether the input is a report whose outcome is stopped, the one
// action the stopped close owns. An input that cannot be read as a dispatch record is not one:
// CheckedDispatch produces the canonical refusal for it.
func dispatchIsStoppedReport(input any) bool {
	b, err := dispatchRecord(input)
	if err != nil {
		return false
	}
	return dispatchIs(b["action"], "report") && dispatchIs(b["outcome"], "stopped")
}
