package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
)

// crw manage integrate: the plain-head integration queue (internal/integrator). One pass takes
// the integration lock, re-checks the gate of every pending head, merges the passing heads onto
// the remote integration branch, verifies the merged tree and pushes it fast-forward only. A
// pending pull request of the configured pull_request_repo (--enqueue-pr) is landed alone: merged,
// verified, reviewed by the configured review command, and pushed only when the review did not ask
// for changes (CRW-1355).

const integrateUsage = "usage: crw manage integrate [--dry-run [--with-gate]] [--watch [--interval <duration>]] [--enqueue-pr OWNER/NAME#N]"

// integrateFlags is the parsed command line.
type integrateFlags struct {
	dryRun    bool
	withGate  bool
	watch     bool
	interval  time.Duration
	enqueuePR string
}

func integrateParse(args []string) (integrateFlags, string) {
	var f integrateFlags
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--dry-run":
			f.dryRun = true
		case a == "--with-gate":
			f.withGate = true
		case a == "--watch":
			f.watch = true
		case a == "--enqueue-pr" || strings.HasPrefix(a, "--enqueue-pr="):
			v := strings.TrimPrefix(a, "--enqueue-pr=")
			if a == "--enqueue-pr" {
				if i+1 >= len(args) {
					return f, "argument --enqueue-pr: expected one argument"
				}
				i++
				v = args[i]
			}
			if v == "" {
				return f, "argument --enqueue-pr: expected OWNER/NAME#N"
			}
			f.enqueuePR = v
		case a == "--interval" || strings.HasPrefix(a, "--interval="):
			v := strings.TrimPrefix(a, "--interval=")
			if a == "--interval" {
				if i+1 >= len(args) {
					return f, "argument --interval: expected one argument"
				}
				i++
				v = args[i]
			}
			d, err := time.ParseDuration(v)
			if err != nil || d < time.Second {
				return f, "argument --interval: expected a duration of at least one second"
			}
			f.interval = d
		default:
			return f, fmt.Sprintf("unexpected argument %q", a)
		}
	}
	switch {
	case f.enqueuePR != "" && (f.dryRun || f.watch):
		return f, "--enqueue-pr puts one pull request into the queue and cannot be combined with --dry-run or --watch"
	case f.dryRun && f.watch:
		return f, "--dry-run reads once and cannot be combined with --watch"
	case f.withGate && !f.dryRun:
		return f, "--with-gate belongs to --dry-run"
	case f.interval > 0 && !f.watch:
		return f, "--interval belongs to --watch"
	}
	return f, ""
}

// integrateSection is the integrate section of the configuration, resolved.
func integrateSection(cfg *Config) (integrator.Config, error) {
	var section integrator.Config
	if cfg != nil {
		if err := cfg.Section("integrate", &section); err != nil {
			return section, fmt.Errorf("the integrate section of the configuration: %w", err)
		}
	}
	return section.Resolved()
}

// integrateChildEnv is the environment the integration queue's commands start with: nil (the
// process's own) when the configuration names no tool root and no --config file, else that
// environment with the configuration file and the tool roots in place. It makes no directory, so
// integrate --dry-run changes nothing on disk; the runner prepares build_work (ToolRoots.Prepare)
// right before it starts the gate, record-version or verification command.
func integrateChildEnv(cfg *Config) []string {
	env, _ := coreEnvValues(nil, cfg)
	return env
}

var integrateCommand = Command{
	Name:    "integrate",
	Summary: "merge the gated heads of the integration queue onto dev, verify and push fast-forward only",
	Run:     integrateRun,
}

func init() { Register(integrateCommand) }

// integrateRunner is the runner the command builds; a test replaces it to drive the seams.
var integrateRunner = integrator.NewRunner

// integratePullRequests makes the tracking adapter the queue reads its pull requests through (the configured pull_request_repo);
// a test replaces it with one over a fake GitHub.
var integratePullRequests = func(repo string) (integrator.PullRequestSource, error) {
	return publication.NewGitHub(repo)
}

// integrateRun is crw manage integrate. The status of a pass is the integration run's own: 0
// pushed or nothing to do, 2 usage, configuration, fetch or lock, 3 verification failed, 4 dev
// moved or the push failed, 5 nothing merged, 6 the review of a pull request asked for changes.
// Watch ends 0 at the stop file. --enqueue-pr is 0 enqueued, 2 refused (nothing written).
func integrateRun(ctx context.Context, e *Env, args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprintln(e.Stdout, integrateUsage)
			return 0
		}
	}
	flags, problem := integrateParse(args)
	if problem != "" {
		fmt.Fprintln(e.Stderr, integrateUsage)
		fmt.Fprintf(e.Stderr, "crw manage integrate: error: %s\n", problem)
		return usageExit
	}
	cfg, err := integrateSection(coreDefaults(e))
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage integrate: error: %v\n", err)
		return usageExit
	}
	if flags.interval > 0 {
		cfg.IntervalSeconds = int(flags.interval / time.Second)
	}
	runner := integrateRunner(cfg)
	// The record-version and verification commands start the go tool: they get the configured tool
	// roots and the configuration file this command was started with (CRW-958). With nothing
	// configured the runner keeps the process environment.
	defaults := coreDefaults(e)
	runner.Env = integrateChildEnv(defaults)
	if defaults != nil {
		runner.Prepare = defaults.tools.Prepare
	}
	if e.Now != nil {
		runner.Now = e.Now
	}
	if cfg.PullRequestRepo != "" {
		prs, err := integratePullRequests(cfg.PullRequestRepo)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage integrate: error: pull_request_repo: %v\n", err)
			return usageExit
		}
		runner.PullRequests = prs
	}
	emit := func(v any) int {
		data, err := json.Marshal(v)
		if err == nil {
			_, err = fmt.Fprintf(e.Stdout, "%s\n", data)
		}
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage integrate: error: write output: %v\n", err)
			return 1
		}
		return 0
	}
	switch {
	case flags.enqueuePR != "":
		entry, err := runner.EnqueuePR(ctx, flags.enqueuePR)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage integrate: error: --enqueue-pr %s: %v\n", flags.enqueuePR, err)
			return usageExit
		}
		return emit(map[string]any{"enqueued": entry})
	case flags.dryRun:
		rep, err := runner.DryRun(ctx, flags.withGate)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage integrate: error: %v\n", err)
			return 1
		}
		return emit(rep)
	case flags.watch:
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		code := 0
		err := runner.Watch(ctx, func(rep integrator.Report) {
			if rc := emit(rep); rc != 0 {
				code = rc
			}
		})
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage integrate: stopped: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
		return code
	}
	rep := runner.Pass(ctx)
	if rc := emit(rep); rc != 0 {
		return rc
	}
	return rep.Exit
}
