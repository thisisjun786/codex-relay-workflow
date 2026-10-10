package hook_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1110: a session that already holds the 64 unresolved verdicts the reader keeps gets a 65th. The main state must stay whole
// (no overflow flag), the extra verdict must stay durable and visible to the parent's completion gate, the 65th child's late
// receipt must resolve only its own verdict, and a file that already holds 65 must be recoverable without losing a verdict.

// overflowSeed writes n unresolved, resolvable verdicts a0..a<n-1> of session s1.
func overflowSeed(t *testing.T, cwd string, n int) {
	t.Helper()
	s := state.DefaultState("s1", "")
	for i := range n {
		s.UnverifiedSubagents = append(s.UnverifiedSubagents, state.UnverifiedSubagent{AgentID: fmt.Sprintf("a%d", i), TurnID: "t",
			AgentType: "executor", Attempts: 3, RecordedAt: "2026-10-10T00:00:00.000Z", Resolvable: true})
	}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

// overflowExhaust spends agent's budget and stops it once more, so its terminal verdict is recorded.
func overflowExhaust(t *testing.T, cwd, agent string) {
	t.Helper()
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, counterStop(t, cwd, "s1", agent, "t", ""), n)
	}
	if out := counterStop(t, cwd, "s1", agent, "t", ""); out != "" {
		t.Fatalf("terminal stop of %s blocked: %s", agent, out)
	}
}

func overflowMainIntact(t *testing.T, cwd string, want int) {
	t.Helper()
	s, unreadable := state.ReadStateStrict(cwd, "s1")
	if unreadable || s.UnverifiedCorrupt || len(s.UnverifiedSubagents) != want {
		t.Fatalf("main state: unreadable %v corrupt %v verdicts %d, want %d intact", unreadable, s.UnverifiedCorrupt, len(s.UnverifiedSubagents), want)
	}
}

func TestSubagentStopOverflowVerdictIsKeptBesideTheCap(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	overflowSeed(t, cwd, state.MaxUnverifiedSubagents)
	overflowExhaust(t, cwd, "a64")
	overflowMainIntact(t, cwd, state.MaxUnverifiedSubagents)
	if !evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a64", TurnID: "t"}) {
		t.Fatal("the 65th verdict is not visible")
	}
	if counterComplete(t, cwd, "s1") == "" {
		t.Fatal("completion allowed with 65 unresolved verdicts")
	}
	// The 65th child's late valid receipt resolves its own verdict only.
	subagentStopPut(t, filepath.Join(cwd, ".crw/evidence/a64.md"), "verified")
	if out := counterStop(t, cwd, "s1", "a64", "t", "EVIDENCE_RECORDED: .crw/evidence/a64.md"); out != "" {
		t.Fatal(out)
	}
	if evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a64", TurnID: "t"}) {
		t.Fatal("the late receipt did not resolve the 65th verdict")
	}
	overflowMainIntact(t, cwd, state.MaxUnverifiedSubagents)
	if counterComplete(t, cwd, "s1") == "" {
		t.Fatal("completion allowed while 64 verdicts remain")
	}
	// The parent resolves a 66th recorded beside the cap with its own receipt.
	overflowExhaust(t, cwd, "a65")
	receipt := filepath.Join(cwd, ".crw/evidence/parent.md")
	subagentStopPut(t, receipt, "parent verified a65")
	if out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a65", Receipt: receipt, Cwd: cwd}); code != 0 {
		t.Fatalf("resolve of an overflow verdict: %s", out)
	}
	if evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a65", TurnID: "t"}) {
		t.Fatal("the CLI did not resolve the overflow verdict")
	}
	overflowMainIntact(t, cwd, state.MaxUnverifiedSubagents)
}

// A file written before the fix holds 65 verdicts: every verdict is checked and kept, the main state is recovered to 64 with no
// overflow flag, and verdicts that arrive at the same time are kept too.
func TestSubagentStopOverflowRecoversA65File(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	overflowSeed(t, cwd, state.MaxUnverifiedSubagents)
	path := state.StatePath(cwd, "s1")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["unverifiedSubagents"] = append(doc["unverifiedSubagents"].([]any), map[string]any{"agentId": "old65", "turnId": "t", "agentType": "executor",
		"attempts": 3, "receiptClaimed": "", "recordedAt": "2026-10-10T00:00:00.000Z", "resolvable": true})
	raw, _ = json.Marshal(doc)
	subagentStopPut(t, path, string(raw))
	if s, _ := state.ReadStateStrict(cwd, "s1"); !s.UnverifiedCorrupt {
		t.Fatal("precondition: a 65-entry file reads as overflowed")
	}
	// The session lock gives up after its 250 ms schedule, and a verdict whose lock gave up goes to the sentinel tier, so two
	// recoveries that overlap on a host where one holder outlasts that schedule (a slow fsync, a descheduled process) lose a
	// verdict for the lock's budget and not for the property under test (CRW-1181). The wait goes on until the holder releases the
	// lock, with a guard that only ends a hang; who is inside is still decided by the lock alone.
	defer state.WaitForSessionLock(3 * time.Minute)()
	var wg sync.WaitGroup
	for _, agent := range []string{"new1", "new2"} {
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, counterStop(t, cwd, "s1", agent, "t", ""), n)
		}
		wg.Go(func() {
			raw, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": cwd, "session_id": "s1", "agent_type": "executor", "agent_id": agent, "turn_id": "t"})
			if out, err := subagentStopInvoke(raw); err != nil || out != "" {
				t.Errorf("%s: %q %v", agent, out, err)
			}
		})
	}
	wg.Wait()
	overflowMainIntact(t, cwd, state.MaxUnverifiedSubagents)
	for _, agent := range []string{"a0", "a63", "old65", "new1", "new2"} {
		if !evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: agent, TurnID: "t"}) {
			t.Errorf("verdict of %s lost", agent)
		}
	}
	receipt := filepath.Join(cwd, ".crw/evidence/old65.md")
	subagentStopPut(t, receipt, "verified")
	if out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "old65", Receipt: receipt, Cwd: cwd}); code != 0 {
		t.Fatalf("resolve of the recovered 65th: %s", out)
	}
	if counterComplete(t, cwd, "s1") == "" {
		t.Fatal("completion allowed while verdicts remain")
	}
}

// CRW-1110 verification round 1: one agent's two verdicts, in different turns, sit one in the main list and one beside the full
// list. A resolve that names no turn is ambiguous across both and resolves neither.
func TestEvidenceCLIAmbiguityAcrossMainAndOverflow(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	overflowSeed(t, cwd, state.MaxUnverifiedSubagents)
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, counterStop(t, cwd, "s1", "a0", "second-turn", ""), n)
	}
	if out := counterStop(t, cwd, "s1", "a0", "second-turn", ""); out != "" {
		t.Fatalf("terminal stop blocked: %s", out)
	}
	if got, _ := evidence.OverflowVerdicts(cwd, "s1"); len(got) != 1 || got[0].AgentID != "a0" || got[0].TurnID != "second-turn" {
		t.Fatalf("precondition: the second turn's verdict is beside the list: %+v", got)
	}
	receipt := filepath.Join(cwd, ".crw/evidence/parent.md")
	subagentStopPut(t, receipt, "parent verified")
	out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a0", Receipt: receipt, Cwd: cwd})
	if code == 0 || !strings.Contains(out, "more than one unverified record") {
		t.Fatalf("an ambiguous resolve across the list and its overflow: code=%d %q", code, out)
	}
	for _, turn := range []string{"t", "second-turn"} {
		if !evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a0", TurnID: turn}) {
			t.Fatalf("the ambiguous resolve removed the verdict of turn %s", turn)
		}
	}
	// Naming the turn resolves exactly that verdict, in either place.
	for _, turn := range []string{"second-turn", "t"} {
		args := cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a0", Receipt: receipt, Cwd: cwd, TurnID: &turn}
		if out, code := cli.RunEvidenceCLI(args); code != 0 {
			t.Fatalf("resolve of turn %s: %s", turn, out)
		}
		if evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a0", TurnID: turn}) {
			t.Fatalf("turn %s still unresolved", turn)
		}
	}
}

// CRW-1110 verification round 2: a resolve that names no turn finds one verdict by its unlocked read and waits for that verdict's
// tuple lock; while it waits, the same agent's verdict of another turn is recorded beside the full list. The request is still the
// one that named no turn, so under the lock it counts every turn again, finds two and resolves neither.
func TestEvidenceCLIImplicitTurnRechecksAmbiguityUnderTheLock(t *testing.T) {
	cwd := subagentStopWorkspace(t)
	overflowSeed(t, cwd, state.MaxUnverifiedSubagents)
	receipt := filepath.Join(cwd, ".crw/evidence/parent.md")
	subagentStopPut(t, receipt, "parent verified")
	type result struct {
		out  string
		code int
	}
	done := make(chan result, 1)
	if err := evidence.WithCounterLock(cwd, "s1", "a0", "t", func() error {
		go func() {
			out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a0", Receipt: receipt, Cwd: cwd})
			done <- result{out, code}
		}()
		time.Sleep(300 * time.Millisecond) // the CLI has read the one verdict and waits for this lock
		for n := 1; n <= 3; n++ {
			subagentStopBlock(t, counterStop(t, cwd, "s1", "a0", "second-turn", ""), n)
		}
		if out := counterStop(t, cwd, "s1", "a0", "second-turn", ""); out != "" {
			t.Errorf("terminal stop blocked: %s", out)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.code == 0 || !strings.Contains(got.out, "more than one unverified record") {
		t.Fatalf("an implicit resolve settled on the turn of its unlocked read: code=%d %q", got.code, got.out)
	}
	for _, turn := range []string{"t", "second-turn"} {
		if !evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a0", TurnID: turn}) {
			t.Fatalf("the ambiguous resolve removed the verdict of turn %s", turn)
		}
	}
}

// CRW-1110 post-evaluation round (d2): a resolve that names no turn is a request for the one verdict of the agent, so it needs both
// places read completely. The same agent's other turn sits beside the full list in a directory the command cannot list: the
// request is not shown to be unique, and it resolves nothing.
func TestEvidenceCLIImplicitResolveRefusesWhenTheOverflowCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	cwd := subagentStopWorkspace(t)
	overflowSeed(t, cwd, state.MaxUnverifiedSubagents)
	for n := 1; n <= 3; n++ {
		subagentStopBlock(t, counterStop(t, cwd, "s1", "a0", "second-turn", ""), n)
	}
	if out := counterStop(t, cwd, "s1", "a0", "second-turn", ""); out != "" {
		t.Fatalf("terminal stop blocked: %s", out)
	}
	dir := filepath.Join(cwd, ".crw", evidence.OverflowSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("precondition: one overflow session directory: %v %v", entries, err)
	}
	unreadable := filepath.Join(dir, entries[0].Name())
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	receipt := filepath.Join(cwd, ".crw/evidence/parent.md")
	subagentStopPut(t, receipt, "parent verified")
	out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a0", Receipt: receipt, Cwd: cwd})
	if code == 0 {
		t.Fatalf("an implicit resolve settled on the main list's verdict although the overflow was unreadable: %q", out)
	}
	if !evidence.HasTombstone(cwd, "s1", evidence.Payload{AgentID: "a0", TurnID: "t"}) {
		t.Fatal("the unverifiable request removed the verdict of turn t")
	}
	// Naming the turn is a request that needs no uniqueness: it resolves the main list's verdict.
	turn := "t"
	if out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a0", Receipt: receipt, Cwd: cwd, TurnID: &turn}); code != 0 {
		t.Fatalf("a named turn was refused: %s", out)
	}
}

// CRW-1112 post-evaluation round (d1): .crw/evidence-overflow is a link to a directory of another store that holds the record of
// this very session, agent and turn. Neither the parent's resolve nor the child's late receipt may unlink it.
func TestOverflowLinkedAncestorIsNeverUnlinked(t *testing.T) {
	cwd, outside := subagentStopWorkspace(t), t.TempDir()
	real := filepath.Join(outside, "backup")
	// Record a verdict beside a full list in a store of its own, then link its overflow directory into this workspace.
	other := subagentStopWorkspace(t)
	overflowSeed(t, other, state.MaxUnverifiedSubagents)
	overflowExhaust(t, other, "a64")
	if err := os.Rename(filepath.Join(other, ".crw", evidence.OverflowSubdir), real); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".crw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(cwd, ".crw", evidence.OverflowSubdir)); err != nil {
		t.Fatal(err)
	}
	var external string
	_ = filepath.WalkDir(real, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			external = path
		}
		return nil
	})
	if external == "" {
		t.Fatal("precondition: the external record")
	}
	receipt := filepath.Join(cwd, ".crw/evidence/parent.md")
	subagentStopPut(t, receipt, "parent verified")
	turn := "t"
	if out, code := cli.RunEvidenceCLI(cli.EvidenceResolveArgs{Verb: "resolve", SessionID: "s1", AgentID: "a64", Receipt: receipt, Cwd: cwd, TurnID: &turn}); code == 0 {
		t.Fatalf("a record behind a link was resolved: %s", out)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("the CLI unlinked the external record: %v", err)
	}
	if out := counterStop(t, cwd, "s1", "a64", "t", "EVIDENCE_RECORDED: .crw/evidence/parent.md"); out != "" {
		t.Fatalf("the late receipt stop: %s", out)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("the late receipt unlinked the external record: %v", err)
	}
}
