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
func RunEvidenceCLI(a EvidenceResolveArgs) (string, int) {
	if !evidence.HasValidReceipt(a.Cwd, a.Receipt) {
		return fmt.Sprintf("evidence resolve: receipt failed the evidence-root guard (must be a real, non-empty, non-symlink file inside .crw/evidence): %s", a.Receipt), 1
	}
	removed, ambiguous := false, false
	err := state.WithSessionLock(a.Cwd, a.SessionID, func() error {
		s := state.ReadState(a.Cwd, a.SessionID)
		index := -1
		for i, entry := range s.UnverifiedSubagents {
			if entry.Resolvable && entry.AgentID == a.AgentID && (a.TurnID == nil || entry.TurnID == *a.TurnID) {
				if index >= 0 {
					ambiguous = true
					return nil
				}
				index = i
			}
		}
		if index < 0 {
			return nil
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
		if err := state.WriteState(a.Cwd, s); err != nil {
			return err
		}
		evidence.ClearAttempts(a.Cwd, a.SessionID, a.AgentID, target.TurnID)
		removed = true
		return nil
	})
	if err != nil {
		return "evidence resolve: " + cliErrorMessage(err), 1
	}
	if ambiguous {
		return fmt.Sprintf("evidence resolve: agent '%s' has more than one unverified record; pass --turn <turn-id> to name exactly which one this receipt verifies", a.AgentID), 1
	}
	if !removed {
		return fmt.Sprintf("evidence resolve: no resolvable unverified record for agent '%s' in session %s", a.AgentID, a.SessionID), 1
	}
	return fmt.Sprintf("evidence resolve: agent %s resolved against %s", a.AgentID, a.Receipt), 0
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
