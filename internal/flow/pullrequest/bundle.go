package pullrequest

// OpenBundle is the parent's pull request opener for a PR-unit milestone (CRW-1423, step 3): called by crw.prOpen after the
// journal proved the milestone's closing acceptance. It opens or reuses the pull request the pinned target names — the
// close's tip on the target's head branch onto its base — with the close-proven body (no caller-supplied acceptance), the
// persisted review record refreshed, and never a duplicate pull request or a forced push.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// finderSource is the pull request surface the bundle opener needs: the existing reader plus the head/base lookup.
type finderSource interface {
	PullRequest(context.Context, tracking.Ref) (tracking.PullRequest, error)
	OpenPullRequest(context.Context, tracking.NewPullRequest) (tracking.PullRequest, error)
	FindPullRequest(context.Context, string, string) (tracking.PullRequest, bool, error)
}

// bundleBody makes the body facts the pull request states from the journal-proved close (its gate and its accepted closing
// review), never from the caller.
func bundleBody(in ops.BundlePROpenRequest) BodyFacts {
	f := BodyFacts{Milestone: in.Milestone.ID, Title: in.Milestone.Title, Criteria: in.Milestone.Criteria, Tip: in.Close.Tip}
	var gate ops.GateOutcome
	if json.Unmarshal(in.Close.Gate, &gate) == nil {
		f.Gate.State = gate.State
		for _, e := range gate.Evidence {
			f.Gate.Evidence = append(f.Gate.Evidence, Evidence{Kind: e.Kind, Command: e.Command, State: e.State, Digest: e.Digest})
		}
	}
	var v map[string]any
	if json.Unmarshal(in.Close.Crosscheck, &v) == nil && str(v, "verdict") != "" {
		f.Crosscheck = Crosscheck{Verdict: str(v, "verdict"), Head: in.Close.Tip, EngineFamily: str(v, "engine_family"),
			Model: str(v, "model"), GuideRev: str(v, "guide_rev"), Round: intOf(v["round"])}
		if list, ok := v["follow_ups"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok {
					f.Crosscheck.FollowUps = append(f.Crosscheck.FollowUps, s)
				}
			}
		}
	}
	return f
}

// OpenBundle opens or reuses the pull request of a closed bundle milestone: it pins the close's tip, pushes exactly it when
// the pull request does not exist yet (never forced, never another sha), refreshes the persisted review record and answers
// the opened pull request. A pull request that already exists at another head or closed-unmerged is a refusal — no second
// pull request is ever opened.
func OpenBundle(ctx context.Context, d *Deps, in ops.BundlePROpenRequest) (ops.BundlePRResult, error) {
	target := in.Target
	if target.Remote == "" {
		target.Remote = "origin"
	}
	if target.Repo == "" || target.Checkout == "" || target.HeadRef == "" || target.BaseRef == "" {
		return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the target lacks a repository, checkout, head or base")
	}
	refRepo := target.Repo
	if err := checkDecideBranch("head", target.HeadRef); err != nil {
		return ops.BundlePRResult{}, err
	}
	if err := checkDecideBranch("base", target.BaseRef); err != nil {
		return ops.BundlePRResult{}, err
	}
	if !decideSHA.MatchString(in.Close.Tip) {
		return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the close's tip %q is not a full commit id", in.Close.Tip)
	}
	if in.Milestone.ID == "" {
		return ops.BundlePRResult{}, errors.New("crw.prOpen: the milestone names no id")
	}
	if d == nil || d.NewPullRequests == nil {
		return ops.BundlePRResult{}, errors.New("crw.prOpen: no pull request adapter factory is wired")
	}
	env := gitprobe.Sanitize(d.Env, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	// the head branch must still be exactly the close's tip before anything remote is done
	tip, err := localTip(ctx, env, target.Checkout, target.HeadRef)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	if tip != in.Close.Tip {
		return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the branch %s of %s is at %s, not the close's tip %s", target.HeadRef, target.Checkout, tip, in.Close.Tip)
	}
	okRemote, err := configuredRemote(ctx, env, target.Checkout, target.Remote)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	if !okRemote {
		return ops.BundlePRResult{}, fmt.Errorf("%s is not a remote configured in %s", target.Remote, target.Checkout)
	}
	prs, err := d.NewPullRequests(refRepo)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	src, ok := prs.(finderSource)
	if !ok {
		return ops.BundlePRResult{}, fmt.Errorf("the pull request adapter of %s cannot look up pull requests", refRepo)
	}
	// one opener at a time per record file; the review record's own round lock is a sibling
	if target.ReviewRecord == "" {
		return ops.BundlePRResult{}, errors.New("crw.prOpen: the target names no review record")
	}
	unlock, err := decideLock(target.ReviewRecord + ".open.lock")
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	defer unlock()
	// the lookup before any POST: an existing pull request at this head/base is reused or refuses, never duplicated
	existing, found, err := src.FindPullRequest(ctx, target.HeadRef, target.BaseRef)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	var pr tracking.PullRequest
	switch {
	case found && existing.Merged:
		pr = existing // a merged pull request at the tip is the record
	case found && existing.State == tracking.StateOpen && existing.HeadSHA == in.Close.Tip:
		pr = existing
	case found:
		return ops.BundlePRResult{State: "refused", Reason: fmt.Sprintf("a pull request of %s onto %s already exists at %s (%s)", target.HeadRef, target.BaseRef, existing.HeadSHA, existing.State)}, nil
	default:
		// the local branch is re-checked right before the push: exactly the close's tip goes out
		if tip, err := localTip(ctx, env, target.Checkout, target.HeadRef); err != nil {
			return ops.BundlePRResult{}, err
		} else if tip != in.Close.Tip {
			return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the branch %s moved to %s during the open", target.HeadRef, tip)
		}
		refs, reason, message := carriedRefs(in.Milestone.Nodes, nil, refRepo)
		if reason != "" {
			return ops.BundlePRResult{State: "refused", Reason: message}, nil
		}
		facts := bundleBody(in)
		pol, err := d.publicationPolicy(refRepo)
		if err != nil {
			return ops.BundlePRResult{}, err
		}
		pubctx := d.pubContext()
		scrub := func(text string) (string, error) {
			r := publishpolicy.Scrub(text, pubctx, pol)
			if r.Blocked {
				return "", fmt.Errorf("crw.prOpen: the publication scrub blocked the pull request's text")
			}
			return r.Text, nil
		}
		// Check raw facts before rendering prefixes or shortened digests can hide a credential shape.
		raw, err := json.Marshal(facts)
		if err != nil {
			return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the body facts cannot be encoded: %w", err)
		}
		if _, err = scrub(string(raw)); err != nil {
			return ops.BundlePRResult{}, err
		}
		body, err := scrub(Body(facts, refs))
		if err != nil {
			return ops.BundlePRResult{}, err
		}
		title, err := scrub("Milestone " + in.Milestone.ID)
		if err != nil {
			return ops.BundlePRResult{}, err
		}
		if err := pushCommit(ctx, env, target.Checkout, target.Remote, target.HeadRef, in.Close.Tip); err != nil {
			return ops.BundlePRResult{}, err
		}
		pr, err = src.OpenPullRequest(ctx, tracking.NewPullRequest{Head: target.HeadRef, Base: target.BaseRef, Title: title, Body: body, Issues: refs})
		if err != nil {
			// a concurrent opener may have landed first: one lookup decides before the error is reported
			if again, ok, lerr := src.FindPullRequest(ctx, target.HeadRef, target.BaseRef); lerr == nil && ok && again.HeadSHA == in.Close.Tip && (again.State == tracking.StateOpen || again.Merged) {
				pr = again
			} else {
				return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: open the pull request of %s onto %s: %w", target.HeadRef, target.BaseRef, err)
			}
		}
	}
	// the pull request must still carry the close's tip (a server-side race is an error, never an approval): the answer's
	// metadata is not trusted — the live read is
	live, err := src.PullRequest(ctx, pr.Ref)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	if !strings.EqualFold(live.Ref.Repo, refRepo) || live.HeadRef != target.HeadRef || live.HeadSHA != in.Close.Tip ||
		live.BaseRef != target.BaseRef || !strings.EqualFold(live.HeadRepo, refRepo) {
		return ops.BundlePRResult{}, fmt.Errorf("crw.prOpen: the pull request %s is %s at %s onto %s of %s, not %s at %s onto %s of %s",
			live.Ref, live.HeadRef, live.HeadSHA, live.BaseRef, live.HeadRepo, target.HeadRef, in.Close.Tip, target.BaseRef, refRepo)
	}
	pr = live
	// refresh the review record for this head: one collection decides the bots' connectivity, and the round carries on.
	// A previous record keeps the bots connected even when the collection is empty now — a deleted comment on a retry must
	// not reset unresolved items by claiming disconnected.
	reviewSrc, ok := prs.(interface {
		PullRequest(context.Context, tracking.Ref) (tracking.PullRequest, error)
		ReviewComments(context.Context, tracking.Ref) ([]tracking.PRReviewComment, error)
	})
	if !ok {
		return ops.BundlePRResult{}, fmt.Errorf("the pull request adapter of %s cannot read review comments", refRepo)
	}
	comments, err := reviewSrc.ReviewComments(ctx, pr.Ref)
	if err != nil {
		return ops.BundlePRResult{}, err
	}
	bots := tracking.ReviewBotsConnected
	prev, perr := tracking.ReadPRReviewHistory(target.ReviewRecord, pr.Ref)
	if perr != nil && !errors.Is(perr, os.ErrNotExist) {
		return ops.BundlePRResult{}, perr
	}
	if len(comments) == 0 && perr != nil {
		bots = tracking.ReviewBotsNotConnected
	} else if len(comments) == 0 && prev.Bots != tracking.ReviewBotsConnected && len(prev.Items) == 0 {
		bots = tracking.ReviewBotsNotConnected
	}
	round := 1
	if perr == nil {
		if prev.Head == pr.HeadSHA {
			round = prev.Round
		} else {
			round = prev.Round + 1
		}
	}
	if round > 2 {
		return ops.BundlePRResult{State: "refused", Reason: "the review record is past its two rounds"}, nil
	}
	if _, err := Comments(ctx, reviewSrc, CommentsInput{PR: pr.Ref.String(), Head: pr.HeadSHA, Record: target.ReviewRecord, Bots: bots, Round: round}); err != nil {
		return ops.BundlePRResult{}, err
	}
	return ops.BundlePRResult{State: "opened", PR: pr.Ref.String(), Head: pr.HeadSHA, Base: pr.BaseRef, ReviewRecord: target.ReviewRecord}, nil
}
