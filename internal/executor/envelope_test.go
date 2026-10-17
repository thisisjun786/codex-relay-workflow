package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testTask(kind Kind) Task {
	prompt := "do the work of n1"
	return Task{
		Schema:   TaskSchema,
		Identity: Identity{TaskID: "task-1", ExecutionID: "exec-1", Attempt: 1, AssignmentGeneration: 1, Plan: "plan-1", Node: "n1", Revision: "r0", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: Repository{Forge: "github", ID: "repo-1", BaseCommit: strings.Repeat("b", 40),
			TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/n1"},
		Regions:  []Region{{Path: "x/f.go"}},
		WorkKind: "implementation",
		Executor: Binding{HostID: "host-1", Incarnation: "inc-1", Kind: kind, CapabilityRevision: "cap-1",
			LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)},
		Requested: Requested{Model: "model-" + string(kind), Effort: "high"},
		Input: Input{Kind: "prompt", Prompt: prompt, BriefDigest: Digest([]byte(prompt)),
			Artifacts: []Artifact{{Path: "brief.md", SHA256: strings.Repeat("e", 64), Size: 3}}},
		Checks: []Check{{ID: "check-1", Args: []string{"go", "test", "./x/"}, ExpectedExitCode: 0,
			Resources: Resources{CPU: 4, MemoryBytes: 1 << 30, Heavy: true}}},
	}
}

func mustFreeze(t *testing.T, task Task) Admission {
	t.Helper()
	a, err := Freeze(task)
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	return a
}

// GradeLocal is part of the region contract (relay/dagsched/grades.go).
func TestFreezeAcceptsLocalRegionGrade1639(t *testing.T) {
	task := testTask(Codex)
	task.Regions[0].Grade = "local"
	a, err := Freeze(task)
	if err != nil {
		t.Fatalf("local region rejected: %v", err)
	}
	if a.Task.Regions[0].Grade != "local" {
		t.Fatalf("local grade changed to %q", a.Task.Regions[0].Grade)
	}
	if err := ValidateAdmission(a); err != nil {
		t.Fatalf("frozen local admission rejected: %v", err)
	}
}

func TestFreezeDigestStableAcrossReorderings(t *testing.T) {
	for _, kind := range []Kind{Codex, Devin, ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			base := testTask(kind)
			base.Regions = []Region{{Path: "b/f.go"}, {Path: "a/f.go"}, {Path: "tree/", Kind: "tree"}}
			base.Checks = []Check{
				{ID: "check-b", Args: []string{"make", "b"}, ExpectedExitCode: 0, Resources: Resources{CPU: 2, MemoryBytes: 1}},
				{ID: "check-a", Args: []string{"make", "a"}, ExpectedExitCode: 1, Resources: Resources{CPU: 1, MemoryBytes: 1}},
			}
			base.Input.Artifacts = []Artifact{
				{Path: "z.md", SHA256: strings.Repeat("e", 64), Size: 1},
				{Path: "a.md", SHA256: strings.Repeat("f", 64), Size: 2},
			}
			first := mustFreeze(t, base)
			re := testTask(kind)
			re.Regions = []Region{{Path: "tree", Kind: "tree"}, {Path: "a/f.go", Kind: "file", Change: "edit"}, {Path: "b/f.go", Change: "edit"}}
			re.Checks = []Check{base.Checks[1], base.Checks[0]}
			re.Input.Artifacts = []Artifact{base.Input.Artifacts[1], base.Input.Artifacts[0]}
			second := mustFreeze(t, re)
			if first.TaskDigest != second.TaskDigest {
				t.Errorf("reordered regions/checks/artifacts changed the digest")
			}
			args := base
			args.Checks[1].Args = []string{"a", "make"}
			third, err := Freeze(args)
			if err == nil && third.TaskDigest == first.TaskDigest {
				t.Errorf("reordered command arguments did not change the digest")
			}
			if len(first.TaskDigest) != 64 {
				t.Errorf("digest %q is not 64 hex", first.TaskDigest)
			}
		})
	}
}

func TestFreezeDetectsEveryMutation(t *testing.T) {
	base := testTask(Codex)
	baseline := mustFreeze(t, base)
	other64 := strings.Repeat("9", 64)
	other40 := strings.Repeat("8", 40)
	cases := []struct {
		name string
		mut  func(*Task)
	}{
		{"task id", func(x *Task) { x.Identity.TaskID = "task-2" }},
		{"execution id", func(x *Task) { x.Identity.ExecutionID = "exec-2" }},
		{"attempt", func(x *Task) { x.Identity.Attempt = 2 }},
		{"assignment generation", func(x *Task) { x.Identity.AssignmentGeneration = 9 }},
		{"plan", func(x *Task) { x.Identity.Plan = "plan-2" }},
		{"node", func(x *Task) { x.Identity.Node = "n2" }},
		{"revision", func(x *Task) { x.Identity.Revision = "r9" }},
		{"criteria digest", func(x *Task) { x.Identity.CriteriaDigest = other64 }},
		{"forge", func(x *Task) { x.Repository.Forge = "gitlab" }},
		{"repository id", func(x *Task) { x.Repository.ID = "repo-2" }},
		{"base commit", func(x *Task) { x.Repository.BaseCommit = other40 }},
		{"target ref", func(x *Task) { x.Repository.TargetRef = "refs/heads/other" }},
		{"task branch", func(x *Task) { x.Repository.TaskBranch = "refs/heads/task/n2" }},
		{"region path", func(x *Task) { x.Regions[0].Path = "y/f.go" }},
		{"region change", func(x *Task) { x.Regions[0].Change = "rename" }},
		{"region exclusive", func(x *Task) { x.Regions[0].Exclusive = true }},
		{"region grade", func(x *Task) { x.Regions[0].Grade = "independent" }},
		{"work kind", func(x *Task) { x.WorkKind = "design"; x.NotApplicable = "" }},
		{"executor host", func(x *Task) { x.Executor.HostID = "host-2" }},
		{"executor incarnation", func(x *Task) { x.Executor.Incarnation = "inc-2" }},
		{"executor kind", func(x *Task) { x.Executor.Kind = Devin }},
		{"capability revision", func(x *Task) { x.Executor.CapabilityRevision = "cap-2" }},
		{"launch digest", func(x *Task) { x.Executor.LaunchProfileDigest = other64 }},
		{"toolchain digest", func(x *Task) { x.Executor.ToolchainProfileDigest = other64 }},
		{"requested model", func(x *Task) { x.Requested.Model = "other-model" }},
		{"requested effort", func(x *Task) { x.Requested.Effort = "low" }},
		{"prompt", func(x *Task) { x.Input.Prompt = "other work"; x.Input.BriefDigest = Digest([]byte("other work")) }},
		{"input artifact", func(x *Task) { x.Input.Artifacts[0].SHA256 = other64 }},
		{"check id", func(x *Task) { x.Checks[0].ID = "check-2" }},
		{"check args", func(x *Task) { x.Checks[0].Args = []string{"go", "test", "./y/"} }},
		{"expected exit", func(x *Task) { x.Checks[0].ExpectedExitCode = 3 }},
		{"check resources", func(x *Task) { x.Checks[0].Resources.CPU = 8 }},
		{"check heavy", func(x *Task) { x.Checks[0].Resources.Heavy = false }},
		{"not applicable", func(x *Task) { x.NotApplicable = "approved" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := testTask(Codex)
			c.mut(&task)
			a, err := Freeze(task)
			if err == nil && a.TaskDigest == baseline.TaskDigest {
				t.Errorf("the mutation did not change the task digest")
			}
		})
	}
}

func TestFreezeDeepCopiesInputSlices(t *testing.T) {
	task := testTask(Devin)
	a := mustFreeze(t, task)
	task.Regions[0].Path = "other/f.go"
	task.Checks[0].ID = "mutated"
	task.Checks[0].Args[0] = "rm"
	task.Input.Artifacts[0].Path = "evil.md"
	if err := ValidateAdmission(a); err != nil {
		t.Fatalf("admission changed when the caller mutated its slices: %v", err)
	}
	if err := ValidateAdmission(Admission{Task: a.Task, TaskDigest: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("a forged digest was accepted")
	}
	mutated := a.Task
	mutated.Identity.Node = "n2"
	if err := ValidateAdmission(Admission{Task: mutated, TaskDigest: a.TaskDigest}); err == nil {
		t.Fatal("a mutated admitted task kept its digest")
	}
}

func TestFreezeRejectsInvalidTasks(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Task)
	}{
		{"wrong schema", func(x *Task) { x.Schema = "crw-executor-task/0" }},
		{"empty task id", func(x *Task) { x.Identity.TaskID = "" }},
		{"task id control", func(x *Task) { x.Identity.TaskID = "task\n1" }},
		{"task id whitespace edge", func(x *Task) { x.Identity.TaskID = " task-1" }},
		{"task id too long", func(x *Task) { x.Identity.TaskID = strings.Repeat("x", 257) }},
		{"zero attempt", func(x *Task) { x.Identity.Attempt = 0 }},
		{"negative generation", func(x *Task) { x.Identity.AssignmentGeneration = -1 }},
		{"empty plan", func(x *Task) { x.Identity.Plan = "" }},
		{"empty node", func(x *Task) { x.Identity.Node = "" }},
		{"empty revision", func(x *Task) { x.Identity.Revision = "" }},
		{"short criteria digest", func(x *Task) { x.Identity.CriteriaDigest = "abc" }},
		{"uppercase criteria digest", func(x *Task) { x.Identity.CriteriaDigest = strings.Repeat("A", 64) }},
		{"empty forge", func(x *Task) { x.Repository.Forge = "" }},
		{"empty repository id", func(x *Task) { x.Repository.ID = "" }},
		{"short base commit", func(x *Task) { x.Repository.BaseCommit = "abc123" }},
		{"uppercase base commit", func(x *Task) { x.Repository.BaseCommit = strings.Repeat("B", 40) }},
		{"target ref traversal", func(x *Task) { x.Repository.TargetRef = "refs/../heads/dev" }},
		{"target ref dots", func(x *Task) { x.Repository.TargetRef = "a..b" }},
		{"target ref brace", func(x *Task) { x.Repository.TargetRef = "a@{b" }},
		{"target ref lock", func(x *Task) { x.Repository.TargetRef = "refs/heads/x.lock" }},
		{"target ref dash", func(x *Task) { x.Repository.TargetRef = "-bad" }},
		{"target ref space", func(x *Task) { x.Repository.TargetRef = "a b" }},
		{"target ref backslash", func(x *Task) { x.Repository.TargetRef = `a\b` }},
		{"target ref tilde", func(x *Task) { x.Repository.TargetRef = "a~b" }},
		{"target ref caret", func(x *Task) { x.Repository.TargetRef = "a^b" }},
		{"target ref question", func(x *Task) { x.Repository.TargetRef = "a?b" }},
		{"target ref star", func(x *Task) { x.Repository.TargetRef = "a*b" }},
		{"target ref bracket", func(x *Task) { x.Repository.TargetRef = "a[b" }},
		{"target ref double slash", func(x *Task) { x.Repository.TargetRef = "a//b" }},
		{"target ref trailing dot", func(x *Task) { x.Repository.TargetRef = "a." }},
		{"target ref leading slash", func(x *Task) { x.Repository.TargetRef = "/a" }},
		{"branch equals target", func(x *Task) { x.Repository.TaskBranch = "refs/heads/dev" }},
		{"region path traversal", func(x *Task) { x.Regions[0].Path = "../x" }},
		{"region embedded traversal", func(x *Task) { x.Regions[0].Path = "a/../b" }},
		{"region absolute", func(x *Task) { x.Regions[0].Path = "/x" }},
		{"region backslash", func(x *Task) { x.Regions[0].Path = `a\b` }},
		{"region dot file", func(x *Task) { x.Regions[0].Path = "." }},
		{"region bad kind", func(x *Task) { x.Regions[0].Kind = "dir" }},
		{"symbol without key", func(x *Task) { x.Regions[0].Kind = "symbol" }},
		{"file with key", func(x *Task) { x.Regions[0].Key = "K" }},
		{"bad change", func(x *Task) { x.Regions[0].Change = "move" }},
		{"bad grade", func(x *Task) { x.Regions[0].Grade = "unknown" }},
		{"mechanical without rule", func(x *Task) { x.Regions[0].Grade = "mechanical" }},
		{"duplicate regions", func(x *Task) { x.Regions = append(x.Regions, Region{Path: "x/f.go"}) }},
		{"region of another repository", func(x *Task) { x.Regions[0].Repository = "other-repo" }},
		{"empty host", func(x *Task) { x.Executor.HostID = "" }},
		{"empty incarnation", func(x *Task) { x.Executor.Incarnation = "" }},
		{"unknown executor kind", func(x *Task) { x.Executor.Kind = "other" }},
		{"empty capability revision", func(x *Task) { x.Executor.CapabilityRevision = "" }},
		{"short launch digest", func(x *Task) { x.Executor.LaunchProfileDigest = "ab" }},
		{"bad toolchain digest", func(x *Task) { x.Executor.ToolchainProfileDigest = "zz" + strings.Repeat("d", 62) }},
		{"empty requested model", func(x *Task) { x.Requested.Model = "" }},
		{"empty requested effort", func(x *Task) { x.Requested.Effort = "" }},
		{"unknown input kind", func(x *Task) { x.Input.Kind = "context" }},
		{"empty prompt", func(x *Task) { x.Input.Prompt = ""; x.Input.BriefDigest = Digest(nil) }},
		{"brief digest mismatch", func(x *Task) { x.Input.BriefDigest = strings.Repeat("f", 64) }},
		{"input artifact bad path", func(x *Task) { x.Input.Artifacts[0].Path = "../x" }},
		{"input artifact bad digest", func(x *Task) { x.Input.Artifacts[0].SHA256 = "not-hex" }},
		{"input artifact negative size", func(x *Task) { x.Input.Artifacts[0].Size = -1 }},
		{"input artifact duplicate", func(x *Task) { x.Input.Artifacts = append(x.Input.Artifacts, x.Input.Artifacts[0]) }},
		{"check empty id", func(x *Task) { x.Checks[0].ID = "" }},
		{"check duplicate id", func(x *Task) {
			x.Checks = append(x.Checks, Check{ID: "check-1", Args: []string{"x"}, Resources: Resources{CPU: 1}})
		}},
		{"check no args", func(x *Task) { x.Checks[0].Args = nil }},
		{"check empty first arg", func(x *Task) { x.Checks[0].Args = []string{""} }},
		{"check NUL arg", func(x *Task) { x.Checks[0].Args = []string{"go", "x\x00y"} }},
		{"check negative exit", func(x *Task) { x.Checks[0].ExpectedExitCode = -1 }},
		{"check exit over 255", func(x *Task) { x.Checks[0].ExpectedExitCode = 256 }},
		{"check no cpu", func(x *Task) { x.Checks[0].Resources.CPU = 0 }},
		{"check negative memory", func(x *Task) { x.Checks[0].Resources.MemoryBytes = -1 }},
		{"unknown work kind", func(x *Task) { x.WorkKind = "docs" }},
		{"implementation without checks", func(x *Task) { x.Checks = nil }},
		{"implementation without regions", func(x *Task) { x.Regions = nil }},
		{"design without checks or approval", func(x *Task) { x.WorkKind = "design"; x.Checks = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task := testTask(Codex)
			c.mut(&task)
			if a, err := Freeze(task); err == nil {
				t.Errorf("an invalid task was admitted with digest %s", a.TaskDigest)
			} else if _, ok := err.(*ValidationError); !ok {
				t.Errorf("the rejection is not a ValidationError: %T %v", err, err)
			}
		})
	}
}

func TestDesignTaskWithApprovedNotApplicable(t *testing.T) {
	task := testTask(ClaudeCode)
	task.WorkKind = "design"
	task.Checks = nil
	task.NotApplicable = "the design produces no executable checks"
	task.Regions = nil
	if _, err := Freeze(task); err != nil {
		t.Fatalf("an approved design task: %v", err)
	}
}

func TestGrantAndHeartbeatNeverTouchTheTaskDigest(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	g := Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "worker-1", OwnerEpoch: 1, ExpiresAt: now.Add(time.Hour)}
	if err := ValidateGrant(a, g, now); err != nil {
		t.Fatalf("a matching grant: %v", err)
	}
	renewed := g
	renewed.OwnerEpoch, renewed.ExpiresAt = 2, now.Add(2*time.Hour)
	if err := ValidateGrant(a, renewed, now); err != nil {
		t.Fatalf("a renewed grant: %v", err)
	}
	if err := ValidateAdmission(a); err != nil {
		t.Fatalf("the admission changed across a grant renewal: %v", err)
	}
	h := Heartbeat{ExecutionID: "exec-1", HostID: "host-1", Incarnation: "inc-1", AssignmentGeneration: 1, ObservedAt: now}
	if err := ValidateHeartbeat(a, h); err != nil {
		t.Fatalf("a matching heartbeat: %v", err)
	}
	bad := []struct {
		name string
		g    Grant
	}{
		{"another execution", Grant{ExecutionID: "exec-9", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: now.Add(time.Hour)}},
		{"another generation", Grant{ExecutionID: "exec-1", AssignmentGeneration: 2, Owner: "w", OwnerEpoch: 1, ExpiresAt: now.Add(time.Hour)}},
		{"no owner", Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, OwnerEpoch: 1, ExpiresAt: now.Add(time.Hour)}},
		{"zero epoch", Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", ExpiresAt: now.Add(time.Hour)}},
		{"expired", Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: now}},
	}
	for _, c := range bad {
		if err := ValidateGrant(a, c.g, now); err == nil {
			t.Errorf("grant %s was accepted", c.name)
		}
	}
	badh := []Heartbeat{
		{ExecutionID: "exec-9", HostID: "host-1", Incarnation: "inc-1", AssignmentGeneration: 1, ObservedAt: now},
		{ExecutionID: "exec-1", HostID: "host-2", Incarnation: "inc-1", AssignmentGeneration: 1, ObservedAt: now},
		{ExecutionID: "exec-1", HostID: "host-1", Incarnation: "inc-2", AssignmentGeneration: 1, ObservedAt: now},
		{ExecutionID: "exec-1", HostID: "host-1", Incarnation: "inc-1", AssignmentGeneration: 9, ObservedAt: now},
		{ExecutionID: "exec-1", HostID: "host-1", Incarnation: "inc-1", AssignmentGeneration: 1},
	}
	for i, hb := range badh {
		if err := ValidateHeartbeat(a, hb); err == nil {
			t.Errorf("heartbeat %d was accepted", i)
		}
	}
}

func TestDecodeTaskRoundTripAndStrictness(t *testing.T) {
	a := mustFreeze(t, testTask(Devin))
	raw, err := EncodeTask(a.Task)
	if err != nil {
		t.Fatalf("EncodeTask: %v", err)
	}
	got, err := DecodeTask(raw, DefaultLimits)
	if err != nil {
		t.Fatalf("DecodeTask: %v", err)
	}
	if got.TaskDigest != a.TaskDigest {
		t.Fatalf("the decoded task digests differently: %q vs %q", got.TaskDigest, a.TaskDigest)
	}
	withField := func(mut func(m map[string]any)) []byte {
		m := map[string]any{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		mut(m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		name string
		raw  []byte
	}{
		{"unknown top field", withField(func(m map[string]any) { m["extra"] = 1 })},
		{"unknown nested field", withField(func(m map[string]any) { m["identity"].(map[string]any)["bogus"] = "x" })},
		{"duplicate top key", []byte(strings.Replace(string(raw), `"schema":"crw-executor-task/1"`, `"schema":"crw-executor-task/1","schema":"crw-executor-task/1"`, 1))},
		{"duplicate nested key", []byte(strings.Replace(string(raw), `"node":"n1"`, `"node":"n1","node":"n2"`, 1))},
		{"invalid utf8", append([]byte(`{"schema":"crw-executor-task/1","x":"`), 0xff, ']', '"', '}')},
		{"unpaired surrogate", []byte(strings.Replace(string(raw), `"do the work of n1"`, `"\ud800"`, 1))},
		{"trailing document", append(append([]byte{}, raw...), ' ', '{', '}')},
		{"two documents", append(append([]byte{}, raw...), raw...)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeTask(c.raw, DefaultLimits); err == nil {
				t.Error("a malformed task document was decoded")
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		limits := DefaultLimits
		limits.EnvelopeBytes = int64(len(raw)) - 1
		if _, err := DecodeTask(raw, limits); err == nil {
			t.Error("an oversize document was decoded")
		}
	})
	t.Run("invalid limits", func(t *testing.T) {
		if _, err := DecodeTask(raw, Limits{}); err == nil {
			t.Error("zero limits were accepted")
		}
	})
}

func TestStrictShapeOfDecodedEnvelopes(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	raw, err := EncodeTask(a.Task)
	if err != nil {
		t.Fatal(err)
	}
	rm := func(key string) []byte {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		var id map[string]json.RawMessage
		if err := json.Unmarshal(m["identity"], &id); err != nil {
			t.Fatal(err)
		}
		delete(id, key)
		m["identity"], _ = json.Marshal(id)
		b, _ := json.Marshal(m)
		return b
	}
	for name, raw := range map[string][]byte{
		"missing exit code":    []byte(strings.Replace(string(raw), `"expected_exit_code":0,`, ``, 1)),
		"null exit code":       []byte(strings.Replace(string(raw), `"expected_exit_code":0`, `"expected_exit_code":null`, 1)),
		"missing generation":   rm("assignment_generation"),
		"uppercase schema key": []byte(strings.Replace(string(raw), `"schema"`, `"SCHEMA"`, 1)),
		"schema plus alias":    []byte(strings.Replace(string(raw), `"schema":"crw-executor-task/1"`, `"schema":"crw-executor-task/1","SCHEMA":"crw-executor-task/1"`, 1)),
		"nested mixed case":    []byte(strings.Replace(string(raw), `"task_id"`, `"Task_Id"`, 1)),
		"null regions":         []byte(strings.Replace(string(raw), `"regions":[{`, `"regions":null,"x":[{`, 1)),
		"null checks":          []byte(strings.Replace(string(raw), `"checks":[{`, `"checks":null,"y":[{`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeTask(raw, DefaultLimits); err == nil {
				t.Error("a malformed envelope was decoded")
			}
		})
	}
	r := mustResult(t, a)
	rraw, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"null candidate": []byte(strings.Replace(string(rraw), `"candidate":{`, `"candidate":null,"z":{`, 1)),
		"null checks":    []byte(strings.Replace(string(rraw), `"checks":[{`, `"checks":null,"z":[{`, 1)),
	} {
		t.Run("result "+name, func(t *testing.T) {
			if _, err := DecodeResult(a, raw, testPeer(), DefaultLimits); err == nil {
				t.Error("a malformed result was decoded")
			}
		})
	}
}

func TestInvalidUTF8FieldsRejected(t *testing.T) {
	task := testTask(Devin)
	task.Identity.TaskID = "task\xff-1"
	if _, err := Freeze(task); err == nil {
		t.Error("a task id with invalid UTF-8 was admitted")
	}
	a := mustFreeze(t, testTask(Devin))
	r := mustResult(t, a)
	r.Payload.Narrative = "done \xff"
	if err := ValidateResult(a, r, testPeer()); err == nil {
		t.Error("a narrative with invalid UTF-8 was accepted")
	}
}
