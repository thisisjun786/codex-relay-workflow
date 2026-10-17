package executor

import (
	"context"
	"testing"
	"time"
)

type fakeAdapter struct {
	kind Kind
	caps Capabilities
}

func (f *fakeAdapter) Kind() Kind                 { return f.kind }
func (f *fakeAdapter) Capabilities() Capabilities { return f.caps }

func (f *fakeAdapter) Start(_ context.Context, a Admission, _ Grant) (Handle, error) {
	return Handle{Reference: Reference{Identity: a.Task.Identity, TaskDigest: a.TaskDigest, Executor: a.Task.Executor},
		NativeSession: Observation{Value: "sess-" + string(f.kind)}}, nil
}

func (f *fakeAdapter) Inspect(_ context.Context, _ Reference) (Inspection, error) {
	return Inspection{State: "running", NativeSession: Observation{UnknownReason: "not yet exposed"}}, nil
}

func (f *fakeAdapter) Cancel(_ context.Context, _ Reference, _ Grant) (Cancellation, error) {
	return Cancellation{Terminated: false}, nil
}

func (f *fakeAdapter) Collect(_ context.Context, _ Reference) (Delivery, error) {
	return Delivery{ResultPath: "result.json"}, nil
}

type fakeResumer struct{ *fakeAdapter }

func (f *fakeResumer) Resume(_ context.Context, r Reference, g Grant) (Handle, error) {
	return Handle{Reference: r}, nil
}

type fakeForker struct{ *fakeAdapter }

func (f *fakeForker) Fork(_ context.Context, r Reference, _ Admission, _ Grant) (Handle, error) {
	return Handle{Reference: r}, nil
}

func TestAdapterPortForEveryProvider(t *testing.T) {
	now := time.Now()
	for _, kind := range []Kind{Codex, Devin, ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			a := mustFreeze(t, testTask(kind))
			f := &fakeAdapter{kind: kind, caps: Capabilities{Revision: "cap-1",
				Supported:   []Capability{CapStart, CapInspect, CapCancel, CapCollect},
				Unsupported: map[Capability]string{CapResume: "the provider has no resume", CapFork: "the provider has no fork"}}}
			if f.Kind() != kind {
				t.Errorf("kind %q", f.Kind())
			}
			g := Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: now.Add(time.Hour)}
			h, err := f.Start(context.Background(), a, g)
			if err != nil {
				t.Fatal(err)
			}
			if h.Reference.TaskDigest != a.TaskDigest || h.Reference.Identity != a.Task.Identity {
				t.Errorf("the handle does not carry the admitted identity: %+v", h.Reference)
			}
			ins, err := f.Inspect(context.Background(), h.Reference)
			if err != nil || ValidateInspection(ins) != nil {
				t.Errorf("inspect: %v %+v", err, ins)
			}
			if ins.NativeSession.UnknownReason == "" {
				t.Error("an unknown session observation was not kept unknown")
			}
			c, err := f.Cancel(context.Background(), h.Reference, g)
			if err != nil || ValidateCancellation(c) != nil {
				t.Errorf("cancel: %v %+v", err, c)
			}
			if c.Terminated {
				t.Error("a refused cancel was reported as terminated")
			}
			d, err := f.Collect(context.Background(), h.Reference)
			if err != nil || d.ResultPath == "" {
				t.Errorf("collect: %v %+v", err, d)
			}
		})
	}
}

func TestRequireCapability(t *testing.T) {
	full := &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1",
		Supported:   []Capability{CapStart, CapInspect, CapCancel, CapCollect},
		Unsupported: map[Capability]string{CapResume: "no resume", CapFork: "no fork"}}}
	for _, c := range []Capability{CapStart, CapInspect, CapCancel, CapCollect} {
		if err := Require(full, "cap-1", c); err != nil {
			t.Errorf("%s is supported: %v", c, err)
		}
	}
	for _, c := range []struct {
		name string
		a    Adapter
		rev  string
		cap  Capability
		want string
	}{
		{"declared unsupported", full, "cap-1", CapResume, CodeCapabilityUnsupported},
		{"declared unsupported fork", full, "cap-1", CapFork, CodeCapabilityUnsupported},
		{"unknown capability", full, "cap-1", "brew", CodeCapabilityUnsupported},
		{"wrong revision", full, "cap-9", CapStart, CodeCapabilityUnsupported},
		{"supported but not implemented", &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1", Supported: []Capability{CapResume}}}, "cap-1", CapResume, CodeCapabilityUnsupported},
		{"supported fork not implemented", &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1", Supported: []Capability{CapFork}}}, "cap-1", CapFork, CodeCapabilityUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := Require(c.a, c.rev, c.cap)
			var ve *ValidationError
			if err == nil || !asValidation(err, &ve) || ve.Code != c.want {
				t.Errorf("err %v, want %s", err, c.want)
			}
		})
	}
	resumer := &fakeResumer{fakeAdapter: &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1", Supported: []Capability{CapStart, CapResume}}}}
	if err := Require(resumer, "cap-1", CapResume); err != nil {
		t.Errorf("an implemented resume: %v", err)
	}
	forker := &fakeForker{fakeAdapter: &fakeAdapter{kind: Codex, caps: Capabilities{Revision: "cap-1", Supported: []Capability{CapStart, CapFork}}}}
	if err := Require(forker, "cap-1", CapFork); err != nil {
		t.Errorf("an implemented fork: %v", err)
	}
}

func TestValidateInspectionAndCancellation(t *testing.T) {
	sess := Observation{Value: "s"}
	for _, c := range []struct {
		name string
		in   Inspection
		bad  bool
	}{
		{"running", Inspection{State: "running", NativeSession: sess}, false},
		{"unknown", Inspection{State: "unknown", NativeSession: Observation{UnknownReason: "gone"}}, false},
		{"terminated with proof", Inspection{State: "terminated", Proof: "pid exit", NativeSession: sess}, false},
		{"not_started with proof", Inspection{State: "not_started", Proof: "never launched", NativeSession: Observation{UnknownReason: "none"}}, false},
		{"terminated without proof", Inspection{State: "terminated", NativeSession: sess}, true},
		{"not_started without proof", Inspection{State: "not_started", NativeSession: sess}, true},
		{"bogus state", Inspection{State: "paused", NativeSession: sess}, true},
		{"empty session", Inspection{State: "running"}, true},
		{"ambiguous session", Inspection{State: "running", NativeSession: Observation{Value: "s", UnknownReason: "u"}}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateInspection(c.in); (err != nil) != c.bad {
				t.Errorf("ValidateInspection: %v", err)
			}
		})
	}
	for _, c := range []struct {
		name string
		cn   Cancellation
		bad  bool
	}{
		{"terminated with proof", Cancellation{Terminated: true, Proof: "group dead"}, false},
		{"terminated without proof", Cancellation{Terminated: true}, true},
		{"still running", Cancellation{Terminated: false}, false},
		{"still running with detail", Cancellation{Terminated: false, Proof: "alive"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := ValidateCancellation(c.cn); (err != nil) != c.bad {
				t.Errorf("ValidateCancellation: %v", err)
			}
		})
	}
}
