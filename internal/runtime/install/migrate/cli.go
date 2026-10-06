package migrate

// cli.go is the M4 explicit surface of docs/port-cxc/state-migration.md: `crw install migrate-state`,
// the one caller of classify, apply and attention. It parses the command line, resolves the selected
// scope's roots from the environment it was given, preflights the whole scope and writes through
// apply. Nothing calls it implicitly: no hook, installer, activation or startup path reaches this
// package, and the copy runs only when an operator names the command (the transition window is Jun's
// decision J2).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// UsageText is this command's own help. The legacy `crw install` usage line is a frozen contract and
// does not name this command.
const UsageText = `usage: crw install migrate-state [--scope project|user|codex|all] [--cwd <workspace>]
                               [--from-home <old-user-root>] [--to-home <new-user-root>]
                               [--codex-home <codex-root>] [--dry-run] [--json]
                               [--report <absent-file>]

Copy CXC v0.2.40 state to its CRW locations, byte for byte, and refuse rather than replace.
The default scope is project (the workspace's .codexclaw to its .crw). --from-home and --to-home
are read only with user or all; --codex-home only with codex or all. The report destination must
be absent and outside the source and destination trees.

A dry run performs the same reads, classification and conflict checks and writes nothing at all:
no root, no report, no temporary, no .gitignore. Its copied count is what a real run would copy.

Exit 0: a verified copy, an already-equal run, or a dry run. Exit 1: a conflict, unsafe input, a
source that changed, an interruption, or an I/O or unsupported-filesystem failure, with the writes
this run completed reported. Exit 2: usage. Cancellation follows the install mode's signal
handling and returns a failed or partial report.
`

// The command's exit codes: the installer's own numbers, without its JSON envelope.
const (
	codeOK      = 0
	codeRefused = 1
	codeUsage   = 2
)

// cli is one parsed command line.
type cli struct {
	scope     Scope
	cwd       string
	fromHome  string
	toHome    string
	codexHome string
	dryRun    bool
	jsonOut   bool
	report    string
}

// Run is `crw install migrate-state`. env is the environment the roots are read from, so the
// caller's environment decides and this process's own is not consulted behind it.
func Run(ctx context.Context, args []string, env []string, stdout, stderr io.Writer) int {
	o, help, err := parseCLI(args)
	if help {
		fmt.Fprint(stdout, UsageText)
		return codeOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "crw install migrate-state: error: "+err.Error())
		fmt.Fprint(stderr, UsageText)
		return codeUsage
	}
	opt, err := o.options(env)
	if err != nil {
		fmt.Fprintln(stderr, "crw install migrate-state: error: "+err.Error())
		return codeUsage
	}
	roots, err := Open(opt)
	if err != nil {
		// Open writes nothing: an unsafe root is refused before any read of the scope.
		report := &Report{DryRun: o.dryRun, Scope: o.scope, Roots: namedRoots(opt), Error: reportError(err)}
		report.Result = summarize(report)
		fmt.Fprintln(stderr, "crw install migrate-state: "+err.Error())
		return o.emit(stdout, stderr, report)
	}
	defer roots.Close()

	target, err := o.target(roots)
	if err != nil {
		fmt.Fprintln(stderr, "crw install migrate-state: error: "+err.Error())
		return codeUsage
	}
	if target != nil {
		defer target.parent.Close()
	}
	if err := ctx.Err(); err != nil {
		return o.emit(stdout, stderr, o.assemble(roots, nil, nil, nil, err))
	}
	plan, err := classify(roots)
	if err != nil {
		return o.emit(stdout, stderr, o.assemble(roots, plan, nil, nil, err))
	}
	atts := attention(roots, plan)
	if o.dryRun {
		return o.emit(stdout, stderr, o.assemble(roots, plan, atts, nil, nil))
	}
	if err := ctx.Err(); err != nil {
		return o.emit(stdout, stderr, o.assemble(roots, plan, atts, nil, err))
	}
	res, err := apply(roots, plan)
	report := o.assemble(roots, plan, atts, res, err)
	code := o.emit(stdout, stderr, report)
	if err == nil && target != nil {
		if perr := target.publish(report); perr != nil {
			fmt.Fprintln(stderr, "crw install migrate-state: the report was not published: "+perr.Error())
			return codeRefused
		}
	}
	return code
}

// parseCLI reads the command line. A help request is reported as help with no error.
func parseCLI(args []string) (cli, bool, error) {
	var o cli
	var scopeName string
	var help bool
	fs := flag.NewFlagSet("crw install migrate-state", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&scopeName, "scope", "", "the stores to copy: project (default), user, codex or all")
	fs.StringVar(&o.cwd, "cwd", "", "the workspace whose .codexclaw and .crw the project scope reads and writes (default the working directory)")
	fs.StringVar(&o.fromHome, "from-home", "", "the old user root (default $CODEXCLAW_HOME, else ~/.codexclaw); user or all only")
	fs.StringVar(&o.toHome, "to-home", "", "the new user root (default $CRW_HOME, else ~/.crw); user or all only")
	fs.StringVar(&o.codexHome, "codex-home", "", "the Codex home (default $CODEX_HOME, else ~/.codex); codex or all only")
	fs.BoolVar(&o.dryRun, "dry-run", false, "read, classify and check conflicts, and write nothing")
	fs.BoolVar(&o.jsonOut, "json", false, "write the report as JSON instead of text")
	fs.StringVar(&o.report, "report", "", "publish the JSON report to this absent file, outside the source and destination trees")
	fs.BoolVar(&help, "help", false, "print this help and write nothing")
	fs.BoolVar(&help, "h", false, "print this help and write nothing")
	if err := fs.Parse(args); err != nil {
		return cli{}, false, err
	}
	if help {
		return cli{}, true, nil
	}
	if rest := fs.Args(); len(rest) > 0 {
		if len(rest) == 1 && rest[0] == "help" {
			return cli{}, true, nil
		}
		return cli{}, false, fmt.Errorf("unrecognized arguments: %s", strings.Join(rest, " "))
	}
	scope, err := ParseScope(scopeName)
	if err != nil {
		return cli{}, false, err
	}
	o.scope = scope
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if (given["from-home"] || given["to-home"]) && !scope.Has(ScopeUser) {
		return cli{}, false, errors.New("--from-home and --to-home are read only with --scope user or --scope all")
	}
	if given["codex-home"] && !scope.Has(ScopeCodex) {
		return cli{}, false, errors.New("--codex-home is read only with --scope codex or --scope all")
	}
	return o, false, nil
}

// options resolves the selected scope's roots from env: an explicit flag, else the variable in the
// environment the caller passed, else the default under the home. Open would otherwise fall back to
// this process's environment, which the caller did not give it.
func (o cli) options(env []string) (Options, error) {
	opt := Options{Scope: o.scope, Cwd: o.cwd}
	lookup := envLookup(env)
	if o.scope.Has(ScopeUser) {
		var err error
		if opt.FromHome, err = envRoot(lookup, o.fromHome, "CODEXCLAW_HOME", ".codexclaw"); err != nil {
			return Options{}, err
		}
		if opt.ToHome, err = envRoot(lookup, o.toHome, "CRW_HOME", ".crw"); err != nil {
			return Options{}, err
		}
	}
	if o.scope.Has(ScopeCodex) {
		var err error
		if opt.CodexHome, err = envRoot(lookup, o.codexHome, "CODEX_HOME", ".codex"); err != nil {
			return Options{}, err
		}
	}
	return opt, nil
}

// envLookup is os.environ.get with presence over the caller's environment, the last entry winning.
func envLookup(env []string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v, true
			}
		}
		return "", false
	}
}

// envRoot picks the explicit root, else the named variable, else the default under the home.
func envRoot(lookup func(string) (string, bool), explicit, variable, def string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if value, set := lookup(variable); set && value != "" {
		return value, nil
	}
	home, err := envHome(lookup)
	if err != nil {
		return "", fmt.Errorf("%s: %w", variable, err)
	}
	return filepath.Join(home, def), nil
}

// envHome is the home the default roots hang under: HOME when the caller's environment sets it,
// else this user's passwd entry, as Open's own default does.
func envHome(lookup func(string) (string, bool)) (string, error) {
	if home, set := lookup("HOME"); set && home != "" {
		return home, nil
	}
	return os.UserHomeDir()
}

// target validates --report: an absent leaf outside every selected tree, in a directory that exists.
// It returns nil when no report was asked for. Nothing is written here.
func (o cli) target(roots *Roots) (*reportTarget, error) {
	if o.report == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(o.report)
	if err != nil {
		return nil, err
	}
	for _, tree := range rootTrees(roots) {
		if within(tree, abs) {
			return nil, fmt.Errorf("--report %s lies inside %s; the report is written outside the source and destination trees", o.report, tree)
		}
	}
	dirPart, leaf := filepath.Split(abs)
	if leaf == "" || leaf == "." || leaf == ".." {
		return nil, fmt.Errorf("--report %s names no file", o.report)
	}
	parent := filepath.Clean(dirPart)
	dir, _, err := pinDir(parent)
	if err != nil {
		return nil, fmt.Errorf("--report: %w", err)
	}
	if dir == nil {
		return nil, fmt.Errorf("--report: the directory %s does not exist", parent)
	}
	if _, _, err := dir.OpenRegular(leaf); err == nil {
		_ = dir.Close()
		return nil, fmt.Errorf("--report %s already exists; name an absent file", o.report)
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = dir.Close()
		return nil, fmt.Errorf("--report %s: %w", o.report, err)
	}
	return &reportTarget{parent: dir, leaf: leaf}, nil
}

// assemble builds one run's report: every planned item with what happened to it, the attention
// entries and the whole-scope error.
func (o cli) assemble(roots *Roots, plan *Plan, atts []Attention, res *ApplyResult, failure error) *Report {
	rep := &Report{DryRun: o.dryRun, Scope: o.scope, Roots: reportRoots(roots), Attention: atts}
	switch {
	case res != nil:
		rep.Items = res.Items
		rep.WritesCompleted = res.WritesCompleted
		rep.SourceVerified = res.SourceVerified
	case plan != nil:
		for _, it := range plan.Items {
			item := ApplyItem{Item: it}
			if o.dryRun {
				item.Result = ResultDryRun
			}
			rep.Items = append(rep.Items, item)
		}
		if o.dryRun {
			// Classification opened, stat'ed and hashed every copy and transform source once.
			rep.SourceVerified = true
		}
	}
	if failure != nil {
		rep.Error = reportError(failure)
	}
	rep.Result = summarize(rep)
	return rep
}

// reportRoots is the selected roots' source and destination pairs.
func reportRoots(r *Roots) []ReportRoot {
	var out []ReportRoot
	if r.Project != nil {
		out = append(out, ReportRoot{ScopeProject, r.Project.SourcePath, r.Project.DestPath})
	}
	if r.User != nil {
		out = append(out, ReportRoot{ScopeUser, r.User.SourcePath, r.User.DestPath})
	}
	if r.CodexPath != "" {
		out = append(out, ReportRoot{ScopeCodex, r.CodexPath, r.CodexPath})
	}
	return out
}

// namedRoots names the roots of opt as Open resolves them, for a report whose Open was refused.
func namedRoots(opt Options) []ReportRoot {
	scope, err := ParseScope(string(opt.Scope))
	if err != nil {
		return nil
	}
	var out []ReportRoot
	if scope.Has(ScopeProject) {
		if w, err := absRoot(opt.Cwd); err == nil {
			out = append(out, ReportRoot{ScopeProject, filepath.Join(w, ProjectSourceName), filepath.Join(w, ".crw")})
		}
	}
	if scope.Has(ScopeUser) {
		out = append(out, ReportRoot{ScopeUser, opt.FromHome, opt.ToHome})
	}
	if scope.Has(ScopeCodex) {
		out = append(out, ReportRoot{ScopeCodex, opt.CodexHome, opt.CodexHome})
	}
	return out
}

// rootTrees is every source and destination tree of the selected scope.
func rootTrees(r *Roots) []string {
	var out []string
	for _, p := range []*Pair{r.Project, r.User} {
		if p != nil {
			out = append(out, p.SourcePath, p.DestPath)
		}
	}
	if r.CodexPath != "" {
		out = append(out, r.CodexPath)
	}
	return out
}
