package planinput

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// Command is the word after `crw flow`.
const Command = "plan-input"

// Usage is the usage of the command.
const Usage = `usage: crw flow plan-input --milestone OWNER/NAME#N [--target-repository DIR --base-ref BRANCH] [--thread T] [--envelope] [--state DIR] [--socket PATH]`

const usageHelp = Usage + `

Build the handoff of crw flow compile (crw-flow-compile-input/1) from one milestone of the tracking repository, and print it on stdout:

  crw flow plan-input --milestone thisisjun786/crw-work#3 | crw flow compile --dry-run -

The project key is the milestone ref (OWNER/NAME#N); each open issue of the milestone is one issue line keyed by its ref, with its
criteria, edit regions and size fields read from its body, and its prerequisites from its "Blocked by:" lines. The body headings are:
  criteria  a bullet list under "끝 조건", "Done when", "Acceptance" or "Acceptance criteria" (none: the title is the one criterion)
  regions   a bullet list under "수정 범위", "Edit regions" or "Regions"; a path in backticks, else the first word, a trailing slash is a tree
  size      "size_fit: V", "size_reason: V" and "issue_size: V" lines under "크기", "Size" or "Size fit"
The command reads through the tracking adapter with GET requests (the token is GH_TOKEN, GITHUB_TOKEN or gh auth token) and writes nothing.

  --milestone REF          the milestone, OWNER/NAME#N (required)
  --target-repository DIR  the absolute path of the checkout the issues integrate on (with --base-ref); compile needs a target for any order
  --base-ref BRANCH        the short name of the branch the issues integrate on
  --thread T               the owner thread whose live parent bindings the project key is compared with (default: CODEX_THREAD_ID)
  --envelope               print {schema, milestone, input, warnings} instead of the bare input, so the warnings are recorded with it
  --state DIR, --socket P  the relay state the bindings are read from (read-only)

A mapping that cannot be carried over, a blocker outside the milestone or closed, and a project key the thread holds no binding of are
warnings, printed on stderr and kept in the envelope's "warnings"; none refuses. Exit 0 built; 2 an input that cannot be used (a ref that
is not OWNER/NAME#N, a milestone that does not exist, a refused token).
`

// Deps are what the command reaches outside: the tracking repository, the relay's bindings and the environment.
type Deps struct {
	// NewReader makes the reader of the tracking repository "owner/name".
	NewReader func(repo string) (Reader, error)
	// Scopes reads the project keys of the live parent bindings of thread.
	Scopes func(ctx context.Context, thread, state, socket string) ([]string, error)
	Getenv func(string) string
}

// Default is the production dependencies.
func Default() *Deps {
	return &Deps{
		NewReader: func(repo string) (Reader, error) {
			g, err := tracking.NewGitHub(repo, tracking.DefaultTokenSource())
			if err != nil {
				return nil, err
			}
			return g, nil
		},
		Scopes: ReadScopes,
		Getenv: os.Getenv,
	}
}

// Handles reports whether `crw flow args...` is this command.
func Handles(args []string) bool { return len(args) > 0 && args[0] == Command }

// RunCLI is `crw flow plan-input args[1:]` over the production dependencies.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == Command {
		args = args[1:]
	}
	return RunWith(ctx, Default(), args, stdout, stderr)
}

// RunWith is the command over explicit dependencies; args are the arguments after the command word.
func RunWith(ctx context.Context, d *Deps, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("crw flow plan-input", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	milestone := fs.String("milestone", "", "the milestone, OWNER/NAME#N")
	targetRepo := fs.String("target-repository", "", "the absolute path of the checkout")
	baseRef := fs.String("base-ref", "", "the branch")
	thread := fs.String("thread", "", "the owner thread")
	envelope := fs.Bool("envelope", false, "print the envelope")
	state := fs.String("state", "", "the relay state directory")
	socket := fs.String("socket", "", "the App Server control socket")
	fail := func(format string, a ...any) int {
		fmt.Fprintln(stderr, Usage)
		fmt.Fprintf(stderr, "crw flow plan-input: error: "+format+"\n", a...)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageHelp)
			return 0
		}
		return fail("%v", err)
	}
	switch {
	case fs.NArg() > 0:
		return fail("takes no argument, got %q", fs.Arg(0))
	case *milestone == "":
		return fail("--milestone OWNER/NAME#N is required: pass the tracking repository and the milestone number")
	}
	ref, err := tracking.ParseRef(*milestone)
	if err != nil {
		return fail("--milestone %q is not OWNER/NAME#N: pass the tracking repository and the milestone number, for example thisisjun786/crw-work#3", *milestone)
	}
	var opts Options
	switch {
	case (*targetRepo == "") != (*baseRef == ""):
		if *targetRepo == "" {
			return fail("--base-ref needs --target-repository (the absolute path of the checkout)")
		}
		return fail("--target-repository needs --base-ref (the branch)")
	case *targetRepo != "":
		opts.Target = &compile.Target{Repository: *targetRepo, BaseRef: *baseRef}
	}
	owner := *thread
	if owner == "" {
		owner = strings.TrimSpace(d.Getenv("CODEX_THREAD_ID"))
	}
	b := &Binding{Thread: owner}
	if owner != "" {
		b.Scopes, b.Err = d.Scopes(ctx, owner, *state, *socket)
	}
	opts.Binding = b

	reader, err := d.NewReader(ref.Repo)
	if err != nil {
		return fail("%v", err)
	}
	res, err := Build(ctx, reader, ref, opts)
	if err != nil {
		return fail("%v", err)
	}
	for _, w := range res.Warnings {
		where := ""
		if w.Issue != "" {
			where = w.Issue + ": "
		}
		fmt.Fprintf(stderr, "crw flow plan-input: warning: [%s] %s%s\n", w.Rule, where, w.Detail)
	}
	var doc any = res.Input
	if *envelope {
		warnings := res.Warnings
		if warnings == nil {
			warnings = []Warning{}
		}
		doc = Envelope{Schema: EnvelopeSchema, Milestone: ref.String(), Input: res.Input, Warnings: warnings}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fail("%v", err)
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}

// ReadScopes reads the project keys of the live parent bindings of thread from the relay store of the state, read-only: the reading
// `crw flow start` compares a plan's project key with (CRW-1317).
func ReadScopes(ctx context.Context, thread, state, socket string) ([]string, error) {
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, err
		}
	}
	sel, err := store.ResolveStateDir(state, socket)
	if err != nil {
		return nil, err
	}
	db := sel.DBPath()
	if err := store.CheckReadable(db); err != nil {
		return nil, fmt.Errorf("there is no relay store at %s (%s): %w", db, sel.Detail, err)
	}
	st, err := store.OpenReadOnlyStore(ctx, db)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return dag.LiveParentScopes(ctx, st.DB, thread)
}
