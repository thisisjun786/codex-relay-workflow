//go:build dev

package ci

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The child's procedure stays in crw-run; crw-next is part of crw-status and crw-logic part of crw-check (CRW-1218). The
// initiative procedure and the midpoint check were crw-run-initiative's, the initiative lead's skill; CRW-1352 retired that
// skill with the layer, and they are the parent's section at the end of crw-run, which links the references below it.
// These checks pin that layout.

const (
	runSkillDir        = "plugins/crw/skills/crw-run"
	runInitiativeDir   = "plugins/crw/skills/crw-run-initiative"
	runSkillFile       = runSkillDir + "/SKILL.md"
	parentSectionTitle = "The parent: the repository session"
	relaySpecsPath     = "internal/relay/argparse/specs.json"
)

// runSkillParts is crw-run's SKILL.md split at the parent's section, the last section of the skill: the child's
// procedure before it and the parent's section from its heading on.
func runSkillParts(t *testing.T) (child, parent string) {
	t.Helper()
	run := splitRead(t, runSkillFile)
	i := strings.Index(run, "\n## "+parentSectionTitle+"\n")
	if i < 0 {
		t.Fatalf("%s has no section %q", runSkillFile, parentSectionTitle)
	}
	return run[:i+1], run[i+1:]
}

func splitRead(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func splitExists(rel string) bool {
	_, err := os.Stat(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	return err == nil
}

// TestRunInitiativePointerSingleLine: crw-run-initiative is gone and nothing in crw-run points at it; the initiative
// procedure and the midpoint check are crw-run's parent section, which links the references below crw-run, and no
// heading of the child's procedure holds them.
func TestRunInitiativePointerSingleLine(t *testing.T) {
	if splitExists(runInitiativeDir) {
		t.Errorf("%s exists: the skill was retired (CRW-1352)", runInitiativeDir)
	}
	run := splitRead(t, runSkillFile)
	if strings.Contains(run, "crw-run-initiative") {
		t.Errorf("%s still names crw-run-initiative", runSkillFile)
	}
	child, parent := runSkillParts(t)
	if strings.Count(run, "\n## "+parentSectionTitle+"\n") != 1 {
		t.Errorf("%s holds the parent's section more than once", runSkillFile)
	}
	for _, heading := range headingsOf(child) {
		if strings.Contains(strings.ToLower(heading), "initiative") || strings.Contains(strings.ToLower(heading), "midpoint") {
			t.Errorf("%s keeps the heading %q outside the parent's section", runSkillFile, heading)
		}
	}
	for _, gone := range []string{"**Supervise an initiative:**", "**Status only:**", "midpoint-check.md"} {
		if strings.Contains(child, gone) {
			t.Errorf("the child's procedure of %s still contains %q", runSkillFile, gone)
		}
	}
	for _, want := range []string{"](references/initiative-supervision.md)", "](references/midpoint-check.md)", "](../crw-canon/references/hierarchy.md#the-levels)"} {
		if !strings.Contains(parent, want) {
			t.Errorf("the parent's section of %s does not link %s", runSkillFile, want)
		}
	}
	if splitExists("plugins/crw/skills/crw-run-project") {
		t.Error("plugins/crw/skills/crw-run-project exists: crw-run is the child's skill")
	}
}

// runCommandSequenceBefore is the order in which crw-run/SKILL.md names a relay command (a name of
// internal/relay/argparse/specs.json written in code font) at a52e7ff09, before the split. The
// initiative procedure named none of them, so the sequence is the project procedure's.
var runCommandSequenceBefore = []string{
	"doctor", "dispositions-show", "managed-start", "managed-start", "dag-release", "managed-start", "managed-start",
	"dag-bundle-candidates", "managed-start", "dag-ready", "dag-integration-observe", "dag-accept", "dag-conflict-sweep",
	"dag-ready", "merge-evidence", "decision-reply", "dag-correct", "admit-turn", "linkage-handover", "linkage-completion",
	"relationship-close-merged", "supervisor-standing",
}

// relayCommandSequence is the relay commands text names, in order of appearance.
func relayCommandSequence(t *testing.T, text string) []string {
	t.Helper()
	var specs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(splitRead(t, relaySpecsPath)), &specs); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, spec := range specs {
		if spec.Name != "" {
			names = append(names, regexp.QuoteMeta(spec.Name))
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	pattern := regexp.MustCompile("`(?:crw )?(" + strings.Join(names, "|") + ")[ `]")
	var out []string
	for _, m := range pattern.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestRunCommandSequenceUnchanged: crw-run names the same relay commands in the same order as before the split.
func TestRunCommandSequenceUnchanged(t *testing.T) {
	// CRW-1222: the section on a flow run names dag-coordinator-claim once, ahead of the commands of the issue-child procedure,
	// to say that this thread never claims the epoch of a plan a run holds (TestRunProjectNeverClaimsRunEpoch); every other
	// command stays where it was.
	child, parent := runSkillParts(t)
	expectEqual(t, "relay commands of "+runSkillFile, relayCommandSequence(t, child), append([]string{"dag-coordinator-claim"}, runCommandSequenceBefore...))
	// The parent sends a project to its child; it runs none of the project's release or close commands itself.
	for _, command := range relayCommandSequence(t, parent) {
		switch command {
		case "managed-start", "dag-release", "dag-correct", "dag-accept", "relationship-close-merged":
			t.Errorf("the parent's section of %s runs the project command %s", runSkillFile, command)
		}
	}
}

// TestAbsorbedSkillsMoved: crw-next and crw-logic have no directory, and nothing in the repository
// (links, tests, the inventory map, the skillport records, the manifests) names them.
func TestAbsorbedSkillsMoved(t *testing.T) {
	root := repoRoot()
	for _, name := range []string{"crw-next", "crw-logic"} {
		if splitExists("plugins/crw/skills/" + name) {
			t.Errorf("plugins/crw/skills/%s still exists", name)
		}
		if splitExists("port/cxc/records/" + name + ".json") {
			t.Errorf("port/cxc/records/%s.json still exists", name)
		}
	}
	// The end condition the CRW-1218 comment adds: the merge leaves 34 skill directories or fewer.
	entries, err := os.ReadDir(filepath.Join(root, "plugins/crw/skills"))
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, entry := range entries {
		if entry.IsDir() {
			dirs++
		}
	}
	if dirs > 34 {
		t.Errorf("plugins/crw/skills has %d skill directories, want at most 34", dirs)
	}
	files, err := repositoryFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`\bcrw-(?:next|logic)\b`)
	self := "internal/dev/ci/run_split_test.go"
	for _, name := range files {
		switch {
		case name == self, strings.HasPrefix(name, "testdata/"), strings.HasPrefix(name, "docs/port-cxc/known-defects/"):
			continue
		case !(strings.HasPrefix(name, "plugins/") || strings.HasPrefix(name, "docs/") || strings.HasPrefix(name, "port/") ||
			strings.HasPrefix(name, "internal/") || strings.HasPrefix(name, "cmd/") || name == "README.md"):
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if named.MatchString(line) {
				t.Errorf("%s:%d names an absorbed skill: %s", name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestMidpointTriggerSingleOwner: the midpoint check has one owner, the parent's section of crw-run: its trigger word is in
// crw-run's description alone, its procedure is in that section and one reference, and crw-status holds neither.
func TestMidpointTriggerSingleOwner(t *testing.T) {
	descriptions, err := skillDescriptions(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	for name, description := range descriptions {
		has := strings.Contains(description, "중간점검")
		if has != (name == "crw-run") {
			t.Errorf("%s: the trigger 중간점검 in the description is %v, want it in crw-run alone", name, has)
		}
	}
	const heading = "Long-run midpoint check"
	owners := 0
	err = filepath.WalkDir(filepath.Join(repoRoot(), "plugins/crw/skills"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) != "SKILL.md" {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, h := range headingsOf(string(data)) {
			if h == heading {
				owners++
				if filepath.Base(filepath.Dir(path)) != "crw-run" {
					t.Errorf("%s holds the heading %q", path, heading)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Errorf("%d SKILL.md files hold the heading %q, want one (crw-run)", owners, heading)
	}
	if _, parent := runSkillParts(t); !strings.Contains(parent, "\n### "+heading+"\n") {
		t.Errorf("the heading %q is not in the parent's section of %s", heading, runSkillFile)
	}
	if splitExists("plugins/crw/skills/crw-status/references/midpoint-check.md") {
		t.Error("crw-status still has references/midpoint-check.md")
	}
	if !splitExists(runSkillDir + "/references/midpoint-check.md") {
		t.Error("the midpoint reference is not under crw-run/references")
	}
	status := splitRead(t, "plugins/crw/skills/crw-status/SKILL.md")
	for _, heading := range headingsOf(status) {
		if strings.Contains(strings.ToLower(heading), "checkpoint") {
			t.Errorf("crw-status keeps the heading %q: status is read-only and has no checkpoint branch", heading)
		}
	}
	if strings.Contains(status, "midpoint-check.md") {
		t.Error("crw-status links the midpoint reference")
	}
}

// checkpointCountNames reads the JSON names of the counts crw manage checkpoint reports and the default thresholds
// from the command's source, so the initiative procedure is checked against the command it consumes.
func checkpointCountNames(t *testing.T) (names map[string]bool, thresholds map[string]string) {
	t.Helper()
	src := splitRead(t, "internal/manage/checkpoint.go")
	block := regexp.MustCompile(`(?s)type CheckpointCounts struct \{(.*?)\n\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("internal/manage/checkpoint.go has no CheckpointCounts struct")
	}
	names = map[string]bool{}
	for _, m := range regexp.MustCompile("`json:\"([a-z0-9_]+)\"`").FindAllStringSubmatch(block[1], -1) {
		names[m[1]] = true
	}
	def := regexp.MustCompile(`Merges: (\d+), RunningHours: (\d+), NeedsChangesOrSplit: (\d+), PairEvalP0P1: (\d+), BacklogNet: (\d+)`).FindStringSubmatch(src)
	if def == nil {
		t.Fatal("internal/manage/checkpoint.go has no default thresholds line")
	}
	thresholds = map[string]string{"merges": def[1], "hours": def[2], "needs": def[3], "pair": def[4], "backlog": def[5]}
	return names, thresholds
}

// TestInitiativeCheckpointAggregatesPerProjectReports: crw manage checkpoint reports one project at a time (its
// manage test pins that two projects at ten merges each are both not due), so the parent's midpoint
// procedure must say how the approved projects' reports become one crossing and how one check is recorded for all of
// them. The count names it sums are the command's real count names, and its thresholds are the command's defaults.
func TestInitiativeCheckpointAggregatesPerProjectReports(t *testing.T) {
	_, skill := runSkillParts(t)
	section, ok := reportSectionOf(skill, "Long-run midpoint check")
	if !ok {
		t.Fatal("the parent's section of crw-run has no Long-run midpoint check section")
	}
	counts, thresholds := checkpointCountNames(t)
	for _, name := range []string{"merges_since_checkpoint", "needs_changes_or_split_2h", "pair_eval_p0p1_since_checkpoint", "backlog_net_4h", "milestone_integrated", "running_hours"} {
		if !counts[name] {
			t.Errorf("the command no longer reports the count %s: update the test with the procedure", name)
		}
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf("the midpoint procedure does not name the count `%s` it aggregates", name)
		}
	}
	for _, phrase := range []string{
		"one report per project",
		"no initiative aggregate",
		"sum",
		"never the initiative's answer",
		"checkpoint record --project",
		"every approved project",
	} {
		if !strings.Contains(section, phrase) {
			t.Errorf("the midpoint procedure does not say %q", phrase)
		}
	}
	words := map[string]string{"20": "twenty", "8": "eight", "3": "three", "2": "two", "15": "fifteen"}
	for key, want := range map[string]string{"merges": " merges", "hours": " continuous hours", "needs": " or more needs-changes", "pair": " or more P0 or P1", "backlog": " issues in the last four hours"} {
		if !strings.Contains(strings.ToLower(section), words[thresholds[key]]+strings.ToLower(want)) {
			t.Errorf("the midpoint procedure's %s threshold does not match the command's default %s", key, thresholds[key])
		}
	}
}

// midpointBullets is each top-level bullet of a section with its indented continuation lines, joined by spaces.
func midpointBullets(section string) []string {
	var bullets []string
	open := false
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			bullets = append(bullets, strings.TrimPrefix(line, "- "))
			open = true
		case open && strings.HasPrefix(line, "  ") && strings.TrimSpace(line) != "":
			bullets[len(bullets)-1] += " " + strings.TrimSpace(line)
		default:
			open = false
		}
	}
	return bullets
}

var backtickedName = regexp.MustCompile("`([a-z0-9_]+)`")

// TestInitiativeCheckpointSumsTheSignedBacklogBeforeJudging: `backlog_net_4h` is a signed net change (created minus
// closed in the window, checkpointBacklogNet), so one project's backlog reason is never the initiative's crossing.
// Project A at +16 and project B at -16 with the same baseline net zero over the approved set although A's report names
// the signal; A at +10 and B at +6 net sixteen although neither report names it. The procedure is applied to both
// scenarios as written: the signals its promotion bullet excepts, then the sum of the approved projects' backlog. The
// bullet that sums it says the count is signed and that an unmeasured one is judged from the coordination record,
// because a partial signed sum bounds nothing.
func TestInitiativeCheckpointSumsTheSignedBacklogBeforeJudging(t *testing.T) {
	_, parent := runSkillParts(t)
	section, ok := reportSectionOf(parent, "Long-run midpoint check")
	if !ok {
		t.Fatal("the parent's section of crw-run has no Long-run midpoint check section")
	}
	_, thresholds := checkpointCountNames(t)
	threshold := 0
	for _, r := range thresholds["backlog"] {
		threshold = threshold*10 + int(r-'0')
	}
	var promote, backlog string
	for _, bullet := range midpointBullets(section) {
		if promote == "" && strings.Contains(bullet, "so the initiative is due") {
			promote = bullet
			continue
		}
		if backlog == "" && strings.Contains(bullet, "`backlog_net_4h`") && strings.Contains(bullet, "sum") {
			backlog = bullet
		}
	}
	if promote == "" {
		t.Fatal("the midpoint procedure has no bullet that makes one project's reason the initiative's crossing")
	}
	clause := regexp.MustCompile(`other than (.*?) is a crossing`).FindStringSubmatch(promote)
	if clause == nil {
		t.Fatalf("the promotion bullet does not say which signals it excepts (\"other than ... is a crossing\"): %s", promote)
	}
	excepted := map[string]bool{}
	for _, m := range backtickedName.FindAllStringSubmatch(clause[1], -1) {
		excepted[m[1]] = true
	}
	for _, monotone := range []string{"merges_since_checkpoint", "needs_changes_or_split_2h", "pair_eval_p0p1_since_checkpoint", "milestone_integrated"} {
		if excepted[monotone] {
			t.Errorf("the promotion bullet excepts `%s`, a count that never falls, so one project's crossing is the initiative's", monotone)
		}
	}
	if !excepted["running_hours"] || !excepted["backlog_net_4h"] {
		t.Errorf("the promotion bullet does not except both `running_hours` and `backlog_net_4h`: %s", promote)
	}
	if backlog == "" {
		t.Error("no bullet sums `backlog_net_4h` over the approved projects")
	} else {
		for _, phrase := range []string{"signed", "every approved project", "coordination record", "fifteen"} {
			if !strings.Contains(backlog, phrase) {
				t.Errorf("the signed backlog bullet does not say %q: %s", phrase, backlog)
			}
		}
	}

	for _, scenario := range []struct {
		name string
		a, b int
		want bool
	}{
		{"A +16, B -16", 16, -16, false},
		{"A +10, B +6", 10, 6, true},
		{"A +16, B +0", 16, 0, true},
	} {
		due := false
		for _, net := range []int{scenario.a, scenario.b} {
			if net > threshold && !excepted["backlog_net_4h"] {
				due = true // the project's backlog reason promoted as the initiative's crossing
			}
		}
		if !due && backlog != "" {
			due = scenario.a+scenario.b > threshold
		}
		if due != scenario.want {
			t.Errorf("%s: the procedure as written judges due=%v, want %v (the approved set's signed net is %d against more than %d)", scenario.name, due, scenario.want, scenario.a+scenario.b, threshold)
		}
	}
}

// TestEntryPointsNameTheSkillOwners: the planning owner table sends an initiative's execution to the parent's section of
// crw-run, and the plugin's user-facing description names the parent's work and no deleted skill (CRW-1218 d3, CRW-1352).
func TestEntryPointsNameTheSkillOwners(t *testing.T) {
	integrations := "plugins/crw/skills/crw-plan/references/integrations.md"
	found := false
	for _, line := range strings.Split(splitRead(t, integrations), "\n") {
		if !strings.HasPrefix(line, "|") || !strings.Contains(line, "initiative's approved projects") {
			continue
		}
		found = true
		if !strings.Contains(line, "](../../crw-run/SKILL.md#the-parent-the-repository-session)") {
			t.Errorf("%s: the initiative execution row does not name the parent's section of crw-run as its owner: %s", integrations, line)
		}
	}
	if !found {
		t.Errorf("%s has no row for an initiative's approved projects", integrations)
	}
	var manifest struct {
		Interface struct {
			LongDescription string `json:"longDescription"`
		} `json:"interface"`
	}
	if err := json.Unmarshal([]byte(splitRead(t, "plugins/crw/.codex-plugin/plugin.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	long := manifest.Interface.LongDescription
	if long == "" {
		t.Fatal("plugins/crw/.codex-plugin/plugin.json has no interface.longDescription")
	}
	if strings.Contains(long, "Run Initiative") || !strings.Contains(long, "the parent's project order") {
		t.Error("the plugin longDescription still advertises Run Initiative, or does not name the parent's work in Run")
	}
	if strings.Contains(long, "Logic") {
		t.Error("the plugin longDescription still advertises Logic, a skill that was merged into Check")
	}
}

// pinnedRunPath is a path below crw-run that Go code or a Go test pins.
var pinnedRunPath = regexp.MustCompile(`crw-run/((?:references|scripts)/[A-Za-z0-9_./*-]*[A-Za-z0-9*]|SKILL\.md)`)

// TestRunSplitPinnedPathsResolve: the crw-run paths the Go code pins still resolve (no path moved), the links crw-run
// must keep resolve, and the parent's section links its references below crw-run.
func TestRunSplitPinnedPathsResolve(t *testing.T) {
	root := repoRoot()
	pinned := []string{
		"plugins/skill_assets.go", "internal/skill/common.go", "internal/dev/ci/contracts.go", "internal/dev/ci/dispatch_cases.go",
		"internal/dev/ci/plugin_test.go", "internal/relay/dagsched/outbox_docs_test.go", "internal/relay/dagsched/sweep_docs_test.go",
		"internal/relay/dagsched/policy_docs_test.go", "internal/relay/delivery/marker.go",
	}
	for _, file := range pinned {
		matches := pinnedRunPath.FindAllStringSubmatch(splitRead(t, file), -1)
		if file == "internal/dev/ci/plugin_test.go" || file == "internal/dev/ci/contracts.go" {
			// These pin the skill directory by a prefix, or build fixture files below it that exist only in a
			// temporary checkout; the skill itself must still be the one they name.
			if !strings.Contains(splitRead(t, file), "crw-run") {
				t.Errorf("%s no longer names crw-run", file)
			}
			continue
		}
		if len(matches) == 0 {
			t.Errorf("%s names no crw-run path: the pin moved", file)
		}
		for _, m := range matches {
			if m[1] == "SKILL.md" || !strings.Contains(m[1], "*") {
				if !splitExists(runSkillDir + "/" + m[1]) {
					t.Errorf("%s pins crw-run/%s, which does not exist", file, m[1])
				}
				continue
			}
			if found, _ := filepath.Glob(filepath.Join(root, runSkillDir, filepath.FromSlash(m[1]))); len(found) == 0 {
				t.Errorf("%s pins crw-run/%s, which matches no file", file, m[1])
			}
		}
	}
	run := splitRead(t, runSkillFile)
	cache := map[string]map[string]bool{}
	for _, link := range []string{"references/relay.md#the-coordination-summary-of-a-dag-plan", "references/region-grades.md#read-the-merge-order"} {
		if !strings.Contains(run, "]("+link+")") {
			t.Errorf("%s does not keep the link %s", runSkillFile, link)
			continue
		}
		file, fragment, _ := strings.Cut(link, "#")
		anchors, err := anchorsOf(cache, filepath.Join(root, runSkillDir, filepath.FromSlash(file)))
		if err != nil || !anchors[fragment] {
			t.Errorf("%s: the anchor #%s does not resolve in %s (%v)", runSkillFile, fragment, file, err)
		}
	}
	_, parent := runSkillParts(t)
	links := regexp.MustCompile(`\]\((references/[^)#]+)(#[^)]*)?\)`).FindAllStringSubmatch(parent, -1)
	if len(links) == 0 {
		t.Errorf("the parent's section of %s links nothing below references/", runSkillFile)
	}
	for _, m := range links {
		target := filepath.Join(root, runSkillDir, filepath.FromSlash(m[1]))
		if _, err := os.Stat(target); err != nil {
			t.Errorf("the parent's section links %s, which does not exist", m[1])
			continue
		}
		if m[2] != "" {
			if anchors, err := anchorsOf(cache, target); err != nil || !anchors[strings.TrimPrefix(m[2], "#")] {
				t.Errorf("the parent's section: the anchor %s does not resolve in %s", m[2], m[1])
			}
		}
	}
	// references/ and scripts/ stay below crw-run.
	for _, dir := range []string{"references", "scripts"} {
		if !splitExists(runSkillDir + "/" + dir) {
			t.Errorf("%s/%s is gone", runSkillDir, dir)
		}
	}
}
