package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// The tests are the B-class tests of CXC v0.2.40 pabcd-state/test/state.test.ts that cover the model and its read path
// (codec, snapshots, restore), named by the oracle test they port. The oracle sets its files up with writeState; the
// writer belongs to the state-writes issue, so a file here is the bytes of Encode(state) or a hand-written text.

func at() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// put writes text as the state file of session id under a fresh cwd and returns the cwd.
func put(t *testing.T, id, text string) string {
	t.Helper()
	cwd := t.TempDir()
	putIn(t, cwd, id, text)
	return cwd
}

func putIn(t *testing.T, cwd, id, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(StatePath(cwd, id)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(cwd, id), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// persist is writeState for a test: the file Encode(s) makes, read back through ReadStateStrict.
func persist(t *testing.T, s State) (State, bool) {
	t.Helper()
	b, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return ReadStateStrict(put(t, s.SessionID, string(b)), s.SessionID)
}

func str(s string) *string { return &s }

func TestReadStateMissingFileIsCleanDefaultButOtherFailuresAreUnreadable(t *testing.T) {
	cwd := t.TempDir()
	s, unreadable := ReadStateStrict(cwd, "sess-1")
	if s.Phase != PhaseIdle || s.SessionID != "sess-1" || s.Flags.Interview || unreadable || s.Interview != nil {
		t.Fatalf("missing dir: %+v unreadable=%v", s, unreadable)
	}
	if err := os.MkdirAll(StatePath(cwd, "sess-1"), 0o755); err != nil { // a directory where the file should be: EISDIR, not ENOENT
		t.Fatal(err)
	}
	if s, unreadable = ReadStateStrict(cwd, "sess-1"); !unreadable || s.Phase != PhaseIdle {
		t.Fatalf("directory in place of the file: %+v unreadable=%v", s, unreadable)
	}
}

func TestReadStateCorruptJSONOrInvalidPhaseIsUnreadableDefault(t *testing.T) {
	for name, text := range map[string]string{"corrupt JSON": "{ not json ", "invalid phase": `{"phase":"Z","sessionId":"sess-z"}`} {
		s, unreadable := ReadStateStrict(put(t, "sess-2", text), "sess-2")
		if !unreadable || s.Phase != PhaseIdle || s.SessionID != "sess-2" {
			t.Errorf("%s: %+v unreadable=%v", name, s, unreadable)
		}
	}
}

func TestRestoreRules(t *testing.T) {
	cases := []struct {
		name, text string
		check      func(State) bool
	}{
		{"unknown persisted keys are dropped", `{"phase":"P","evil":"x","flags":{"interview":true,"bogus":1}}`, func(s State) bool {
			b, _ := Encode(s)
			return s.Phase == PhaseP && !s.Flags.Interview && !bytes.Contains(b, []byte("evil")) && !bytes.Contains(b, []byte("bogus"))
		}},
		{"old file reads loopArmSeen false and idleEditNudges 0", `{"phase":"P"}`, func(s State) bool { return !s.LoopArmSeen && s.IdleEditNudges == 0 }},
		{"loopArmSeen and idleEditNudges invalid values coerce to defaults", `{"phase":"P","loopArmSeen":"yes","idleEditNudges":-3}`, func(s State) bool { return !s.LoopArmSeen && s.IdleEditNudges == 0 }},
		{"stopBlockTurnId malformed value becomes null", `{"phase":"P","stopBlockTurnId":42}`, func(s State) bool { return s.StopBlockTurnID == nil }},
		{"stopBlockCapNotified is true only for true", `{"phase":"P","stopBlockCapNotified":"true"}`, func(s State) bool { return !s.StopBlockCapNotified }},
		{"injectedTurns invalid persisted value is empty", `{"phase":"P","injectedTurns":[1,2,"ok"]}`, func(s State) bool { return s.InjectedTurns != nil && len(s.InjectedTurns) == 0 }},
		{"lastInjectedPhase invalid is null, orchestrationActive non-bool is false", `{"phase":"P","lastInjectedPhase":"Z","orchestrationActive":"yes"}`, func(s State) bool { return s.LastInjectedPhase == nil && !s.OrchestrationActive }},
		{"IDLE phase forces orchestrationActive false and drops lastInjectedPhase IDLE", `{"phase":"IDLE","lastInjectedPhase":"IDLE","orchestrationActive":true}`, func(s State) bool { return s.LastInjectedPhase == nil && !s.OrchestrationActive }},
		{"050 snapshot dirty:\"yes\" is rejected rather than coerced", `{"phase":"B","phaseEntrySource":{"kind":"resolved","commitSha":"abc","dirty":"yes","capturedAt":"t"}}`, func(s State) bool { return s.PhaseEntrySource == nil }},
		{"050 dirty snapshot without a tree hash is rejected", `{"phase":"B","phaseEntrySource":{"kind":"resolved","commitSha":"abc","dirty":true,"capturedAt":"t"}}`, func(s State) bool { return s.PhaseEntrySource == nil }},
		{"050 snapshot found outside B is dropped", `{"phase":"P","phaseEntrySource":{"kind":"resolved","commitSha":"abc","dirty":false,"capturedAt":"t"}}`, func(s State) bool { return s.PhaseEntrySource == nil }},
		{"050 B keeps its own snapshot", `{"phase":"B","phaseEntrySource":{"kind":"resolved","commitSha":"abc","dirty":false,"capturedAt":"t"}}`, func(s State) bool { return s.PhaseEntrySource != nil && s.PhaseEntrySource.CommitSha == "abc" }},
		{"L8 fresh session reads interview null", `{"phase":"P"}`, func(s State) bool { return s.Interview == nil }},
		{"L8 HIGH-1 persisted flags.interview true with a non-ready tracker reads false", `{"phase":"I","flags":{"interview":true},"interview":{"roundId":1,"dimensions":{},"contradictions":[],"assumptions":[]}}`, func(s State) bool { return !s.Flags.Interview && s.Interview != nil }},
		{"wp5 marker without the successor field restores as legacy", `{"phase":"IDLE","checkEpoch":"c","dcloseRecovery":{"sessionId":"s","checkEpoch":"c","closedWorkPhaseId":"wp-1"}}`, func(s State) bool {
			return s.DcloseRecovery != nil && s.DcloseRecovery.Legacy && s.DcloseRecovery.NextWorkPhaseID == nil
		}},
		{"wp5 malformed successor value restores as legacy instead of an explicit null", `{"phase":"IDLE","checkEpoch":"c","dcloseRecovery":{"sessionId":"s","checkEpoch":"c","closedWorkPhaseId":"wp-1","nextWorkPhaseId":7}}`, func(s State) bool {
			return s.DcloseRecovery != nil && s.DcloseRecovery.Legacy && s.DcloseRecovery.NextWorkPhaseID == nil
		}},
		{"wp5 explicit null successor restores without the legacy flag", `{"phase":"IDLE","checkEpoch":"c","dcloseRecovery":{"sessionId":"s","checkEpoch":"c","closedWorkPhaseId":"wp-1","nextWorkPhaseId":null}}`, func(s State) bool {
			return s.DcloseRecovery != nil && !s.DcloseRecovery.Legacy && s.DcloseRecovery.NextWorkPhaseID == nil
		}},
		{"wp5 foreign marker is dropped and cannot retain an IDLE epoch", `{"phase":"IDLE","checkEpoch":"c","dcloseRecovery":{"sessionId":"other","checkEpoch":"c","closedWorkPhaseId":"wp-1","nextWorkPhaseId":"wp-2"}}`, func(s State) bool { return s.DcloseRecovery == nil && s.CheckEpoch == nil }},
	}
	for _, c := range cases {
		s, unreadable := restore("s", []byte(c.text), at())
		if unreadable || !c.check(s) {
			t.Errorf("%s: %+v unreadable=%v", c.name, s, unreadable)
		}
	}
}

func TestPersistedStatesRoundTrip(t *testing.T) {
	full := DefaultState("rt", "slug")
	full.Phase, full.OrchestrationActive, full.LastInjectedPhase = PhaseB, true, &[]Phase{PhaseB}[0]
	full.InjectedTurns, full.StopBlockTurnID, full.StopBlockCapNotified = []string{"t1", "t2"}, str("turn-new"), true
	full.LoopArmSeen, full.IdleEditNudges, full.CheckEpoch = true, 7, str("c-valid")
	full.DcloseRecovery = &DcloseRecoveryMarker{SessionID: "rt", CheckEpoch: "c-valid", ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: str("wp-2")}
	full.Flags = Flags{Interview: true, AuditPassed: true} // Interview is derived from the tracker: no tracker, so false
	got, unreadable := persist(t, full)
	if unreadable {
		t.Fatal("unreadable")
	}
	full.Flags.Interview, full.CheckEpoch = false, nil // phase B holds no check epoch
	if a, b := mustEncode(t, got), mustEncode(t, full); a != b {
		t.Fatalf("round trip:\n%s\nwant\n%s", a, b)
	}
	// session isolation: two ids in one cwd keep their own files
	cwd, other := t.TempDir(), DefaultState("beta", "")
	other.Phase = PhaseP
	putIn(t, cwd, "alpha", mustEncode(t, DefaultState("alpha", "")))
	putIn(t, cwd, "beta", mustEncode(t, other))
	if a, b := ReadState(cwd, "alpha"), ReadState(cwd, "beta"); a.Phase != PhaseIdle || b.Phase != PhaseP {
		t.Fatalf("alpha %s beta %s", a.Phase, b.Phase)
	}
}

func mustEncode(t *testing.T, s State) string {
	t.Helper()
	b, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestInterviewTrackerSurvivesRestoreWithoutUnknownKeys(t *testing.T) {
	dim := `{"level":"max","known":["k"],"unknown":[],"confidence":1,"EVIL":1}`
	text := `{"phase":"P","orchestrationActive":true,"injectedTurns":["t1"],"interview":{"roundId":9,"dimensions":{"goal":` + dim + `,"constraint":` + dim + `,"success":` + dim + `,"ontology":` + dim + `},"contradictions":[],"assumptions":[{"id":"a","text":"x","recorded":true}],"scanRounds":1,"EVIL":"drop"}}`
	s, unreadable := restore("iv-1", []byte(text), at())
	b, _ := Encode(s)
	if unreadable || s.Phase != PhaseP || !s.OrchestrationActive || len(s.InjectedTurns) != 1 || s.Interview == nil || s.Interview.RoundID != 9 ||
		s.Interview.Dimensions.Goal.Level != "max" || !s.Flags.Interview || bytes.Contains(b, []byte("EVIL")) {
		t.Fatalf("%+v unreadable=%v\n%s", s, unreadable, b)
	}
}

func TestDefaultStateAndPhaseLists(t *testing.T) {
	d := DefaultState("x", "y")
	b, _ := Encode(d) // nil slices would encode as null
	if d.Slug != "y" || d.Phase != PhaseIdle || !bytes.Contains(b, []byte(`"injectedTurns": [],`)) || !bytes.Contains(b, []byte(`"unverifiedSubagents": [],`)) ||
		!slices.Equal(WorkPhases(), []Phase{PhaseI, PhaseP, PhaseA, PhaseB, PhaseC, PhaseD}) || !slices.Equal(AllPhases(), append([]Phase{PhaseIdle}, WorkPhases()...)) {
		t.Fatalf("%+v\n%s", d, b)
	}
}

func TestSanitizeKeyAndIsCanonicalSessionID(t *testing.T) {
	for in, want := range map[string]string{"019f/13ab:cd": "019f-13ab-cd", "": "missing", "///": "missing", "a!!b": "a-b", "a!-b": "a--b", "세션-1": "1"} {
		if got := SanitizeKey(in); got != want {
			t.Errorf("SanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
	for id, want := range map[string]bool{"019f4a8a-b1a1-7113-b72a-460a39a8f096": true, "session_1.example": true, "": false, "  session-1  ": false, "../session-1": false, "세션-1": false, "-session-1": false} {
		if got := IsCanonicalSessionID(id); got != want {
			t.Errorf("IsCanonicalSessionID(%q) = %v", id, got)
		}
	}
}

func TestFindForeignSessionCopiesListsOtherTreesHoldingTheSameSession(t *testing.T) {
	mine, other, empty := put(t, "s", "{}"), put(t, "s", "{}"), t.TempDir()
	got := FindForeignSessionCopies(mine, "s", []string{mine, "", empty, other, other})
	if want := StatePath(other, "s"); len(got) != 1 || got[0] != want {
		t.Fatalf("got %v, want [%s]", got, want)
	}
}

func TestMatchesDcloseRecoveryNeedsOwnSessionEpochAndPhase(t *testing.T) {
	m := &DcloseRecoveryMarker{SessionID: "s", CheckEpoch: "c", ClosedWorkPhaseID: "wp-1"}
	for i, c := range []struct {
		s     State
		phase string
		want  bool
	}{
		{State{SessionID: "s", CheckEpoch: str("c"), DcloseRecovery: m}, "wp-1", true},
		{State{SessionID: "s", CheckEpoch: str("c"), DcloseRecovery: m}, "wp-2", false},
		{State{SessionID: "s", CheckEpoch: str("other"), DcloseRecovery: m}, "wp-1", false},
		{State{SessionID: "s", DcloseRecovery: m}, "wp-1", false},
		{State{SessionID: "t", CheckEpoch: str("c"), DcloseRecovery: m}, "wp-1", false},
		{State{SessionID: "s", CheckEpoch: str("c")}, "wp-1", false},
	} {
		if got := MatchesDcloseRecovery(c.s, c.phase); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

func TestReconstructUnverifiedReadsPlainJSONDecoding(t *testing.T) {
	var raw any // json.Unmarshal yields float64; ReadStateStrict decodes with UseNumber
	if err := json.Unmarshal([]byte(`[{"agentId":"a","recordedAt":"t","attempts":3}]`), &raw); err != nil {
		t.Fatal(err)
	}
	if got, corrupt := ReconstructUnverified(raw); corrupt || len(got) != 1 || got[0].Attempts != 3 {
		t.Fatalf("%+v corrupt=%v", got, corrupt)
	}
}

// The oracle resolves paths without following symlinks (path.resolve), so a symlink to cwd's own tree is another tree.
func TestFindForeignSessionCopiesTakesASymlinkAliasForAnotherTree(t *testing.T) {
	mine := put(t, "s", "{}")
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(mine, alias); err != nil {
		t.Skip(err)
	}
	if got := FindForeignSessionCopies(mine, "s", []string{alias}); len(got) != 1 || got[0] != StatePath(alias, "s") {
		t.Fatalf("%v", got)
	}
}

func TestSourceIdentityConvertsToTheSourceType(t *testing.T) {
	hash, root := "h", "/ws"
	got := SourceIdentity{Kind: source.KindResolved, CommitSha: "c", Dirty: true, CapturedAt: "t", TreeHash: &hash, SourceRoot: &root}.Identity()
	if got.Kind != source.KindResolved || got.TreeHash != "h" || *got.SourceRoot != "/ws" || (SourceIdentity{}).Identity().TreeHash != "" {
		t.Fatalf("%+v", got)
	}
}
