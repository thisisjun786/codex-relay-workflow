package crwdir

import "os"

// Rename publishes tmp as finalPath (atomic-write.ts renameWithRetry on a POSIX platform).
// rename(2) replaces the destination unconditionally, so the win32-only retry is not ported: a
// failed rename is returned after one attempt.
func Rename(tmp, finalPath string) error { return renameWith(os.Rename, tmp, finalPath) }

// renameWith takes the rename call as an argument so a test can count the attempts.
func renameWith(rename func(oldpath, newpath string) error, tmp, finalPath string) error {
	return rename(tmp, finalPath)
}
