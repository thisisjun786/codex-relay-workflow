package integrate

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The commands of the integration target record (CRW-1304). dag-integration-target-record records the remote branch an integration ref
// is pushed to; dag-integration-target-check reads the remote branch of every recorded target once and records what it found. Neither
// pushes, reverts or blocks: see dagsched/integration_target_record.go and integration_target_check.go.

// SchemaTargetRecord and SchemaTargetCheck name the documents the two commands print.
const (
	SchemaTargetRecord = "dag-integration-target-record/1"
	SchemaTargetCheck  = "dag-integration-target-check/1"
)

func init() {
	dispatch.Register(nil,
		dispatch.Command{Name: "dag-integration-target-record", Run: runTargetRecord},
		dispatch.Command{Name: "dag-integration-target-check", Run: runTargetCheck},
	)
}

func runTargetRecord(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s}
	row, appended, err := sched.RecordTarget(ctx, dagsched.TargetInput{Checkout: args.Text("checkout"), IntegrationRef: args.Text("integration-ref"),
		Remote: args.Text("remote"), RemoteRef: args.Text("remote-ref"), Plan: args.Text("plan"), Actor: args.Text("actor"), Source: store.TargetSourceRecord})
	if err != nil {
		return nil, err
	}
	return targetRecordObject(row, appended), nil
}

// targetRecordObject is the document dag-integration-target-record prints: the integration ref's current target row, and whether this call
// appended a row (false for a repeat of the newest record).
func targetRecordObject(row store.IntegrationTargetRow, appended bool) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaTargetRecord},
		{Key: "checkout", Value: row.Checkout}, {Key: "integration_ref", Value: row.IntegrationRef},
		{Key: "remote", Value: row.Remote}, {Key: "remote_ref", Value: row.RemoteRef},
		{Key: "plan_id", Value: nullableText(row.PlanID)}, {Key: "recorded", Value: appended},
		{Key: "recorded_by", Value: row.RecordedBy}, {Key: "recorded_at", Value: row.RecordedAt},
	}
}

func runTargetCheck(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	sched := &dagsched.Scheduler{Store: s}
	readings, err := sched.CheckRecordedTargets(ctx, args.Text("checkout"), args.Text("integration-ref"))
	if err != nil {
		return nil, err
	}
	return targetCheckObject(readings), nil
}

// targetCheckObject is the document dag-integration-target-check prints: one entry per recorded target, whatever the reading found. ok is
// true whenever the command ran; a target that was deferred (unreachable, absent, unreadable) says so in its status and detail. moved is
// present only on a target whose remote head is outside the recorded heads, and observation_error only when the head could not be stored.
func targetCheckObject(readings []dagsched.TargetReading) contract.OrderedObject {
	targets := make([]any, len(readings))
	moved := 0
	for i, r := range readings {
		entry := contract.OrderedObject{
			{Key: "checkout", Value: r.Target.Checkout}, {Key: "integration_ref", Value: r.Target.IntegrationRef},
			{Key: "remote", Value: r.Target.Remote}, {Key: "remote_ref", Value: r.Target.RemoteRef},
			{Key: "status", Value: r.Status}, {Key: "remote_head", Value: nullableText(r.RemoteHead)}, {Key: "detail", Value: nullableText(r.Detail)},
			{Key: "observed", Value: r.Observed},
		}
		if m := r.Moved; m != nil {
			moved++
			expected := make([]any, len(m.Expected))
			for j, head := range m.Expected {
				expected[j] = head
			}
			entry = append(entry, pyjson.Field{Key: "moved", Value: contract.OrderedObject{
				{Key: "fault_class", Value: dagsched.ClassIntegrationTargetMoved}, {Key: "fault_id", Value: nullableText(m.FaultID)}, {Key: "fault_error", Value: nullableText(m.FaultError)},
				{Key: "expected_heads", Value: expected}, {Key: "expected_total", Value: m.ExpectedTotal},
			}})
		}
		if r.ObservationError != "" {
			entry = append(entry, pyjson.Field{Key: "observation_error", Value: r.ObservationError})
		}
		targets[i] = entry
	}
	return contract.OrderedObject{{Key: "ok", Value: true}, {Key: "schema", Value: SchemaTargetCheck}, {Key: "checked", Value: len(readings)}, {Key: "moved", Value: moved}, {Key: "targets", Value: targets}}
}

// batchTargetInput is the target the --remote and --remote-ref flags of dag-integrate name; nil when the line names none.
type batchTargetInput struct{ in dagsched.TargetInput }

// batchTarget reads the two flags before the batch runs: they are given together or not at all, and a name git would read as an option is
// refused before anything is merged. Neither flag given is no target, and the batch is the batch it was.
func batchTarget(args dispatch.Args) (*batchTargetInput, error) {
	remote, remoteRef := args.Text("remote"), args.Text("remote-ref")
	if remote == "" && remoteRef == "" {
		return nil, nil
	}
	if remote == "" || remoteRef == "" {
		return nil, &dispatch.UsageError{Detail: "--remote and --remote-ref name the integration target together: give both or neither", Code: contract.ExitUsage}
	}
	in := dagsched.TargetInput{Checkout: args.Text("checkout"), IntegrationRef: args.Text("integration-ref"), Remote: remote, RemoteRef: remoteRef,
		Actor: args.Text("actor"), Source: store.TargetSourceIntegrate}
	if err := dagsched.RefuseTargetInput(in); err != nil {
		return nil, err
	}
	return &batchTargetInput{in: in}, nil
}

// recordBatchTarget records the target after the batch ran and adds its answer to the batch's document as target. The batch is what it
// was whatever the record says: a record that cannot be written is named in target.error and the exit code is unchanged.
func recordBatchTarget(ctx context.Context, sched *dagsched.Scheduler, target *batchTargetInput, doc contract.OrderedObject) contract.OrderedObject {
	if target == nil {
		return doc
	}
	row, appended, err := sched.RecordTarget(ctx, target.in)
	entry := contract.OrderedObject{{Key: "remote", Value: target.in.Remote}, {Key: "remote_ref", Value: target.in.RemoteRef}, {Key: "recorded", Value: appended}, {Key: "error", Value: nil}}
	if err != nil {
		entry[3].Value = err.Error()
	} else {
		entry = append(entry, pyjson.Field{Key: "plan_id", Value: nullableText(row.PlanID)})
	}
	return append(doc, pyjson.Field{Key: "target", Value: entry})
}

// recordPushRead records the target and the head a push read, once the remote branch was read: a pushed or up-to-date push, or a refusal
// that came after the read (the remote head is set). It answers why the record could not be written. A push that was deferred, or refused
// before the remote was read, records nothing.
func recordPushRead(ctx context.Context, sched *dagsched.Scheduler, args dispatch.Args, result dagsched.PushResult, pushErr error) error {
	read := pushErr == nil && result.Outcome != dagsched.PushDeferred || pushErr != nil && result.RemoteHead != ""
	if !read {
		return nil
	}
	return sched.RecordPushRead(ctx, args.Text("checkout"), args.Text("remote"), args.Text("remote-ref"), args.Text("integration-ref"), result.RemoteHead)
}

// nameTargetRecordError adds why the target could not be recorded to the detail of a refusal the push answers, so the parent that reads
// only the refusal still sees it. The refusal is otherwise the push's own.
func nameTargetRecordError(err, recordErr error) error {
	var refused *store.RefusedError
	if recordErr == nil || !errors.As(err, &refused) {
		return err
	}
	refused.Detail += "; the integration target could not be recorded: " + recordErr.Error()
	return err
}
