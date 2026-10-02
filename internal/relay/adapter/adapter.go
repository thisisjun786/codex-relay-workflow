// Package adapter connects the relay to the in-process thread bridge.
package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const Page = 50
const MaxPagesPerCheck = 4

type HostUnavailable struct{ Message string }

func (e *HostUnavailable) Error() string               { return e.Message }
func (e *HostUnavailable) PythonExceptionKind() string { return "HostUnavailable" }

// Options takes state paths from the caller, never from the host environment.
type Options struct {
	RPC         bridge.RPC
	Ledger      *ledger.Ledger
	LedgerPath  string
	Store       *store.Store
	Clock       delivery.Clock
	Page        int
	Policy      bridge.ExecutionPolicy
	Timeout     time.Duration
	CallerSlack time.Duration
	Drain       time.Duration
}

// Adapter shares one RPC client and one bridge ledger between concurrent callers.
// Reads stay available while a mutation is in flight for another recipient.
type Adapter struct {
	rpc                         bridge.RPC
	ledger                      *ledger.Ledger
	bridge                      *bridge.Bridge
	store                       *store.Store
	clock                       delivery.Clock
	page                        int
	transport                   *transport
	identityMu                  sync.Mutex
	identity                    map[string]any
	timeout, callerSlack, drain time.Duration
}

func New(options Options) *Adapter {
	page := options.Page
	if page == 0 {
		page = Page
	}
	a := &Adapter{rpc: options.RPC, ledger: options.Ledger, store: options.Store, clock: options.Clock, page: page, timeout: options.Timeout, callerSlack: options.CallerSlack, drain: options.Drain}
	if a.timeout == 0 {
		a.timeout = 20 * time.Second
	}
	if a.callerSlack == 0 {
		a.callerSlack = 10 * time.Second
	}
	if a.drain == 0 {
		a.drain = 5 * time.Second
	}
	if a.ledger != nil {
		if options.Policy == nil {
			options.Policy = execution.Policy{}
		}
		a.bridge = bridge.New(a.rpc, a.ledger, options.Policy)
		a.transport = newTransport(a)
	}
	return a
}

// HostCall is one read of the host. It enters the transport's admission like every other read:
// refused with ErrTransportClosing once Close began, unless ctx belongs to work the transport
// already admitted, and cancelled when Close spends its drain budget.
func (a *Adapter) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	ctx, release, err := a.admitRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return a.hostCall(ctx, method, params)
}

// hostCall is the host read without admission, for work that is already admitted.
func (a *Adapter) hostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	raw, err := a.rpc.Call(ctx, method, params)
	if err != nil {
		return nil, pythonConnectionError(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var result any
	if err := dec.Decode(&result); err != nil {
		return nil, err
	}
	if m, ok := result.(map[string]any); ok {
		return m, nil
	}
	return nil, attributeError(result, "get")
}

func pythonConnectionError(err error) error {
	for _, candidate := range []struct {
		err     error
		kind    string
		message string
	}{
		{syscall.ENOENT, "FileNotFoundError", "[Errno 2] No such file or directory"},
		{syscall.ECONNREFUSED, "ConnectionRefusedError", "[Errno 111] Connection refused"},
		{syscall.EACCES, "PermissionError", "[Errno 13] Permission denied"},
	} {
		if errors.Is(err, candidate.err) {
			return &delivery.HostError{Kind: candidate.kind, Message: candidate.message}
		}
	}
	return err
}

func object(value any) (map[string]any, error) {
	if !pyvalue.Truthy(value) {
		return map[string]any{}, nil
	}
	if m, ok := value.(map[string]any); ok {
		return m, nil
	}
	return nil, attributeError(value, "get")
}

func rawEntries(page map[string]any) ([]any, error) {
	value, exists := page["data"]
	if !exists {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		switch v := value.(type) {
		case string:
			if v == "" {
				return nil, nil
			}
			return nil, attributeError(v[:1], "get")
		case map[string]any:
			if len(v) == 0 {
				return nil, nil
			}
			return nil, attributeError("", "get")
		}
		return nil, &pythonError{"TypeError", fmt.Sprintf("'%s' object is not iterable", pyvalue.TypeName(value))}
	}
	return items, nil
}

func (a *Adapter) ReadThread(thread string) (delivery.ThreadFacts, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return delivery.ThreadFacts{}, err
	}
	defer release()
	r, err := a.hostCall(ctx, "thread/read", map[string]any{"threadId": thread})
	if err != nil {
		return delivery.ThreadFacts{}, err
	}
	th, err := object(r["thread"])
	if err != nil {
		return delivery.ThreadFacts{}, err
	}
	status, err := object(th["status"])
	if err != nil {
		return delivery.ThreadFacts{}, err
	}
	var runtime any = "unknown"
	if v, exists := status["type"]; exists {
		runtime = v
	}
	return delivery.ThreadFacts{RuntimeStatus: runtime, CanAcceptInput: th["canAcceptDirectInput"]}, nil
}
func (a *Adapter) ReadGoalStatus(thread string) (any, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := a.hostCall(ctx, "thread/goal/get", map[string]any{"threadId": thread})
	if err != nil {
		return nil, err
	}
	goal, _ := r["goal"].(map[string]any)
	return goal["status"], nil
}
func (a *Adapter) ListTurnIDs(thread string, limit int) ([]any, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := a.hostCall(ctx, "thread/turns/list", map[string]any{"threadId": thread, "limit": limit, "itemsView": "summary"})
	if err != nil {
		return nil, err
	}
	rows, err := rawEntries(r)
	if err != nil {
		return nil, err
	}
	ids := make([]any, len(rows))
	for i, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return nil, &pythonError{"TypeError", "'" + pyvalue.TypeName(value) + "' object is not subscriptable"}
		}
		id, exists := row["id"]
		if !exists {
			return nil, &pythonError{"KeyError", "'id'"}
		}
		ids[i] = id
	}
	return ids, nil
}
func turnInfo(row map[string]any) (*delivery.TurnInfo, error) {
	id, ok := row["id"].(string)
	if !ok {
		return nil, &HostUnavailable{"host turn has no string id"}
	}
	status := any("unknown")
	if v, exists := row["status"]; exists {
		status = v
	}
	return &delivery.TurnInfo{TurnID: id, Status: status, StartedAt: row["startedAt"]}, nil
}
func (a *Adapter) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	var cursor any
	for range MaxPagesPerCheck {
		params := map[string]any{"threadId": thread, "limit": a.page, "itemsView": "summary"}
		if pyvalue.Truthy(cursor) {
			params["cursor"] = cursor
		}
		r, err := a.hostCall(ctx, "thread/turns/list", params)
		if err != nil {
			return nil, err
		}
		rows, err := rawEntries(r)
		if err != nil {
			return nil, err
		}
		for _, value := range rows {
			row, ok := value.(map[string]any)
			if !ok {
				return nil, attributeError(value, "get")
			}
			if row["id"] == turn {
				return turnInfo(row)
			}
		}
		cursor = r["nextCursor"]
		if !pyvalue.Truthy(cursor) {
			return nil, nil
		}
	}
	return nil, &HostUnavailable{fmt.Sprintf("turn '%s' not found within %d pages; the listing was not exhausted, so this is not evidence of absence", turn, MaxPagesPerCheck)}
}
func (a *Adapter) IsArchived(thread string, cwd any) (*bool, error) {
	rpc, release, err := a.admitRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	checks := []struct {
		name    string
		filters map[string]any
	}{}
	if path, ok := cwd.(string); ok && path != "" {
		checks = append(checks, struct {
			name    string
			filters map[string]any
		}{"unarchived_cwd", map[string]any{"archived": false, "cwd": path}})
	}
	checks = append(checks,
		struct {
			name    string
			filters map[string]any
		}{"archived", map[string]any{"archived": true}},
		struct {
			name    string
			filters map[string]any
		}{"archived:exec", map[string]any{"archived": true, "sourceKinds": []string{"exec"}}},
		struct {
			name    string
			filters map[string]any
		}{"unarchived_all:exec", map[string]any{"archived": false, "sourceKinds": []string{"exec"}}},
		struct {
			name    string
			filters map[string]any
		}{"unarchived_all", map[string]any{"archived": false}})
	for _, check := range checks {
		found, err := a.scanListing(rpc, thread, check.name, check.filters)
		if err != nil {
			return nil, err
		}
		if found {
			v := check.filters["archived"].(bool)
			return &v, nil
		}
	}
	return nil, nil
}

// scanListing takes the admitted context for its host calls only. What it persists goes to the
// relay's own store, which Close does not own, so that keeps a context that Close cannot cancel.
func (a *Adapter) scanListing(rpc context.Context, thread, listing string, filters map[string]any) (bool, error) {
	ctx := context.Background()
	var cursor any
	if a.store != nil {
		saved, err := a.store.DiscoveryCursor(ctx, thread, listing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if !saved.Exhausted {
			cursor = saved.Cursor.String
		}
	}
	scanned := 0
	save := func(exhausted bool) error {
		if a.store == nil || a.clock == nil {
			return nil
		}
		var saved sql.NullString
		if pyvalue.Truthy(cursor) {
			value, err := a.cursorString(ctx, cursor)
			if err != nil {
				return err
			}
			saved = sql.NullString{String: value, Valid: true}
		}
		return a.store.RecordDiscoveryCursor(ctx, store.DiscoveryCursor{TaskID: thread, Listing: listing, Cursor: saved, Exhausted: exhausted, Scanned: int64(scanned), UpdatedAt: a.clock.ISO()})
	}
	for range MaxPagesPerCheck {
		params := map[string]any{"limit": a.page, "useStateDbOnly": true}
		for k, v := range filters {
			params[k] = v
		}
		if pyvalue.Truthy(cursor) {
			params["cursor"] = cursor
		}
		page, err := a.hostCall(rpc, "thread/list", params)
		if err != nil {
			var shape *pythonError
			if errors.As(err, &shape) {
				return false, err
			}
			return false, save(false)
		} // Python records the cursor and answers unknown on a failed listing call.
		rows, err := rawEntries(page)
		if err != nil {
			return false, err
		}
		for _, value := range rows {
			row, ok := value.(map[string]any)
			if !ok {
				return false, attributeError(value, "get")
			}
			scanned++
			if row["id"] == thread {
				cursor = ""
				return true, save(true)
			}
		}
		cursor = page["nextCursor"]
		if !pyvalue.Truthy(cursor) {
			return false, save(true)
		}
	}
	return false, save(false)
}

// ordered converts decoded JSON without changing its values. Receipt byte ordering is supplied
// by the relay's OrderedObject boundary, not encoding/json's map encoder.
func ordered(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slicesSort(keys)
		o := make(contract.OrderedObject, 0, len(keys))
		for _, k := range keys {
			o = append(o, contract.Field{Key: k, Value: ordered(x[k])})
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = ordered(value)
		}
		return out
	default:
		return v
	}
}
func (a *Adapter) GetOperation(id string) (delivery.Obj, error) {
	if a.ledger == nil {
		return nil, nil
	}
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	_, err = a.ledger.Get(ctx, id)
	if errors.Is(err, ledger.ErrUnknown) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a.receipt(ctx, id, false)
}

func pythonJSONNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		if strings.ContainsAny(string(v), ".eE") {
			if number, err := v.Float64(); err == nil {
				return number
			}
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = pythonJSONNumbers(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = pythonJSONNumbers(item)
		}
		return out
	}
	return value
}

func itemText(value any) (string, error) {
	entry, isObject := value.(map[string]any)
	if !isObject {
		return dumps(ordered(pythonJSONNumbers(value)), true), nil
	}
	if item, ok := entry["item"].(map[string]any); ok {
		for _, key := range []string{"text", "preview", "summary", "aggregatedOutput"} {
			if s, ok := item[key].(string); ok {
				return s, nil
			}
		}
		return dumps(ordered(pythonJSONNumbers(item)), true), nil
	}
	return dumps(ordered(pythonJSONNumbers(entry)), true), nil
}
func itemKind(value any) string {
	entry, _ := value.(map[string]any)
	item, _ := entry["item"].(map[string]any)
	return pyjson.Text(item["type"])
}
func (a *Adapter) FindToken(thread, token string, limit int, messageOnly bool) (delivery.TokenScan, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return delivery.TokenScan{}, err
	}
	defer release()
	var cursor any
	scanned := 0
	for scanned < limit {
		params := map[string]any{"threadId": thread, "sortDirection": "desc", "limit": min(a.page, limit-scanned)}
		if pyvalue.Truthy(cursor) {
			params["cursor"] = cursor
		}
		page, err := a.hostCall(ctx, "thread/items/list", params)
		if err != nil {
			return delivery.TokenScan{}, err
		}
		rows, err := rawEntries(page)
		if err != nil {
			return delivery.TokenScan{}, err
		}
		for _, row := range rows {
			scanned++
			if kind := itemKind(row); messageOnly && kind != "" && kind != "userMessage" {
				continue
			}
			body, err := itemText(row)
			if err != nil {
				return delivery.TokenScan{}, err
			}
			if strings.Contains(body, token) {
				var turn any
				if entry, ok := row.(map[string]any); ok {
					turn = entry["turnId"]
				}
				return delivery.TokenScan{Found: true, TurnID: turn, Scanned: scanned}, nil
			}
		}
		cursor = page["nextCursor"]
		if !pyvalue.Truthy(cursor) {
			return delivery.TokenScan{Exhausted: true, Scanned: scanned}, nil
		}
		if len(rows) == 0 {
			break
		}
	}
	return delivery.TokenScan{Scanned: scanned}, nil
}
func (a *Adapter) RecipientFingerprint(thread string) (string, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return "", err
	}
	defer release()
	page, err := a.hostCall(ctx, "thread/items/list", map[string]any{"threadId": thread, "sortDirection": "desc", "limit": 8})
	if err != nil {
		return "", err
	}
	rows, err := rawEntries(page)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	for _, value := range rows {
		row, ok := value.(map[string]any)
		if !ok {
			return "", attributeError(value, "get")
		}
		body, err := itemText(row)
		if err != nil {
			return "", err
		}
		item, _ := row["item"].(map[string]any)
		id, ok := item["id"].(string)
		if !ok {
			id = pyvalue.Str(row["id"])
		}
		turn := pyvalue.Str(row["turnId"])
		fmt.Fprintf(digest, "%s:%s:%x|", turn, id, sha256.Sum256([]byte(body)))
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), nil
}
