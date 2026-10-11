package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the dispatch side of the creation rule (engine.dispatch.reconcile): a thread/start or thread/fork is the
// one mutation that may be sent again, and only when the host's own fields show that the first request made nothing and the
// connection that carried it is closed. The backend therefore records, for every creation request it writes, the connection
// the frame went out on, and names the process it lives in on the intent row so that a later process can tell whether that
// connection can still be open.

// DefaultSource is the Thread.source of the threads this backend creates: the app-server source the bridge connects as.
// thread/list is filtered by it, and a thread of another source is never a candidate.
const DefaultSource = "appServer"

// creationKey names one creation request: the call and its send ordinal.
func creationKey(callID string, send int) string { return fmt.Sprintf("%s#%d", callID, max(1, send)) }

// sentRecord is the connection a creation request's frame went out on: the client that held it and the id it gave it.
type sentRecord struct {
	client *appserver.Client
	conn   uint64
}

// sentCreations is the record of the creation requests this process wrote, by host socket, owner and request. It belongs to
// the process, not to one Backend value: a backend made again in the same process (same owner, same or another client) must
// still know what its predecessor wrote, or it would take a request that is pending on an open connection for one that
// never left. The entries are a few words each and live as long as the process.
var (
	sentMu        sync.Mutex
	sentCreations = map[string]sentRecord{}
)

func (b *Backend) sentKey(owner, callID string, send int) string {
	return b.client.SocketPath() + "|" + owner + "|" + creationKey(callID, send)
}

// sendCreation sends a thread/start or thread/fork and records the connection its frame went out on, before the frame is
// written. A request that was never written leaves no record: the host certainly did not receive it.
func (b *Backend) sendCreation(ctx context.Context, callID string, send int, method string, params map[string]any) (json.RawMessage, error) {
	key := b.sentKey(b.cfg.Owner, callID, send)
	ctx = appserver.WithSendHook(ctx, func(sent string) {
		// the hook also sees the handshake's own request when the call has to connect first
		if sent != method {
			return
		}
		sentMu.Lock()
		sentCreations[key] = sentRecord{client: b.client, conn: b.client.ConnectionID()}
		sentMu.Unlock()
	})
	return b.client.Call(ctx, method, params)
}

// Source implements engine.ReconHost: the source kind the backend connects as and lists threads by.
func (b *Backend) Source() string { return b.cfg.Source }

// Owner implements engine.Owned: the name of the process this backend lives in, written on every creation intent.
func (b *Backend) Owner() string { return b.cfg.Owner }

// CreationState implements engine.ReconHost: what is known about the creation request of a call with a send ordinal, which
// the process owner wrote. A request of this process is judged by the connection it went out on; a request of another
// process by whether that process is still alive (a dead process holds no connection).
func (b *Backend) CreationState(owner, callID string, send int) engine.Creation {
	if owner == "" {
		return engine.CreationUnknown
	}
	if owner == b.cfg.Owner {
		sentMu.Lock()
		rec, written := sentCreations[b.sentKey(owner, callID, send)]
		sentMu.Unlock()
		switch {
		case !written:
			return engine.CreationUnsent
		case rec.conn != 0 && rec.client.ConnectionID() == rec.conn:
			return engine.CreationOpen
		}
		return engine.CreationClosed
	}
	if b.cfg.OwnerAlive(owner) {
		return engine.CreationOpen
	}
	return engine.CreationClosed
}

// ProcessOwner is the Owner of the current process: its pid and its start time on the kernel's clock where the system
// gives it, so that a pid that was reused is not mistaken for the process that wrote an intent.
func ProcessOwner() string {
	pid := os.Getpid()
	return fmt.Sprintf("pid:%d:%d", pid, processStart(pid))
}

// processStart is the start time field of /proc/<pid>/stat (clock ticks since boot), 0 where the system has no /proc.
func processStart(pid int) int64 {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// the command name is in parentheses and may hold spaces; the fields after it start at the state
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return 0
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return start
}

// ProcessAlive is the default OwnerAlive: whether the process an owner names can still exist. An owner it cannot read,
// and a pid it cannot prove gone, are alive: a creation is sent again only on proof.
func ProcessAlive(owner string) bool {
	parts := strings.Split(owner, ":")
	if len(parts) != 3 || parts[0] != "pid" {
		return true
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return true
	}
	if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
		return false
	} else if err != nil && err != syscall.EPERM {
		return true
	}
	want, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || want == 0 {
		return true
	}
	// the pid exists: it is the same process only if it started at the same time
	if got := processStart(pid); got != 0 && got != want {
		return false
	}
	return true
}
