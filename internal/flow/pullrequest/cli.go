// Package pullrequest is `crw flow pr`, the project session's side of PR-unit landing (CRW-1355): the run lands the milestone's
// nodes on the milestone integration branch (the plan's target base_ref), and the project session opens one pull request from that
// branch toward dev naming the issues it carries, and reads its state and checks. The dev merge is the dev queue's (`crw manage
// integrate`, internal/integrator), never this command's. The command goes through the tracking adapter (internal/tracking).
package pullrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// Command is the word after `crw flow`.
const Command = "pr"

// Usage is the usage of the command.
const Usage = `usage: crw flow pr open --repo OWNER/NAME --head BRANCH [--base BRANCH] --title T [--body TEXT | --body-file PATH] --issue REF... [--draft] [--push DIR [--remote NAME]]
       crw flow pr open --repo OWNER/NAME --head BRANCH [--base BRANCH] --title T --run RUN --push DIR [--remote NAME] [--issue REF...] [--draft] [--state DIR] [--socket PATH]
       crw flow pr show --pr OWNER/NAME#N [--checks]
       crw flow pr branch --checkout DIR --milestone M [--base BRANCH] [--name BRANCH]`

const usageHelp = Usage + `

The project session's pull request of a milestone (PR-unit landing): the run lands the milestone's nodes on the milestone integration
branch (the plan's target base_ref), and this command opens one pull request from that branch toward dev and reads it back. The dev
merge is done by the dev queue (crw manage integrate --enqueue-pr REF, then its pass), never by this command.

  open   opens the pull request in OWNER/NAME from --head onto --base (default dev). Its body is --body (or the file) followed by one
         line "Carries: REF, REF" naming the issues it carries (--issue, repeated or comma-separated, OWNER/NAME#N, at least one).
         With --push DIR the head branch of the checkout DIR is pushed to --remote (default origin; a remote configured in DIR, such as origin or team/review, not a URL or a path) first, never forced: a push the
         remote refuses opens nothing.
         With --run RUN the pull request is opened from the run's journal instead of a text: the run must have closed its milestone
         (finished completed, outcome closed, the target not moved after the gate) and the milestone's crosscheck must be verified at
         the close tip (a verdict row; an evidence row, a needs_changes or a verified row of another head is not accepted). The body is
         made from the run: the result, the issues it closes, the gate's verification, the crosscheck and its follow-ups. The issues are
         the milestone nodes' OWNER:NAME:N keys (--issue, when given, must name the same; a milestone whose keys are not of that form
         takes --issue as given). --run needs --push DIR and pushes exactly the crosschecked tip, never forced: the checkout's --head
         branch must be at that tip. --body and --body-file are not taken. --state DIR and --socket PATH select the relay store the run
         is read from (read-only). The title and the body are refused (exit 2) when they hold a path under /home, /state or /scratch,
         the host's name, a thread, call or run id or a secret; this check also runs on --body.
  show   reads the pull request: state, merged, head commit, head and base branch, the issues it carries; --checks adds the check
         runs and commit statuses of its head and their judgment (none, pending, success, failure).
  branch makes the bundle's integration branch in the checkout DIR: the local branch --name (default bundle/M) at the tip of the local
         branch --base (default dev), created once. An existing branch that contains the base's tip is reused as it is (created false);
         one that does not is refused (exit 2) and never moved, and nothing is fetched or pushed. The answer names the branch, the base
         and its tip, the branch's tip and whether this call created it.

The answer is one JSON object on stdout. The token is GH_TOKEN, GITHUB_TOKEN or gh auth token. Exit 0 done; 1 GitHub or git failed
(nothing is retried); 2 an input that cannot be used.
`

// Deps are what the command reaches outside.
type Deps struct {
	// NewPullRequests makes the pull request side of the tracking adapter for the repository "owner/name".
	NewPullRequests func(repo string) (tracking.PullRequests, error)
	// Env is the environment git runs with (nil: the process's), without its routing variables (pushEnv).
	Env []string
	// Journal reads the rows of a run from the relay store src selects (nil: `open --run` cannot be served).
	Journal func(ctx context.Context, src Source, run string) ([]engine.Event, error)
	// Hostname and Home are the host's name and the user's home directory the leak check looks for (nil: os.Hostname, os.UserHomeDir).
	Hostname, Home func() string
}

// Default is the production dependencies.
func Default() *Deps {
	return &Deps{NewPullRequests: func(repo string) (tracking.PullRequests, error) {
		return tracking.NewGitHub(repo, tracking.DefaultTokenSource())
	}, Journal: readJournal}
}

// leakContext is what is local to this host, for the leak check.
func (d *Deps) leakContext() LeakContext {
	var c LeakContext
	if d.Hostname != nil {
		c.Hostname = d.Hostname()
	} else if h, err := os.Hostname(); err == nil {
		c.Hostname = h
	}
	if d.Home != nil {
		c.Home = d.Home()
	} else if h, err := os.UserHomeDir(); err == nil {
		c.Home = h
	}
	return c
}

// Handles reports whether `crw flow args...` is this command.
func Handles(args []string) bool { return len(args) > 0 && args[0] == Command }

// RunCLI is `crw flow pr args[1:]` over the production dependencies.
func RunCLI(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == Command {
		args = args[1:]
	}
	return RunWith(ctx, Default(), args, stdout, stderr)
}

// View is a pull request as the answer prints it.
type View struct {
	Ref     string   `json:"ref"`
	URL     string   `json:"url"`
	Title   string   `json:"title"`
	State   string   `json:"state"`
	Merged  bool     `json:"merged"`
	Draft   bool     `json:"draft"`
	Head    string   `json:"head"`
	HeadRef string   `json:"head_ref"`
	Base    string   `json:"base"`
	Issues  []string `json:"issues"`
}

// CheckView is one check of the answer.
type CheckView struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	State      string `json:"state"`
	URL        string `json:"url,omitempty"`
}

// ChecksView is the checks of the answer.
type ChecksView struct {
	Head   string      `json:"head"`
	State  string      `json:"state"`
	Checks []CheckView `json:"checks"`
}

// Answer is what the command prints.
type Answer struct {
	PullRequest View        `json:"pull_request"`
	Pushed      string      `json:"pushed,omitempty"`
	Checks      *ChecksView `json:"checks,omitempty"`
	Warnings    []string    `json:"warnings,omitempty"`
}

func viewOf(pr tracking.PullRequest) View {
	v := View{Ref: pr.Ref.String(), URL: pr.URL, Title: pr.Title, State: string(pr.State), Merged: pr.Merged, Draft: pr.Draft,
		Head: pr.HeadSHA, HeadRef: pr.HeadRef, Base: pr.BaseRef, Issues: []string{}}
	for _, is := range pr.Issues {
		v.Issues = append(v.Issues, is.String())
	}
	return v
}

// issueList collects --issue values, repeated or comma-separated.
type issueList []string

func (l *issueList) String() string { return strings.Join(*l, ",") }
func (l *issueList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*l = append(*l, part)
		}
	}
	return nil
}

// RunWith is the command over explicit dependencies; args are the arguments after the command word.
func RunWith(ctx context.Context, d *Deps, args []string, stdout, stderr io.Writer) int {
	usage := func(format string, a ...any) int {
		fmt.Fprintln(stderr, Usage)
		fmt.Fprintf(stderr, "crw flow pr: error: "+format+"\n", a...)
		return 2
	}
	failed := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "crw flow pr: error: "+format+"\n", a...)
		return 1
	}
	if len(args) == 0 {
		return usage("a command is required: open, show or branch")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(stdout, usageHelp)
		return 0
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("crw flow pr "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	emit := func(a Answer) int {
		data, err := json.Marshal(a)
		if err == nil {
			_, err = fmt.Fprintf(stdout, "%s\n", data)
		}
		if err != nil {
			return failed("write the answer: %v", err)
		}
		return 0
	}
	parse := func() (bool, int) {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, usageHelp)
				return false, 0
			}
			return false, usage("%v", err)
		}
		if fs.NArg() > 0 {
			return false, usage("takes no argument, got %q", fs.Arg(0))
		}
		return true, 0
	}
	switch sub {
	case "open":
		repo := fs.String("repo", "", "the repository OWNER/NAME")
		head := fs.String("head", "", "the milestone integration branch")
		base := fs.String("base", "dev", "the branch the pull request goes to")
		title := fs.String("title", "", "the title")
		body := fs.String("body", "", "the body text")
		bodyFile := fs.String("body-file", "", "a file holding the body text")
		draft := fs.Bool("draft", false, "open it as a draft")
		push := fs.String("push", "", "the checkout whose head branch is pushed first")
		remote := fs.String("remote", "origin", "the remote --push pushes to")
		runID := fs.String("run", "", "the run whose closed milestone the pull request is opened for")
		state := fs.String("state", "", "the relay state directory --run reads the run from")
		socket := fs.String("socket", "", "the relay control socket --run reads the run through")
		var issues issueList
		fs.Var(&issues, "issue", "an issue the pull request carries, OWNER/NAME#N")
		if ok, code := parse(); !ok {
			return code
		}
		if *repo == "" {
			return usage("--repo OWNER/NAME is required: the repository the pull request is opened in")
		}
		if *body != "" && *bodyFile != "" {
			return usage("--body and --body-file are one choice")
		}
		if *runID == "" && (*state != "" || *socket != "") {
			return usage("--state and --socket select the store --run reads: give --run")
		}
		if *runID != "" {
			if *body != "" || *bodyFile != "" {
				return usage("--run makes the body from the run: --body and --body-file are not taken")
			}
			if *push == "" {
				return usage("--run pushes the crosschecked tip itself: give --push DIR")
			}
		}
		text := *body
		if *bodyFile != "" {
			data, err := os.ReadFile(*bodyFile)
			if err != nil {
				return usage("--body-file: %v", err)
			}
			text = string(data)
		}
		in := tracking.NewPullRequest{Head: *head, Base: *base, Title: *title, Body: text, Draft: *draft}
		for _, s := range issues {
			ref, err := tracking.ParseRef(s)
			if err != nil {
				return usage("--issue %q is not OWNER/NAME#N", s)
			}
			in.Issues = append(in.Issues, ref)
		}
		prs, err := d.NewPullRequests(*repo)
		if err != nil {
			return usage("%v", err)
		}
		// a refusal of --run: one line, exit 2, before anything is pushed or sent
		refuse := func(reason, format string, a ...any) int {
			fmt.Fprintf(stderr, "crw flow pr: error: %s: %s\n", reason, fmt.Sprintf(format, a...))
			return 2
		}
		tip := ""
		if *runID != "" {
			if d.Journal == nil {
				return failed("no run reader is wired")
			}
			events, err := d.Journal(ctx, Source{State: *state, Socket: *socket}, *runID)
			if errors.Is(err, ErrNoRun) {
				return refuse(reasonRunUnknown, "%v", err)
			}
			if err != nil {
				return failed("the run %s cannot be read: %v", *runID, err)
			}
			facts, err := FactsOf(events)
			if err != nil {
				return failed("the run %s cannot be read: %v", *runID, err)
			}
			if !facts.Closed {
				return refuse(reasonRunNotClosed, "%s", facts.Why)
			}
			if _, reason := crosscheckOf(events, facts.Body.Milestone, facts.Body.Tip); reason != "" {
				return refuse(reason, "%s", crosscheckWhy(reason, facts.Body.Milestone, facts.Body.Tip))
			}
			refs, reason, message := carriedRefs(facts.Nodes, in.Issues, *repo)
			if reason != "" {
				return refuse(reason, "%s", message)
			}
			in.Issues, in.Body, tip = refs, Body(facts.Body, refs), facts.Body.Tip
		}
		// the input is checked before anything is pushed: what the adapter would refuse pushes nothing
		if err := in.Check(); err != nil {
			return usage("%v", err)
		}
		if leaks := Leaks(publishedText(in), d.leakContext()); len(leaks) > 0 {
			return refuse(reasonBodyLeak, "%s", leakMessage(leaks))
		}
		if *push != "" {
			// a configured remote of the checkout and nothing else, refused before anything is pushed
			configured, err := configuredRemote(ctx, d.Env, *push, *remote)
			if err != nil {
				return failed("%v; nothing is pushed and no pull request is opened", err)
			}
			if !configured {
				return usage("--remote %q is not a remote configured in %s", *remote, *push)
			}
		}
		if tip != "" {
			// the head that is opened is the commit the crosscheck judged: the local branch must be at it
			local, err := localTip(ctx, d.Env, *push, *head)
			if err != nil {
				return failed("%v; nothing is pushed and no pull request is opened", err)
			}
			if local != tip {
				return refuse(reasonCrosscheckMoved, "the branch %s of %s is at %s, the crosschecked tip is %s", *head, *push, orNone(short(local, 12)), short(tip, 12))
			}
		}
		var ans Answer
		switch {
		case tip != "":
			if err := pushCommit(ctx, d.Env, *push, *remote, *head, tip); err != nil {
				return failed("%v; no pull request is opened", err)
			}
			ans.Pushed = tip
		case *push != "":
			pushed, err := pushBranch(ctx, d.Env, *push, *remote, *head)
			if err != nil {
				return failed("%v; no pull request is opened", err)
			}
			ans.Pushed = pushed
		}
		pr, err := prs.OpenPullRequest(ctx, in)
		if errors.Is(err, tracking.ErrInvalidPullRequest) {
			return usage("%v", err)
		}
		if err != nil {
			return failed("%v", err)
		}
		ans.PullRequest = viewOf(pr)
		if tip != "" {
			// the remote branch moved between the push and the POST, a window the REST API cannot close: detected, not prevented. The
			// pull request exists, and the merge request refuses it until a verified crosscheck stands at its head.
			if pr.HeadSHA != tip {
				ans.Warnings = append(ans.Warnings, fmt.Sprintf("pr_head_moved: the pull request's head is %s, the crosschecked tip is %s", short(pr.HeadSHA, 12), short(tip, 12)))
				if code := emit(ans); code != 0 {
					return code
				}
				return 1
			}
		} else if ans.Pushed != "" && pr.HeadSHA != ans.Pushed {
			ans.Warnings = append(ans.Warnings, fmt.Sprintf("the pull request's head is %s, the pushed branch is at %s", pr.HeadSHA, ans.Pushed))
		}
		return emit(ans)
	case "show":
		ref := fs.String("pr", "", "the pull request OWNER/NAME#N")
		checks := fs.Bool("checks", false, "read the checks of its head")
		if ok, code := parse(); !ok {
			return code
		}
		r, err := tracking.ParseRef(*ref)
		if err != nil {
			return usage("--pr %q is not OWNER/NAME#N", *ref)
		}
		prs, err := d.NewPullRequests(r.Repo)
		if err != nil {
			return usage("%v", err)
		}
		pr, err := prs.PullRequest(ctx, r)
		if err != nil {
			return failed("%v", err)
		}
		ans := Answer{PullRequest: viewOf(pr)}
		if *checks {
			c, err := prs.Checks(ctx, r)
			if err != nil {
				return failed("%v", err)
			}
			cv := &ChecksView{Head: c.Head, State: string(c.State), Checks: []CheckView{}}
			for _, ch := range c.Checks {
				cv.Checks = append(cv.Checks, CheckView{Name: ch.Name, Kind: ch.Kind, Status: ch.Status, Conclusion: ch.Conclusion, State: string(ch.State), URL: ch.URL})
			}
			ans.Checks = cv
		}
		return emit(ans)
	case "branch":
		checkout := fs.String("checkout", "", "the checkout holding the branches")
		milestone := fs.String("milestone", "", "the milestone the branch carries")
		base := fs.String("base", "dev", "the local branch the integration branch is cut from")
		name := fs.String("name", "", "the integration branch (default bundle/<milestone>)")
		if ok, code := parse(); !ok {
			return code
		}
		if *checkout == "" {
			return usage("--checkout DIR is required: the checkout the branch is made in")
		}
		if *milestone == "" {
			return usage("--milestone M is required: the milestone the branch carries")
		}
		if *name == "" {
			*name = "bundle/" + *milestone
		}
		ans, err := Branch(ctx, d.Env, *checkout, *base, *name)
		var unusable inputError
		if errors.As(err, &unusable) {
			fmt.Fprintf(stderr, "crw flow pr: error: %v\n", unusable)
			return 2
		}
		if err != nil {
			return failed("%v", err)
		}
		data, err := json.Marshal(ans)
		if err == nil {
			_, err = fmt.Fprintf(stdout, "%s\n", data)
		}
		if err != nil {
			return failed("write the answer: %v", err)
		}
		return 0
	}
	return usage("unknown command %q: open, show or branch", sub)
}

// configuredRemote says whether name is one of the remotes the checkout configures (git remote): any name git lists, team/review
// and foo@bar included. A value that starts with a dash is never one, and a URL or a path is not a configured name.
func configuredRemote(ctx context.Context, env []string, checkout, name string) (bool, error) {
	if name == "" || strings.HasPrefix(name, "-") {
		return false, nil
	}
	list, err := gitIn(ctx, env, checkout, "remote")
	if err != nil {
		return false, fmt.Errorf("the remotes of %s could not be listed: %w", checkout, err)
	}
	for _, line := range strings.Split(list, "\n") {
		if line == name {
			return true, nil
		}
	}
	return false, nil
}

// gitIn runs git in the checkout with the push's environment and answers its trimmed standard output.
func gitIn(ctx context.Context, env []string, checkout string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", checkout}, args...)...)
	cmd.Env = pushEnv(env)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// pushEnv is the environment of the push's git calls: env (nil is the process environment) without the routing variables
// (GIT_DIR, GIT_WORK_TREE, GIT_OBJECT_DIRECTORY, GIT_NAMESPACE, ...), so -C names the repository, and GIT_TERMINAL_PROMPT=0 in
// place of any value env had, as the dev queue (internal/integrator) builds its git environment.
func pushEnv(env []string) []string {
	return gitprobe.Sanitize(env, "GIT_TERMINAL_PROMPT=0")
}

// pushBranch pushes refs/heads/<branch> of the checkout to the remote's branch of the same name, never forced, and returns the commit
// it pushed.
func pushBranch(ctx context.Context, env []string, checkout, remote, branch string) (string, error) {
	tip, err := gitIn(ctx, env, checkout, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("push: the checkout %s has no branch %s: %w", checkout, branch, err)
	}
	if err := pushCommit(ctx, env, checkout, remote, branch, tip); err != nil {
		return "", err
	}
	return tip, nil
}

// pushCommit pushes the commit sha to the remote's branch, never forced: no plus and no --force, so a remote branch that is not an
// ancestor of sha refuses the push. The commit is named by its sha, so a move of the local branch after the caller judged it cannot
// change what is pushed.
func pushCommit(ctx context.Context, env []string, checkout, remote, branch, sha string) error {
	if _, err := gitIn(ctx, env, checkout, "push", "--porcelain", "--", remote, sha+":refs/heads/"+branch); err != nil {
		return fmt.Errorf("push of %s to %s refused: %w", branch, remote, err)
	}
	return nil
}

// localTip is the commit of refs/heads/<branch> in the checkout, "" when the checkout has no such branch (rev-parse --verify --quiet
// answers 1 for a missing ref; any other git failure is returned).
func localTip(ctx context.Context, env []string, checkout, branch string) (string, error) {
	tip, err := gitIn(ctx, env, checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if code, ok := gitExitCode(err); ok && code == 1 {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("the branch %s of %s could not be read: %w", branch, checkout, err)
	}
	return tip, nil
}

// publishedText is everything a pull request publishes: the title, and the body with the Carries line tracking appends.
func publishedText(in tracking.NewPullRequest) string {
	var refs []string
	for _, r := range in.Issues {
		refs = append(refs, r.String())
	}
	return in.Title + "\n" + in.Body + "\nCarries: " + strings.Join(refs, ", ")
}

// crosscheckWhy is the sentence of a refused crosscheck.
func crosscheckWhy(reason, milestone, tip string) string {
	switch reason {
	case reasonCrosscheckMissing:
		return "the run holds no crosscheck of milestone " + milestone
	case reasonCrosscheckMoved:
		return "the crosschecks of milestone " + milestone + " are of other heads, none of the close tip " + short(tip, 12)
	}
	return "the crosscheck of milestone " + milestone + " at the close tip " + short(tip, 12) + " is not a verified verdict"
}
