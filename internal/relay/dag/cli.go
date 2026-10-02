package dag

import (
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay commands that define, store and read a plan (docs/relay/dag-plans.md). Every answer is
// the relay's JSON envelope; a refusal is exit 2 with its reason, a document that is not JSON is a
// usage error (exit 4), and a plan that disagrees with itself is the host's failure (exit 3).

func init() {
	dispatch.Register(nil,
		// dag-plan-put opens the store itself, after it has refused what it can refuse without writing
		// (Preflight): a rejected revision must not create the state directory's store.
		dispatch.Command{Name: "dag-plan-put", OwnAdmission: true, Run: runPut},
		dispatch.Command{Name: "dag-plan-show", ReadOnly: true, Run: runShow},
		dispatch.Command{Name: "dag-plan-log", ReadOnly: true, Run: runLog},
	)
}

// pageDefault is the page size of dag-plan-log when --limit is not given.
const pageDefault = 100

func readDocument(input string) ([]byte, error) {
	if !strings.HasPrefix(input, "@") {
		if err := store.EncodeUTF8(input); err != nil {
			return nil, &dispatch.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
		}
		return []byte(input), nil
	}
	file, err := os.Open(input[1:])
	if err != nil {
		return nil, &dispatch.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, &dispatch.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	return raw, nil
}

// hostFailure answers a plan that does not agree with itself as the host's failure.
func hostFailure(err error) error {
	var corrupt *CorruptError
	if errors.As(err, &corrupt) {
		return dispatch.Host(corrupt.Error())
	}
	return err
}

func runPut(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	raw, err := readDocument(args.Text("request"))
	if err != nil {
		return nil, err
	}
	rev, err := DecodeRevision(raw)
	if err != nil {
		var unreadable *UnreadableError
		if errors.As(err, &unreadable) {
			return nil, &dispatch.UsageError{Detail: unreadable.Error(), Code: contract.ExitUsage}
		}
		return nil, err
	}
	if err := Preflight(ctx, services.Selection.DBPath(), rev); err != nil {
		return nil, hostFailure(err)
	}
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	result, err := (&Repo{Store: s}).Put(ctx, rev)
	if err != nil {
		return nil, hostFailure(err)
	}
	return putAnswer(result), nil
}

func putAnswer(r Result) contract.OrderedObject {
	digests := make(contract.OrderedObject, 0, len(r.NodeDigests))
	ids := make([]string, 0, len(r.NodeDigests))
	for id := range r.NodeDigests {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		digests = append(digests, contract.Field{Key: id, Value: r.NodeDigests[id]})
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "replayed", Value: r.Replayed},
		{Key: "plan_id", Value: r.PlanID}, {Key: "project_key", Value: r.ProjectKey},
		{Key: "revision_no", Value: r.RevisionNo}, {Key: "parent_revision_no", Value: r.ParentRevisionNo},
		{Key: "request_id", Value: r.RequestID}, {Key: "request_digest", Value: r.RequestDigest},
		{Key: "state_digest", Value: r.StateDigest}, {Key: "coordinator_epoch", Value: r.CoordinatorEpoch},
		{Key: "author_task_id", Value: r.AuthorTaskID}, {Key: "recorded_at", Value: r.RecordedAt},
		{Key: "node_digests", Value: digests},
	}
}

func openRead(ctx context.Context, services dispatch.Services) (*Repo, func(), error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, nil, err
	}
	return &Repo{Store: s}, func() { _ = s.Close() }, nil
}

func runShow(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	repo, closeStore, err := openRead(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	planID := args.Text("plan")
	var rev int64
	if args.Given("revision") {
		rev = args.Integer("revision").Int64()
		if rev < 1 {
			return nil, notFound("revisions start at 1")
		}
	}
	snap, head, err := repo.Snapshot(ctx, planID, rev)
	if err != nil {
		return nil, hostFailure(err)
	}
	var logVerified any
	if args.Bool("verify") {
		if err := repo.VerifyLog(ctx, planID); err != nil {
			return nil, hostFailure(err)
		}
		logVerified = true
	}
	return snapshotAnswer(snap, head, logVerified), nil
}

func optional(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nodeAnswer(n SnapNode) contract.OrderedObject {
	o := contract.OrderedObject{
		{Key: "node_id", Value: n.NodeID}, {Key: "issue_key", Value: n.IssueKey}, {Key: "kind", Value: n.Kind},
		{Key: "title", Value: optional(n.Title)}, {Key: "criteria_set_digest", Value: n.CriteriaSetDigest},
		{Key: "slice_digest", Value: n.SliceDigest}, {Key: "supersedes_node_id", Value: optional(n.SupersedesNodeID)},
		{Key: "introduced_rev", Value: n.IntroducedRev},
	}
	if n.Lifecycle != "" {
		// only a node the plan paused, cancelled or archived carries the key: an active node reads as it always did
		o = append(o, contract.Field{Key: "lifecycle", Value: n.Lifecycle})
	}
	return o
}

func edgeAnswer(e SnapEdge) contract.OrderedObject {
	var authority any
	if len(e.RequiredAuthority) > 0 {
		authority = authorityList(e.RequiredAuthority)
	}
	return contract.OrderedObject{
		{Key: "edge_id", Value: e.EdgeID}, {Key: "from_node_id", Value: e.FromNodeID}, {Key: "to_node_id", Value: e.ToNodeID},
		{Key: "kind", Value: e.Kind}, {Key: "target_repository", Value: optional(e.TargetRepository)},
		{Key: "target_base_ref", Value: optional(e.TargetBaseRef)}, {Key: "pins_code_head", Value: e.PinsCodeHead},
		{Key: "decision_subject", Value: optional(e.DecisionSubject)}, {Key: "decision_digest", Value: optional(e.DecisionDigest)},
		{Key: "required_authority", Value: authority}, {Key: "introduced_rev", Value: e.IntroducedRev},
	}
}

// snapshotAnswer is the envelope of dag-plan-show. The plan's nodes are the top-level "nodes" list and its
// edges the top-level "edges" list: the one place a reader looks.
func snapshotAnswer(s Snapshot, head int64, logVerified any) contract.OrderedObject {
	nodes := make([]any, len(s.Nodes))
	for i, n := range s.Nodes {
		nodes[i] = nodeAnswer(n)
	}
	edges := make([]any, len(s.Edges))
	for i, e := range s.Edges {
		edges[i] = edgeAnswer(e)
	}
	o := contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: SchemaSnapshot},
		{Key: "plan_id", Value: s.PlanID}, {Key: "project_key", Value: s.ProjectKey},
		{Key: "revision_no", Value: s.Revision}, {Key: "head_revision_no", Value: head},
		{Key: "state_digest", Value: s.StateDigest}, {Key: "digests_verified", Value: true}, {Key: "log_verified", Value: logVerified},
	}
	if s.PlanState != "" {
		o = append(o, contract.Field{Key: "plan_state", Value: s.PlanState}) // present only while the plan is paused
	}
	return append(o, contract.Field{Key: "nodes", Value: nodes}, contract.Field{Key: "edges", Value: edges})
}

func runLog(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
	repo, closeStore, err := openRead(ctx, services)
	if err != nil {
		return nil, err
	}
	defer closeStore()
	limit := pageDefault
	if args.Given("limit") {
		n := args.Integer("limit").Int64()
		if n < 1 || n > MaxPage {
			return nil, &dispatch.UsageError{Detail: "--limit is between 1 and 1000", Code: contract.ExitUsage}
		}
		limit = int(n)
	}
	after := int64(0)
	if args.Given("after") {
		after = args.Integer("after").Int64()
		if after < 0 {
			return nil, &dispatch.UsageError{Detail: "--after is a revision number, 0 or more", Code: contract.ExitUsage}
		}
	}
	page, err := repo.Events(ctx, args.Text("plan"), after, limit)
	if err != nil {
		return nil, hostFailure(err)
	}
	events := make([]any, len(page.Events))
	for i, ev := range page.Events {
		changes := make([]any, len(ev.Changes))
		for j, c := range ev.Changes {
			changes[j] = changeObject(c)
		}
		events[i] = contract.OrderedObject{
			{Key: "revision_no", Value: ev.RevisionNo}, {Key: "parent_revision_no", Value: ev.ParentRevisionNo},
			{Key: "request_id", Value: ev.RequestID}, {Key: "request_digest", Value: ev.RequestDigest},
			{Key: "coordinator_epoch", Value: ev.CoordinatorEpoch}, {Key: "author_task_id", Value: ev.AuthorTaskID},
			{Key: "recorded_at", Value: ev.RecordedAt}, {Key: "state_digest", Value: ev.StateDigest}, {Key: "changes", Value: changes},
		}
	}
	return contract.OrderedObject{
		{Key: "ok", Value: true}, {Key: "schema", Value: "dag-plan-log/1"}, {Key: "plan_id", Value: page.PlanID},
		{Key: "after", Value: page.After}, {Key: "head_revision_no", Value: page.Head}, {Key: "cursor", Value: page.Cursor},
		{Key: "events", Value: events},
	}, nil
}
