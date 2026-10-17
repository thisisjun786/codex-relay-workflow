package executor

import (
	"context"
	"time"
)

type Port struct {
	Adapter Adapter
	Now     func() time.Time
}

func (p Port) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p Port) check(a Admission, c Capability) error {
	if err := ValidateAdmission(a); err != nil {
		return err
	}
	if p.Adapter == nil {
		return errInvalid(CodeCapabilityUnsupported, "adapter", "no adapter is bound")
	}
	if p.Adapter.Kind() != a.Task.Executor.Kind {
		return errInvalid(CodeIdentityMismatch, "adapter", "the adapter serves another executor kind")
	}
	return Require(p.Adapter, a.Task.Executor.CapabilityRevision, c)
}

func (p Port) checkHandle(a Admission, h Handle) error {
	if h.Reference != ReferenceOf(a) {
		return errInvalid(CodeIdentityMismatch, "handle", "the adapter returned a foreign handle")
	}
	return validObservation(h.NativeSession, "handle.native_session")
}

func (p Port) Start(ctx context.Context, a Admission, g Grant) (Handle, error) {
	if err := p.check(a, CapStart); err != nil {
		return Handle{}, err
	}
	if err := ValidateGrant(a, g, p.now()); err != nil {
		return Handle{}, err
	}
	h, err := p.Adapter.Start(ctx, a, g)
	if err != nil {
		return Handle{}, err
	}
	if err := p.checkHandle(a, h); err != nil {
		return Handle{}, err
	}
	return h, nil
}

func (p Port) Inspect(ctx context.Context, a Admission) (Inspection, error) {
	if err := p.check(a, CapInspect); err != nil {
		return Inspection{}, err
	}
	i, err := p.Adapter.Inspect(ctx, ReferenceOf(a))
	if err != nil {
		return Inspection{}, err
	}
	if err := ValidateInspection(i); err != nil {
		return Inspection{}, err
	}
	return i, nil
}

func (p Port) Cancel(ctx context.Context, a Admission, g Grant) (Cancellation, error) {
	if err := p.check(a, CapCancel); err != nil {
		return Cancellation{}, err
	}
	if err := ValidateGrant(a, g, p.now()); err != nil {
		return Cancellation{}, err
	}
	cn, err := p.Adapter.Cancel(ctx, ReferenceOf(a), g)
	if err != nil {
		return Cancellation{}, err
	}
	if err := ValidateCancellation(cn); err != nil {
		return Cancellation{}, err
	}
	return cn, nil
}

func (p Port) Collect(ctx context.Context, a Admission) (Delivery, error) {
	if err := p.check(a, CapCollect); err != nil {
		return Delivery{}, err
	}
	d, err := p.Adapter.Collect(ctx, ReferenceOf(a))
	if err != nil {
		return Delivery{}, err
	}
	if !validArtifactPath(d.ResultPath) {
		return Delivery{}, errInvalid(CodeArtifactInvalid, "delivery.result_path", "unsafe result path "+d.ResultPath)
	}
	return d, nil
}

func (p Port) Resume(ctx context.Context, a Admission, g Grant) (Handle, error) {
	if err := p.check(a, CapResume); err != nil {
		return Handle{}, err
	}
	if err := ValidateGrant(a, g, p.now()); err != nil {
		return Handle{}, err
	}
	h, err := p.Adapter.(Resumer).Resume(ctx, ReferenceOf(a), g)
	if err != nil {
		return Handle{}, err
	}
	if err := p.checkHandle(a, h); err != nil {
		return Handle{}, err
	}
	return h, nil
}

func (p Port) Fork(ctx context.Context, parent, child Admission, g Grant) (Handle, error) {
	if err := p.check(parent, CapFork); err != nil {
		return Handle{}, err
	}
	if err := ValidateAdmission(child); err != nil {
		return Handle{}, err
	}
	if child.Task.Executor != parent.Task.Executor {
		return Handle{}, errInvalid(CodeIdentityMismatch, "executor", "the child is bound to another host or capability")
	}
	if child.Task.Identity.ExecutionID == parent.Task.Identity.ExecutionID {
		return Handle{}, errInvalid(CodeIdentityMismatch, "execution_id", "the fork shares the parent's execution")
	}
	if err := ValidateGrant(child, g, p.now()); err != nil {
		return Handle{}, err
	}
	h, err := p.Adapter.(Forker).Fork(ctx, ReferenceOf(parent), child, g)
	if err != nil {
		return Handle{}, err
	}
	if err := p.checkHandle(child, h); err != nil {
		return Handle{}, err
	}
	return h, nil
}
