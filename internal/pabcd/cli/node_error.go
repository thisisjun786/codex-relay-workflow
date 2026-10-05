package cli

import (
	"errors"
	"os"
	"syscall"
)

// nodeErrorMessage is the oracle's Error.message for err. The Node runtime spells a filesystem
// failure whose errno planErrno names as "<NAME>: <description>, <op> '<path>'", with the path left
// out for write and close; anything else keeps err.Error(). divergence-cli.ts:153 prints err.message
// verbatim for a failed candidate save, and plan-cli.ts's uncaught failures carry the same text, so
// planFailure and the divergence candidate add branch share this conversion.
func nodeErrorMessage(err error) string {
	output := err.Error()
	var path *os.PathError
	var errno syscall.Errno
	if errors.As(err, &path) && errors.As(err, &errno) {
		name, desc := planErrno(errno)
		if name != "" {
			output = name + ": " + desc + ", " + path.Op
			if path.Op != "write" && path.Op != "close" {
				output += " '" + path.Path + "'"
			}
		}
	}
	return output
}
