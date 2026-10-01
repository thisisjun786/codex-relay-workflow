package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
)

// Open uses only explicit selectors. Environment resolution belongs to the CLI;
// a pinned managed ledger never follows a later change in the process environment.
func Open(socket, directory string, options Options) (*Adapter, error) {
	ledgerOptions := ledger.Options{Encode: encodeReceipt}
	if options.Clock != nil {
		ledgerOptions.Now = options.Clock.Now
	}
	canonical, l, err := ledger.EndpointWithOptions(socket, directory, ledgerOptions)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(canonical))
	path := filepath.Join(directory, fmt.Sprintf("operations-%x.sqlite3", digest[:8]))
	options.Ledger = l
	options.LedgerPath = path
	if options.RPC == nil {
		options.RPC = appserver.New(canonical, appserver.DefaultBounds)
	}
	if options.Policy == nil {
		options.Policy = execution.Policy{}
	}
	a := New(options)
	identity, err := LedgerIdentity(path)
	if err != nil {
		return nil, errors.Join(err, a.Close())
	}
	a.identity = identity
	return a, nil
}
func LedgerIdentity(path string) (map[string]any, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, &HostUnavailable{"ledger identity is unknown: " + err.Error()}
	}
	info, err := os.Stat(real)
	if err != nil {
		return nil, &HostUnavailable{"ledger identity is unknown: " + err.Error()}
	}
	if !info.Mode().IsRegular() {
		return nil, &HostUnavailable{"ledger identity is unknown: " + real + " is not a regular file"}
	}
	stat := info.Sys().(*syscall.Stat_t)
	return map[string]any{"path": path, "realPath": real, "device": int64(stat.Dev), "inode": int64(stat.Ino)}, nil
}
func (a *Adapter) LedgerIdentityRecord(context.Context) (map[string]any, error) {
	a.identityMu.Lock()
	defer a.identityMu.Unlock()
	if a.identity == nil {
		return nil, nil
	}
	out := map[string]any{}
	for k, v := range a.identity {
		out[k] = v
	}
	return out, nil
}
func (a *Adapter) RequireLedger(ctx context.Context, expected map[string]any) error {
	captured, err := a.LedgerIdentityRecord(ctx)
	if err != nil {
		return err
	}
	if captured == nil {
		return &HostUnavailable{"this adapter has no ledger to revalidate"}
	}
	observed, err := LedgerIdentity(text(captured["realPath"]))
	if err != nil {
		return &HostUnavailable{"ledger identity changed or is unknown; refusing before mutation"}
	}
	for _, key := range []string{"realPath", "device", "inode"} {
		if key != "realPath" {
			switch expected[key].(type) {
			case int, int64, uint64, float64, json.Number:
			default:
				return &HostUnavailable{"ledger identity changed or is unknown; refusing before mutation"}
			}
		}
		if expected[key] == nil || fmt.Sprint(expected[key]) != fmt.Sprint(observed[key]) {
			return &HostUnavailable{"ledger identity changed or is unknown; refusing before mutation"}
		}
	}
	return nil
}

// admitCreate runs the preconditions of a create and admits it. Create and Managed.CreateThread
// share it, so the receipt Managed reads back is read inside the admission of the create.
func (a *Adapter) admitCreate(ctx context.Context) (context.Context, func(), error) {
	if a.bridge == nil {
		return nil, nil, &HostUnavailable{"this adapter was built read-only, with no transport to create a thread on"}
	}
	if a.identity != nil {
		if err := a.RequireLedger(ctx, a.identity); err != nil {
			return nil, nil, err
		}
	}
	run, release, ok := a.transport.admit(ctx)
	if !ok {
		return nil, nil, errors.New("the relay transport is shutting down; nothing was sent")
	}
	return run, release, nil
}
func (a *Adapter) Create(ctx context.Context, in bridge.CreateThread) (ledger.Receipt, error) {
	run, release, err := a.admitCreate(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return a.bridge.CreateThread(run, in)
}

// reads is the bridge's read seam over one admitted context. A scan makes several host calls and
// takes them all under the admission its caller holds, so Close refuses or finishes it whole.
func (a *Adapter) reads(ctx context.Context) delivery.BridgeReads {
	return delivery.BridgeReads{Page: a.page, Call: func(method string, params delivery.Obj) (delivery.Obj, error) {
		return a.callOrdered(ctx, method, plain(params).(map[string]any))
	}}
}
func (a *Adapter) FindDispatchedTurn(thread, turn string, sentAt float64) (delivery.TurnPresence, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return delivery.TurnPresence{}, err
	}
	defer release()
	return a.reads(ctx).FindDispatchedTurn(thread, turn, sentAt)
}
func (a *Adapter) FindTokenSince(thread, token string, older []string, limit int) (delivery.TokenScan, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return delivery.TokenScan{}, err
	}
	defer release()
	return a.reads(ctx).FindTokenSince(thread, token, older, limit)
}
func (a *Adapter) FindTokenInTurn(thread, token, turn string, limit int) (delivery.TokenScan, error) {
	ctx, release, err := a.admitRead(context.Background())
	if err != nil {
		return delivery.TokenScan{}, err
	}
	defer release()
	return a.reads(ctx).FindTokenInTurn(thread, token, turn, limit)
}

var _ delivery.Adapter = (*Adapter)(nil)

// Managed bridges the context-bearing managed interface and the older delivery
// interface without changing either contract. Both retain the same client and ledger.
type Managed struct{ *Adapter }

func (a Managed) CreateThread(ctx context.Context, in managed.CreateThreadRequest) (map[string]any, error) {
	run, release, err := a.admitCreate(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	_, err = a.bridge.CreateThread(run, bridge.CreateThread{RequestID: in.RequestID, CWD: in.CWD, Prompt: in.Prompt, Title: in.Title, Sandbox: in.Sandbox, Model: in.Model, Effort: in.ReasoningEffort, Role: in.Role, Roots: in.RuntimeWorkspaceRoots, Policy: in.ExpectedSandboxPolicy})
	if err != nil {
		return nil, err
	}
	// Managed consumes decoded JSON lists, while Bridge.call uses []string for
	// runtime roots internally. Read the actual retained receipt at this boundary.
	// It is read inside the create's own admission: Close cannot end the ledger between the two.
	return a.ledger.Get(run, in.RequestID)
}
func (a Managed) GetOperation(ctx context.Context, id string) (map[string]any, error) {
	ctx, release, err := a.admitRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r, err := a.ledger.Get(ctx, id)
	if errors.Is(err, ledger.ErrUnknown) {
		return nil, nil
	}
	return r, err
}
func (a Managed) SendMessage(ctx context.Context, in managed.SendRequest) (map[string]any, error) {
	r, err := a.Send(ctx, in.RequestID, in.ThreadID, in.Message, &delivery.TaskSettings{Data: ordered(in.Settings).(contract.OrderedObject)}, in.BeforeStart, in.GuardRPCRequests)
	if err != nil {
		return nil, err
	}
	return plain(r).(map[string]any), nil
}
func (a Managed) ReadTurn(_ context.Context, thread, turn string) (*managed.Turn, error) {
	r, err := a.Adapter.ReadTurn(thread, turn)
	if err != nil || r == nil {
		return nil, err
	}
	status, _ := r.Status.(string)
	return &managed.Turn{ID: r.TurnID, Status: status}, nil
}

var _ managed.Adapter = Managed{}
