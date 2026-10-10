package spawn

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1114: the role/spawn sweep. Each test reproduces one known-defects item of the oracle that the port kept and now fixes.

// spawnSweepSkills is a resolved skills directory named name holding crw-dev and crw-search with frontmatter.
func spawnSweepSkills(t *testing.T, name string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	spawnHookMust(t, err)
	dir := filepath.Join(base, name)
	for _, folder := range []string{"crw-dev", "crw-search"} {
		spawnHookMust(t, os.MkdirAll(filepath.Join(dir, folder), 0o755))
		spawnHookMust(t, os.WriteFile(filepath.Join(dir, folder, "SKILL.md"), []byte("---\nname: "+folder+"\ndescription: \"The "+folder+" skill\"\n---\nbody\n"), 0o644))
	}
	return dir
}

// known-defects.md:854: a load link only for a regular skill file inside the skills directory.
func TestSweepSkillLinkNeedsARegularContainedFile(t *testing.T) {
	dir := spawnSweepSkills(t, "skills")
	spawnHookMust(t, os.Remove(filepath.Join(dir, "crw-dev", "SKILL.md")))
	spawnHookMust(t, os.Mkdir(filepath.Join(dir, "crw-dev", "SKILL.md"), 0o755))
	if got := NormalizeSkillMentions("use $crw-dev now", dir); got != "use $crw-dev now" {
		t.Errorf("a directory named SKILL.md got a load link: %q", got)
	}
	outside := filepath.Join(filepath.Dir(dir), "outside", "crw-qa")
	spawnHookMust(t, os.MkdirAll(outside, 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(outside, "SKILL.md"), []byte("x"), 0o644))
	spawnHookMust(t, os.Symlink(outside, filepath.Join(dir, "crw-qa")))
	if got := NormalizeSkillMentions("use $crw-qa now", dir); got != "use $crw-qa now" {
		t.Errorf("a skill outside the skills directory got a load link: %q", got)
	}
	// A standalone link whose target is a directory named SKILL.md is repaired to the real skill.
	dirTarget := filepath.Join(filepath.Dir(dir), "fake", "SKILL.md")
	spawnHookMust(t, os.MkdirAll(dirTarget, 0o755))
	if got := NormalizeSkillMentions("[$crw-search](skill://"+dirTarget+")", dir); got != "[$crw-search](skill://"+filepath.Join(dir, "crw-search", "SKILL.md")+")" {
		t.Errorf("a link to a directory named SKILL.md was kept: %q", got)
	}
	if got := NormalizeSkillMentions("use $crw-search", dir); got != "use [$crw-search](skill://"+filepath.Join(dir, "crw-search", "SKILL.md")+")" {
		t.Errorf("a regular skill lost its link: %q", got)
	}
}

// known-defects.md:855: an escaped mention and one inside a token are not rewritten; the documented forms still are.
func TestSweepBareMentionsRespectEscapesAndTokenBoundaries(t *testing.T) {
	dir := spawnSweepSkills(t, "skills")
	link := "[$crw-dev](skill://" + filepath.Join(dir, "crw-dev", "SKILL.md") + ")"
	for in, want := range map[string]string{
		"prefix$crw-dev":    "prefix$crw-dev",
		`\$crw-dev`:         `\$crw-dev`,
		"a_$crw-dev":        "a_$crw-dev",
		"use $crw-dev":      "use " + link,
		"$crw-dev first":    link + " first",
		"use $crw:crw-dev.": "use " + link + ".",
		"(see $crw-dev)":    "(see " + link + ")",
	} {
		if got := NormalizeSkillMentions(in, dir); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

// known-defects.md:856: a skills directory whose path cannot stand as a raw Markdown link target keeps the plugin mention.
func TestSweepUnsafeLinkTargetsKeepThePluginMention(t *testing.T) {
	for _, name := range []string{"sk<ills>", `sk"ills`, "sk'ills", "sk`ills", `sk\ills`} {
		dir := spawnSweepSkills(t, name)
		if got := NormalizeSkillMentions("use $crw-dev", dir); got != "use $crw:crw-dev" {
			t.Errorf("%q: got %q", name, got)
		}
	}
}

// known-defects.md:936 and :937: catalog metadata comes from the frontmatter only, and the entry names the folder the self-load
// path uses.
func TestSweepCatalogReadsFrontmatterAndNamesTheFolder(t *testing.T) {
	dir := spawnSweepSkills(t, "skills")
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "crw-dev", "SKILL.md"), []byte("---\nname: Fancy Dev\ndescription: \"Develops\"\n---\nname: body-name\n"), 0o644))
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "crw-search", "SKILL.md"), []byte("# Search\nname: from-the-body\ndescription: body text\n"), 0o644))
	spawnHookMust(t, os.MkdirAll(filepath.Join(dir, "crw-qa"), 0o755))
	spawnHookMust(t, os.WriteFile(filepath.Join(dir, "crw-qa", "SKILL.md"), []byte("---\nname:\ndescription: next line\n---\n"), 0o644))
	got := BuildLeafSkillCatalog(dir)
	want := "Available skills (self-load from " + dir + "/<name>/SKILL.md):\n- crw-dev: Develops"
	if got != want {
		t.Errorf("catalog:\n got %q\nwant %q", got, want)
	}
}

// known-defects.md:938: the skill file is opened without blocking, so a FIFO put in place of the checked file cannot stop the hook.
func TestSweepSkillOpenDoesNotBlockOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	spawnHookMust(t, syscall.Mkfifo(filepath.Join(dir, "SKILL.md"), 0o644))
	root, err := os.OpenRoot(dir)
	spawnHookMust(t, err)
	defer root.Close()
	done := make(chan bool, 1)
	go func() {
		f, err := spawnInlineOpenSkill(root, "SKILL.md")
		if err == nil {
			f.Close()
		}
		done <- err == nil
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
}

// known-defects.md:1088: the review keywords match at a word start, so "preview" is not a review; the explicit rules come first.
func TestSweepReviewKeywordsMatchAtAWordStart(t *testing.T) {
	for message, want := range map[string]role.RoleName{
		"preview the diff":       role.Explorer,
		"unverifying the claims": role.Explorer,
		"please review the diff": role.Reviewer,
		"code-review this":       role.Reviewer,
		"Reviewer pass":          role.Reviewer,
		"run the verification":   role.Reviewer,
		"코드리뷰 부탁":                role.Reviewer,
		"red team it":            role.Reviewer,
	} {
		if got := InferRole(nil, message); got != want {
			t.Errorf("%q: %s, want %s", message, got, want)
		}
	}
	if got := InferRole("worker", "review it"); got != role.Executor {
		t.Errorf("explicit worker: %s", got)
	}
	if got := InferRole("explorer", "CRW-ROLE: architect\nTASK: preview"); got != role.Architect {
		t.Errorf("CRW-ROLE marker: %s", got)
	}
}

// known-defects.md:1133: a null message beside valid text items is no message: the items are guarded.
func TestSweepNullMessageDoesNotHideItems(t *testing.T) {
	rig := spawnHookNewRig(t, nil, spawnHookCase{})
	got := RunSpawnAttachHook(`{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":"rec-s1","cwd":`+strconv.Quote(rig.ws)+
		`,"tool_input":{"message":null,"items":[{"type":"text","text":"do the work"}]}}`, rig.env)
	if !strings.Contains(got, `"items":[{"type":"text","text":"[CRW-SUBAGENT-SCOPE]`) || !strings.Contains(got, `do the work"}]`) {
		t.Fatalf("null message with items = %.300q", got)
	}
}

// spawnSweepGatePlan writes a final-gate plan with the given criteria JSON and a receipt of identity.
func spawnSweepGatePlan(t *testing.T, cwd, criteria string, identity source.Identity) {
	t.Helper()
	receipt, err := json.Marshal(map[string]any{"kind": "test", "sourceIdentity": identity})
	spawnHookMust(t, err)
	spawnFinalGateTestWrite(t, cwd, ".crw/sessions/sess-1.json", `{"sessionId":"sess-1","slug":"demo"}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/goalplans/demo/goalplan.json", `{"criteria":`+criteria+`,"finalGate":{"testReceiptPath":".crw/evidence/test.json"}}`)
	spawnFinalGateTestWrite(t, cwd, ".crw/evidence/test.json", string(receipt))
}

// known-defects.md:1233: a recognized final-gate plan whose criteria cannot be read is a precondition that is not in place.
func TestSweepMalformedCriteriaAreReported(t *testing.T) {
	identity := source.Identity{Kind: source.KindResolved, CommitSha: "aaaaaaa"}
	capture := func(string) source.Identity { return identity }
	for _, criteria := range []string{`"web"`, `{"surface":"web"}`, `[5]`, `[{"surface":7}]`} {
		cwd := spawnFinalGateTestTree(t)
		spawnSweepGatePlan(t, cwd, criteria, identity)
		if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, capture); got.OK || !strings.Contains(got.Reason, "criteria") {
			t.Errorf("criteria %s: %+v", criteria, got)
		}
	}
	for _, criteria := range []string{`[]`, `[{"id":"c1"}]`, `[{"surface":"logic"}]`} {
		cwd := spawnFinalGateTestTree(t)
		spawnSweepGatePlan(t, cwd, criteria, identity)
		if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, capture); !got.OK {
			t.Errorf("criteria %s: %+v", criteria, got)
		}
	}
}

// known-defects.md:1237: at a marked gate, an unavailable identity on either side cannot verify the receipt.
func TestSweepUnavailableIdentityCannotVerifyAReceipt(t *testing.T) {
	resolved := source.Identity{Kind: source.KindResolved, CommitSha: "aaaaaaa"}
	unavailable := source.Identity{Kind: source.KindUnavailable, CommitSha: ""}
	for _, c := range []struct {
		name             string
		receipt, current source.Identity
	}{{"receipt unavailable", unavailable, resolved}, {"tree unavailable", resolved, unavailable}} {
		cwd := spawnFinalGateTestTree(t)
		spawnSweepGatePlan(t, cwd, `[]`, c.receipt)
		current := c.current
		if got := CheckFinalGatePrereqs(spawnFinalGateTestPacket, "sess-1", cwd, func(string) source.Identity { return current }); got.OK || !strings.Contains(got.Reason, "cannot be verified") {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}
