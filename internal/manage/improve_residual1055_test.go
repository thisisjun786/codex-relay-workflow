package manage

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The CRW-1055 tests pin the answers the issue asks to be decided after the propose counts were
// made sighting based (CRW-988): what a first migration of a multi-origin record counts, what a
// migration loses, what a posted draft keeps, where the candidates a rerun cap cut can still be
// drafted, and how the comparison helpers treat values a decoder never produces on its own.

// improveResidual1055Multi is one split record that names three origins, as a record merged from
// three events does.
func improveResidual1055Multi() improveRecord {
	record := improveProposeTestSplit("project-a", "size overrun", "rel-a")
	record.Evidence = []string{"events:rel-a", "events:rel-b", "events:rel-c"}
	record.Count = 3
	return record
}

// TestImproveResidual1055MultiOriginMigrationCountsWhatAFreshDraftCounts covers item 1: a draft
// without the improve item holds one sighting for a record, named by the record's where. When the
// record now names three origins, the first origin takes that sighting over and the other two are
// new sightings, so the first migration adds origins minus one. A merged record counts one per row
// behind it, and a row names one origin, so that is the count a draft written fresh over the same
// bundle holds, and a rerun changes nothing.
func TestImproveResidual1055MultiOriginMigrationCountsWhatAFreshDraftCounts(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	single := improveProposeTestBundle(t, w, []improveRecord{improveProposeTestSplit("project-a", "size overrun", "rel-a")})
	first := improveReview988Propose(t, w, single)
	path := improveReview988DraftPath(w, first.Created[0].Fingerprint)
	// The earlier build wrote no item and named the sighting by the record's where.
	improveReview988EditRaw(t, path, func(doc map[string]any) {
		delete(doc, "improve")
		doc["seen"].([]any)[0].(map[string]any)["head"] = "rel-a"
	})

	multi := improveProposeTestBundle(t, w, []improveRecord{improveResidual1055Multi()})
	if again := improveReview988Propose(t, w, multi); len(again.Updated) != 1 {
		t.Fatalf("updated = %+v, want the legacy draft to migrate once", again.Updated)
	}
	if got := improveReview988Counts(t, path); len(got) != 1 || got[0] != "project-a=3" {
		t.Errorf("the migrated counts = %v, want project-a=3 (one per origin)", got)
	}
	seen := improveReview988Raw(t, path)["seen"].([]any)
	if len(seen) != 3 || seen[0].(map[string]any)["head"] != "events:rel-a" {
		t.Errorf("the migrated sightings = %v, want three, the first taken over under events:rel-a", seen)
	}

	fresh := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, fresh, map[string]any{})
	created := improveReview988Propose(t, fresh, improveProposeTestBundle(t, fresh, []improveRecord{improveResidual1055Multi()}))
	freshCounts := improveReview988Counts(t, improveReview988DraftPath(fresh, created.Created[0].Fingerprint))
	if got := improveReview988Counts(t, path); strings.Join(got, ",") != strings.Join(freshCounts, ",") {
		t.Errorf("the migrated counts %v differ from a fresh draft's %v", got, freshCounts)
	}

	before := improveReview988Read(t, path)
	if third := improveReview988Propose(t, w, multi); len(third.Updated) != 0 {
		t.Errorf("a rerun after the migration updated the draft: %+v", third.Updated)
	}
	if after := improveReview988Read(t, path); after != before {
		t.Errorf("a rerun after the migration rewrote the draft file")
	}
}

// TestImproveResidual1055MigrationDropsTheSurplusOfAMergedCount covers item 2: the count of a
// draft without the improve item is its stored sightings, never a number read back from its body,
// so a merged record whose count ran past its sightings loses the surplus at the first migration.
// CRW-988 refused any body parser, so this loss is the accepted hand-over cost.
func TestImproveResidual1055MigrationDropsTheSurplusOfAMergedCount(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	record := improveProposeTestSplit("project-a", "size overrun", "rel-a")
	record.Count = 5
	bundle := improveProposeTestBundle(t, w, []improveRecord{record})
	first := improveReview988Propose(t, w, bundle)
	path := improveReview988DraftPath(w, first.Created[0].Fingerprint)
	improveReview988EditRaw(t, path, func(doc map[string]any) {
		delete(doc, "improve")
		doc["seen"].([]any)[0].(map[string]any)["head"] = "rel-a"
		doc["body"] = strings.Replace(doc["body"].(string), "- project-a (1)", "- project-a (5)", 1)
	})

	if again := improveReview988Propose(t, w, bundle); len(again.Updated) != 1 {
		t.Fatalf("updated = %+v, want the legacy draft to migrate once", again.Updated)
	}
	if got := improveReview988Counts(t, path); len(got) != 1 || got[0] != "project-a=1" {
		t.Errorf("the migrated counts = %v, want project-a=1: the count is the stored sightings, not the body's five", got)
	}
}

// TestImproveResidual1055PostedDraftKeepsItsCountsWhileItsSeenGrows covers item 3: a posted draft
// is the record of an issue already opened, so a sighting that arrives after the posting grows the
// seen list and nothing else. The counts, the project, the title and the body stay as the issue was
// opened with them; the seen list is where a later sighting is read.
func TestImproveResidual1055PostedDraftKeepsItsCountsWhileItsSeenGrows(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	first := improveReview988Propose(t, w, improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
	}))
	fingerprint := first.Created[0].Fingerprint
	path := improveReview988DraftPath(w, fingerprint)
	var stdout, stderr strings.Builder
	if code := auditDraftRunMark(improveProposeTestEnv(w, &stdout, &stderr), []string{"--fingerprint", fingerprint, "--posted", "CRW-1"}); code != 0 {
		t.Fatalf("mark: exit %d, stderr %s", code, stderr.String())
	}
	postedCounts := improveReview988Counts(t, path)
	postedBody := improveReview988Body(t, path)

	later := improveReview988Propose(t, w, improveProposeTestBundle(t, w, []improveRecord{
		improveProposeTestSplit("project-a", "size overrun", "rel-a"),
		improveProposeTestSplit("project-b", "size overrun", "rel-b"),
	}))
	if len(later.Updated) != 1 {
		t.Fatalf("updated = %+v, want the posted draft to grow its seen list", later.Updated)
	}
	doc := improveProposeTestDraft(t, w, fingerprint)
	if len(doc.Seen) != 2 {
		t.Errorf("the posted draft holds %d sightings, want the two it has seen", len(doc.Seen))
	}
	if got := improveReview988Counts(t, path); strings.Join(got, ",") != strings.Join(postedCounts, ",") {
		t.Errorf("the posted counts changed from %v to %v", postedCounts, got)
	}
	if body := improveReview988Body(t, path); body != postedBody {
		t.Errorf("the posted body changed:\n%s", body)
	}
	if doc.State != auditDraftStatePosted || doc.Posted != "CRW-1" || doc.Project != "project-a" {
		t.Errorf("the posted draft changed state, key or project: %+v", doc)
	}
}

// improveResidual1055ThreeFrictions seeds three distinct blockages, so a cap of two leaves one.
func improveResidual1055ThreeFrictions(t *testing.T, db *sql.DB) {
	t.Helper()
	improveReview789SeedSplit(t, db, "rel-a", "CRW-1", "project-a", "size overrun", "ev-a")
	improveReview789SeedSplit(t, db, "rel-b", "CRW-2", "project-b", "name collision", "ev-b")
	improveReview789SeedSplit(t, db, "rel-c", "CRW-3", "project-c", "test gap", "ev-c")
}

// TestImproveResidual1055CutCandidateIsDraftedByProposingTheRoadmapBundle covers item 4: a run of
// a boundary and ref that already has a roadmap drafts nothing new (CRW-920 C5), so the candidate
// a cap cut stays out of the drafts on that ref. It is not lost: the roadmap lists it with the
// bundle that holds its evidence, and proposing that bundle with the configured cap drafts it. The
// roadmap names that command when a candidate was left.
func TestImproveResidual1055CutCandidateIsDraftedByProposingTheRoadmapBundle(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, improveResidual1055ThreeFrictions)
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{"max_new_drafts": 2})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("the first run: exit %d, stderr %s", code, errOut)
	}
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("the repeated run: exit %d, stderr %s", code, errOut)
	}
	if drafts := improveRoadmapTestDrafts(t, manageState); len(drafts) != 2 {
		t.Fatalf("the repeated run left %v, want the two drafts of the first run", drafts)
	}
	body := improveReview789LatestRoadmap(t, manageState)
	bundles := improveRoadmapTestBundles(t, manageState, "M2")
	bundle := filepath.Join(manageState, "improve", "M2", bundles[len(bundles)-1])
	if want := "- draft the left candidates: crw manage improve propose --bundle " + bundle + "\n"; !strings.Contains(body, want) {
		t.Errorf("the roadmap does not name the way to draft what was left; want the line %q in:\n%s", want, body)
	}
	if !strings.Contains(body, "### 3. ") {
		t.Errorf("the roadmap does not list the cut candidate:\n%s", body)
	}

	var out, errBuf strings.Builder
	if code := improveRunPropose(t.Context(), improveRoadmapTestEnv(s, &out, &errBuf), []string{"--bundle", bundle}); code != 0 {
		t.Fatalf("propose over the roadmap bundle: exit %d, stderr %s", code, errBuf.String())
	}
	if report := improveProposeTestReport(t, out.String()); len(report.Created) != 1 || report.Remaining != 0 {
		t.Errorf("created = %+v remaining = %d, want the one cut candidate drafted", report.Created, report.Remaining)
	}
	if drafts := improveRoadmapTestDrafts(t, manageState); len(drafts) != 3 {
		t.Errorf("the drafts directory holds %v, want three", drafts)
	}
}

// TestImproveResidual1055RoadmapCommandSurvivesTheShell follows the roadmap's command the way a
// person does: the line is pasted into a shell. A ref and a state directory may hold a space or a
// shell metacharacter, so the bundle path in the line must read back as one word, the one argument
// of --bundle, and nothing in it may be run.
func TestImproveResidual1055RoadmapCommandSurvivesTheShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no POSIX shell")
	}
	for _, c := range []struct{ name, state, ref string }{
		{"space in ref", "manage-state", "M 2"},
		{"space in state", "manage state", "M2"},
		{"metacharacters", "manage $(touch pwned) state", "M'2;`touch pwned`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := improveTestSetup(t)
			manageState := filepath.Join(s.root, c.state)
			improveTestStore(t, s, improveResidual1055ThreeFrictions)
			improveRoadmapTestConfigure(t, s, manageState, map[string]any{"max_new_drafts": 2})
			var stdout, stderr strings.Builder
			e := improveRoadmapTestEnv(s, &stdout, &stderr)
			if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", c.ref); code != 0 {
				t.Fatalf("run: exit %d, stderr %s", code, errOut)
			}
			body := improveReview789LatestRoadmap(t, manageState)
			const prefix = "- draft the left candidates: crw manage improve propose "
			var rest string
			for _, line := range strings.Split(body, "\n") {
				if after, ok := strings.CutPrefix(line, prefix); ok {
					rest = after
				}
			}
			if rest == "" {
				t.Fatalf("the roadmap names no command to draft the left candidates:\n%s", body)
			}
			bundles := improveRoadmapTestBundles(t, manageState, c.ref)
			bundle := filepath.Join(manageState, "improve", c.ref, bundles[len(bundles)-1])
			workDir := t.TempDir()
			cmd := exec.Command(sh, "-c", "printf '%s\\n' "+rest)
			cmd.Dir = workDir
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("the shell rejected the roadmap command %q: %v", rest, err)
			}
			if got, want := string(out), "--bundle\n"+bundle+"\n"; got != want {
				t.Errorf("the shell reads the roadmap command %q as %q, want %q", rest, got, want)
			}
			if entries, _ := os.ReadDir(workDir); len(entries) != 0 {
				t.Errorf("reading the roadmap command ran something: %v", entries)
			}
		})
	}
}

// TestImproveResidual1055RoadmapNamesNoCommandWhenNothingWasLeft keeps the line out of a roadmap
// that left no candidate.
func TestImproveResidual1055RoadmapNamesNoCommandWhenNothingWasLeft(t *testing.T) {
	s := improveTestSetup(t)
	manageState := filepath.Join(s.root, "manage-state")
	improveTestStore(t, s, improveResidual1055ThreeFrictions)
	improveRoadmapTestConfigure(t, s, manageState, map[string]any{})
	var stdout, stderr strings.Builder
	e := improveRoadmapTestEnv(s, &stdout, &stderr)
	if code, _, errOut := improveRoadmapTestRun(t, e, "--boundary", "milestone", "--ref", "M2"); code != 0 {
		t.Fatalf("run: exit %d, stderr %s", code, errOut)
	}
	if body := improveReview789LatestRoadmap(t, manageState); strings.Contains(body, "draft the left candidates") {
		t.Errorf("the roadmap names a command although nothing was left:\n%s", body)
	}
}

// TestImproveResidual1055ComparisonsKeepInvalidValuesApart covers item 6: the three comparison
// helpers compare element by element and never join values with a separator, so a value that is
// not valid UTF-8 (a lone surrogate encoded as bytes, a stray continuation byte) is equal to
// itself, different from every other value, and not folded onto the replacement character.
func TestImproveResidual1055ComparisonsKeepInvalidValuesApart(t *testing.T) {
	values := []string{"", "a", "\xff", "\xfe", "\xed\xa0\x80", "\xed\xa0\x81", "\xc0\xaf", "a\xffb", "a\xfeb", "�", "\x00", "a\x00b", "a,b", "a|b"}
	for i, a := range values {
		for j, b := range values {
			want := i == j
			if got := improveProposeStringsEqual([]string{a}, []string{b}); got != want {
				t.Errorf("improveProposeStringsEqual(%q, %q) = %v, want %v", a, b, got, want)
			}
			seenA := []auditDraftSeen{{Mode: "improve", Subject: a, Head: a, At: a}}
			seenB := []auditDraftSeen{{Mode: "improve", Subject: b, Head: b, At: b}}
			if got := improveProposeSeenEqual(seenA, seenB); got != want {
				t.Errorf("improveProposeSeenEqual(%q, %q) = %v, want %v", a, b, got, want)
			}
			// One field apart is enough to differ.
			if improveProposeSeenEqual([]auditDraftSeen{{Subject: a, Head: b}}, []auditDraftSeen{{Subject: b, Head: a}}) != want {
				t.Errorf("improveProposeSeenEqual swaps the subject and head of %q and %q", a, b)
			}
			projA := []improveProposeProject{{Project: a, Count: 1}}
			projB := []improveProposeProject{{Project: b, Count: 1}}
			if got := improveProposeProjectsEqual(projA, projB); got != want {
				t.Errorf("improveProposeProjectsEqual(%q, %q) = %v, want %v", a, b, got, want)
			}
		}
	}
	// A list is not folded into one joined key: two elements are not one element that holds the
	// separator-like text, and the order of strings and sightings matters.
	if improveProposeStringsEqual([]string{"a", "b"}, []string{"a,b"}) || improveProposeStringsEqual([]string{"a", "b"}, []string{"b", "a"}) {
		t.Error("improveProposeStringsEqual folded a list or ignored its order")
	}
	// The project lists compare counts whatever the order, sum a name given twice, and ignore a
	// project that holds no count.
	one := []improveProposeProject{{Project: "\xff", Count: 1}, {Project: "\xed\xa0\x80", Count: 2}}
	two := []improveProposeProject{{Project: "\xed\xa0\x80", Count: 1}, {Project: "\xff", Count: 1}, {Project: "\xed\xa0\x80", Count: 1}, {Project: "gone", Count: 0}}
	if !improveProposeProjectsEqual(one, two) {
		t.Error("improveProposeProjectsEqual rejected equal counts in another order")
	}
	if improveProposeProjectsEqual(one, []improveProposeProject{{Project: "\xff", Count: 2}, {Project: "\xed\xa0\x80", Count: 1}}) {
		t.Error("improveProposeProjectsEqual accepted swapped counts")
	}
}

// TestImproveResidual1055BundleWithInvalidValuesProposesOnceAndStaysStable covers item 6 through the
// command: a bundle that carries a lone surrogate escape and a raw invalid byte decodes to the
// replacement character, the draft is written with it, and a rerun neither updates nor rewrites it.
func TestImproveResidual1055BundleWithInvalidValuesProposesOnceAndStaysStable(t *testing.T) {
	w := improveProposeTestSetup(t)
	improveProposeTestConfigure(t, w, map[string]any{})
	raw := "{\"schema\":\"" + improveBundleSchema + "\",\"sources\":[],\"records\":[" +
		"{\"kind\":\"fault\",\"key\":\"scope\",\"where\":\"scope\",\"what\":\"bad \\ud800 value\",\"count\":1,\"first_at\":\"t\",\"last_at\":\"t\",\"evidence\":[\"fault:a\\ud800\",\"fault:b\xff\"]}," +
		"{\"kind\":\"fault\",\"key\":\"scope\",\"where\":\"scope\",\"what\":\"bad \\ud800 value\",\"count\":1,\"first_at\":\"t\",\"last_at\":\"t\",\"evidence\":[\"fault:a\xff\",\"fault:b\\udc00\"]}" +
		"]}\n"
	bundle := filepath.Join(w.root, "invalid.json")
	if err := os.WriteFile(bundle, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	first := improveReview988Propose(t, w, bundle)
	if len(first.Created) != 1 {
		t.Fatalf("created = %+v, want one draft", first.Created)
	}
	path := improveReview988DraftPath(w, first.Created[0].Fingerprint)
	doc := improveProposeTestDraft(t, w, first.Created[0].Fingerprint)
	// Both records name the same two origins once the decoder has read each invalid value as U+FFFD.
	if len(doc.Seen) != 2 {
		t.Errorf("the draft holds %d sightings, want the two origins the decoder reads: %+v", len(doc.Seen), doc.Seen)
	}
	before := improveReview988Read(t, path)
	for run := 0; run < 2; run++ {
		if again := improveReview988Propose(t, w, bundle); len(again.Created) != 0 || len(again.Updated) != 0 {
			t.Errorf("rerun %d created %d and updated %d, want neither", run+1, len(again.Created), len(again.Updated))
		}
	}
	if after := improveReview988Read(t, path); after != before {
		t.Errorf("a rerun rewrote the draft file")
	}
}
