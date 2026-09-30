package install

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
)

// look is one look at a record a write decides on: whether anything is there and, when it is,
// what - its device, inode, size, modification time and bytes, all through one descriptor. A
// write lands only on the document it was decided from: the look taken before the decision is
// compared, under the lock the write acts under, with a look taken there, and any difference
// (another file renamed in, a rewrite in place, bytes of the same length) is a document the
// decision never saw.
type look struct {
	present  bool
	failed   string
	dev, ino uint64
	size     int64
	mtime    int64
	raw      []byte
}

// lookAt is one look at path. An absent path is a look at nothing; one that cannot be opened as
// a regular file or read is a failed look, which never agrees with anything.
func lookAt(path string) look {
	file, err := reading.OpenRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return look{}
	}
	if err != nil {
		return look{present: true, failed: store.PythonOSError(err)}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return look{present: true, failed: store.PythonOSError(err)}
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return look{present: true, failed: store.PythonOSError(err)}
	}
	out := look{present: true, size: info.Size(), mtime: info.ModTime().UnixNano(), raw: raw}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		out.dev, out.ino = uint64(sys.Dev), sys.Ino
	}
	return out
}

// same is whether two looks saw the same document: both at nothing, or both at the same file
// with the same size, modification time and bytes.
func (l look) same(other look) bool {
	if l.failed != "" || other.failed != "" || l.present != other.present {
		return false
	}
	return !l.present || l.dev == other.dev && l.ino == other.ino && l.size == other.size && l.mtime == other.mtime && bytes.Equal(l.raw, other.raw)
}

// beforeWriteLock runs between a write's decision and the lock it acts under, with the path it
// is about to lock. It is a variable only so that a test can change the document there, as
// another writer can.
var beforeWriteLock = func(string) {}
