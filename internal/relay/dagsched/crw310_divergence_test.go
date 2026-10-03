package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-310: four places where two copies of one rule behaved differently. Every test says whether it is red on the baseline (it pins the change) or a guard that is green on the
// baseline (it pins what must not change).

// baselineScopeOf is scopeOf as it was before CRW-310, kept here as the oracle for "only inputs that were wrong change".
func baselineScopeOf(path string, roots []string) string {
	best := ""
	for _, root := range roots {
		clean := strings.TrimRight(root, "/")
		if (path == clean || strings.HasPrefix(path, clean+"/")) && len(clean) > len(best) {
			best = root
		}
	}
	return best
}

// c1 (red for the root "/"): the same question store.IsWithin answers is answered the same way, a root of "/" holds every absolute path.
func TestScopeOfFollowsTheStoresContainmentRule(t *testing.T) {
	rows := []struct {
		name  string
		roots []string
		path  string
		want  string
	}{
		{"a root of / holds a path", []string{"/"}, "/x/f", "/"},
		{"a root of / holds the root itself", []string{"/"}, "/", "/"},
		{"a root of / is the answer when no other root holds the path", []string{"/a", "/"}, "/b", "/"},
		{"a nested root beats the root of /", []string{"/", "/a"}, "/a/x", "/a"},
		{"a root holds itself", []string{"/a"}, "/a", "/a"},
		{"a sibling that shares a prefix is not inside", []string{"/a/b"}, "/a/bc", ""},
		{"a relative path is inside nothing", []string{"/a"}, "a/b", ""},
		{"the longest root wins, given as spelled", []string{"/a/", "/a/b"}, "/a/b/f", "/a/b"},
		{"no root holds the path", []string{"/a"}, "/b", ""},
		{"a root with dot segments is read as the store reads it", []string{"/r/./a"}, "/r/a/f", "/r/./a"},
		{"a path that leaves its root by .. is outside it", []string{"/r"}, "/r/../o/f", ""},
	}
	for _, row := range rows {
		got := scopeOf(row.path, row.roots)
		if got != row.want {
			t.Errorf("%s: scopeOf(%q, %q) = %q, want %q", row.name, row.path, row.roots, got, row.want)
		}
		// the parity the issue asks for: an answer exists exactly when the store's rule holds for some root, and the answer is such a root
		held := false
		for _, root := range row.roots {
			held = held || store.IsWithin(root, row.path)
		}
		if (got != "") != held || (got != "" && !store.IsWithin(got, row.path)) {
			t.Errorf("%s: scopeOf says %q and store.IsWithin says %v", row.name, got, held)
		}
	}
}

// c1 (guard, green on the baseline): every answer the baseline gave for a normalised path is kept, in every order of up to three roots, so no manifest that could be built before changes its
// scope, its digest, or the route of a criteria-only revalidation. The row set includes the spellings that make the baseline's comparison odd (repeated trailing slashes, a root of "/", a root the
// raw prefix test does not read).
func TestScopeOfKeepsEveryAnswerTheBaselineGaveForANormalisedPath(t *testing.T) {
	roots := []string{"/", "/r", "/r/", "/r//", "/r/a", "/r/a/", "/r/b", "/rr", "/a", "///", "/r//a"}
	paths := []string{"/", "/r", "/r/a", "/r/a/f", "/r/b/f", "/rr/f", "/a/x", "/x"}
	var rows int
	check := func(set []string) {
		for _, path := range paths {
			if _, err := store.NormalizeDeclaredPath(path); err != nil {
				t.Fatalf("the row path %q is not normalised: %v", path, err)
			}
			want := baselineScopeOf(path, set)
			if want == "" {
				continue
			}
			rows++
			if got := scopeOf(path, set); got != want {
				t.Errorf("scopeOf(%q, %q) = %q, the baseline answered %q", path, set, got, want)
			}
		}
	}
	for _, a := range roots {
		check([]string{a})
		for _, b := range roots {
			check([]string{a, b})
			for _, c := range roots {
				check([]string{a, b, c})
			}
		}
	}
	if rows < 1000 {
		t.Fatalf("only %d rows were compared", rows)
	}
}

// c1: the inputs whose answer changes, one by one, each with its reason. The baseline answer is asserted too, so the table says what changed.
func TestScopeOfChangesOnlyForInputsTheStoreReadsDifferently(t *testing.T) {
	rows := []struct {
		name         string
		roots        []string
		path         string
		before, want string
	}{
		{"a root of / (the raw test trims it to nothing and never compares it)", []string{"/"}, "/x", "", "/"},
		{"a root with a dot segment the raw prefix does not read", []string{"/r/./a"}, "/r/a/f", "", "/r/./a"},
		{"a path that leaves its root by .. (the raw prefix test is satisfied, the store's is not)", []string{"/r"}, "/r/../o/f", "/r", ""},
		{"a ..-path under roots of which a dot-segment root made the baseline answer later", []string{"/r/a", "/r///////", "/r/a/../b"}, "/r/a/../b/f", "/r/a/../b", "/r///////"},
	}
	for _, row := range rows {
		if got := baselineScopeOf(row.path, row.roots); got != row.before {
			t.Errorf("%s: the baseline answered %q, the table says %q", row.name, got, row.before)
		}
		if got := scopeOf(row.path, row.roots); got != row.want {
			t.Errorf("%s: scopeOf(%q, %q) = %q, want %q", row.name, row.path, row.roots, got, row.want)
		}
	}
}

// c1 (red): a predecessor whose relationship holds a root of "/" can be consumed: the manifest builds and names "/" as the scope of the artifact. On the baseline the artifact "lies under none
// of the predecessor's artifact roots" (B-05).
func TestAManifestOverARootOfSlashBuilds(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	a := f.acceptNode("p1", "research", acceptOpts{})
	f.exec("UPDATE relationships SET artifact_roots = ? WHERE relationship_id = ?", "[\"/\"]", a.Acceptance.RelationshipID)
	body, blocked := crw310Build(t, f, "p1", "design")
	if len(blocked) != 0 {
		t.Fatalf("blocked: %+v", blocked)
	}
	if scope := crw310FirstScope(t, body); scope != "/" {
		t.Fatalf("scope = %q, want /", scope)
	}
}

// c1 (guard): the scope a manifest records is still the baseline's for roots whose comparison is odd. The enclosing root is spelled with so many trailing slashes that the baseline's
// raw-length comparison keeps it over the root that is exactly the artifact: that answer is in the digest of the manifests already stored, and it must not move.
func TestTheScopeOfAStoredManifestDoesNotMove(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	a := f.acceptNode("p1", "research", acceptOpts{})
	file := a.Files[0]
	enclosing := a.Root + strings.Repeat("/", len(filepath.Base(file))+3)
	roots, err := json.Marshal([]string{enclosing, file})
	if err != nil {
		t.Fatal(err)
	}
	if got := baselineScopeOf(file, []string{enclosing, file}); got != enclosing {
		t.Fatalf("the fixture is wrong: the baseline answers %q, not the enclosing root", got)
	}
	f.exec("UPDATE relationships SET artifact_roots = ? WHERE relationship_id = ?", string(roots), a.Acceptance.RelationshipID)
	body, blocked := crw310Build(t, f, "p1", "design")
	if len(blocked) != 0 {
		t.Fatalf("blocked: %+v", blocked)
	}
	if scope := crw310FirstScope(t, body); scope != enclosing {
		t.Fatalf("scope = %q, want the enclosing root %q the baseline recorded", scope, enclosing)
	}
}

// c1 (red): a volatile snapshot that leaves the artifact root by .. is refused as out of scope by the scope check itself. On the baseline the raw prefix passes it, and only the later hash (which the
// store half of a release skips) would refuse it.
func TestAVolatileSnapshotThatLeavesTheRootByDotDotIsOutOfScope(t *testing.T) {
	f := newFixture(t)
	finding := f.sched.verifyVolatile(context.Background(), []Volatile{{Source: "linear:comment", SnapshotURI: "/r/../o/f", SHA256: dig("x"), CapturedAt: "2026-10-02T00:00:00Z"}},
		VerifyOptions{SkipFileBytes: true, ArtifactRoots: []string{"/r"}})
	if finding == nil || finding.Code != "B-05" || finding.Reason != BlockedInputOutOfScope {
		t.Fatalf("finding = %+v, want B-05 %s", finding, BlockedInputOutOfScope)
	}
}

// crw310Build builds the manifest of a node from the store as it is, reading no file.
func crw310Build(t *testing.T, f *fixture, plan, node string) (map[string]any, []BlockedFinding) {
	t.Helper()
	ctx := context.Background()
	snap := f.snapshot(plan)
	n, ok := nodeOf(snap, node)
	if !ok {
		t.Fatalf("no node %s", node)
	}
	rule := RuleVersion{SkillsDigest: dig("skills"), Model: "gpt-5", Effort: "medium", PromptTemplate: "template-1", RelayBuild: "test-build"}
	body, blocked, err := f.sched.BuildManifest(ctx, f.s.Q(ctx), plan, snap, n, ManifestInput{RuleVersion: rule, CreatedByTaskID: "parent", CreatedAt: f.clock()}, VerifyOptions{SkipFileBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	return body, blocked
}

func crw310FirstScope(t *testing.T, body map[string]any) string {
	t.Helper()
	inputs, _ := body["inputs"].([]any)
	if len(inputs) == 0 {
		t.Fatalf("the manifest has no input: %v", body)
	}
	input, _ := inputs[0].(map[string]any)
	artifacts, _ := input["artifacts"].([]any)
	if len(artifacts) == 0 {
		t.Fatalf("the input names no artifact: %v", input)
	}
	artifact, _ := artifacts[0].(map[string]any)
	return textOf(artifact["scope"])
}

// implementation node I, released and reporting: the state a correction is prepared from.
func crw310CorrectionKit(t *testing.T) (*releaseKit, string) {
	t.Helper()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.mustRelease("rp", "I")
	var rid string
	if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_node_executions WHERE node_id = 'I'").Scan(&rid); err != nil {
		t.Fatal(err)
	}
	k.seedReport(rid, "I", "rp")
	return k, rid
}

var crw310Notes int

// crw310Prepare prepares a correction of I with the given base and a volatile snapshot (so the manifest differs from the first release's: a manifest is content addressed).
func crw310Prepare(k *releaseKit, base *BaseRef) (Prepared, error) {
	k.t.Helper()
	crw310Notes++
	notes := "the notes of the correction"
	snapshot := writeFile(k.t, k.root, "crw310-notes.md", notes)
	return k.sched.PrepareCorrection(context.Background(), "rp", "I", "parent", ManifestInput{RuleVersion: k.request(true).RuleVersion, Base: base,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: snapshot, SHA256: shaOf([]byte(notes)), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
}

func crw310StoredBase(t *testing.T, k *releaseKit, digest string) map[string]any {
	t.Helper()
	body, found, err := k.repo.ReadManifest(context.Background(), digest)
	if err != nil || !found {
		t.Fatalf("the manifest %s is not stored: %v %v", digest, found, err)
	}
	base, _ := body["base"].(map[string]any)
	return base
}

// c2 (red): a correction names the repository and the ref and the relay reads the commit from the target, as a release does. On the baseline the sha stays empty and the manifest is refused.
func TestACorrectionReadsItsBaseFromTheTargetTip(t *testing.T) {
	k, _ := crw310CorrectionKit(t)
	prepared, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev"})
	if err != nil {
		t.Fatalf("prepare = %v", err)
	}
	if base := crw310StoredBase(t, k, prepared.ManifestDigest); base["sha"] != head1 || base["repository"] != "owner/repo" || base["ref"] != "dev" {
		t.Fatalf("stored base = %v, want the tip %s", base, head1)
	}
}

// c2 (red): a base the caller states that is not what the target reads is refused with the existing reason that means exactly that, and nothing is stored. On the baseline it was stored as given.
func TestACorrectionRefusesABaseThatIsNotTheTip(t *testing.T) {
	k, _ := crw310CorrectionKit(t)
	manifests, copies := k.count("SELECT COUNT(*) FROM dag_input_manifests"), len(k.copies())
	other := strings.Repeat("2", 40)
	_, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev", SHA: other})
	if refusalReason(err) != "merge_base_mismatch" {
		t.Fatalf("prepare = %v, want merge_base_mismatch", err)
	}
	if !strings.Contains(err.Error(), other) || !strings.Contains(err.Error(), head1) {
		t.Fatalf("the refusal does not name both commits in full: %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_input_manifests") != manifests || len(k.copies()) != copies {
		t.Fatal("a refused correction left a manifest or a copy behind")
	}
}

// c2: the sha the caller states may be the tip (a guard, green on the baseline) and then the manifest is the one built without it (red on the baseline, where the omission is refused); the stored
// commit is the relay's reading, not the caller's spelling.
func TestACorrectionWithTheTipStatedIsTheCorrectionWithoutIt(t *testing.T) {
	k, _ := crw310CorrectionKit(t)
	tip := "abcdef0123456789abcdef0123456789abcdef01"
	k.tips.sha = tip
	stated, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev", SHA: tip})
	if err != nil {
		t.Fatalf("prepare with the tip stated = %v", err)
	}
	omitted, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev"})
	if err != nil {
		t.Fatalf("prepare without the sha = %v", err)
	}
	if stated.ManifestDigest != omitted.ManifestDigest {
		t.Fatalf("the manifest digests differ: %s and %s", stated.ManifestDigest, omitted.ManifestDigest)
	}
	shouted, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev", SHA: strings.ToUpper(tip)})
	if err != nil {
		t.Fatalf("prepare with the tip in capitals = %v", err)
	}
	if base := crw310StoredBase(t, k, shouted.ManifestDigest); base["sha"] != tip {
		t.Fatalf("stored sha = %v, want the relay's reading %s", base["sha"], tip)
	}
}

// c2 (red): a tip that cannot be read, or a scheduler that cannot read one, never stores a manifest. On the baseline neither is consulted.
func TestACorrectionNeedsTheTipItRecords(t *testing.T) {
	t.Run("the tip is unreadable", func(t *testing.T) {
		k, _ := crw310CorrectionKit(t)
		manifests := k.count("SELECT COUNT(*) FROM dag_input_manifests")
		k.tips.err = errors.New("the forge did not answer")
		if _, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev", SHA: head1}); err == nil || !strings.Contains(err.Error(), "the forge did not answer") {
			t.Fatalf("prepare = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_input_manifests") != manifests {
			t.Fatal("a manifest was stored")
		}
	})
	t.Run("the scheduler has no target reader", func(t *testing.T) {
		k, _ := crw310CorrectionKit(t)
		manifests := k.count("SELECT COUNT(*) FROM dag_input_manifests")
		k.sched.Tips = nil
		if _, err := crw310Prepare(k, &BaseRef{Repository: "owner/repo", Ref: "dev", SHA: head1}); err == nil || !strings.Contains(err.Error(), "no target reader") {
			t.Fatalf("prepare = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_input_manifests") != manifests {
			t.Fatal("a manifest was stored")
		}
	})
}

// c2 (guard, green on the baseline): a base that names no repository or no ref never reaches the target reader: the manifest check refuses it as before.
func TestACorrectionWithAnIncompleteBaseIsRefusedWithoutReadingTheTarget(t *testing.T) {
	k, _ := crw310CorrectionKit(t)
	k.tips.err = errors.New("the tip must not be read for an incomplete base")
	for _, base := range []*BaseRef{{Ref: "dev", SHA: head1}, {Repository: "owner/repo", SHA: head1}, {}} {
		_, err := crw310Prepare(k, base)
		if err == nil || strings.Contains(err.Error(), "must not be read") || !strings.Contains(err.Error(), "B-01") {
			t.Fatalf("prepare with base %+v = %v, want the manifest check's B-01 refusal", *base, err)
		}
	}
}

// c3 (red): the same situation, an actor that is not the parent of the relationship, is one refusal reason in both steps of a correction, with one text.
func TestBothStepsOfACorrectionRefuseANonParentUnderOneReason(t *testing.T) {
	k := newReleaseKit(t)
	rid := k.correctionKit()
	_, prepareErr := k.sched.PrepareCorrection(context.Background(), "rp", "A", "intruder", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}})
	k.openCorrection(rid, nil)
	_, recordErr := k.sched.RecordCorrection(context.Background(), "rp", "A", "intruder", "")
	if refusalReason(prepareErr) != "scope_role_mismatch" || refusalReason(recordErr) != "scope_role_mismatch" {
		t.Fatalf("prepare = %v, record = %v: both want scope_role_mismatch", prepareErr, recordErr)
	}
	var a, b *store.RefusedError
	if !errors.As(prepareErr, &a) || !errors.As(recordErr, &b) || a.Detail != b.Detail || !strings.Contains(a.Detail, "intruder") {
		t.Fatalf("the two refusals do not read alike: %q and %q", prepareErr, recordErr)
	}
	// guard: a relationship that is not active is refused first, in both steps, as before (the order of the checks is the same in both)
	k.exec("UPDATE relationships SET status = 'paused'")
	if _, err := k.sched.PrepareCorrection(context.Background(), "rp", "A", "intruder", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}}); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("prepare on a paused relationship = %v", err)
	}
	if _, err := k.sched.RecordCorrection(context.Background(), "rp", "A", "intruder", ""); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("record on a paused relationship = %v", err)
	}
}

// crw310GH writes a stand-in for gh that runs the given shell body.
func crw310GH(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// c4 (red): a compare call that does not answer is cut at the bound and says so. The stand-in execs sleep, so the kill reaches the process that holds the pipe.
func TestTheAncestryCompareCallHasADeadline(t *testing.T) {
	gh := crw310GH(t, "exec sleep 3")
	started := time.Now()
	_, _, err := GitAncestry{GH: gh, Timeout: 150 * time.Millisecond}.Ancestry(context.Background(), "owner/repo", head1, strings.Repeat("2", 40))
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("the call took %v, the bound is 150ms", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "exceeded the timeout of 150ms") {
		t.Fatalf("err = %v, want the timeout named", err)
	}
}

// c4 (guard once the seam is there): the bound is the one the other forge read has, by construction, and it is the 30 seconds that read always had.
func TestTheAncestryBoundIsTheBranchReadBound(t *testing.T) {
	got := (GitAncestry{}).forgeTimeout()
	if got != mergeturn.ForgeCallTimeout {
		t.Fatalf("forgeTimeout = %v, mergeturn.ForgeCallTimeout = %v", got, mergeturn.ForgeCallTimeout)
	}
	if want := 30 * time.Second; got != want {
		t.Fatalf("the bound is %v, the branch read has always used %v", got, want)
	}
	// a stand-in that answers in time still gets its answer through the bound
	gh := crw310GH(t, "echo '{\"status\":\"ahead\",\"behind_by\":0}'")
	if in, method, err := (GitAncestry{GH: gh}).Ancestry(context.Background(), "owner/repo", head1, strings.Repeat("2", 40)); err != nil || !in || method != "gh api compare behind_by" {
		t.Fatalf("an answer inside the bound = %v %q %v", in, method, err)
	}
}

// crw310ReadRow reads one stored manifest body from a read-only copy of the database the binary used.
func crw310ReadRow(t *testing.T, state, digest string) map[string]any {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow("SELECT body_json FROM dag_input_manifests WHERE manifest_digest = ?", digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// c2 through the built binary (red on the baseline, where the command never reads a tip): dag-correct --prepare reads the branch tip of a local repository, refuses a base that is not it with exit 2 and
// merge_base_mismatch, and answers a branch it cannot read as the host failure the same read is in dag-release (exit 3, "error":"host", no reason). The wiring of the target reader in the command is
// observed here: a library test that injects its own reader cannot see it.
func TestCLICorrectPrepareReadsTheTipOfTheBase(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	k := newReleaseKitAt(t, state)
	releasePlan(k.fixture, "rp")
	k.mustRelease("rp", "I")
	var rid string
	if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_node_executions WHERE node_id = 'I'").Scan(&rid); err != nil {
		t.Fatal(err)
	}
	k.seedReport(rid, "I", "rp")
	repo := newGitRepo(t)
	tip := repo.git("rev-parse", "dev")
	notes := writeFile(t, k.root, "crw310-cli-notes.md", "the notes of the correction")
	if err := k.s.Close(); err != nil {
		t.Fatal(err)
	}
	rule := k.request(true).RuleVersion
	request := func(base map[string]any) string {
		raw, err := json.Marshal(map[string]any{"base": base, "artifact_roots": []string{k.root},
			"rule_version": map[string]any{"skills_digest": rule.SkillsDigest, "model": rule.Model, "effort": rule.Effort, "prompt_template": rule.PromptTemplate, "relay_build": rule.RelayBuild},
			"volatile":     []map[string]any{{"source": "linear:comment", "snapshot_uri": notes, "sha256": shaOf([]byte("the notes of the correction")), "captured_at": "2026-10-02T00:00:00Z"}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	prepare := func(base map[string]any) (map[string]any, int) {
		out, code := crw(t, state, "dag-correct", "--plan", "rp", "--node", "I", "--actor", "parent", "--prepare", "--manifest-request", request(base))
		return parseOut(t, out), code
	}
	// the sha omitted: the command reads the tip and records it
	m, code := prepare(map[string]any{"repository": repo.path, "ref": "dev"})
	if code != 0 || m["schema"] != "dag-correct/1" || m["ok"] != true {
		t.Fatalf("prepare with no sha: exit %d %v", code, m)
	}
	digest, _ := m["manifest_digest"].(string)
	if base, _ := crw310ReadRow(t, state, digest)["base"].(map[string]any); base["sha"] != tip || base["repository"] != repo.path || base["ref"] != "dev" {
		t.Fatalf("the stored base = %v, want the tip %s of %s", base, tip, repo.path)
	}
	// the sha stated and wrong: the existing refusal, exit 2
	other := strings.Repeat("a", 40)
	m, code = prepare(map[string]any{"repository": repo.path, "ref": "dev", "sha": other})
	if code != 2 || m["reason"] != "merge_base_mismatch" {
		t.Fatalf("prepare with a wrong sha: exit %d %v", code, m)
	}
	// a branch that is not there: a plain error from the reader, the host failure of the command family (exit 3, no refusal reason)
	m, code = prepare(map[string]any{"repository": repo.path, "ref": "nowhere"})
	if _, hasReason := m["reason"]; code != 3 || m["error"] != "host" || hasReason {
		t.Fatalf("prepare on a missing branch: exit %d %v", code, m)
	}
}
