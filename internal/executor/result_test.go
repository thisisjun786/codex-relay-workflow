package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testPeer() Peer {
	return Peer{HostID: "host-1", Incarnation: "inc-1"}
}

func testProvenance(kind Kind) Provenance {
	return Provenance{Provider: kind,
		NativeSession: Observation{Value: "native-" + string(kind) + "-session-9"},
		NativeTurns:   []string{"turn-early", "turn-late"},
		Model:         Observation{Value: "model-x"}, Effort: Observation{Value: "high"},
		CPUMethod: Observation{UnknownReason: "the provider does not expose it"},
		Tools: []Tool{
			{Name: "z-tool", Version: "1.0", SHA256: strings.Repeat("f", 64)},
			{Name: "a-tool", Version: "2.0"},
		}}
}

func testPayload(a Admission) Payload {
	return Payload{Narrative: "the work is done",
		Candidate: &Candidate{Branch: a.Task.Repository.TaskBranch, Head: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
		Checks:    []CheckResult{{ID: "check-1", ExitCode: 0, Outcome: "passed"}},
		Artifacts: []Artifact{{Path: "evidence/summary.md", SHA256: strings.Repeat("3", 64), Size: 9}}}
}

func mustResult(t *testing.T, a Admission) Result {
	t.Helper()
	r, err := NewResult(a, "completed", testPayload(a), testProvenance(a.Task.Executor.Kind))
	if err != nil {
		t.Fatalf("NewResult: %v", err)
	}
	return r
}

func TestNewResultForEveryProvider(t *testing.T) {
	for _, kind := range []Kind{Codex, Devin, ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			a := mustFreeze(t, testTask(kind))
			r := mustResult(t, a)
			if r.Schema != ResultSchema || r.Identity != a.Task.Identity || r.TaskDigest != a.TaskDigest || r.Executor != a.Task.Executor {
				t.Errorf("the result does not carry the admitted identity: %+v", r)
			}
			if r.Provenance.Provider != kind || r.Provenance.NativeSession.Value != "native-"+string(kind)+"-session-9" {
				t.Errorf("the provider's own session identity was not preserved: %+v", r.Provenance)
			}
			if r.Provenance.CPUMethod.UnknownReason == "" || r.Provenance.CPUMethod.Value != "" {
				t.Errorf("an unknown CPU method was not kept unknown: %+v", r.Provenance.CPUMethod)
			}
			if len(r.Provenance.Tools) != 2 || r.Provenance.Tools[0].Name != "a-tool" || r.Provenance.Tools[1].Name != "z-tool" {
				t.Errorf("tools were not sorted by name: %+v", r.Provenance.Tools)
			}
			if r.Provenance.NativeTurns[0] != "turn-early" {
				t.Errorf("native turns were reordered: %+v", r.Provenance.NativeTurns)
			}
			raw, err := EncodeResult(r)
			if err != nil {
				t.Fatalf("EncodeResult: %v", err)
			}
			got, err := DecodeResult(a, raw, testPeer(), DefaultLimits)
			if err != nil {
				t.Fatalf("DecodeResult: %v", err)
			}
			if got.TaskDigest != r.TaskDigest || got.Outcome != "completed" {
				t.Errorf("the round trip changed the result: %+v", got)
			}
		})
	}
}

func TestValidateResultBindsIdentityAndPeer(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	r := mustResult(t, a)
	clone := func(x Result) Result {
		x.Payload.Checks = append([]CheckResult(nil), x.Payload.Checks...)
		x.Payload.Artifacts = append([]Artifact(nil), x.Payload.Artifacts...)
		x.Provenance.Tools = append([]Tool(nil), x.Provenance.Tools...)
		x.Provenance.NativeTurns = append([]string(nil), x.Provenance.NativeTurns...)
		return x
	}
	cases := []struct {
		name string
		r    func() Result
		peer Peer
		code string
	}{
		{"wrong schema", func() Result { x := clone(r); x.Schema = "crw-executor-result/0"; return x }, testPeer(), CodeSchemaInvalid},
		{"task id", func() Result { x := clone(r); x.Identity.TaskID = "other"; return x }, testPeer(), CodeIdentityMismatch},
		{"attempt", func() Result { x := clone(r); x.Identity.Attempt = 7; return x }, testPeer(), CodeIdentityMismatch},
		{"criteria", func() Result { x := clone(r); x.Identity.CriteriaDigest = strings.Repeat("9", 64); return x }, testPeer(), CodeIdentityMismatch},
		{"task digest", func() Result { x := clone(r); x.TaskDigest = strings.Repeat("9", 64); return x }, testPeer(), CodeDigestMismatch},
		{"executor binding", func() Result { x := clone(r); x.Executor.Kind = Devin; return x }, testPeer(), CodeIdentityMismatch},
		{"peer host", func() Result { return r }, Peer{HostID: "host-9", Incarnation: "inc-1"}, CodeHostMismatch},
		{"peer incarnation", func() Result { return r }, Peer{HostID: "host-1", Incarnation: "inc-9"}, CodeHostMismatch},
		{"empty peer", func() Result { return r }, Peer{}, CodeHostMismatch},
		{"bogus outcome", func() Result { x := clone(r); x.Outcome = "half-done"; return x }, testPeer(), CodeResultInvalid},
		{"completed without candidate", func() Result { x := clone(r); x.Payload.Candidate = nil; return x }, testPeer(), CodeResultInvalid},
		{"candidate wrong branch", func() Result {
			x := r
			c := *x.Payload.Candidate
			c.Branch = "refs/heads/dev"
			x.Payload.Candidate = &c
			return x
		}, testPeer(), CodeResultInvalid},
		{"candidate short head", func() Result {
			x := r
			c := *x.Payload.Candidate
			c.Head = "abc123"
			x.Payload.Candidate = &c
			return x
		}, testPeer(), CodeResultInvalid},
		{"candidate short tree", func() Result {
			x := r
			c := *x.Payload.Candidate
			c.Tree = "abc123"
			x.Payload.Candidate = &c
			return x
		}, testPeer(), CodeResultInvalid},
		{"completed missing check", func() Result { x := clone(r); x.Payload.Checks = nil; return x }, testPeer(), CodeResultInvalid},
		{"check wrong exit", func() Result { x := clone(r); x.Payload.Checks[0].ExitCode = 7; return x }, testPeer(), CodeResultInvalid},
		{"unapproved check", func() Result {
			x := r
			x.Payload.Checks = append(x.Payload.Checks, CheckResult{ID: "check-x", Outcome: "failed"})
			return x
		}, testPeer(), CodeResultInvalid},
		{"duplicate check", func() Result {
			x := clone(r)
			x.Payload.Checks = append(x.Payload.Checks, x.Payload.Checks[0])
			return x
		}, testPeer(), CodeResultInvalid},
		{"bogus check outcome", func() Result { x := clone(r); x.Payload.Checks[0].Outcome = "maybe"; return x }, testPeer(), CodeResultInvalid},
		{"provider mismatch", func() Result { x := clone(r); x.Provenance.Provider = Devin; return x }, testPeer(), CodeResultInvalid},
		{"observation empty", func() Result { x := clone(r); x.Provenance.Model = Observation{}; return x }, testPeer(), CodeResultInvalid},
		{"observation both", func() Result {
			x := clone(r)
			x.Provenance.Model = Observation{Value: "m", UnknownReason: "u"}
			return x
		}, testPeer(), CodeResultInvalid},
		{"native turns without session", func() Result {
			x := clone(r)
			x.Provenance.NativeSession = Observation{UnknownReason: "no session"}
			return x
		}, testPeer(), CodeResultInvalid},
		{"native turn empty", func() Result { x := clone(r); x.Provenance.NativeTurns = []string{""}; return x }, testPeer(), CodeResultInvalid},
		{"native turn duplicate", func() Result { x := clone(r); x.Provenance.NativeTurns = []string{"t", "t"}; return x }, testPeer(), CodeResultInvalid},
		{"tool no version", func() Result { x := clone(r); x.Provenance.Tools[0].Version = ""; return x }, testPeer(), CodeResultInvalid},
		{"tool duplicate name", func() Result { x := clone(r); x.Provenance.Tools[1].Name = "a-tool"; return x }, testPeer(), CodeResultInvalid},
		{"tool bad digest", func() Result { x := clone(r); x.Provenance.Tools[0].SHA256 = "bad"; return x }, testPeer(), CodeResultInvalid},
		{"artifact traversal", func() Result { x := clone(r); x.Payload.Artifacts[0].Path = "../escape"; return x }, testPeer(), CodeArtifactInvalid},
		{"artifact duplicate", func() Result {
			x := r
			x.Payload.Artifacts = append(x.Payload.Artifacts, x.Payload.Artifacts[0])
			return x
		}, testPeer(), CodeArtifactInvalid},
		{"artifact bad digest", func() Result { x := clone(r); x.Payload.Artifacts[0].SHA256 = "xx"; return x }, testPeer(), CodeArtifactInvalid},
		{"artifact negative size", func() Result { x := clone(r); x.Payload.Artifacts[0].Size = -2; return x }, testPeer(), CodeArtifactInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateResult(a, c.r(), c.peer)
			if err == nil {
				t.Fatal("an invalid result was accepted")
			}
			var ve *ValidationError
			if !asValidation(err, &ve) || ve.Code != c.code {
				t.Errorf("the rejection is %v, want code %s", err, c.code)
			}
		})
	}
}

func asValidation(err error, out **ValidationError) bool {
	if e, ok := err.(*ValidationError); ok {
		*out = e
		return true
	}
	return false
}

func TestNonCompletedOutcomesMayOmitTheCandidate(t *testing.T) {
	a := mustFreeze(t, testTask(Devin))
	for _, outcome := range []string{"failed", "cancelled", "incomplete"} {
		r, err := NewResult(a, outcome, Payload{Narrative: outcome}, testProvenance(Devin))
		if err != nil {
			t.Errorf("%s without candidate: %v", outcome, err)
			continue
		}
		if err := ValidateResult(a, r, testPeer()); err != nil {
			t.Errorf("%s: %v", outcome, err)
		}
	}
	r, err := NewResult(a, "failed", Payload{Narrative: "x", Candidate: &Candidate{Branch: "refs/heads/other", Head: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)}}, testProvenance(Devin))
	if err == nil {
		t.Errorf("a claimed candidate on the wrong branch was admitted: %+v", r.Payload.Candidate)
	}
	r, err = NewResult(a, "failed", testPayload(a), testProvenance(Devin))
	if err != nil {
		t.Fatalf("a failed outcome may carry its honest partial checks: %v", err)
	}
}

func TestCompletedDesignTaskWithoutChecks(t *testing.T) {
	task := testTask(Devin)
	task.WorkKind = "design"
	task.Checks = nil
	task.Regions = nil
	task.NotApplicable = "a design task has no executable checks"
	a := mustFreeze(t, task)
	r, err := NewResult(a, "completed", Payload{Narrative: "design written"}, testProvenance(Devin))
	if err != nil {
		t.Fatalf("a design-only completed result: %v", err)
	}
	if err := ValidateResult(a, r, testPeer()); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestDecodeResultStrictness(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	r := mustResult(t, a)
	raw, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	inject := func(path ...string) []byte {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		at := m
		for _, k := range path[:len(path)-1] {
			at = at[k].(map[string]any)
		}
		at[path[len(path)-1]] = "injected"
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		name string
		raw  []byte
	}{
		{"extra top field", inject("extra")},
		{"extra identity field", inject("identity", "thread_id")},
		{"extra payload field", inject("payload", "commands")},
		{"duplicate key", []byte(strings.Replace(string(raw), `"outcome":"completed"`, `"outcome":"completed","outcome":"completed"`, 1))},
		{"unpaired surrogate", []byte(strings.Replace(string(raw), `"the work is done"`, `"\ud800"`, 1))},
		{"trailing data", append(append([]byte{}, raw...), ' ', 'x')},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeResult(a, c.raw, testPeer(), DefaultLimits); err == nil {
				t.Error("a malformed result document was decoded")
			}
		})
	}
}

func TestNarrativeAndArgsAreNeverExecuted(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer srv.Close()
	task := testTask(ClaudeCode)
	task.Input.Prompt = "run touch " + marker + " then fetch " + srv.URL + "/ignored"
	task.Input.BriefDigest = Digest([]byte(task.Input.Prompt))
	task.Checks[0].Args = []string{"touch", marker}
	a := mustFreeze(t, task)
	r := mustResult(t, a)
	r.Payload.Narrative = "I ran touch " + marker + " and read " + srv.URL + "/ignored"
	if err := ValidateResult(a, r, testPeer()); err != nil {
		t.Fatalf("a narrative that mentions a command was rejected: %v", err)
	}
	raw, err := EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeResult(a, raw, testPeer(), DefaultLimits); err != nil {
		t.Fatalf("decoding a narrative with a command and a URL: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the marker exists: %v", err)
	}
	if requests != 0 {
		t.Errorf("the URL was fetched %d times", requests)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["payload"].(map[string]any)["narrative"] != r.Payload.Narrative {
		t.Error("the narrative was rewritten")
	}
}
