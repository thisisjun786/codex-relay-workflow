package dagsched

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// The commands of the release policy and the measurements (CRW-411, docs/relay/dag-scheduler.md "Release policy" and "Measurements"). dag-release-policy-record and dag-landing-result-record decide
// something the scheduler reads, so they take the coordinator epoch like the other commands that decide; dag-measurements reads only.

func init() {
	dispatch.Register(nil,
		dispatch.Command{Name: "dag-release-policy-record", Run: runReleasePolicyRecord},
		dispatch.Command{Name: "dag-landing-result-record", Run: runLandingResultRecord},
		// dag-measurements never writes and never creates the store.
		dispatch.Command{Name: "dag-measurements", ReadOnly: true, Run: runMeasurements},
	)
}

// Schemas the commands print.
const (
	SchemaReleasePolicy = "dag-release-policy-record/1"
	SchemaLandingResult = "dag-landing-result-record/1"
)

func runReleasePolicyRecord(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	got, err := sched.RecordReleasePolicy(ctx, args.Text("plan"), args.Text("actor"), PolicyInput{
		Window: args.Integer("window"), HandlingSeconds: args.Integer("handling-seconds"), RedMerges: args.Integer("red-merges"), CleanRun: args.Integer("clean-run")})
	if err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaReleasePolicy}, {Key: "plan_id", Value: got.PlanID}, {Key: "policy_seq", Value: got.Settings.Seq},
		{Key: "replayed", Value: got.Replayed}, {Key: "window", Value: got.Settings.Window}, {Key: "handling_seconds", Value: got.Settings.HandlingSeconds},
		{Key: "red_merges", Value: got.Settings.RedMerges}, {Key: "clean_run", Value: got.Settings.CleanRun}}, nil
}

func runLandingResultRecord(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	got, err := sched.RecordLandingResult(ctx, args.Text("plan"), args.Text("node"), args.Text("actor"), ResultInput{Kind: args.Text("kind"), Commit: args.Text("commit"), Evidence: args.Text("evidence")})
	if err != nil {
		return nil, hostFailure(err)
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaLandingResult}, {Key: "plan_id", Value: got.PlanID}, {Key: "node_id", Value: got.NodeID},
		{Key: "result_id", Value: got.ResultID}, {Key: "kind", Value: got.Kind}, {Key: "commit", Value: optionalText(got.Commit)}, {Key: "evidence", Value: got.Evidence}, {Key: "replayed", Value: got.Replayed}}, nil
}

func runMeasurements(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	sched, closeStore, err := openScheduler(ctx, services, args)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	got, err := sched.Measurements(ctx, args.Text("plan"))
	if err != nil {
		return nil, hostFailure(err)
	}
	return got.Object(), nil
}
