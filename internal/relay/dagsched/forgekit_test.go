package dagsched

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// forgeKitRepository is the synthetic forge repository (owner/name) that the integration kit's edges, acceptances and observations name. No forge answers for it:
// forgeKitReaders maps it to the kit's temporary repository.
const forgeKitRepository = "owner/repo"

// forgeKitReaders are the integration kit's fake tip and ancestry readers.
//
// An owner/name repository is a synthetic forge identity. It is answered only when the kit mapped it to a temporary repository, and then from that repository's git. A forge nobody mapped is an
// error, and it is also remembered, so that a caller which swallows the error (a sweep skips a tip it cannot read) still fails the test at its end. An absolute path is a local checkout and
// goes to the production local readers unchanged: that is how the rows that seed a local target on purpose (acceptNode with a path) and the generic local ancestry reader keep their own coverage.
// Nothing here starts gh or reaches a network.
type forgeKitReaders struct {
	t        testing.TB
	mu       sync.Mutex
	mapped   map[string]string // forge identity -> absolute path of the temporary repository
	unmapped []string          // the forge identities that were asked about and mapped to nothing, in order
}

func newForgeKitReaders(t testing.TB) *forgeKitReaders {
	t.Helper()
	r := &forgeKitReaders{t: t, mapped: map[string]string{}}
	t.Cleanup(func() {
		if left := r.takeUnmapped(); len(left) > 0 {
			t.Errorf("the integration kit was asked about forge repositories it maps to no temporary repository: %v", left)
		}
	})
	return r
}

// mapTo says that the forge repository is the temporary repository at path.
func (r *forgeKitReaders) mapTo(forge, path string) {
	r.t.Helper()
	if err := r.tryMapTo(forge, path); err != nil {
		r.t.Fatal(err)
	}
}

// tryMapTo is mapTo that says why it refuses. A forge is spelled exactly as the product's tip reader takes it: evidence.SplitRepository trims what surrounds the name, but mergeturn.TargetReader
// matches the repository against its slug pattern as given and refuses a spelling that differs from the trimmed one, so this does not map it.
func (r *forgeKitReaders) tryMapTo(forge, path string) error {
	owner, name, err := evidence.SplitRepository(forge)
	if err != nil || owner+"/"+name != forge {
		return fmt.Errorf("%q is not an owner/name forge repository spelled the way the product spells it", forge)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not the absolute path of a temporary repository", path)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mapped[forge] = path
	return nil
}

// resolve is the repository git is asked about for a repository, and whether the repository is a forge identity.
func (r *forgeKitReaders) resolve(repository string) (path string, forge bool, err error) {
	if filepath.IsAbs(repository) {
		return repository, false, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if path, ok := r.mapped[repository]; ok {
		return path, true, nil
	}
	r.unmapped = append(r.unmapped, repository)
	return "", true, fmt.Errorf("the integration kit maps no temporary repository to forge repository %q", repository)
}

// takeUnmapped returns the forge identities asked about and mapped to nothing since the last call, and forgets them: a test that provokes the failure on purpose takes it.
func (r *forgeKitReaders) takeUnmapped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.unmapped
	r.unmapped = nil
	return out
}

// Tip reads the branch of the repository a forge identity is mapped to, and reports it as the forge repository's tip, which is what a forge read reports.
func (r *forgeKitReaders) Tip(ctx context.Context, repository, base string) (mergeturn.Tip, error) {
	path, forge, err := r.resolve(repository)
	if err != nil {
		return mergeturn.Tip{}, err
	}
	if forge && forgeKitRefusesBranch(base) {
		return mergeturn.Tip{}, &mergeturn.TargetUnreadable{Detail: fmt.Sprintf("a branch name is a path segment without traversal or query characters, not %q", base)}
	}
	tip, err := mergeturn.TargetReader{}.Tip(ctx, path, base)
	if err == nil && forge {
		tip.Repository = repository
	}
	return tip, err
}

// forgeKitRefusesBranch is the check the forge reader makes of a branch name beyond the one every reader makes (mergeturn target.go:140), and a local checkout does not make. It is a copy, and
// TestForgeKitRejectsTheBranchNamesTheForgeReaderRejects holds it equal to the product's.
func forgeKitRefusesBranch(base string) bool {
	return strings.ContainsAny(base, "?#%:`^\\") || strings.Contains(base, "..") || strings.HasPrefix(base, "/")
}

// Ancestry asks the production local reader about the repository a forge identity is mapped to. The method it reports is the one that measured: git, not the forge compare API.
func (r *forgeKitReaders) Ancestry(ctx context.Context, repository, subject, tip string) (bool, string, error) {
	path, _, err := r.resolve(repository)
	if err != nil {
		return false, "", err
	}
	return GitAncestry{}.Ancestry(ctx, path, subject, tip)
}

// forgeTarget is a branch of the kit's forge repository: where the kit's plan lands the head of I, and what an observation under the forge names.
func (k *integrationKit) forgeTarget(base string) Target {
	return Target{Repository: forgeKitRepository, BaseRef: base}
}

// acceptOnForge seeds the acceptance a NEW implementation node leaves: its integration target is the forge repository of its pull request, the shape the acceptance of a node whose outgoing edges
// land on the forge writes. The raw-SQL seeds of LEGACY local rows stay with acceptNode and a path as its Repository; this helper cannot be pointed at one.
func (k *integrationKit) acceptOnForge(plan, node string, o acceptOpts) accepted {
	k.t.Helper()
	if o.Repository != "" || o.Forge != "" {
		k.t.Fatalf("acceptOnForge names the forge itself: Repository %q and Forge %q were given", o.Repository, o.Forge)
	}
	o.Repository, o.Forge = forgeKitRepository, forgeKitRepository
	return k.acceptNode(plan, node, o)
}

// The mapping is the kit's explicit statement of which temporary repository a forge identity stands for: a forge is read from the repository it is mapped to and from no other, and the tip is reported
// under the forge's own name.
func TestForgeKitMapsEachForgeToItsOwnRepository(t *testing.T) {
	t.Parallel()
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	other := newGitRepo(t)
	other.commit("other.txt", "other")
	k.readers.mapTo("owner/other", other.path)
	ctx := context.Background()

	tip, err := k.readers.Tip(ctx, forgeKitRepository, "dev")
	if err != nil || tip.SHA != repo.git("rev-parse", "dev") || tip.Repository != forgeKitRepository || tip.Reference != "refs/heads/dev" || tip.Source != "local_git" {
		t.Fatalf("the forge's tip = %+v %v, want dev of its temporary repository, read from git, under the forge's name", tip, err)
	}
	if otherTip, err := k.readers.Tip(ctx, "owner/other", "dev"); err != nil || otherTip.SHA != other.git("rev-parse", "dev") || otherTip.SHA == tip.SHA || otherTip.Repository != "owner/other" {
		t.Fatalf("the second forge's tip = %+v %v", otherTip, err)
	}
	// the feature commit exists in the first repository only: the answer depends on the mapping
	if in, method, err := k.readers.Ancestry(ctx, forgeKitRepository, repo.git("rev-parse", "dev"), feature); err != nil || !in || method != "git merge-base --is-ancestor" {
		t.Fatalf("dev in feature of the forge = %v %q %v", in, method, err)
	}
	if _, _, err := k.readers.Ancestry(ctx, "owner/other", feature, other.git("rev-parse", "dev")); err == nil {
		t.Fatal("the commit of one repository was found in the repository another forge is mapped to")
	}
}

// A forge nobody mapped has no answer. Each reader errors, and the kit also remembers the request, so that a caller which swallows the error (liveheads.go skips a tip it cannot read) still fails
// the test when it ends. This test takes what it provoked.
func TestForgeKitFailsOnAnUnmappedForge(t *testing.T) {
	t.Parallel()
	k := newIntegrationKit(t)
	k.declare("g", "I", "x.go")
	k.acceptOnForge("g", "I", acceptOpts{HeadSHA: head1, PR: 5})
	ctx := context.Background()
	var want []string
	for _, repository := range []string{"other/repo", "owner/Repo", "repo", "not a repo"} {
		if tip, err := k.readers.Tip(ctx, repository, "dev"); err == nil || tip.SHA != "" || !strings.Contains(err.Error(), repository) {
			t.Fatalf("the tip of unmapped %q = %+v %v", repository, tip, err)
		}
		if in, _, err := k.readers.Ancestry(ctx, repository, head1, head1); err == nil || in {
			t.Fatalf("the ancestry in unmapped %q = %v %v", repository, in, err)
		}
		want = append(want, repository, repository)
	}
	// through the scheduler: the observation of an unmapped target is an error and writes nothing
	if _, err := k.observe(Target{Repository: "other/repo", BaseRef: "dev"}); err == nil || !strings.Contains(err.Error(), "other/repo") {
		t.Fatalf("observing an unmapped forge = %v", err)
	}
	want = append(want, "other/repo")
	if n := k.count("SELECT COUNT(*) FROM dag_integration_observations"); n != 0 {
		t.Fatalf("%d observations were written for an unmapped forge", n)
	}
	if got := k.readers.takeUnmapped(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the unmapped requests = %q, want %q", got, want)
	}
}

// An absolute path is a local checkout, and the forge kit leaves it to the production local readers: a legacy local row keeps being observed under its path, the forge is never consulted for it,
// and nothing is recorded under the forge for it.
func TestForgeKitLeavesLocalCheckoutsToTheLocalReaders(t *testing.T) {
	t.Parallel()
	k := newIntegrationKit(t)
	// the local checkout is a repository of its own, so that sending its requests to the repository the forge is mapped to would not give the same answers
	repo := newGitRepo(t)
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.declare("g", "I", "feature.txt")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: forgeKitRepository, Repository: repo.path})
	res, err := k.observe(Target{Repository: repo.path, BaseRef: "dev"})
	if err != nil || len(res.Observations) != 1 || !res.Observations[0].IsAncestor || res.Observations[0].Repository != repo.path || res.Observations[0].Method != "git merge-base --is-ancestor" ||
		res.Observations[0].TipSHA != repo.git("rev-parse", "dev") || res.Observations[0].TipSHA == k.repo.git("rev-parse", "dev") {
		t.Fatalf("a legacy local target = %v %+v", err, res)
	}
	if k.count("SELECT COUNT(*) FROM dag_integration_observations WHERE repository = ?", repo.path) != 1 || k.count("SELECT COUNT(*) FROM dag_integration_observations WHERE repository = ?", forgeKitRepository) != 0 {
		t.Fatal("the observation of a local checkout was not recorded under that checkout alone")
	}
	if got := k.readers.takeUnmapped(); len(got) != 0 {
		t.Fatalf("a local checkout consulted the forge mapping: %q", got)
	}
}

// recordingTB is the testing.TB a forge kit's readers can be built on to watch what their cleanup reports, instead of failing the test that is running.
type recordingTB struct {
	testing.TB
	cleanups []func()
	errors   []string
}

func (r *recordingTB) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// finish runs the cleanups the way the test runner does, last registered first.
func (r *recordingTB) finish() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	r.cleanups = nil
}

// What catches a caller that swallows the reader's error is the failure at the end of the test: an unmapped request nobody took is reported once, naming the forge, and one a test took is not.
func TestForgeKitReportsAnUnmappedRequestNobodyTook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	left := &recordingTB{TB: t}
	readers := newForgeKitReaders(left)
	readers.mapTo(forgeKitRepository, t.TempDir())
	if _, err := readers.Tip(ctx, "swallowed/repo", "dev"); err == nil {
		t.Fatal("an unmapped forge was answered")
	}
	left.finish()
	if len(left.errors) != 1 || !strings.Contains(left.errors[0], "swallowed/repo") {
		t.Fatalf("the cleanup reported %q, want one error naming the unmapped forge", left.errors)
	}
	taken := &recordingTB{TB: t}
	readers = newForgeKitReaders(taken)
	if _, err := readers.Tip(ctx, "swallowed/repo", "dev"); err == nil {
		t.Fatal("an unmapped forge was answered")
	}
	readers.takeUnmapped()
	taken.finish()
	if len(taken.errors) != 0 {
		t.Fatalf("the cleanup reported %q for a request the test took", taken.errors)
	}
}

// The guard of the mapping is activated by spellings the product's tip reader would not read: mergeturn.TargetReader checks the repository against its slug pattern as given, so a name that differs
// from its trimmed form is refused there, and a kit that mapped it would answer for a repository that reader refuses. Nothing is registered by a refusal.
func TestForgeKitRefusesMisspelledMappings(t *testing.T) {
	t.Parallel()
	readers := newForgeKitReaders(t)
	path := t.TempDir()
	for _, forge := range []string{" owner/repo ", "owner/repo\n", "repo", "/tmp/owner/repo", "/repo", "owner/", "a/b/c", ""} {
		if err := readers.tryMapTo(forge, path); err == nil {
			t.Errorf("the forge %q was mapped", forge)
		}
	}
	if err := readers.tryMapTo(forgeKitRepository, "relative/path"); err == nil {
		t.Error("a relative path was mapped")
	}
	if len(readers.mapped) != 0 {
		t.Fatalf("a refused mapping was registered: %v", readers.mapped)
	}
	if err := readers.tryMapTo("owner-x/repo.y_z", path); err != nil {
		t.Fatalf("a valid spelling was refused: %v", err)
	}
}

// The forge reader refuses some branch names that git accepts. The kit's reader answers for a forge, so it has to refuse them too, and this holds the copy of the rule equal to the product's: the
// product reader is pointed at a gh that does not exist, so a branch name it refuses is refused before any process starts and one it accepts fails to start gh. Neither reaches a network.
func TestForgeKitRejectsTheBranchNamesTheForgeReaderRejects(t *testing.T) {
	t.Parallel()
	k := newIntegrationKit(t)
	ctx := context.Background()
	product := mergeturn.TargetReader{GH: filepath.Join(t.TempDir(), "no-gh")}
	refused := func(err error) bool {
		return err != nil && (strings.Contains(err.Error(), "a branch name is a path segment") || strings.Contains(err.Error(), "a base ref is a plain branch name"))
	}
	for _, base := range []string{"dev", "release/1.0", "topic#42", "50%off", "a`b", "x:y", "a^b", "a?b", "a\\b", "a..b", "/abs", "-flag", "trailing.", "a b"} {
		_, productErr := product.Tip(ctx, forgeKitRepository, base)
		_, kitErr := k.readers.Tip(ctx, forgeKitRepository, base)
		if refused(productErr) != refused(kitErr) {
			t.Errorf("branch %q: the forge reader refuses it = %v (%v), the kit's reader = %v (%v)", base, refused(productErr), productErr, refused(kitErr), kitErr)
		}
	}
	// the probes that only the forge reader refuses stay meaningful: if the product stops refusing one, this fails and the copy is reconciled
	for _, base := range []string{"topic#42", "50%off", "a`b"} {
		if _, err := product.Tip(ctx, forgeKitRepository, base); !refused(err) {
			t.Errorf("the forge reader no longer refuses %q (%v)", base, err)
		}
	}
}
