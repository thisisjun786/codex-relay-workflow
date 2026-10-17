package cli

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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/transport"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// WorkerConfig is provisioned by the operator. Backend names a pinned adapter
// implementing crw-worker/1 on stdin/stdout, including worker-rpc tool supervision.
// An ordinary unconfined provider CLI is not such an adapter and cannot be enabled.
type WorkerConfig struct {
	Host        crwconfig.ExecutionHost `json:"host"`
	Incarnation string                  `json:"incarnation"`
	Backend     string                  `json:"backend"`
	Toolchain   worker.Toolchain        `json:"toolchain"`
	CgroupRoot  string                  `json:"cgroup_root"`
	CommandUID  uint32                  `json:"command_uid"`
	CommandGID  uint32                  `json:"command_gid"`
}
type pinnedWorkerBackend struct {
	path, digest string
	peer         executor.Peer
	roots        map[string]string
}

func (p pinnedWorkerBackend) Call(ctx context.Context, r transport.Request) (transport.Response, error) {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return transport.Response{}, err
	}
	if executor.Digest(raw) != p.digest {
		return transport.Response{}, errors.New("pinned executor adapter changed")
	}
	r.Schema = transport.Schema
	if r.Method == "start" {
		r.Workspace = worker.Workspace(p.roots[r.Admission.Task.Repository.ID], executor.ReferenceOf(r.Admission))
	}
	body, err := json.Marshal(r)
	if err != nil {
		return transport.Response{}, err
	}
	cmd := exec.CommandContext(ctx, p.path)
	cmd.Stdin = bytes.NewReader(body)
	// Backend stdout is a protocol response, never provider diagnostic output.
	var stdout boundedWorkerOutput
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		return transport.Response{}, fmt.Errorf("executor adapter: %w", err)
	}
	var result transport.Response
	if err = transport.Decode(bytes.NewReader(stdout.raw), &result); err != nil {
		return result, err
	}
	if result.Schema != transport.Schema || result.Peer != p.peer {
		return result, errors.New("adapter identity mismatch")
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	return result, nil
}

type boundedWorkerOutput struct{ raw []byte }

func (b *boundedWorkerOutput) Write(p []byte) (int, error) {
	if len(b.raw)+len(p) > transport.MaxFrame {
		return 0, errors.New("adapter response too large")
	}
	b.raw = append(b.raw, p...)
	return len(p), nil
}
func workerBackend(cfg WorkerConfig, peer executor.Peer) (pinnedWorkerBackend, error) {
	if !filepath.IsLocal(cfg.Backend) {
		return pinnedWorkerBackend{}, errors.New("adapter path must be relative to the owned toolchain version")
	}
	for _, f := range cfg.Toolchain.Files {
		if f.Path == cfg.Backend && f.Executable {
			return pinnedWorkerBackend{path: filepath.Join(cfg.Host.Root, "versions", cfg.Toolchain.Digest(), cfg.Backend), digest: f.SHA256, peer: peer, roots: cfg.Host.Repositories}, nil
		}
	}
	return pinnedWorkerBackend{}, errors.New("adapter is not a pinned executable in the toolchain")
}
func WorkerRun(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) int {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Fprintln(out, "crw worker serve [--config FILE] (one authenticated crw-worker/1 JSON frame on stdin)")
		return 0
	}
	if args[0] != "serve" && args[0] != "profile" {
		return 2
	}
	fs := flag.NewFlagSet("worker serve", flag.ContinueOnError)
	fs.SetOutput(errout)
	config := fs.String("config", os.Getenv("CRW_WORKER_CONFIG"), "approved worker configuration")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return 2
	}
	if *config == "" {
		root, err := os.UserConfigDir()
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		*config = filepath.Join(root, "crw", "worker.json")
	}
	var cfg WorkerConfig
	if err := hostDocument(*config, &cfg); err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	if err := cfg.Host.Validate(); err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	if args[0] == "profile" {
		return hostAnswer(out, errout, map[string]string{"launch_profile": cfg.LaunchDigest(), "toolchain_profile": cfg.Toolchain.Digest()}, nil)
	}
	if cfg.Incarnation == "" {
		return hostAnswer(out, errout, nil, errors.New("worker incarnation required"))
	}
	peer := executor.Peer{HostID: cfg.Host.ID, Incarnation: cfg.Incarnation}
	var request transport.Request
	if err := transport.Decode(in, &request); err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	var backend pinnedWorkerBackend
	if request.Method != "toolchain-apply" {
		active, err := worker.ActiveToolchain(cfg.Host.Root, cfg.Toolchain)
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
		cfg.Toolchain = active
		backend, err = workerBackend(cfg, peer)
		if err != nil {
			return hostAnswer(out, errout, nil, err)
		}
	}
	w, err := worker.Open(filepath.Join(cfg.Host.Root, "journal"), peer, transport.Adapter{Caller: backend, ExecutorKind: cfg.Host.Kind, Revision: cfg.Host.CapabilityRevision})
	if err != nil {
		return hostAnswer(out, errout, nil, err)
	}
	defer w.Close()
	response := transport.Response{Schema: transport.Schema, Peer: peer}
	if request.Schema != transport.Schema || request.Workspace != "" {
		err = errors.New("unsupported worker schema")
	} else {
		switch request.Method {
		case "check":
			var probed transport.Response
			probed, err = backend.Call(ctx, request)
			if err == nil {
				_, err = worker.ReadToolchain(cfg.Host.Root, cfg.Toolchain)
			}
			if err == nil {
				response.Observation, err = workerObservation(ctx, w, cfg, probed.Observation.Supervised)
			}
		case "start":
			if request.Admission.Task.Executor.LaunchProfileDigest != cfg.LaunchDigest() || request.Admission.Task.Executor.ToolchainProfileDigest != cfg.Toolchain.Digest() {
				err = errors.New("worker launch/toolchain profile drift")
				break
			}
			if cfg.Host.Repositories[request.Admission.Task.Repository.ID] == "" {
				err = errors.New("repository not enrolled on worker")
				break
			}
			if _, err = worker.ReadToolchain(cfg.Host.Root, cfg.Toolchain); err == nil {
				response.Handle, err = w.Start(ctx, request.Admission, request.Grant)
			}
		case "inspect":
			if err = w.CheckProfiles(ctx, request.Reference, cfg.LaunchDigest(), cfg.Toolchain.Digest()); err != nil {
				break
			}
			err = w.ReconcileExecution(ctx, request.Reference, nil, worker.CgroupRunner{Root: cfg.CgroupRoot, CommandUID: cfg.CommandUID, CommandGID: cfg.CommandGID}, false)
			if err != nil {
				break
			}
			response.Inspection, err = w.Inspect(ctx, request.Reference)
		case "cancel":
			if err = w.CheckProfiles(ctx, request.Reference, cfg.LaunchDigest(), cfg.Toolchain.Digest()); err != nil {
				break
			}
			err = w.ReconcileExecution(ctx, request.Reference, &request.Grant, worker.CgroupRunner{Root: cfg.CgroupRoot, CommandUID: cfg.CommandUID, CommandGID: cfg.CommandGID}, true)
			if err != nil {
				break
			}
			response.Cancellation, err = w.Cancel(ctx, request.Reference, request.Grant)
		case "collect":
			if err = w.CheckProfiles(ctx, request.Reference, cfg.LaunchDigest(), cfg.Toolchain.Digest()); err != nil {
				break
			}
			response.Delivery, err = w.Collect(ctx, request.Reference)
		case "renew":
			if err = w.CheckProfiles(ctx, request.Reference, cfg.LaunchDigest(), cfg.Toolchain.Digest()); err == nil {
				err = w.RenewGrant(ctx, request.Reference, request.Grant)
			}
		case "exec":
			if err = w.CheckProfiles(ctx, request.Group.Reference, cfg.LaunchDigest(), cfg.Toolchain.Digest()); err != nil {
				break
			}
			if err = w.RenewGrant(ctx, request.Group.Reference, request.Grant); err != nil {
				break
			}
			if err = w.AuthorizeGroup(ctx, request.Group); err == nil {
				var repo string
				repo, err = w.Repository(ctx, request.Group.Reference)
				if err == nil {
					dir := worker.Workspace(cfg.Host.Repositories[repo], request.Group.Reference)
					resolved, e := filepath.EvalSymlinks(dir)
					if e != nil || resolved != dir {
						err = errors.New("execution workspace is missing or contains a symlink")
						break
					}
					runner := worker.CgroupRunner{Root: cfg.CgroupRoot, Directory: dir, Env: os.Environ(), CommandUID: cfg.CommandUID, CommandGID: cfg.CommandGID}
					response.Command, err = w.RunGroup(ctx, request.Group, runner)
				}
			}
		case "toolchain-apply":
			next := cfg
			next.Toolchain = request.Toolchain
			if _, err = workerBackend(next, peer); err != nil {
				break
			}
			if err = w.Drained(ctx); err == nil {
				response.Toolchain, err = worker.InstallToolchain(cfg.Host.Root, request.Toolchain)
				if err == nil {
					err = worker.ActivateToolchain(cfg.Host.Root, request.Toolchain)
				}
			}
		default:
			err = errors.New("unsupported worker method")
		}
	}
	if err != nil {
		response.Error = err.Error()
	}
	return hostAnswer(out, errout, response, nil)
}
func workerObservation(ctx context.Context, w *worker.Worker, cfg WorkerConfig, supervised bool) (capacity.HostObservation, error) {
	if err := (worker.CgroupRunner{Root: cfg.CgroupRoot, CommandUID: cfg.CommandUID, CommandGID: cfg.CommandGID}).Available(); err != nil {
		return capacity.HostObservation{}, err
	}
	seq, err := w.Sequence(ctx)
	if err != nil {
		return capacity.HostObservation{}, err
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return capacity.HostObservation{}, err
	}
	mem := dagsched.ReadHostMemory("")
	o := capacity.HostObservation{HostID: cfg.Host.ID, Incarnation: cfg.Incarnation, BootID: strings.TrimSpace(string(boot)), Sequence: seq, EffectiveCPU: effectiveWorkerCPU(), ToolchainProfile: cfg.Toolchain.Digest(), LaunchProfile: cfg.LaunchDigest(), CapabilityRevision: cfg.Host.CapabilityRevision, Supervised: supervised}
	o.Measured = mem.AvailableBytes != nil && mem.SwapTotalBytes != nil && mem.SwapFreeBytes != nil && mem.PressureSomeAvg60 != nil && mem.PressureFullAvg10 != nil
	if o.Measured {
		o.AvailableBytes = *mem.AvailableBytes
		o.PressureSomeAvg60 = *mem.PressureSomeAvg60
		o.PressureFullAvg10 = *mem.PressureFullAvg10
		if *mem.SwapTotalBytes > 0 {
			o.SwapUsedPercent = float64(*mem.SwapTotalBytes-*mem.SwapFreeBytes) / float64(*mem.SwapTotalBytes) * 100
		}
	}
	return o, nil
}
func effectiveWorkerCPU() int {
	return effectiveWorkerCPUFrom(runtime.NumCPU(), os.ReadFile)
}

func effectiveWorkerCPUFrom(cpus int, readFile func(string) ([]byte, error)) int {
	raw, err := readFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "0::") {
			group := filepath.Clean(filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(line, "0::")))
			for group == "/sys/fs/cgroup" || strings.HasPrefix(group, "/sys/fs/cgroup/") {
				if b, e := readFile(filepath.Join(group, "cpu.max")); e == nil {
					f := strings.Fields(string(b))
					if len(f) == 2 && f[0] != "max" {
						q, e1 := strconv.ParseInt(f[0], 10, 64)
						p, e2 := strconv.ParseInt(f[1], 10, 64)
						if e1 != nil || e2 != nil || p <= 0 {
							return 0
						}
						cpus = min(cpus, int(q/p))
					}
				} else if group != "/sys/fs/cgroup" || !errors.Is(e, os.ErrNotExist) {
					return 0
				}
				if group == "/sys/fs/cgroup" {
					break
				}
				group = filepath.Dir(group)
			}
		}
	}
	return cpus
}

func (cfg WorkerConfig) LaunchDigest() string {
	body := map[string]any{"host_id": cfg.Host.ID, "kind": cfg.Host.Kind, "capability_revision": cfg.Host.CapabilityRevision, "root": cfg.Host.Root, "repositories": cfg.Host.Repositories, "auth_profile": cfg.Host.AuthProfile, "trust_profile": cfg.Host.TrustProfile, "supervision": cfg.Host.Supervision, "backend": cfg.Backend, "cgroup_root": cfg.CgroupRoot, "command_uid": cfg.CommandUID, "command_gid": cfg.CommandGID}
	raw, _ := json.Marshal(body)
	return executor.Digest(raw)
}
