package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// rpc.py:362: a frame that is not JSON reaches the reader's generic handler, so the pending
// request fails as a TransportError reading "App Server transport failed: JSONDecodeError: ...".
func TestReceive_non_json_frame_fails_pending_request_as_python_transport_error(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		for {
			_, raw, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &message) != nil {
				return
			}
			switch message.Method {
			case "initialize":
				_ = conn.Write(ctx, websocket.MessageText, []byte(`{"id":`+string(message.ID)+`,"result":{"userAgent":"x"}}`))
			case "thread/read":
				_ = conn.Write(ctx, websocket.MessageText, []byte("{bad"))
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	client := New(socket, PhaseBounds{Establish: 5 * time.Second, Transmit: 5 * time.Second, Ack: 5 * time.Second})
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.Call(context.Background(), "thread/read", map[string]any{})
	var transport *TransportError
	if !errors.As(err, &transport) || !strings.HasPrefix(transport.Reason, "App Server transport failed: JSONDecodeError: ") {
		t.Fatalf("non-JSON frame failed the request as %T %v", err, err)
	}
}
