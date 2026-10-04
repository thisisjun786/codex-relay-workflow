package childcleanup

import (
	"context"
	"fmt"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// ConfigureSubscriptions is executable composition; bridge libraries import no relay.
func ConfigureSubscriptions(client *appserver.Client) {
	client.ConfigureSubscriptions(func(ctx context.Context, root string) (bool, error) {
		report, err := Clean(ctx, client, root, Options{DescendantsOnly: true})
		if err != nil || report.Complete() {
			return report.Complete(), err
		}
		if slices.ContainsFunc(report.Items, func(i Item) bool { return i.Outcome == OutcomeHeldActive }) {
			return false, nil
		}
		return false, fmt.Errorf("%w: items=%v unresolved=%v", appserver.ErrDescendantsUnproved, report.Items, report.Unresolved)
	})
}
