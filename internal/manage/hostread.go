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

// hostReadMethods is the allow-list crw manage host-read may call: reads only, so a write method is
// refused before a socket is opened.
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

// hostReadReason reads the refusal an error carries; one this file did not raise is a host error,
// since the only host calls it makes are reads.
func hostReadReason(err error) (hostReadRefusal, string) {
	var refusal *hostReadError
	if errors.As(err, &refusal) {
		return refusal.reason, refusal.detail
	}
	return hostReadHostError, err.Error()
}

// HostRead performs one allow-listed read against the App Server socket in cfg; a method outside the
// allow-list is refused before any connection, so a write method never reaches the host.
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

// hostReadEnvelope is the one JSON object host-read and child-check write; Result is the host's own
// bytes, embedded without re-encoding.
type hostReadEnvelope struct {
	OK     bool            `json:"ok"`
	Method string          `json:"method,omitempty"`
	Reason hostReadRefusal `json:"reason,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// hostReadWrite writes one JSON value and a newline to w.
func hostReadWrite(w io.Writer, value any) {
	data, _ := json.Marshal(value)
	fmt.Fprintf(w, "%s\n", data)
}

// hostReadParse reads "--flag value" pairs, refusing anything else; help reports -h/--help.
func hostReadParse(args []string, allowed map[string]bool) (values map[string]string, help bool, err error) {
	values = map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "-h" || args[i] == "--help" {
			return values, true, nil
		}
		if !strings.HasPrefix(args[i], "--") {
			return nil, false, fmt.Errorf("unexpected argument %q", args[i])
		}
		name := strings.TrimPrefix(args[i], "--")
		if !allowed[name] {
			return nil, false, fmt.Errorf("unknown option %q", args[i])
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

// hostReadRunCommand is crw manage host-read: method_not_read_only with exit 2, the host failures
// with exit 3.
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
	var params map[string]any
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
			hostReadWrite(e.Stdout, hostReadEnvelope{Reason: reason, Method: method})
			return usageExit
		}
		hostReadWrite(e.Stdout, hostReadEnvelope{Reason: reason, Detail: detail})
		return 3
	}
	hostReadWrite(e.Stdout, hostReadEnvelope{OK: true, Method: method, Result: result})
	return 0
}
