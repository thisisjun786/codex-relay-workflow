package job

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

// The oracle argv facade and the oracle hook entry (CRW-828). The product reaches the job verbs through RunParsedCLI after the relay validated the
// arguments, and the hooks through the Handle* entries; these forms of the oracle exist for the tests that pin the oracle's own parsing and drain.

// RunCLI is the oracle argv facade. The relay uses RunParsedCLI after validation.
func RunCLI(argv []string, cwd string, getenv func(string) (string, bool), clock func() time.Time) (CLIResult, error) {
	verb, args := "", []string{}
	if len(argv) > 0 {
		verb, args = argv[0], argv[1:]
	}
	opts := CLIOptions{Verb: verb, Note: cliFlagValue(args, "--note"), Tail: cliFlagValue(args, "--tail"), Session: cliFlagValue(args, "--session"), JSON: slices.Contains(args, "--json")}
	if len(args) > 0 {
		opts.ID = args[0]
	}
	if verb == "run" {
		sep := slices.Index(args, "--")
		if sep < 0 || sep == len(args)-1 {
			return CLIResult{cliUsage, 1}, nil
		}
		opts.Command, opts.Note = args[sep+1:], cliFlagValue(args[:sep], "--note")
	}
	return RunParsedCLI(opts, cwd, getenv, clock)
}

func cliFlagValue(args []string, name string) *string {
	i := slices.Index(args, name)
	if i < 0 || i+1 == len(args) {
		return nil
	}
	return &args[i+1]
}

// ParseCLIPayload keeps parsePayload's object/array tolerance and silent fallback.
func ParseCLIPayload(raw string) any {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		if _, err := dec.Token(); err == io.EOF {
			switch v.(type) {
			case map[string]any, []any:
				return v
			}
		}
	}
	return map[string]any{}
}

// ReadCLIStdin is readStdin, including reading before applying the unit bound.
// It is a forward-use seam for the separately ported hook entry.
func ReadCLIStdin(in io.Reader) string {
	b, err := io.ReadAll(in)
	if err != nil {
		return ""
	}
	raw := decodeUTF8(b)
	if len(utf16.Encode([]rune(raw))) > MaxCLIStdinBytes {
		return ""
	}
	return raw
}

// DrainNow deliberately ignores both wake switches: explicit collection still works. It selects and stamps under the store lock.
func DrainNow(ws string, sessionID *string, clock func() time.Time) string {
	return silent(func() string {
		out, _ := deliver(ws, sessionID, clock, nil, func(due []BgRecord) (string, []BgRecord) { return fitWake(due, completionBody, wireSize, wireSize) }, acceptAll)
		return out
	})
}
