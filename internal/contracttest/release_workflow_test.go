package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The release workflow's owner-controlled route, run step by step against a real bare remote
// and the fake gh and git (release.go). Each test starts from the same fixture: main at base,
// dev one commit later at candidate, the latest dev-push CI run successful and no tag or release.
// The corpus's release fixture (contract/fixtures/records/test_release__*) holds that an owner
// dispatch with valid inputs passes; these hold everything the route must refuse or recover.
// (The Python test_release.py cases, ported when the scripts/ci/tests suite left CI.)

func releaseFixture(t *testing.T) *releaseRepo {
	t.Helper()
	r, err := newReleaseRepo(t)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// releaseBlocks are the steps these tests run, by the names release_steps.py's tests gave them.
var releaseBlocks = map[string][2]string{
	"inputs":        {"validate", "release-inputs"},
	"source":        {"validate", "release-source"},
	"credentials":   {"validate", "release-credentials"},
	"verify":        {"verify", "release-verify"},
	"publish":       {"publish", "release-publish"},
	"go-source":     {"release-go", "release-go-source"},
	"go-tree":       {"release-go", "release-go-tree"},
	"snapshot-tree": {"validate", "release-go-snapshot-tree"},
}

// step runs the named block with extra over the fixture's environment.
func (r *releaseRepo) step(t *testing.T, block string, extra map[string]string) releaseOutcome {
	t.Helper()
	where, known := releaseBlocks[block]
	if !known {
		t.Fatalf("no release block %q", block)
	}
	outcome, err := r.run(where[0], where[1], extra)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func (r *releaseRepo) pass(t *testing.T, label, block string, extra map[string]string) releaseOutcome {
	t.Helper()
	outcome := r.step(t, block, extra)
	if outcome.exit != 0 {
		t.Fatalf("%s: exit %d\n%s%s", label, outcome.exit, outcome.stdout, outcome.stderr)
	}
	return outcome
}

func (r *releaseRepo) refuse(t *testing.T, label, block string, extra map[string]string) releaseOutcome {
	t.Helper()
	outcome := r.step(t, block, extra)
	if outcome.exit == 0 {
		t.Fatalf("%s: unexpectedly succeeded\n%s", label, outcome.stdout)
	}
	return outcome
}

func (r *releaseRepo) setCase(t *testing.T, fake string, values map[string]any) {
	t.Helper()
	if err := r.set(fake, values); err != nil {
		t.Fatal(err)
	}
}

func (r *releaseRepo) mustGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := r.git(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// expectRemote requires the remote's ref to resolve to want ("" for absent).
func (r *releaseRepo) expectRemote(t *testing.T, ref, want string) {
	t.Helper()
	got, found := r.remoteRef(ref)
	if !found {
		got = ""
	}
	if got != want {
		t.Fatalf("remote %s = %q, want %q", ref, got, want)
	}
}

func (r *releaseRepo) created(t *testing.T) (string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.state, "created.txt"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), true
}

func (r *releaseRepo) expectNoRelease(t *testing.T) {
	t.Helper()
	if text, found := r.created(t); found {
		t.Fatalf("a release was created: %s", text)
	}
}

// verifyRecord writes a verification-record.json into the fixture and returns the RUNNER_TEMP the
// verify step reads it from. values are merged over a passing record for r.candidate, so a case
// changes only the field it is about.
func (r *releaseRepo) verifyRecord(t *testing.T, values map[string]any) string {
	t.Helper()
	dir := filepath.Join(r.dir, "verify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"schema": "verification-record/1", "runner": "github-release",
		"headCommit": r.candidate, "result": "pass", "pinMismatch": []any{},
		"digest": "sha256:" + strings.Repeat("0", 64),
	}
	for key, value := range values {
		if value == nil {
			delete(record, key)
			continue
		}
		record[key] = value
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verification-record.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// CRW-966: release.yml verifies the SHA it will publish itself. The dev push CI run it used to
// read is gone, and the evidence is the repository's local full verification: a
// verification-record/1 whose result is a pass for the commit being released. A missing record, a
// non-passing result, a pin mismatch, a foreign commit and a wrong schema each refuse publication.
func TestReleaseWorkflow_verify_requires_a_passing_record_for_the_commit(t *testing.T) {
	r := releaseFixture(t)
	r.pass(t, "a passing record", "verify", map[string]string{"RUNNER_TEMP": r.verifyRecord(t, nil)})
	for label, values := range map[string]map[string]any{
		"missing record": nil,
		"failed result":  {"result": "fail"},
		"no result":      {"result": nil},
		"pin mismatch":   {"pinMismatch": []any{"node"}},
		"foreign commit": {"headCommit": strings.Repeat("0", 40)},
		"no head commit": {"headCommit": nil},
		"wrong schema":   {"schema": "verification-record/2"},
		"no schema":      {"schema": nil},
	} {
		t.Run(label, func(t *testing.T) {
			temp := r.verifyRecord(t, values)
			if label == "missing record" {
				if err := os.Remove(filepath.Join(temp, "verification-record.json")); err != nil {
					t.Fatal(err)
				}
			}
			r.refuse(t, label, "verify", map[string]string{"RUNNER_TEMP": temp})
		})
	}
}

// Publication and the binary attachment both stand behind the verification: a failed or absent
// verify job cannot publish, because publish needs it and release-go needs publish.
func TestReleaseWorkflow_publication_needs_the_verification(t *testing.T) {
	r := releaseFixture(t)
	verify, err := releaseJobBody(r.workflow, "verify")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verify, "ci local") {
		t.Fatalf("verify does not run the local full verification:\n%s", verify)
	}
	publish, err := releaseJobBody(r.workflow, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publish, "needs: [validate, verify]") {
		t.Fatalf("publish does not need verify:\n%s", publish)
	}
	job, err := releaseJobBody(r.workflow, "release-go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(job, "needs: [validate, verify, publish]") {
		t.Fatalf("release-go does not need verify:\n%s", job)
	}
	// The dev push CI run the workflow used to read is gone from every job.
	for _, name := range []string{"validate", "publish", "release-go"} {
		body, err := releaseJobBody(r.workflow, name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "actions/workflows/ci.yml/runs") {
			t.Errorf("%s still reads a dev push CI run", name)
		}
	}
}

func (r *releaseRepo) ghLog(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r.log)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReleaseWorkflow_publication_needs_validation_and_a_real_request(t *testing.T) {
	r := releaseFixture(t)
	publish, err := releaseJobBody(r.workflow, "publish")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(publish, "needs: [validate, verify]") || !strings.Contains(publish, "inputs.dry_run == false") {
		t.Fatal("publish must follow validation and verification, and a real publication request")
	}
	metadata, _, err := releaseStepBlock(r.workflow, "validate", "release-credentials")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metadata, "if: inputs.dry_run == false") {
		t.Fatal("the credential check must run only for publication")
	}
}

func TestReleaseWorkflow_owner_dispatch_inputs(t *testing.T) {
	r := releaseFixture(t)
	r.pass(t, "prerelease tag", "inputs", map[string]string{"RELEASE_TAG": "v0.1.0-rc.1"})
	for label, extra := range map[string]map[string]string{
		"caller":    {"ACTOR": "intruder"},
		"rerun":     {"TRIGGERING_ACTOR": "intruder"},
		"branch":    {"GITHUB_REF": "refs/heads/main"},
		"sha":       {"RELEASE_SHA": "1234567"},
		"uppercase": {"RELEASE_SHA": strings.ToUpper(r.candidate)},
		"tag":       {"RELEASE_TAG": "v0.1.0; false"},
		"notes":     {"RELEASE_NOTES": "   "},
	} {
		t.Run(label, func(t *testing.T) { r.refuse(t, label, "inputs", extra) })
	}
}

// CRW-966: the release source check keeps its ancestry and remote tag rules. It no longer reads a
// dev push CI run, so the CI-run cases left with the lookup; the verification is the verify job's.
func TestReleaseWorkflow_source_requires_ancestry_and_remote_tag(t *testing.T) {
	r := releaseFixture(t)
	r.pass(t, "valid source", "source", nil)
	r.setCase(t, "tag", map[string]any{"case": "commit", "object_sha": r.base, "object_type": "commit"})
	r.refuse(t, "lightweight tag points elsewhere", "source", nil)
	r.setCase(t, "tag", map[string]any{"case": "annotated", "object_sha": strings.Repeat("a", 40), "object_type": "tag",
		"peeled_sha": r.base, "peeled_type": "commit"})
	r.refuse(t, "annotated tag peels to another commit", "source", nil)
	r.setCase(t, "tag", map[string]any{"peeled_sha": r.candidate})
	r.pass(t, "annotated tag peels to selected commit", "source", nil)
	r.setCase(t, "tag", map[string]any{"case": "error"})
	r.refuse(t, "tag api error", "source", nil)
	r.setCase(t, "tag", map[string]any{"case": "annotated", "peel": "error"})
	r.refuse(t, "annotated peel error", "source", nil)
}

func TestReleaseWorkflow_dry_run_reads_without_publication_and_missing_token_refuses_writes(t *testing.T) {
	r := releaseFixture(t)
	r.pass(t, "dry-run inputs", "inputs", nil)
	r.pass(t, "dry-run source", "source", nil)
	r.expectRemote(t, "main", r.base)
	r.expectRemote(t, "refs/tags/v0.1.0", "")
	if strings.Contains(r.ghLog(t), "release create") {
		t.Fatal("a dry run created a release")
	}
	r.refuse(t, "publication without token", "credentials", map[string]string{"RELEASE_TOKEN": ""})
	r.refuse(t, "publish without token", "publish", map[string]string{"GH_TOKEN": ""})
	r.expectRemote(t, "main", r.base)
	r.expectNoRelease(t)
}

func TestReleaseWorkflow_publication_creates_source_release_then_fast_forwards_main(t *testing.T) {
	r := releaseFixture(t)
	r.setCase(t, "release", map[string]any{"case": "error"})
	r.refuse(t, "release lookup error", "publish", nil)
	r.expectRemote(t, "main", r.base)
	r.expectNoRelease(t)
	r.setCase(t, "release", map[string]any{"case": "missing"})
	r.pass(t, "publish", "publish", nil)
	r.expectRemote(t, "main", r.candidate)
	r.expectRemote(t, "refs/tags/v0.1.0", r.candidate)
	created, _ := r.created(t)
	if !strings.Contains(created, "--verify-tag") || !strings.Contains(created, r.candidate) || strings.Contains(created, "--prerelease") {
		t.Fatalf("created: %s", created)
	}
	r.setCase(t, "tag", map[string]any{"case": "commit", "object_sha": r.candidate, "object_type": "commit"})
	r.setCase(t, "release", map[string]any{"case": "existing", "draft": false, "target": r.candidate})
	before := r.ghLog(t)
	r.pass(t, "same sha recovery", "publish", nil)
	if after := r.ghLog(t)[len(before):]; strings.Contains(after, "release create") {
		t.Fatalf("recovery created another release: %s", after)
	}
	r.expectRemote(t, "main", r.candidate)
}

func TestReleaseWorkflow_publication_refuses_conflicts_and_reports_partial_main_failure(t *testing.T) {
	r := releaseFixture(t)
	r.setCase(t, "tag", map[string]any{"case": "annotated", "object_sha": strings.Repeat("b", 40), "object_type": "tag",
		"peeled_sha": r.base, "peeled_type": "commit"})
	r.refuse(t, "remote annotated conflict", "publish", nil)
	r.expectRemote(t, "main", r.base)
	r.expectNoRelease(t)
	r.setCase(t, "tag", map[string]any{"case": "missing"})
	r.mustGit(t, "tag", "v0.9.0", r.base)
	r.refuse(t, "local tag points elsewhere", "publish", map[string]string{"RELEASE_TAG": "v0.9.0"})
	r.expectRemote(t, "refs/tags/v0.9.0", "")
	if got := r.mustGit(t, "rev-parse", "refs/tags/v0.9.0^{commit}"); got != r.base {
		t.Fatalf("local tag moved to %s", got)
	}
	r.expectNoRelease(t)
	r.setCase(t, "release", map[string]any{"case": "existing", "draft": true, "target": r.candidate})
	r.refuse(t, "draft release", "publish", nil)
	r.setCase(t, "release", map[string]any{"case": "existing", "draft": false, "target": r.base})
	r.refuse(t, "release target mismatch", "publish", nil)
	r.setCase(t, "release", map[string]any{"case": "missing"})
	r.setCase(t, "push", map[string]any{"fail_main": true})
	r.refuse(t, "main update failure", "publish", map[string]string{"RELEASE_TAG": "v0.1.1"})
	r.expectRemote(t, "refs/tags/v0.1.1", r.candidate)
	r.expectRemote(t, "main", r.base)
	if _, found := r.created(t); !found {
		t.Fatal("the release was not created before the main update")
	}
	r.setCase(t, "push", map[string]any{"fail_main": false, "lie_main": true})
	r.setCase(t, "tag", map[string]any{"case": "commit", "object_sha": r.candidate, "object_type": "commit"})
	r.setCase(t, "release", map[string]any{"case": "existing", "draft": false, "target": r.candidate})
	r.refuse(t, "main readback mismatch", "publish", map[string]string{"RELEASE_TAG": "v0.1.1"})
}

func TestReleaseWorkflow_draft_collision_refuses_before_creating_immutable_tag(t *testing.T) {
	r := releaseFixture(t)
	r.setCase(t, "release", map[string]any{"case": "existing", "draft": true, "target": r.candidate})
	r.refuse(t, "draft must be seen before any write", "publish", nil)
	r.expectRemote(t, "refs/tags/v0.1.0", "")
	r.expectRemote(t, "main", r.base)
	r.expectNoRelease(t)
	r.setCase(t, "release", map[string]any{"case": "paged-draft"})
	r.refuse(t, "draft on later page must be seen", "publish", nil)
	r.expectRemote(t, "refs/tags/v0.1.0", "")
}

func TestReleaseWorkflow_tag_only_failure_reports_and_recovers_same_commit(t *testing.T) {
	r := releaseFixture(t)
	r.setCase(t, "release", map[string]any{"case": "create-error"})
	outcome := r.refuse(t, "release creation failed after tag", "publish", nil)
	if !strings.Contains(outcome.stdout, "The tag exists, but release creation failed") {
		t.Fatalf("stdout: %s", outcome.stdout)
	}
	r.expectRemote(t, "refs/tags/v0.1.0", r.candidate)
	r.expectRemote(t, "main", r.base)
	r.expectNoRelease(t)
	r.setCase(t, "tag", map[string]any{"case": "commit", "object_sha": r.candidate, "object_type": "commit"})
	r.setCase(t, "release", map[string]any{"case": "missing"})
	r.pass(t, "recover without rewriting tag", "publish", nil)
	r.expectRemote(t, "main", r.candidate)
	r.expectRemote(t, "refs/tags/v0.1.0", r.candidate)
}

func TestReleaseWorkflow_release_go_runs_after_publication_and_snapshot_stays_in_validation(t *testing.T) {
	r := releaseFixture(t)
	ids, err := releaseStepIDs(r.workflow, "validate")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"release-credentials", "release-go-snapshot-tree", "release-go-snapshot"}; len(ids) < 3 || !slices.Equal(ids[len(ids)-3:], want) {
		t.Fatalf("validate steps end %v, want %v", ids, want)
	}
	validate, err := releaseJobBody(r.workflow, "validate")
	if err != nil {
		t.Fatal(err)
	}
	snapshot := validate[strings.Index(validate, "id: release-go-snapshot\n"):]
	if !strings.Contains(snapshot, "args: release --snapshot --clean") || strings.Contains(snapshot, "if:") {
		t.Fatalf("the snapshot build must also run on dry-run:\n%s", snapshot)
	}
	job, err := releaseJobBody(r.workflow, "release-go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(job, "needs: [validate, verify, publish]") || !strings.Contains(job, "if: inputs.dry_run == false") {
		t.Fatalf("release-go must follow verification and publication:\n%s", job)
	}
	if ids, err = releaseStepIDs(r.workflow, "release-go"); err != nil || !slices.Equal(ids, []string{"release-go-source", "release-go-tree", "release-go-publish"}) {
		t.Fatalf("release-go steps %v %v", ids, err)
	}
	publish := job[strings.Index(job, "id: release-go-publish"):]
	if !strings.Contains(publish, "args: release --clean") || strings.Contains(publish, "--snapshot") {
		t.Fatalf("release-go-publish:\n%s", publish)
	}
}

// Decision 34: the binaries carry HEAD's tree as their installed revision, which
// .goreleaser.yaml reads from SOURCE_TREE; a checkout with changes is not HEAD's tree.
func TestReleaseWorkflow_the_binaries_are_stamped_with_the_clean_checkouts_tree(t *testing.T) {
	r := releaseFixture(t)
	config, err := os.ReadFile(filepath.Join(RootMust(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "internal/runtime/record.sourceTree={{ .Env.SOURCE_TREE }}") {
		t.Fatal(".goreleaser.yaml does not stamp SOURCE_TREE")
	}
	tree := r.mustGit(t, "rev-parse", "HEAD^{tree}")
	for _, block := range []string{"go-tree", "snapshot-tree"} {
		t.Run(block, func(t *testing.T) {
			envFile := filepath.Join(r.dir, block+".env")
			read := func() string {
				raw, err := os.ReadFile(envFile)
				if err != nil {
					t.Fatal(err)
				}
				return string(raw)
			}
			write := func(path, text string) {
				if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write(envFile, "")
			r.pass(t, "clean checkout", block, map[string]string{"GITHUB_ENV": envFile})
			if got := read(); got != "SOURCE_TREE="+tree+"\n" {
				t.Fatalf("GITHUB_ENV %q", got)
			}
			stray := filepath.Join(r.checkout, "stray")
			write(stray, "x")
			write(envFile, "")
			r.refuse(t, "checkout with changes", block, map[string]string{"GITHUB_ENV": envFile})
			if got := read(); got != "" {
				t.Fatalf("GITHUB_ENV %q after a refusal", got)
			}
			if err := os.Remove(stray); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReleaseWorkflow_release_go_refuses_a_tag_that_is_not_the_released_commit(t *testing.T) {
	r := releaseFixture(t)
	r.mustGit(t, "tag", "v0.1.0", r.base)
	r.refuse(t, "tag on another commit", "go-source", nil)
	r.mustGit(t, "tag", "-f", "v0.1.0", r.candidate)
	r.pass(t, "tag on the released commit", "go-source", nil)
}

func TestReleaseWorkflow_old_and_divergent_sources_do_not_advance_main(t *testing.T) {
	r := releaseFixture(t)
	r.mustGit(t, "commit", "--allow-empty", "-m", "newer main")
	advanced := r.mustGit(t, "rev-parse", "HEAD")
	r.mustGit(t, "push", "origin", "HEAD:main")
	r.refuse(t, "source behind main", "source", nil)
	r.refuse(t, "publish behind main", "publish", map[string]string{"RELEASE_TAG": "v0.4.0"})
	r.expectRemote(t, "main", advanced)
	r.mustGit(t, "reset", "--hard", "origin/dev")
	restored := r.mustGit(t, "rev-parse", "HEAD")
	r.mustGit(t, "push", "--force-with-lease", "origin", "HEAD:main")
	r.mustGit(t, "switch", "-c", "outside")
	r.mustGit(t, "commit", "--allow-empty", "-m", "outside")
	outside := r.mustGit(t, "rev-parse", "HEAD")
	r.refuse(t, "outside dev", "source", map[string]string{"RELEASE_SHA": outside})
	r.refuse(t, "publish outside", "publish", map[string]string{"RELEASE_SHA": outside, "RELEASE_TAG": "v0.5.0"})
	r.expectRemote(t, "main", restored)
	r.expectNoRelease(t)
}
