package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

// Control is bound only while the caller holds daemon and scope ownership.
// Close waits for every bounded handler before the writer gate is released.
type Control struct {
	listener *net.UnixListener
	cancel   context.CancelFunc
	done     chan error
}

func ListenControl(ctx context.Context, state string) (*Control, error) {
	info, err := os.Stat(state)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("control socket requires an owned 0700 state directory")
	}
	path := filepath.Join(state, "control.sock")
	if existing, err := os.Lstat(path); err == nil {
		if existing.Mode()&os.ModeSocket == 0 || existing.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			return nil, fmt.Errorf("refuse replacing foreign control endpoint")
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	serveCtx, cancel := context.WithCancel(ctx)
	control := &Control{listener, cancel, make(chan error, 1)}
	go func() {
		var wg sync.WaitGroup
		var handlerErr error
		var mu sync.Mutex
		remember := func(e error) {
			if e != nil {
				mu.Lock()
				handlerErr = errors.Join(handlerErr, e)
				mu.Unlock()
			}
		}
		var acceptErr error
		for {
			conn, e := listener.Accept()
			if e != nil {
				if !errors.Is(e, net.ErrClosed) {
					acceptErr = e
				}
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				defer func() {
					if p := recover(); p != nil {
						remember(fmt.Errorf("control handler panic: %v", p))
					}
				}()
				requestCtx, stop := context.WithTimeout(serveCtx, 5*time.Second)
				defer stop()
				// The request deadline bounds malformed/idle clients too.
				deadline, _ := requestCtx.Deadline()
				if e := conn.SetDeadline(deadline); e != nil {
					remember(e)
					return
				}
				finished := context.AfterFunc(requestCtx, func() { _ = conn.Close() })
				defer finished()
				if e := dispatchControl(requestCtx, conn, state); e != nil && !errors.Is(e, net.ErrClosed) && !errors.Is(e, context.Canceled) {
					remember(e)
				}
			}()
		}
		wg.Wait()
		control.done <- errors.Join(acceptErr, handlerErr)
	}()
	return control, nil
}
func (c *Control) Close() error {
	c.cancel()
	err := c.listener.Close()
	return errors.Join(err, <-c.done)
}

type framedConn struct {
	net.Conn
	reader io.Reader
}

func (c *framedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func dispatchControl(ctx context.Context, conn net.Conn, state string) error {
	raw, err := bufio.NewReader(io.LimitReader(conn, (64<<20)+1)).ReadBytes('\n')
	if err != nil {
		return err
	}
	if len(raw) > 64<<20 {
		return fmt.Errorf("control frame too large")
	}
	var envelope struct {
		Protocol int    `json:"protocol"`
		Method   string `json:"method"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	if envelope.Protocol == 1 && envelope.Method == "inbox-submit" {
		return inboxUnavailable(conn)
	}
	// Delegate exactly to todo 33, including shared state-selection refusals.
	return hook.HandleControl(ctx, &framedConn{conn, bytes.NewReader(raw)}, state)
}

// inboxUnavailable is the explicit todo-31 ingress seam. It promises no durable
// acceptance and does not reinterpret decision-25 bytes or claim application.
func inboxUnavailable(conn net.Conn) error {
	_, err := io.WriteString(conn, "{\"error\":\"host\",\"reason\":\"inbox_unavailable\",\"detail\":\"Go inbox ingress is not available in this build\"}\n")
	return err
}
