package scenario

import (
	"path/filepath"
	"strings"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/delivery"
)

func TestCRW1609DeliveryReaderIgnoresForeignGitEnvironment(t *testing.T) {
	l := NewLab(t, oneNode()...)
	remote := filepath.Join(t.TempDir(), "origin.git")
	l.Git("clone", "--bare", l.Repo, remote)
	l.Git("remote", "add", "origin", remote)
	foreign := filepath.Join(t.TempDir(), "foreign")
	l.Git("clone", remote, foreign)
	l.GitIn(foreign, "remote", "set-url", "origin", l.Repo)
	branch := strings.TrimPrefix(IntegrationRef, "refs/heads/")
	l.Git("checkout", "-B", branch)
	l.Git("commit", "--allow-empty", "-m", "not pushed to origin")
	closedHead := l.Tip()
	reader := flowcli.DefaultDeliveryReader()
	before, err := reader.Remote(t.Context(), l.Repo, branch)
	if err != nil || before != l.Base {
		t.Fatalf("baseline remote %s, %v; want %s", before, err, l.Base)
	}
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Run("remote does not certify an unpublished head", func(t *testing.T) {
		criteria := []delivery.Criterion{{ID: "c1", Checks: []delivery.Check{{Kind: delivery.RemotePush}}}}
		observations := delivery.Observe(t.Context(), criteria,
			delivery.Target{Checkout: l.Repo, Branch: branch, Head: closedHead}, *reader)
		if delivery.State(observations) != delivery.Pending {
			t.Fatalf("unpublished head %s must remain pending against origin %s: %+v", closedHead, before, observations)
		}
	})
	t.Run("ancestry is read from the intended checkout", func(t *testing.T) {
		contains, err := reader.Ancestor(t.Context(), l.Repo, l.Base, closedHead)
		if err != nil || !contains {
			t.Fatalf("target checkout contains %s after %s: contains=%t, err=%v", closedHead, l.Base, contains, err)
		}
	})
}
