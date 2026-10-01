package cli

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"os/signal"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// DaemonFactory is wired once at executable composition, before command execution.
// It opens the adapter only after ownership has been acquired.
var DaemonFactory func(context.Context, dispatch.Services, *store.Store) (*daemon.Daemon, error)

var daemonCommand = dispatch.Command{Name: "daemon", OwnAdmission: true, Run: runDaemon}

// serviceCommands are service's subcommands: status reads, the rest manage the supervisor, and
// start, restart and run reach the host.
func serviceCommands() []dispatch.Command {
	var commands []dispatch.Command
	for _, sub := range []string{"status", "enable", "disable", "stop", "declare", "start", "restart", "run"} {
		commands = append(commands, dispatch.Command{Name: "service " + sub, Exempt: true, OwnAdmission: true,
			ReadOnly: sub == "status", ReportsMismatch: sub == "status", Run: runService})
	}
	return commands
}

func boundValue(args dispatch.Args, name string) (*float64, error) {
	if !args.Given(name) {
		return nil, nil
	}
	v := args.Float(name)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, &dispatch.UsageError{Detail: "--" + name + " must be a finite number of seconds", Code: 4}
	}
	return &v, nil
}
func declaredBound(args dispatch.Args) (*float64, *float64, error) {
	duration, err := boundValue(args, "deadline")
	if err != nil {
		return nil, nil, err
	}
	instant, err := boundValue(args, "deadline-monotonic")
	if err != nil {
		return nil, nil, err
	}
	if duration != nil && instant != nil {
		return nil, nil, &dispatch.UsageError{Detail: "--deadline and --deadline-monotonic are two different bounds; pass one", Code: 4}
	}
	if duration != nil && *duration < 0 {
		return nil, nil, &dispatch.UsageError{Detail: "--deadline cannot be negative", Code: 4}
	}
	if instant != nil && *instant < 0 {
		return nil, nil, &dispatch.UsageError{Detail: "--deadline-monotonic cannot be negative", Code: 4}
	}
	return duration, instant, nil
}
func integerOption(args dispatch.Args, name string) *int {
	if !args.Given(name) {
		return nil
	}
	i := int(args.Integer(name).Int64()) // the parser reads an int option within int64
	return &i
}
func serviceError(err error) error {
	var ownershipRefusal *store.RefusedError
	if errors.As(err, &ownershipRefusal) {
		return ownershipRefusal
	}
	var refusal *service.Refused
	if errors.As(err, &refusal) {
		return &dispatch.PayloadExit{Code: 2, Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: refusal.Reason}, {Key: "detail", Value: nullableText(refusal.Detail)}}}
	}
	if err != nil {
		return dispatch.Host(err.Error())
	}
	return nil
}
func requireDaemonHost(s dispatch.Services) error {
	if !s.AdapterRequested {
		return &dispatch.UsageError{Detail: "this command needs --socket to reach the host", Code: 4}
	}
	return nil
}

// ownershipPreflight is Python's check_start before a service or daemon command
// (store.StartPreflight), with the command's App Server socket.
func ownershipPreflight(ctx context.Context, services dispatch.Services) error {
	return store.StartPreflight(ctx, services.Selection.DBPath())
}

// applyLaunchPolicy is cli.py main's _apply_launch_policy for `service run`: before the handler
// asks for --socket or reads a bound, a supervisor started here takes the policy its service
// declares into this process's environment, or is refused. A run launched by `service start`
// carries its launch's settled decision (_launch_already_settled) and is not asked again.
func applyLaunchPolicy(s *service.Service) error {
	settled := pyvalue.Strip(os.Getenv(service.SettledEnv))
	if settled != "" && s.LaunchID != "" && settled == s.LaunchID {
		return nil
	}
	resolution := s.ResolveLaunchPolicy()
	if refusal := service.LaunchRefusal(resolution); refusal != nil {
		return &dispatch.PayloadExit{Payload: refusal, Code: contract.ExitRefused}
	}
	value, recorded, err := service.LaunchVariable(resolution)
	if err != nil {
		return dispatch.Host(err.Error())
	}
	if recorded {
		return os.Setenv(execution.EnvPolicy, value)
	}
	return nil
}

func runService(ctx context.Context, services dispatch.Services, args dispatch.Args) (out any, err error) {
	if args.Positionals[0] != "status" {
		if err = ownershipPreflight(ctx, services); err != nil {
			return nil, err
		}
	}
	s, err := service.New(ctx, services.Selection, services.SocketPath)
	if err != nil {
		return nil, err
	}
	if args.Positionals[0] == "run" {
		s.LaunchID, _ = args.String("launch-id")
		if err = applyLaunchPolicy(s); err != nil {
			return nil, err
		}
	}
	actor, _ := args.String("actor")
	if actor == "" {
		actor = "cli"
	}
	var payload contract.OrderedObject
	switch args.Positionals[0] {
	case "status":
		status := s.Status(ctx)
		// Reported, not refused: status is how the mismatch is diagnosed (decision 73).
		if mismatch := dispatch.Mismatch(services); mismatch != nil {
			status = append(status, contract.Field{Key: "socketMismatch", Value: mismatch})
		}
		return status, nil
	case "enable":
		payload, err = s.Enable(actor)
	case "disable":
		payload, err = s.Disable(actor, 10*time.Second)
	case "stop":
		payload, err = s.Stop(actor, 10*time.Second)
	case "declare":
		path, _ := args.String("execution-policy")
		payload, err = s.Declare(path, actor, args.Bool("forget-execution-policy"))
	default:
		if err = requireDaemonHost(services); err != nil {
			return nil, err
		}
		duration, instant, e := declaredBound(args)
		if e != nil {
			return nil, e
		}
		segment, e := boundValue(args, "segment-seconds")
		if e != nil {
			return nil, e
		}
		if segment != nil && *segment <= 0 {
			return nil, &dispatch.UsageError{Detail: "--segment-seconds must be greater than zero", Code: 4}
		}
		options := service.Options{Actor: actor, AllowIsolated: args.Bool("allow-isolated-scope"), Takeover: args.Bool("takeover-scope"), Deadline: duration, DeadlineMonotonic: instant, SegmentSeconds: segment, MaxSegments: integerOption(args, "max-segments")}
		switch args.Positionals[0] {
		case "start":
			payload, err = s.Start(ctx, options)
		case "restart":
			payload, err = s.Restart(ctx, options)
		case "run":
			s.Takeover = options.Takeover
			var db *store.Store
			defer func() {
				if db != nil {
					err = errors.Join(err, db.Close())
				}
			}()
			// cli.py _supervise recover(): the supervisor opens the store, under both service
			// locks, only for a run whose bound is not already spent, so a spent run creates no
			// store and its records keep the identity read at start (null for an absent store).
			payload, err = s.Supervise(ctx, options, func() (recoveryErr error) {
				var e error
				if db, e = store.Open(ctx, services.Selection.DBPath(), services.SocketPath); e != nil {
					return e
				}
				loc, e := db.Locate(ctx)
				if e != nil {
					return e
				}
				s.StoreID = loc.StoreID
				if e = s.PublishStoreIdentity(); e != nil {
					return e
				}
				d, e := DaemonFactory(ctx, services, db)
				if e != nil {
					return e
				}
				defer func() { recoveryErr = errors.Join(recoveryErr, d.Host.Close()) }()
				defer func() { recoveryErr = errors.Join(recoveryErr, db.Close()); db = nil }()
				if _, e = d.Reconciler.RecoverOnStart(ctx, d.Host, nil); e != nil {
					return e
				}
				e = db.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
					_, e := db.Q(tx).ExecContext(tx, "UPDATE deliveries SET state='held_uncertain',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE state='sending' AND lease_until IS NOT NULL AND lease_until<=?", d.Clock.ISO(), d.Clock.Now())
					return e
				})
				return e
			}, nil)
		}
	}
	if err != nil {
		return nil, serviceError(err)
	}
	if get(payload, "ok") != true {
		return nil, &dispatch.PayloadExit{Payload: payload, Code: 2}
	}
	return payload, nil
}

func runDaemon(ctx context.Context, services dispatch.Services, args dispatch.Args) (out any, err error) {
	if args.Given("supervised-lock-fd") {
		// Decision 42: a supervised worker can be interrupted twice for one request, by its
		// process group or a drain and again by its supervisor passing the interrupt on. The
		// repeat is absorbed rather than meeting SIGINT's restored default disposition, which
		// would kill the worker before its cleanup or before its exit status is reported, so
		// the channel stays registered until the process exits. Its hard stops stay SIGTERM,
		// SIGKILL and its supervisor's death (PR_SET_PDEATHSIG).
		signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	}
	if !services.AdapterRequested {
		// cli.py main runs check_start before the daemon's handler asks for its --socket, so a
		// socketless daemon is refused for any store the fence refuses first (a foreign,
		// draining or partial one that holds a mirror), in its words, and only then for the
		// missing socket; a store check_start passes (absent, legacy, a gate alone) gets the
		// usage error. Nothing is written either way.
		if err = store.CheckStartLikeFence(ctx, services.Selection.DBPath()); err != nil {
			return nil, err
		}
	}
	if err = requireDaemonHost(services); err != nil {
		return nil, err
	}
	if err = ownershipPreflight(ctx, services); err != nil {
		return nil, err
	}
	s, err := service.New(ctx, services.Selection, services.SocketPath)
	if err != nil {
		return nil, err
	}
	duration, instant, err := declaredBound(args)
	if err != nil {
		return nil, err
	}
	maxTicks := integerOption(args, "max-ticks")
	noTicks := maxTicks != nil && *maxTicks <= 0
	clock := delivery.CommandClock
	if clock == nil {
		clock = delivery.SystemClock{}
	}
	spent := func(detail string) error {
		if e := s.JournalNote("this run served nothing: " + detail); e != nil {
			return e
		}
		return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bound_already_spent"}, {Key: "detail", Value: detail}}, Code: 5}
	}
	var deadline, bound *float64
	if instant != nil {
		remaining := *instant - service.Monotonic()
		if remaining <= 0 && !noTicks {
			return nil, spent("the instant this run was given had passed by the time it reached its own clock, so it took no tick")
		}
		v := clock.Now() + remaining
		deadline = &v
		bound = instant
	} else if duration != nil {
		v := clock.Now() + *duration
		deadline = &v
		b := service.Monotonic() + *duration
		bound = &b
	}
	var db *store.Store
	defer func() {
		if db != nil {
			err = errors.Join(err, db.Close())
		}
	}()
	s.Prepare = func() error {
		var e error
		db, e = store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
		if e != nil {
			return e
		}
		loc, e := db.Locate(ctx)
		if e == nil {
			s.StoreID = loc.StoreID
		}
		return e
	}
	// Ownership supersedes the pre-fence initializer: Own calls Prepare only
	// after both permanent service locks are held, including inherited workers.
	var token *string
	if args.Given("supervised-token") {
		v, _ := args.String("supervised-token")
		token = &v
	}
	lockFD, scopeFD := integerOption(args, "supervised-lock-fd"), integerOption(args, "supervised-scope-fd")
	if err = s.Adopt(token, lockFD, scopeFD); err != nil {
		return nil, serviceError(err)
	}
	owned, err := s.Own(args.Bool("allow-isolated-scope"), false, lockFD, scopeFD)
	if err != nil {
		return nil, serviceError(err)
	}
	defer func() {
		// Connection, admission, scope, daemon: reverse acquisition order.
		if db != nil {
			err = errors.Join(err, db.Close())
			db = nil
		}
		err = errors.Join(err, owned.Close())
	}()
	d, err := DaemonFactory(ctx, services, db)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, d.Host.Close()) }()
	if e := s.PublishWorkerPolicy(registry.EnvironmentRolePolicy().Summary()); e != nil {
		if err = s.JournalNote("worker policy receipt unavailable: " + e.Error()); err != nil {
			return nil, err
		}
	}
	control, e := service.ListenControl(ctx, services.Selection.Path)
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, control.Close()) }()
	stop := func() bool {
		return ctx.Err() != nil || s.StopRequested() || s.Draining() || (bound != nil && service.Monotonic() >= *bound)
	}
	reports, err := daemon.Run(ctx, d.Tick, clock, d.Policy.PollInterval, maxTicks, deadline, stop, func(seconds float64) error { return daemon.SchedulerWait(ctx, clock, deadline, seconds) })
	if err != nil {
		return nil, dispatch.Host(err.Error())
	}
	if len(reports) == 0 && bound != nil && service.Monotonic() >= *bound && !noTicks {
		return nil, spent("the bound was spent while this run was taking its locks and building its adapter, so it began with nothing left and took no tick")
	}
	ticks := []any{}
	for _, r := range reports {
		ticks = append(ticks, r.Object())
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "reason", Value: nil}, {Key: "pid", Value: get(owned.Record, "pid")}, {Key: "ticks", Value: ticks}}, nil
}
