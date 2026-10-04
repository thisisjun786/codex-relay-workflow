//go:build !linux

package dagsched

import (
	"context"
	"errors"
)

func pinnedInputPublish(context.Context, string, string, string, []string) error {
	return errors.New("retaining input snapshots requires Linux authorized artifact I/O")
}
