package cli

// The pull request seams of the shipped lane (CRW-1423): the plan's pinned handoff decides whether a milestone is a bundle
// (its target names pr_base), and the parent's opener is crw flow pr's OpenBundle over the tracking adapter the factory
// makes — lazily, so a direct plan never touches GitHub or the configuration's tracking section.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/pullrequest"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// prFactory makes the tracking adapter of one repository: the option's, else the shipped GitHub factory. It is called only
// when a pull request is actually read or opened.
type prFactory func(string) (tracking.PullRequests, error)

// lanePRFactory is opt.NewPullRequests or the shipped adapter factory.
func (opt LaneOptions) lanePRFactory() prFactory {
	if opt.NewPullRequests != nil {
		return opt.NewPullRequests
	}
	return func(repo string) (tracking.PullRequests, error) {
		return publication.NewGitHub(repo)
	}
}

// lanePRRepo is the tracking repository a bundle's pull request lives in: the test override, else the configuration's
// tracking section (kind github) — read lazily, only when a bundle is actually opened.
func (opt LaneOptions) lanePRRepo(getenv func(string) (string, bool)) (string, error) {
	if opt.PRRepository != "" {
		return opt.PRRepository, nil
	}
	get := func(k string) string {
		v, _ := getenv(k)
		return v
	}
	file, err := crwconfig.Load(get, "")
	if err != nil {
		return "", err
	}
	t, err := file.Tracking()
	if err != nil {
		return "", err
	}
	if t.Kind != crwconfig.TrackingGitHub {
		return "", fmt.Errorf("the tracking section is kind %q, a bundle's pull request needs github", t.Kind)
	}
	return t.Repo, nil
}

// lanePRReader is ops.PullRequests over the factory: each call makes the adapter of the ref's repository, lazily.
type lanePRReader struct{ f prFactory }

func (r lanePRReader) PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error) {
	prs, err := r.f(ref.Repo)
	if err != nil {
		return tracking.PullRequest{}, err
	}
	return prs.PullRequest(ctx, ref)
}

func (r lanePRReader) Checks(ctx context.Context, ref tracking.Ref) (tracking.Checks, error) {
	prs, err := r.f(ref.Repo)
	if err != nil {
		return tracking.Checks{}, err
	}
	return prs.Checks(ctx, ref)
}

// lanePRSeams wires crw.prOpen and the pull request reads of the shipped lane over the run's handoff and store.
func lanePRSeams(relay *SchedRelay, req LaneRequest, opt LaneOptions, spec Spec) (ops.PullRequests, func(context.Context, string) (ops.BundlePRTarget, error), func(context.Context, ops.BundlePROpenRequest) (ops.BundlePRResult, error)) {
	getenv := opt.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	factory := opt.lanePRFactory()
	runDir := RunDir(req.State, spec.RunID)
	target := func(ctx context.Context, plan string) (ops.BundlePRTarget, error) {
		p, err := relay.project(plan)
		if err != nil {
			return ops.BundlePRTarget{}, err
		}
		if p.Target == nil || p.Target.PRBase == "" {
			return ops.BundlePRTarget{}, nil
		}
		// the relay's target answers the validated local checkout and head branch (its repository must be the run's), not the
		// raw handoff fields
		checkout, headRef, err := relay.target(plan)
		if err != nil {
			return ops.BundlePRTarget{}, err
		}
		repo, err := opt.lanePRRepo(getenv)
		if err != nil {
			return ops.BundlePRTarget{}, err
		}
		return ops.BundlePRTarget{
			Repo: repo, Checkout: checkout, HeadRef: headRef,
			BaseRef: strings.TrimPrefix(p.Target.PRBase, "refs/heads/"), Remote: "origin", ReviewRecord: runDir,
		}, nil
	}
	open := func(ctx context.Context, in ops.BundlePROpenRequest) (ops.BundlePRResult, error) {
		// the review record file lives under the run's directory, named by the milestone only (never by a path it hands)
		sum := sha256.Sum256([]byte(in.Milestone.ID))
		in.Target.ReviewRecord = filepath.Join(in.Target.ReviewRecord, "pr-review-"+hex.EncodeToString(sum[:])[:16]+".json")
		d := &pullrequest.Deps{NewPullRequests: func(repo string) (tracking.PullRequests, error) { return factory(repo) }, Env: nil}
		return pullrequest.OpenBundle(ctx, d, in)
	}
	return lanePRReader{f: factory}, target, open
}
