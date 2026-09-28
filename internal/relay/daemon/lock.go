// Package daemon owns the bounded relay loop and its permanent-inode single-instance lock.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SingleInstance shares an open file description with supervised workers. Closing
// the supervisor's copy must NEVER unlock the description a worker still holds.
type SingleInstance struct {
	File   *os.File
	shared bool
}

func Acquire(directory string, shared bool, adopt *int) (*SingleInstance, error) {
	path := filepath.Join(directory, "daemon.lock")
	if adopt != nil {
		return &SingleInstance{File: os.NewFile(uintptr(*adopt), path), shared: true}, nil
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another relay daemon already holds %s", path)
	}
	lock := &SingleInstance{File: f, shared: shared}
	if err = f.Truncate(0); err == nil {
		_, err = fmt.Fprint(f, os.Getpid())
	}
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}
func (l *SingleInstance) Close() error {
	if l.File == nil {
		return nil
	}
	var err error
	if !l.shared {
		err = unix.Flock(int(l.File.Fd()), unix.LOCK_UN)
	}
	closeErr := l.File.Close()
	l.File = nil
	if err != nil {
		return err
	}
	return closeErr
}
