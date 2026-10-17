package executor

import (
	"context"
	"slices"
)

type Capability string

const (
	CapStart   Capability = "start"
	CapInspect Capability = "inspect"
	CapCancel  Capability = "cancel"
	CapCollect Capability = "collect"
	CapResume  Capability = "resume"
	CapFork    Capability = "fork"
)

type Capabilities struct {
	Revision    string                `json:"revision"`
	Supported   []Capability          `json:"supported"`
	Unsupported map[Capability]string `json:"unsupported"`
}

type Reference struct {
	Identity   Identity `json:"identity"`
	TaskDigest string   `json:"task_digest"`
	Executor   Binding  `json:"executor"`
}

type Handle struct {
	Reference     Reference   `json:"reference"`
	NativeSession Observation `json:"native_session"`
}

type Inspection struct {
	State         string      `json:"state"`
	Proof         string      `json:"proof"`
	NativeSession Observation `json:"native_session"`
}

type Cancellation struct {
	Terminated bool   `json:"terminated"`
	Proof      string `json:"proof"`
}

type Delivery struct {
	ResultPath string `json:"result_path"`
}

type Adapter interface {
	Kind() Kind
	Capabilities() Capabilities
	Start(context.Context, Admission, Grant) (Handle, error)
	Inspect(context.Context, Reference) (Inspection, error)
	Cancel(context.Context, Reference, Grant) (Cancellation, error)
	Collect(context.Context, Reference) (Delivery, error)
}

type Resumer interface {
	Resume(context.Context, Reference, Grant) (Handle, error)
}

type Forker interface {
	Fork(context.Context, Reference, Admission, Grant) (Handle, error)
}

func Require(a Adapter, revision string, c Capability) error {
	unsupported := func(detail string) error {
		return &ValidationError{Code: CodeCapabilityUnsupported, Field: "capability", Detail: detail}
	}
	switch c {
	case CapStart, CapInspect, CapCancel, CapCollect, CapResume, CapFork:
	default:
		return unsupported("unknown capability " + string(c))
	}
	if a == nil {
		return unsupported("no adapter")
	}
	caps := a.Capabilities()
	if caps.Revision != revision {
		return unsupported("capability revision " + caps.Revision + " is not the required " + revision)
	}
	if !slices.Contains(caps.Supported, c) {
		return unsupported(string(c) + " is not supported: " + caps.Unsupported[c])
	}
	switch c {
	case CapResume:
		if _, ok := a.(Resumer); !ok {
			return unsupported("resume is declared but the adapter does not implement Resumer")
		}
	case CapFork:
		if _, ok := a.(Forker); !ok {
			return unsupported("fork is declared but the adapter does not implement Forker")
		}
	}
	return nil
}

func ValidateInspection(i Inspection) error {
	switch i.State {
	case "running", "unknown":
	case "terminated", "not_started":
		if i.Proof == "" {
			return errInvalid(CodeResultInvalid, "inspection.proof", "a terminated or never-started execution needs its proof")
		}
	default:
		return errInvalid(CodeResultInvalid, "inspection.state", "unknown state "+i.State)
	}
	return validObservation(i.NativeSession, "inspection.native_session")
}

func ValidateCancellation(c Cancellation) error {
	if c.Terminated && c.Proof == "" {
		return errInvalid(CodeResultInvalid, "cancellation.proof", "a terminated execution needs its proof")
	}
	return nil
}
