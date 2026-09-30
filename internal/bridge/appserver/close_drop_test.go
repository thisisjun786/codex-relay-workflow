package appserver

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// droppingHost is an App Server stand-in that answers the initialize handshake and then, as
// codex-cli 0.154.0's App Server was measured to (todo 42 on the owner's host), ends the
// connection when the client's close frame arrives without answering it with its own.
func droppingHost(t *testing.T) string {
	t.Helper()
	// Not t.TempDir: this test's long name would push the socket path past the 108-byte limit.
	dir, err := os.MkdirTemp("", "crw-drophost-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "app.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveAndDrop(conn)
		}
	}()
	return path
}

func serveAndDrop(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	digest := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if _, err = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:])); err != nil {
		return
	}
	for {
		opcode, payload, err := readClientFrame(reader)
		if err != nil || opcode == 0x8 {
			return // the close frame: drop the connection, no close frame back
		}
		var message struct {
			ID json.RawMessage `json:"id"`
		}
		if opcode != 0x1 || json.Unmarshal(payload, &message) != nil || message.ID == nil {
			continue
		}
		reply, _ := json.Marshal(map[string]any{"id": message.ID, "result": map[string]any{"userAgent": "dropping-host"}})
		header := []byte{0x81, byte(len(reply))}
		if len(reply) >= 126 {
			header = append([]byte{0x81, 126}, byte(len(reply)>>8), byte(len(reply)))
		}
		if _, err = conn.Write(append(header, reply...)); err != nil {
			return
		}
	}
}

// readClientFrame reads one masked client frame (RFC 6455 5.2).
func readClientFrame(r *bufio.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		size = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if head[1]&0x80 != 0 {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return head[0] & 0x0f, payload, nil
}

// A peer that ends the connection instead of answering the close frame has closed it: Close
// reports success, as Python's websockets close() does, rather than the handshake's EOF, which
// failed every Go relay command that reached the host on the owner's host.
func TestClose_accepts_a_peer_that_drops_the_connection_instead_of_answering(t *testing.T) {
	client := New(droppingHost(t), PhaseBounds{Establish: 2 * time.Second, Transmit: time.Second, Ack: time.Second})
	if err := client.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("close after the peer dropped the connection: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("close waited %s for a peer that had already gone", elapsed)
	}
}
