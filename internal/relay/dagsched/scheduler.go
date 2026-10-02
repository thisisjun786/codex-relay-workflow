package dagsched

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Scheduler is the DAG scheduler over one relay store. Every external effect is behind a seam so the same code
// runs against the host in production and against a fake App Server, a temporary git repository and a scripted
// forge in tests.
type Scheduler struct {
	Store *store.Store
	// Tips reads where a base branch points now (mergeturn.TargetReader in production).
	Tips mergeturn.Reader
	// Ancestry answers whether one commit is an ancestor of another.
	Ancestry Ancestry
	// PRs reads a pull request the way merge-evidence does.
	PRs PullRequestReader
	// Start creates, registers and dispatches a managed child (the managed-start engine).
	Start Starter
	// Selectors are the spellings of the marker root, the socket and the state selector a release is fingerprinted with.
	Selectors Selectors
	// Now is the clock of the recorded_at columns; registry.SystemISO when nil. A reading never uses it.
	Now func() string

	// Test seams (zero in production): between the unlocked judgement and the intent, after the slot is reserved, and between the managed start and the bind.
	testBetweenReadAndIntent func()
	testAfterReserve         func() error
	testAfterStart           func() error
}

// Selectors are what the managed identity fingerprints besides the request: the marker root, the socket and the state
// selector as the caller spelled them (managed/identity.go). A replay must present the same spellings.
type Selectors struct{ MarkerRoot, Socket, StateSelector string }

// Starter runs one managed-start request and reports where it ended. The production Starter is managed.HostStart
// behind the command's services; tests run a real managed.Start over a fake host.
type Starter func(ctx context.Context, raw []byte) (StartAnswer, error)

// StartAnswer is the managed-start receipt: State "admitted" is the only success; refused and incomplete leave the
// intent in place for a replay of the same request.
type StartAnswer struct {
	State, Stage, Reason string
	Answer               contract.OrderedObject
}

// Ancestry reports whether subject is an ancestor of tip in repository (a local path or owner/name).
type Ancestry func(ctx context.Context, repository, subject, tip string) (isAncestor bool, method string, err error)

// PullRequest is what the scheduler needs of merge-evidence's snapshot of one pull request.
type PullRequest struct {
	Repository        string
	Number            int64
	State             string // open | closed | merged
	IsDraft           bool
	HeadSHA           string
	BaseRef, BaseSHA  string
	Checks            []Check
	RequiredDeclared  []string
	RequiredReadable  bool
	RequiredProviders map[string][]string
	// CheckProblems is evidence.ChecksProblemsWith(head, required, checks, true, providers), already rendered.
	CheckProblems []string
	ReviewDigest  string
	// Verdict is evidence.VerdictOf(problems): ready | not_ready | stale | unknown.
	Verdict  string
	Problems []Problem
}

// Check is one check run of the pull request's head as merge-evidence reports it.
type Check struct {
	RunID, Name, HeadSHA, Conclusion string
	Attempt                          int64
}

// Problem is one problem code of a merge-evidence snapshot.
type Problem struct{ Code, Detail string }

// PullRequestReader reads one pull request. The production reader needs an owner/name repository.
type PullRequestReader func(ctx context.Context, repository string, number int64) (PullRequest, error)
