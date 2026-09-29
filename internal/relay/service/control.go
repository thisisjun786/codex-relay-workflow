package service

import (
	"bufio"
	"bytes"
	"context"
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
	path     string
}

func ListenControl(ctx context.Context, state string) (*Control, error) {
	info, err := os.Stat(state)
	if err != nil {
		return nil, err
	}
	// The same rule every client applies (decision 24): owned by this user and not
	// group or world writable. Existing directories are never chmodded.
	if !info.IsDir() || info.Mode().Perm()&0022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("control socket requires an owned state directory that is not group or world writable")
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
	address, release, err := hook.ControlAddress(path)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	release()
	if err != nil {
		return nil, err
	}
	// Close removes the real pathname itself: an unlink by a /proc/self/fd name would
	// resolve against whatever that descriptor number names by then.
	listener.SetUnlinkOnClose(false)
	if err = os.Chmod(path, 0600); err != nil {
		return nil, errors.Join(err, listener.Close(), os.Remove(path))
	}
	serveCtx, cancel := context.WithCancel(ctx)
	control := &Control{listener, cancel, make(chan error, 1), path}
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
	if e := os.Remove(c.path); e != nil && !errors.Is(e, os.ErrNotExist) {
		err = errors.Join(err, e)
	}
	return errors.Join(err, <-c.done)
}

type framedConn struct {
	net.Conn
	reader io.Reader
}

func (c *framedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func dispatchControl(ctx context.Context, conn net.Conn, state string) error {
	raw, err := bufio.NewReader(io.LimitReader(conn, (64<<20)+1)).ReadBytes('\n')
	// Judged before the read error: the limit ends an oversized frame with io.EOF too.
	if len(raw) > 64<<20 {
		return fmt.Errorf("control frame too large")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
		// The client went away before it finished a request line (a probe, or a hook that
		// gave up): it asked nothing, so nothing failed. Python's GuardServer only closes it.
		return nil
	}
	if err != nil {
		return err
	}
	// control.sock serves guard-evaluate only, as Python's GuardServer does. Decision-25
	// ingress is the queued command's own file publication, never a socket method.
	// Delegate exactly to todo 33, including shared state-selection refusals.
	return hook.HandleControl(ctx, &framedConn{conn, bytes.NewReader(raw)}, state)
}
