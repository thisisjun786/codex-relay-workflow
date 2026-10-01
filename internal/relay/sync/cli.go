package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// commandNames are the sync and packet commands, in cli.py's add_parser order.
var commandNames = []string{"sync-target", "sync-next", "sync-claim", "sync-operation", "sync-reconcile", "sync-complete", "sync-fail", "sync-retry", "sync-status", "sync-progress", "packet-check"}

func init() {
	var commands []dispatch.Command
	for _, name := range commandNames {
		commands = append(commands, dispatch.Command{Name: name, Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
			return run(ctx, services, name, args)
		}, Defaults: commandDefaults[name], ReadOnly: readOnly[name],
			// packet-check reads a supplied record, or its store-backed check consults the
			// selection refusal itself (run).
			Exempt: name == "packet-check"})
	}
	dispatch.Register(nil, commands...)
}

var (
	commandDefaults = map[string]map[string]any{"sync-target": {"target": "coordination_document"}, "sync-next": {"limit": int64(4)}}
	readOnly        = map[string]bool{"sync-next": true, "sync-operation": true, "sync-status": true, "packet-check": true}
)

// run answers one command from its options' texts: each option the line gave, and each default.
func run(ctx context.Context, services dispatch.Services, name string, args dispatch.Args) (any, error) {
	values := map[string]string{}
	for key, given := range args.Parsed.Values {
		values[key] = given[len(given)-1]
	}
	for key := range args.Defaults {
		values[key] = args.Text(key)
	}
	var policy *registry.RolePolicy
	if name == "packet-check" && values["receiver"] != "" && values["applied"] != "true" {
		if err := dispatch.CheckSelection(services); err != nil {
			return nil, err
		}
		// cli.py main settles the launch declaration before any handler reads the
		// packet, so a refused declaration answers before a bad packet does.
		resolved, err := packetPolicy(services.Selection.Path, os.Getenv(execution.EnvPolicy))
		if err != nil {
			return nil, err
		}
		policy = &resolved
	}
	result, err := runCommand(ctx, services, name, values, policy)
	return wireValue(result), err
}

func runCommand(ctx context.Context, services dispatch.Services, name string, args map[string]string, policy *registry.RolePolicy) (any, error) {
	if name == "packet-check" {
		return packetCommand(ctx, services, args, policy)
	}
	s, e := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if e != nil {
		return nil, e
	}
	defer s.Close()
	outbox := cliOutbox(s)
	id := args["sync"]
	switch name {
	case "sync-target":
		return outbox.SetTarget(ctx, args["relationship"], args["target"], args["target-ref"])
	case "sync-next":
		limit := 4
		if args["limit"] != "" {
			limit, e = strconv.Atoi(args["limit"])
			if e != nil {
				return nil, e
			}
		}
		jobs, e := outbox.Next(ctx, args["target"], limit, outbox.Clock.Now())
		return obj("jobs", jobs), e
	case "sync-claim":
		return outbox.Claim(ctx, id, args["owner"], outbox.Clock.Now())
	case "sync-operation":
		return outbox.Operation(ctx, id)
	case "sync-reconcile":
		observed, e := readText(args["observed"])
		if e != nil {
			return nil, e
		}
		return outbox.Reconcile(ctx, id, observed)
	case "sync-complete":
		readback, e := readText(args["readback"])
		if e != nil {
			return nil, e
		}
		var external *string
		if v, ok := args["external-ref"]; ok {
			external = &v
		}
		return outbox.Complete(ctx, id, args["claim-token"], args["target-ref"], readback, external)
	case "sync-fail":
		return outbox.Fail(ctx, id, args["claim-token"], args["error"], outbox.Clock.Now())
	case "sync-retry":
		return outbox.Retry(ctx, id)
	case "sync-status":
		return outbox.Snapshot(ctx, args["relationship"])
	case "sync-progress":
		reg := &registry.Registry{Store: s}
		assignment, e := registry.NewAssignmentView(reg).State(ctx, args["relationship"])
		if e != nil {
			return nil, e
		}
		head := reception.Get(assignment, "head")
		revision := text(reception.Get(head, "revisionHash"))
		if revision == "" {
			revision = "none"
		}
		if len(revision) > 12 {
			revision = revision[:12]
		}
		summary := fmt.Sprintf("%s · %s · %s\ngeneration %v, next: %s\ncurrent revision: %s (%s)", reception.Get(assignment, "issueKey"), reception.Get(assignment, "childTaskId"), reception.Get(assignment, "state"), reception.Get(assignment, "executionGeneration"), reception.Get(assignment, "nextExpectedAction"), revision, reception.Get(head, "evidence"))
		var identifier any
		e = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
			var e error
			identifier, e = outbox.EnqueueIn(ctx, Enqueue{RelationshipID: args["relationship"], IssueKey: text(reception.Get(assignment, "issueKey")), SubjectKind: "progress", Summary: summary, EventID: reception.Get(head, "eventId"), Generation: reception.Get(assignment, "executionGeneration"), Revision: reception.Get(head, "revisionHash")})
			return e
		})
		return obj("syncId", identifier, "state", reception.Get(assignment, "state")), e
	}
	return nil, fmt.Errorf("unknown sync command %s", name)
}
func readText(value string) (string, error) {
	if !strings.HasPrefix(value, "@") {
		return value, nil
	}
	raw, e := os.ReadFile(value[1:])
	if e != nil {
		return "", e
	}
	return string(raw), nil
}

// pathProblem is what went wrong with a path, without the operation and the path its
// *fs.PathError repeats.
func pathProblem(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

func readDocument(path, what string, packet bool) (any, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, &dispatch.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %q could not be read: %v", what, path, pathProblem(e))}
	}
	if problem := reception.JSONReaderDepthProblem(raw); problem != "" {
		return nil, &dispatch.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %q could not be read: %s", what, path, problem)}
	}
	if packet {
		if e = reception.CheckJSONText(raw); e != nil {
			if store.RefusalReason(e) != "" {
				return nil, e
			}
		}
	}
	v, e := registry.DecodeJSON(string(raw))
	if e != nil {
		return nil, &dispatch.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %q could not be read: %v", what, path, e)}
	}
	return v, nil
}

// packetCommand is cmd_packet_check; policy is the role policy the store-backed check judges
// pairs by, settled before the handler (packetPolicy).
func packetCommand(ctx context.Context, services dispatch.Services, args map[string]string, policy *registry.RolePolicy) (any, error) {
	packet, e := readDocument(args["packet"], "relay-packet/1 message", true)
	if e != nil {
		return nil, e
	}
	receiver, storeMode := args["receiver"]
	applied := args["applied"] == "true"
	if !storeMode {
		if args["observation"] != "" || args["ledger"] != "" || applied {
			return nil, &dispatch.UsageError{Code: 4, Detail: "--observation, --ledger and --applied belong to the store reading (--receiver); a supplied record answers for itself"}
		}
		record, e := readDocument(args["record"], "receiver's own reading", false)
		if e != nil {
			return nil, e
		}
		answer, e := reception.Reception(packet, record)
		if e != nil {
			return nil, e
		}
		reception.Set(&answer, "recordSource", "supplied")
		ladder := reception.Get(packet, "progression")
		if ladder == nil {
			ladder = reception.Unobserved()
		}
		promotions, e := reception.UnsupportedPromotions(ladder)
		reception.Set(&answer, "promotions", promotions)
		return answer, e
	}
	ledgerPath := args["ledger"]
	if applied {
		if ledgerPath == "" {
			return nil, &dispatch.UsageError{Code: 4, Detail: "--applied records what you did in your own reception ledger; name it with --ledger"}
		}
		if args["observation"] != "" {
			return nil, &dispatch.UsageError{Code: 4, Detail: "--applied reads nothing; an observation belongs to the check that came before it"}
		}
		var answer Obj
		e = reception.WithLedgerLock(ledgerPath, func() error {
			ledger, e := reception.LoadLedger(ledgerPath, receiver)
			if e != nil {
				return e
			}
			answer, e = reception.RecordApplied(&ledger, packet)
			if e != nil {
				return e
			}
			if reception.Get(answer, "alreadyApplied") != true {
				return reception.SaveLedger(ledgerPath, ledger)
			}
			return nil
		})
		if e != nil {
			return nil, ledgerUsage(e)
		}
		reception.Set(&answer, "receiver", receiver)
		reception.Set(&answer, "ledger", ledgerPath)
		return answer, nil
	}
	var observation any
	if args["observation"] != "" {
		document, e := readDocument(args["observation"], "observation", false)
		if e != nil {
			return nil, e
		}
		observation, e = reception.Observation(document)
		if e != nil {
			return nil, e
		}
	}
	if policy == nil {
		// An empty --receiver still reads the store (args.receiver is not None) but is not
		// the receive check main settles a declaration for (bool(args.receiver)), so the
		// process's own snapshot judges, as rolepolicy.declared() takes it.
		own := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: os.Getenv(execution.EnvPolicy)})
		policy = &own
	}
	reader := reception.Reader{Policy: *policy}
	connection, openError := store.OpenReadOnly(ctx, services.Selection.DBPath(), time.Second)
	if errors.Is(openError, store.ErrLiveState) {
		// The live-state guard (test isolation, CRW_REFUSE_LIVE_STATE=1) is a refusal to
		// report, never a store that merely could not be opened read-only.
		return nil, openError
	}
	if openError == nil {
		defer connection.Close()
	}
	var answer Obj
	check := func(ledger any) error {
		if connection == nil {
			var e error
			answer, e = reader.Check(ctx, nil, packet, receiver, observation, ledger)
			return e
		}
		return connection.ReadSnapshot(ctx, func(ctx context.Context, s *store.Store) error {
			var e error
			answer, e = reader.Check(ctx, s, packet, receiver, observation, ledger)
			return e
		})
	}
	if ledgerPath == "" {
		e = check(nil)
		return answer, e
	}
	e = reception.WithLedgerLock(ledgerPath, func() error {
		ledger, e := reception.LoadLedger(ledgerPath, receiver)
		if e != nil {
			return e
		}
		if e = check(ledger); e != nil {
			return e
		}
		if reception.RecordAnswer(&ledger, packet, answer) {
			return reception.SaveLedger(ledgerPath, ledger)
		}
		return nil
	})
	if e != nil {
		return nil, ledgerUsage(e)
	}
	reception.Set(&answer, "ledger", ledgerPath)
	return answer, nil
}
func ledgerUsage(e error) error {
	var unusable *reception.LedgerError
	if errors.As(e, &unusable) {
		return &dispatch.UsageError{Code: 4, Detail: unusable.Detail}
	}
	return e
}
