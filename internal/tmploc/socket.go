package tmploc

import (
	"fmt"
	"os"
)

// SocketRoot is the short root every socket directory is made below. It is independent of TMPDIR
// and exists on linux and darwin alike (/dev/shm does not exist on darwin; /run/user exists only on
// linux). It is one fixed place for every run, not a fallback: no run moves here because of its
// TMPDIR, and none leaves it because of one.
const SocketRoot = "/tmp"

// SocketBound is the longest unix socket path every supported platform can bind or dial: darwin
// refuses 104 bytes or more (its sun_path holds 104 with the NUL) and linux 108, so the shared bound
// is 103 bytes. It replaces the per-package copies (fakehost socketPathBound, ci local
// localSocketLimit, laneops integrationSocketLimit). The product's own configured-socket rule
// (crwconfig) is a separate rule.
const SocketBound = 103

// SocketTailMax is the longest path any test puts below a socket directory (the "tail": the part
// after the directory and its separator). It is the longest tail the socket inventory measured
// (CRW-1568): 58 bytes, relative/codex-session-relay/<16 hex>/control.sock below a relay hook test
// home (internal/relay/hook Test33ReviewD8/relative_xdg). tmploctest.SocketPath and
// tmploctest.CheckSocket fail any tail over it, so a tail that grows fails in every run, even while
// it still fits SocketBound; a test that needs a longer one raises it here.
const SocketTailMax = 58

// socketRoot is SocketRoot, held in a variable so the package's own tests can point it at an
// unusable root. Nothing else changes it.
var socketRoot = SocketRoot

// SocketDir makes a fresh 0700 directory below SocketRoot, named by pattern as os.MkdirTemp does.
// It fails with socket_dir_unusable and the reason when the root cannot hold the directory, or when
// the directory leaves no room for a SocketTailMax tail within SocketBound (the directory is then
// removed). There is no second root.
func SocketDir(pattern string) (string, error) {
	root := socketRoot
	dir, err := os.MkdirTemp(root, pattern)
	if err != nil {
		return "", fmt.Errorf("socket_dir_unusable: %s: %w", root, err)
	}
	if err := SocketFits(dir, SocketTailMax); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("socket_dir_unusable: %s: %w", root, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("socket_dir_unusable: %s: %w", root, err)
	}
	return dir, nil
}

// SocketFits reports whether a socket path of dir, a separator and a tail of tail bytes fits
// SocketBound. It is the one socket budget calculation in the repository; a refusal is
// socket_path_too_long with every number in it.
func SocketFits(dir string, tail int) error {
	if tail < 0 {
		return fmt.Errorf("socket_path_too_long: tail %d below %s is negative", tail, dir)
	}
	needed := len(dir) + 1 + tail
	if needed > SocketBound {
		return fmt.Errorf("socket_path_too_long: %s (%d bytes) + 1 + tail %d = %d bytes, over the %d-byte bound of a unix socket path by %d",
			dir, len(dir), tail, needed, SocketBound, needed-SocketBound)
	}
	return nil
}
