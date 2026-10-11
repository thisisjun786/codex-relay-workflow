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
)

// crw manage integrate: the plain-head integration queue (internal/integrator). One pass takes
// the integration lock, re-checks the gate of every pending head, merges the passing heads onto
// the remote integration branch, verifies the merged tree and pushes it fast-forward only.

const integrateUsage = "usage: crw manage integrate [--dry-run [--with-gate]] [--watch [--interval <duration>]]"

// integrateFlags is the parsed command line.
type integrateFlags struct {
	dryRun   bool
	withGate bool
	watch    bool
	interval time.Duration
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

var integrateCommand = Command{
	Name:    "integrate",
	Summary: "merge the gated heads of the integration queue onto dev, verify and push fast-forward only",
	Run:     integrateRun,
}

func init() { Register(integrateCommand) }

// integrateRunner is the runner the command builds; a test replaces it to drive the seams.
var integrateRunner = integrator.NewRunner

// integrateRun is crw manage integrate. The status of a pass is the integration run's own: 0
// pushed or nothing to do, 2 usage, configuration, fetch or lock, 3 verification failed, 4 dev
// moved or the push failed, 5 nothing merged. Watch ends 0 at the stop file.
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
	if e.Now != nil {
		runner.Now = e.Now
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
