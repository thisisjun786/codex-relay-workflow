package service

import (
	"context"
	"errors"
	"fmt"
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

// acceptControl is the listener's accept; a test replaces it to inject the kernel's refusals.
var acceptControl = (*net.UnixListener).Accept

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
		var acceptErr error
		for {
			conn, e := acceptControl(listener)
			if e != nil {
				if errors.Is(e, net.ErrClosed) {
					break
				}
				if acceptRetried(e) {
					// control.py: back off briefly, then accept again.
					time.Sleep(50 * time.Millisecond)
					continue
				}
				acceptErr = e
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				// Nothing a peer does ends or fails the owner (control.py GuardServer._serve,
				// PR #185 4128954449). The handler answers every request it cannot serve with
				// the host record, which the requester journals; what it still returns is a peer
				// that went away or ran out of time before its answer, whose own adapter journals
				// that. Neither is this listener's failure, so neither reaches Close.
				defer func() { _ = recover() }()
				// control.py reads the request line under a 5 s timeout and answers its expiry;
				// the handler keeps ControlAnswerGrace past it to write that answer.
				requestCtx, stop := context.WithTimeout(serveCtx, 5*time.Second+hook.ControlAnswerGrace)
				defer stop()
				// The request deadline bounds malformed/idle clients too.
				deadline, _ := requestCtx.Deadline()
				if conn.SetDeadline(deadline) != nil {
					return
				}
				finished := context.AfterFunc(requestCtx, func() { _ = conn.Close() })
				defer finished()
				_ = dispatchControl(requestCtx, conn, state)
			}()
		}
		wg.Wait()
		control.done <- acceptErr
	}()
	return control, nil
}

// acceptRetried is whether a failed accept refused one connection rather than the listener:
// the kernel out of descriptors, buffers or memory, or a connection aborted before it was
// accepted. control.py backs off 50 ms and accepts again; any other error ends the listener
// and Close reports it.
func acceptRetried(err error) bool {
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM, syscall.ECONNABORTED, syscall.EPROTO, syscall.EINTR, syscall.EAGAIN} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}
func (c *Control) Close() error {
	c.cancel()
	err := c.listener.Close()
	if e := os.Remove(c.path); e != nil && !errors.Is(e, os.ErrNotExist) {
		err = errors.Join(err, e)
	}
	return errors.Join(err, <-c.done)
}

// control.sock serves guard-evaluate only, as Python's GuardServer does. Decision-25 ingress
// is the queued command's own file publication, never a socket method. Delegate exactly to
// todo 33, including shared state-selection refusals; the handler reads the one request line.
func dispatchControl(ctx context.Context, conn net.Conn, state string) error {
	return hook.HandleControl(ctx, conn, state)
}
