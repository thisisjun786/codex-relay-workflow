package integrate

import (
	"context"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/publication"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

var integrationStatusGit = func(ctx context.Context, checkout string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", checkout}, args...)...)
	cmd.Env = gitprobe.Sanitize(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

var integrationStatusGitHub = publication.NewGitHub

func integrationStatusPush(sched *dagsched.Scheduler) (dagsched.BeforeIntegrationPush, error) {
	settings, err := crwconfig.LoadIntegration(os.Getenv, "")
	if err != nil || settings.RequiredStatus == "" {
		return nil, err
	}
	publisher := integrator.RequiredStatusPublisher{Context: settings.RequiredStatus, Git: integrationStatusGit, GitHub: integrationStatusGitHub}
	return func(ctx context.Context, checkout, remote, remoteRef, integrationRef, local string) error {
		raw, base, err := sched.VerifiedMoveRecord(ctx, integrationRef, local)
		if err != nil {
			return err
		}
		return publisher.Publish(ctx, checkout, remote, remoteRef, local, base, raw)
	}, nil
}
