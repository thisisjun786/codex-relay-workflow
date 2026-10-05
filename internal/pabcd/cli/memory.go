package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// MemoryAllowWriteArgs and MemoryCliParse are the request and three-way parse
// result of memory-cli.ts (CXC v0.2.40, 3c1459ac). Only one result field is set.
type MemoryAllowWriteArgs struct{ Verb, SessionID, Cwd string }
type MemoryCliParse struct {
	Args  *MemoryAllowWriteArgs
	Help  bool
	Error string
}

// MemoryUsage is the oracle's usage text with the settled CRW verb and directory names.
const MemoryUsage = `Usage:
  crw pabcd memory allow-write --session <id>
  crw pabcd memory allow-write --session=<id>
  crw pabcd memory allow-write --help

Authorizes exactly ONE memory write (memories.add_ad_hoc_note, or an edit under
~/.codex/memories) for that session. The grant is consumed by the next write.

The grant is stored at <cwd>/.crw/sessions/<id>.json and is keyed by that
cwd plus the session id. The success line names the cwd it wrote to. Issuing the
grant from a different working directory will print success and never be seen by
the hook running in this session.

The ordinary path needs no command: when the user asks in their own words to
remember something, the session records that request and the next write passes.`

// ParseMemoryCLIArgs checks the verb before help, then preserves help-anywhere,
// separate/equals session flags, last-value-wins and the oracle's unknown-token error.
func ParseMemoryCLIArgs(argv []string, cwd string) MemoryCliParse {
	verb := ""
	if len(argv) > 0 {
		verb = argv[0]
	}
	if verb != "allow-write" {
		return MemoryCliParse{Error: fmt.Sprintf("unknown memory verb '%s' (expected allow-write)", verb)}
	}
	rest := argv[1:]
	for _, tok := range rest {
		if tok == "--help" || tok == "-h" {
			return MemoryCliParse{Help: true}
		}
	}
	sessionID := ""
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		switch {
		case tok == "--session":
			sessionID = ""
			if i+1 < len(rest) {
				sessionID = rest[i+1]
			}
			i++
		case strings.HasPrefix(tok, "--session="):
			sessionID = strings.TrimPrefix(tok, "--session=")
		default:
			return MemoryCliParse{Error: fmt.Sprintf("unknown argument '%s'", tok)}
		}
	}
	if !state.IsCanonicalSessionID(sessionID) {
		return MemoryCliParse{Error: "missing required argument: --session <id> (must be a canonical session id)"}
	}
	return MemoryCliParse{Args: &MemoryAllowWriteArgs{Verb: verb, SessionID: sessionID, Cwd: cwd}}
}

// RunMemoryCLI writes precisely the existing memoryWriteGrant boolean under the
// session lock. Consumption is the memory gate's responsibility, not this library's.
func RunMemoryCLI(a MemoryAllowWriteArgs) (string, int) {
	err := state.WithSessionLock(a.Cwd, a.SessionID, func() error {
		s, unreadable := state.ReadStateStrict(a.Cwd, a.SessionID)
		// Intentionally changed: the oracle replaces unreadable bytes with a default.
		if unreadable {
			return errors.New("session state is unreadable; refusing to overwrite it")
		}
		if !cliVerdictsIntact(a.Cwd, a.SessionID, len(s.UnverifiedSubagents)) {
			return errors.New("session state holds unreadable unverified records; refusing to rewrite it")
		}
		s.MemoryWriteGrant = true
		return state.WriteState(a.Cwd, s)
	})
	if err != nil {
		return fmt.Sprintf("memory allow-write: could not record the grant (%s)", cliErrorMessage(err)), 1
	}
	return fmt.Sprintf("memory allow-write: session %s may perform ONE memory write; grant recorded for cwd %s; the next write consumes this grant.", a.SessionID, a.Cwd), 0
}

// cliVerdictsIntact prevents publishing a reconstructed list that discarded or changed
// raw records: the list the reader rebuilds from the file must equal what the file
// stores (state.RewriteKeepsUnverified), and hold the count the caller read. The existing
// evidence owner's count helper is private; keeping this scoped check here avoids changing
// that package's public API. Reads occur under WithSessionLock. Absent/null lists are
// valid old-schema states; non-arrays are not.
func cliVerdictsIntact(cwd, sessionID string, count int) bool {
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if os.IsNotExist(err) {
		return count == 0
	}
	if err != nil {
		return false
	}
	s, unreadable := state.ReadStateStrict(cwd, sessionID)
	return !unreadable && len(s.UnverifiedSubagents) == count && state.RewriteKeepsUnverified(raw, s.UnverifiedSubagents)
}
