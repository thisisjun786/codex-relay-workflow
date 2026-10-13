package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BurntSushi/toml"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is what `crw flow start`, the run process and `crw flow next` know of a session run (CRW-1316 S3, CRW-1348): the
// backend a run is started on, the subagent slots of the owner's host, the orchestrator binding of the owner thread and the
// preflight facts a session run adds.

// The backends a run is started on (`crw flow start --backend`). session is the default: the run's calls are role subagents of the
// owner thread. app-server opens a thread per call; it stays for the tests and shadow runs.
const (
	BackendSession   = engine.BackendSession
	BackendAppServer = engine.BackendAppServer
)

// specBackend is the backend a spec names; a spec written before the field existed names the App Server backend.
func specBackend(spec Spec) string {
	if spec.Backend == "" {
		return BackendAppServer
	}
	return spec.Backend
}

// DefaultMaxThreads is the host's default of agents.max_threads: the subagents one session keeps open at once.
const DefaultMaxThreads = 6

// codexAgents is what the Codex configuration says about subagents: agents.max_threads (set says the file names it) and
// whether features.multi_agent_v2 is on. path is the file read; err a file that exists and cannot be read or parsed.
type codexAgents struct {
	MaxThreads int
	Set        bool
	V2         bool
	Path       string
	Err        error
}

// readCodexAgents reads <CODEX_HOME>/config.toml (CODEX_HOME when the environment names one, else ~/.codex); a file that is not
// there is the host's defaults.
func readCodexAgents(getenv func(string) string) codexAgents {
	out := codexAgents{MaxThreads: DefaultMaxThreads}
	home := getenv("CODEX_HOME")
	if home == "" {
		h := getenv("HOME")
		if h == "" {
			h, _ = os.UserHomeDir()
		}
		if h == "" {
			return out
		}
		home = filepath.Join(h, ".codex")
	}
	out.Path = filepath.Join(home, "config.toml")
	raw, err := os.ReadFile(out.Path)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		out.Err = err
		return out
	}
	var doc struct {
		Agents struct {
			MaxThreads *int64 `toml:"max_threads"`
		} `toml:"agents"`
		Features map[string]any `toml:"features"`
	}
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		out.Err = err
		return out
	}
	if doc.Agents.MaxThreads != nil {
		out.MaxThreads, out.Set = int(*doc.Agents.MaxThreads), true
	}
	switch v := doc.Features["multi_agent_v2"].(type) {
	case bool:
		out.V2 = v
	case map[string]any:
		out.V2, _ = v["enabled"].(bool)
	}
	return out
}

// sessionPool is the agent pool of a session run: the pool asked for (the default when none), at most agents.max_threads - 1 (the
// owner keeps one slot for a helper of its own), and at least 1.
func sessionPool(pool, maxThreads int) int {
	if pool <= 0 {
		pool = engine.DefaultPool
	}
	return max(1, min(pool, maxThreads-1))
}

// ownerPairs are the six role pairs of the run's table, as the orchestrator line carries them.
func ownerPairs(table *engine.Policy) map[string]bound.Pair {
	out := map[string]bound.Pair{}
	for _, role := range bound.OrchestratorRoles {
		if spec, ok := table.Roles[role]; ok {
			out[role] = bound.Pair{Model: spec.Pair.Model, Effort: spec.Pair.Effort}
		}
	}
	return out
}

// bindOwner writes the orchestrator line of a session run to its owner thread (CRW-1316 §9): a live run is one whose process holds
// its run lock, and the open line of a run without the lock is closed first.
func bindOwner(state, run, thread string, table *engine.Policy) (bound.BindResult, error) {
	return bound.BindOrchestrator(bound.OrchestratorBinding{RunID: run, Thread: thread, Digest: table.Digest, Pairs: ownerPairs(table),
		Live: func(other string) bool { return validRunID(other) && LockHeld(state, other) }})
}

// closeOwner closes the run's orchestrator line when it is the owner thread's last (the run ended).
func closeOwner(run, thread string) (bool, error) { return bound.CloseOrchestrator(nil, run, thread) }

// bindWarning is the warning a start, a resume or `crw flow next` prints for a binding that was not written ("" when it was, or
// was there already).
func bindWarning(res bound.BindResult, err error, thread, run string) string {
	switch {
	case err != nil:
		return fmt.Sprintf("the owner thread %s could not be bound to run %s (%v): the spawn hook gives its spawns no role pair, and a subagent spawned before a dispatched report cannot be found again after a restart", thread, run, err)
	case res.Holder != "":
		return fmt.Sprintf("the owner thread %s is bound to run %s, whose process holds its lock: run %s is not bound (one thread binds one live run), so the spawn hook applies run %s's pairs", thread, res.Holder, run, res.Holder)
	}
	return ""
}

// The preflight facts a session run adds (CRW-1316 §11). Each is pass or warn: a session run starts whatever they read (CRW-1318).
// The sixth fact of the design, verifier_prerequisites, is judged for every run (FactVerifier, CRW-1319) and is listed after them.
const (
	FactOwnerBound       = "owner_bound"
	FactRoleModels       = "role_models"
	FactOwnerWritable    = "owner_state_writable"
	FactSubagentSlots    = "subagent_slots"
	FactMultiAgentV1     = "multi_agent_v1"
	sessionProtocolStops = "a warning: the run starts, and the session protocol stops at the first step this blocks"
)

// SessionFacts are the session facts in the order the preview lists them.
var SessionFacts = []string{FactOwnerBound, FactRoleModels, FactOwnerWritable, FactSubagentSlots, FactMultiAgentV1}

// sessionFactRequest is what the session facts are judged on.
type sessionFactRequest struct {
	State, Socket, Repo, Thread string
	Table                       *engine.Policy
	Getenv                      func(string) string
}

// sessionFacts judges the five session facts.
func sessionFacts(req sessionFactRequest) []Fact {
	var out []Fact
	add := func(name, level, reason, reading string) {
		out = append(out, Fact{Name: name, Level: level, Reading: reading, Reason: reason}.leveled())
	}

	// owner_bound: the binding line the start writes
	switch line, present, err := bound.OrchestratorLine(nil, req.Thread); {
	case !bound.ValidThreadID(req.Thread):
		add(FactOwnerBound, LevelWarn, "a flow-bound line names a thread by a thread id: the spawn hook gives this owner's spawns no role pair, and a subagent spawned before its dispatched report is not found after a restart",
			fmt.Sprintf("the owner %q is not a thread id", req.Thread))
	case err != nil:
		add(FactOwnerBound, LevelWarn, "the start cannot write the binding the spawn hook decides by; "+sessionProtocolStops, fmt.Sprintf("the flow binding of thread %s cannot be read: %v", req.Thread, err))
	case present && line.Kind != bound.KindClosed && validRunID(line.RunID) && LockHeld(req.State, line.RunID):
		add(FactOwnerBound, LevelWarn, "one thread binds one live run: this run is not bound, and the hook applies the other run's pairs",
			fmt.Sprintf("thread %s is bound to run %s, whose process holds its lock", req.Thread, line.RunID))
	case present && line.Kind != bound.KindClosed:
		add(FactOwnerBound, LevelPass, "", fmt.Sprintf("thread %s holds the open line of run %s, which no process holds: the start closes it and binds this run", req.Thread, line.RunID))
	default:
		add(FactOwnerBound, LevelPass, "", fmt.Sprintf("thread %s is bound to no live run: the start binds it", req.Thread))
	}

	// role_models: the six pairs, a role without one, and a reviewer of an author's family
	var missing, pairs, overlap []string
	for _, role := range bound.OrchestratorRoles {
		spec, ok := req.Table.Roles[role]
		if !ok || spec.Pair.Model == "" || spec.Pair.Effort == "" {
			missing = append(missing, role)
			continue
		}
		pairs = append(pairs, role+" "+spec.Pair.Model+"/"+spec.Pair.Effort)
	}
	for _, reviewer := range []string{"reviewer", "reviewer-final"} {
		for _, author := range []string{"architect", "executor", "executor-t1"} {
			r, a := req.Table.Roles[reviewer].Pair.Model, req.Table.Roles[author].Pair.Model
			if r != "" && a != "" && execution.ModelFamily(r) == execution.ModelFamily(a) {
				overlap = append(overlap, reviewer+" and "+author+" are both "+execution.ModelFamily(r))
			}
		}
	}
	switch reading := strings.Join(pairs, ", "); {
	case len(missing) > 0:
		add(FactRoleModels, LevelWarn, "an undecided role (no pair) is spawned with the host's default and its dispatched report is not checked against a pair", "no pair for "+strings.Join(missing, ", ")+"; "+reading)
	case len(overlap) > 0:
		add(FactRoleModels, LevelWarn, "a review is independent only from another family than the author's: relay acceptance refuses it (independence_refused)", strings.Join(overlap, "; ")+"; "+reading)
	default:
		add(FactRoleModels, LevelPass, "", reading)
	}

	// owner_state_writable: the run's directory (steps, inbox), the flow-bound directory, the control socket, the repository
	var short []string
	if err := writableDir(filepath.Join(req.State, "flow")); err != nil {
		short = append(short, "the run directory: "+err.Error())
	}
	if dir, err := bound.Dir(); err != nil {
		short = append(short, "the flow-bound directory: "+err.Error())
	} else if exists, err := bound.CheckDir(dir); err != nil {
		short = append(short, "the flow-bound directory: "+err.Error())
	} else if exists {
		if err := writableDir(dir); err != nil {
			short = append(short, "the flow-bound directory: "+err.Error())
		}
	} else if err := writableDir(filepath.Dir(dir)); err != nil {
		short = append(short, "the flow-bound directory cannot be made: "+err.Error())
	}
	if req.Socket != "" {
		if info, err := os.Stat(req.Socket); err != nil {
			short = append(short, "the control socket: "+err.Error())
		} else if info.Mode()&os.ModeSocket == 0 {
			short = append(short, "the control socket "+req.Socket+" is not a socket")
		}
	}
	if req.Repo != "" {
		if err := writableDir(req.Repo); err != nil {
			short = append(short, "the repository: "+err.Error())
		}
	}
	if len(short) > 0 {
		add(FactOwnerWritable, LevelWarn, "the owner's commands write the run's inbox and the hook its ledger; "+sessionProtocolStops, strings.Join(short, "; "))
	} else {
		add(FactOwnerWritable, LevelPass, "", "the run directory, the flow-bound directory, the control socket and the repository can be written")
	}

	// subagent_slots and multi_agent_v1: the Codex configuration
	cfg := readCodexAgents(req.Getenv)
	source := "the host default"
	if cfg.Set {
		source = cfg.Path
	}
	switch {
	case cfg.Err != nil:
		add(FactSubagentSlots, LevelWarn, "the Codex configuration cannot be read; the run takes the host default", fmt.Sprintf("%s: %v; agents.max_threads %d assumed", cfg.Path, cfg.Err, cfg.MaxThreads))
	case cfg.MaxThreads < 2:
		add(FactSubagentSlots, LevelWarn, "the owner keeps one slot for a helper of its own, so no session call has a slot; "+sessionProtocolStops, fmt.Sprintf("agents.max_threads %d (%s)", cfg.MaxThreads, source))
	default:
		add(FactSubagentSlots, LevelPass, "", fmt.Sprintf("agents.max_threads %d (%s): up to %d session calls at once", cfg.MaxThreads, source, cfg.MaxThreads-1))
	}
	if cfg.V2 {
		add(FactMultiAgentV1, LevelWarn, "the steps name the V1 spawn_agent arguments and a V2 task is ciphertext: under multi_agent_v2 the protocol stops; "+sessionProtocolStops,
			"features.multi_agent_v2 is on in "+cfg.Path)
	} else {
		add(FactMultiAgentV1, LevelPass, "", "multi-agent V1 (features.multi_agent_v2 is off)")
	}

	return out
}

// writableDir says why a directory (or, when it is not there, the nearest directory above it) cannot be written to; nil when it can.
func writableDir(dir string) error {
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Stat(d)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", d)
			}
			if err := syscall.Access(d, 2 /* W_OK */); err != nil {
				return fmt.Errorf("%s cannot be written to: %w", d, err)
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(d) == d {
			return err
		}
	}
}

// spawnFormat is how the owner spawns a role's subagent: the spawn_agent arguments of a step of the role, with the message the
// step gives.
func spawnFormat(role string, table *engine.Policy) string {
	pair := table.Roles[role].Pair
	return fmt.Sprintf("spawn_agent(agent_type=%q, message=<the step's message>, model=%q, reasoning_effort=%q, fork_context=false)", bound.AgentTypeOf(role), pair.Model, pair.Effort)
}

// RoleSpawn is the spawn format of one role (start and next print them).
type RoleSpawn struct {
	Role            string `json:"role"`
	AgentType       string `json:"agent_type"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Spawn           string `json:"spawn"`
}

func roleSpawns(table *engine.Policy) []RoleSpawn {
	var out []RoleSpawn
	for _, role := range bound.OrchestratorRoles {
		pair := table.Roles[role].Pair
		out = append(out, RoleSpawn{Role: role, AgentType: bound.AgentTypeOf(role), Model: pair.Model, ReasoningEffort: pair.Effort, Spawn: spawnFormat(role, table)})
	}
	return out
}

// sessionLoop is the guidance the commands print for the owner's loop over a session run's steps (CRW-1316 §3.2), with the live
// tool argument names of 0.154/0.155 (send_input and close_agent take target, wait_agent targets and timeout_ms).
func sessionLoop(run string) string {
	return "take the steps of `crw flow next " + run + " --json`: a spawn step is spawn_agent(agent_type, message, model, reasoning_effort, fork_context=false) with the step's values, " +
		"then at once `crw flow dispatched " + run + " <call> --agent <the id spawn_agent returned> --k <k> --attempt <attempt>`; a send_input step is send_input(target=<agent_id>, message=<message>), " +
		"then `crw flow dispatched`; a close step is close_agent(target=<agent_id>). While subagents work, alternate wait_agent(targets=[<open agents>], timeout_ms=600000) with " +
		"`crw flow next " + run + " --wait 600 --json`. Never copy a subagent's answer: the run reads it from the host"
}
