package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type integrationClock struct {
	stamp string
	now   float64
}

func (c *integrationClock) ISO() string  { return c.stamp }
func (c *integrationClock) Now() float64 { return c.now }
func tablesJSON(ctx context.Context, s *store.Store) (string, error) {
	names, err := s.All(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='schema_meta' ORDER BY name")
	if err != nil {
		return "", err
	}
	tables := Object{}
	for _, name := range names {
		key := text(name.Get("name"))
		rows, err := s.All(ctx, "SELECT * FROM \""+key+"\" ORDER BY rowid")
		if err != nil {
			return "", err
		}
		values := []any{}
		for _, row := range rows {
			m := Object{}
			for _, c := range row {
				m[c.Name] = c.Value
			}
			values = append(values, m)
		}
		tables[key] = values
	}
	return evidence.Dumps(tables, false, true, false), nil
}

// retiredLedgerScenarios are the recorded PRD-13 scenarios that ran with the ledger contract
// absent and whose answers are its route_ledger_pending refusal. The Go runtime always carries
// the ledger contract, so that refusal is unreachable and was retired in wave R1; the recording
// keeps them, and the replay skips them. The other PRD-13 scenarios answer the same with the
// contract present.
var retiredLedgerScenarios = map[string]bool{
	"test_every_port_method_refuses_by_name_while_the_contract_is_absent": true,
	"test_every_ledger_backed_path_refuses_before_writing":                true,
	"test_a_binding_whose_decisions_were_refused_is_not_kept":             true,
}

func integrationReplay(t *testing.T, property string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	script := filepath.Join(filepath.Dir(file), "testdata/integration_capture.py")
	raw := scenarioAnswer(t, "integration_capture.py", property, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "--no-project", "python3", script, property, filepath.Join(filepath.Dir(file), "testdata/properties.md"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Python %s: %v\n%s", property, err, stderr.String())
		}
		return raw, nil
	})
	var records []struct {
		Operation                               string
		Args                                    []any
		Kwargs                                  Object
		Expected, Wire, Tables, Stamp, Scenario string
		Now                                     float64
		PageSize                                int64
		PageInjection                           bool
		Failure                                 string
		TransactionReads                        []any
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&records); err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 {
		t.Fatal("oracle captured no calls")
	}
	root := t.TempDir()
	var s *store.Store
	var router *Router
	clock := &integrationClock{}
	defer func() {
		if s != nil {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	retired := false
	for i, record := range records {
		if record.Operation == "reset" {
			retired = retiredLedgerScenarios[record.Scenario]
		}
		if retired {
			continue
		}
		ok := t.Run(fmt.Sprintf("%03d_%s", i, record.Operation), func(t *testing.T) {
			clock.stamp, clock.now = record.Stamp, record.Now
			entropy := make([]byte, 256)
			for i := range entropy {
				entropy[i] = byte(i % 32)
			}
			ctx := faults.WithInputs(context.Background(), clock, bytes.NewReader(entropy))
			args := list(restoreNumbers(record.Args))
			kw := object(restoreNumbers(record.Kwargs))
			var answer any
			var err error
			reads := []any{}
			if router != nil {
				router.test.decisionRead = func(ctx context.Context, name string) { reads = append(reads, []any{name, s.InTransaction(ctx)}) }
			}
			if record.Operation == "reset" {
				if s != nil {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
				}
				s, err = store.Open(ctx, filepath.Join(root, fmt.Sprint(i), "relay.sqlite3"), "")
				if err == nil {
					router = New(s, clock)
				}
			} else if record.Operation == "sql" {
				_, err = s.Q(ctx).ExecContext(ctx, text(args[0]), list(args[1])...)
			} else {
				router.test.digestPageSize = record.PageSize
				router.test.beforeWrite = nil
				if record.Failure != "" {
					router.test.beforeWrite = func(_ context.Context, step string) error {
						if step == "queue" && record.Failure == "queue" || step == "save" && record.Failure == "filing failed" {
							return fmt.Errorf("%s", record.Failure)
						}
						return nil
					}
				}
				router.test.beforeDigestPage = nil
				if record.PageInjection {
					router.test.beforeDigestPage = func(ctx context.Context, page int) error {
						if page != 2 {
							return nil
						}
						destination, err := PlainTarget(Object{"team": "GMK"})
						if err != nil {
							return err
						}
						return router.routes().Upsert(ctx, Object{"fault_id": strings.Repeat("f", 32), "product": "gamma-kit", "workspace": "example-ws", "disposition": "project_proposal", "stage": "filed", "target": destination, "origin": "observed", "claimed_severity": "notice", "goal": "offline_sync"})
					}
				}
				answer, err = integrationCall(ctx, router, record.Operation, args, kw)
			}
			if err != nil {
				detail := cleanError(err)
				reason, _, _ := strings.Cut(detail, ":")
				answer = Object{"error": "refused", "reason": reason, "detail": detail}
				if record.Failure != "" && detail == record.Failure {
					answer = Object{"error": "RuntimeError", "detail": detail}
				}
			}
			if record.TransactionReads != nil && !equal(record.TransactionReads, reads) {
				t.Fatalf("transaction reads differ\nPython: %s\nGo: %s", evidence.Dumps(record.TransactionReads, false, true, false), evidence.Dumps(reads, false, true, false))
			}
			got := evidence.Dumps(answer, false, true, false)
			if got != record.Expected {
				t.Fatalf("whole reply differs\nargs=%s kw=%s\nPython: %s\nGo:     %s", evidence.Dumps(args, false, true, false), evidence.Dumps(kw, false, true, false), record.Expected, got)
			}
			commands := map[string]string{"router.register_product": "product-register", "router.bind": "product-bind", "router.set_policy": "route-policy", "router.show_products": "product-show", "router.intake": "route-intake", "router.classify": "route-classify", "router.reconcile": "route-reconcile", "router.evaluate_projects": "route-projects", "router.check_completion": "completion-check", "router.digest": "route-digest", "router.show": "route-show"}
			if command := commands[record.Operation]; record.Wire != "" && command != "" {
				wire := evidence.Dumps(CommandRecord(command, answer), false, false, true)
				if wire != record.Wire {
					t.Fatalf("reply wire bytes differ\nPython: %s\nGo:     %s", record.Wire, wire)
				}
			}
			if record.Tables != "" {
				got, err := tablesJSON(ctx, s)
				if err != nil {
					t.Fatal(err)
				}
				if got != record.Tables {
					var py, goValue any
					d := json.NewDecoder(strings.NewReader(record.Tables))
					d.UseNumber()
					if err := d.Decode(&py); err != nil {
						t.Fatal(err)
					}
					d = json.NewDecoder(strings.NewReader(got))
					d.UseNumber()
					if err := d.Decode(&goValue); err != nil {
						t.Fatal(err)
					}
					t.Fatalf("persisted tables differ: %s", firstDifference("tables", py, goValue))
				}
			}
		})
		if !ok {
			return
		}
	}
}
func firstDifference(path string, want, got any) string {
	if equal(want, got) {
		return ""
	}
	switch w := want.(type) {
	case map[string]any:
		g := object(got)
		for _, k := range sortedKeys(w) {
			if d := firstDifference(path+"."+k, w[k], g[k]); d != "" {
				return d
			}
		}
	case []any:
		g := list(got)
		if len(w) != len(g) {
			return fmt.Sprintf("%s.length Python=%d Go=%d", path, len(w), len(g))
		}
		for i := range w {
			if d := firstDifference(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); d != "" {
				return d
			}
		}
	}
	return fmt.Sprintf("%s\nPython: %s\nGo:     %s", path, evidence.Dumps(want, false, true, false), evidence.Dumps(got, false, true, false))
}
func integrationCall(ctx context.Context, r *Router, name string, a []any, k Object) (any, error) {
	arg := func(i int) any {
		if i < len(a) {
			return a[i]
		}
		return nil
	}
	limit := func(fallback int) int {
		if k["limit"] == nil {
			return fallback
		}
		n, _ := evidence.PyInt(k["limit"])
		return int(n)
	}
	pageLimit := func(fallback int) any {
		if value, ok := k["limit"]; ok {
			return value
		}
		return fallback
	}
	l := r.Ledger
	switch name {
	case "registry-installed":
		return Object{"project_create": faults.RegisteredKind("project_create"), "completion_mismatch": faults.RegisteredClass("completion_mismatch"), "missing": []any{}}, nil
	case "router.register_product":
		return r.RegisterProduct(ctx, a[0])
	case "router.bind":
		return r.Bind(ctx, a[0])
	case "router.set_policy":
		return r.SetPolicy(ctx, a[0])
	case "router.show_products":
		return r.ShowProducts(ctx, arg(0))
	case "router.registry":
		return r.Registry(ctx, text(a[0]))
	case "router.registries":
		m, err := r.Registries(ctx)
		out := Object{}
		for key, v := range m {
			out[key] = v
		}
		return out, err
	case "router.bindings":
		return r.Bindings(ctx, text(a[0]))
	case "router.policy":
		return r.Policy(ctx)
	case "router.run_issue":
		return r.RunIssue(ctx, arg(0))
	case "router.intake":
		return r.Intake(ctx, a[0])
	case "router.classify":
		return r.Classify(ctx, text(a[0]), a[1])
	case "router.reconcile":
		return r.Reconcile(ctx, k["product"], pageLimit(50), k["after"])
	case "router.evaluate_projects":
		return r.EvaluateProjects(ctx, text(a[0]))
	case "router.check_completion":
		return r.CheckCompletion(ctx, a[0])
	case "router.digest":
		return r.Digest(ctx, pageLimit(500), k["after"])
	case "router.show":
		product := arg(0)
		if k["product"] != nil {
			product = k["product"]
		}
		return r.Show(ctx, product, k["attention"] == true, pageLimit(20), k["after"])
	case "ledger.get":
		return l.Get(ctx, text(a[0]))
	case "ledger.canonical_id":
		return l.CanonicalID(ctx, text(a[0]), text(a[1]), object(a[2]), text(k["workspace"]))
	case "ledger.set_target":
		return l.SetWorkspaceTarget(ctx, text(k["product"]), text(k["workspace"]), text(k["project"]), text(k["team"]), text(k["project_ref"]))
	case "ledger.adopt":
		return l.Adopt(ctx, text(a[0]), text(k["external_ref"]), object(k["scope"]))
	case "ledger.move":
		return l.Move(ctx, text(a[0]), object(k["scope"]))
	case "ledger.request_update":
		return l.RequestUpdate(ctx, text(a[0]), text(k["op"]), k["value"])
	case "ledger.queue":
		return l.Queue(ctx, text(a[0]), text(k["kind"]), text(k["trigger"]), k["payload"])
	case "ledger.publication":
		return l.Publication(ctx, text(a[0]))
	case "ledger.publications":
		return l.Publications(ctx, text(a[0]), k["kind"], k["state"], limit(20), k["after"])
	case "ledger.cancel":
		return l.Cancel(ctx, text(a[0]), text(k["reason"]))
	case "ledger.claim":
		return l.Claim(ctx, text(a[0]), text(k["owner"]), k["takeover"] == true)
	case "ledger.operation":
		return l.Operation(ctx, text(a[0]), text(k["claim_token"]))
	case "ledger.complete":
		return l.Complete(ctx, text(a[0]), text(k["claim_token"]), text(k["readback"]), text(k["external_ref"]), text(k["project_ref"]), k["observed"])
	case "ledger.fail":
		return l.Fail(ctx, text(a[0]), text(k["claim_token"]), text(k["error"]), k["ended"] == true)
	case "ledger.reconcile":
		return l.Reconcile(ctx, text(a[0]), text(k["observed_text"]), k["searched"] == true, k["observed"], k["prior_ended"] == true, text(k["reason"]))
	case "ledger.record_fix":
		return l.RecordRemediation(ctx, text(a[0]), "fix", text(k["ref"]), "", "", text(k["detail"]))
	case "ledger.record_reverification":
		return l.RecordRemediation(ctx, text(a[0]), "reverification", text(k["ref"]), text(k["method"]), text(k["outcome"]), text(k["detail"]))
	case "ledger.resolve":
		return l.ResolveRecord(ctx, text(a[0]))
	case "ledger.next":
		return l.Next(ctx, limit(4))
	case "ledger.expire_leases":
		return l.ExpireLeases(ctx)
	case "ledger.raise_notification":
		return l.Notify(ctx, text(a[0]), text(k["reason"]), text(k["ref"]))
	case "ledger.notifications":
		return l.Notifications(ctx, text(k["state"]), limit(20), k["after"])
	case "ledger.remediations":
		return l.Remediations(ctx, text(a[0]), limit(20))
	case "ledger.snapshot":
		return l.Snapshot(ctx, text(k["product"]), text(k["fault_class"]), text(k["state"]), limit(20), k["after"])
	case "ledger.set_policy":
		return l.SetPolicy(ctx, text(a[0]), text(a[1]), text(a[2]), k["threshold"], k["window"], text(k["reason"]))
	case "ledger.set_limit":
		return l.SetLimit(ctx, text(a[0]), text(a[1]), k["max_count"], k["window"])
	case "ledger.record":
		m := object(a[0])
		o := faults.Observation{Product: text(m["product"]), FaultClass: text(m["faultClass"]), Severity: text(m["severity"]), Signature: object(m["signature"]), OccurrenceKey: text(m["occurrenceKey"]), Scope: object(m["scope"]), Detail: text(m["detail"]), Evidence: list(m["evidence"]), Cleared: m["cleared"] == true, ObservedAt: m["observedAt"]}
		var adopt *faults.Adoption
		if v := object(k["adopt"]); v != nil {
			adopt = &faults.Adoption{ExternalRef: text(v["externalRef"]), Scope: object(v["scope"])}
		}
		return l.RecordObservation(ctx, o, adopt)
	default:
		return nil, fmt.Errorf("uncaptured implementation %s", name)
	}
}
func Test23_PR_1_PlacementIntegration(t *testing.T)    { integrationReplay(t, "PR-1") }
func Test23_PR_2_ControlGroups(t *testing.T)           { integrationReplay(t, "PR-2") }
func Test23_PR_3_LostCreate(t *testing.T)              { integrationReplay(t, "PR-3") }
func Test23_PR_4_CompletionIntegration(t *testing.T)   { integrationReplay(t, "PR-4") }
func Test23_PR_5_UnplacedMismatch(t *testing.T)        { integrationReplay(t, "PR-5") }
func Test23_PR_6_RemediationHistory(t *testing.T)      { integrationReplay(t, "PR-6") }
func Test23_PR_7_ProjectProposals(t *testing.T)        { integrationReplay(t, "PR-7") }
func Test23_PR_8_ProjectPreIssue(t *testing.T)         { integrationReplay(t, "PR-8") }
func Test23_PR_9_ProjectMembers(t *testing.T)          { integrationReplay(t, "PR-9") }
func Test23_PR_10_ProposalPaging(t *testing.T)         { integrationReplay(t, "PR-10") }
func Test23_PR_11_ProjectLinkage(t *testing.T)         { integrationReplay(t, "PR-11") }
func Test23_PR_12_OriginIsolation(t *testing.T)        { integrationReplay(t, "PR-12") }
func Test23_PR_13_CompletionReplay(t *testing.T)       { integrationReplay(t, "PR-13") }
func Test23_PR_14_ExistingItems(t *testing.T)          { integrationReplay(t, "PR-14") }
func Test23_PR_15_SharedCause(t *testing.T)            { integrationReplay(t, "PR-15") }
func Test23_PR_16_Classification(t *testing.T)         { integrationReplay(t, "PR-16") }
func Test23_PR_17_Reporting(t *testing.T)              { integrationReplay(t, "PR-17") }
func Test23_PR_18_StaticKind(t *testing.T)             { integrationReplay(t, "PR-18") }
func Test23_PR_19_LateOwner(t *testing.T)              { integrationReplay(t, "PR-19") }
func Test23_PR_20_RegistryChanges(t *testing.T)        { integrationReplay(t, "PR-20") }
func Test23_PR_21_TestTargets(t *testing.T)            { integrationReplay(t, "PR-21") }
func Test23_PR_22_TransactionBoundaries(t *testing.T)  { integrationReplay(t, "PR-22") }
func Test23_PR_23_LateBinding(t *testing.T)            { integrationReplay(t, "PR-23") }
func Test23_PR_24_OccurrenceReplay(t *testing.T)       { integrationReplay(t, "PR-24") }
func Test23_PR_25_StaticRegistry(t *testing.T)         { integrationReplay(t, "PR-25") }
func Test23_PR_26_FilingRollback(t *testing.T)         { integrationReplay(t, "PR-26") }
func Test23_PR_27_CurrentRun(t *testing.T)             { integrationReplay(t, "PR-27") }
func Test23_PRD_10_RegistryStore(t *testing.T)         { integrationReplay(t, "PRD-10") }
func Test23_PRD_13_LedgerGate(t *testing.T)            { integrationReplay(t, "PRD-13") }
func Test23_PRD_17_SurfaceClassification(t *testing.T) { integrationReplay(t, "PRD-17") }
