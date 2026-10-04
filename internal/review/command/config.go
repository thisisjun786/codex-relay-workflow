package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

const (
	usageExit       = 2 // a command line this command cannot use
	defaultDailyCap = 20
	defaultLockWait = 30 * time.Minute
	usageLine       = "usage: crw review --base <sha> --head <sha> --issue CRW-N --out <dir> [options]"
)

var issueID = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]+$`)

// Config is every setting of one run: a flag, else a CRW_REVIEW_* variable, else the default. Paths are absolute once parsed.
type Config struct {
	Repo, Base, Head, Issue, Out string
	StateDir                     string        // holds ledger.jsonl and run.lock; default: the directory of the agy lock
	DailyCap                     int           // reviews that may start per UTC day; default 20
	Model, Binary, LockPath      string        // default agy.DefaultModel, "agy", agy.DefaultLockPath()
	LockWait                     time.Duration // for the run lock and for agy's own lock; default 30 minutes, negative tries once
	TimeLimitFloor               time.Duration // the agy call time limits; zero takes agy's defaults
	TimeLimitCeiling             time.Duration
	PostSummary                  bool   // keep the summary comment on pull request PR
	PR                           int    // the pull request, with PostSummary
	Gh                           string // the gh executable that talks to the forge; default "gh"
}

// parseConfig reads args; exit is the status to stop with, or -1 to carry on. A problem with a value of the environment is a usage error like one with a flag.
func parseConfig(args []string, stdout, stderr io.Writer) (c Config, exit int) {
	var bad []string
	env := func(name string, set func(string) error) {
		if v := os.Getenv(name); v != "" {
			if err := set(v); err != nil {
				bad = append(bad, name+": "+err.Error())
			}
		}
	}
	str := func(dst *string, name string) { env(name, func(v string) error { *dst = v; return nil }) }
	dur := func(dst *time.Duration, name string) {
		env(name, func(v string) (err error) { *dst, err = time.ParseDuration(v); return })
	}
	c = Config{Repo: ".", StateDir: filepath.Dir(agy.DefaultLockPath()), DailyCap: defaultDailyCap, Model: agy.DefaultModel, Binary: "agy", Gh: "gh", LockPath: agy.DefaultLockPath()}
	str(&c.StateDir, "CRW_REVIEW_STATE_DIR")
	str(&c.Model, "CRW_REVIEW_MODEL")
	str(&c.Binary, "CRW_REVIEW_AGY")
	str(&c.Gh, "CRW_REVIEW_GH")
	str(&c.LockPath, "CRW_REVIEW_LOCK")
	env("CRW_REVIEW_DAILY_CAP", func(v string) (err error) { c.DailyCap, err = strconv.Atoi(v); return })
	dur(&c.LockWait, "CRW_REVIEW_LOCK_WAIT")
	dur(&c.TimeLimitFloor, "CRW_REVIEW_TIME_LIMIT_FLOOR")
	dur(&c.TimeLimitCeiling, "CRW_REVIEW_TIME_LIMIT_CEILING")

	fs := flag.NewFlagSet("crw review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.Repo, "repo", c.Repo, "the repository to review")
	fs.StringVar(&c.Base, "base", "", "the base commit or ref (required)")
	fs.StringVar(&c.Head, "head", "", "the head commit or ref to review (required)")
	fs.StringVar(&c.Issue, "issue", "", "the issue id, recorded as data and never sent to the reviewer, like CRW-506 (required)")
	fs.StringVar(&c.Out, "out", "", "the directory that receives <head>.json and <head>.json.sha256 (required)")
	fs.StringVar(&c.StateDir, "state-dir", c.StateDir, "the directory of the run ledger (CRW_REVIEW_STATE_DIR)")
	fs.IntVar(&c.DailyCap, "daily-cap", c.DailyCap, "the reviews that may start per UTC day, at least 1 (CRW_REVIEW_DAILY_CAP)")
	fs.StringVar(&c.Model, "model", c.Model, "the agy model (CRW_REVIEW_MODEL)")
	fs.StringVar(&c.Binary, "agy", c.Binary, "the agy executable (CRW_REVIEW_AGY)")
	fs.BoolVar(&c.PostSummary, "post-summary", false, "keep the one summary comment of the pull request given by --pr: a reference opinion, never a review thread")
	fs.IntVar(&c.PR, "pr", 0, "the pull request of the repository of --repo that --post-summary comments on")
	fs.StringVar(&c.Gh, "gh", c.Gh, "the gh executable that talks to the forge (CRW_REVIEW_GH)")
	fs.StringVar(&c.LockPath, "lock", c.LockPath, "the host-wide agy lock file (CRW_REVIEW_LOCK)")
	fs.DurationVar(&c.LockWait, "lock-wait", c.LockWait, "how long to wait for the run lock and for agy's lock; 0 is 30m, negative tries once (CRW_REVIEW_LOCK_WAIT)")
	fs.DurationVar(&c.TimeLimitFloor, "time-limit-floor", c.TimeLimitFloor, "the agy call time limit for a small prompt; 0 is agy's default (CRW_REVIEW_TIME_LIMIT_FLOOR)")
	fs.DurationVar(&c.TimeLimitCeiling, "time-limit-ceiling", c.TimeLimitCeiling, "the most that limit grows to; 0 is agy's default (CRW_REVIEW_TIME_LIMIT_CEILING)")
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(stdout, usageLine)
		fs.VisitAll(func(f *flag.Flag) {
			fmt.Fprintf(stdout, "  --%s  %s", f.Name, f.Usage)
			if f.DefValue != "" && f.DefValue != "0s" && f.DefValue != "0" && f.DefValue != "false" {
				fmt.Fprintf(stdout, " (default %s)", f.DefValue)
			}
			fmt.Fprintln(stdout)
		})
		return c, 0
	case err != nil:
		return c, usageError(stderr, err.Error())
	}
	var missing []string
	for _, f := range []struct{ flag, value string }{{"--base", c.Base}, {"--head", c.Head}, {"--issue", c.Issue}, {"--out", c.Out}} {
		if f.value == "" {
			missing = append(missing, f.flag)
		}
	}
	switch {
	case len(missing) > 0:
		bad = append(bad, "the following arguments are required: "+strings.Join(missing, ", "))
	case fs.NArg() > 0:
		bad = append(bad, fmt.Sprintf("unrecognized argument %q", fs.Arg(0)))
	case !issueID.MatchString(c.Issue):
		bad = append(bad, fmt.Sprintf("--issue %q is not an issue id like CRW-506", c.Issue))
	}
	if c.DailyCap < 1 {
		bad = append(bad, fmt.Sprintf("the daily cap must be at least 1, got %d", c.DailyCap))
	}
	if c.PostSummary && c.PR < 1 {
		bad = append(bad, "--post-summary needs --pr <number>")
	}
	if !c.PostSummary && c.PR != 0 {
		bad = append(bad, "--pr is only used with --post-summary")
	}
	if len(bad) > 0 {
		return c, usageError(stderr, strings.Join(bad, "; "))
	}
	if c.LockWait == 0 {
		c.LockWait = defaultLockWait
	}
	for _, p := range []*string{&c.Repo, &c.Out, &c.StateDir, &c.LockPath} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return c, usageError(stderr, err.Error())
		}
		*p = abs
	}
	return c, -1
}

func usageError(stderr io.Writer, reason string) int {
	fmt.Fprintln(stderr, usageLine)
	fmt.Fprintln(stderr, "crw review: error: "+reason)
	return usageExit
}
