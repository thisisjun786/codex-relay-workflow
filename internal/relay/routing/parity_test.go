package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestMain(m *testing.M) {
	testsupport.Main(m, testsupport.TempDirInRoot, func(string) (func() error, error) {
		return func() error {
			if testBinary == "" {
				return nil
			}
			return os.RemoveAll(filepath.Dir(testBinary))
		}, nil
	})
}
func restoreNumbers(value any) any {
	switch v := value.(type) {
	case map[string]any:
		if token, ok := v["$float"].(string); ok {
			switch token {
			case "nan":
				return math.NaN()
			case "inf":
				return math.Inf(1)
			case "-inf":
				return math.Inf(-1)
			}
		}
		for k, x := range v {
			v[k] = restoreNumbers(x)
		}
	case []any:
		for i, x := range v {
			v[i] = restoreNumbers(x)
		}
	case json.Number:
		if strings.ContainsAny(v.String(), ".eE") {
			n, err := v.Float64()
			if err != nil {
				panic(err)
			}
			return n
		}
	}
	return value
}

func pythonReplay(t *testing.T, property string) {
	t.Helper()
	var records []struct {
		Operation string
		Arguments []any
	}
	scenarioInputs(t, "capture-"+property+".json", &records)
	if len(records) == 0 {
		t.Fatal("the scenario made no calls")
	}
	for i, r := range records {
		name := fmt.Sprintf("%03d_%s", i, r.Operation)
		var got string
		if !t.Run(name, func(t *testing.T) {
			args := list(restoreNumbers(r.Arguments[0]))
			kwargs := pyjson.Map(restoreNumbers(r.Arguments[1]))
			value, err := replayCall(t, r.Operation, args, kwargs)
			if err != nil {
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatal(err)
				}
				value = Object{"error": "refused", "reason": refusal.Reason, "detail": refusal.Error()}
			}
			got = pyjson.Dumps(value, pyjson.Options{SortKeys: true, Unicode: true})
		}) {
			continue
		}
		golden.Check(t, name, []byte(got), goldenPaths()...)
	}
}
func replayCall(t *testing.T, name string, args []any, kwargs Object) (any, error) {
	switch name {
	case "read_registry":
		return ReadRegistry(args[0])
	case "read_binding":
		var registry Object
		if len(args) > 1 {
			registry = pyjson.Map(args[1])
		}
		return ReadBinding(args[0], registry)
	case "read_incident":
		return ReadIncident(args[0])
	case "read_policy":
		return ReadPolicy(args[0])
	case "coverage":
		return Coverage(pyjson.Map(args[0])), nil
	case "canonical":
		return Canonical(args[0])
	case "resolve_product":
		registries := map[string]Object{}
		for k, v := range pyjson.Map(args[0]) {
			registries[k] = pyjson.Map(v)
		}
		product, reason := ResolveProduct(registries, pyjson.Map(args[1]))
		return []any{product, reason}, nil
	case "workspace_for":
		var registry Object
		if len(args) > 1 {
			registry = pyjson.Map(args[1])
		}
		return WorkspaceFor(pyjson.Map(args[0]), registry)
	case "defect_signature":
		attached := kwargs["attached"]
		if len(args) > 1 {
			attached = args[1]
		}
		return DefectSignature(pyjson.Map(args[0]), attached), nil
	case "pending_signature":
		return PendingSignature(pyjson.Map(args[0])), nil
	case "decide":
		bindings := []Object{}
		for _, v := range list(args[2]) {
			bindings = append(bindings, pyjson.Map(v))
		}
		run := kwargs["run_issue"]
		if len(args) > 3 {
			run = args[3]
		}
		return Decide(pyjson.Map(args[0]), pyjson.Map(args[1]), bindings, run), nil
	case "issue_labels":
		return IssueLabels(pyjson.Map(args[0])), nil
	case "detail_text":
		return DetailText(pyjson.Map(args[0])), nil
	case "read_reading":
		return ReadReading(args[0])
	case "evaluate":
		return EvaluateCompletion(pyjson.Map(args[0]), pyjson.Map(args[1])), nil
	case "_obligations":
		return Obligations(pyjson.Map(args[0]), pyjson.Map(args[1]), args[2], list(kwargs["labels"])), nil
	case "read_classification":
		return ReadClassification(args[0])
	case "attention":
		return Attention(pyjson.Map(args[0])), nil
	case "_validate":
		return ValidateProjectPayload(args[0]), nil
	case "_confirm":
		return ConfirmProject(args[0], args[1]), nil
	case "eligibility":
		ctx := context.Background()
		s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		var answer []string
		err = s.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
			for _, table := range []string{"product_registry", "product_bindings", "routing_policy", "incident_routes", "route_incidents"} {
				for _, raw := range list(pyjson.Map(args[0])[table]) {
					row := pyjson.Map(raw)
					columns := sortedKeys(row)
					values := make([]any, len(columns))
					slots := make([]string, len(columns))
					for i, c := range columns {
						values[i], slots[i] = row[c], "?"
					}
					if _, err := s.Q(ctx).ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(slots, ",")+")", values...); err != nil {
						return err
					}
				}
			}
			var err error
			answer, err = ProjectEligibility(ctx, s, pyjson.Map(args[1]))
			return err
		})
		return answer, err
	default:
		return nil, fmt.Errorf("unimplemented captured operation %s", name)
	}
}
func Test23_PRD_1_Validation(t *testing.T)             { pythonReplay(t, "PRD-1") }
func Test23_PRD_2_PolicyLibrary(t *testing.T)          { pythonReplay(t, "PRD-2-library") }
func Test23_PRD_3_FollowUpChecks(t *testing.T)         { pythonReplay(t, "PRD-3") }
func Test23_PRD_4_Coverage(t *testing.T)               { pythonReplay(t, "PRD-4") }
func Test23_PRD_5_ProductResolution(t *testing.T)      { pythonReplay(t, "PRD-5") }
func Test23_PRD_6_Workspace(t *testing.T)              { pythonReplay(t, "PRD-6") }
func Test23_PRD_7_Placement(t *testing.T)              { pythonReplay(t, "PRD-7") }
func Test23_PRD_8_SimulatedIsolation(t *testing.T)     { pythonReplay(t, "PRD-8") }
func Test23_PRD_9_LabelsAndObligations(t *testing.T)   { pythonReplay(t, "PRD-9") }
func Test23_PRD_11_CompletionChecks(t *testing.T)      { pythonReplay(t, "PRD-11") }
func Test23_PRD_12_CompletionClosure(t *testing.T)     { pythonReplay(t, "PRD-12") }
func Test23_PRD_17_ClassificationLibrary(t *testing.T) { pythonReplay(t, "PRD-17-library") }
func Test23_PRD_19_Attention(t *testing.T)             { pythonReplay(t, "PRD-19") }
func Test23_PRD_20_ProjectEligibility(t *testing.T)    { pythonReplay(t, "PRD-20") }
