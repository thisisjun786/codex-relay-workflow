package install

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
)

// SumsName is the checksum file a release publishes beside its archives (.goreleaser.yaml).
const SumsName = "SHA256SUMS"

// ReleaseURL is where `--release <tag>` fetches the release's assets from.
const ReleaseURL = "https://github.com/thisisjun786/codex-relay-workflow/releases/download"

// MaxArchiveBytes bounds what is read into memory: the archive is verified and unpacked from
// the same bytes, so nothing can change between the digest and the unpack.
const MaxArchiveBytes = 256 << 20

// Binary is the multi-call binary's name inside an archive and in <runtime>/bin.
const Binary = "crw"

// archiveName is GoReleaser's name_template: crw_<version>_<os>_<arch>.tar.gz.
var archiveName = regexp.MustCompile(`^crw_([0-9A-Za-z][0-9A-Za-z.+~-]*)_([a-z0-9]+)_([a-z0-9]+)\.tar\.gz$`)

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Archive is a release archive read and verified against its SHA256SUMS.
type Archive struct {
	Path, Name, Version, Target, Digest, Sums string
	bytes                                     []byte
}

// Object is the archive as a report shows it.
func (a Archive) Object() Object {
	return Object{{Key: "path", Value: a.Path}, {Key: "name", Value: a.Name}, {Key: "version", Value: a.Version},
		{Key: "target", Value: a.Target}, {Key: "sha256", Value: a.Digest}, {Key: "sums", Value: a.Sums}}
}

// Source names where the archive comes from: a file (with its SHA256SUMS beside it unless Sums
// names one), or a release tag fetched from ReleaseURL (or BaseURL).
type Source struct {
	From, Sums, Release, BaseURL string
}

// refusal is a source that could not be used, before anything was created.
type refusal struct{ detail string }

func (r *refusal) Error() string { return r.detail }

func refuse(format string, args ...any) error { return &refusal{fmt.Sprintf(format, args...)} }

// Resolve reads the archive a source names and verifies it against its SHA256SUMS: the archive
// must be named for this host's target, listed exactly once, and hash to the listed digest.
// Nothing under the destination is created before this answers.
func Resolve(ctx context.Context, source Source) (Archive, func(), error) {
	done := func() {}
	switch {
	case source.From != "" && source.Release != "":
		return Archive{}, done, refuse("--from and --release name two sources; give one")
	case source.Release != "":
		scratch, err := os.MkdirTemp("", "crw-install-release-")
		if err != nil {
			return Archive{}, done, err
		}
		done = func() { _ = os.RemoveAll(scratch) }
		base := source.BaseURL
		if base == "" {
			base = ReleaseURL
		}
		version := strings.TrimPrefix(source.Release, "v")
		name := "crw_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
		for _, asset := range []string{SumsName, name} {
			if err := fetch(ctx, strings.TrimSuffix(base, "/")+"/"+source.Release+"/"+asset, filepath.Join(scratch, asset)); err != nil {
				return Archive{}, done, refuse("the release asset %s could not be fetched: %v", asset, err)
			}
		}
		archive, err := read(filepath.Join(scratch, name), filepath.Join(scratch, SumsName))
		return archive, done, err
	case source.From != "":
		from, err := filepath.Abs(source.From)
		if err != nil {
			return Archive{}, done, err
		}
		sums := source.Sums
		if sums == "" {
			sums = filepath.Join(filepath.Dir(from), SumsName)
		}
		archive, err := read(from, sums)
		return archive, done, err
	}
	return Archive{}, done, refuse("name the archive with --from <crw_<version>_<os>_<arch>.tar.gz> or a release with --release <tag>")
}

func fetch(ctx context.Context, url, target string) error {
	run, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(run, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, response.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxArchiveBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > MaxArchiveBytes {
		return fmt.Errorf("%s is larger than %d bytes", url, MaxArchiveBytes)
	}
	return os.WriteFile(target, raw, 0o600)
}

func read(archivePath, sumsPath string) (Archive, error) {
	name := filepath.Base(archivePath)
	match := archiveName.FindStringSubmatch(name)
	if match == nil {
		return Archive{}, refuse("%s is not named crw_<version>_<os>_<arch>.tar.gz, so its version and target are unknown", name)
	}
	target := match[2] + "/" + match[3]
	if host := runtime.GOOS + "/" + runtime.GOARCH; target != host {
		return Archive{}, refuse("%s is built for %s and this host is %s", name, target, host)
	}
	listed, err := sumsFor(sumsPath, name)
	if err != nil {
		return Archive{}, err
	}
	raw, err := readBounded(archivePath)
	if err != nil {
		return Archive{}, refuse("the archive %s could not be read: %s", archivePath, store.PythonOSError(err))
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	if digest != listed {
		return Archive{}, refuse("%s hashes to %s and %s lists %s for it, so it is not the archive that was released; nothing was unpacked", archivePath, digest, sumsPath, listed)
	}
	return Archive{Path: archivePath, Name: name, Version: match[1], Target: target, Digest: digest, Sums: sumsPath, bytes: raw}, nil
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxArchiveBytes {
		return nil, fmt.Errorf("larger than %d bytes", MaxArchiveBytes)
	}
	return raw, nil
}

// sumsFor is the digest SHA256SUMS lists for name: `<64 hex>  <name>` lines (a leading `*`
// marks binary mode). A name listed twice, or not at all, verifies nothing.
func sumsFor(sumsPath, name string) (string, error) {
	raw, err := readBounded(sumsPath)
	if err != nil {
		return "", refuse("the checksum file %s could not be read: %s; an archive is never unpacked unverified", sumsPath, store.PythonOSError(err))
	}
	var found []string
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			found = append(found, strings.ToLower(fields[0]))
		}
	}
	switch {
	case len(found) == 0:
		return "", refuse("%s does not list %s, so it cannot be verified; nothing was unpacked", sumsPath, name)
	case len(found) > 1:
		return "", refuse("%s lists %s %d times, so which digest verifies it is not established; nothing was unpacked", sumsPath, name, len(found))
	case !hexDigest.MatchString(found[0]):
		return "", refuse("%s lists %q for %s, which is not a SHA-256 digest", sumsPath, found[0], name)
	}
	return found[0], nil
}

// Unpack writes the verified bytes into environment: the binary into bin/crw, the three
// compatibility names as symlinks to it (the archive's own entries for them are checked, never
// trusted), and every other regular file (the licences) at its relative path. An absolute or
// escaping name, a link to anything but crw, a hard link, a device or a repeated name refuses
// the whole archive.
func (a Archive) Unpack(environment string) error {
	compressed, err := gzip.NewReader(bytes.NewReader(a.bytes))
	if err != nil {
		return err
	}
	bin := filepath.Join(environment, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	links := map[string]bool{}
	for _, name := range definition.Links() {
		links[name] = true
	}
	seen := map[string]bool{}
	pending := map[string][]byte{}
	var binary []byte
	entries := tar.NewReader(compressed)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("the archive names %q, outside the directory it is unpacked into", header.Name)
		}
		if seen[name] {
			return fmt.Errorf("the archive names %q twice", name)
		}
		seen[name] = true
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink:
			if !links[name] || header.Linkname != Binary {
				return fmt.Errorf("the archive carries a symbolic link %s -> %s; only %s -> %s are expected", name, header.Linkname, strings.Join(definition.Links(), ", "), Binary)
			}
			continue
		case tar.TypeReg:
		default:
			return fmt.Errorf("the archive carries %q of a kind that is not a regular file, a directory or a compatibility link", name)
		}
		body, err := io.ReadAll(io.LimitReader(entries, MaxArchiveBytes+1))
		if err != nil {
			return err
		}
		switch {
		case name == Binary:
			binary = body
		case links[name]:
			// A dereferenced compatibility name: it must be the binary's own bytes.
			pending[name] = body
		default:
			if err := writeFile(filepath.Join(environment, filepath.FromSlash(name)), body, 0o644); err != nil {
				return err
			}
		}
	}
	if len(binary) == 0 {
		return fmt.Errorf("the archive carries no %s binary", Binary)
	}
	for name, body := range pending {
		if !bytes.Equal(body, binary) {
			return fmt.Errorf("the archive's %s is neither a link to %s nor its bytes", name, Binary)
		}
	}
	if err := writeFile(filepath.Join(bin, Binary), binary, 0o755); err != nil {
		return err
	}
	for _, name := range definition.Links() {
		if err := os.Symlink(Binary, filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(target string, body []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
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
