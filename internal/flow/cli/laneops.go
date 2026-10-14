package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/buildinfo"
)

// The operations of the project lane as the shipped binary installs them (CRW-1277 h): cmd/crw/main.go assigns SchedulerLane to OpsFor,
// so `crw flow start project-lane` and `crw flow start night-batch` run the lane over the relay's scheduler (SchedRelay), the repository's
// CI (ops.CILocalVerifier), the decision tables of the store (ops.StoreDecisions) and the reviewer threads of the host (audit.BusReader);
// the report of a milestone goes up through F11's reporter (report.Reporter), which ProductionWorld builds over the run's store and journal.

// LaneRequest is what the operations of a run's project lane are built for: the world's request, the store the run's journal is in, the
// journal, the run record, the repository the worktrees are made from, the role table and the host as the milestone close reads it.
type LaneRequest struct {
	WorldRequest
	Store    *store.Store
	Journal  engine.Journal
	Identity journal.Identity
	Repo     string
	Policy   *engine.Policy
	Threads  ops.Threads
	// Reporter sends the milestone report up (F11, CRW-1204); nil records the call and answers that nothing is wired.
	Reporter ops.Reporter
	// Socket is the App Server socket the run's world resolved; the lane itself reads nothing from it.
	Socket string
}

// LaneWorld is what OpsFor installs in a run's World.
type LaneWorld struct {
	Ops           map[string]engine.Op
	Prover        engine.Prover
	InputSnapshot func(ctx context.Context, req engine.SnapshotRequest) (string, error)
	// Base is the commit a node's first worktree starts from (the tip of the plan's target branch); nil is the repository's HEAD.
	Base func(node string) (string, error)
	// Journal is the journal the engine appends to; nil is the run's own.
	Journal engine.Journal
}

// LaneOptions replace what the shipped lane reaches outside the store; the zero value is the shipped binary's.
type LaneOptions struct {
	// Verifier verifies a node's head and the final gate's commands; nil is ops.CILocalVerifier.
	Verifier ops.Verifier
	// IntegrationVerify verifies the merged tree of an integration batch; nil runs `crw-dev ci local` over it with the record path the
	// batch gives (CRW_VERIFY_HEAD, CRW_VERIFY_BASE, CRW_VERIFY_RECORD), through the verifier's tools of the configuration.
	IntegrationVerify dagsched.IntegrationBatchVerifier
	// SkillsDigest is the digest of the skills the run's agents load; "" is the installed crw plugin's (PluginSkillsDigest).
	SkillsDigest string
	// Now is the clock of the scheduler's recorded_at columns; nil is the relay's.
	Now func() string
	// Getenv reads the environment; nil is os.LookupEnv.
	Getenv func(string) (string, bool)
	// Relay is a test seam: the relay the lane is given in place of the run's SchedRelay (a wrapper around it); nil is the SchedRelay.
	Relay func(*SchedRelay) ops.Relay
	// Reporter replaces the request's reporter; nil keeps it.
	Reporter ops.Reporter
}

// SchedulerLane is the OpsFor of the shipped binary.
func SchedulerLane(ctx context.Context, req LaneRequest) (LaneWorld, error) {
	return BuildLane(ctx, req, LaneOptions{})
}

// BuildLane builds the operations of a run's project lane over the relay's scheduler (CRW-1277 h): ops.NewLane over the SchedRelay of the
// run, the verifier, the store's decision tables, the host's threads and the reporter of F11, with the actor the run's owner thread; it
// answers the operations, the engine's prover and the lane's input snapshot.
func BuildLane(ctx context.Context, req LaneRequest, opt LaneOptions) (LaneWorld, error) {
	spec := req.Spec
	if req.Store == nil || req.Journal == nil || req.Policy == nil {
		return LaneWorld{}, errors.New("the lane needs the run's store, journal and role table")
	}
	getenv := opt.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	src, err := spec.Load()
	if err != nil {
		return LaneWorld{}, err
	}
	if len(LaneOperations(src)) == 0 {
		// a workflow that calls no crw.* operation needs no lane: its run does not depend on a role table naming the executor and the reviewer
		return LaneWorld{}, nil
	}
	skills := opt.SkillsDigest
	if skills == "" {
		skills = PluginSkillsDigest(getenv)
	}
	exec, review := req.Policy.Roles["executor"].Pair, req.Policy.Roles["reviewer"].Pair
	if exec.Model == "" || exec.Effort == "" || review.Model == "" || review.Effort == "" {
		return LaneWorld{}, errors.New("the role table names no pair for the executor or the reviewer")
	}
	prompt := "script:" + src.Program.Digest
	for _, h := range src.Program.Helpers {
		prompt += ";" + h.Spec + "@" + h.Digest
	}
	var tools crwconfig.VerifierTools
	if opt.IntegrationVerify == nil || opt.Verifier == nil {
		if tools, err = laneVerifierTools(getenv); err != nil {
			return LaneWorld{}, err
		}
	}
	verify := opt.IntegrationVerify
	if verify == nil {
		verify = CILocalIntegration(tools, os.Stderr)
	}
	sched := &dagsched.Scheduler{Store: req.Store, Tips: mergeturn.TargetReader{}, Ancestry: dagsched.GitAncestry{}.Ancestry}
	if opt.Now != nil {
		sched.Now = opt.Now
	}
	journal := runJournal{req.Journal}
	ref, _ := HandoffOf(spec.Args)
	nonce := req.Identity.EpochNonce
	if nonce == "" {
		nonce = spec.RunID
	}
	relay := &SchedRelay{Sched: sched, RunID: spec.RunID, Actor: spec.Thread, Nonce: nonce, Revision: req.Identity.PlanRevision, Handoff: ref, State: req.State, Repo: req.Repo,
		CreatedAt: spec.CreatedAt, Journal: journal, Verify: verify,
		Rule:     dagsched.RuleVersion{SkillsDigest: skills, Model: exec.Model, Effort: exec.Effort, PromptTemplate: prompt, RelayBuild: buildinfo.ID()},
		Verifier: dagsched.VerifierRule{SkillsDigest: skills, Model: review.Model, Effort: review.Effort}}
	verifier := opt.Verifier
	if verifier == nil {
		verifier = ops.CILocalVerifier{Tools: tools}
		// the shipped verifier runs crw-dev ci local only: a milestone that requires another command is refused before the run starts
		// (CheckHandoff) and again when the run reads it
		relay.RunnableCommand = ShippedRunnableCommand
	}
	relay.HostMemory = func() (*dagsched.HostMemoryBound, error) {
		return dagsched.HostMemoryFromEnvironment(func(k string) string { v, _ := getenv(k); return v })
	}
	var seam ops.Relay = relay
	if opt.Relay != nil {
		seam = opt.Relay(relay)
	}
	reporter := req.Reporter
	if opt.Reporter != nil {
		reporter = opt.Reporter
	}
	lane := ops.NewLane(ops.Deps{RunID: spec.RunID, Repo: req.Repo, Journal: journal, Relay: seam, Verifier: verifier, Decisions: ops.StoreDecisions{DB: req.Store.DB},
		Threads: req.Threads, Reporter: reporter, Actor: spec.Thread, LandRoot: RunDir(req.State, spec.RunID)})
	out := LaneWorld{Ops: lane.Ops(), Prover: ops.NewProver(), InputSnapshot: lane.InputSnapshot, Journal: journal}
	if plan := planOf(spec.Args); plan != "" && ref.Path != "" {
		out.Base = func(string) (string, error) { return relay.IntegrationTip(ctx, plan) }
	}
	return out, nil
}

// ShippedRunnableCommand says why the shipped lane's verifier cannot run a required command of a milestone ("" when it can): the
// verifier is ops.CILocalVerifier, which runs crw-dev ci local and refuses every other command. It is the one predicate of the shipped
// lane, applied by CheckHandoff before a run starts (crw flow start --handoff, the start of a night batch, crw manage batch decide
// --handoff) and by SchedRelay.Milestone when the run reads its milestone (CRW-1285). A verifier that runs other commands (make e2e)
// is the first real project's to decide (CRW-1209); until then such a milestone is an input error the start refuses.
func ShippedRunnableCommand(command string) string {
	if command != ops.CILocalCommand {
		return fmt.Sprintf("the shipped lane's verifier runs %q only", ops.CILocalCommand)
	}
	return ""
}

// laneVerifierTools is the verifier section of the run's configuration file (CRW-1319). A section the verifier cannot use is the
// PATH fallback, and the verifier's note says it was ignored (fail-open); only a file the configuration's reading refuses as a
// whole refuses the lane.
func laneVerifierTools(getenv func(string) (string, bool)) (crwconfig.VerifierTools, error) {
	tools, err := crwconfig.LoadVerifierTools(func(k string) string { v, _ := getenv(k); return v }, "")
	if err != nil {
		return crwconfig.VerifierTools{}, fmt.Errorf("the verifier's tools: %w", err)
	}
	return tools, nil
}

// CILocalIntegration is the verification of an integration batch's merged tree by `crw-dev ci local`: the command the relay documents for
// dag-integrate --verify (docs/relay/dag-scheduler.md), run through the same command verifier with the batch's head, base and record.
// The command is the one the verifier's tools name (crwconfig.VerifierTools.Command): with a Go root configured it is the merged tree's
// own crw-dev, so the merged tree verifies itself. A command that comes from PATH because nothing configured it writes its note to notes,
// and one that cannot start is a host error that names the setting.
func CILocalIntegration(tools crwconfig.VerifierTools, notes io.Writer) dagsched.IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		vars := map[string]string{}
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				vars[k] = v
			}
		}
		command, err := tools.Command(dir, os.Getenv("PATH"))
		if err != nil {
			return &dagsched.IntegrationVerifierHostError{Detail: err.Error()}
		}
		if command.Note != "" && notes != nil {
			fmt.Fprintf(notes, "[crw note] integration verification: %s\n", command.Note)
		}
		argv := append(append([]string{}, command.Argv...), "ci", "local", "--commit", vars["CRW_VERIFY_HEAD"], "--base", vars["CRW_VERIFY_BASE"], "--record", vars["CRW_VERIFY_RECORD"], "--runner", "local")
		err = dagsched.CommandVerifier(argv)(ctx, dir, append(append([]string{}, command.Env...), env...))
		var host *dagsched.IntegrationVerifierHostError
		if errors.As(err, &host) {
			return &dagsched.IntegrationVerifierHostError{Detail: host.Detail + "; " + command.Hint}
		}
		return err
	}
}

// runJournal is the run's journal with what the relay's agent acceptance reads of a call (dagsched/accept_agent.go) that the engine's rows
// do not carry by themselves:
//
//   - the execution a node's call belongs to: the prepared row of an agent call of a node names the execution of the node's latest
//     release (the crw.release row that released it), so every row of the call carries it (flow_calls.execution_id). The journal
//     itself carries a node's execution from call to call once one row named it, and a resumed journal reads it back;
//   - the criteria a reviewer saw: the completed row of a reviewer call names the criteria digest of its input (detail_json
//     criteria_digest), which the lane's input snapshot of the node ("...;criteria=<digest>", ops.Lane.InputSnapshot) recorded on the
//     call's prepared row.
//
// A row that names its own execution or criteria digest is stored as it is.
type runJournal struct{ engine.Journal }

var snapshotCriteria = regexp.MustCompile(`(?:^|;)criteria=([^;]*)`)

func (j runJournal) Append(e engine.Event) engine.Event {
	if e.Kind != engine.KindCall || e.Op != "agent" || e.CallID == "" {
		return j.Journal.Append(e)
	}
	add := map[string]any{}
	switch e.Event {
	case engine.EvPrepared:
		if node := str(e.Data, "node"); node != "" && str(e.Data, "execution_id") == "" {
			if execution := releasedExecution(j.Journal.Events(), node); execution != "" {
				add["execution_id"] = execution
			}
		}
	case engine.EvCompleted:
		if _, has := e.Data["criteria_digest"]; !has {
			if digest := reviewerCriteria(j.Journal.Events(), e.CallID); digest != "" {
				add["criteria_digest"] = digest
			}
		}
	}
	if len(add) > 0 {
		data := make(map[string]any, len(e.Data)+len(add))
		for k, v := range e.Data {
			data[k] = v
		}
		for k, v := range add {
			data[k] = v
		}
		e.Data = data
	}
	return j.Journal.Append(e)
}

// SetOnCommit forwards the commit notice (engine.CommitNotifier): the engine registers its upward hook's batch notice through the journal it
// is given (CRW-1204), so the wrapper must not hide it. A journal that cannot notify has nothing to do.
func (j runJournal) SetOnCommit(notify func()) {
	if n, ok := j.Journal.(engine.CommitNotifier); ok {
		n.SetOnCommit(notify)
	}
}

var _ engine.CommitNotifier = runJournal{}

// Err is the failure of the run's journal (engine.FailingJournal).
func (j runJournal) Err() error {
	if f, ok := j.Journal.(engine.FailingJournal); ok {
		return f.Err()
	}
	return nil
}

// Flush stores the rows the run's journal still buffers, so the relay reads every call the run made (a journal that buffers nothing has
// nothing to do).
func (j runJournal) Flush() error {
	if f, ok := j.Journal.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// releasedExecution is the execution of a node's latest release in the journal: the crw.release row that released it.
func releasedExecution(events []engine.Event, node string) string {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind == engine.KindCall && ev.Op == ops.OpRelease && ev.Event == engine.EvCompleted && str(ev.Data, "node") == node {
			if released, _ := ev.Data["released"].(bool); released && str(ev.Data, "execution_id") != "" {
				return str(ev.Data, "execution_id")
			}
		}
	}
	return ""
}

// reviewerCriteria is the criteria digest the input snapshot of a reviewer call's prepared row names.
func reviewerCriteria(events []engine.Event, call string) string {
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Kind != engine.KindCall || ev.CallID != call || ev.Event != engine.EvPrepared {
			continue
		}
		if role := str(ev.Data, "role"); role != "reviewer" && role != "reviewer-final" {
			return ""
		}
		if m := snapshotCriteria.FindStringSubmatch(str(ev.Data, "input_snapshot")); m != nil {
			return m[1]
		}
		return ""
	}
	return ""
}

// PluginSkillsDigest is the digest of the skills of the installed crw plugin: the package PLUGIN_ROOT names, else the one crw version
// the Codex home's plugin cache holds (<CODEX_HOME or ~/.codex>/plugins/cache/<marketplace>/crw/<version>). The digest is "sha256:" and
// the sha256 of the sorted lines "<path>\x00<sha256 of the bytes>" of the files under its skills directory. A host where no single plugin
// is found answers "unresolved:" and why: the release records it, it does not stop the run.
func PluginSkillsDigest(getenv func(string) (string, bool)) string {
	root, why := pluginRoot(getenv)
	if root == "" {
		return "unresolved:" + why
	}
	digest, err := treeDigest(filepath.Join(root, "skills"))
	if err != nil {
		return "unresolved:" + err.Error()
	}
	return "sha256:" + digest
}

func pluginRoot(getenv func(string) (string, bool)) (string, string) {
	if v, ok := getenv("PLUGIN_ROOT"); ok && v != "" {
		return v, ""
	}
	home, _ := getenv("CODEX_HOME")
	if home == "" {
		h, _ := getenv("HOME")
		if h == "" {
			return "", "no CODEX_HOME and no HOME"
		}
		home = filepath.Join(h, ".codex")
	}
	found, _ := filepath.Glob(filepath.Join(home, "plugins", "cache", "*", "crw", "*", ".codex-plugin", "plugin.json"))
	switch len(found) {
	case 0:
		return "", "no crw plugin is installed under " + home
	case 1:
		return filepath.Dir(filepath.Dir(found[0])), ""
	}
	return "", fmt.Sprintf("%d crw plugin versions are installed under %s", len(found), home)
}

// maxTreeFiles bounds the files a digested tree may hold (links included), so a link to a large tree is an unresolved digest and not a
// walk of the host.
const maxTreeFiles = 20000

// treeDigest is the digest of the regular files under a directory, the content of a link taken as the content of what it resolves to (a
// linked directory, a linked file, the directory itself being a link): a change in linked bytes changes the digest. A link that cannot be
// resolved, a link back into the tree that holds it and a tree larger than maxTreeFiles are errors, never an empty digest.
func treeDigest(dir string) (string, error) {
	var lines []string
	files := 0
	if err := collectTree(dir, "", map[string]bool{}, &files, &lines); err != nil {
		return "", err
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// collectTree adds the lines "<path>\x00<sha256>" of the regular files under dir to lines; stack holds the resolved directories being walked.
func collectTree(dir, rel string, stack map[string]bool, files *int, lines *[]string) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if stack[real] {
		return fmt.Errorf("%s is a link back into the tree that holds it", dir)
	}
	stack[real] = true
	defer delete(stack, real)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if *files++; *files > maxTreeFiles {
			return fmt.Errorf("more than %d files under %s", maxTreeFiles, dir)
		}
		path, name := filepath.Join(dir, e.Name()), filepath.ToSlash(filepath.Join(rel, e.Name()))
		info, err := os.Stat(path) // follows a link
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if err := collectTree(path, name, stack, files, lines); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			*lines = append(*lines, name+"\x00"+sha256Hex(raw))
		}
	}
	return nil
}

// runRecord is the run record an earlier process of a run stored (flow_runs): the plan it names, the revision it pinned, the owner and
// the epoch nonce.
func runRecord(ctx context.Context, st *store.Store, run string) (journal.Identity, error) {
	var id journal.Identity
	err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(plan_id, ''), COALESCE(plan_revision, 0), COALESCE(actor_task_id, ''), COALESCE(epoch_nonce, '') FROM flow_runs WHERE run_id = ?`, run).
		Scan(&id.PlanID, &id.PlanRevision, &id.ActorTaskID, &id.EpochNonce)
	if errors.Is(err, sql.ErrNoRows) {
		// no process of the run stored its start: the run pins nothing yet
		return journal.Identity{}, nil
	}
	if err != nil {
		return journal.Identity{}, fmt.Errorf("the run record of %s: %w", run, err)
	}
	return id, nil
}
