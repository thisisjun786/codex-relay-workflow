package pluginversion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"
)

// GitRunner runs git and answers its standard output; stdin is fed to the command when it is not
// nil. It is how a caller keeps the reads of a version on the same view of the objects as the rest
// of its proof: the check that proves a base refresh passes a runner over its own throwaway
// repository, which borrows the objects and has no configuration or replace refs of its own.
type GitRunner func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)

// RevisionPayload is the files a revision ships under the plugin package, by package-relative name,
// with what the installer could not copy faithfully. It reads the revision's tree, never a work tree.
func RevisionPayload(ctx context.Context, repo, revision string) (Payload, []string, error) {
	return RevisionPayloadWith(ctx, repoRunner(repo), revision)
}

// RevisionPayloadWith is RevisionPayload read through run.
func RevisionPayloadWith(ctx context.Context, run GitRunner, revision string) (Payload, []string, error) {
	// --full-tree keeps every path relative to the tree root, so a repository read from a
	// subdirectory names the package's files the same way.
	listing, err := gitText(ctx, run, "ls-tree", "-r", "-z", "--full-tree", revision, "--", PluginRelative)
	if err != nil {
		return nil, nil, err
	}
	result, errs := Payload{}, []string{}
	type request struct{ name, mode, sha string }
	var requests []request
	for _, record := range strings.Split(listing, "\x00") {
		if record == "" {
			continue
		}
		meta, path, _ := strings.Cut(record, "\t")
		fields := strings.SplitN(meta, " ", 3)
		if len(fields) != 3 {
			return nil, nil, fmt.Errorf("git ls-tree wrote %q, which is not a tree entry", record)
		}
		mode, kind, sha := fields[0], fields[1], fields[2]
		parts := pathParts(path)
		if len(parts) < len(pathParts(PluginRelative)) {
			continue
		}
		name := strings.Join(parts[len(pathParts(PluginRelative)):], "/")
		if kind != "blob" {
			errs = append(errs, "release "+path+": the package may not contain a "+kind)
			continue
		}
		if mode == "120000" {
			errs = append(errs, "release "+path+": the installer drops symlinks, so the package may not contain one")
			continue
		}
		requests = append(requests, request{name, mode, sha})
	}
	if len(requests) > 0 {
		shas := make([]string, len(requests))
		for i, r := range requests {
			shas[i] = r.sha
		}
		batch, err := run(ctx, []byte(strings.Join(shas, "\n")), "cat-file", "--batch")
		if err != nil {
			return nil, nil, fmt.Errorf("git cat-file --batch: %w", err)
		}
		offset := 0
		for _, r := range requests {
			headerEnd := offset + bytes.IndexByte(batch[offset:], '\n')
			if headerEnd < offset {
				return nil, nil, errors.New("git cat-file --batch answered a blob without a header")
			}
			fields := bytes.Fields(batch[offset:headerEnd])
			if len(fields) < 3 {
				return nil, nil, errors.New("git cat-file --batch answered a blob header this check cannot read")
			}
			size, err := strconv.Atoi(string(fields[2]))
			if err != nil {
				return nil, nil, fmt.Errorf("git cat-file --batch: %w", err)
			}
			start := headerEnd + 1
			if start+size > len(batch) {
				return nil, nil, errors.New("git cat-file --batch answered fewer bytes than it named")
			}
			result[r.name] = Entry{r.mode, batch[start : start+size]}
			offset = start + size + 1
		}
	}
	return result, errs, nil
}

// DirectoryPayload reads an installed or working-tree plugin directory as the installer would copy
// it, refusing what it cannot copy faithfully.
func DirectoryPayload(pluginRoot string) (Payload, []string) {
	result, errs := Payload{}, []string{}
	filepath.WalkDir(pluginRoot, func(path string, d fs.DirEntry, err error) error {
		if path == pluginRoot {
			if err != nil {
				return filepath.SkipDir
			}
			return nil
		}
		name := filepath.ToSlash(strings.TrimPrefix(path, pluginRoot+string(filepath.Separator)))
		if d != nil && d.Type()&fs.ModeSymlink != 0 {
			errs = append(errs, "installed "+name+": the installer drops symlinks, so the package may not contain one")
			return nil
		}
		if d != nil && d.IsDir() {
			if entries, readErr := os.ReadDir(path); readErr == nil && len(entries) == 0 {
				errs = append(errs, name+": an empty directory still ships; remove it")
			}
			if err != nil {
				return filepath.SkipDir
			}
			return nil
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil
		}
		mode := "100644"
		if info.Mode().Perm()&0o111 != 0 {
			mode = "100755"
		}
		data, readErr := regularBytes(path)
		if readErr != nil {
			errs = append(errs, "installed "+name+" could not be read as a regular file ("+readErr.Error()+
				"); the installer copies files, and this one is not a file it can copy")
			return nil
		}
		result[name] = Entry{mode, data}
		return nil
	})
	return result, errs
}

// regularBytes reads a file through one descriptor opened without blocking and judged a regular
// file, so a pipe cannot hold the check (and a lock its caller holds) open.
func regularBytes(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return io.ReadAll(file)
}

// TreeVersion derives the version that names a commit's plugin payload: the release the manifest
// records, plus the digest of that payload with the recorded suffix elided.
//
// A non-empty reason says the commit's payload cannot name a version at all (the manifest is missing
// or unreadable, or the package holds something the installer could not copy) - a fact about the
// commit, not a failure to read it. err is only a failure to read: git could not answer. A caller
// that decides what to do with such a commit uses reason; one that only wants the version uses
// VersionOfTree.
func TreeVersion(ctx context.Context, run GitRunner, commit string) (version, reason string, err error) {
	p, errs, err := RevisionPayloadWith(ctx, run, commit)
	if err != nil {
		return "", "", err
	}
	if len(errs) > 0 {
		return "", strings.Join(errs, "; "), nil
	}
	recorded, err := ManifestVersion(p)
	if err != nil {
		return "", err.Error(), nil
	}
	derived, err := PayloadVersion(p, recorded)
	if err != nil {
		return "", err.Error(), nil
	}
	return derived, "", nil
}

// VersionOfTree is the version that names the plugin payload of a commit's tree: the version
// crw-dev ci plugin computes for that commit's work tree and --record-version writes.
func VersionOfTree(ctx context.Context, repo, commit string) (string, error) {
	version, reason, err := TreeVersion(ctx, repoRunner(repo), commit)
	if err != nil {
		return "", err
	}
	if reason != "" {
		return "", errors.New(reason)
	}
	return version, nil
}

// repoRunner runs git in repo.
func repoRunner(repo string) GitRunner {
	return func(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
		return gitRun(ctx, repo, stdin, args...)
	}
}

// pathParts is a slash-separated relative path's components, without empty and "." ones.
func pathParts(text string) []string {
	var parts []string
	for _, part := range strings.Split(text, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// gitText runs git through run and answers its standard output as UTF-8 text.
func gitText(ctx context.Context, run GitRunner, args ...string) (string, error) {
	out, err := run(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(out) {
		return "", fmt.Errorf("git %s: %w", args[0], errNotUTF8)
	}
	return string(out), nil
}

// gitRun runs git in repo; a non-zero exit carries git's own words as the error, as the checks read
// them, and stdin is fed to the command when it is not nil.
func gitRun(ctx context.Context, repo string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, errors.New(strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "\ufffd")))
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}
