package routing

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

type routeClock string

func (c routeClock) ISO() string { return string(c) }

func routeReplay(t *testing.T, property string) {
	t.Helper()
	var records []struct {
		Operation string
		Args      []any
		Kwargs    Object
		Stamp     string
	}
	scenarioInputs(t, "routes-"+property+".json", &records)
	ctx := context.Background()
	stateRoot := t.TempDir()
	var s *store.Store
	defer func() {
		if s != nil {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	for i, record := range records {
		name := fmt.Sprintf("%03d_%s", i, record.Operation)
		var got string
		if !t.Run(name, func(t *testing.T) {
			args, kw := record.Args, record.Kwargs
			r := RouteStore{Store: s, Clock: routeClock(record.Stamp)}
			var answer any
			var err error
			switch record.Operation {
			case "reset":
				if s != nil {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
				}
				s, err = store.Open(ctx, filepath.Join(stateRoot, fmt.Sprintf("case-%d", i), "relay.sqlite3"), "")
			case "upsert":
				err = r.Upsert(ctx, kw)
			case "store_incident":
				keep := store.MaxStoredIncidents
				bound := &keep
				if v, ok := kw["keep"]; ok {
					if v == nil {
						bound = nil
					} else {
						keep, _ = evidence.PyInt(v)
					}
				}
				err = r.StoreIncident(ctx, text(args[0]), object(args[1]), bound, kw["replace"] == true)
			case "get":
				answer, err = r.Get(ctx, text(args[0]))
			case "incidents":
				answer, err = r.Incidents(ctx, text(args[0]))
			case "listing":
				limit := absent(kw["limit"], 20)
				var stages, dispositions []string
				for _, v := range list(kw["stages"]) {
					stages = append(stages, text(v))
				}
				for _, v := range list(kw["dispositions"]) {
					dispositions = append(dispositions, text(v))
				}
				answer, err = r.Listing(ctx, kw["product"], stages, dispositions, limit, kw["after"])
			case "outstanding_proposals":
				answer, err = r.OutstandingProposals(ctx, args[0])
			case "unreached_proposal":
				answer, err = r.UnreachedProposal(ctx, text(args[0]), args[1], list(args[2]))
			case "unreached_count":
				answer, err = r.UnreachedCount(ctx, list(args[0]))
			case "checked":
				err = s.CheckIncidentRoute(ctx, text(args[0]))
			case "settle":
				err = s.SettleIncidentRoute(ctx, text(args[0]), "observed", text(args[1]), record.Stamp)
			case "_replayed":
				closed := []string{}
				for _, v := range list(args[0]) {
					closed = append(closed, text(v))
				}
				answer, err = r.Replayed(ctx, closed, text(args[1]))
			case "reconcile":
				answer, err = New(s, &integrationClock{stamp: record.Stamp}).reconcile(ctx, kw["product"], absent(kw["limit"], 50), kw["after"])
			case "sql":
				_, err = s.Q(ctx).ExecContext(ctx, text(args[0]), list(args[1])...)
			case "tables":
				answer = storeTables(t, s)
			default:
				t.Fatalf("unknown operation %s", record.Operation)
			}
			if err != nil {
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatal(err)
				}
				answer = Object{"error": "refused", "reason": refusal.Reason, "detail": refusal.Error()}
			}
			got = pyjson.Dumps(answer, pyjson.Options{SortKeys: true, Unicode: true})
		}) {
			return
		}
		golden.Check(t, name, []byte(got), goldenPaths()...)
	}
}
func Test23_PRD_18_RouteRows(t *testing.T)        { routeReplay(t, "PRD-18") }
func Test23_PRD_21_ReplayStorage(t *testing.T)    { routeReplay(t, "PRD-21-replay") }
func Test23_PRD_22_ProposalRotation(t *testing.T) { routeReplay(t, "PRD-22") }
