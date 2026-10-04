package childcleanup

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// ConfigureSubscriptions is executable composition; bridge libraries import no relay.
func ConfigureSubscriptions(client *appserver.Client) {
	client.ConfigureSubscriptions(func(ctx context.Context, root string) (bool, error) {
		report, err := Clean(ctx, client, root, Options{DescendantsOnly: true})
		return report.Complete(), err
	})
}
