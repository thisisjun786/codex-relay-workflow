//go:build linux

package dagsched

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Only the two snapshot directories are created. Every ancestor of relay state must
// exist and is opened without following links, as in store.UnlinkPinned.
func pinnedInputOpenDirectory(root string) (*os.File, error) {
	if _, err := store.NormalizeDeclaredPath(root); err != nil {
		return nil, err
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(root), "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for i, component := range components {
		next, err := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && i >= len(components)-2 {
			if err = unix.Mkdirat(fd, component, 0o700); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		_ = unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("open retained input directory %s: %w", root, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), root), nil
}

type pinnedInputReader struct {
	ctx  context.Context
	file *os.File
}

func (r pinnedInputReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}

// Publish by linking a complete, synced file, never by writing the digest pathname.
// A concurrent publisher can reuse the winner only after the caller hashes it.
func pinnedInputPublish(ctx context.Context, source, digest, root string, roots []string) (err error) {
	input, err := store.OpenAuthorized(source, roots, false)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	dir, err := pinnedInputOpenDirectory(root)
	if err != nil {
		return err
	}
	defer dir.Close()
	dirfd := int(dir.Fd())
	name := ".pending-" + rand.Text()
	fd, err := unix.Openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() {
		err = errors.Join(err, file.Close())
		if removed := unix.Unlinkat(dirfd, name, 0); removed != nil {
			err = errors.Join(err, removed)
		}
	}()
	hash := sha256.New()
	if _, err = io.Copy(io.MultiWriter(file, hash), pinnedInputReader{ctx, input.File}); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return &store.RefusedError{Reason: store.ReasonManifestUnverified, Detail: source + ": the copied bytes do not hash to " + digest}
	}
	if err = input.VerifyStable(); err != nil {
		return err
	}
	if err = file.Chmod(0o400); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = unix.Linkat(dirfd, name, dirfd, digest, 0); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	return dir.Sync()
}
