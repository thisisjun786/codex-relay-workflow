package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// record is the installed record crw writes beside the executable: the tool's name, its version,
// the archive's sha256 and when the archive was fetched. It is what makes a directory an install.
type record struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	FetchedAt string `json:"fetched_at"`
}

// installedPath answers the installed executable's path, or not_installed. A directory without a
// record that names this pin, or without the executable, is not an install.
func installedPath(pin Pin, toolsRoot string) (string, error) {
	path := pin.ExecutablePath(toolsRoot)
	raw, err := os.ReadFile(pin.RecordPath(toolsRoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s", pin.Name, pin.Version, toolsRoot)
		}
		return "", hostFail("the record %s could not be read: %v", pin.RecordPath(toolsRoot), err)
	}
	var found record
	if err := json.Unmarshal(raw, &found); err != nil {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: %s is not readable", pin.Name, pin.Version, toolsRoot, pin.RecordPath(toolsRoot))
	}
	if found.Name != pin.Name || found.Version != pin.Version || found.SHA256 != pin.SHA256 {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the record does not name this pin", pin.Name, pin.Version, toolsRoot)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the executable is missing", pin.Name, pin.Version, toolsRoot)
	}
	// An executable that lost its execute bits is not usable as the pinned tool, so it is not an
	// install: install repairs it rather than handing back a path that cannot be run.
	if info.Mode().Perm()&0o111 == 0 {
		return "", refuse("not_installed", notInstalledExit, "%s %s is not installed under %s: the executable is not executable", pin.Name, pin.Version, toolsRoot)
	}
	return path, nil
}

// install makes the pin installed under toolsRoot, fetching nothing when it already is. A digest
// that is not the pin's is refused with nothing left under toolsRoot: the archive is verified in
// memory before the tools root is written at all.
func install(ctx context.Context, pin Pin, seams *Seams, toolsRoot, tempRoot string) (string, error) {
	// The platform rule is checked before the installed-path fast path, so a tools root shared or
	// restored from another host never hands back a binary this host cannot run.
	goos, goarch := seams.platform()
	if !pin.Supports(goos, goarch) {
		return "", refuse("unsupported_platform", usageExit, "%s has no archive for %s", pin.describe(), platformName(goos, goarch))
	}
	if path, err := installedPath(pin, toolsRoot); err == nil {
		return path, nil
	}
	archive, err := fetch(ctx, pin, seams, tempRoot)
	if err != nil {
		return "", err
	}
	// The first interrupt must be honoured before anything durable happens: the bytes are in
	// memory and nothing has been written under the tools root yet.
	if err := ctx.Err(); err != nil {
		return "", hostFail("the install was cancelled: %v", err)
	}
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if digest != pin.SHA256 {
		return "", refuse("digest_mismatch", digestMismatchExit, "the archive %s has sha256 %s, not the pinned %s", pin.Archive, digest, pin.SHA256)
	}
	body, err := member(archive, pin.Executable)
	if err != nil {
		return "", err
	}
	return stage(pin, seams, toolsRoot, body)
}

// fetch downloads the pin's archive into a fresh directory under tempRoot and returns its bytes.
// The directory is removed whatever the answer, so a failed or refused download leaves nothing.
func fetch(ctx context.Context, pin Pin, seams *Seams, tempRoot string) ([]byte, error) {
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return nil, hostFail("the download root %s could not be made: %v", tempRoot, err)
	}
	dir, err := os.MkdirTemp(tempRoot, pin.Name+"-")
	if err != nil {
		return nil, hostFail("the download directory under %s could not be made: %v", tempRoot, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	run, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	url := seams.urlBase() + "/v" + pin.Version + "/" + pin.Archive
	request, err := http.NewRequestWithContext(run, http.MethodGet, url, nil)
	if err != nil {
		return nil, hostFail("%s could not be requested: %v", url, err)
	}
	response, err := seams.client().Do(request)
	if err != nil {
		return nil, hostFail("%s could not be fetched: %v", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, hostFail("%s answered %s", url, response.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, hostFail("%s could not be read: %v", url, err)
	}
	if int64(len(raw)) > maxArchiveBytes {
		return nil, hostFail("%s is larger than %d bytes", url, maxArchiveBytes)
	}
	return raw, nil
}

// member reads one regular file out of a tar.gz in memory. A member the archive does not carry, a
// name that is not the one asked for, or an entry that is not a regular file is refused.
func member(archive []byte, name string) ([]byte, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, hostFail("the archive is not a gzip stream: %v", err)
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, hostFail("the archive could not be read: %v", err)
		}
		if filepath.Base(header.Name) != name {
			continue
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, hostFail("the archive's %s is not a regular file", name)
		}
		body, err := io.ReadAll(io.LimitReader(reader, maxArchiveBytes+1))
		if err != nil {
			return nil, hostFail("the archive's %s could not be read: %v", name, err)
		}
		if int64(len(body)) > maxArchiveBytes {
			return nil, hostFail("the archive's %s is larger than %d bytes", name, maxArchiveBytes)
		}
		return body, nil
	}
	return nil, hostFail("the archive does not carry %s", name)
}

// stage writes the executable and its record into a fresh directory under toolsRoot and renames
// it into place, so the destination never holds a partial install: the rename is on one
// filesystem because the staging directory is inside the tools root. A failure after the staging
// directory exists removes it.
func stage(pin Pin, seams *Seams, toolsRoot string, body []byte) (string, error) {
	if err := os.MkdirAll(toolsRoot, 0o755); err != nil {
		return "", hostFail("the tools root %s could not be made: %v", toolsRoot, err)
	}
	dir, err := os.MkdirTemp(toolsRoot, stagingPrefix+pin.Name+"-")
	if err != nil {
		return "", hostFail("a staging directory under %s could not be made: %v", toolsRoot, err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := os.WriteFile(filepath.Join(dir, pin.Executable), body, executableMode); err != nil {
		return "", hostFail("the executable could not be written: %v", err)
	}
	// A restrictive umask can mask the execute bits os.WriteFile asked for, so the mode is set
	// explicitly: a tool the host cannot run is not installed.
	if err := os.Chmod(filepath.Join(dir, pin.Executable), executableMode); err != nil {
		return "", hostFail("the executable's mode could not be set: %v", err)
	}
	// The record is written after the executable, so a directory that holds a record holds a
	// complete install.
	record := record{
		Name: pin.Name, Version: pin.Version, SHA256: pin.SHA256,
		FetchedAt: seams.now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", hostFail("the record could not be written: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, recordFile), append(data, '\n'), recordMode); err != nil {
		return "", hostFail("the record could not be written: %v", err)
	}
	target := pin.InstallDir(toolsRoot)
	moved, err := replace(pin, toolsRoot, dir, target)
	if err != nil {
		return "", err
	}
	// keep is set only when this call's own directory was renamed into place. When another
	// install won the race, the deferred cleanup removes the staged copy rather than leaking it.
	keep = moved
	return pin.ExecutablePath(toolsRoot), nil
}

// replace renames a staged directory into place. A destination that is already this pin's
// install is kept, because another install reached it first and its bytes are the pinned ones. A
// destination that is not (a stale record, a partial install) is removed and the rename retried
// once, so an install repairs what it finds without ever leaving a partial directory under the
// final name.
func replace(pin Pin, toolsRoot, dir, target string) (bool, error) {
	if err := os.Rename(dir, target); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrExist) && !isNotEmpty(err) {
		return false, hostFail("the install directory %s could not be made: %v", target, err)
	}
	if _, err := installedPath(pin, toolsRoot); err == nil {
		// Another install reached the destination first with a complete, matching install; keep
		// it and let the caller's defer remove the staged copy.
		return false, nil
	}
	if err := os.RemoveAll(target); err != nil {
		return false, hostFail("the incomplete install at %s could not be removed: %v", target, err)
	}
	if err := os.Rename(dir, target); err != nil {
		return false, hostFail("the install directory %s could not be made: %v", target, err)
	}
	return true, nil
}

// isNotEmpty reports whether an error is a rename onto a non-empty directory.
func isNotEmpty(err error) bool {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return strings.Contains(linkErr.Err.Error(), "not empty")
	}
	return false
}
