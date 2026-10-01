package capacity

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay CLI commands todo 27 part A owns (cli.py:5215-5262 capacity, :5271-5387 edit
// regions). Their lines are parsed by their argparse specs, as every relay command's; their help
// is the usage line alone (dispatch.Command.UsageHelp).

// command registers one capacity or edit-region command; run reads the store it is handed.
func command(name string, defaults map[string]any, run func(context.Context, *store.Store, dispatch.Args) (any, error)) dispatch.Command {
	return dispatch.Command{Name: name, Defaults: defaults, UsageHelp: true, ReadOnly: name == "capacity-show" || name == "region-show",
		Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
			// cli.cmd_region_settle refuses before services.edit_regions is reached, so that
			// refusal leaves no store behind.
			if name == "region-settle" && args.Text("disposition") == "accepted" && args.Given("condition") {
				return nil, settleConditionRefusal()
			}
			s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
			if err != nil {
				return nil, err
			}
			defer s.Close()
			return run(ctx, s, args)
		}}
}

func init() {
	dispatch.Register(nil,
		command("slot-reserve", nil, cmdSlotReserve),
		command("slot-release", nil, cmdSlotRelease),
		command("limit-declare", nil, cmdLimitDeclare),
		command("usage-observe", nil, cmdUsageObserve),
		command("capacity-show", map[string]any{"scope-kind": "project"}, cmdCapacityShow),
		command("region-propose", map[string]any{"class": classSource}, cmdRegionPropose),
		command("region-settle", nil, cmdRegionSettle),
		command("region-restate-revision", nil, cmdRegionRestate),
		command("region-reaffirm", nil, cmdRegionReaffirm),
		command("region-followup", nil, cmdRegionFollowup),
		command("region-followup-accept", nil, cmdRegionFollowupAccept),
		command("region-followup-settle", nil, cmdRegionFollowupSettle),
		command("region-show", nil, cmdRegionShow))
}

// optional is the option's value, or None when the line did not give it.
func optional(args dispatch.Args, name string) sql.NullString {
	value, given := args.String(name)
	return sql.NullString{String: value, Valid: given}
}

func newRegions(s *store.Store) *EditRegions { return &EditRegions{Store: s, Now: registry.SystemISO} }

func cmdRegionPropose(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).Propose(ctx, Proposal{Repository: p.Text("repository"), BaseRevision: p.Text("revision"), Path: p.Text("path"),
		RegionKind: p.Text("kind"), RegionKey: p.Text("key"), RegionClass: p.Text("class"), RegenerateFrom: optional(p, "regenerate-from"),
		LeftProject: p.Text("left-project"), RightProject: p.Text("right-project"), PeerLinkID: p.Text("peer-link"),
		ProposerTaskID: p.Text("task"), ConstraintText: p.Text("constraint"), Condition: optional(p, "condition"),
		IssueKey: optional(p, "issue"), NextOwner: optional(p, "next-owner")})
}

// settleConditionRefusal is cli.cmd_region_settle's own refusal (cli.py:1433), printed whole
// before the service is reached.
func settleConditionRefusal() error {
	return &dispatch.PayloadExit{Payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"},
		{Key: "detail", Value: "an acceptance takes no condition and --condition would be dropped. State" +
			" your condition when you propose, restate it with region-reaffirm" +
			" --condition after a base move, or decline with the condition you would accept."}}, Code: contract.ExitRefused}
}

func cmdRegionSettle(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	if p.Text("disposition") == "accepted" && p.Given("condition") {
		return nil, settleConditionRefusal()
	}
	return newRegions(s).Settle(ctx, p.Text("agreement"), p.Text("actor"), p.Text("disposition"), optional(p, "condition"), optional(p, "reason"))
}

func cmdRegionRestate(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).RestateRevision(ctx, p.Text("repository"), p.Text("from-revision"), p.Text("to-revision"), p.Text("actor"))
}

func cmdRegionReaffirm(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).Reaffirm(ctx, p.Text("agreement"), p.Text("actor"), p.Text("revision"), optional(p, "condition"))
}

func cmdRegionFollowup(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).Followup(ctx, Followup{Agreement: p.Text("agreement"), Trigger: p.Text("trigger"), Acceptance: p.Text("acceptance"),
		RecordedBy: p.Text("recorded-by"), IssueRef: optional(p, "issue-ref"), AssigneeTask: optional(p, "assignee"),
		AssigneeProject: optional(p, "assignee-project")})
}

func cmdRegionFollowupAccept(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).AcceptFollowup(ctx, p.Text("followup"), p.Text("actor"), p.Text("assignee-project"))
}

func cmdRegionFollowupSettle(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newRegions(s).SettleFollowup(ctx, p.Text("followup"), p.Text("actor"), p.Text("disposition"), optional(p, "reason"))
}

func cmdRegionShow(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	answer, err := newRegions(s).Show(ctx, p.Text("repository"), ShowFilter{BaseRevision: optional(p, "revision"),
		Project: optional(p, "project"), Path: optional(p, "path")})
	if err != nil {
		return nil, err
	}
	return withEnforcement(s, answer), nil
}

func newCapacity(s *store.Store) *Capacity { return &Capacity{Store: s, Now: registry.SystemISO} }

func cmdSlotReserve(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newCapacity(s).Reserve(ctx, Reservation{SubjectKind: p.Text("kind"), SubjectKey: p.Text("subject"),
		ParentTask: p.Text("parent-task"), Project: p.Text("project"), ReservedBy: p.Text("actor"), Detail: optional(p, "detail")})
}

func cmdSlotRelease(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	var tenure sql.NullInt64
	if p.Given("tenure") {
		n, _ := strconv.ParseInt(strings.TrimSpace(p.Text("tenure")), 10, 64)
		tenure = sql.NullInt64{Int64: n, Valid: true}
	}
	return newCapacity(s).Release(ctx, Release{SubjectKind: p.Text("kind"), SubjectKey: p.Text("subject"),
		ReleasedBy: p.Text("actor"), Reason: p.Text("reason"), Tenure: tenure})
}

func cmdLimitDeclare(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newCapacity(s).DeclareLimit(ctx, Limit{ScopeKind: p.Text("scope-kind"), ScopeKey: p.Text("scope"),
		Dimension: p.Text("dimension"), Unit: p.Text("unit"), Ceiling: p.Float("ceiling"), DeclaredBy: p.Text("declared-by"),
		Source: p.Text("source"), Enforce: !p.Given("no-enforce")})
}

func cmdUsageObserve(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	return newCapacity(s).Observe(ctx, Observation{ScopeKind: p.Text("scope-kind"), ScopeKey: p.Text("scope"),
		Dimension: p.Text("dimension"), Observed: p.Float("observed"), ObservedBy: p.Text("observed-by"), Method: p.Text("method")})
}

func cmdCapacityShow(ctx context.Context, s *store.Store, p dispatch.Args) (any, error) {
	c := newCapacity(s)
	answer, err := c.Report(ctx, ReportFilter{Project: optional(p, "project"), ParentTask: optional(p, "parent-task"),
		Initiative: optional(p, "initiative")})
	if err != nil {
		return nil, err
	}
	if p.Text("scope") != "" {
		room, err := c.Headroom(ctx, p.Text("scope-kind"), p.Text("scope"))
		if err != nil {
			return nil, err
		}
		answer = append(answer, contract.Field{Key: "headroom", Value: room})
	}
	return withEnforcement(s, answer), nil
}

// withEnforcement is cli._with_enforcement: say when the store could not install a guard index.
func withEnforcement(s *store.Store, answer contract.OrderedObject) contract.OrderedObject {
	if len(s.UnenforcedIndexes) == 0 {
		return answer
	}
	unenforced := []any{}
	for _, index := range s.UnenforcedIndexes {
		unenforced = append(unenforced, contract.OrderedObject{{Key: "index", Value: index.Index}, {Key: "detail", Value: index.Detail}})
	}
	return append(answer, contract.Field{Key: "unenforcedIndexes", Value: unenforced})
}
