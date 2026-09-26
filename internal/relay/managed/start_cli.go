package managed

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func init() {
	cli.Commands = append(cli.Commands, cli.Command{Name: "managed-start", Required: []string{"request", "marker-root"}, Flags: func(f *flag.FlagSet) { f.String("request", "", ""); f.String("marker-root", "", "") }, Exempt: true, Run: runStart})
}

// runStart performs input validation before acquiring the store. The production
// host adapter is todo 28: never authorize a host effect without its ledger.
func runStart(_ context.Context, services cli.Services, args cli.Args) (any, error) {
	if services.SocketPath == "" || services.Selection.Source != "flag" {
		return nil, &cli.UsageError{Detail: "managed-start requires explicit --state and --socket", Code: contract.ExitUsage}
	}
	input, _ := args.String("request")
	var raw []byte
	if strings.HasPrefix(input, "@") {
		file, err := os.Open(input[1:])
		if err != nil {
			return nil, &cli.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
		}
		defer file.Close()
		raw, err = io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
		if err != nil {
			return nil, &cli.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
		}
	} else {
		raw = []byte(input)
	}
	request, err := ParseRequest(raw)
	if err != nil {
		return nil, &cli.UsageError{Detail: err.Error(), Code: contract.ExitUsage}
	}
	// A missing store cannot have the worker policy receipt that preflight requires.
	// Refuse before constructing an adapter or creating the absent store.
	if _, err := os.Stat(services.Selection.DBPath()); os.IsNotExist(err) {
		return nil, &cli.PayloadExit{Payload: contract.OrderedObject{
			{Key: "schema", Value: Schema}, {Key: "state", Value: "refused"},
			{Key: "stage", Value: "preflight"}, {Key: "requestId", Value: request["requestId"]},
			{Key: "reason", Value: "worker_policy_unreadable"},
		}, Code: contract.ExitRefused}
	}
	// The bridge transport and observed ledger are todo 28's dependency. Do not
	// open (or create) a relay store before that boundary can be authenticated.
	if _, err := store.CanonicalSocket(services.SocketPath); err != nil {
		return nil, &cli.HostError{Class: "HostUnavailable", Detail: err.Error()}
	}
	return nil, &cli.HostError{Class: "HostUnavailable", Detail: "the relay host adapter (bridge_adapter.py) is not ported to Go yet (todo 28)"}
}
