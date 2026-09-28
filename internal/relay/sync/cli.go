package sync

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func init() {
	for _, name := range []string{"sync-target", "sync-next", "sync-claim", "sync-operation", "sync-reconcile", "sync-complete", "sync-fail", "sync-retry", "sync-status", "sync-progress", "packet-check"} {
		name := name
		spec := argparse.Specs[name]
		required := []string{}
		for _, action := range spec.Actions {
			if action.Required {
				required = append(required, strings.TrimPrefix(action.Flags[len(action.Flags)-1], "--"))
			}
		}
		cli.Commands = append(cli.Commands, cli.Command{Name: name, Required: required, Exempt: name == "packet-check", Flags: func(f *flag.FlagSet) {
			for _, action := range spec.Actions {
				if len(action.Flags) == 0 || action.Kind == "_HelpAction" {
					continue
				}
				key := strings.TrimPrefix(action.Flags[len(action.Flags)-1], "--")
				if action.Kind == "_StoreTrueAction" {
					f.Bool(key, false, "")
				} else if action.Type == "int" {
					fallback := int64(0)
					if name == "sync-next" && key == "limit" {
						fallback = 4
					}
					f.Int64(key, fallback, "")
				} else {
					fallback := ""
					if name == "sync-target" && key == "target" {
						fallback = "coordination_document"
					}
					f.String(key, fallback, "")
				}
			}
		}, Run: func(ctx context.Context, services cli.Services, args cli.Args) (any, error) {
			values := map[string]string{}
			args.Flags.VisitAll(func(f *flag.Flag) {
				if args.Set[f.Name] || f.DefValue != "" {
					values[f.Name] = f.Value.String()
				}
			})
			if name == "packet-check" && values["receiver"] != "" && values["applied"] != "true" {
				if err := cli.CheckPacketSelection(services); err != nil {
					return nil, err
				}
			}
			result, err := runCommand(ctx, services, name, values)
			return wireValue(result), err
		}})
	}
}
func runCommand(ctx context.Context, services cli.Services, name string, args map[string]string) (any, error) {
	if name == "packet-check" {
		return packetCommand(ctx, services, args)
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
		return "", &cli.HostError{Class: "FileNotFoundError", Detail: store.PythonOSErrorText(e)}
	}
	return string(raw), nil
}
func readDocument(path, what string, packet bool) (any, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, &cli.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %s could not be read: FileNotFoundError: %s", what, store.PyRepr(path), store.PythonOSErrorText(e))}
	}
	if problem := reception.JSONReaderDepthProblem(raw); problem != "" {
		return nil, &cli.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %s could not be read: RecursionError: %s", what, store.PyRepr(path), problem)}
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
		return nil, &cli.UsageError{Code: 4, Detail: fmt.Sprintf("the %s at %s could not be read: JSONDecodeError: %s", what, store.PyRepr(path), e)}
	}
	return v, nil
}
func packetCommand(ctx context.Context, services cli.Services, args map[string]string) (any, error) {
	packet, e := readDocument(args["packet"], "relay-packet/1 message", true)
	if e != nil {
		return nil, e
	}
	receiver, storeMode := args["receiver"]
	applied := args["applied"] == "true"
	if !storeMode {
		if args["observation"] != "" || args["ledger"] != "" || applied {
			return nil, &cli.UsageError{Code: 4, Detail: "--observation, --ledger and --applied belong to the store reading (--receiver); a supplied record answers for itself"}
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
			return nil, &cli.UsageError{Code: 4, Detail: "--applied records what you did in your own reception ledger; name it with --ledger"}
		}
		if args["observation"] != "" {
			return nil, &cli.UsageError{Code: 4, Detail: "--applied reads nothing; an observation belongs to the check that came before it"}
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
	policy, e := packetPolicy(services.Selection.Path, os.Getenv(execution.EnvPolicy))
	if e != nil {
		return nil, e
	}
	reader := reception.Reader{Policy: policy}
	connection, openError := store.OpenReadOnly(ctx, services.Selection.DBPath(), time.Second)
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
		return &cli.UsageError{Code: 4, Detail: unusable.Detail}
	}
	return e
}
