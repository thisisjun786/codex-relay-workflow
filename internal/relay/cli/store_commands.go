package cli

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// openStore is services.store: the Store constructor, which creates and migrates the database
// exactly as Python's Store.__init__ does. The caller closes it.
func openStore(ctx context.Context, services dispatch.Services) (*store.Store, error) {
	return store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
}

var storeIdentityCommand = dispatch.Command{
	Name: "store-identity", ReadOnly: true,
	Run: func(ctx context.Context, services dispatch.Services, _ dispatch.Args) (any, error) {
		opened, err := openStore(ctx, services)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		loc, err := opened.Locate(ctx)
		if err != nil {
			return nil, err
		}
		return contract.OrderedObject{
			{Key: "stateSelection", Value: selectionRecord(services.Selection)},
			{Key: "store", Value: locationRecord(loc, true)},
		}, nil
	},
}

var storeChallengeCommand = dispatch.Command{
	Name: "store-challenge",
	// A challenge read (--read) is read-only; a write is not.
	ReadOnlyWhen: func(args dispatch.Args) bool { return args.Given("read") },
	Run: func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
		read, _ := args.String("read")
		if !args.Bool("write") && read == "" {
			return nil, &dispatch.UsageError{Detail: "store-challenge needs --write or --read <nonce>", Code: contract.ExitUsage}
		}
		opened, err := openStore(ctx, services)
		if err != nil {
			return nil, err
		}
		defer opened.Close()
		if args.Bool("write") {
			actor, _ := args.String("actor")
			if actor == "" {
				actor = "cli"
			}
			written, err := opened.WriteChallengeFor(ctx, actor)
			if err != nil {
				return nil, err
			}
			return contract.OrderedObject{
				{Key: "nonce", Value: written.Nonce}, {Key: "writtenBy", Value: written.WrittenBy},
				{Key: "writtenAt", Value: written.WrittenAt},
			}, nil
		}
		reading, err := opened.ReadChallenge(ctx, read)
		if err != nil {
			return nil, err
		}
		if !reading.Found {
			return contract.OrderedObject{
				{Key: "nonce", Value: read}, {Key: "found", Value: false},
				{Key: "writtenBy", Value: nil}, {Key: "writtenAt", Value: nil},
			}, nil
		}
		return contract.OrderedObject{
			{Key: "nonce", Value: read}, {Key: "found", Value: true},
			{Key: "writtenBy", Value: reading.WrittenBy}, {Key: "writtenAt", Value: reading.WrittenAt},
		}, nil
	},
}
