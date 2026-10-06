package install

import (
	"os"
	"path/filepath"
	"syscall"
)

// writeExecutable writes a file this process will execute, holding syscall.ForkLock for reading
// from before its open until after its last close.
//
// A fork copies every descriptor that is not close-on-exec into the child, and the child keeps it
// until it execs. A path a fork inherited that way is open for writing in another process, and
// Linux refuses to execute a file open for writing with ETXTBSY, "text file busy"
// (golang/go#22315). os/exec forks under this same lock for writing (syscall/exec_unix.go) and
// os.Pipe takes it for reading while it makes its descriptors (os/pipe_unix.go), so holding it
// across the write keeps a fork from landing inside it. It is never held across an exec or a
// blocking read.
//
// The install package writes the release binary this way; its tests write the executables they
// then run through it for the same reason.
func writeExecutable(target string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
