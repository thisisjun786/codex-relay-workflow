package contracttest

import (
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
	if !strings.Contains(publish, "needs: validate") || !strings.Contains(publish, "inputs.dry_run == false") {
		t.Fatal("publish must follow validation and a real publication request")
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

func TestReleaseWorkflow_source_requires_latest_exact_push_ci_and_remote_tag(t *testing.T) {
	r := releaseFixture(t)
	r.pass(t, "valid source", "source", nil)
	r.setCase(t, "ci", map[string]any{"older": true, "conclusion": "failure", "run_number": 3})
	r.refuse(t, "older success does not hide latest failure", "source", nil)
	for _, c := range []struct {
		label  string
		values map[string]any
	}{
		{"missing", nil},
		{"error", nil},
		{"running", map[string]any{"status": "in_progress", "conclusion": ""}},
		{"failed", map[string]any{"conclusion": "failure"}},
		{"cancelled", map[string]any{"conclusion": "cancelled"}},
		{"wrong-sha", map[string]any{"sha": strings.Repeat("0", 40)}},
		{"pr-only", map[string]any{"event": "pull_request"}},
		{"manual", map[string]any{"event": "workflow_dispatch"}},
		{"wrong-branch", map[string]any{"branch": "main"}},
		{"latest-cancelled", nil},
		{"latest-attempt-failed", nil},
		{"stale-attempt", nil},
		{"paged-pending", nil},
		{"bad-page", nil},
	} {
		t.Run(c.label, func(t *testing.T) {
			r.setCase(t, "ci", map[string]any{"case": "success", "older": false, "conclusion": "success",
				"status": "completed", "event": "push", "branch": "dev", "sha": "", "attempt": 1})
			r.setCase(t, "ci", map[string]any{"case": c.label})
			r.setCase(t, "ci", c.values)
			r.refuse(t, c.label, "source", nil)
		})
	}
	r.setCase(t, "ci", map[string]any{"case": "paged-success"})
	r.pass(t, "paginated successes still select the newest", "source", nil)
	r.setCase(t, "ci", map[string]any{"case": "success"})
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
	if !strings.Contains(job, "needs: [validate, publish]") || !strings.Contains(job, "if: inputs.dry_run == false") {
		t.Fatalf("release-go must follow publication:\n%s", job)
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
