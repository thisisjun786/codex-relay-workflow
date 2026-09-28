package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/service"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// DaemonFactory is wired once at executable composition, before command execution.
// It opens the adapter only after ownership has been acquired.
var DaemonFactory func(context.Context, Services, *store.Store) (*daemon.Daemon, error)

var daemonCommand = Command{Name: "daemon", Flags: func(f *flag.FlagSet) {
	f.Int("max-ticks", 0, "")
	f.Float64("deadline", 0, "")
	f.Float64("deadline-monotonic", 0, "")
	f.Bool("allow-isolated-scope", false, "")
	f.String("supervised-token", "", "")
	f.Int("supervised-lock-fd", 0, "")
	f.Int("supervised-scope-fd", 0, "")
}, Run: runDaemon}
var serviceCommand = Command{Name: "service", Exempt: true, Flags: func(f *flag.FlagSet) {
	f.String("actor", "", "")
	f.String("execution-policy", "", "")
	f.Bool("forget-execution-policy", false, "")
	f.Bool("allow-isolated-scope", false, "")
	f.Float64("segment-seconds", 0, "")
	f.Int("max-segments", 0, "")
	f.Float64("deadline", 0, "")
	f.Float64("deadline-monotonic", 0, "")
	f.String("launch-id", "", "")
	f.Bool("takeover-scope", false, "")
}, Run: runService}

func boundValue(args Args, name string) (*float64, error) {
	if !args.Set[name] {
		return nil, nil
	}
	v, err := strconv.ParseFloat(args.Flags.Lookup(name).Value.String(), 64)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, &UsageError{"--" + name + " must be a finite number of seconds", 4}
	}
	return &v, nil
}
func declaredBound(args Args) (*float64, *float64, error) {
	duration, err := boundValue(args, "deadline")
	if err != nil {
		return nil, nil, err
	}
	instant, err := boundValue(args, "deadline-monotonic")
	if err != nil {
		return nil, nil, err
	}
	if duration != nil && instant != nil {
		return nil, nil, &UsageError{"--deadline and --deadline-monotonic are two different bounds; pass one", 4}
	}
	if duration != nil && *duration < 0 {
		return nil, nil, &UsageError{"--deadline cannot be negative", 4}
	}
	if instant != nil && *instant < 0 {
		return nil, nil, &UsageError{"--deadline-monotonic cannot be negative", 4}
	}
	return duration, instant, nil
}
func integerOption(args Args, name string) *int {
	if !args.Set[name] {
		return nil
	}
	n := args.Integer(name)
	i := int(n.Int64())
	if !n.IsInt64() {
		if n.Sign() > 0 {
			i = int(^uint(0) >> 1)
		} else {
			i = -int(^uint(0) >> 1)
		}
	}
	return &i
}
func serviceError(err error) error {
	var ownershipRefusal *store.RefusedError
	if errors.As(err, &ownershipRefusal) {
		return ownershipRefusal
	}
	var refusal *service.Refused
	if errors.As(err, &refusal) {
		return &PayloadExit{Code: 2, Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: refusal.Reason}, {Key: "detail", Value: nullableText(refusal.Detail)}}}
	}
	if err != nil {
		return &HostError{Class: "RuntimeError", Detail: err.Error()}
	}
	return nil
}
func requireDaemonHost(s Services) error {
	if !s.AdapterRequested {
		return &UsageError{Detail: "this command needs --socket to reach the host", Code: 4}
	}
	return nil
}
func runService(ctx context.Context, services Services, args Args) (out any, err error) {
	s, err := service.New(ctx, services.Selection, services.SocketPath)
	if err != nil {
		return nil, err
	}
	actor, _ := args.String("actor")
	if actor == "" {
		actor = "cli"
	}
	var payload contract.OrderedObject
	switch args.Positionals[0] {
	case "status":
		return s.Status(ctx), nil
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
			return nil, &UsageError{"--segment-seconds must be greater than zero", 4}
		}
		options := service.Options{Actor: actor, AllowIsolated: args.Bool("allow-isolated-scope"), Takeover: args.Bool("takeover-scope"), Deadline: duration, DeadlineMonotonic: instant, SegmentSeconds: segment, MaxSegments: integerOption(args, "max-segments")}
		switch args.Positionals[0] {
		case "start":
			payload, err = s.Start(ctx, options)
		case "restart":
			payload, err = s.Restart(ctx, options)
		case "run":
			s.LaunchID, _ = args.String("launch-id")
			s.Takeover = options.Takeover
			if s.LaunchID == "" || os.Getenv(service.SettledEnv) != s.LaunchID {
				resolution := s.ResolveLaunchPolicy()
				if refusal := service.LaunchRefusal(resolution); refusal != nil {
					payload = refusal
					break
				}
				if get(resolution, "source") == "record" {
					if err = os.Setenv(execution.EnvPolicy, get(resolution, "path").(string)); err != nil {
						return nil, err
					}
				}
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
			payload, err = s.Supervise(ctx, options, func() (recoveryErr error) {
				d, e := DaemonFactory(ctx, services, db)
				if e != nil {
					return e
				}
				defer func() { recoveryErr = errors.Join(recoveryErr, d.Host.Close()) }()
				defer func() { recoveryErr = errors.Join(recoveryErr, db.Close()); db = nil }()
				if _, e = d.Reconciler.RecoverOnStart(ctx, d.Host, nil); e != nil {
					return e
				}
				return db.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
					_, e := db.Q(tx).ExecContext(tx, "UPDATE deliveries SET state='held_uncertain',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE state='sending' AND lease_until IS NOT NULL AND lease_until<=?", d.Clock.ISO(), d.Clock.Now())
					return e
				})
			}, nil)
		}
	}
	if err != nil {
		return nil, serviceError(err)
	}
	if get(payload, "ok") != true {
		return nil, &PayloadExit{Payload: payload, Code: 2}
	}
	return payload, nil
}
func runDaemon(ctx context.Context, services Services, args Args) (out any, err error) {
	if err = requireDaemonHost(services); err != nil {
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
		return &PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bound_already_spent"}, {Key: "detail", Value: detail}}, Code: 5}
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
	if args.Set["supervised-token"] {
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
	if _, candidate := ctx.Value(activationKey{}).(*service.CandidateChannel); candidate {
		if _, e = d.Reconciler.RecoverOnStart(ctx, d.Host, nil); e != nil {
			return nil, e
		}
		if e = db.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
			_, e := db.Q(tx).ExecContext(tx, "UPDATE deliveries SET state='held_uncertain',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE state='sending' AND lease_until IS NOT NULL AND lease_until<=?", d.Clock.ISO(), d.Clock.Now())
			return e
		}); e != nil {
			return nil, e
		}
		if e = activateCandidate(ctx); e != nil {
			return nil, e
		}
	}
	stop := func() bool {
		return ctx.Err() != nil || s.StopRequested() || s.Draining() || (bound != nil && service.Monotonic() >= *bound)
	}
	reports, err := daemon.Run(ctx, d.Tick, clock, d.Policy.PollInterval, maxTicks, deadline, stop, func(seconds float64) error { return daemon.SchedulerWait(ctx, clock, deadline, seconds) })
	if err != nil {
		return nil, &HostError{Class: "ValueError", Detail: err.Error()}
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
