package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-871: every mutating entry that takes --session refuses an id for which
// state.IsCanonicalSessionID is false, before it reads, locks or writes anything. The state
// library keeps sanitising (its recorded oracle rows pin that); the entries simply never hand
// it an id whose sanitised key differs from the id itself, so the existence check, the read,
// the lock and the write all use one key.

const (
	sessionAliasRaw  = "raw id"
	sessionAliasKey  = "raw-id"
	sessionAliasText = "session id is not canonical"
)

// sessionAliasWrite writes a session file under the literal name, bypassing StatePath's
// sanitising: the reproduction needs a file named with the raw id and the file the sanitiser
// maps it to, side by side.
func sessionAliasWrite(t *testing.T, cwd, name, body string) {
	t.Helper()
	orchestrateTransitionPut(t, filepath.Join(cwd, ".crw", "sessions", name+".json"), body)
}

// sessionAliasRead reads a session file under the literal name.
func sessionAliasRead(t *testing.T, cwd, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "sessions", name+".json"))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// sessionAliasLockPath is the lock the sanitiser would use for the raw id.
func sessionAliasLockPath(cwd, name string) string {
	return filepath.Join(cwd, ".crw", "sessions", state.SanitizeKey(name)+".json.lock")
}

// sessionAliasWorkspace seeds the evaluation's reproduction: a file named with the raw id
// (phase B) and the file the sanitiser maps it to (phase C with a check epoch).
func sessionAliasWorkspace(t *testing.T) string {
	t.Helper()
	cwd := orchestrateTransitionRoot(t)
	sessionAliasWrite(t, cwd, sessionAliasRaw, `{"phase":"B","sessionId":"raw id"}`)
	sessionAliasWrite(t, cwd, sessionAliasKey, `{"phase":"C","sessionId":"raw-id","checkEpoch":"c-1"}`)
	return cwd
}

// TestSessionAliasResetRefusesWithoutTouchingEitherFile is the evaluation's reproduction
// (P0, data loss): reset through the raw id must not reset the sanitised session.
func TestSessionAliasResetRefusesWithoutTouchingEitherFile(t *testing.T) {
	cwd := sessionAliasWorkspace(t)
	beforeRaw := sessionAliasRead(t, cwd, sessionAliasRaw)
	beforeKey := sessionAliasRead(t, cwd, sessionAliasKey)
	got := orchestrateTransitionRun(t, cwd, "reset", "--session", sessionAliasRaw)
	if got.Code != 1 || got.Output != "orchestrate reset: "+sessionAliasText {
		t.Fatalf("reset --session %q: %+v", sessionAliasRaw, got)
	}
	if after := sessionAliasRead(t, cwd, sessionAliasRaw); after != beforeRaw {
		t.Fatalf("the refusal rewrote the raw file:\n got %q\nwant %q", after, beforeRaw)
	}
	if after := sessionAliasRead(t, cwd, sessionAliasKey); after != beforeKey {
		t.Fatalf("the refusal rewrote the sanitised file:\n got %q\nwant %q", after, beforeKey)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refusal wrote a ledger row: %+v", rows)
	}
}

// TestSessionAliasEveryMutatingVerbRefuses drives every mutating verb but status: each answers
// the refusal, writes nothing and leaves no lock file. The phase verbs previously answered with
// the SOURCE-ROOT text; they now answer this one.
func TestSessionAliasEveryMutatingVerbRefuses(t *testing.T) {
	// The parser folds a lowercase verb to its uppercase form, as the oracle does
	// (cli__orchestrate__lowercase_verb_accepted), so the answer names the parsed verb.
	for _, tc := range []struct{ input, verb string }{
		{"I", "I"}, {"P", "P"}, {"A", "A"}, {"B", "B"}, {"C", "C"}, {"D", "D"},
		{"i", "I"}, {"p", "P"}, {"a", "A"}, {"reset", "reset"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			cwd := sessionAliasWorkspace(t)
			before := statusTree(t, cwd)
			got := orchestrateTransitionRun(t, cwd, tc.input, "--session", sessionAliasRaw)
			if got.Code != 1 || got.Output != "orchestrate "+tc.verb+": "+sessionAliasText {
				t.Fatalf("%s --session %q: %+v", tc.input, sessionAliasRaw, got)
			}
			if !reflect.DeepEqual(before, statusTree(t, cwd)) {
				t.Fatal("the refusal changed the workspace")
			}
			if _, err := os.Lstat(sessionAliasLockPath(cwd, sessionAliasRaw)); !os.IsNotExist(err) {
				t.Fatalf("the refusal left a lock file: %v", err)
			}
		})
	}
}

// TestSessionAliasTransitionLibraryRefuses covers a direct caller of the library entry, which
// the read-side guard cannot protect: the judgement runs before the lock is taken.
func TestSessionAliasTransitionLibraryRefuses(t *testing.T) {
	cwd := sessionAliasWorkspace(t)
	beforeKey := sessionAliasRead(t, cwd, sessionAliasKey)
	got, err := RunOrchestrateTransition(OrchestrateCliArgs{Verb: fsm.VerbReset, Cwd: cwd}, sessionAliasRaw)
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got.Code != 1 || got.Output != "orchestrate reset: "+sessionAliasText {
		t.Fatalf("library reset with a non-canonical id: %+v", got)
	}
	if after := sessionAliasRead(t, cwd, sessionAliasKey); after != beforeKey {
		t.Fatalf("the library refusal rewrote the sanitised file: %q", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the library refusal wrote a ledger row: %+v", rows)
	}
	if _, err := os.Lstat(sessionAliasLockPath(cwd, sessionAliasRaw)); !os.IsNotExist(err) {
		t.Fatalf("the library refusal left a lock file: %v", err)
	}
}

// TestSessionAliasStatusStaysOnTheRawID pins the read-only verb: status still reads the raw id
// exactly as on dev (port: kept, no write), so the corpus fixture raw_and_sanitized replays.
func TestSessionAliasStatusStaysOnTheRawID(t *testing.T) {
	cwd := sessionAliasWorkspace(t)
	got := orchestrateTransitionRun(t, cwd, "status", "--session", sessionAliasRaw)
	if got.Code != 0 || !strings.Contains(got.Output, "session="+sessionAliasRaw+" phase=C") {
		t.Fatalf("status --session %q: %+v", sessionAliasRaw, got)
	}
}

// TestSessionAliasScanRecordRefuses pins the scan entry: the parser refuses the id right after
// the empty-session check, so the runner is never reached.
func TestSessionAliasScanRecordRefuses(t *testing.T) {
	cwd := sessionAliasWorkspace(t)
	before := sessionAliasRead(t, cwd, sessionAliasKey)
	parsed := ParseScanCliArgs([]string{"record", "--session", sessionAliasRaw}, cwd)
	if parsed.Args != nil || parsed.Error != "scan record: "+sessionAliasText {
		t.Fatalf("scan record parse: %+v", parsed)
	}
	if after := sessionAliasRead(t, cwd, sessionAliasKey); after != before {
		t.Fatalf("the scan refusal rewrote the sanitised file: %q", after)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "interviews")); err == nil {
		t.Fatal("the scan refusal wrote an interview ledger")
	}
}

// TestSessionAliasReviewRoundRefusesOpenAndAbort pins the review-round entry: open and abort
// refuse the id after it is trimmed, and show keeps reading as on dev.
func TestSessionAliasReviewRoundRefusesOpenAndAbort(t *testing.T) {
	cwd := reviewRoundRunSeed(t)
	// The sanitised key is the file show reads: seed it too, so the control proves show still
	// answers from the session the raw id maps to, exactly as on dev.
	seeded := state.DefaultState(sessionAliasKey, reviewRoundRunSlug)
	seeded.Phase = state.PhaseA
	if err := state.WriteState(cwd, seeded); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"open", "abort"} {
		argv := []string{verb, "--session", sessionAliasRaw}
		if verb == "open" {
			argv = append(argv, "--plan-path", reviewRoundRunDoc)
		}
		res, err := reviewRoundRunTry(cwd, nil, argv...)
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if res.Code != 1 || res.Output != "review-round "+verb+": "+sessionAliasText {
			t.Fatalf("%s --session %q: %+v", verb, sessionAliasRaw, res)
		}
	}
	// show is a read: it still answers the sanitised session's own state, as on dev.
	show, err := reviewRoundRunTry(cwd, nil, "show", "--session", sessionAliasRaw)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if show.Code != 0 || !strings.HasPrefix(show.Output, "review-round show: ") {
		t.Fatalf("show --session %q: %+v", sessionAliasRaw, show)
	}
}

// TestSessionAliasCanonicalIDsAreUntouched is the control: an id that passes the guard is its own
// sanitised key, so every entry behaves exactly as before.
func TestSessionAliasCanonicalIDsAreUntouched(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	for _, id := range []string{"rec-s1", "cli", "11111111-1111-4111-8111-111111111111"} {
		if !state.IsCanonicalSessionID(id) {
			t.Fatalf("%q is not canonical", id)
		}
	}
	orchestrateTransitionSession(t, cwd, "rec-s1", `{"phase":"B","sessionId":"rec-s1"}`)
	got := orchestrateTransitionRun(t, cwd, "reset", "--session", "rec-s1")
	if got.Code != 0 || !strings.Contains(got.Output, "orchestrate reset: current=B -> IDLE") {
		t.Fatalf("canonical reset: %+v", got)
	}
	other := orchestrateTransitionRun(t, cwd, "P", "--session", "cli")
	if other.Code != 0 || !strings.Contains(other.Output, "orchestrate P: current=IDLE -> P") {
		t.Fatalf("reserved cli bootstrap: %+v", other)
	}
}
