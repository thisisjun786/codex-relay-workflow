package managed

import (
	"context"
	"flag"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func init() {
	cli.Commands = append(cli.Commands,
		cli.Command{Name: "managed-show", Flags: func(f *flag.FlagSet) { f.String("request-id", "", "") }, Exempt: true, Run: runShow},
		cli.Command{Name: "managed-release", Flags: func(f *flag.FlagSet) {
			f.String("request-id", "", "")
			f.String("fingerprint", "", "")
			f.Int64("revision", 0, "")
			f.String("reason", "", "")
		}, Run: runRelease})
}
func runShow(ctx context.Context, services cli.Services, args cli.Args) (any, error) {
	id, _ := args.String("request-id")
	var row contract.OrderedObject
	var observation any
	read := store.ReadOnlyRows(ctx, services.Selection, "SELECT r.*, (SELECT detail FROM journal WHERE kind='managed_start_observed' AND subject=r.request_id ORDER BY rowid DESC LIMIT 1) AS observation FROM managed_start_requests r WHERE request_id=?", []any{id}, func(scanner store.RowScanner) error {
		values := make([]any, 20)
		pointers := make([]any, 20)
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := scanner.Scan(pointers...); err != nil {
			return err
		}
		keys := []string{"request_id", "issue_key", "request_fingerprint", "fingerprint_version", "workspace", "marker_root", "socket_identity", "create_request_id", "dispatch_request_id", "state", "revision", "child_task_id", "standby_turn_id", "relationship_id", "execution_generation", "receipt_status", "release_reason", "created_at", "updated_at"}
		for i, key := range keys {
			value := values[i]
			if b, ok := value.([]byte); ok {
				value = string(b)
			}
			row = append(row, contract.Field{Key: key, Value: value})
		}
		if values[19] != nil {
			var data []byte
			switch value := values[19].(type) {
			case string:
				data = []byte(value)
			case []byte:
				data = value
			}
			decoded, err := decodeOrderedJSON(data)
			if err != nil {
				return err
			}
			observation = decoded
		}
		return nil
	})
	if read.Raised != nil {
		return nil, read.Raised
	}
	var request any
	if row != nil {
		request = row
	}
	return contract.OrderedObject{{Key: "request", Value: request}, {Key: "lastObservation", Value: observation}, {Key: "readable", Value: read.Readable}, {Key: "detail", Value: nullableDetail(read.Detail)}}, nil
}

// decodeOrderedJSON retains the journal's Python insertion order in nested observations: every
// number a json.Number as spelled, a repeated key kept as a field of its own, and whatever
// follows the value left unread.
func decodeOrderedJSON(data []byte) (any, error) {
	return pyjson.Loads(string(data), pyjson.LoadOptions{Numbers: pyjson.SpelledNumbers, Repeats: true, Trailing: pyjson.TrailingAnything})
}
func nullableDetail(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func runRelease(ctx context.Context, services cli.Services, args cli.Args) (any, error) {
	s, err := store.Open(ctx, services.Selection.DBPath(), services.SocketPath)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	id, _ := args.String("request-id")
	fp, _ := args.String("fingerprint")
	reason, _ := args.String("reason")
	n := args.Integer("revision")
	row, err := (Reservation{Store: s, Now: registry.SystemISO}).Release(ctx, id, fp, n, reason)
	if err != nil {
		return nil, err
	}
	return reservationRecord(row), nil
}
