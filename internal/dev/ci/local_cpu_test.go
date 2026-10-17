//go:build dev

package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
)

// CRW-1591: one verification's CPU budget. The helpers run inside this test binary (the
// CRW_TEST_CPU_HELPER dispatch in TestMain), so a step observes its real process environment.

// crw1591TestCommand is the helper entry the test binary re-execs as: "probe" writes a receipt of
// its real process environment for a step that runs it, "verify" runs the real
// localRunWithCPU over a small injected plan.
func crw1591TestCommand() int {
	switch os.Getenv("CRW_TEST_CPU_HELPER") {
	case "probe":
		return crw1591Probe()
	case "verify":
		return crw1591Verify()
	}
	return 2
}

// crw1591Verify is the verifier helper: argv is <binary> <root> <record> <workroot> <cpus>
// <jobs> <receipts>. It builds a two-job plan whose steps run the probe and calls the real
// localRunWithCPU with its own argv, so a scope re-entry reconstructs identical options. The
// verify marker is sealed from the resolved method; the probe mode reaches a step only
// through opts.Env.
func crw1591Verify() int {
	if len(os.Args) != 7 {
		return 2
	}
	cpus, err := strconv.Atoi(os.Args[4])
	if err != nil {
		return 2
	}
	jobs, err := strconv.Atoi(os.Args[5])
	if err != nil {
		return 2
	}
	receipts := os.Args[6]
	probe := func(job string) localStep {
		return localStep{name: "probe " + job, kind: localRun, scope: "full", heavy: true,
			command: `"$CRW_CPU_TEST_BIN"`,
			env:     []string{"CRW_CPU_RECEIPT=" + filepath.Join(receipts, job+".json")}}
	}
	plan := []localJob{
		{name: "cpu-a", steps: []localStep{probe("cpu-a")}},
		{name: "cpu-b", steps: []localStep{probe("cpu-b")}},
	}
	// CRW_CPU_FAIL_MARK adds a step that appends once then exits 17, so a run that executed the
	// plan twice is visible in the marker.
	if mark := os.Getenv("CRW_CPU_FAIL_MARK"); mark != "" {
		plan[0].steps = append(plan[0].steps, localStep{name: "fail", kind: localRun, scope: "full",
			command: "echo run >> '" + mark + "'; exit 17"})
	}
	opts := localOptions{
		Root:        os.Args[1],
		Commit:      "HEAD",
		Record:      os.Args[2],
		Runner:      "local",
		Plan:        plan,
		WorkRoot:    os.Args[3],
		Jobs:        jobs,
		CPUs:        cpus,
		HeavyGate:   os.Getenv("CRW_CPU_TEST_HEAVY_GATE"),
		gitCeilings: []string{localTestTempDir()},
		Env:         []string{"CRW_TEST_CPU_HELPER=probe", "CRW_CPU_TEST_BIN=" + os.Args[0]},
	}
	return localRunWithCPU(opts, os.Args, "", os.Stdout, os.Stderr)
}

// crw1591RunVerify launches this test binary as the verify helper over repo. pathEnv replaces
// PATH (empty: a fake Node 24.20.0 ahead of the host's), extraEnv sets named variables.
func crw1591RunVerify(t *testing.T, repo *localFixture, cpus, jobs, receipts, record, pathEnv string, extraEnv []string) result {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if pathEnv == "" {
		pathEnv = fakeNodeBin(t, "24.20.0")
	}
	env := append(os.Environ(), "CRW_TEST_CPU_HELPER=verify", "CRW_CPU_TEST_BIN="+self)
	// This is an independent verification, even when the test suite is a scoped step.
	// Only its own launcher may mark the helper as having entered its CPU scope.
	env = crwconfig.SetEnv(env, localCPUEnteredEnv, "")
	env = crwconfig.SetEnv(env, "PATH", pathEnv)
	for _, kv := range extraEnv {
		name, value, _ := strings.Cut(kv, "=")
		env = crwconfig.SetEnv(env, name, value)
	}
	return runEnv(t, repo.root, env, self, repo.root, record, repo.work, cpus, jobs, receipts)
}

// crw1591QuotaCores parses a probe's cpu.max receipt into cores; a "max" or malformed value is a
// test failure, not a quota.
func crw1591QuotaCores(t *testing.T, cpuMax any) float64 {
	t.Helper()
	fields := strings.Fields(fmt.Sprintf("%v", cpuMax))
	if len(fields) != 2 || fields[0] == "max" {
		t.Fatalf("cpu.max %v is not a finite quota", cpuMax)
	}
	quota, err1 := strconv.ParseInt(fields[0], 10, 64)
	period, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil || quota <= 0 || period <= 0 {
		t.Fatalf("cpu.max %v carries no positive quota/period", cpuMax)
	}
	return float64(quota) / float64(period)
}

// crw1591FakeTool writes an executable named name with body in a directory of its own.
func crw1591FakeTool(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// crw1591Probe records the environment the sealed step actually gave it: the runtime's
// GOMAXPROCS (read, never set), GOFLAGS/GOMAXPROCS/GODEBUG as inherited variables, and the
// unified cgroup and its cpu.max when readable. The receipt goes to CRW_CPU_RECEIPT.
func crw1591Probe() int {
	receipt := map[string]any{
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"numCPU":     runtime.NumCPU(),
		"GOFLAGS":    os.Getenv("GOFLAGS"),
		"GOMAXPROCS": os.Getenv("GOMAXPROCS"),
		"GODEBUG":    os.Getenv("GODEBUG"),
		"cpuProcs":   os.Getenv("CRW_CI_CPU_PROCS"),
	}
	group := ""
	if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "0::") {
				group = strings.TrimPrefix(line, "0::")
			}
		}
	}
	receipt["cgroup"] = group
	if group != "" && !strings.Contains(group, "..") {
		if max, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", filepath.FromSlash(group), "cpu.max")); err == nil {
			receipt["cpuMax"] = strings.TrimSpace(string(max))
		}
	}
	out, err := os.Create(os.Getenv("CRW_CPU_RECEIPT"))
	if err != nil {
		return 2
	}
	defer out.Close()
	if err := json.NewEncoder(out).Encode(receipt); err != nil {
		return 2
	}
	return 0
}

// crw1591Receipt reads a probe's receipt file.
func crw1591Receipt(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// The command takes --cpus and its usage describes it. An unknown flag on the baseline exits 2.
func TestCRW1591CPUFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Local([]string{"--cpus", "4", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Local --cpus 4 --help exits %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "cpus") {
		t.Errorf("the usage does not describe --cpus: %s", stdout.String())
	}
}

// A budgeted run seals the Go limit into the step's process and names the budget in the record.
func TestCRW1591FallbackProcessAndRecord(t *testing.T) {
	t.Setenv("GOMAXPROCS", "17")
	repo := newLocalFixture(t)
	receipt := filepath.Join(t.TempDir(), "probe.json")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := localRunOptions(repo, localFixturePlan(`"$CRW_CPU_TEST_BIN"`), filepath.Join(t.TempDir(), "record.json"))
	opts.Env = []string{
		"PATH=" + fakeNodeBin(t, "24.20.0"),
		"CRW_TEST_CPU_HELPER=probe",
		"CRW_CPU_RECEIPT=" + receipt,
		"CRW_CPU_TEST_BIN=" + self,
	}
	opts.CPUs = 4
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %+v", made.Result, made.Jobs)
	}
	probe := crw1591Receipt(t, receipt)
	if got := probe["gomaxprocs"]; got != float64(4) {
		t.Errorf("the step's GOMAXPROCS = %v, want 4 (receipt %v)", got, probe)
	}
	if got := probe["GOFLAGS"]; got != "-p=1" {
		t.Errorf("the step's GOFLAGS = %v, want -p=1", got)
	}
	raw, err := json.Marshal(made)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		CPU struct {
			Limit  int    `json:"limit"`
			Method string `json:"method"`
		} `json:"cpu"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.CPU.Limit != 4 || fields.CPU.Method != "gomaxprocs" {
		t.Errorf("record cpu = %+v, want limit 4 method gomaxprocs", fields.CPU)
	}
}

// The budget comes from --cpus, else CRW_CI_CPUS, else the default 4; an explicit flag wins and
// every resolved value is positive.
func TestCRW1591CPUOption(t *testing.T) {
	for name, test := range map[string]struct {
		flag     int
		explicit bool
		env      string
		want     int
		wantErr  bool
	}{
		"absent":                 {0, false, "", 4, false},
		"env eight":              {0, false, "8", 8, false},
		"explicit beats env":     {4, true, "8", 4, false},
		"explicit beats bad env": {4, true, "bogus", 4, false},
		"bad env":                {0, false, "bogus", 0, true},
		"env zero":               {0, false, "0", 0, true},
		"explicit zero":          {0, true, "", 0, true},
		"explicit negative":      {-2, true, "", 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := localCPUOption(test.flag, test.explicit, test.env)
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, test.wantErr)
			}
			if err == nil && got != test.want {
				t.Errorf("cpus=%d, want %d", got, test.want)
			}
			if err != nil && !strings.Contains(err.Error(), "cpus") && !strings.Contains(err.Error(), localCPUEnvName) {
				t.Errorf("error %q names neither cpus nor %s", err, localCPUEnvName)
			}
		})
	}
}

// The budget divides into jobs and per-job parallelism: jobs never exceed the budget, and the
// fallback's GOMAXPROCS is the per-job share.
func TestCRW1591BudgetDistribution(t *testing.T) {
	for name, test := range map[string]struct {
		cpus, jobs, parallel           int
		wantJobs, wantParallel, wantGM int
	}{
		"default four":       {4, 0, 0, 1, 1, 4},
		"eight two jobs":     {8, 0, 4, 2, 1, 4},
		"explicit jobs":      {4, 2, 4, 2, 1, 2},
		"jobs clamp to cpus": {4, 9, 4, 4, 1, 1},
		"wide parallel":      {4, 0, 8, 1, 1, 4},
		"eight p1":           {8, 2, 1, 2, 1, 4},
		"five two jobs":      {5, 2, 2, 2, 1, 2},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := localCPUOptions(localOptions{CPUs: test.cpus, Jobs: test.jobs, Parallel: test.parallel})
			if err != nil {
				t.Fatal(err)
			}
			if got.Jobs != test.wantJobs || got.Parallel != test.wantParallel {
				t.Errorf("jobs/parallel = %d/%d, want %d/%d", got.Jobs, got.Parallel, test.wantJobs, test.wantParallel)
			}
			want := recordCPU{Limit: test.cpus, Method: localCPUFallback, Jobs: test.wantJobs,
				Parallel: test.wantParallel, GOMAXPROCS: test.wantGM}
			if got.cpu == nil {
				t.Fatal("resolved options carry no cpu record")
			}
			if *got.cpu != want {
				t.Errorf("cpu = %+v, want %+v", got.cpu, want)
			}
			if product := got.Jobs * got.Parallel * got.cpu.GOMAXPROCS; product > test.cpus {
				t.Errorf("jobs*parallel*GOMAXPROCS = %d exceeds budget %d", product, test.cpus)
			}
		})
	}
	for name, opts := range map[string]localOptions{
		"negative cpus": {CPUs: -1},
		"negative jobs": {CPUs: 4, Jobs: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := localCPUOptions(opts); err == nil {
				t.Error("a negative value must be refused before execution")
			}
		})
	}
}

// A real helper verification under cpus=4, at one job and at two parallel jobs: where a user
// manager answers the scope's systemd-run the record's method is systemd-cgroup-v2 and all
// probes sit in the one shared scope with a finite cpu.max of at most four cores; where it does
// not the method is gomaxprocs and the sealed environment carries the per-job GOMAXPROCS share.
// Either way the recorded method agrees with the kernel receipts, and the jobs share one budget.
func TestCRW1591HostScopeOrFallback(t *testing.T) {
	for _, jobs := range []int{1, 2} {
		t.Run(fmt.Sprintf("jobs%d", jobs), func(t *testing.T) {
			repo := newLocalFixture(t)
			receipts := t.TempDir()
			record := filepath.Join(t.TempDir(), "record.json")
			res := crw1591RunVerify(t, repo, "4", strconv.Itoa(jobs), receipts, record, "", nil)
			if res.code != 0 {
				t.Fatalf("the verify helper exits %d: %s\n%s", res.code, res.stderr, res.stdout)
			}
			made, err := readRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			if made.CPU == nil {
				t.Fatal("the record carries no CPU budget")
			}
			a := crw1591Receipt(t, filepath.Join(receipts, "cpu-a.json"))
			b := crw1591Receipt(t, filepath.Join(receipts, "cpu-b.json"))
			t.Logf("record cpu: %+v", *made.CPU)
			t.Logf("probe cpu-a: %v", a)
			t.Logf("probe cpu-b: %v", b)
			wantParallel := 4 / jobs
			if made.CPU.Method == localCPUFallback {
				wantParallel = 1
			}
			if made.CPU.Limit != 4 || made.CPU.Jobs != jobs || made.CPU.Parallel != wantParallel {
				t.Errorf("record cpu = %+v, want limit 4 jobs %d parallel %d", *made.CPU, jobs, wantParallel)
			}
			scoped := strings.Contains(fmt.Sprintf("%v", a["cgroup"]), "crw-ci-cpu-")
			switch made.CPU.Method {
			case localCPUCgroup:
				if !scoped {
					t.Errorf("method %s but the probe ran in cgroup %v", made.CPU.Method, a["cgroup"])
				}
				if a["cgroup"] != b["cgroup"] {
					t.Errorf("the two jobs ran in different groups %v / %v", a["cgroup"], b["cgroup"])
				}
				for _, probe := range []map[string]any{a, b} {
					if want := min(4, probe["numCPU"].(float64)); probe["gomaxprocs"] != want {
						t.Errorf("scoped probe GOMAXPROCS = %v, want %v (%v)", probe["gomaxprocs"], want, probe)
					}
					if cores := crw1591QuotaCores(t, probe["cpuMax"]); cores > 4 {
						t.Errorf("scoped cpu.max %v = %v cores, want <= 4", probe["cpuMax"], cores)
					}
				}
			case localCPUFallback:
				if scoped {
					t.Errorf("method gomaxprocs but the probe ran inside scope %v", a["cgroup"])
				}
				for _, probe := range []map[string]any{a, b} {
					if want := 4 / jobs; probe["GOMAXPROCS"] != strconv.Itoa(want) || probe["gomaxprocs"] != float64(want) {
						t.Errorf("fallback probe env/limit = %v/%v, want %d (%v)", probe["GOMAXPROCS"], probe["gomaxprocs"], want, probe)
					}
				}
			default:
				t.Errorf("record method %q is neither %s nor %s", made.CPU.Method, localCPUCgroup, localCPUFallback)
			}
		})
	}
}

// A launcher that never starts the child (no user manager, systemd-run exits 1) is a fallback,
// not a failure: the verification still runs once, under per-job Go limits.
func TestCRW1591UnavailableScopeFallsBack(t *testing.T) {
	// The suite itself can run inside the default four-CPU verifier scope.
	t.Setenv(localCPUEnteredEnv, "4")
	repo := newLocalFixture(t)
	receipts := t.TempDir()
	record := filepath.Join(t.TempDir(), "record.json")
	fake := crw1591FakeTool(t, "systemd-run", "#!/bin/sh\nexit 1\n")
	pathEnv := fake + ":" + fakeNodeBin(t, "24.20.0")
	res := crw1591RunVerify(t, repo, "4", "2", receipts, record, pathEnv, nil)
	if res.code != 0 {
		t.Fatalf("the verify helper exits %d: %s\n%s", res.code, res.stderr, res.stdout)
	}
	if !strings.Contains(res.stdout, "using per-job Go limits") {
		t.Errorf("the fallback was not announced: %q", res.stdout)
	}
	made, err := readRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	want := recordCPU{Limit: 4, Method: localCPUFallback, Jobs: 2, Parallel: 1, GOMAXPROCS: 2}
	if made.CPU == nil || *made.CPU != want {
		t.Fatalf("record cpu = %+v, want %+v", made.CPU, want)
	}
	for _, job := range []string{"cpu-a", "cpu-b"} {
		probe := crw1591Receipt(t, filepath.Join(receipts, job+".json"))
		if probe["GOMAXPROCS"] != "2" || probe["gomaxprocs"] != float64(2) {
			t.Errorf("%s probe env/limit = %v/%v, want 2 (%v)", job, probe["GOMAXPROCS"], probe["gomaxprocs"], probe)
		}
	}
}

// Once the child verifier entered, a failed verification is the answer — it is never rerun. The
// fake launcher execs the entry script so the entered marker exists; the marker file's one append
// proves the plan ran exactly once.
func TestCRW1591EnteredChildFailureIsNotRetried(t *testing.T) {
	repo := newLocalFixture(t)
	receipts := t.TempDir()
	record := filepath.Join(t.TempDir(), "record.json")
	mark := filepath.Join(t.TempDir(), "runs")
	fake := crw1591FakeTool(t, "systemd-run", "#!/bin/sh\nwhile [ $# -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\n[ $# -gt 0 ] && shift\nexec \"$@\"\n")
	pathEnv := fake + ":" + fakeNodeBin(t, "24.20.0")
	res := crw1591RunVerify(t, repo, "4", "1", receipts, record, pathEnv, []string{"CRW_CPU_FAIL_MARK=" + mark})
	if res.code == 0 {
		t.Fatalf("a failed child verification exits %d, want nonzero: %s", res.code, res.stdout)
	}
	made, err := readRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("record result = %q, want %q", made.Result, localFail)
	}
	if n := localMarkerLines(t, mark); n != 1 {
		t.Errorf("the failing step ran %d times, want exactly 1 (no fallback rerun)", n)
	}
}

// Neither opts.Env nor a step's own env can widen the budget: GOMAXPROCS/GOFLAGS/GODEBUG
// injections are sealed to the resolved values.
func TestCRW1591CPUSettingsAreSealed(t *testing.T) {
	repo := newLocalFixture(t)
	receipt := filepath.Join(t.TempDir(), "probe.json")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	plan := []localJob{{name: "validate", steps: []localStep{
		{kind: localAction, action: localCheckout, scope: "full"},
		{name: "probe", kind: localRun, scope: "full", command: `"$CRW_CPU_TEST_BIN"`,
			env: []string{"GOMAXPROCS=99", "GOFLAGS=-p=99", "GODEBUG=containermaxprocs=0",
				"CRW_CI_CPU_PROCS=/dev/null"}},
	}}}
	opts := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "record.json"))
	opts.CPUs = 4
	opts.Env = []string{
		"PATH=" + fakeNodeBin(t, "24.20.0"),
		"CRW_TEST_CPU_HELPER=probe",
		"CRW_CPU_RECEIPT=" + receipt,
		"CRW_CPU_TEST_BIN=" + self,
		"GOMAXPROCS=99", "GOFLAGS=-p=99", "GODEBUG=containermaxprocs=0",
		"CRW_CI_CPU_PROCS=/dev/null",
	}
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %+v", made.Result, made.Jobs)
	}
	probe := crw1591Receipt(t, receipt)
	if probe["gomaxprocs"] != float64(4) || probe["GOMAXPROCS"] != "4" {
		t.Errorf("sealed GOMAXPROCS = %v/%v, want 4 (%v)", probe["gomaxprocs"], probe["GOMAXPROCS"], probe)
	}
	if probe["cpuProcs"] != "" {
		t.Errorf("fallback step leaked CRW_CI_CPU_PROCS = %v, want unset", probe["cpuProcs"])
	}
	if probe["GOFLAGS"] != "-p=1" {
		t.Errorf("sealed GOFLAGS = %v, want -p=1", probe["GOFLAGS"])
	}
	if probe["GODEBUG"] != "" {
		t.Errorf("sealed GODEBUG = %v, want unset", probe["GODEBUG"])
	}
}

// The budget is a reuse key: a record's cpu survives sealing, the same budget reuses, a changed
// limit/method/jobs split or a legacy nil record answers nothing, and different budgets pick
// different cache directories.
func TestCRW1591CPUReuseKeys(t *testing.T) {
	repo := newLocalFixture(t)
	mark := filepath.Join(t.TempDir(), "mark")
	plan := localFixturePlan("echo run >> '" + mark + "'")
	recordPath := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, plan, recordPath)
	opts.CPUs = 4
	opts.Env = []string{"PATH=" + fakeNodeBin(t, "24.20.0")}
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localPass {
		t.Fatalf("the run is %q: %+v", made.Result, made.Jobs)
	}
	if made.CPU == nil || made.CPU.Limit != 4 || made.CPU.Method != localCPUFallback {
		t.Fatalf("record cpu = %+v, want limit 4 method %s", made.CPU, localCPUFallback)
	}
	crw1590Write(t, recordPath, made)
	back, err := readRecord(recordPath)
	if err != nil {
		t.Fatalf("the sealed record does not validate: %v", err)
	}
	if back.CPU == nil || *back.CPU != *made.CPU {
		t.Fatalf("the record's cpu did not survive the digest: %+v vs %+v", back.CPU, made.CPU)
	}
	current4, err := localCurrentKeys(opts)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := localTreeReuse(back, current4); !ok {
		t.Errorf("the same budget does not reuse: %s", why)
	}
	// The same options answer from the sealed record without running the step again.
	optsAgain := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "again.json"))
	optsAgain.CPUs = 4
	optsAgain.Env = opts.Env
	got, reused, err := localVerify(optsAgain, recordPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || localMarkerLines(t, mark) != 1 {
		t.Errorf("same-budget run: reused=%t executions=%d, want reused with 1", reused, localMarkerLines(t, mark))
	}
	if got.CPU == nil || *got.CPU != *made.CPU {
		t.Errorf("the reused record's cpu = %+v, want %+v", got.CPU, made.CPU)
	}
	// A different budget reruns the step and selects a different cache directory.
	opts8 := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "eight.json"))
	opts8.CPUs = 8
	opts8.Env = opts.Env
	made8, reused8, err := localVerify(opts8, recordPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if reused8 || made8.Result != localPass || localMarkerLines(t, mark) != 2 {
		t.Errorf("cpus=8 reused=%t result=%s executions=%d, want a fresh pass", reused8, made8.Result, localMarkerLines(t, mark))
	}
	if made8.CPU == nil || made8.CPU.Limit != 8 {
		t.Errorf("the cpus=8 record's cpu = %+v", made8.CPU)
	}
	current8, err := localCurrentKeys(opts8)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := localTreeReuse(back, current8); ok || !strings.Contains(why, "CPU budget") {
		t.Errorf("cpus=8 reuse = %t (%s), want the budget refusal", ok, why)
	}
	dir4, err := localCacheDirectory(opts, current4)
	if err != nil {
		t.Fatal(err)
	}
	dir8, err := localCacheDirectory(opts8, current8)
	if err != nil {
		t.Fatal(err)
	}
	if dir4 == dir8 {
		t.Errorf("different budgets select the same cache directory %s", dir4)
	}
	// A changed method or job split is a different budget too, as is a legacy nil record.
	otherMethod := back
	changed := *back.CPU
	changed.Method = localCPUCgroup
	otherMethod.CPU = &changed
	if ok, _ := localTreeReuse(otherMethod, current4); ok {
		t.Error("a cgroup-method record answers a gomaxprocs run")
	}
	otherJobs := back
	split := *back.CPU
	split.Jobs, split.Parallel, split.GOMAXPROCS = 2, 2, 1
	otherJobs.CPU = &split
	if ok, _ := localTreeReuse(otherJobs, current4); ok {
		t.Error("a different job split answers this budget")
	}
	legacy := back
	legacy.CPU = nil
	if ok, _ := localTreeReuse(legacy, current4); ok {
		t.Error("a legacy record without cpu answers a budgeted run")
	}
	opts0 := localRunOptions(repo, plan, filepath.Join(t.TempDir(), "zero.json"))
	opts0.Env = opts.Env
	current0, err := localCurrentKeys(opts0)
	if err != nil {
		t.Fatal(err)
	}
	if current0.CPU != nil {
		t.Error("an unbudgeted run's keys carry a cpu member")
	}
	if ok, _ := localTreeReuse(back, current0); ok {
		t.Error("a budgeted record answers an unbudgeted run")
	}
}

// CRW-1601: a heavy gate that makes its own systemd scope must not move a step out of the
// verification's shared budget. The fake gate below really launches an unbounded sibling scope
// when it finds the verification's crw-ci-cpu- group, and writes the group it moved to beside
// each probe receipt; the step body's own cgroup after the gate is what is judged.
func TestCRW1601HeavyGateRemainsInBudget(t *testing.T) {
	crw1621HeavyGateRemainsInBudget(t, false)
}

func TestCRW1621ScopedHeavyGateRemainsInBudget(t *testing.T) {
	crw1621HeavyGateRemainsInBudget(t, true)
}

func crw1621HeavyGateRemainsInBudget(t *testing.T, scopeAware bool) {
	t.Helper()
	repo := newLocalFixture(t)
	receipts := t.TempDir()
	record := filepath.Join(t.TempDir(), "record.json")
	stayInScope := ""
	if scopeAware {
		stayInScope = `if [ -n "${CRW_CI_CPU_SCOPED:-}" ]; then
  exec "$0" --inside "$@"
fi
`
	}
	gate := crw1591FakeTool(t, "crw-1601-gate", `#!/bin/sh
if [ "$1" = "--inside" ]; then
  shift
  if [ -n "${CRW_CPU_RECEIPT:-}" ]; then
    sed -n 's/^0:://p' /proc/self/cgroup > "${CRW_CPU_RECEIPT}.gate"
  fi
  exec "$@"
fi
`+stayInScope+`if grep -q crw-ci-cpu- /proc/self/cgroup; then
  exec systemd-run --user --scope --quiet --expand-environment=no -- "$0" --inside "$@"
fi
exec "$0" --inside "$@"
`)
	res := crw1591RunVerify(t, repo, "4", "2", receipts, record, "",
		[]string{"CRW_CPU_TEST_HEAVY_GATE=" + filepath.Join(gate, "crw-1601-gate")})
	if res.code != 0 {
		t.Fatalf("the verify helper exits %d: %s\n%s", res.code, res.stderr, res.stdout)
	}
	made, err := readRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if made.CPU == nil {
		t.Fatal("the record carries no CPU budget")
	}
	a := crw1591Receipt(t, filepath.Join(receipts, "cpu-a.json"))
	b := crw1591Receipt(t, filepath.Join(receipts, "cpu-b.json"))
	gateGroup := func(job string) string {
		data, err := os.ReadFile(filepath.Join(receipts, job+".json.gate"))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	ga, gb := gateGroup("cpu-a"), gateGroup("cpu-b")
	t.Logf("record cpu: %+v; gate groups %q %q", *made.CPU, ga, gb)
	t.Logf("probe cpu-a: %v", a)
	t.Logf("probe cpu-b: %v", b)
	switch made.CPU.Method {
	case localCPUCgroup:
		for _, probe := range []map[string]any{a, b} {
			group := fmt.Sprintf("%v", probe["cgroup"])
			if !strings.Contains(group, "crw-ci-cpu-") {
				t.Errorf("the gated step ran in %q, outside the verification scope", group)
			}
			if cores := crw1591QuotaCores(t, probe["cpuMax"]); cores > 4 {
				t.Errorf("gated cpu.max %v = %v cores, want <= 4", probe["cpuMax"], cores)
			}
			if want := min(4, probe["numCPU"].(float64)); probe["gomaxprocs"] != want {
				t.Errorf("gated probe GOMAXPROCS = %v, want %v (%v)", probe["gomaxprocs"], want, probe)
			}
		}
		if a["cgroup"] != b["cgroup"] {
			t.Errorf("the two gated jobs ran in different groups %v / %v", a["cgroup"], b["cgroup"])
		}
		for i, g := range []string{ga, gb} {
			same := g == []map[string]any{a, b}[i]["cgroup"]
			if g == "" || same != scopeAware {
				t.Errorf("gate group %q, body group %v: want same=%t", g, []map[string]any{a, b}[i]["cgroup"], scopeAware)
			}
		}
	case localCPUFallback:
		want := made.CPU.GOMAXPROCS
		if want <= 0 || made.CPU.Jobs*made.CPU.Parallel*want > made.CPU.Limit {
			t.Errorf("fallback product %d*%d*%d exceeds limit %d", made.CPU.Jobs, made.CPU.Parallel, want, made.CPU.Limit)
		}
		for _, probe := range []map[string]any{a, b} {
			if probe["GOMAXPROCS"] != strconv.Itoa(want) || probe["gomaxprocs"] != float64(want) {
				t.Errorf("fallback probe %v does not match record gomaxprocs %d", probe, want)
			}
		}
	default:
		t.Errorf("method %q is neither %s nor %s", made.CPU.Method, localCPUCgroup, localCPUFallback)
	}
}

// CRW-1601/1621: the fallback caps concurrent Go packages at the job's GOMAXPROCS share, so
// jobs × parallel × GOMAXPROCS stays inside the budget — observed with real step processes.
func TestCRW1601FallbackPackageBudget(t *testing.T) {
	for name, tc := range map[string]struct{ cpus, jobs, parallel, wantGM int }{
		"four one job":   {4, 1, 4, 4},
		"eight two jobs": {8, 2, 2, 4},
	} {
		t.Run(name, func(t *testing.T) {
			repo := newLocalFixture(t)
			receipt := filepath.Join(t.TempDir(), "probe.json")
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			opts := localRunOptions(repo, localFixturePlan(`"$CRW_CPU_TEST_BIN"`), filepath.Join(t.TempDir(), "record.json"))
			opts.CPUs, opts.Jobs, opts.Parallel = tc.cpus, tc.jobs, tc.parallel
			opts.Env = []string{"PATH=" + fakeNodeBin(t, "24.20.0"), "CRW_TEST_CPU_HELPER=probe",
				"CRW_CPU_RECEIPT=" + receipt, "CRW_CPU_TEST_BIN=" + self}
			made, _, err := localVerify(opts, "", io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if made.Result != localPass {
				t.Fatalf("the run is %q: %+v", made.Result, made.Jobs)
			}
			probe := crw1591Receipt(t, receipt)
			if probe["gomaxprocs"] != float64(tc.wantGM) {
				t.Errorf("GOMAXPROCS = %v, want %d", probe["gomaxprocs"], tc.wantGM)
			}
			if probe["GOFLAGS"] != "-p=1" {
				t.Errorf("GOFLAGS = %v, want -p=1", probe["GOFLAGS"])
			}
			if made.CPU == nil {
				t.Fatal("the record carries no CPU budget")
			}
			if made.CPU.GOMAXPROCS != tc.wantGM {
				t.Errorf("record cpu = %+v, want gomaxprocs %d", made.CPU, tc.wantGM)
			}
			observed := int(probe["gomaxprocs"].(float64))
			if product := made.CPU.Jobs * made.CPU.Parallel * observed; product > made.CPU.Limit {
				t.Errorf("jobs*parallel*GOMAXPROCS = %d exceeds limit %d", product, made.CPU.Limit)
			}
		})
	}
}

// CRW-1601: a gate that cannot bring its step back into the verification's scope refuses the
// step before its body — a WARNING, a failed record, method systemd-cgroup-v2-gate-unavailable,
// and the body never ran. The seam is deterministic: the named group does not exist, so the
// guard's own-PID write fails without needing real systemd.
func TestCRW1601UnjoinableGateRefuses(t *testing.T) {
	repo := newLocalFixture(t)
	sentinel := filepath.Join(t.TempDir(), "body-ran")
	gate := crw1591FakeTool(t, "crw-1601-pass", "#!/bin/sh\nexec \"$@\"\n")
	recordPath := filepath.Join(t.TempDir(), "record.json")
	plan := []localJob{{name: "validate", steps: []localStep{
		{kind: localAction, action: localCheckout, scope: "full"},
		{name: "sentinel", kind: localRun, scope: "full", heavy: true,
			command: "echo ran > '" + sentinel + "'"},
	}}}
	opts := localRunOptions(repo, plan, recordPath)
	opts.CPUs = 4
	opts.cpuMethod = localCPUCgroup
	opts.cpuGroup = "/crw-1601-missing"
	opts.HeavyGate = filepath.Join(gate, "crw-1601-pass")
	opts.Env = []string{"PATH=" + fakeNodeBin(t, "24.20.0")}
	var stderr bytes.Buffer
	code := localRunAndReport(opts, "", io.Discard, &stderr)
	if code == 0 {
		t.Fatalf("a step that cannot rejoin exits %d, want nonzero", code)
	}
	made, err := readRecord(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("record result = %q, want %q", made.Result, localFail)
	}
	if made.CPU == nil || made.CPU.Method != localCPUGateUnavailable {
		t.Errorf("record cpu = %+v, want method %s", made.CPU, localCPUGateUnavailable)
	}
	if !strings.Contains(stderr.String(), "WARNING") || !strings.Contains(stderr.String(), localCPUGateFailure) {
		t.Errorf("stderr lacks the gate warning: %q", stderr.String())
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Errorf("the refused body ran: %v exists", sentinel)
	}
}

// CRW-1601: the gate-unavailable method classifies only a cgroup run whose failed step printed
// the marker — an ordinary fallback step that happens to print the same warning text keeps its
// honest gomaxprocs method.
func TestCRW1601FallbackFailureKeepsMethod(t *testing.T) {
	repo := newLocalFixture(t)
	recordPath := filepath.Join(t.TempDir(), "record.json")
	opts := localRunOptions(repo, localFixturePlan("printf '%s\\n' '"+localCPUGateFailure+"'; exit 1"), recordPath)
	opts.CPUs = 4
	opts.Env = []string{"PATH=" + fakeNodeBin(t, "24.20.0")}
	made, _, err := localVerify(opts, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Fatalf("the run is %q, want %q", made.Result, localFail)
	}
	if made.CPU == nil || made.CPU.Method != localCPUFallback {
		t.Errorf("record cpu = %+v, want method %s", made.CPU, localCPUFallback)
	}
}

// A scope-aware heavy gate still acquires its slot, but must not launch a sibling
// systemd scope when the verifier already owns the shared CPU scope. The fake
// launcher records actual calls; no host user manager or writable cgroup is needed.
func TestCRW1621HeavyGateUsesOuterScope(t *testing.T) {
	for _, method := range []string{localCPUCgroup, localCPUFallback} {
		t.Run(method, func(t *testing.T) {
			dir := t.TempDir()
			launched := filepath.Join(dir, "scope-launched")
			entered := filepath.Join(dir, "gate-entered")
			body := filepath.Join(dir, "body-ran")
			fake := crw1591FakeTool(t, "systemd-run", `#!/bin/sh
echo sibling > "$CRW_TEST_SCOPE_LOG"
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`)
			gate := crw1591FakeTool(t, "heavy-gate", `#!/bin/sh
echo slot > "$CRW_TEST_GATE_LOG"
if [ "${CRW_CI_CPU_SCOPED:-}" = 4 ]; then
  exec "$@"
fi
exec systemd-run --user --scope -- "$@"
`)
			script := filepath.Join(dir, "step.sh")
			if err := os.WriteFile(script, []byte(`echo ran > "$CRW_TEST_BODY_LOG"`), 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{"PATH=" + fake + ":" + os.Getenv("PATH"),
				"CRW_TEST_SCOPE_LOG=" + launched, "CRW_TEST_GATE_LOG=" + entered,
				"CRW_TEST_BODY_LOG=" + body,
				// An inherited or step-supplied marker is not evidence of a scope.
				"CRW_CI_CPU_SCOPED=4", "CRW_CI_CPU_SCOPED=99"}
			env = localCPUEnv(env, localOptions{CPUs: 4, cpuMethod: method, cpuGroup: "/outer.scope"})
			argv := localGateArgv(filepath.Join(gate, "heavy-gate"), true, script, dir, env)
			res := runEnv(t, dir, env, argv[0], argv[1:]...)
			if res.code != 0 {
				t.Fatalf("gate exits %d: %s", res.code, res.stderr)
			}
			for _, path := range []string{entered, body} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("gate/body did not run: %v", err)
				}
			}
			_, err := os.Stat(launched)
			if method == localCPUCgroup && !os.IsNotExist(err) {
				t.Errorf("gate launched a sibling scope inside the shared budget: %v", err)
			}
			if method == localCPUFallback && err != nil {
				t.Errorf("forged scope marker bypassed the gate's scope launcher: %v", err)
			}
		})
	}
}

func TestCRW1621FallbackClampsPackageParallelism(t *testing.T) {
	for _, tc := range []struct{ cpus, jobs, parallel int }{
		{4, 1, 4}, {8, 2, 4}, {5, 2, 9}, {2, 9, 4}, {8, 1, 1},
	} {
		t.Run(fmt.Sprintf("cpus%d-jobs%d-p%d", tc.cpus, tc.jobs, tc.parallel), func(t *testing.T) {
			got, err := localCPUOptions(localOptions{CPUs: tc.cpus, Jobs: tc.jobs, Parallel: tc.parallel})
			if err != nil {
				t.Fatal(err)
			}
			wantGM := tc.cpus / min(tc.cpus, tc.jobs)
			if got.cpu.GOMAXPROCS != wantGM || got.Parallel != 1 {
				t.Errorf("fallback = %+v, want GOMAXPROCS=%d and -p=1", got.cpu, wantGM)
			}
			if got.Jobs*got.Parallel*got.cpu.GOMAXPROCS > tc.cpus {
				t.Errorf("fallback exceeds budget: %+v", got.cpu)
			}
		})
	}
}
