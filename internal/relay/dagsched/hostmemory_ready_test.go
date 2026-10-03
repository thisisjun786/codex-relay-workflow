package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-468: the host memory bound in the ready set, the release, the pass record, the measurements and the commands. Every test feeds a sample or a fake /proc root; none reads the live host.

func healthyHost() HostMemory { return fixedMemory(40<<30, 8<<30, 8<<30, 0) }

// hostBound is the bound of a sample under the documented defaults.
func hostBound(sample HostMemory) *HostMemoryBound {
	return &HostMemoryBound{Sample: sample, Limits: DefaultHostMemoryLimits(), LimitsFrom: LimitsDefault}
}

// shortHosts are the three ways the host can be short, each alone.
var shortHosts = []struct {
	name     string
	sample   HostMemory
	detail   string
	exceeded []string
}{
	{"available", fixedMemory(9<<30, 8<<30, 8<<30, 0), "available 9.00 GiB is under the 15.00 GiB floor", []string{"available"}},
	{"swap", fixedMemory(40<<30, 8<<30, 2<<30, 0), "swap use 75.00 percent is over the 50.00 percent ceiling", []string{"swap"}},
	{"pressure", fixedMemory(40<<30, 8<<30, 8<<30, 12.5), "pressure some avg10 12.50 is over the 10.00 ceiling", []string{"pressure"}},
}

func (f *fixture) threeNodes(plan string) {
	f.t.Helper()
	f.projectParent()
	f.putPlan(plan, 0, plan+"-r1", addNode("n1", dag.NodeNonPR), addNode("n2", dag.NodeNonPR), addNode("n3", dag.NodeNonPR))
}

// asJSON is a printed object as the generic document a caller of the command reads.
func asJSON(t testing.TB, o any) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(pyjson.Dumps(o, pyjson.Options{Compact: true})), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// hostState is the state of the bound a reading carries, "" for a reading without one.
func hostState(r Reading) string {
	if r.Pass.HostMemory == nil {
		return ""
	}
	return r.Pass.HostMemory.State
}

func readyIDs(r Reading) []string {
	var ids []string
	for _, n := range r.Ready {
		ids = append(ids, n.NodeID)
	}
	return ids
}

// c1: while any of the three readings is over its threshold every candidate is deferred with the host memory reason and the measured values ride with the reading; once the readings are
// back within their thresholds the same nodes are ready again.
func TestReadyDefersEveryCandidateWhileHostMemoryIsShort(t *testing.T) {
	for _, c := range shortHosts {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.threeNodes("hm")
			f.sched.Host = hostBound(healthyHost())
			well := f.read("hm")
			if !reflect.DeepEqual(readyIDs(well), []string{"n1", "n2", "n3"}) || well.Pass.DecidingLimit != LimitNone || hostState(well) != HostMemoryWithin {
				t.Fatalf("a healthy host: ready %v, pass %+v", readyIDs(well), well.Pass)
			}

			f.sched.Host = hostBound(c.sample)
			short := f.read("hm")
			if len(short.Ready) != 0 || short.Pass.ReadyCount != 0 || short.Pass.DecidingLimit != LimitHostMemory || short.Pass.FreeSlots != 6 {
				t.Fatalf("a short host: ready %v, pass %+v, want nothing ready, the host memory limit and the slots untouched", readyIDs(short), short.Pass)
			}
			for _, id := range []string{"n1", "n2", "n3"} {
				n := short.node(id)
				if n.State != StateReady || n.Disposition != DispDefer || n.Reason != DeferHostMemory || n.Rank == nil || !strings.Contains(n.Detail, c.detail) {
					t.Errorf("%s = %+v, want a ready-state candidate deferred by %s with %q in its detail", id, n, DeferHostMemory, c.detail)
				}
			}
			if hostState(short) != HostMemoryDeferring || !reflect.DeepEqual(short.Pass.HostMemory.Exceeded, c.exceeded) {
				t.Fatalf("verdict = %+v, want deferring on %v", short.Pass.HostMemory, c.exceeded)
			}

			// the values are in the printed reading
			printed := asJSON(t, short.Object())["pass"].(map[string]any)["host_memory"].(map[string]any)
			measured := printed["measured"].(map[string]any)
			limits := printed["limits"].(map[string]any)
			if printed["state"] != "deferring" || printed["limits_from"] != "default" || limits["min_available_bytes"] != float64(15<<30) || limits["max_swap_percent"] != float64(50) || limits["max_pressure_some_avg10"] != float64(10) ||
				measured["available_bytes"] != float64(*c.sample.AvailableBytes) || measured["pressure_some_avg10"] != *c.sample.PressureSomeAvg10 {
				t.Fatalf("host_memory = %v", printed)
			}

			f.sched.Host = hostBound(healthyHost())
			if again := f.read("hm"); !reflect.DeepEqual(readyIDs(again), []string{"n1", "n2", "n3"}) || again.Pass.DecidingLimit != LimitNone {
				t.Fatalf("once the readings are back within their thresholds: ready %v, pass %+v", readyIDs(again), again.Pass)
			}
		})
	}
}

// c2: children already running are untouched; only the candidates are held.
func TestHostMemoryLeavesRunningChildrenAlone(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("hm", 0, "hm-r1", addNode("run", dag.NodeNonPR), addNode("next", dag.NodeNonPR))
	f.startNode("hm", "run")
	f.holdSlotsFor("hm", "run")
	f.sched.Host = hostBound(healthyHost())
	before := f.read("hm")
	f.sched.Host = hostBound(shortHosts[0].sample)
	short := f.read("hm")
	if !reflect.DeepEqual(before.node("run"), short.node("run")) || short.node("run").State != StateRunning {
		t.Fatalf("the running child changed: %+v then %+v", before.node("run"), short.node("run"))
	}
	if short.Pass.Held != before.Pass.Held || short.Pass.Held != 1 {
		t.Fatalf("held slots %d then %d, want the one slot kept", before.Pass.Held, short.Pass.Held)
	}
	if n := short.node("next"); n.Reason != DeferHostMemory {
		t.Fatalf("next = %+v, want it held", n)
	}
}

// A host that is short outranks a full ceiling in the reason, and a bound that is not there changes nothing: no key, no limit, the digest of a plan read without a host.
func TestHostMemoryPrecedenceAndAbsence(t *testing.T) {
	t.Run("a full ceiling and a short host: the host memory reason", func(t *testing.T) {
		f := newFixture(t)
		f.threeNodes("hm")
		f.holdSlots(6)
		f.sched.Host = hostBound(shortHosts[0].sample)
		short := f.read("hm")
		if n := short.node("n1"); n.Reason != DeferHostMemory || short.Pass.DecidingLimit != LimitHostMemory || short.Pass.FreeSlots != 0 {
			t.Fatalf("n1 = %+v pass = %+v", n, short.Pass)
		}
	})
	t.Run("no host: no host memory object and no host limit", func(t *testing.T) {
		f := newFixture(t)
		f.threeNodes("hm")
		reading := f.read("hm")
		if reading.Pass.HostMemory != nil || reading.Pass.DecidingLimit != LimitNone {
			t.Fatalf("pass = %+v", reading.Pass)
		}
		if _, present := asJSON(t, reading.Object())["pass"].(map[string]any)["host_memory"]; present {
			t.Fatal("a reading without a host prints host_memory")
		}
	})
	t.Run("nothing could be read: unmeasured, and nothing is held", func(t *testing.T) {
		f := newFixture(t)
		f.threeNodes("hm")
		f.sched.Host = &HostMemoryBound{Sample: ReadHostMemory(t.TempDir()), Limits: DefaultHostMemoryLimits(), LimitsFrom: LimitsDefault}
		reading := f.read("hm")
		if len(reading.Ready) != 3 || hostState(reading) != HostMemoryUnmeasured {
			t.Fatalf("ready %v, host %+v, want all three ready and the bound unmeasured", readyIDs(reading), reading.Pass.HostMemory)
		}
	})
}

// The digest says what the reading depended on, the host sample included.
func TestHostMemoryIsInTheInputDigest(t *testing.T) {
	f := newFixture(t)
	f.threeNodes("hm")
	f.sched.Host = hostBound(healthyHost())
	first, second := f.read("hm"), f.read("hm")
	if first.InputDigest != second.InputDigest {
		t.Fatal("one sample, two digests")
	}
	f.sched.Host = hostBound(fixedMemory(41<<30, 8<<30, 8<<30, 0))
	if other := f.read("hm"); other.InputDigest == first.InputDigest {
		t.Fatal("two samples with the same verdict and other values digest alike: the digest does not cover the sample")
	}
	f.sched.Host = hostBound(shortHosts[0].sample)
	if short := f.read("hm"); short.InputDigest == first.InputDigest {
		t.Fatal("a short host digests as a healthy one")
	}
	f.sched.Host = nil
	if bare := f.read("hm"); bare.InputDigest == first.InputDigest {
		t.Fatal("a reading without a host digests as one with a host")
	}
}

// dag-progress is a function of the store alone: a host that is short does not move a node out of the ready stage.
func TestHostMemoryDoesNotReachTheProgressProjection(t *testing.T) {
	f := newFixture(t)
	f.threeNodes("hm")
	f.sched.Host = hostBound(shortHosts[0].sample)
	progress, err := f.sched.ReadProgress(context.Background(), "hm")
	if err != nil {
		t.Fatal(err)
	}
	wantStages(t, progress, map[string][]string{StageReady: {"n1", "n2", "n3"}})
}

// c1 at the release: a short host refuses a new release with the existing capacity reason and leaves no row; the same call binds once the host recovers; a decided release is replayed whatever the host says.
func TestReleaseRefusedWhileHostMemoryIsShort(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.sched.Host = hostBound(shortHosts[0].sample)
	if n := k.read("rp").node("A"); n.Reason != DeferHostMemory {
		t.Fatalf("the reading says %+v", n)
	}
	_, err := k.release("rp", "A")
	if refusalReason(err) != "capacity_exhausted" || !strings.Contains(err.Error(), DeferHostMemory) || !strings.Contains(err.Error(), "available 9.00 GiB") {
		t.Fatalf("release = %v, want capacity_exhausted naming %s and the value", err, DeferHostMemory)
	}
	if rows := k.rows(); rows != (rowCounts{}) {
		t.Fatalf("a refused release left rows %+v", rows)
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatal("a child was created while the host was short")
	}

	k.sched.Host = hostBound(healthyHost())
	first := k.mustRelease("rp", "A")
	if !first.Bound {
		t.Fatalf("the release after the host recovered = %+v", first)
	}
	k.sched.Host = hostBound(shortHosts[0].sample)
	replay, err := k.release("rp", "A")
	if err != nil || !replay.Bound || replay.RequestID != first.RequestID {
		t.Fatalf("the replay of a decided release under a short host = %+v, %v", replay, err)
	}
	if created, _ := k.host.counts(); created != 1 {
		t.Fatalf("%d children, want the one", created)
	}
}

// openReadOnly is the store of a state directory, read the way an operator's sqlite3 would.
func openReadOnly(t *testing.T, state string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type hostRow struct{ state, limit, doc string }

func (f *fixture) hostRows(plan string) map[int64]hostRow {
	f.t.Helper()
	rows, err := f.s.DB.QueryContext(contextBackground(), "SELECT pass_seq, state, reading_limit, host_json FROM dag_pass_host_memory WHERE plan_id = ? ORDER BY pass_seq", plan)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]hostRow{}
	for rows.Next() {
		var seq int64
		var r hostRow
		if err := rows.Scan(&seq, &r.state, &r.limit, &r.doc); err != nil {
			f.t.Fatal(err)
		}
		out[seq] = r
	}
	return out
}

// c2: the decided bound is recorded with each recorded pass, the way the release policy is, and the measurements count the passes the host decided.
func TestPassRecordsTheHostMemoryBound(t *testing.T) {
	f := newFixture(t)
	f.threeNodes("hm")

	if _, seq := f.recordPass("hm"); seq != 1 || len(f.hostRows("hm")) != 0 {
		t.Fatalf("a pass without a host kept %v", f.hostRows("hm"))
	}
	f.sched.Host = hostBound(healthyHost())
	f.recordPass("hm")
	f.sched.Host = hostBound(shortHosts[1].sample)
	short, seq := f.recordPass("hm")
	if seq != 3 || short.Pass.DecidingLimit != LimitHostMemory {
		t.Fatalf("pass %d = %+v", seq, short.Pass)
	}

	rows := f.passRows("hm")
	if rows[0].limit != "none" || rows[1].limit != "none" || rows[2].limit != "no_capacity" {
		t.Fatalf("stored limits %q %q %q: dag_passes keeps its closed set, and a host memory cut is stored as the capacity cut it is", rows[0].limit, rows[1].limit, rows[2].limit)
	}
	kept := f.hostRows("hm")
	if len(kept) != 2 || kept[2].state != "within" || kept[2].limit != "none" || kept[3].state != "deferring" || kept[3].limit != "host_memory" {
		t.Fatalf("host rows = %+v", kept)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(kept[3].doc), &doc); err != nil {
		t.Fatal(err)
	}
	values := doc["measured"].(map[string]any)
	if doc["state"] != "deferring" || !reflect.DeepEqual(doc["exceeded"], []any{"swap"}) || values["swap_used_percent"] != float64(75) || values["swap_free_bytes"] != float64(2<<30) {
		t.Fatalf("the kept values = %v", doc)
	}
	if rows[2].dispositions == "" || !strings.Contains(rows[2].dispositions, DeferHostMemory) {
		t.Fatalf("the pass does not keep the reason: %s", rows[2].dispositions)
	}

	par := metric(t, wantPresent(t, measured(t, f, "hm"), 3, "parallelism"), "limited_by")
	if par["host_memory"] != float64(1) || par["no_capacity"] != float64(0) || par["none"] != float64(2) {
		t.Fatalf("limited by = %v, want the one pass the host decided apart from the capacity ones", par)
	}
}

// A read-only command never creates the zone, so a store that predates the side table is measured as it always was and is left as it is.
func TestMeasurementsReadAStoreWithoutTheHostMemoryTable(t *testing.T) {
	f := newFixture(t)
	f.threeNodes("hm")
	f.sched.Host = hostBound(shortHosts[0].sample)
	f.recordPass("hm")
	f.exec("DROP TABLE dag_pass_host_memory")
	objects := f.count("SELECT COUNT(*) FROM sqlite_master")
	par := metric(t, wantPresent(t, measured(t, f, "hm"), 1, "parallelism"), "limited_by")
	if _, present := par["host_memory"]; present || len(par) != 4 || par["no_capacity"] != float64(1) {
		t.Fatalf("limited by = %v, want the four stored limits as dag_passes keeps them", par)
	}
	if f.count("SELECT COUNT(*) FROM sqlite_master") != objects {
		t.Fatal("a measurement created a table")
	}
}

// The commands: the built binary reads the host through the environment. A fake proc root stands for the host.
func TestCLIReadyAndRecordUnderAFakeHost(t *testing.T) {
	state, _ := cliState(t)
	t.Setenv(EnvHostProcRoot, fakeProc(t, memInfo(9*kibPerGiB, 8*kibPerGiB, 8*kibPerGiB), psi("0.00")))
	out, code := crw(t, state, "dag-ready", "--plan", "p1")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	reading := parseOut(t, out)
	host, _ := reading["pass"].(map[string]any)["host_memory"].(map[string]any)
	if host["state"] != "deferring" || reading["pass"].(map[string]any)["deciding_limit"] != LimitHostMemory || len(reading["ready"].([]any)) != 0 {
		t.Fatalf("a short fake host:\n%s", out)
	}
	for _, n := range reading["nodes"].([]any) {
		if m := n.(map[string]any); m["node_id"] == "design" && m["reason"] != DeferHostMemory {
			t.Fatalf("design = %v", m)
		}
	}

	out, code = crw(t, state, "dag-ready", "--plan", "p1", "--record", "--actor", "parent")
	if code != 0 {
		t.Fatalf("record: exit %d\n%s", code, out)
	}
	db := openReadOnly(t, state)
	var kept, limit string
	if err := db.QueryRow("SELECT state, reading_limit FROM dag_pass_host_memory WHERE plan_id = 'p1'").Scan(&kept, &limit); err != nil || kept != "deferring" || limit != "host_memory" {
		t.Fatalf("kept %q %q %v", kept, limit, err)
	}

	t.Setenv(EnvHostProcRoot, fakeProc(t, memInfo(40*kibPerGiB, 8*kibPerGiB, 8*kibPerGiB), psi("0.00")))
	out, _ = crw(t, state, "dag-ready", "--plan", "p1")
	if ready := parseOut(t, out)["ready"].([]any); len(ready) != 1 || ready[0].(map[string]any)["node_id"] != "design" {
		t.Fatalf("a healthy fake host:\n%s", out)
	}
}

// A threshold that is not usable is a usage error of the two commands that read the host, and of no other.
func TestCLIInvalidHostThresholdIsAUsageError(t *testing.T) {
	state, _ := cliState(t)
	t.Setenv(EnvHostMinAvailableGiB, "plenty")
	out, code := crw(t, state, "dag-ready", "--plan", "p1")
	if code != 4 || !strings.Contains(out, EnvHostMinAvailableGiB) {
		t.Fatalf("dag-ready with a bad threshold: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{{"dag-progress", "--plan", "p1"}, {"dag-restart", "--plan", "p1", "--actor", "parent"}} {
		if out, code := crw(t, state, args...); code == 4 {
			t.Fatalf("%v read the host environment: exit %d\n%s", args, code, out)
		}
	}
}
