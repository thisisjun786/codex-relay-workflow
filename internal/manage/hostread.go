package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
)

// hostReadMethods is the allow-list host-read may call: reads only, so a write method is refused first.
var hostReadMethods = map[string]bool{
	"thread/read": true, "thread/list": true, "thread/loaded/list": true,
	"thread/turns/list": true, "mcpServerStatus/list": true,
}

// hostReadRefusal is why a read was refused.
type hostReadRefusal string

const (
	hostReadMethodNotReadOnly hostReadRefusal = "method_not_read_only"
	hostReadUnreachable       hostReadRefusal = "host_unreachable"
	hostReadHostError         hostReadRefusal = "host_error"
)

// hostReadError is a refused read: the reason and, where the host gave one, its detail.
type hostReadError struct {
	reason hostReadRefusal
	detail string
}

func (e *hostReadError) Error() string { return string(e.reason) + ": " + e.detail }

// hostReadReason reads the refusal an error carries; one this file did not raise is a host error.
func hostReadReason(err error) (hostReadRefusal, string) {
	var refusal *hostReadError
	if errors.As(err, &refusal) {
		return refusal.reason, refusal.detail
	}
	return hostReadHostError, err.Error()
}

// HostRead performs one allow-listed read against the App Server socket in cfg.
func HostRead(ctx context.Context, cfg *Config, method string, params map[string]any) (json.RawMessage, error) {
	if !hostReadMethods[method] {
		return nil, &hostReadError{reason: hostReadMethodNotReadOnly}
	}
	client := appserver.New(cfg.Relay.Socket, appserver.DefaultBounds)
	defer client.Close()
	if err := client.Connect(ctx); err != nil {
		return nil, &hostReadError{reason: hostReadUnreachable, detail: err.Error()}
	}
	result, err := client.Call(ctx, method, params)
	if err != nil {
		return nil, &hostReadError{reason: hostReadHostError, detail: err.Error()}
	}
	return result, nil
}

// hostReadFailure is a refused read, as host-read and child-check report one.
type hostReadFailure struct {
	OK     bool            `json:"ok"`
	Reason hostReadRefusal `json:"reason"`
	Method string          `json:"method,omitempty"`
	Detail string          `json:"detail,omitempty"`
}

// hostReadWrite writes one JSON value and a newline to w.
func hostReadWrite(w io.Writer, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		fmt.Fprintf(w, "{\"ok\":false,\"reason\":\"host_error\",\"detail\":%q}\n", err.Error())
		return
	}
	fmt.Fprintf(w, "%s\n", data)
}

// hostReadWriteResult writes a read the host answered, splicing the result bytes in unchanged.
func hostReadWriteResult(w io.Writer, method string, result json.RawMessage) {
	quoted, _ := json.Marshal(method)
	fmt.Fprintf(w, "{\"ok\":true,\"method\":%s,\"result\":%s}\n", quoted, result)
}

// hostReadParse reads "--flag value" pairs, refusing anything else; help reports -h/--help.
func hostReadParse(args []string, allowed map[string]bool) (values map[string]string, help bool, err error) {
	values = map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "-h" || args[i] == "--help" {
			return values, true, nil
		}
		name := strings.TrimPrefix(args[i], "--")
		if name == args[i] || !allowed[name] {
			return nil, false, fmt.Errorf("unexpected argument %q", args[i])
		}
		if i+1 >= len(args) {
			return nil, false, fmt.Errorf("option %q needs a value", args[i])
		}
		i++
		values[name] = args[i]
	}
	return values, false, nil
}

var hostReadCommand = Command{Name: "host-read", Summary: "read one allow-listed App Server method over the relay socket", Run: hostReadRunCommand}

func init() { Register(hostReadCommand) }

// hostReadRunCommand is crw manage host-read: method_not_read_only exit 2, host failures exit 3.
func hostReadRunCommand(ctx context.Context, e *Env, args []string) int {
	const usage = "usage: crw manage host-read --method M [--params JSON]"
	values, help, err := hostReadParse(args, map[string]bool{"method": true, "params": true})
	if help {
		fmt.Fprintln(e.Stdout, usage)
		return 0
	}
	method, ok := values["method"]
	if err == nil && !ok {
		err = errors.New("--method is required")
	}
	params := map[string]any{}
	if err == nil {
		if raw, present := values["params"]; present {
			err = json.Unmarshal([]byte(raw), &params)
		}
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, usage)
		fmt.Fprintf(e.Stderr, "crw manage host-read: error: %v\n", err)
		return usageExit
	}
	result, err := HostRead(ctx, coreDefaults(e), method, params)
	if err != nil {
		reason, detail := hostReadReason(err)
		if reason == hostReadMethodNotReadOnly {
			hostReadWrite(e.Stdout, hostReadFailure{Reason: reason, Method: method})
			return usageExit
		}
		hostReadWrite(e.Stdout, hostReadFailure{Reason: reason, Detail: detail})
		return 3
	}
	hostReadWriteResult(e.Stdout, method, result)
	return 0
}
