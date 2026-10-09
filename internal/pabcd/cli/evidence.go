package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// EvidenceResolveArgs is evidence-cli.ts's resolve request (CXC v0.2.40, 3c1459ac).
// A nil TurnID matches any turn; a pointer to "" matches only a turn-less verdict.
type EvidenceResolveArgs struct {
	Verb, SessionID, AgentID, Receipt, Cwd string
	TurnID                                 *string
	// writeState is the state publication the resolve performs; nil means state.WriteState. It is a
	// field rather than a package-level variable, so a test can stage a publication that fails after
	// the rename without affecting any other caller.
	writeState func(string, state.State) error
}

// ParseEvidenceCLIArgs preserves the oracle's first exact flag lookup: unknown
// tokens are ignored, equals forms are not recognized, and the next flag can be a value.
func ParseEvidenceCLIArgs(argv []string, cwd string) (EvidenceResolveArgs, error) {
	verb := ""
	if len(argv) > 0 {
		verb = argv[0]
	}
	if verb != "resolve" {
		return EvidenceResolveArgs{}, fmt.Errorf("unknown evidence verb '%s' (expected resolve)", verb)
	}
	flag := func(name string) *string {
		for i, tok := range argv {
			if tok == "--"+name {
				if i+1 < len(argv) {
					value := argv[i+1]
					return &value
				}
				return nil
			}
		}
		return nil
	}
	value := func(s *string) string {
		if s != nil {
			return *s
		}
		return ""
	}
	a := EvidenceResolveArgs{Verb: verb, SessionID: value(flag("session")), AgentID: value(flag("agent")),
		Receipt: value(flag("receipt")), TurnID: flag("turn"), Cwd: cwd}
	missing := []string{}
	if !state.IsCanonicalSessionID(a.SessionID) {
		missing = append(missing, "--session <id> (must be a canonical session id)")
	}
	if a.AgentID == "" {
		missing = append(missing, "--agent <agent-id>")
	}
	if a.Receipt == "" {
		missing = append(missing, `--receipt <path> (resolving without evidence would defeat the gate; if an external blocker prevents verifying the work, use update_goal status "blocked")`)
	}
	if len(missing) != 0 {
		return EvidenceResolveArgs{}, fmt.Errorf("missing required argument(s): %s", strings.Join(missing, "; "))
	}
	return a, nil
}

// RunEvidenceCLI resolves exactly one resolvable verdict against a valid receipt.
// The ledger is written before the atomic state publication; counters clear only
// after that publication succeeds. Dispatching, streams and newlines belong to the caller.
//
// A publication that reports state.Published is a success with a warning: the new state is already at the
// final path, so the tombstone is gone and the attempt record is cleared to match it. A failure before the
// rename is still an error and leaves the attempts alone.
func RunEvidenceCLI(a EvidenceResolveArgs) (string, int) {
	if !evidence.HasValidReceipt(a.Cwd, a.Receipt) {
		return fmt.Sprintf("evidence resolve: receipt failed the evidence-root guard (must be a real, non-empty, non-symlink file inside .crw/evidence): %s", a.Receipt), 1
	}
	writeState := a.writeState
	if writeState == nil {
		writeState = state.WriteState
	}
	removed, ambiguous, warning := false, false, error(nil)
	// CRW-1106: a receipt's resolution is serialised with the SubagentStop gate of the same child, under the lock of the exact
	// (session, agent, turn), which is taken before the session lock (the one lock order). The turn is found by an unlocked read
	// first and then pinned, so the locked step resolves exactly the verdict whose lock it holds. A request that names no turn and
	// finds none or several is run under the session lock alone, where it resolves nothing but reports the ambiguity; if it finds
	// exactly one there (the unlocked read raced a writer), it only names the turn and the whole request runs again pinned.
	if turn, ok := cliEvidenceTurn(a); ok {
		a.TurnID = &turn
	}
	var pinTurn *string
	resolve := func() error {
		// CRW-1110: a file that holds more verdicts than the reader keeps is recovered first, so no verdict is lost to the rewrite.
		if err := evidence.RecoverOverflow(a.Cwd, a.SessionID, writeState); err != nil {
			return err
		}
		s := state.ReadState(a.Cwd, a.SessionID)
		// CRW-1110: the verdicts of the main list and those recorded beside it are one set. A request that matches more than one of
		// them, in either place, is ambiguous and resolves none.
		index, beside := -1, 0
		for i, entry := range s.UnverifiedSubagents {
			if cliEvidenceMatches(a, entry) {
				if index >= 0 {
					ambiguous = true
					return nil
				}
				index = i
			}
		}
		overflow, _ := evidence.OverflowVerdicts(a.Cwd, a.SessionID)
		for _, entry := range overflow {
			if cliEvidenceMatches(a, entry) {
				beside++
			}
		}
		inMain := 0
		if index >= 0 {
			inMain = 1
		}
		if inMain+beside > 1 {
			ambiguous = true
			return nil
		}
		if a.TurnID == nil && inMain+beside == 1 {
			// The one match of an unpinned request: name its turn and run again with the tuple lock held.
			turn := ""
			if index >= 0 {
				turn = s.UnverifiedSubagents[index].TurnID
			} else {
				for _, entry := range overflow {
					if cliEvidenceMatches(a, entry) {
						turn = entry.TurnID
					}
				}
			}
			pinTurn = &turn
			return nil
		}
		if index < 0 {
			return cliResolveOverflow(a, s.Phase, &removed, &ambiguous)
		}
		// Intentionally changed: publishing a capped/repaired read loses interview records too. This
		// check runs first because cliVerdictsIntact now covers the tracker, and the interview loss
		// keeps this command's own sentence.
		if !cliInterviewIntact(a.Cwd, a.SessionID) {
			return errors.New(cliInterviewRefusalReason)
		}
		// Intentionally changed: publishing a capped/repaired read loses verdicts.
		if !cliVerdictsIntact(a.Cwd, a.SessionID, len(s.UnverifiedSubagents)) {
			return errors.New("session state holds unreadable unverified records; refusing to rewrite it")
		}
		target := s.UnverifiedSubagents[index]
		turn := target.TurnID
		if turn == "" {
			turn = "<none>"
		}
		override := false
		if err := state.AppendLedger(a.Cwd, state.LedgerEntry{
			TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), SessionID: a.SessionID, From: &s.Phase, To: s.Phase,
			Reason:   fmt.Sprintf("evidence resolve: agent=%s turn=%s resolved with a valid receipt", a.AgentID, turn),
			Evidence: &a.Receipt, EvidenceAfterReason: true, Actor: "agent", Override: &override,
		}); err != nil {
			return err
		}
		s.UnverifiedSubagents = append(s.UnverifiedSubagents[:index], s.UnverifiedSubagents[index+1:]...)
		s.SessionID = a.SessionID
		if err := writeState(a.Cwd, s); err != nil {
			if !state.Published(err) {
				return err // nothing was published: the tombstone is still there and the resolve failed
			}
			// The state reached the final path and only the directory sync failed, so the tombstone is
			// already gone. Clear the attempt record to match the published state; otherwise the counter
			// stays at the cap and HasSpentBudget stays true for good.
			evidence.ClearAttempts(a.Cwd, a.SessionID, a.AgentID, target.TurnID)
			removed, warning = true, err
			return nil
		}
		evidence.ClearAttempts(a.Cwd, a.SessionID, a.AgentID, target.TurnID)
		removed = true
		return nil
	}
	var err error
	for range 3 {
		pinTurn = nil
		if a.TurnID != nil {
			err = evidence.WithCounterLock(a.Cwd, a.SessionID, a.AgentID, *a.TurnID, func() error { return state.WithSessionLock(a.Cwd, a.SessionID, resolve) })
		} else {
			err = state.WithSessionLock(a.Cwd, a.SessionID, resolve)
		}
		if err != nil || pinTurn == nil {
			break
		}
		a.TurnID = pinTurn
	}
	if err != nil {
		return "evidence resolve: " + cliErrorMessage(err), 1
	}
	if ambiguous {
		return fmt.Sprintf("evidence resolve: agent '%s' has more than one unverified record; pass --turn <turn-id> to name exactly which one this receipt verifies", a.AgentID), 1
	}
	if !removed {
		return fmt.Sprintf("evidence resolve: no resolvable unverified record for agent '%s' in session %s", a.AgentID, a.SessionID), 1
	}
	line := fmt.Sprintf("evidence resolve: agent %s resolved against %s", a.AgentID, a.Receipt)
	if warning != nil {
		return line + "\n" + fmt.Sprintf("evidence resolve: warning: the session state was published but its directory sync failed: %s", warning), 0
	}
	return line, 0
}

// cliEvidenceMatches reports whether a verdict is a resolvable one of the agent (and of the turn, when the request names one).
func cliEvidenceMatches(a EvidenceResolveArgs, entry state.UnverifiedSubagent) bool {
	return entry.Resolvable && entry.AgentID == a.AgentID && (a.TurnID == nil || entry.TurnID == *a.TurnID)
}

// cliResolveOverflow resolves the agent's one resolvable verdict recorded beside the full main list (CRW-1110), with the same
// ledger row as a verdict of the list. The caller holds the session lock and has checked the receipt.
func cliResolveOverflow(a EvidenceResolveArgs, phase state.Phase, removed, ambiguous *bool) error {
	verdicts, _ := evidence.OverflowVerdicts(a.Cwd, a.SessionID)
	var target *state.UnverifiedSubagent
	for i, entry := range verdicts {
		if cliEvidenceMatches(a, entry) {
			if target != nil {
				*ambiguous = true
				return nil
			}
			target = &verdicts[i]
		}
	}
	if target == nil {
		return nil
	}
	turn := target.TurnID
	if turn == "" {
		turn = "<none>"
	}
	override := false
	if err := state.AppendLedger(a.Cwd, state.LedgerEntry{
		TS: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), SessionID: a.SessionID, From: &phase, To: phase,
		Reason:   fmt.Sprintf("evidence resolve: agent=%s turn=%s resolved with a valid receipt", a.AgentID, turn),
		Evidence: &a.Receipt, EvidenceAfterReason: true, Actor: "agent", Override: &override,
	}); err != nil {
		return err
	}
	if !evidence.ResolveOverflowVerdict(a.Cwd, a.SessionID, a.AgentID, target.TurnID) {
		return errors.New("the unverified record beside the session state could not be removed")
	}
	evidence.ClearAttempts(a.Cwd, a.SessionID, a.AgentID, target.TurnID)
	*removed = true
	return nil
}

// cliEvidenceTurn is the turn of the one resolvable verdict of the agent that the request names, read without a lock: the turn
// itself when the request gives one, the single match's when it does not, and false when there is none or more than one (the
// locked step then reports that).
func cliEvidenceTurn(a EvidenceResolveArgs) (string, bool) {
	if a.TurnID != nil {
		return *a.TurnID, true
	}
	turn, found := "", 0
	overflow, _ := evidence.OverflowVerdicts(a.Cwd, a.SessionID)
	for _, entry := range append(state.ReadState(a.Cwd, a.SessionID).UnverifiedSubagents, overflow...) {
		if entry.Resolvable && entry.AgentID == a.AgentID {
			turn, found = entry.TurnID, found+1
		}
	}
	return turn, found == 1
}

// The stale-lock diagnostic is observable in the corpus. Other filesystem errors
// keep Go's native diagnostic, as the state owner does; no runtime Node is involved.
func cliErrorMessage(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && pathErr.Op == "open" && errors.Is(pathErr.Err, syscall.EEXIST) {
		return fmt.Sprintf("EEXIST: file already exists, open '%s'", pathErr.Path)
	}
	return err.Error()
}
