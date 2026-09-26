package appserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func TestCall_handshake_identity_and_compression(t *testing.T) {
	// Given: a real unix-socket fake host that records the client's handshake.
	host := fakehost.Start(t)
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When: initialize is observed by the host.
	requests := host.Requests()
	var handshake struct {
		ClientInfo struct {
			Name string `json:"name"`
		} `json:"clientInfo"`
		Capabilities struct {
			ExperimentalAPI bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(requests[0].Params, &handshake); err != nil {
		t.Fatal(err)
	}
	// Then: client identity and experimental capability are sent on an uncompressed connection.
	if handshake.ClientInfo.Name != "codex_thread_bridge" || !handshake.Capabilities.ExperimentalAPI {
		t.Fatalf("handshake: %+v", handshake)
	}
	if host.CompressionOff() != 1 {
		t.Fatalf("compression was offered: %d uncompressed handshakes", host.CompressionOff())
	}
}

func TestCall_accepts_frame_just_below_real_limit(t *testing.T) {
	// Given: response framing JSON occupies 35 bytes around its padding.
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{PadBytes: 16*1024*1024 - len(`{"id":2,"result":{"padding":""}}`)})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	result, err := client.Call(bounded(t), "thread/read", map[string]any{})
	// Then: a frame at the real boundary is accepted, not a false overflow.
	if err != nil || !strings.Contains(string(result), `"padding"`) {
		t.Fatalf("boundary response: %v", err)
	}
}

func TestCall_preserves_rpc_error_and_reconnects_only_for_new_requests(t *testing.T) {
	// Given: first an API error, then a dropped connection, then a healthy host.
	host := fakehost.Start(t)
	host.Script("thread/goal/get", fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: "unsupported"}}, fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001}}, fakehost.Reply{Result: map[string]any{"goal": "new"}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When: the three distinct calls each make exactly one attempt.
	_, apiErr := client.Call(bounded(t), "thread/goal/get", map[string]any{})
	_, dropErr := client.Call(bounded(t), "thread/goal/get", map[string]any{})
	result, nextErr := client.Call(bounded(t), "thread/goal/get", map[string]any{})
	// Then
	var rpc *appserver.RPCError
	if !errors.As(apiErr, &rpc) || rpc.Code != -32601 || rpc.Message != "unsupported" {
		t.Fatalf("lost API error: %v", apiErr)
	}
	var transport *appserver.ResponseTooLarge
	if dropErr == nil || errors.As(dropErr, &transport) {
		t.Fatalf("wrong drop error: %v", dropErr)
	}
	if nextErr != nil || string(result) != `{"goal":"new"}` || host.Count("thread/goal/get") != 3 {
		t.Fatalf("reconnect/retry: %s %v, count %d", result, nextErr, host.Count("thread/goal/get"))
	}
}

func TestCall_host_accepts_large_request_below_sixteen_mib(t *testing.T) {
	// Given: a client frame larger than 8MiB but below the 16MiB host receive ceiling.
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"ok": true}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	result, err := client.Call(bounded(t), "thread/read", map[string]any{"padding": strings.Repeat("x", 9*1024*1024)})
	// Then: the fake peer actually reads and answers the frame.
	if err != nil || string(result) != `{"ok":true}` || host.Count("thread/read") != 1 {
		t.Fatalf("host rejected 9MiB frame: %v, count %d", err, host.Count("thread/read"))
	}
}

func TestCall_oversized_notification_is_not_attributed_to_pending_request(t *testing.T) {
	// Given: a notification larger than the receive limit precedes the pending response.
	host := fakehost.Start(t)
	host.Respond("thread/goal/get", fakehost.Reply{Before: []fakehost.Notification{{Method: "thread/status/changed", Params: map[string]any{"padding": strings.Repeat("x", 16*1024*1024)}}}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	_, err = client.Call(bounded(t), "thread/goal/get", map[string]any{})
	// Then: a frame without an id cannot be blamed on the pending method.
	var oversized *appserver.ResponseTooLarge
	if !errors.As(err, &oversized) || strings.Contains(err.Error(), "thread/goal/get") {
		t.Fatalf("attributed notification: %v", err)
	}
}

func TestCall_does_not_retry_any_method_on_ack_or_transport_failure(t *testing.T) {
	methods := []string{"project/read", "thread/start", "thread/name/set", "turn/start", "thread/read", "thread/resume", "thread/goal/get", "thread/goal/set", "thread/turns/list", "thread/items/list", "turn/steer"}
	for _, method := range methods {
		for _, path := range []string{"ack", "drop"} {
			t.Run(method+"/"+path, func(t *testing.T) {
				// Given: one request unanswered or a peer dropping its transport.
				host := fakehost.Start(t)
				reply := fakehost.Reply{Delay: time.Second}
				if path == "drop" {
					reply = fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001}}
				}
				host.Respond(method, reply)
				client := appserver.New(host.SocketPath, appserver.PhaseBounds{Establish: time.Second, Transmit: time.Second, Ack: 15 * time.Millisecond})
				defer client.Close()
				// When
				_, err := client.Call(bounded(t), method, map[string]any{})
				// Then: neither failure path is safe to replay, for reads or mutations.
				if err == nil || host.Count(method) != 1 {
					t.Fatalf("retry or missing failure: %v, count %d", err, host.Count(method))
				}
			})
		}
	}
}

func TestCall_peer_1009_is_not_local_receive_overflow(t *testing.T) {
	// Given: the peer itself sends 1009; our receive limit has not been reached.
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1009, Reason: "frame with 17825792 bytes exceeds limit of 16777216 bytes"}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	_, err = client.Call(bounded(t), "thread/read", map[string]any{})
	// Then
	var oversized *appserver.ResponseTooLarge
	if err == nil || errors.As(err, &oversized) {
		t.Fatalf("peer close misclassified: %v", err)
	}
}

func TestCall_reports_establish_timeout_when_handshake_stalls(t *testing.T) {
	// Given: initialize cannot complete before establishment's bound.
	host := fakehost.Start(t)
	host.Script("initialize", fakehost.Reply{Delay: time.Second})
	client := appserver.New(host.SocketPath, appserver.PhaseBounds{Establish: 30 * time.Millisecond, Transmit: time.Second, Ack: time.Second})
	defer client.Close()
	// When
	_, err := client.Call(bounded(t), "thread/start", map[string]any{})
	// Then
	var timeout *appserver.PhaseTimeout
	if !errors.As(err, &timeout) || timeout.Phase != "establish" || timeout.Method != "thread/start" {
		t.Fatalf("want establish timeout: %v", err)
	}
	if got := host.Count("thread/start"); got != 0 {
		t.Fatalf("sent frame before handshake: %d", got)
	}
}

func TestCall_delivers_notification_before_response(t *testing.T) {
	// Given
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Before: []fakehost.Notification{{Method: "thread/status/changed", Params: map[string]any{"threadId": "a"}}}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// When
	_, err = client.Call(bounded(t), "thread/read", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	// Then
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	select {
	case event := <-client.Notifications():
		if event.Method != "thread/status/changed" {
			t.Fatalf("wrong event: %+v", event)
		}
		var params map[string]any
		if err := json.Unmarshal(event.Params, &params); err != nil || params["threadId"] != "a" {
			t.Fatalf("wrong params: %+v %v", params, err)
		}
	case <-ctx.Done():
		t.Fatal("notification not delivered")
	}
}
