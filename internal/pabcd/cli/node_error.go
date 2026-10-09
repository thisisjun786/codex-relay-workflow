package cli

import (
	"errors"
	"os"
	"syscall"
)

// nodeErrorMessage is the oracle's Error.message for err. The Node runtime spells a filesystem
// failure whose errno planErrno names as "<NAME>: <description>, <op> '<path>'", with the path left
// out for write and close, and "<NAME>: <description>, <op> '<old>' -> '<new>'" for a rename (*os.LinkError); anything else keeps err.Error(). divergence-cli.ts:153 prints err.message
// verbatim for a failed candidate save, and plan-cli.ts's uncaught failures carry the same text, so
// planFailure and the divergence candidate add branch share this conversion.
func nodeErrorMessage(err error) string {
	output := err.Error()
	var path *os.PathError
	var link *os.LinkError
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return output
	}
	name, desc := planErrno(errno)
	if name == "" {
		return output
	}
	switch {
	case errors.As(err, &path):
		output = name + ": " + desc + ", " + path.Op
		if path.Op != "write" && path.Op != "close" {
			output += " '" + path.Path + "'"
		}
	case errors.As(err, &link):
		// A rename, link or symlink names both ends: Node's "rename 'old' -> 'new'" (the divergence mode write ends in a rename).
		output = name + ": " + desc + ", " + link.Op + " '" + link.Old + "' -> '" + link.New + "'"
	}
	return output
}

// nodeSpelledError is err with the text Node prints for it: Error returns the Node spelling and Unwrap keeps the original, so
// errors.Is and errors.As (a cancelled context, the *os.PathError) still answer for the cause.
type nodeSpelledError struct {
	err     error
	message string
}

func (e *nodeSpelledError) Error() string { return e.message }

func (e *nodeSpelledError) Unwrap() error { return e.err }

// nodeSpelled is err carrying nodeErrorMessage(err) as its text; nil stays nil and an error whose text is already the Node
// spelling is returned as it is.
func nodeSpelled(err error) error {
	if err == nil {
		return nil
	}
	if message := nodeErrorMessage(err); message != err.Error() {
		return &nodeSpelledError{err: err, message: message}
	}
	return err
}
