package adapter

import (
	"net"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func (a *Adapter) WakeConnection() delivery.WakeConnection {
	source, ok := a.rpc.(interface {
		ConnectionSnapshot() (net.Conn, uint64)
		SocketPath() string
	})
	if !ok {
		return delivery.WakeConnection{}
	}
	conn, id := source.ConnectionSnapshot()
	return delivery.WakeConnection{Conn: conn, ID: id, Socket: source.SocketPath()}
}
