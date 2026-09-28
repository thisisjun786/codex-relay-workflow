package hook

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

type peerAuthError struct{ detail string }

func (e *peerAuthError) Error() string { return "the native guard could not be trusted: " + e.detail }

// peerIdentityKey injects only observed ownership in tests, not the trust decision.
type peerIdentityKey struct{}
type peerIdentity struct {
	peerUID, socketUID, directoryUID uint32
	directoryMode                    os.FileMode
}

func authenticatePeer(ctx context.Context, conn net.Conn) error {
	refuse := func(detail string) error { return &peerAuthError{detail: detail} }
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return refuse("control connection is not a Unix socket")
	}
	uid, err := peerUID(unixConn)
	if err != nil {
		return refuse("peer credentials unavailable: " + err.Error())
	}
	path := conn.RemoteAddr().String()
	info, err := os.Lstat(path)
	if err != nil {
		return refuse("socket ownership unavailable: " + err.Error())
	}
	if info.Mode()&os.ModeSocket == 0 {
		return refuse("control path is not a socket")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return refuse("socket directory unavailable: " + err.Error())
	}
	if !parent.IsDir() {
		return refuse("socket parent is not a directory")
	}
	observed := peerIdentity{uid, info.Sys().(*syscall.Stat_t).Uid, parent.Sys().(*syscall.Stat_t).Uid, parent.Mode()}
	if inject, ok := ctx.Value(peerIdentityKey{}).(func(peerIdentity) peerIdentity); ok {
		observed = inject(observed)
	}
	ours := uint32(os.Getuid())
	if observed.peerUID != ours {
		return refuse(fmt.Sprintf("peer uid %d differs from our uid %d", observed.peerUID, ours))
	}
	if observed.socketUID != ours {
		return refuse(fmt.Sprintf("socket uid %d differs from our uid %d", observed.socketUID, ours))
	}
	if observed.directoryUID != ours {
		return refuse(fmt.Sprintf("socket directory uid %d differs from our uid %d", observed.directoryUID, ours))
	}
	if observed.directoryMode.Perm()&0022 != 0 {
		return refuse("socket directory is group- or world-writable")
	}
	return nil
}
