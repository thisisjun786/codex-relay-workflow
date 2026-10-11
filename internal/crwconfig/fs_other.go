//go:build !linux && !darwin

package crwconfig

import "errors"

// statfsHost has no answer on this platform: the file system is reported unknown.
func statfsHost(string) (FS, error) {
	return FS{}, errors.New("file system type and free space are not read on this platform")
}
