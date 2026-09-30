package routing

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "routing-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if os.Getenv("GOPATH") == "" {
		if err := os.Setenv("GOPATH", filepath.Join(os.Getenv("HOME"), "go")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if os.Getenv("GOCACHE") == "" {
		if err := os.Setenv("GOCACHE", filepath.Join(cache, "go-build")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, "tmp"), 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	env := map[string]string{"HOME": home, "XDG_STATE_HOME": filepath.Join(home, "state"), "XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_DATA_HOME": filepath.Join(home, "data"), "CODEX_HOME": filepath.Join(home, "codex"), "CODEX_SESSION_RELAY_STATE": filepath.Join(home, "relay"), "CODEX_SESSION_RELAY_SCOPE_DIR": filepath.Join(home, "scope"), "TMPDIR": filepath.Join(home, "tmp"), "UV_CACHE_DIR": filepath.Join(home, "uv-cache"), "UV_PYTHON_DOWNLOADS": "never"}
	for k, v := range env {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	if testBinary != "" {
		if err := os.RemoveAll(filepath.Dir(testBinary)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
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

// pythonTemporaries names each temporary directory the Python scenarios made under TMPDIR by its
// order of first appearance instead of its random name, so the answer is the same on every run.
// The directories are gone when Python exits; Go only ever sees their names.
func pythonTemporaries(raw []byte) []byte {
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		return raw
	}
	names := map[string]string{}
	return regexp.MustCompile(regexp.QuoteMeta(tmp)+`/[A-Za-z0-9._-]+`).ReplaceAllFunc(raw, func(path []byte) []byte {
		name, ok := names[string(path)]
		if !ok {
			name = fmt.Sprintf("%s/python-temporary-%d", tmp, len(names)+1)
			names[string(path)] = name
		}
		return []byte(name)
	})
}

// scenarioAnswer returns what a Python scenario capture script printed for property: the
// recording by default, a live run under CRW_PYTHON_ORACLE=record or check (see pyoracle).
func scenarioAnswer(t *testing.T, script, property string, capture func() ([]byte, error)) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return pyoracle.AnswerInterned(t, script+" "+property, func() ([]byte, error) {
		raw, err := capture()
		return pythonTemporaries(raw), err
	}, pyoracle.Substitute(filepath.Clean(filepath.Join(filepath.Dir(file), "../../..")), "<repo>"), pyoracle.Substitute(os.Getenv("TMPDIR"), "<tmpdir>"))
}

func pythonReplay(t *testing.T, property string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	raw := scenarioAnswer(t, "capture.py", property, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "--no-project", "python3", filepath.Join(root, "internal/relay/routing/testdata/capture.py"), property)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Python %s: %v\n%s", property, err, stderr.String())
		}
		return raw, nil
	})
	var records []struct {
		Operation string
		Arguments []any
		Expected  string
		Wire      string
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&records); err != nil {
		t.Fatalf("capture: %v\n%s", err, raw)
	}
	if len(records) == 0 {
		t.Fatal("oracle captured no calls")
	}
	for i, r := range records {
		t.Run(fmt.Sprintf("%03d_%s", i, r.Operation), func(t *testing.T) {
			args := list(restoreNumbers(r.Arguments[0]))
			kwargs := object(restoreNumbers(r.Arguments[1]))
			value, err := replayCall(t, r.Operation, args, kwargs)
			if err != nil {
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatal(err)
				}
				value = Object{"error": "refused", "reason": refusal.Reason, "detail": refusal.Error()}
			}
			got := evidence.Dumps(value, false, true, false)
			if got != r.Expected {
				t.Fatalf("complete Python result differs\noperation: %s\narguments: %s\nPython: %s\nGo:     %s", r.Operation, evidence.Dumps(args, false, true, false), r.Expected, got)
			}
			if r.Wire != "" {
				var wire any
				switch r.Operation {
				case "decide":
					wire = PlacementRecord(object(value))
				case "evaluate":
					wire = CompletionRecord(object(value))
				default:
					t.Fatalf("missing wire encoder for %s", r.Operation)
				}
				got := evidence.Dumps(wire, false, false, true)
				if got != r.Wire {
					t.Fatalf("wire bytes differ\nPython: %s\nGo:     %s", r.Wire, got)
				}
			}
		})
	}
}
func replayCall(t *testing.T, name string, args []any, kwargs Object) (any, error) {
	switch name {
	case "read_registry":
		return ReadRegistry(args[0])
	case "read_binding":
		var registry Object
		if len(args) > 1 {
			registry = object(args[1])
		}
		return ReadBinding(args[0], registry)
	case "read_incident":
		return ReadIncident(args[0])
	case "read_policy":
		return ReadPolicy(args[0])
	case "coverage":
		return Coverage(object(args[0])), nil
	case "canonical":
		return Canonical(args[0])
	case "resolve_product":
		registries := map[string]Object{}
		for k, v := range object(args[0]) {
			registries[k] = object(v)
		}
		product, reason := ResolveProduct(registries, object(args[1]))
		return []any{product, reason}, nil
	case "workspace_for":
		var registry Object
		if len(args) > 1 {
			registry = object(args[1])
		}
		return WorkspaceFor(object(args[0]), registry)
	case "defect_signature":
		attached := kwargs["attached"]
		if len(args) > 1 {
			attached = args[1]
		}
		return DefectSignature(object(args[0]), attached), nil
	case "pending_signature":
		return PendingSignature(object(args[0])), nil
	case "decide":
		bindings := []Object{}
		for _, v := range list(args[2]) {
			bindings = append(bindings, object(v))
		}
		run := kwargs["run_issue"]
		if len(args) > 3 {
			run = args[3]
		}
		return Decide(object(args[0]), object(args[1]), bindings, run), nil
	case "issue_labels":
		return IssueLabels(object(args[0])), nil
	case "detail_text":
		return DetailText(object(args[0])), nil
	case "read_reading":
		return ReadReading(args[0])
	case "evaluate":
		return EvaluateCompletion(object(args[0]), object(args[1])), nil
	case "_obligations":
		return Obligations(object(args[0]), object(args[1]), args[2], list(kwargs["labels"])), nil
	case "read_classification":
		return ReadClassification(args[0])
	case "attention":
		return Attention(object(args[0])), nil
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
				for _, raw := range list(object(args[0])[table]) {
					row := object(raw)
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
			answer, err = ProjectEligibility(ctx, s, object(args[1]))
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
