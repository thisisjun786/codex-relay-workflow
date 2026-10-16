package roles

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/role/agents"
)

// TestEmbeddedAgentsUnchanged pins the bytes of the CXC role definitions to the cut: the overlay is appended to
// them and never edits them, so the sha-256 of each file is the one it had when the engine's overlays were written.
func TestEmbeddedAgentsUnchanged(t *testing.T) {
	cut := map[string]string{
		"architect.toml": "c285250433d4e6187bdf0bd73623e10bcf38d13789e5d9d8ff632d80d4fa2672",
		"executor.toml":  "4ee08fdb36e34adf2cee76a98ceb71f69de9cc6f6d7b8a13b5f398b3884ba04f",
		"explorer.toml":  "5c1e712b2c66fded83fead2fceb36d4e74fc40f9cc00fa798d9855704434f2c2",
		"reviewer.toml":  "e68ffbf7ea309245cb622a9cfad8a95d2c51c5dec6dacdff10bf1347c92a8bde",
	}
	entries, err := agents.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(cut) {
		t.Fatalf("the cut has %d role definitions, the directory has %d", len(cut), len(entries))
	}
	for name, want := range cut {
		raw, err := agents.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: sha-256 %s, the cut has %s", name, got, want)
		}
		// The registration embeds the same directory; the file on disk is what both read.
		onDisk, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "role", "agents", name))
		if err != nil || string(onDisk) != string(raw) {
			t.Errorf("%s: the embedded bytes differ from the file (%v)", name, err)
		}
	}
}

func TestOverlayAppendsToEmbeddedPrompt(t *testing.T) {
	for _, name := range Names {
		base, err := Base(name)
		if err != nil {
			t.Fatal(err)
		}
		overlay, err := Overlay(name, Options{})
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := Prompt(name, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(prompt, base) {
			t.Errorf("%s: the prompt does not start with the role definition", name)
		}
		if prompt != base+Separator+overlay {
			t.Errorf("%s: the prompt is not the definition, a blank line and the overlay", name)
		}
		if !strings.HasPrefix(overlay, "# CRW flow overlay: "+name+"\n") {
			t.Errorf("%s: the overlay does not open with its heading: %q", name, firstLine(overlay))
		}
		if !strings.HasSuffix(overlay, "\n") || strings.HasSuffix(overlay, "\n\n") {
			t.Errorf("%s: the overlay ends in exactly one newline", name)
		}
		// The overlay says what it adds on the points the design names.
		for _, point := range []string{"cwd", "output schema"} {
			if !strings.Contains(strings.ToLower(overlay), point) {
				t.Errorf("%s: the overlay does not say anything about %s", name, point)
			}
		}
	}
	if _, err := Prompt("child", Options{}); err == nil {
		t.Error("a relay role has no overlay")
	}
	// reviewer-final is a retired role key (CRW-1450): it has no definition, no overlay and no prompt.
	for _, part := range []func() (string, error){
		func() (string, error) { return Base("reviewer-final") },
		func() (string, error) { return Overlay("reviewer-final", Options{}) },
		func() (string, error) { return Prompt("reviewer-final", Options{}) },
	} {
		if _, err := part(); err == nil {
			t.Error("the retired reviewer-final key still has a part of a prompt")
		}
	}
	if _, err := overlays.ReadFile("reviewer-final.md"); err == nil {
		t.Error("the reviewer-final overlay file is still embedded")
	}
	// The second tier and the critic are built on the definitions of their families.
	for role, family := range map[string]string{"executor-t1": "executor", "critic": "reviewer"} {
		a, _ := Base(role)
		b, _ := Base(family)
		if a != b {
			t.Errorf("%s is built on the %s definition", role, family)
		}
	}
	// Executors read the crw-dev-* skills by name, and every skill the overlays name exists.
	executor, _ := Overlay("executor", Options{})
	for _, skill := range []string{"crw-dev", "crw-dev-frontend", "crw-dev-backend", "crw-dev-testing", "crw-dev-debugging", "crw-dev-scaffolding"} {
		if !strings.Contains(executor, "`"+skill+"`") {
			t.Errorf("the executor overlay does not name the %s skill", skill)
		}
	}
	for _, name := range Names {
		for _, opts := range []Options{{}, {QA: name == "executor"}} {
			overlay, _ := Overlay(name, opts)
			for _, skill := range regexp.MustCompile("`(crw-[a-z-]+)` skill|`(crw-dev[a-z-]*)`").FindAllStringSubmatch(overlay, -1) {
				skillName := skill[1] + skill[2]
				if _, err := os.Stat(filepath.Join(repoRoot(t), "plugins", "crw", "skills", skillName, "SKILL.md")); err != nil {
					t.Errorf("%s overlay names the skill %s, which does not exist: %v", name, skillName, err)
				}
			}
		}
	}
}

// TestOverlayReplacesLeafSentence: the overlay names the LEAF sentence of the definition and says what applies instead.
func TestOverlayReplacesLeafSentence(t *testing.T) {
	helper := regexp.MustCompile(`up to one helper in this call, and it must have agent_type 'explorer'`)
	none := regexp.MustCompile(`zero helpers in this call \(0\)`)
	for _, row := range []struct {
		role   string
		opts   Options
		helper bool
	}{
		{"executor", Options{}, true}, {"executor-t1", Options{}, true}, {"architect", Options{}, true},
		{"reviewer", Options{}, true},
		{"explorer", Options{}, false}, {"critic", Options{}, false}, {"executor", Options{QA: true}, false},
	} {
		base, _ := Base(row.role)
		overlay, err := Overlay(row.role, row.opts)
		if err != nil {
			t.Fatal(err)
		}
		label := row.role
		if row.opts.QA {
			label += " (QA)"
		}
		// A definition with the sentence is named by the sentence; the architect's has none and the overlay says so.
		if strings.Contains(base, LeafSentence) {
			if !strings.Contains(overlay, `begins "`+LeafSentence+`"`) {
				t.Errorf("%s: the overlay does not point at the LEAF sentence by its opening words", label)
			}
			if !strings.Contains(overlay, "does not apply to this engine call") {
				t.Errorf("%s: the overlay does not say the sentence is replaced", label)
			}
		} else if row.role != "architect" || !strings.Contains(overlay, "carries no leaf sentence") {
			t.Errorf("%s: the definition has no LEAF sentence and the overlay does not say so", label)
		}
		if got, want := NativeHelpers(row.role, row.opts), map[bool]int{true: 1, false: 0}[row.helper]; got != want {
			t.Errorf("%s: NativeHelpers gives %d, but the overlay allows %d", label, got, want)
		}
		switch {
		case row.helper && (!helper.MatchString(overlay) || none.MatchString(overlay)):
			t.Errorf("%s: the overlay must allow one explorer helper", label)
		case !row.helper && (!none.MatchString(overlay) || helper.MatchString(overlay)):
			t.Errorf("%s: the overlay must allow zero helpers", label)
		}
		if !strings.Contains(overlay, "CRW-SUBSPAWN-ALLOWED has no effect") {
			t.Errorf("%s: the overlay does not say CRW-SUBSPAWN-ALLOWED has no effect", label)
		}
	}
	for _, name := range []string{"executor", "reviewer", "explorer"} {
		base, _ := Base(name)
		if !strings.Contains(base, LeafSentence) {
			t.Errorf("the %s definition lost the LEAF sentence this package points at", name)
		}
	}
}

// TestQAOverlayForbidsOracleSpawn: the QA overlay does not start the two reviewer passes (the oracle passes of SP-QA-2)
// and asks for the captures. Since CRW-1220 the crw-qa skill calls them reviewer passes, so the overlay names the
// section by the heading the skill has.
func TestQAOverlayForbidsOracleSpawn(t *testing.T) {
	overlay, err := Overlay("executor", Options{QA: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Do not start the two reviewer passes of section 5 of the `crw-qa` skill (`## 5. Reviewer passes`) yourself",
		"SP-QA-2",
		"Do not call spawn_agent",
		"the receipt field `captures`",
		"A capture that is not listed in `captures` does not exist for the gate",
		"scratch worktree",
	} {
		if !strings.Contains(overlay, want) {
			t.Errorf("the QA overlay lacks %q", want)
		}
	}
	plain, _ := Overlay("executor", Options{})
	if plain == overlay || strings.Contains(plain, "SP-QA-2") || strings.Contains(plain, "reviewer passes") {
		t.Error("the ordinary executor overlay is not the QA overlay")
	}
	// The QA call is an executor call and nothing else.
	for _, role := range []string{"reviewer", "explorer", "architect"} {
		if _, err := Overlay(role, Options{QA: true}); err == nil {
			t.Errorf("%s has no QA overlay", role)
		}
	}
	// The guidance names the skill sections it points at.
	skill, err := os.ReadFile(filepath.Join(repoRoot(t), "plugins", "crw", "skills", "crw-qa", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, heading := range []string{"## 5. Reviewer passes", "## 6. Teardown receipts"} {
		if !strings.Contains(string(skill), "\n"+heading) {
			t.Errorf("the crw-qa skill has no %q the overlay points at", heading)
		}
	}
	// No word of the overlay still calls the passes by the name the skill no longer uses.
	if strings.Contains(strings.ToLower(overlay), "oracle pass") {
		t.Error("the QA overlay calls the passes oracle passes; section 5 of the crw-qa skill calls them reviewer passes")
	}
}

func firstLine(s string) string { line, _, _ := strings.Cut(s, "\n"); return line }

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// sectionOf is the text of the overlay section under the heading, up to the next heading of the same level.
func sectionOf(overlay, heading string) string {
	_, rest, ok := strings.Cut(overlay, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

// TestArchitectOverlayExecutorContract (CRW-1321): the architect plans for the executor that will carry the plan out, so its
// overlay states the executor contract the run enforces - the executor never commits, the run commits with a fixed message,
// the fields a RECEIPT has, done conditions that are checks of the committed HEAD or of the worktree, and generated assets
// planned with their generation and verification commands.
func TestArchitectOverlayExecutorContract(t *testing.T) {
	overlay, err := Overlay("architect", Options{})
	if err != nil {
		t.Fatal(err)
	}
	contract := sectionOf(overlay, "## Executor contract")
	if contract == "" {
		t.Fatal("the architect overlay has no \"## Executor contract\" section")
	}
	for _, want := range []string{
		"The executor never commits",
		"The run commits",
		"fixed message `crw <node> <call>`",
		"request id",
		"no commit message and no commit body",
		"RECEIPT",
		"`outcome`", "`files`", "`verification`", "`summary`", "`gap`", "`research_question`", "`plan_mismatch`",
		"committed HEAD",
		"state of the worktree",
		"`make gui`", "`crw-dev ci gui-drift`",
		"generation command and its verification command",
	} {
		if !strings.Contains(contract, want) {
			t.Errorf("the executor contract section lacks %q:\n%s", want, contract)
		}
	}
	// The executor overlay is where the commit rule is the executor's own; the architect's contract says the same thing.
	executor, _ := Overlay("executor", Options{})
	if !strings.Contains(executor, "makes the commit itself (`crw.commit`)") {
		t.Error("the executor overlay no longer says the run makes the commit; the architect's contract rests on it")
	}
}

// TestArchitectOverlayAuthorityBoundary (CRW-1321, CRW-1347): exceptions to repository rules, other issues' records, policy
// decisions and execution-environment prerequisites are not the architect's to decide. The overlay says they are raised as open
// questions with the scope notation of their kind, that a question does not stop the run (no hold), and describes the route an
// open question takes: never back to the architect, to the orchestrator for a notation of the four kinds, to an explorer otherwise;
// and that the answers come back to the architect's thread before the design audit, where it answers with the DESIGN again (CRW-1466).
func TestArchitectOverlayAuthorityBoundary(t *testing.T) {
	overlay, err := Overlay("architect", Options{})
	if err != nil {
		t.Fatal(err)
	}
	boundary := sectionOf(overlay, "## Authority boundary")
	if boundary == "" {
		t.Fatal("the architect overlay has no \"## Authority boundary\" section")
	}
	for _, want := range []string{
		"You do not decide",
		"an exception to a rule of the repository",
		"the record of another issue",
		"a policy decision",
		"a prerequisite of the execution environment",
		"`open_questions`",
		"does not stop the run",
		"not a hold",
		"explorer",
		"`design_question`",
		"the orchestrator",
		"`assumptions`",
		// the notation the router reads (ops.AuthorityScope): at the start of the scope, the token as given
		"`scope` that starts with the notation of its kind",
		"`authority:repo-rule`", "`authority:other-record`", "`authority:policy`", "`authority:environment`",
		// the route as ops.exploreRoute takes it (lane_decisions.go): no open question is the architect's to answer
		"What the run does with an open question:",
		"None is left to you to answer",
		"whatever `files_needed` lists",
		// the answers go back to the architect's thread before the critic (CRW-1466)
		"come back to your thread before the design audit",
		"you answer with the DESIGN again",
		"goes to no explorer",
		"non-blocking ask",
		"a missing or unknown notation among them",
		"beyond that cap",
		"and again before the audit of a re-plan",
		"each question's route and what it got",
	} {
		if !strings.Contains(boundary, want) {
			t.Errorf("the authority boundary section lacks %q:\n%s", want, boundary)
		}
	}
	// The section no longer says a question of the DESIGN is the architect's to answer or reaches the audit unanswered for that.
	for _, claim := range []string{"is the asker's to answer", "reaches the audit with no answer", "no route today hands it to a layer above the run", "open question today"} {
		if strings.Contains(boundary, claim) {
			t.Errorf("the authority boundary section says %q, which the route no longer does:\n%s", claim, boundary)
		}
	}
	// The section says plainly which questions go to the layers above the run: those with a notation and those beyond the explorer
	// cap, not every question the architect raises.
	for _, claim := range []string{"since it is a question for the layers above the run", "(none included)"} {
		if strings.Contains(boundary, claim) {
			t.Errorf("the authority boundary section says %q, which the route does not do or does not say plainly:\n%s", claim, boundary)
		}
	}
}

// TestCriticRoleHasOverlayAndDefinition (CRW-1434): the critic is the last role of an engine call. It has no definition
// file of its own: its thread is built on the reviewer definition (it spawns as agent_type reviewer), followed by its own
// overlay, which names the LEAF sentence and allows no helper.
func TestCriticRoleHasOverlayAndDefinition(t *testing.T) {
	found := false
	for _, name := range Names {
		if name == "critic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("critic is not a role of an engine call: %v", Names)
	}
	if Names[len(Names)-1] != "critic" {
		t.Errorf("the critic is not the last role, every older table keeps its rows: %v", Names)
	}
	overlay, err := Overlay("critic", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(overlay, "# CRW flow overlay: critic\n") {
		t.Errorf("the critic overlay does not open with its heading: %q", firstLine(overlay))
	}
	if !strings.Contains(overlay, `begins "`+LeafSentence+`"`) || !strings.Contains(overlay, "does not apply to this engine call") {
		t.Error("the critic overlay does not name the LEAF sentence and say it is replaced")
	}
	base, err := Base("critic")
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _ := Base("reviewer")
	if base != reviewer {
		t.Error("the critic is not built on the reviewer definition")
	}
	for _, want := range []string{"DESIGN_AUDIT", "PRIOR FINDINGS", "rollback", "same `ref`"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("the critic overlay lacks %q", want)
		}
	}
}

// TestReplanPacketAsksForTheWholeDesign (CRW-1462): the architect overlay has a re-plan section that says what the re-plan packet
// asks (internal/flow/workflows/packets_test.go pins the packet): the answer is the whole DESIGN, every decision and every issue of
// every node, the started ones as they were released, with the ids kept; it is not a delta.
func TestReplanPacketAsksForTheWholeDesign(t *testing.T) {
	overlay, err := Overlay("architect", Options{})
	if err != nil {
		t.Fatal(err)
	}
	replan := sectionOf(overlay, "## Re-plan")
	if replan == "" {
		t.Fatal("the architect overlay has no \"## Re-plan\" section")
	}
	for _, want := range []string{"the whole DESIGN", "every decision", "every issue of every node", "as they were released", "UNSTARTED NODES", "ids"} {
		if !strings.Contains(replan, want) {
			t.Errorf("the re-plan section lacks %q:\n%s", want, replan)
		}
	}
	if strings.Contains(replan, "delta") {
		t.Errorf("the re-plan section still speaks of a delta:\n%s", replan)
	}
}

// TestArchitectOverlayNamesTheBackgroundFindings (CRW-1466): background explorers run before the architect's first DESIGN, one for
// each node, and their answers come in the packet's BACKGROUND FINDINGS (internal/flow/cli/lanee2e_test.go pins the packet); the
// overlay says so, says they are the run's (not the one helper of the call) and that a re-plan has none.
func TestArchitectOverlayNamesTheBackgroundFindings(t *testing.T) {
	overlay, err := Overlay("architect", Options{})
	if err != nil {
		t.Fatal(err)
	}
	bg := sectionOf(overlay, "## Background findings")
	if bg == "" {
		t.Fatal("the architect overlay has no \"## Background findings\" section")
	}
	for _, want := range []string{"Background explorers run before you", "one question for each node", "BACKGROUND FINDINGS", "A re-plan has no background explorers"} {
		if !strings.Contains(bg, want) {
			t.Errorf("the background findings section lacks %q:\n%s", want, bg)
		}
	}
	if spawn := sectionOf(overlay, "## Native spawn"); !strings.Contains(spawn, "The background explorers are the run's, not yours") {
		t.Errorf("the native spawn section does not set the background explorers apart from the call's helper:\n%s", spawn)
	}
}

// TestRollbackIsAskedInRisksAndCheckedThere: the critic checks a plan's rollback, and the DESIGN has no field of its own for it, so
// the critic's rule names the field it reads (risks) and the architect overlay asks for the rollback there.
func TestRollbackIsAskedInRisksAndCheckedThere(t *testing.T) {
	critic, _ := Overlay("critic", Options{})
	architect, _ := Overlay("architect", Options{})
	const rule = "for a change to stored data or a behaviour change, `risks` must say how it rolls back"
	if !strings.Contains(critic, rule) {
		t.Errorf("the critic's rollback rule does not name the field it reads: want %q", rule)
	}
	if !strings.Contains(architect, "For a change to stored data or a behaviour change, say in `risks` how it rolls back") {
		t.Error("the architect overlay does not ask for the rollback in risks, which the critic checks")
	}
}

// CRW-1497: the role prompt (definition and overlay) is the fixed head of every call of the role, the same bytes on every run: it
// names no date, time, absolute path, run, call or turn of a run. The values of a call are in its task packet after "## Task".
func TestRoleHeadHasNoPerRunValues(t *testing.T) {
	perRun := map[string]*regexp.Regexp{
		"a date":                regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`),
		"a clock time":          regexp.MustCompile(`\b\d\d:\d\d:\d\d\b`),
		"an absolute path":      regexp.MustCompile(`(^|[\s"'(=])/(home|state|scratch|tmp|var|Users|opt|w)/`),
		"a run id":              regexp.MustCompile(`\brun-[0-9a-f]{6,}\b`),
		"a call id":             regexp.MustCompile(`\bc_[0-9a-f]{6,}\b`),
		"a step marker":         regexp.MustCompile(`/t\d+-a\d+\b`),
		"a crw-flow-op marker":  regexp.MustCompile(`crw-flow-op: \S`),
		"a thread or agent id":  regexp.MustCompile(`\b(thread|agent)-[0-9a-f]{8,}\b`),
		"a full commit sha":     regexp.MustCompile(`\b[0-9a-f]{40}\b`),
		"a home directory form": regexp.MustCompile(`~/`),
	}
	type variant struct {
		role string
		opts Options
	}
	var variants []variant
	for _, name := range Names {
		variants = append(variants, variant{name, Options{}})
	}
	variants = append(variants, variant{"executor", Options{QA: true}})
	for _, v := range variants {
		prompt, err := Prompt(v.role, v.opts)
		if err != nil {
			t.Fatalf("%s (qa %v): %v", v.role, v.opts.QA, err)
		}
		again, _ := Prompt(v.role, v.opts)
		if again != prompt {
			t.Errorf("%s (qa %v): two builds of the prompt differ", v.role, v.opts.QA)
		}
		for what, re := range perRun {
			if m := re.FindString(prompt); m != "" {
				t.Errorf("%s (qa %v): the role prompt names %s (%q), a per-run value", v.role, v.opts.QA, what, m)
			}
		}
	}
}

// CRW-1498: the executor overlays tell the executor to batch its tool calls: read every file it needs in one command, apply a
// multi-file edit with one script and run the target test in the same command as the edit. The rule is one bullet of the fixed
// head ("## Your thread", before the Skills bullet) of the executor and the second tier; the QA capture call has no edit to batch.
func TestExecutorOverlayTellsToReadOnceEditByOneScriptAndTestInTheSameCall(t *testing.T) {
	rules := []string{
		"read every file you need in one command",
		"apply a multi-file edit with one script",
		"run the target test in the same command as the edit",
	}
	var bullets []string
	for _, role := range []string{"executor", "executor-t1"} {
		overlay, err := Overlay(role, Options{})
		if err != nil {
			t.Fatal(err)
		}
		thread := sectionOf(overlay, "## Your thread")
		if thread == "" {
			t.Fatalf("%s: the overlay has no \"## Your thread\" section", role)
		}
		skills := strings.Index(thread, "\n- Skills:")
		if skills < 0 {
			t.Fatalf("%s: the \"## Your thread\" section has no Skills bullet", role)
		}
		var bullet string
		for _, line := range strings.Split(thread, "\n") {
			if strings.HasPrefix(line, "- ") && strings.Contains(line, rules[0]) {
				bullet = line
			}
		}
		if bullet == "" {
			t.Errorf("%s: no bullet of \"## Your thread\" says to %s", role, rules[0])
			continue
		}
		bullets = append(bullets, bullet)
		for _, rule := range rules {
			if !strings.Contains(bullet, rule) {
				t.Errorf("%s: the batching bullet does not say to %s: %q", role, rule, bullet)
			}
			if n := strings.Count(overlay, rule); n != 1 {
				t.Errorf("%s: the overlay says %q %d times, want once", role, rule, n)
			}
		}
		if at := strings.Index(thread, bullet); at > skills {
			t.Errorf("%s: the batching bullet comes after the Skills bullet", role)
		}
	}
	if len(bullets) == 2 && bullets[0] != bullets[1] {
		t.Errorf("the executor and the second tier carry different batching bullets:\n%q\n%q", bullets[0], bullets[1])
	}
	qa, err := Overlay("executor", Options{QA: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		if strings.Contains(qa, rule) {
			t.Errorf("the QA overlay carries the batching rule %q", rule)
		}
	}
}
