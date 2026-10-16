package publication

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// Deps are the seams a test replaces: the environment the policy is read through, the local context
// the scrub uses, and the adapter factory (which points at a fake forge).
type Deps struct {
	Getenv    func(string) string
	Context   *publishpolicy.Context
	NewGitHub func(string, publishpolicy.Policy, publishpolicy.Context, func(publishpolicy.Event)) (*tracking.GitHub, error)
}

func (d Deps) getenv() func(string) string {
	if d.Getenv != nil {
		return d.Getenv
	}
	return os.Getenv
}

func (d Deps) pubctx() publishpolicy.Context {
	if d.Context != nil {
		return *d.Context
	}
	return publishpolicy.HostContext()
}

// github makes the adapter for repo: the repository's configured policy (a bad one is a usage
// error), the scrub context and the stderr event sink. The NewGitHub seam lets a test run over a
// fake without a token or a host.
func (d Deps) github(repo string, stderr io.Writer) (*tracking.GitHub, error) {
	p, err := Policy(repo, d.getenv())
	if err != nil {
		return nil, err
	}
	if d.NewGitHub != nil {
		return d.NewGitHub(repo, p, d.pubctx(), Recorder(stderr))
	}
	return tracking.NewGitHub(repo, tracking.DefaultTokenSource(), tracking.WithPublication(p, d.pubctx(), Recorder(stderr)))
}

// Run is `crw publish` over the real environment.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunWith(ctx, args, stdin, stdout, stderr, Deps{})
}

// RunWith is `crw publish`: issue, milestone and comment creation through the tracking adapter's
// scrub boundary, and prepare, the offline form of the same scrub. Exit 0 success, 2 a bad command
// line, a bad configuration or a blocked text, 1 a network or file failure. A blocked text is
// ErrSecret: the error names that and never the draft.
func RunWith(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, d Deps) int {
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw publish: error: the following arguments are required: command")
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	case "issue", "milestone", "comment", "prepare":
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw publish: error: invalid command %q (choose from 'issue', 'milestone', 'comment', 'prepare')\n", verb)
		return 2
	}
	switch verb {
	case "issue", "milestone":
		return runCreate(ctx, verb, rest, stdin, stdout, stderr, d)
	case "comment":
		return runComment(ctx, rest, stdin, stdout, stderr, d)
	}
	return runPrepare(verb, rest, stdin, stdout, stderr, d)
}

// jsonOut writes one result object to stdout; a writer that fails is a 1.
func jsonOut(stdout, stderr io.Writer, fields ...any) int {
	obj := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		key, _ := fields[i].(string)
		obj[key] = fields[i+1]
	}
	if err := json.NewEncoder(stdout).Encode(obj); err != nil {
		fmt.Fprintf(stderr, "crw publish: error: write the answer: %v\n", err)
		return 1
	}
	return 0
}

// errOut writes one safe error line; a secret draft never reaches it.
func errOut(verb string, stderr io.Writer, err error, usage bool) int {
	fmt.Fprintf(stderr, "crw publish %s: error: %v\n", verb, err)
	if usage {
		return 2
	}
	return 1
}

// bodyText is the --body text or the file --body-file names (- is stdin). The mutual exclusion was
// checked before this is called, so the error here is only a read failure (exit 1).
func bodyText(body, bodyFile string, hasFile bool, stdin io.Reader) ([]byte, error) {
	if hasFile {
		if bodyFile == "-" {
			return io.ReadAll(stdin)
		}
		return os.ReadFile(bodyFile)
	}
	return []byte(body), nil
}

// bodyFlags is the --body and --body-file values and whether --body-file was given (an explicitly
// empty --body still counts as supplied); giving both is refused.
func bodyFlags(verb string, fs *flag.FlagSet, stderr io.Writer) (string, string, bool, bool) {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["body"] && set["body-file"] {
		errOut(verb, stderr, errors.New("--body and --body-file are mutually exclusive"), true)
		return "", "", false, false
	}
	b, bf := fs.Lookup("body"), fs.Lookup("body-file")
	if b == nil || bf == nil {
		return "", "", false, false
	}
	return b.Value.String(), bf.Value.String(), set["body-file"], true
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runCreate(ctx context.Context, verb string, args []string, stdin io.Reader, stdout, stderr io.Writer, d Deps) int {
	fs := flag.NewFlagSet("crw publish "+verb, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "the repository to publish to, OWNER/NAME")
	title := fs.String("title", "", "the title, one line")
	fs.String("body", "", "the body text (the milestone's description)")
	fs.String("body-file", "", "a file of the body text (- is stdin)")
	var milestone *int
	var labels multiFlag
	if verb == "issue" {
		milestone = fs.Int("milestone", 0, "the milestone number")
		fs.Var(&labels, "label", "a label to set (repeatable)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return 0
		}
		return errOut(verb, stderr, err, true)
	}
	if fs.NArg() > 0 {
		return errOut(verb, stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)), true)
	}
	bv, bfv, hasFile, okBody := bodyFlags(verb, fs, stderr)
	if !okBody {
		return 2
	}
	if crwconfig.ValidateRepo(*repo) != nil {
		return errOut(verb, stderr, errors.New("--repo must be OWNER/NAME"), true)
	}
	if strings.TrimSpace(*title) == "" {
		return errOut(verb, stderr, errors.New("--title is required"), true)
	}
	text, err := bodyText(bv, bfv, hasFile, stdin)
	if err != nil {
		return errOut(verb, stderr, err, false)
	}
	g, err := d.github(*repo, stderr)
	if err != nil {
		return errOut(verb, stderr, err, true)
	}
	switch verb {
	case "issue":
		is, err := g.OpenIssue(ctx, tracking.NewIssue{Title: *title, Body: string(text), Milestone: *milestone, Labels: labels})
		if err != nil {
			return errOut(verb, stderr, err, errors.Is(err, publishpolicy.ErrSecret) || errors.Is(err, tracking.ErrInvalidIssue))
		}
		return jsonOut(stdout, stderr, "repo", *repo, "issue", map[string]any{"number": is.Ref.Number, "url": is.URL, "title": is.Title})
	case "milestone":
		m, err := g.OpenMilestone(ctx, tracking.NewMilestone{Title: *title, Description: string(text)})
		if err != nil {
			return errOut(verb, stderr, err, errors.Is(err, publishpolicy.ErrSecret) || errors.Is(err, tracking.ErrInvalidMilestone))
		}
		return jsonOut(stdout, stderr, "repo", *repo, "milestone", map[string]any{"number": m.Ref.Number, "title": m.Title})
	}
	return 2
}

func runComment(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, d Deps) int {
	fs := flag.NewFlagSet("crw publish comment", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	issue := fs.String("issue", "", "the issue or pull request to comment on, OWNER/NAME#N")
	header := fs.String("header", "", `the comment header, "[layer · session]"`)
	fs.String("body", "", "the comment text")
	fs.String("body-file", "", "a file of the comment text (- is stdin)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return 0
		}
		return errOut("comment", stderr, err, true)
	}
	if fs.NArg() > 0 {
		return errOut("comment", stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)), true)
	}
	bv, bfv, hasFile, okBody := bodyFlags("comment", fs, stderr)
	if !okBody {
		return 2
	}
	ref, err := tracking.ParseRef(*issue)
	if err != nil {
		return errOut("comment", stderr, errors.New("--issue must be OWNER/NAME#N"), true)
	}
	if *header == "" {
		return errOut("comment", stderr, errors.New("--header is required"), true)
	}
	text, err := bodyText(bv, bfv, hasFile, stdin)
	if err != nil {
		return errOut("comment", stderr, err, false)
	}
	g, err := d.github(ref.Repo, stderr)
	if err != nil {
		return errOut("comment", stderr, err, true)
	}
	if err := g.Comment(ctx, ref, *header, string(text)); err != nil {
		return errOut("comment", stderr, err,
			errors.Is(err, publishpolicy.ErrSecret) || errors.Is(err, tracking.ErrInvalidHeader))
	}
	return jsonOut(stdout, stderr, "repo", ref.Repo, "comment", map[string]any{"issue": ref.Number})
}

var prepareKinds = map[string]bool{"issue": true, "milestone": true, "comment": true, "pr": true, "commit": true}

func runPrepare(verb string, args []string, stdin io.Reader, stdout, stderr io.Writer, d Deps) int {
	fs := flag.NewFlagSet("crw publish prepare", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "the repository the text is for, OWNER/NAME")
	kind := fs.String("kind", "", "what the text is: issue, milestone, comment, pr or commit")
	input := fs.String("input", "", "the draft file (- is stdin)")
	output := fs.String("output", "", "where the sanitized text goes")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return 0
		}
		return errOut(verb, stderr, err, true)
	}
	if fs.NArg() > 0 {
		return errOut(verb, stderr, fmt.Errorf("unexpected argument %q", fs.Arg(0)), true)
	}
	if crwconfig.ValidateRepo(*repo) != nil {
		return errOut(verb, stderr, errors.New("--repo must be OWNER/NAME"), true)
	}
	if !prepareKinds[*kind] {
		return errOut(verb, stderr, errors.New("--kind must be one of issue, milestone, comment, pr, commit"), true)
	}
	if *input == "" || *output == "" {
		return errOut(verb, stderr, errors.New("--input and --output are required"), true)
	}
	var draft []byte
	var err error
	if *input == "-" {
		draft, err = io.ReadAll(stdin)
	} else {
		draft, err = os.ReadFile(*input)
	}
	if err != nil {
		return errOut(verb, stderr, err, false)
	}
	p, err := Policy(*repo, d.getenv())
	if err != nil {
		return errOut(verb, stderr, err, true)
	}
	text, err := Prepare(*repo, "prepare", *kind, string(draft), d.pubctx(), p, stderr)
	if err != nil {
		return errOut(verb, stderr, err, true)
	}
	// atomic: a temporary file beside the target (mode 0600), renamed over it; the target keeps its
	// bytes when anything fails and nothing new is made but the answer.
	dir := filepath.Dir(*output)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return errOut(verb, stderr, fmt.Errorf("the output directory does not exist"), false)
	}
	tmp, err := os.CreateTemp(filepath.Dir(*output), ".publish-*")
	if err != nil {
		return errOut(verb, stderr, err, false)
	}
	_, werr := tmp.Write([]byte(text))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return errOut(verb, stderr, err, false)
	}
	if err := os.Rename(tmp.Name(), *output); err != nil {
		os.Remove(tmp.Name())
		return errOut(verb, stderr, err, false)
	}
	return jsonOut(stdout, stderr, "repo", *repo, "kind", *kind, "output", *output)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw publish [-h] {issue,milestone,comment,prepare} ...")
	fmt.Fprintln(w, "  issue\topen an issue: --repo OWNER/NAME --title TITLE [--body TEXT|--body-file FILE] [--milestone N] [--label L]...")
	fmt.Fprintln(w, "  milestone\topen a milestone: --repo OWNER/NAME --title TITLE [--body TEXT|--body-file FILE]")
	fmt.Fprintln(w, `  comment\tcomment on an issue or pull request: --issue OWNER/NAME#N --header "[layer · session]" [--body TEXT|--body-file FILE]`)
	fmt.Fprintln(w, "  prepare\tscrub a draft offline: --repo OWNER/NAME --kind issue|milestone|comment|pr|commit --input FILE --output FILE")
}
