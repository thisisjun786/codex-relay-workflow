//go:build !linux

package delivery

import (
	"context"
	"fmt"
)

func wakeRuntime(context.Context, string, int) (WakeIdentity, error) {
	return WakeIdentity{}, fmt.Errorf("wake process incarnation is unmeasured on this platform")
}
