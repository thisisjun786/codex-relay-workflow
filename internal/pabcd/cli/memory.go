package cli

import (
	"context"
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
	return cliPublishedMemoryAllowWrite(a, state.WriteState)
}

// RunMemoryCLIContext is RunMemoryCLI for a caller the first SIGINT can end (the memory row of cmd/crw serve,
// CRW-1074): the session lock wait ends with ctx, and ctx is read once more with the lock held, immediately
// before the grant is written. A run they end returns the context's own error with the state untouched and
// nothing to print, as the oracle's process dies at the signal; once the write has started the run finishes
// and answers as RunMemoryCLI does.
func RunMemoryCLIContext(ctx context.Context, a MemoryAllowWriteArgs) (string, int, error) {
	return cliMemoryAllowWriteRun(ctx, a, state.WriteState, nil)
}

// cliPublishedMemoryAllowWrite is the CRW-823 write seam. writeState is an argument, never package
// state, so a test can drive the published-but-unsynced path without changing what any other caller
// does; RunMemoryCLI passes state.WriteState.
func cliPublishedMemoryAllowWrite(a MemoryAllowWriteArgs, writeState func(string, state.State) error) (string, int) {
	output, code, _ := cliMemoryAllowWriteRun(context.Background(), a, writeState, nil)
	return output, code
}

// cliMemoryAllowWriteRun is the grant under the invocation's context. interrupt is the CRW-1074 test seam, a
// field of the caller's own: it runs immediately before the pre-write context check, so a test can end the
// context exactly between the lock and the write. nil means no hook.
//
// Every answer reached before the grant write began (the lock's own error, and the unreadable, interview and
// verdict refusals of the state read under the lock) is the answer of a process the signal would already have
// ended: an ended context takes precedence and nothing is printed. Once the write has begun, the answer stands.
func cliMemoryAllowWriteRun(ctx context.Context, a MemoryAllowWriteArgs, writeState func(string, state.State) error, interrupt func()) (string, int, error) {
	begun := false
	err := state.WithSessionLockContext(ctx, a.Cwd, a.SessionID, func() error {
		s, unreadable := state.ReadStateStrict(a.Cwd, a.SessionID)
		// Intentionally changed: the oracle replaces unreadable bytes with a default.
		if unreadable {
			return errors.New("session state is unreadable; refusing to overwrite it")
		}
		// The interview check runs first: cliVerdictsIntact now covers the tracker too, so a state that
		// would lose interview records must keep this command's own interview sentence.
		if !cliInterviewIntact(a.Cwd, a.SessionID) {
			return errors.New(cliInterviewRefusalReason)
		}
		if !cliVerdictsIntact(a.Cwd, a.SessionID, len(s.UnverifiedSubagents)) {
			return errors.New("session state holds unreadable unverified records; refusing to rewrite it")
		}
		// CRW-1074: the pre-write cancellation check. Cancelled here, the grant is not written.
		if interrupt != nil {
			interrupt()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.MemoryWriteGrant = true
		begun = true
		return writeState(a.Cwd, s)
	})
	if !begun {
		if cerr := ctx.Err(); cerr != nil {
			return "", 0, cerr
		}
	}
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return "", 0, err
	}
	// A write that published the state at the final path and then failed the directory sync is a
	// written grant: the grant is visible to every reader, so a retry would record it twice. The
	// durability failure is carried as a warning instead. A failure before the rename published
	// nothing and stays the failure it was.
	if err != nil && !state.Published(err) {
		return fmt.Sprintf("memory allow-write: could not record the grant (%s)", cliErrorMessage(err)), 1, nil
	}
	recorded := fmt.Sprintf("memory allow-write: session %s may perform ONE memory write; grant recorded for cwd %s; the next write consumes this grant.", a.SessionID, a.Cwd)
	if err != nil {
		return recorded + "\n" + cliPublishedStateWarning(err), 0, nil
	}
	return recorded, 0, nil
}

// cliPublishedStateWarning is CRW-823's state durability warning: the state at the final path is the
// new one, so the write counts as done, but the directory that holds it could not be synced and the
// publication may not survive a crash. The oracle never syncs that directory, so it has no
// counterpart; the wording is the issue's own.
func cliPublishedStateWarning(err error) string {
	return "session state was published but its directory could not be synced: " + cliErrorMessage(err)
}

// cliVerdictsIntact prevents publishing a state the reader rebuilt that discarded or changed a stored
// record: the file must keep every record class the shared judgement covers (state.RewriteKeepsStored:
// the unverified-subagent list and the interview tracker), and the rebuilt list must hold the count the
// caller read. The existing evidence owner's count helper is private; keeping this scoped check here
// avoids changing that package's public API. Reads occur under WithSessionLock. Absent/null lists are
// valid old-schema states; non-arrays are not.
func cliVerdictsIntact(cwd, sessionID string, count int) bool {
	raw, err := state.ReadStateFile(cwd, sessionID)
	if os.IsNotExist(err) {
		return count == 0
	}
	if err != nil {
		return false
	}
	s, unreadable := state.ReadStateStrict(cwd, sessionID)
	return !unreadable && len(s.UnverifiedSubagents) == count && state.RewriteKeepsStored(raw, s)
}

// cliInterviewRefusalReason is the reason a cli writer gives when the rewrite would drop stored interview records.
const cliInterviewRefusalReason = "session state holds interview records this command cannot rewrite without losing them; refusing to rewrite it"

// cliInterviewIntact prevents publishing a reconstructed interview tracker that discarded or changed
// raw records: ReadStateStrict rebuilds the tracker through interview.ReconstructInterview, which caps
// contradictions and assumptions at interview.MaxTrackerArray (drop-oldest) and drops an ontology entity
// with no name and a relationship with no target, so a rewrite from that read loses those records for
// good (state.RewriteKeepsInterview). A file that does not exist stores nothing to lose; one that cannot be
// read is refused, as is one the reader calls unreadable. Reads occur under WithSessionLock, as cliVerdictsIntact's do.
func cliInterviewIntact(cwd, sessionID string) bool {
	raw, err := state.ReadStateFile(cwd, sessionID)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil {
		return false
	}
	s, unreadable := state.ReadStateStrict(cwd, sessionID)
	return !unreadable && state.RewriteKeepsInterview(raw, s.Interview)
}
