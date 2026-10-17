package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/transport"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

const HostUsage = `crw host add --file HOST.json
crw host status [--host ID]
crw host check|heartbeat --host ID [--watch] [--worker-config FILE]
crw host enable --host ID --sessions N --test-slots N --cpus N
crw host drain --host ID
crw host toolchain plan --host ID --file MANIFEST.json
crw host toolchain apply --plan DIGEST [--worker-config FILE]
Every command accepts --state DIR --socket PATH. New hosts are disabled.`

func hostDocument(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return transport.Decode(f, v)
}
func hostAnswer(out, errout io.Writer, v any, err error) int {
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if err = json.NewEncoder(out).Encode(v); err != nil {
		fmt.Fprintln(errout, err)
		return 2
	}
	return 0
}
func hostTransport(h crwconfig.ExecutionHost, config, inc string) transport.Process {
	exe, _ := os.Executable()
	return transport.Process{Host: h, LocalBinary: exe, Config: config, ExpectedIncarnation: inc}
}
func HostRun(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(out, HostUsage)
		return 0
	}
	action := args[0]
	args = args[1:]
	if action == "toolchain" {
		if len(args) == 0 {
			fmt.Fprintln(errout, HostUsage)
			return 2
		}
		action += "-" + args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("host "+action, flag.ContinueOnError)
	fs.SetOutput(errout)
	state := fs.String("state", "", "controller state directory")
	socket := fs.String("socket", "", "controller socket identity")
	id := fs.String("host", "", "enrolled host id")
	file := fs.String("file", "", "approved JSON document")
	plan := fs.String("plan", "", "approved toolchain plan digest")
	config := fs.String("worker-config", "", "local worker configuration")
	sessions := fs.Int("sessions", 0, "explicit session limit")
	slots := fs.Int("test-slots", 0, "explicit heavy slot limit")
	cpus := fs.Int("cpus", 0, "explicit shared CPU budget")
	watch := fs.Bool("watch", false, "sample every 30 seconds until cancelled")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	st, _, err := OpenWriter(ctx, *state, *socket)
	if err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	defer st.Close()
	pool := &capacity.HostPool{Store: st}
	switch action {
	case "add":
		var h crwconfig.ExecutionHost
		if err = hostDocument(*file, &h); err == nil {
			err = pool.Add(ctx, h)
		}
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		s, err := pool.Status(ctx, h.ID)
		return hostAnswer(out, errout, s, err)
	case "status":
		if *id == "" {
			s, err := pool.List(ctx)
			return hostAnswer(out, errout, s, err)
		}
		s, err := pool.Status(ctx, *id)
		return hostAnswer(out, errout, s, err)
	case "drain":
		err = pool.Drain(ctx, *id)
	case "enable":
		err = pool.Enable(ctx, *id, crwconfig.HostCapacity{Sessions: *sessions, TestSlots: *slots, TestCPU: *cpus})
	case "check", "heartbeat":
		for {
			status, e := pool.Status(ctx, *id)
			if e != nil {
				return hostAnswer(out, errout, nil, e)
			}
			answer, e := hostTransport(status.Host, *config, "").Call(ctx, transport.Request{Method: "check"})
			if e == nil {
				if answer.Observation.HostID != status.Host.ID || answer.Observation.Incarnation != answer.Peer.Incarnation {
					e = errors.New("host observation identity mismatch")
				} else if action == "check" {
					e = pool.Check(ctx, answer.Observation)
				} else {
					e = pool.Heartbeat(ctx, answer.Observation)
				}
			}
			if e != nil {
				return hostAnswer(out, errout, nil, e)
			}
			s, e := pool.Status(ctx, *id)
			if code := hostAnswer(out, errout, s, e); code != 0 || !*watch {
				return code
			}
			timer := time.NewTimer(capacity.HostHeartbeatInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return 0
			case <-timer.C:
			}
		}
	case "toolchain-plan":
		var m worker.Toolchain
		if err = hostDocument(*file, &m); err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		p, err := pool.PlanTools(ctx, *id, m)
		return hostAnswer(out, errout, p, err)
	case "toolchain-apply":
		p, err := pool.ApplyTools(ctx, *plan, func(ctx context.Context, p capacity.ToolPlan) (worker.ToolchainReadback, error) {
			s, err := pool.Status(ctx, p.HostID)
			if err != nil {
				return worker.ToolchainReadback{}, err
			}
			r, err := hostTransport(s.Host, *config, "").Call(ctx, transport.Request{Method: "toolchain-apply", Toolchain: p.Manifest})
			return r.Toolchain, err
		})
		return hostAnswer(out, errout, p, err)
	default:
		fmt.Fprintln(errout, HostUsage)
		return 2
	}
	if err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	s, err := pool.Status(ctx, *id)
	return hostAnswer(out, errout, s, err)
}

type ExecutorSubmission struct {
	Plan      string                     `json:"plan"`
	Node      string                     `json:"node"`
	Actor     string                     `json:"actor"`
	Epoch     int64                      `json:"epoch"`
	Request   json.RawMessage            `json:"release_request"`
	Placement dagsched.ExecutorPlacement `json:"placement"`
}

const ExecutorUsage = `crw executor result check --execution ID --actor TASK --epoch N --spool LOCAL_DIR [--verification FILE] [--premerge FILE]
crw executor submit --file SUBMISSION.json [--worker-config FILE]
crw executor status|start|cancel|collect --execution ID --actor TASK --epoch N
crw executor exec --execution ID --actor TASK --epoch N --check CHECK_ID
Every command accepts --state DIR --socket PATH. status reconciles the worker;
collect reports a delivery path, not candidate acceptance.`

func ExecutorRun(ctx context.Context, args []string, out, errout io.Writer) int {
	if len(args) > 0 && args[0] == "result" {
		if len(args) < 2 || args[1] != "check" {
			fmt.Fprintln(errout, ExecutorResultCheckUsage)
			return 2
		}
		return executorResultCheck(ctx, args[2:], out, errout)
	}
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(out, ExecutorUsage)
		return 0
	}
	action := args[0]
	fs := flag.NewFlagSet("executor "+action, flag.ContinueOnError)
	fs.SetOutput(errout)
	state := fs.String("state", "", "controller state")
	socket := fs.String("socket", "", "controller socket")
	file := fs.String("file", "", "submission document")
	id := fs.String("execution", "", "execution identity")
	actor := fs.String("actor", "", "plan owner")
	epoch := fs.Int64("epoch", 0, "current coordinator epoch")
	config := fs.String("worker-config", "", "local worker configuration")
	check := fs.String("check", "", "frozen check id")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return 2
	}
	st, _, err := OpenWriter(ctx, *state, *socket)
	if err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	defer st.Close()
	pool := &capacity.HostPool{Store: st}
	sched := &dagsched.Scheduler{Store: st, ExpectedEpoch: *epoch, Tips: mergeturn.TargetReader{}}
	var a capacity.Assignment
	if action == "submit" {
		var in ExecutorSubmission
		if err = hostDocument(*file, &in); err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		req, e := dagsched.DecodeAgentReleaseRequest(in.Request)
		if e != nil {
			return hostAnswer(out, errout, nil, e)
		}
		sched.ExpectedEpoch = in.Epoch
		*actor = in.Actor
		a, err = sched.ReleaseExecutor(ctx, in.Plan, in.Node, in.Actor, req, dagsched.AgentReleaseOptions{}, in.Placement)
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		*id = a.Admission.Task.Identity.ExecutionID
		action = "start"
	} else {
		a, err = sched.ExecutorAssignment(ctx, *id)
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
	}
	h, err := pool.Status(ctx, a.Admission.Task.Executor.HostID)
	if err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	inc := a.Admission.Task.Executor.Incarnation
	if action != "start" && h.Observation.Incarnation != "" {
		inc = h.Observation.Incarnation
	}
	client := hostTransport(h.Host, *config, inc)
	port := executor.Port{Adapter: transport.Adapter{Caller: client, ExecutorKind: h.Host.Kind, Revision: h.Host.CapabilityRevision}}
	if action == "status" {
		action = "inspect"
	}
	if action == "exec" {
		// Fence by the same control path before acquiring any command resource.
		if a, err = sched.ExecutorAction(ctx, *id, *actor, "inspect", port); err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		var command *executor.Check
		for i := range a.Admission.Task.Checks {
			if a.Admission.Task.Checks[i].ID == *check {
				command = &a.Admission.Task.Checks[i]
			}
		}
		if command == nil {
			return hostAnswer(out, errout, nil, errors.New("check is not in the frozen task"))
		}
		supervisor := worker.Supervisor{Permits: hostPermits{pool, sched, *actor}, Runner: hostGroupRunner{client, a.Grant}}
		res, err := supervisor.Run(ctx, executor.ReferenceOf(a.Admission), *check, command.Args, command.Resources.CPU)
		return hostAnswer(out, errout, res, err)
	}
	a, err = sched.ExecutorAction(ctx, *id, *actor, action, port)
	if err == nil && action == "collect" {
		d, e := port.Collect(ctx, a.Admission)
		return hostAnswer(out, errout, d, e)
	}
	return hostAnswer(out, errout, a, err)
}

type hostPermits struct {
	pool  *capacity.HostPool
	sched *dagsched.Scheduler
	actor string
}

func (p hostPermits) Acquire(ctx context.Context, r executor.Reference, id string, cpu int) (worker.Permit, error) {
	v, err := p.sched.ExecutorPermit(ctx, r, p.actor, id, cpu)
	return worker.Permit{ID: v.ID, CPU: v.CPU}, err
}
func (p hostPermits) End(ctx context.Context, r executor.Reference, v worker.Permit, proof string) error {
	return p.pool.EndHeavy(ctx, r, v.ID, proof)
}

type hostGroupRunner struct {
	client transport.Caller
	grant  executor.Grant
}

func (r hostGroupRunner) Run(ctx context.Context, g worker.Group) (worker.CommandResult, error) {
	v, err := r.client.Call(ctx, transport.Request{Method: "exec", Group: g, Grant: r.grant})
	return v.Command, err
}
