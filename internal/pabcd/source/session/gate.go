package session

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// GateResult is the verdict of CheckBound; Reason says why not when OK is false, and Cause which kind of refusal it is.
type GateResult struct {
	OK     bool
	Reason string
	Cause  GateCause
	// Probe is the failure of the git probe behind CauseProbeFailed (CauseNone's and the other causes' is nil).
	Probe error
}

// GateCause is the kind of a CheckBound refusal, so that a caller that words its own guidance (the Stop hook) offers binding a
// git source tree only where binding one clears the refusal.
type GateCause int

const (
	// CauseNone is a passed gate.
	CauseNone GateCause = iota
	// CauseUnavailable is a session without a source binding whose working directory has no resolvable git source identity:
	// binding a git source tree clears it.
	CauseUnavailable
	// CauseBoundUnavailable is a session whose bound source worktree yields no resolvable git source identity: a binding is
	// immutable, so binding another tree does not clear it.
	CauseBoundUnavailable
	// CauseBindingError is a binding or source-root error (a corrupt, foreign or moved binding, a missing binding for a pinned
	// worktree, a git probe of the binding that failed); Reason is that error.
	CauseBindingError
	// CauseProbeFailed is a source tree whose git could not answer: git could not start, could not read the tree, failed, ran
	// out of time or was cancelled, which is not git saying that the directory is in no repository. Binding a git source tree
	// does not clear it (the bind runs the same git); Probe is the failure, and the verdict is not that the source is missing.
	CauseProbeFailed
)

// CheckBound refuses entry to a goalplan-bound cycle whose source identity cannot be resolved: C -> D needs a test receipt
// (CHECK-BINDING-01) and a receipt cannot be bound to a commit without one, so the verdict is given before the plan, audit
// and implementation are spent. The predicate is "the source identity is unavailable", never "there is no .git": binding a
// git source worktree clears it without this function knowing anything about split working directories. A thrown binding or
// source-root error is also "cannot resolve". Call it only for a bound session; an unbound one closes D on its check output
// and exit code alone. A dirty tree is a resolved identity.
func CheckBound(cwd, sessionID string) GateResult { return checkBound(prober{}, cwd, sessionID) }

// CheckBoundContext is CheckBound for a caller that must not leave work running behind it (see CaptureContext): the git it
// runs is owned by ctx, and it returns only once that git has ended. A git that could not answer is CauseProbeFailed (with the
// failure in Probe), as it is for CheckBound; OK and Reason are CheckBound's.
func CheckBoundContext(ctx context.Context, cwd, sessionID string) GateResult {
	return checkBound(prober{ctx}, cwd, sessionID)
}

func checkBound(p prober, cwd, sessionID string) GateResult {
	exclude := true
	id, probe, err := capture(p, cwd, sessionID, CaptureOptions{ExcludeStateArtifacts: &exclude})
	if err != nil {
		return GateResult{Reason: err.Error(), Cause: CauseBindingError}
	}
	if id.Kind != source.KindUnavailable {
		return GateResult{OK: true}
	}
	cause := CauseUnavailable
	switch {
	case probe != nil && !source.NotARepository(probe):
		cause = CauseProbeFailed
	case id.SourceRoot != nil:
		cause = CauseBoundUnavailable
	}
	return GateResult{Cause: cause, Probe: probe, Reason: strings.Join([]string{
		"this workspace has no resolvable git source identity, and a goalplan-bound",
		"cycle cannot be closed without one: C -> D requires a testReceiptPath",
		"(CHECK-BINDING-01) and `crw pabcd receipt test` refuses to write a receipt it cannot",
		"bind to a commit.",
		"",
		"Pick one:",
		"  - bind a git source tree:  crw relay session source <absolute-path> --json",
		"  - or run this cycle unbound (no `crw pabcd loop init --session`), which closes D on",
		"    checkOutput + exitCode alone.",
	}, "\n")}
}
