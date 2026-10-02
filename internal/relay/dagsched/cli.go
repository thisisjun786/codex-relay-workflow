package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay commands of the DAG scheduler (docs/relay/dag-scheduler.md). Every answer is the relay's JSON envelope; a refusal is exit 2 with an existing
// reason, a document that is not JSON is a usage error (exit 4) and a stored plan that disagrees with itself is the host's failure (exit 3).

func init() {
	dispatch.Register(nil,
		// dag-ready reads, and writes only when it is asked to keep the pass.
		dispatch.Command{Name: "dag-ready", ReadOnlyWhen: func(args dispatch.Args) bool { return !args.Bool("record") }, Run: runReady},
		dispatch.Command{Name: "dag-region-declare", Run: runRegionDeclare},
		// dag-release starts a managed task: like managed-start it opens its own admitted connection and needs the explicit --state and --socket.
		dispatch.Command{Name: "dag-release", OwnAdmission: true, Exempt: true, Run: runRelease},
		// dag-release-close ends an abandoned release (it starts nothing, so it needs no socket): the node can then be released again.
		dispatch.Command{Name: "dag-release-close", Run: runReleaseClose},
		dispatch.Command{Name: "dag-accept", Run: runAccept},
		dispatch.Command{Name: "dag-integration-observe", Run: runObserve},
		dispatch.Command{Name: "dag-decision-record", Run: runDecision},
		dispatch.Command{Name: "dag-correct", Run: runCorrect},
		dispatch.Command{Name: "dag-merge-judge", Run: runMergeJudge},
		dispatch.Command{Name: "dag-merge-request", Run: runMergeRequest},
		dispatch.Command{Name: "dag-conflict-observe", Run: runConflictObserve},
		dispatch.Command{Name: "dag-cap-basis-record", Run: runCapBasis},
	)
}

func usage(detail string) error {
	return &dispatch.UsageError{Detail: detail, Code: contract.ExitUsage}
}

// hostFailure answers a plan that does not agree with itself as the host's failure.
func hostFailure(err error) error {
	var corrupt *dag.CorruptError
	if errors.As(err, &corrupt) {
		return dispatch.Host(corrupt.Error())
	}
	return err
}

// openScheduler opens the selected store for a scheduler command. Selectors are the spellings a release fingerprints; commands that do not release leave them empty.
func openScheduler(ctx context.Context, services dispatch.Services) (*Scheduler, func(), error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, nil, err
	}
	return &Scheduler{Store: s}, func() { _ = s.Close() }, nil
}

func runReady(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	record := args.Bool("record")
	actor := args.Text("actor")
	if record && actor == "" {
		return nil, usage("--record needs --actor: a recorded pass names who asked")
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	if !record {
		reading, err := sched.Read(ctx, args.Text("plan"), ReadyOptions{})
		if err != nil {
			return nil, hostFailure(err)
		}
		return reading.Object(), nil
	}
	reading, seq, err := sched.RecordPass(ctx, args.Text("plan"), actor, ReadyOptions{})
	if err != nil {
		return nil, hostFailure(err)
	}
	return append(reading.Object(), contract.Field{Key: "pass_seq", Value: seq}), nil
}

// readDocument is a JSON option's value: the text itself, or @file.
func readDocument(input string) ([]byte, error) {
	if !strings.HasPrefix(input, "@") {
		if err := store.EncodeUTF8(input); err != nil {
			return nil, usage(err.Error())
		}
		return []byte(input), nil
	}
	file, err := os.Open(input[1:])
	if err != nil {
		return nil, usage(err.Error())
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, dag.MaxDocumentBytes+1))
	if err != nil {
		return nil, usage(err.Error())
	}
	return raw, nil
}

// wireRegion is one region of the --regions document.
type wireRegion struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Key        string `json:"key"`
	Change     string `json:"change"`
	Exclusive  bool   `json:"exclusive"`
}

func decodeRegions(raw []byte) ([]Region, error) {
	var wire []wireRegion
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, usage("--regions is a JSON list of {repository, path, kind, key, change, exclusive}: " + err.Error())
	}
	if decoder.More() {
		return nil, usage("--regions holds one JSON list")
	}
	out := make([]Region, len(wire))
	for i, w := range wire {
		out[i] = Region(w)
	}
	return out, nil
}

func runRegionDeclare(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	raw, err := readDocument(args.Text("regions"))
	if err != nil {
		return nil, err
	}
	regions, err := decodeRegions(raw)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	declared, err := sched.DeclareRegions(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), regions)
	if err != nil {
		return nil, hostFailure(err)
	}
	list := make([]any, len(declared.Regions))
	for i, r := range declared.Regions {
		list[i] = contract.OrderedObject{{Key: "repository", Value: r.Repository}, {Key: "path", Value: r.Path}, {Key: "kind", Value: r.Kind},
			{Key: "key", Value: optionalText(r.Key)}, {Key: "change", Value: r.Change}, {Key: "exclusive", Value: r.Exclusive}}
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "plan_id", Value: declared.PlanID}, {Key: "node_id", Value: declared.NodeID},
		{Key: "declaration_seq", Value: declared.Seq}, {Key: "replayed", Value: declared.Replayed}, {Key: "regions", Value: list}}, nil
}

func runRelease(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	if services.SocketPath == "" || services.Selection.Source != "flag" {
		return nil, usage("dag-release starts a managed task and requires explicit --state and --socket")
	}
	raw, err := readDocument(args.Text("request"))
	if err != nil {
		return nil, err
	}
	request, err := DecodeReleaseRequest(raw)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.Tips = mergeturn.TargetReader{}
	sched.PRs = ForgePullRequestReader(ExecRunner)
	sched.Start = ProductionStarter(services, args)
	sched.Selectors = Selectors{MarkerRoot: args.Text("marker-root"), Socket: services.SocketPath, StateSelector: services.Selection.Path}
	result, err := sched.Release(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), request)
	if err != nil {
		return nil, hostFailure(err)
	}
	if !result.Bound {
		// the managed start was refused or left incomplete: the intent and the slot stay, and the same call again continues it.
		return nil, &dispatch.PayloadExit{Payload: result.Object(), Code: contract.ExitRefused}
	}
	return result.Object(), nil
}

func runReleaseClose(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.CloseRelease(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), args.Text("manifest"), args.Text("reason"), args.Text("request-id"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return result.Object(), nil
}

func runAccept(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	raw, err := readDocument(args.Text("rule-version"))
	if err != nil {
		return nil, err
	}
	var rule VerifierRule
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rule); err != nil {
		return nil, usage("--rule-version is {skills_digest, model, effort}: " + err.Error())
	}
	input := AcceptInput{Event: args.Text("event"), Supersedes: args.Text("supersedes"), RuleVersion: rule}
	if args.Given("repository") || args.Given("pull-request") {
		if !args.Given("repository") || !args.Given("pull-request") {
			return nil, usage("--repository and --pull-request name a pull request together")
		}
		input.PullRequest = &PRRef{Repository: args.Text("repository"), Number: args.Integer("pull-request").Int64()}
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.PRs = ForgePullRequestReader(ExecRunner)
	result, err := sched.Accept(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), input)
	if err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-accept/1"}, {Key: "plan_id", Value: result.PlanID}, {Key: "node_id", Value: result.NodeID},
		{Key: "acceptance_id", Value: result.AcceptanceID}, {Key: "relationship_id", Value: optionalText(result.RelationshipID)}, {Key: "execution_generation", Value: result.Generation},
		{Key: "replayed", Value: result.Replayed}, {Key: "revalidated", Value: result.Revalidated}, {Key: "superseded_acceptance_id", Value: optionalText(result.SupersededID)},
		{Key: "head_sha", Value: optionalText(result.HeadSHA)}, {Key: "evidence_digest", Value: optionalText(result.EvidenceDigest)}, {Key: "slot_released", Value: result.SlotReleased}}, nil
}

func runObserve(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	var targets []Target
	for _, spelled := range args.Strings("target") {
		repository, ref, ok := strings.Cut(spelled, "@")
		if !ok || repository == "" || ref == "" {
			return nil, usage("--target is repository@ref, not " + spelled)
		}
		targets = append(targets, Target{Repository: repository, BaseRef: ref})
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.Tips = mergeturn.TargetReader{}
	sched.Ancestry = GitAncestry{}.Ancestry
	result, err := sched.ObserveIntegration(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), targets)
	if err != nil {
		return nil, hostFailure(err)
	}
	list := make([]any, len(result.Observations))
	for i, o := range result.Observations {
		list[i] = contract.OrderedObject{{Key: "observation_id", Value: o.ObservationID}, {Key: "repository", Value: o.Repository}, {Key: "base_ref", Value: o.BaseRef}, {Key: "subject_sha", Value: o.SubjectSHA},
			{Key: "tip_sha", Value: o.TipSHA}, {Key: "is_ancestor", Value: o.IsAncestor}, {Key: "method", Value: o.Method}, {Key: "merge_turn_id", Value: optionalText(o.MergeTurnID)},
			{Key: "observed_seq", Value: o.Seq}, {Key: "replayed", Value: o.Replayed}}
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-integration-observe/1"}, {Key: "plan_id", Value: result.PlanID}, {Key: "node_id", Value: result.NodeID},
		{Key: "acceptance_id", Value: result.AcceptanceID}, {Key: "observations", Value: list}, {Key: "integrated", Value: result.Integrated}, {Key: "mark_present", Value: result.MarkPresent},
		{Key: "slot_released", Value: result.SlotReleased}}, nil
}

func runDecision(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.RecordDecision(ctx, args.Text("plan"), args.Text("actor"), DecisionInput{Subject: args.Text("subject"), Digest: args.Text("digest"), Disposition: args.Text("disposition"),
		AuthorityKind: args.Text("authority-kind"), AuthorityRef: args.Text("authority-ref")})
	if err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-decision-record/1"}, {Key: "plan_id", Value: result.PlanID}, {Key: "decision_id", Value: result.DecisionID},
		{Key: "subject", Value: result.Subject}, {Key: "revision", Value: result.Revision}, {Key: "replayed", Value: result.Replayed}, {Key: "superseded_decision_id", Value: optionalText(result.SupersededID)}}, nil
}

func runCorrect(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	if args.Bool("prepare") {
		raw, err := readDocument(args.Text("manifest-request"))
		if err != nil {
			return nil, err
		}
		var wire struct {
			Base          *BaseRef    `json:"base"`
			RuleVersion   RuleVersion `json:"rule_version"`
			Volatile      []Volatile  `json:"volatile"`
			ArtifactRoots []string    `json:"artifact_roots"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&wire); err != nil {
			return nil, usage("--manifest-request is {base, rule_version, volatile, artifact_roots}: " + err.Error())
		}
		prepared, err := sched.PrepareCorrection(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), ManifestInput{Base: wire.Base, Volatile: wire.Volatile, RuleVersion: wire.RuleVersion},
			VerifyOptions{ArtifactRoots: wire.ArtifactRoots})
		if err != nil {
			return nil, hostFailure(err)
		}
		return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-correct/1"}, {Key: "manifest_digest", Value: prepared.ManifestDigest}, {Key: "instruction", Value: prepared.Instruction},
			{Key: "dispatch_request_id", Value: prepared.DispatchRequestID}}, nil
	}
	result, err := sched.RecordCorrection(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), args.Text("manifest-digest"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-correct/1"}, {Key: "plan_id", Value: result.PlanID}, {Key: "node_id", Value: result.NodeID},
		{Key: "relationship_id", Value: result.RelationshipID}, {Key: "execution_generation", Value: result.Generation}, {Key: "manifest_digest", Value: result.ManifestDigest},
		{Key: "replayed", Value: result.Replayed}, {Key: "carried_over", Value: result.CarriedOver}, {Key: "opened_by", Value: optionalText(result.OpenedBy)},
		{Key: "dispatch_request_id", Value: optionalText(result.DispatchRequestID)}, {Key: "dispatch_turn_id", Value: optionalText(result.DispatchTurnID)}}, nil
}

// namedPullRequest is the optional --repository/--pull-request pair of the merge commands.
func namedPullRequest(args dispatch.Args) (*PRRef, error) {
	if !args.Given("repository") && !args.Given("pull-request") {
		return nil, nil
	}
	if !args.Given("repository") || !args.Given("pull-request") {
		return nil, usage("--repository and --pull-request name a pull request together")
	}
	return &PRRef{Repository: args.Text("repository"), Number: args.Integer("pull-request").Int64()}, nil
}

func judgeObject(schema string, r JudgeResult) contract.OrderedObject {
	failed := make([]any, len(r.FailedRequired))
	for i, f := range r.FailedRequired {
		failed[i] = f
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: schema}, {Key: "plan_id", Value: r.PlanID}, {Key: "node_id", Value: r.NodeID}, {Key: "acceptance_id", Value: optionalText(r.AcceptanceID)},
		{Key: "outcome", Value: r.Outcome}, {Key: "reason", Value: r.Reason}, {Key: "eligible", Value: r.Eligible()}, {Key: "round", Value: int64(r.Round)}, {Key: "check_seq", Value: r.CheckSeq},
		{Key: "accepted_head", Value: optionalText(r.HeadSHA)}, {Key: "observed_head", Value: optionalText(r.ObservedHeadSHA)}, {Key: "base_tip", Value: optionalText(r.BaseTipSHA)},
		{Key: "failed_required", Value: failed}, {Key: "replayed", Value: r.Replayed}}
}

func runMergeJudge(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	named, err := namedPullRequest(args)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.Tips, sched.Ancestry, sched.PRs = mergeturn.TargetReader{}, GitAncestry{}.Ancestry, ForgePullRequestReader(ExecRunner)
	result, err := sched.Judge(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), JudgeInput{PullRequest: named})
	if err != nil {
		return nil, hostFailure(err)
	}
	return judgeObject("dag-merge-judge/1", result), nil
}

func runMergeRequest(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	named, err := namedPullRequest(args)
	if err != nil {
		return nil, err
	}
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	sched.Tips, sched.Ancestry, sched.PRs = mergeturn.TargetReader{}, GitAncestry{}.Ancestry, ForgePullRequestReader(ExecRunner)
	result, turn, err := sched.RequestMergeTurn(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), MergeRequestInput{PullRequest: named, Host: args.Text("host")})
	if err != nil {
		return nil, hostFailure(err)
	}
	return append(judgeObject("dag-merge-request/1", result), contract.Field{Key: "merge_turn", Value: turn}), nil
}

func runConflictObserve(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	result, err := sched.ObserveConflicts(ctx, args.Text("plan"), args.Text("actor"), ConflictInput{Repository: args.Text("repository"), LeftNode: args.Text("left-node"), RightNode: args.Text("right-node"),
		LeftHead: args.Text("left-head"), RightHead: args.Text("right-head")})
	if err != nil {
		return nil, hostFailure(err)
	}
	files := make([]any, len(result.Files))
	for i, f := range result.Files {
		files[i] = f
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-conflict-observe/1"}, {Key: "plan_id", Value: result.PlanID}, {Key: "observation_id", Value: result.ObservationID},
		{Key: "left_node_id", Value: result.LeftNode}, {Key: "right_node_id", Value: result.RightNode}, {Key: "left_head", Value: result.LeftHead}, {Key: "right_head", Value: result.RightHead},
		{Key: "base_sha", Value: result.BaseSHA}, {Key: "conflicts", Value: int64(result.Conflicts)}, {Key: "files", Value: files}, {Key: "method", Value: result.Method}, {Key: "replayed", Value: result.Replayed}}, nil
}

func runCapBasis(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	basis := CapBasis{LimitID: args.Text("limit"), Revision: args.Integer("revision").Int64(), WMinutes: args.Float("w-minutes"), WSource: args.Text("w-source"),
		SMinutes: args.Float("s-minutes"), SSource: args.Text("s-source"), DecidedBy: args.Text("actor")}
	if err := sched.RecordCapBasis(ctx, basis); err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: "dag-cap-basis-record/1"}, {Key: "limit_id", Value: basis.LimitID}, {Key: "limit_revision", Value: basis.Revision},
		{Key: "w_minutes", Value: basis.WMinutes}, {Key: "s_minutes", Value: basis.SMinutes}}, nil
}
