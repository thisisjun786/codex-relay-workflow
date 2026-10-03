package bridge

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// Subscription watches exist only on the real transport; scripted RPC doubles
// carry no connection subscription. Tests of release use Client over fakehost.
type subscriptionScopeKey struct{}
type subscriptionScope struct{ watch *appserver.TurnWatch }

func (b *Bridge) watchSubscription(ctx context.Context, thread string, created ...bool) (*appserver.TurnWatch, error) {
	if client, ok := b.RPC.(*appserver.Client); ok {
		var watch *appserver.TurnWatch
		var err error
		if len(created) > 0 && created[0] {
			watch, err = client.WatchCreatedTurn(context.WithoutCancel(ctx), thread)
		} else {
			watch, err = client.WatchTurn(ctx, thread)
		}
		if scope, ok := ctx.Value(subscriptionScopeKey{}).(*subscriptionScope); ok {
			scope.watch = watch
		}
		return watch, err
	}
	return nil, nil
}

func finishSubscription(watch *appserver.TurnWatch, receipt ledger.Receipt, created bool) {
	if watch == nil {
		return
	}
	turn, _ := receipt["turnId"].(string)
	watch.Finish(turn, created && turn == "")
}
