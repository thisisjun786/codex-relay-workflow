package trace_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

const goldenPath = "../../../testdata/flow/shadow/trace.golden.json"

// export reads the copy of the scenario's store.
func export(t *testing.T) (*tracetest.Scenario, string, *trace.Trace) {
	t.Helper()
	s := tracetest.NewScenario(tracetest.New(t))
	path := s.Copy()
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return s, path, tr
}

// TestTraceExportContract pins crw-flow-trace/1: the export of a synthetic store copy is the golden file (the copy's own
// digest and store id aside), the file reads back through the strict offline reader to the same bytes, the event ordinals,
// the node stage transitions, the calls with their tokens and the delivery, acceptance and integration times are what the
// store holds, and the reader refuses what is not the contract.
func TestTraceExportContract(t *testing.T) {
	s, path, tr := export(t)

	// the source names the copy's bytes
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if tr.Schema != trace.Schema || tr.Source.Digest != hex.EncodeToString(sum[:]) || tr.Source.Bytes != int64(len(data)) || tr.Source.StoreID == "" {
		t.Errorf("source = %+v, schema %q", tr.Source, tr.Schema)
	}

	// the golden file holds everything but the source (a copy's digest and id differ from copy to copy)
	golden := *tr
	golden.Source = trace.Source{}
	got, err := trace.Marshal(&golden)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CRW_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (CRW_UPDATE_GOLDEN=1 writes it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the export differs from %s (CRW_UPDATE_GOLDEN=1 rewrites it)\n%s", goldenPath, firstDifference(got, want))
	}

	// the reader gives back the same trace, byte for byte
	full, err := trace.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	back, err := trace.Parse(full)
	if err != nil {
		t.Fatal(err)
	}
	again, err := trace.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, again) || !reflect.DeepEqual(tr, back) {
		t.Errorf("the reader does not give back the trace it read")
	}

	run := tr.Run(s.Run)
	if run == nil || len(tr.Runs) != 1 {
		t.Fatalf("runs = %d", len(tr.Runs))
	}
	// event ordinals: the journal's own, 1..n, every one with the time it was recorded
	for i, e := range run.Events {
		if e.Seq != i+1 || e.At == "" {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
	// calls: tokens, status, roles, restored from the rows alone
	byLabel := map[string]trace.Call{}
	for _, c := range run.Calls {
		byLabel[c.Label] = c
	}
	for label, want := range map[string]struct {
		role, status string
		tokens       int64
	}{
		"build-n1": {"executor", "completed", 1000}, "review-n1": {"reviewer", "completed", 400}, "build-n2": {"executor", "completed", 700},
		"build-n2-t1": {"executor-t1", "completed", 900}, "build-n3": {"executor", "unknown", 0},
	} {
		c := byLabel[label]
		if c.Role != want.role || c.Status != want.status || c.Tokens.Total != want.tokens || c.CallID != s.Calls[label] {
			t.Errorf("call %s = %+v, want %+v", label, c, want)
		}
	}
	if byLabel["build-n3"].Reason != "dispatch_unknown" || byLabel["build-n2-t1"].StepDecision == "" {
		t.Errorf("build-n3 / build-n2-t1 = %+v / %+v", byLabel["build-n3"], byLabel["build-n2-t1"])
	}
	// node stage transitions, oldest first
	stages := map[string][]string{}
	for _, n := range run.Nodes {
		for _, tr := range n.Transitions {
			stages[n.NodeID] = append(stages[n.NodeID], tr.Stage)
		}
	}
	if want := []string{"released", "build", "review", "accepted", "integrated"}; !slices.Equal(stages["n1"], want) {
		t.Errorf("n1 stages = %v, want %v", stages["n1"], want)
	}
	if want := []string{"released", "build", "build", "accepted", "integrated"}; !slices.Equal(stages["n2"], want) {
		t.Errorf("n2 stages = %v, want %v", stages["n2"], want)
	}
	if want := []string{"released", "build", "escalated"}; !slices.Equal(stages["n3"], want) {
		t.Errorf("n3 stages = %v, want %v", stages["n3"], want)
	}
	// recorded decisions, delivery, acceptance and integration times
	var sources []string
	for _, d := range run.Decisions {
		sources = append(sources, d.Source+":"+d.Kind)
	}
	slices.Sort(sources)
	if want := []string{"dag_agent_closures:escalation", "flow_spawn_decisions:inline", "flow_step_decisions:tier"}; !slices.Equal(sources, want) {
		t.Errorf("decisions = %v, want %v", sources, want)
	}
	var journal []string
	for _, j := range tr.Journal {
		journal = append(journal, j.Kind+":"+j.Subject)
	}
	if want := []string{"delivery_deferred_busy:ev-2", "delivery_deferred_busy:ev-3", "delivery_queued:ev-1", "event_accepted:ev-1"}; !slices.Equal(journal, want) {
		t.Errorf("relay journal rows = %v, want %v (a row of another kind is not carried)", journal, want)
	}
	if len(tr.Deliveries) != 3 || tr.Deliveries[1].FirstSentAt != tracetest.At(210) || tr.Deliveries[1].Sends != 2 || tr.Deliveries[2].FirstSentAt != "" {
		t.Errorf("deliveries = %+v", tr.Deliveries)
	}
	if len(tr.Acceptances) != 2 || tr.Acceptances[0].At != tracetest.At(100) || tr.Acceptances[0].ReviewerCallID != s.Calls["review-n1"] {
		t.Errorf("acceptances = %+v", tr.Acceptances)
	}
	if len(tr.Integration) != 2 || tr.Integration[0].Landed || !tr.Integration[1].Landed || tr.Integration[1].MovedAt != tracetest.At(125) || tr.Integration[1].Nodes[0].MarkedAt != tracetest.At(126) {
		t.Errorf("integration = %+v", tr.Integration)
	}
	if tr.Aux.SyncOutbox.Rows != 2 || tr.Aux.MergeTrains.Trains != 1 {
		t.Errorf("aux = %+v", tr.Aux)
	}

	// --run keeps one run and refuses one the copy lacks
	one, err := trace.Export(tracetest.Context(t), trace.Options{Path: path, Run: s.Run})
	if err != nil || len(one.Runs) != 1 {
		t.Errorf("--run %s: %v", s.Run, err)
	}
	if _, err := trace.Export(tracetest.Context(t), trace.Options{Path: path, Run: "run-none"}); err == nil || !strings.Contains(err.Error(), "run-none") {
		t.Errorf("--run run-none: %v", err)
	}

	// the reader is strict
	for name, mutate := range map[string]func(map[string]any){
		"another schema": func(m map[string]any) { m["schema"] = "crw-flow-trace/2" },
		"unknown member": func(m map[string]any) { m["extra"] = 1 },
		"rows disordered": func(m map[string]any) {
			m["runs"].([]any)[0].(map[string]any)["events"].([]any)[2].(map[string]any)["seq"] = 1
		},
	} {
		var m map[string]any
		if err := json.Unmarshal(full, &m); err != nil {
			t.Fatal(err)
		}
		mutate(m)
		bad, _ := json.Marshal(m)
		if _, err := trace.Parse(bad); err == nil {
			t.Errorf("%s: the reader accepted it", name)
		}
	}
	if _, err := trace.Parse(append(append([]byte{}, full...), []byte(" {}")...)); err == nil {
		t.Errorf("data after the trace: the reader accepted it")
	}
	if _, err := trace.Parse([]byte("SQLite format 3\x00")); err == nil {
		t.Errorf("a store file: the reader accepted it")
	}

	// a store without the flow zone yields a trace without runs, a file that is no store is refused
	plain := filepath.Join(t.TempDir(), "not-a-store")
	if err := os.WriteFile(plain, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Export(tracetest.Context(t), trace.Options{Path: plain}); err == nil {
		t.Errorf("a file that is no store was exported")
	}
}

func firstDifference(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return fmt.Sprintf("line %d: got  %s\n        want %s", i+1, g[i], w[i])
		}
	}
	return "lengths differ"
}

// TestExportRefusesLiveStore keeps RefuseLive honest at the package: a file under the live state directory, a hard link and a
// symbolic link to it are refused by Export, Write and ReadFile; a faithful copy elsewhere (the same store id) is not.
func TestExportRefusesLiveStore(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	copyPath := s.Copy()
	home := os.Getenv("HOME") // the home the isolation gave this test binary
	if home == "" {
		t.Fatal("HOME is not set")
	}
	live := filepath.Join(home, ".local", "state", "codex-session-relay", "0123456789abcdef", "relay.sqlite3")
	if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, data, 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	hard := filepath.Join(other, "hard.sqlite3")
	if err := os.Link(live, hard); err != nil {
		t.Fatal(err)
	}
	soft := filepath.Join(other, "soft.sqlite3")
	if err := os.Symlink(live, soft); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"the live file": live, "a hard link": hard, "a symbolic link": soft, "a new file in the live directory": filepath.Join(filepath.Dir(live), "trace.json")} {
		if err := trace.RefuseLive(p); !errors.Is(err, trace.ErrLiveStore) {
			t.Errorf("%s: RefuseLive = %v", name, err)
		}
	}
	for _, p := range []string{live, hard, soft} {
		if _, err := trace.Export(tracetest.Context(t), trace.Options{Path: p}); !errors.Is(err, trace.ErrLiveStore) {
			t.Errorf("Export(%s) = %v", p, err)
		}
		if _, err := trace.ReadFile(p); !errors.Is(err, trace.ErrLiveStore) {
			t.Errorf("ReadFile(%s) = %v", p, err)
		}
	}
	if err := trace.Write(hard, &trace.Trace{Schema: trace.Schema}); !errors.Is(err, trace.ErrLiveStore) {
		t.Errorf("Write(hard link) = %v", err)
	}
	if after, _ := os.ReadFile(live); !bytes.Equal(after, data) {
		t.Errorf("the live file changed")
	}
	// a faithful copy keeps the store id of the store it came from and is no live store
	if err := trace.RefuseLive(copyPath); err != nil {
		t.Errorf("RefuseLive(copy) = %v", err)
	}
	if err := trace.RefuseLive(filepath.Join(other, "new-output.json")); err != nil {
		t.Errorf("RefuseLive(new output) = %v", err)
	}
}
