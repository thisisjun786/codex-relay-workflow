// Package transport frames authenticated local/SSH worker requests as JSON.
// No task bytes, repository path or command arguments enter an SSH shell line.
package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
)

const MaxFrame = 4 << 20
const Schema = "crw-worker/1"

type Request struct {
	Workspace string             `json:"workspace,omitempty"`
	Schema    string             `json:"schema"`
	Method    string             `json:"method"`
	Admission executor.Admission `json:"admission"`
	Grant     executor.Grant     `json:"grant"`
	Reference executor.Reference `json:"reference"`
	Toolchain worker.Toolchain   `json:"toolchain"`
	Group     worker.Group       `json:"group"`
}
type Response struct {
	Schema       string                   `json:"schema"`
	Peer         executor.Peer            `json:"peer"`
	Error        string                   `json:"error,omitempty"`
	Handle       executor.Handle          `json:"handle"`
	Inspection   executor.Inspection      `json:"inspection"`
	Cancellation executor.Cancellation    `json:"cancellation"`
	Delivery     executor.Delivery        `json:"delivery"`
	Observation  worker.Observation       `json:"observation"`
	Toolchain    worker.ToolchainReadback `json:"toolchain"`
	Command      worker.CommandResult     `json:"command"`
}
type Caller interface {
	Call(context.Context, Request) (Response, error)
}
type Process struct {
	Host                crwconfig.ExecutionHost
	LocalBinary         string
	Config              string
	ExpectedIncarnation string
}

func (p Process) Command(ctx context.Context) *exec.Cmd {
	if p.Host.Transport == "ssh" {
		return exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+p.Host.KnownHosts, "--", p.Host.Endpoint, "crw worker serve")
	}
	binary := p.LocalBinary
	if binary == "" {
		binary = "crw"
	}
	args := []string{"worker", "serve"}
	if p.Config != "" {
		args = append(args, "--config", p.Config)
	}
	return exec.CommandContext(ctx, binary, args...)
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxFrame {
		return 0, errors.New("worker response exceeds frame limit")
	}
	return b.Buffer.Write(p)
}
func (p Process) Call(ctx context.Context, r Request) (Response, error) {
	if err := p.Host.Validate(); err != nil {
		return Response{}, err
	}
	r.Schema = Schema
	raw, err := json.Marshal(r)
	if err != nil {
		return Response{}, err
	}
	if len(raw) > MaxFrame {
		return Response{}, errors.New("worker request exceeds frame limit")
	}
	cmd := p.Command(ctx)
	cmd.Stdin = bytes.NewReader(raw)
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return Response{}, fmt.Errorf("worker transport: %w", err)
	}
	var response Response
	if err = Decode(&stdout, &response); err != nil {
		return response, err
	}
	if response.Schema != Schema || response.Peer.HostID != p.Host.ID || (p.ExpectedIncarnation != "" && response.Peer.Incarnation != p.ExpectedIncarnation) {
		return Response{}, errors.New("worker response identity mismatch")
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}
func Decode(r io.Reader, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r, MaxFrame+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxFrame {
		return errors.New("worker frame too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing worker frame")
	}
	return nil
}

type Adapter struct {
	Caller       Caller
	ExecutorKind executor.Kind
	Revision     string
}

func (a Adapter) Kind() executor.Kind { return a.ExecutorKind }
func (a Adapter) Capabilities() executor.Capabilities {
	return executor.Capabilities{Revision: a.Revision, Supported: []executor.Capability{executor.CapStart, executor.CapInspect, executor.CapCancel, executor.CapCollect}, Unsupported: map[executor.Capability]string{executor.CapResume: "worker resume is not enabled", executor.CapFork: "worker fork is not enabled"}}
}
func (a Adapter) Start(ctx context.Context, in executor.Admission, g executor.Grant) (executor.Handle, error) {
	r, err := a.Caller.Call(ctx, Request{Method: "start", Admission: in, Grant: g})
	return r.Handle, err
}
func (a Adapter) Inspect(ctx context.Context, ref executor.Reference) (executor.Inspection, error) {
	r, err := a.Caller.Call(ctx, Request{Method: "inspect", Reference: ref})
	return r.Inspection, err
}
func (a Adapter) Cancel(ctx context.Context, ref executor.Reference, g executor.Grant) (executor.Cancellation, error) {
	r, err := a.Caller.Call(ctx, Request{Method: "cancel", Reference: ref, Grant: g})
	return r.Cancellation, err
}
func (a Adapter) Collect(ctx context.Context, ref executor.Reference) (executor.Delivery, error) {
	r, err := a.Caller.Call(ctx, Request{Method: "collect", Reference: ref})
	return r.Delivery, err
}

func (a Adapter) RenewGrant(ctx context.Context, r executor.Reference, g executor.Grant) error {
	_, err := a.Caller.Call(ctx, Request{Method: "renew", Reference: r, Grant: g})
	return err
}
