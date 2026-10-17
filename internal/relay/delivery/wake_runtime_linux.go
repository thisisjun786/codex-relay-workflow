package delivery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"golang.org/x/sys/unix"
)

// Process incarnation includes boot and start ticks, so PID reuse is not reuse of
// evidence. A replaced executable also changes the transport revision.
func wakeProcess(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("worker PID is unknown")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(raw), ") ")
	if end < 0 {
		return "", fmt.Errorf("process stat is unreadable")
	}
	fields := strings.Fields(string(raw)[end+2:])
	if len(fields) < 20 || strings.ContainsAny(fields[0], "TtZXx") {
		return "", fmt.Errorf("process is unavailable")
	}
	return strings.TrimSpace(string(boot)) + ":" + strconv.Itoa(pid) + ":" + fields[19], nil
}

func wakeRuntime(ctx context.Context, socket string, workerPID int, connections ...net.Conn) (WakeIdentity, error) {
	var identity WakeIdentity
	canonical, err := store.CanonicalSocket(socket)
	if err != nil || socket == "" {
		return identity, fmt.Errorf("wake socket is unknown: %v", err)
	}
	worker, err := wakeProcess(workerPID)
	if err != nil {
		return identity, err
	}
	var conn net.Conn
	switch len(connections) {
	case 0:
		conn, err = (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", canonical)
		if err != nil {
			return identity, err
		}
		defer conn.Close()
	case 1:
		conn = connections[0]
	default:
		return identity, fmt.Errorf("one sending connection is required")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok || unixConn == nil {
		return identity, fmt.Errorf("sending connection is not a Unix socket")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return identity, err
	}
	var peer *unix.Ucred
	var peerErr error
	if err := raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return identity, err
	}
	if peerErr != nil {
		return identity, peerErr
	}
	if peer.Uid != uint32(os.Geteuid()) {
		return identity, fmt.Errorf("wake socket belongs to another user")
	}
	server, err := wakeProcess(int(peer.Pid))
	if err != nil {
		return identity, err
	}
	executable, err := os.Open("/proc/" + strconv.Itoa(workerPID) + "/exe")
	if err != nil {
		return identity, err
	}
	digest := sha256.New()
	_, readErr := io.Copy(digest, executable)
	closeErr := executable.Close()
	if readErr != nil {
		return identity, readErr
	}
	if closeErr != nil {
		return identity, closeErr
	}
	after, err := wakeProcess(workerPID)
	if err != nil || after != worker {
		return identity, fmt.Errorf("worker changed during observation")
	}
	boot := strings.SplitN(worker, ":", 2)[0]
	identity.OperatingScope = fmt.Sprintf("%s:uid:%d", boot, os.Geteuid())
	identity.AppServerInstance, identity.ServiceInstance = server, worker
	identity.Socket, identity.TransportRevision = canonical, fmt.Sprintf("sha256:%x", digest.Sum(nil))
	return identity, nil
}
