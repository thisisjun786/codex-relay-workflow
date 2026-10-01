package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func controlPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	home := hookHome(t, 5)
	path := filepath.Join(home, "state/control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	client, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	server, err := listener.Accept()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return client, server
}

func Test33PeerCredentials(t *testing.T) {
	client, _ := controlPair(t)
	uid, err := peerUID(client.(*net.UnixConn))
	if err != nil || uid != uint32(os.Getuid()) {
		t.Fatal(uid, err)
	}
	if err := authenticatePeer(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"peer_uid", "socket_uid", "directory_uid", "writable_directory"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), peerIdentityKey{}, func(p peerIdentity) peerIdentity {
				switch name {
				case "peer_uid":
					p.peerUID++
				case "socket_uid":
					p.socketUID++
				case "directory_uid":
					p.directoryUID++
				case "writable_directory":
					p.directoryMode |= 0020
				}
				return p
			})
			var auth *peerAuthError
			if err := authenticatePeer(ctx, client); !errors.As(err, &auth) {
				t.Fatal(err)
			}
		})
	}
}

func Test33UntrustedPeerReleases(t *testing.T) {
	for _, name := range []string{"wrong_owner", "writable_directory"} {
		t.Run(name, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			// A real same-uid listener is ready to block. Refusal must occur before
			// sending any request, not merely after examining an untrusted answer.
			done, _ := fakeControl(t, home, func(conn net.Conn) error {
				raw, err := io.ReadAll(conn)
				if err != nil {
					return err
				}
				if len(raw) != 0 {
					return fmt.Errorf("untrusted peer received request: %s", raw)
				}
				return nil
			})
			ctx := context.Background()
			if name == "wrong_owner" {
				ctx = context.WithValue(ctx, peerIdentityKey{}, func(p peerIdentity) peerIdentity { p.socketUID++; return p })
				var out bytes.Buffer
				if code := Run(ctx, nil, bytes.NewBufferString(`{"session_id":"s","turn_id":"t"}`), &out, time.Now()); code != 0 || out.Len() != 0 {
					t.Fatal(code, out.String())
				}
			} else {
				if err := os.Chmod(filepath.Join(home, "state"), 0777); err != nil {
					t.Fatal(err)
				}
				command := hookCommand(t, home, `{"session_id":"s","turn_id":"t"}`)
				out, err := command.CombinedOutput()
				if err != nil || len(out) != 0 {
					t.Fatalf("%v %s", err, out)
				}
			}
			awaitHost(t, done)
			rows := rowsAt(t, home)
			if len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_unreachable" || rows[0]["errno"] != "EACCES" || rows[0]["held"] != false || rows[0]["processEnding"] != "not_started" {
				t.Fatal(rows)
			}
		})
	}
}

// Under a umask of 002 the Python store created its state directory owner-only (0700) and kept
// an existing one's mode (0775); the Go store does the same.
func Test33PythonStateDirectoryMode(t *testing.T) {
	home := t.TempDir()
	previous := syscall.Umask(0o002)
	defer syscall.Umask(previous)
	for _, c := range []struct {
		name string
		mode fs.FileMode
	}{{"new", 0o700}, {"existing", 0o775}} {
		directory := filepath.Join(home, c.name)
		if c.name == "existing" {
			if err := os.Mkdir(directory, 0o777); err != nil {
				t.Fatal(err)
			}
		}
		s, err := fixtureStore(context.Background(), filepath.Join(directory, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != c.mode {
			t.Fatalf("Go store's %s state directory: %v %v, Python's %o", c.name, info.Mode(), err, c.mode)
		}
	}
}
