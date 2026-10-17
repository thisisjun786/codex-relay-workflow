package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type countAdapter struct {
	kind     Kind
	caps     Capabilities
	calls    map[string]int
	err      error
	delivery string
	grant    Grant
	handle   func(Reference) Handle
}

func newCountAdapter(kind Kind, supported ...Capability) *countAdapter {
	return &countAdapter{kind: kind, calls: map[string]int{},
		caps: Capabilities{Revision: "cap-1", Supported: supported, Unsupported: map[Capability]string{CapResume: "no", CapFork: "no"}}}
}

func (a *countAdapter) Kind() Kind                 { return a.kind }
func (a *countAdapter) Capabilities() Capabilities { return a.caps }

func (a *countAdapter) count(name string, err error) error {
	a.calls[name]++
	return err
}

func (a *countAdapter) refHandle(r Reference) Handle {
	if a.handle != nil {
		return a.handle(r)
	}
	return Handle{Reference: r, NativeSession: Observation{Value: "native-1"}}
}

func (a *countAdapter) Start(_ context.Context, adm Admission, _ Grant) (Handle, error) {
	if err := a.count("start", a.err); err != nil {
		return Handle{}, err
	}
	return a.refHandle(ReferenceOf(adm)), nil
}

func (a *countAdapter) Inspect(_ context.Context, r Reference) (Inspection, error) {
	if err := a.count("inspect", a.err); err != nil {
		return Inspection{}, err
	}
	return Inspection{State: "unknown", NativeSession: Observation{UnknownReason: "not exposed"}}, nil
}

func (a *countAdapter) Cancel(_ context.Context, _ Reference, g Grant) (Cancellation, error) {
	a.grant = g
	if err := a.count("cancel", a.err); err != nil {
		return Cancellation{}, err
	}
	return Cancellation{Terminated: false}, nil
}

func (a *countAdapter) Collect(_ context.Context, _ Reference) (Delivery, error) {
	if err := a.count("collect", a.err); err != nil {
		return Delivery{}, err
	}
	p := "result.json"
	if a.delivery != "" {
		p = a.delivery
	}
	return Delivery{ResultPath: p}, nil
}

func (a *countAdapter) Resume(_ context.Context, r Reference, _ Grant) (Handle, error) {
	if err := a.count("resume", a.err); err != nil {
		return Handle{}, err
	}
	return a.refHandle(r), nil
}

func (a *countAdapter) Fork(_ context.Context, _ Reference, child Admission, _ Grant) (Handle, error) {
	if err := a.count("fork", a.err); err != nil {
		return Handle{}, err
	}
	return a.refHandle(ReferenceOf(child)), nil
}

var _ Adapter = (*countAdapter)(nil)
var _ Resumer = (*countAdapter)(nil)
var _ Forker = (*countAdapter)(nil)

func portGrant() Grant {
	return Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestPortForEveryProvider(t *testing.T) {
	caps := []Capability{CapStart, CapInspect, CapCancel, CapCollect, CapResume, CapFork}
	for _, kind := range []Kind{Codex, Devin, ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			ad := newCountAdapter(kind, caps...)
			p := Port{Adapter: ad}
			a := mustFreeze(t, testTask(kind))
			g := portGrant()
			ctx := context.Background()
			if _, err := p.Start(ctx, a, g); err != nil {
				t.Errorf("start: %v", err)
			}
			if _, err := p.Inspect(ctx, a); err != nil {
				t.Errorf("inspect: %v", err)
			}
			if _, err := p.Cancel(ctx, a, g); err != nil {
				t.Errorf("cancel: %v", err)
			}
			if _, err := p.Collect(ctx, a); err != nil {
				t.Errorf("collect: %v", err)
			}
			if _, err := p.Resume(ctx, a, g); err != nil {
				t.Errorf("resume: %v", err)
			}
			child := testTask(kind)
			child.Identity.ExecutionID = "exec-2"
			child.Identity.AssignmentGeneration = 2
			child.Identity.Attempt = 2
			ca := mustFreeze(t, child)
			cg := g
			cg.ExecutionID, cg.AssignmentGeneration = "exec-2", 2
			if _, err := p.Fork(ctx, a, ca, cg); err != nil {
				t.Errorf("fork: %v", err)
			}
			for _, name := range []string{"start", "inspect", "cancel", "collect", "resume", "fork"} {
				if ad.calls[name] != 1 {
					t.Errorf("%s ran %d times", name, ad.calls[name])
				}
			}
		})
	}
}

func TestPortNeverCallsOnFailedChecks(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	g := portGrant()
	ctx := context.Background()
	wrongKind := newCountAdapter(Devin, CapStart)
	if _, err := (Port{Adapter: wrongKind}).Start(ctx, a, g); err == nil || wrongKind.calls["start"] != 0 {
		t.Error("a foreign kind adapter was called")
	}
	staleCaps := newCountAdapter(Codex)
	staleCaps.caps.Revision = "cap-9"
	if _, err := (Port{Adapter: staleCaps}).Start(ctx, a, g); err == nil || staleCaps.calls["start"] != 0 {
		t.Error("a stale capability adapter was called")
	}
	noStart := newCountAdapter(Codex, CapInspect)
	if _, err := (Port{Adapter: noStart}).Start(ctx, a, g); err == nil || noStart.calls["start"] != 0 {
		t.Error("an unsupported start was emulated")
	}
	bad := a
	bad.TaskDigest = strings.Repeat("9", 64)
	if _, err := (Port{Adapter: newCountAdapter(Codex, CapStart)}).Start(ctx, bad, g); err == nil {
		t.Error("a mutated admission started")
	}
	dead := g
	dead.ExpiresAt = time.Now().Add(-time.Hour)
	ad := newCountAdapter(Codex, CapStart)
	if _, err := (Port{Adapter: ad}).Start(ctx, a, dead); err == nil || ad.calls["start"] != 0 {
		t.Error("an expired grant started")
	}
	declared := &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1", Supported: []Capability{CapResume}}}
	if _, err := (Port{Adapter: declared}).Resume(ctx, a, g); err == nil {
		t.Error("resume ran without a Resumer")
	}
}

func TestPortNoRetryNoRewrite(t *testing.T) {
	a := mustFreeze(t, testTask(Devin))
	g := portGrant()
	ctx := context.Background()
	ad := newCountAdapter(Devin, CapStart, CapInspect, CapCancel, CapCollect)
	ad.err = errors.New("transport gone")
	if _, err := (Port{Adapter: ad}).Start(ctx, a, g); err == nil || ad.calls["start"] != 1 {
		t.Errorf("a transport error was retried: %v %d", err, ad.calls["start"])
	}
	ad.err = nil
	ad.handle = func(r Reference) Handle {
		r.TaskDigest = strings.Repeat("1", 64)
		return Handle{Reference: r, NativeSession: Observation{Value: "s"}}
	}
	if _, err := (Port{Adapter: ad}).Start(ctx, a, g); err == nil {
		t.Error("a handle with a foreign identity was returned")
	}
	ad.handle = func(r Reference) Handle {
		return Handle{Reference: r}
	}
	if _, err := (Port{Adapter: ad}).Start(ctx, a, g); err == nil {
		t.Error("a handle without a session observation was returned")
	}
	ad.handle = nil
	if ins, err := (Port{Adapter: ad}).Inspect(ctx, a); err != nil || ins.State != "unknown" || ins.NativeSession.UnknownReason == "" {
		t.Errorf("an unknown inspection did not stay unknown: %v %+v", err, ins)
	}
	cn, err := (Port{Adapter: ad}).Cancel(ctx, a, g)
	if err != nil || cn.Terminated {
		t.Errorf("a refused cancel became a termination: %v %+v", err, cn)
	}
	for _, path := range []string{"../escape.json", "/tmp/x.json", `a\b.json`, "https://example.invalid/x", ".executor.lock"} {
		evil := newCountAdapter(Devin, CapCollect)
		evil.delivery = path
		if _, err := (Port{Adapter: evil}).Collect(ctx, a); err == nil {
			t.Errorf("a delivery path %q was returned", path)
		}
		if evil.calls["collect"] != 1 {
			t.Errorf("collect did not run once for %q", path)
		}
	}
}

func TestPortCancelCarriesGrant(t *testing.T) {
	a := mustFreeze(t, testTask(Codex))
	ctx := context.Background()
	ad := newCountAdapter(Codex, CapCancel)
	g := portGrant()
	if _, err := (Port{Adapter: ad}).Cancel(ctx, a, g); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := ad.grant
	if got.ExecutionID != g.ExecutionID || got.AssignmentGeneration != g.AssignmentGeneration || got.Owner != g.Owner || got.OwnerEpoch != g.OwnerEpoch || !got.ExpiresAt.Equal(g.ExpiresAt) {
		t.Fatalf("the adapter saw grant %+v, want %+v", got, g)
	}
	renewed := g
	renewed.Owner, renewed.OwnerEpoch, renewed.ExpiresAt = "owner-2", 2, g.ExpiresAt.Add(time.Hour)
	if _, err := (Port{Adapter: ad}).Cancel(ctx, a, renewed); err != nil {
		t.Fatalf("renewed cancel: %v", err)
	}
	if ad.grant.Owner != "owner-2" || ad.grant.OwnerEpoch != 2 {
		t.Errorf("the adapter saw %+v, want the renewed grant", ad.grant)
	}
	dead := renewed
	dead.ExpiresAt = time.Now().Add(-time.Hour)
	calls := ad.calls["cancel"]
	if _, err := (Port{Adapter: ad}).Cancel(ctx, a, dead); err == nil || ad.calls["cancel"] != calls {
		t.Error("an expired grant reached the adapter")
	}
	cn, err := (Port{Adapter: ad}).Cancel(ctx, a, renewed)
	if err != nil || cn.Terminated {
		t.Errorf("a refused cancel became a termination: %v %+v", err, cn)
	}
	if err := ValidateAdmission(a); err != nil {
		t.Errorf("the cancel touched the task digest: %v", err)
	}
}
