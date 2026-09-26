package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func init() {
	cli.Commands = append(cli.Commands,
		cli.Command{Name: "managed-show", Required: []string{"request-id"}, Flags: func(f *flag.FlagSet) { f.String("request-id", "", "") }, Exempt: true, Run: runShow},
		cli.Command{Name: "managed-release", Required: []string{"request-id", "fingerprint", "revision", "reason"}, Flags: func(f *flag.FlagSet) {
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
	var request any
	if row != nil {
		request = row
	}
	return contract.OrderedObject{{Key: "request", Value: request}, {Key: "lastObservation", Value: observation}, {Key: "readable", Value: read.Readable}, {Key: "detail", Value: nullableDetail(read.Detail)}}, nil
}

// decodeOrderedJSON retains the journal's Python insertion order in nested observations.
func decodeOrderedJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return readOrdered(decoder)
}
func readOrdered(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		result := contract.OrderedObject{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := readOrdered(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, contract.Field{Key: key.(string), Value: value})
		}
		_, err = decoder.Token()
		return result, err
	case json.Delim('['):
		result := []any{}
		for decoder.More() {
			value, err := readOrdered(decoder)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		_, err = decoder.Token()
		return result, err
	default:
		return token, nil
	}
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
	revision, _ := args.String("revision")
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		return nil, &cli.UsageError{Detail: "argument --revision: invalid int value: " + store.PythonRepr(revision), Code: 2}
	}
	row, err := (Reservation{Store: s, Now: registry.SystemISO}).Release(ctx, id, fp, n, reason)
	if err != nil {
		return nil, err
	}
	return reservationRecord(row), nil
}
