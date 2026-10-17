package executor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const lockLeaf = ".executor.lock"

func openSecureDir(path string) (int, error) {
	if !filepath.IsAbs(path) {
		return -1, errInvalid(CodeSchemaInvalid, "spool", "the spool path is not absolute")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, seg := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if seg == "" {
			continue
		}
		nfd, err := unix.Openat(fd, seg, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("the spool segment %q: %w", seg, err)
		}
		fd = nfd
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if st.Uid != uint32(os.Getuid()) || st.Mode&0o777 != 0o700 {
		unix.Close(fd)
		return -1, errInvalid(CodeSchemaInvalid, "spool", "the spool is not a private directory of this owner")
	}
	return fd, nil
}

func openRelDir(dirfd int, rel string) (int, error) {
	segs := strings.Split(rel, "/")
	fd := dirfd
	for _, seg := range segs[:len(segs)-1] {
		nfd, err := unix.Openat(fd, seg, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if fd != dirfd {
			unix.Close(fd)
		}
		if err != nil {
			return -1, fmt.Errorf("the directory segment %q: %w", seg, err)
		}
		fd = nfd
	}
	return fd, nil
}

func leafOf(rel string) string {
	return rel[strings.LastIndexByte(rel, '/')+1:]
}

func readLeafFile(dirfd int, rel string, max int64) ([]byte, error) {
	pfd, err := openRelDir(dirfd, rel)
	if err != nil {
		return nil, err
	}
	if pfd != dirfd {
		defer unix.Close(pfd)
	}
	fd, err := unix.Openat(pfd, leafOf(rel), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("the leaf %q: %w", rel, err)
	}
	f := os.NewFile(uintptr(fd), rel)
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		return nil, errInvalid(CodeArtifactInvalid, rel, "not a regular file of this owner held once")
	}
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errInvalid(CodeArtifactInvalid, rel, "the file is over the byte limit")
	}
	return body, nil
}

func ReadArtifact(spool string, artifact Artifact, limits Limits) ([]byte, error) {
	if err := checkLimits(limits); err != nil {
		return nil, err
	}
	dirfd, err := openSecureDir(spool)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirfd)
	return readArtifactAt(dirfd, artifact, limits)
}

func readArtifactAt(dirfd int, artifact Artifact, limits Limits) ([]byte, error) {
	if err := validateArtifact(artifact, "artifact"); err != nil {
		return nil, err
	}
	body, err := readLeafFile(dirfd, artifact.Path, limits.ArtifactBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != artifact.Size {
		return nil, errInvalid(CodeArtifactInvalid, artifact.Path, "the file size does not match the receipt")
	}
	if Digest(body) != artifact.SHA256 {
		return nil, errInvalid(CodeArtifactInvalid, artifact.Path, "the file does not hash to the receipt")
	}
	return body, nil
}

func validateArtifactsAt(dirfd int, r Result, limits Limits) error {
	if len(r.Payload.Artifacts) > limits.ArtifactCount {
		return errInvalid(CodeArtifactInvalid, "payload.artifacts", "over the artifact count")
	}
	var total int64
	for _, art := range r.Payload.Artifacts {
		if art.Size > limits.ArtifactBytes || total > limits.TotalArtifactBytes-art.Size {
			return errInvalid(CodeArtifactInvalid, "payload.artifacts", "over the artifact byte limits")
		}
		total += art.Size
		if _, err := readArtifactAt(dirfd, art, limits); err != nil {
			return err
		}
	}
	return nil
}

func Receive(a Admission, spool, resultPath string, peer Peer, limits Limits) (Result, string, error) {
	if err := checkLimits(limits); err != nil {
		return Result{}, "", err
	}
	if err := ValidateAdmission(a); err != nil {
		return Result{}, "", err
	}
	if !validID(peer.HostID) || !validID(peer.Incarnation) || peer.HostID != a.Task.Executor.HostID || peer.Incarnation != a.Task.Executor.Incarnation {
		return Result{}, "", errInvalid(CodeHostMismatch, "peer", "the delivery is not from the admitted host and incarnation")
	}
	if !validArtifactPath(resultPath) {
		return Result{}, "", errInvalid(CodeSchemaInvalid, "result_path", "unsafe path "+resultPath)
	}
	dirfd, err := openSecureDir(spool)
	if err != nil {
		return Result{}, "", err
	}
	defer unix.Close(dirfd)
	raw, err := readLeafFile(dirfd, resultPath, limits.EnvelopeBytes)
	if err != nil {
		return Result{}, "", err
	}
	r, err := DecodeResult(a, raw, peer, limits)
	if err != nil {
		return Result{}, "", err
	}
	if err := validateArtifactsAt(dirfd, r, limits); err != nil {
		return Result{}, "", err
	}
	return r, Digest(raw), nil
}

func WriteEnvelope(spool, name string, raw []byte, limits Limits) error {
	if err := checkLimits(limits); err != nil {
		return err
	}
	if !validArtifactPath(name) || strings.Contains(name, "/") {
		return errInvalid(CodeSchemaInvalid, "name", "the envelope name is not a leaf")
	}
	if int64(len(raw)) > limits.EnvelopeBytes {
		return errInvalid(CodeSchemaInvalid, "bytes", "the envelope is over the byte limit")
	}
	var doc any
	if err := decodeStrict(raw, limits, &doc); err != nil {
		return err
	}
	dirfd, err := openSecureDir(spool)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	lfd, err := unix.Openat(dirfd, lockLeaf, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	lk := os.NewFile(uintptr(lfd), lockLeaf)
	defer lk.Close()
	info, err := lk.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 {
		return errInvalid(CodeSchemaInvalid, lockLeaf, "the lock is not a regular file of this owner held once")
	}
	if err := unix.Flock(lfd, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(lfd, unix.LOCK_UN)
	if old, err := readLeafFile(dirfd, name, limits.EnvelopeBytes); err == nil {
		if string(old) == string(raw) {
			return nil
		}
		return errInvalid(CodeSchemaInvalid, name, "the envelope exists with other bytes")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	rnd := make([]byte, 8)
	if _, err := rand.Read(rnd); err != nil {
		return err
	}
	tmp := "." + name + ".tmp-" + hex.EncodeToString(rnd)
	tfd, err := unix.Openat(dirfd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	tf := os.NewFile(uintptr(tfd), tmp)
	err = func() error {
		if _, err := tf.Write(raw); err != nil {
			return err
		}
		if err := tf.Sync(); err != nil {
			return err
		}
		return tf.Close()
	}()
	if err != nil {
		tf.Close()
		unix.Unlinkat(dirfd, tmp, 0)
		return err
	}
	if err := unix.Renameat(dirfd, tmp, dirfd, name); err != nil {
		unix.Unlinkat(dirfd, tmp, 0)
		return err
	}
	return unix.Fsync(dirfd)
}
