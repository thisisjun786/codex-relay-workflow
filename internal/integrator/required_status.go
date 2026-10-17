package integrator

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

type RequiredStatusPublisher struct {
	Context string
	Git     func(context.Context, string, ...string) (string, error)
	GitHub  func(string) (*tracking.GitHub, error)
}

func requiredStatusSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (p RequiredStatusPublisher) pushURLs(ctx context.Context, checkout, remote string) (string, error) {
	if publication.Repository(remote) == "" {
		return p.Git(ctx, checkout, "remote", "get-url", "--push", "--all", remote)
	}
	const probe = "crw-required-status-url"
	output, err := p.Git(ctx, checkout,
		"-c", "remote."+probe+".url=",
		"-c", "remote."+probe+".url="+remote,
		"-c", "remote."+probe+".pushurl=", "remote", "-v")
	if err != nil {
		return "", err
	}
	var urls []string
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(line, probe+"\t")
		if !ok {
			continue
		}
		if url, ok := strings.CutSuffix(value, " (push)"); ok {
			urls = append(urls, url)
		}
	}
	return strings.Join(urls, "\n"), nil
}

func (p RequiredStatusPublisher) Publish(ctx context.Context, checkout, remote, branch, head, base string, raw []byte) error {
	if p.Context == "" {
		return nil
	}
	if err := (crwconfig.IntegrationSettings{RequiredStatus: p.Context}).Validate(); err != nil {
		return fmt.Errorf("required status: %w", err)
	}
	if !requiredStatusSHA(head) || !requiredStatusSHA(base) {
		return errors.New("required status: the head and base are not full SHAs")
	}
	target := strings.TrimPrefix(branch, "refs/heads/")
	if target == "ci/required-status/"+head {
		return errors.New("required status: the integration target is the staging branch itself")
	}
	keys, err := dagsched.CommitVerificationKeys(ctx, checkout, head)
	if err != nil {
		return fmt.Errorf("required status: %w", err)
	}
	keys.Base, keys.OS, keys.Arch = base, runtime.GOOS, runtime.GOARCH
	record, err := dagsched.JudgeVerificationRecord(raw, keys)
	if err != nil {
		return fmt.Errorf("required status: %w", err)
	}
	if record.HeadCommit != head {
		return errors.New("required status: verification head is not the integration head")
	}
	if _, err := p.Git(ctx, checkout, "merge-base", "--is-ancestor", base, head); err != nil {
		return errors.New("required status: the base is not an ancestor of the head")
	}
	urls, err := p.pushURLs(ctx, checkout, remote)
	if err != nil {
		return errors.New("required status: the destination remote cannot be read")
	}
	repo := ""
	for _, line := range strings.Split(urls, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		got := publication.Repository(line)
		if got == "" {
			return errors.New("required status: a push destination is not a GitHub repository")
		}
		if repo == "" {
			repo = got
			continue
		}
		if !strings.EqualFold(repo, got) {
			return errors.New("required status: the push destinations are not one GitHub repository")
		}
	}
	if repo == "" {
		return errors.New("required status: the destination remote names no GitHub repository")
	}
	targetURL := ""
	if record.GitHub != nil {
		if !strings.EqualFold(record.Repository, repo) {
			return errors.New("required status: the verification record names another repository")
		}
		targetURL = record.GitHub.URL
		tree, err := p.Git(ctx, checkout, "rev-parse", record.GitHub.RunHead+"^{tree}")
		if err != nil || strings.TrimSpace(tree) != keys.Tree {
			return errors.New("required status: the verification run's tree is not the integration tree")
		}
	}
	gh := p.GitHub
	if gh == nil {
		gh = publication.NewGitHub
	}
	client, err := gh(repo)
	if err != nil {
		return fmt.Errorf("required status: %w", err)
	}
	if _, err := p.Git(ctx, checkout, "push", "--no-follow-tags", remote, head+":refs/heads/ci/required-status/"+head); err != nil {
		return errors.New("required status: the verified head could not be staged on the destination")
	}
	return client.SetCommitStatus(ctx, head, tracking.CommitStatusUpdate{State: "success", Context: p.Context, TargetURL: targetURL, Description: "Verified integration tree"})
}
