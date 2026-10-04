package role

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"io"
	"os"
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
			result, err = CheckedDispatch(context.Background(), cwd, input, env, nil)
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
