// Package buildinfo identifies the runtime executing a stored operation.
package buildinfo

import (
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Version and Build are stamped by the entry point and linker respectively.
var Version, Build = "dev", ""

// ID is the holder build doctor reports, falling back to the runtime version.
func ID() string {
	if Build != "" {
		return Build
	}
	return Version
}

// Snapshot records the running executable, without rereading an install pointer.
// Failure to locate it is unknown, never the configured executable of a peer.
func Snapshot() contract.OrderedObject {
	var path any
	if executable, err := os.Executable(); err == nil {
		path = pyvalue.FSDecode(executable)
	}
	return contract.OrderedObject{{Key: "build", Value: ID()}, {Key: "executable", Value: path}}
}
