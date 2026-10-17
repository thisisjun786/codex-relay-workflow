//go:build !linux

package delivery

import (
	"context"
	"fmt"
	"net"
)

func wakeRuntime(context.Context, string, int, ...net.Conn) (WakeIdentity, error) {
	return WakeIdentity{}, fmt.Errorf("wake process incarnation is unmeasured on this platform")
}
