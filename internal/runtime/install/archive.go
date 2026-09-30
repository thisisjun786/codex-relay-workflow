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

// maxUnpacked bounds what an archive may unpack to, in any one entry and in all of them
// together (MaxArchiveBytes; the compressed archive is bounded by it too). It is a variable only
// so that a test can reach the total without writing that many bytes.
var maxUnpacked int64 = MaxArchiveBytes

// Binary is the multi-call binary's name inside an archive and in <runtime>/bin.
const Binary = "crw"

// archiveName is GoReleaser's name_template: crw_<version>_<os>_<arch>.tar.gz.
var archiveName = regexp.MustCompile(`^crw_([0-9A-Za-z][0-9A-Za-z.+~-]*)_([a-z0-9]+)_([a-z0-9]+)\.tar\.gz$`)

// releaseVersion is the version --release <tag> names once a leading v is taken off: the
// version archiveName carries, so a tag never holds a path separator or a dot segment and names
// nothing but one release's archive.
var releaseVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~-]*$`)

// fetchedArchive is the fixed name a fetched archive is written under in the run's scratch
// directory: the tag chooses what is requested, never where anything is written.
const fetchedArchive = "archive.tar.gz"

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
		// Checked before anything is created, fetched or named after it.
		version := strings.TrimPrefix(source.Release, "v")
		if !releaseVersion.MatchString(version) {
			return Archive{}, done, refuse("--release %q is not a release tag (v<version>, the version in the archive name's grammar: letters, digits, '.', '+', '~' and '-', starting with a letter or digit); nothing was fetched", source.Release)
		}
		scratch, err := os.MkdirTemp("", "crw-install-release-")
		if err != nil {
			return Archive{}, done, err
		}
		done = func() { _ = os.RemoveAll(scratch) }
		base := source.BaseURL
		if base == "" {
			base = ReleaseURL
		}
		name := "crw_" + version + "_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
		for _, asset := range []struct{ remote, local string }{{SumsName, SumsName}, {name, fetchedArchive}} {
			if err := fetch(ctx, strings.TrimSuffix(base, "/")+"/"+source.Release+"/"+asset.remote, filepath.Join(scratch, asset.local)); err != nil {
				return Archive{}, done, refuse("the release asset %s could not be fetched: %v", asset.remote, err)
			}
		}
		archive, err := read(filepath.Join(scratch, fetchedArchive), name, filepath.Join(scratch, SumsName))
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
		archive, err := read(from, filepath.Base(from), sums)
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

// read verifies the archive at archivePath under the name it was published as.
func read(archivePath, name, sumsPath string) (Archive, error) {
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

// reservedName is why an archive entry's name (cleaned, slash-separated, relative) is one the
// installer or the doctor reads as control data inside a runtime directory, or "" when it is
// not. Unpacking such an entry would plant that data rather than a release file:
//
//   - bin/ is the installer's own: bin/crw and the three compatibility links are placed from the
//     archive's crw, and the doctor reads bin/crw, the links and bin/python* there;
//   - a component beginning .crw- is installer control data at any level: the staging lock
//     (.crw-staging-lock), the claim (.crw-staging-claim.json), the claim's .crw-lock sidecar
//     and the claim's atomic-write temporaries (.crw-write-*);
//   - a component ending .crw-lock is the O_EXCL lock sidecar of the file beside it
//     (decision 33), which a writer of that file would wait on and read as another run.
//
// Names are compared case-insensitively, because a case-insensitive filesystem makes .CRW-LOCK
// the same file.
func reservedName(name string) string {
	parts := strings.Split(strings.ToLower(name), "/")
	if parts[0] == "bin" {
		return "bin/ is where the installer places crw and its three compatibility links itself"
	}
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, ".crw-"):
			return "a name beginning .crw- is the installer's staging lock, claim, claim lock or claim write"
		case strings.HasSuffix(part, ".crw-lock"):
			return "a name ending .crw-lock is the lock sidecar of the file beside it"
		}
	}
	return ""
}

// Unpack writes the verified bytes into environment: the binary into bin/crw, the three
// compatibility names as symlinks to it (the archive's own entries for them are checked, never
// trusted), and every other regular file (the licences) at its relative path. Every entry is
// judged before anything is written: an absolute or escaping name, a name the installer or the
// doctor reads as control data (reservedName), a link to anything but crw, a hard link, a
// device, a repeated name, an entry declaring more than MaxArchiveBytes (or entries declaring
// more in all), or a body that is not exactly the size its header declares refuses the whole
// archive with nothing written. Nothing is ever truncated to a bound.
func (a Archive) Unpack(environment string) error {
	links := map[string]bool{}
	for _, name := range definition.Links() {
		links[name] = true
	}
	// entries walks the archive, handing each entry that is not a directory to visit with its
	// cleaned name.
	entries := func(visit func(name string, header *tar.Header, body io.Reader) error) error {
		compressed, err := gzip.NewReader(bytes.NewReader(a.bytes))
		if err != nil {
			return err
		}
		reader := tar.NewReader(compressed)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			name := path.Clean(strings.TrimPrefix(header.Name, "./"))
			if name == "." {
				continue
			}
			if err := visit(name, header, reader); err != nil {
				return err
			}
		}
	}
	seen := map[string]bool{}
	var total int64
	judged := entries(func(name string, header *tar.Header, body io.Reader) error {
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("the archive names %q, outside the directory it is unpacked into", header.Name)
		}
		if seen[name] {
			return fmt.Errorf("the archive names %q twice", name)
		}
		seen[name] = true
		if name != Binary && !links[name] {
			if why := reservedName(name); why != "" {
				return fmt.Errorf("the archive carries %q, a name the installer or the doctor reads as control data (%s); nothing was unpacked", header.Name, why)
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
		case tar.TypeReg:
			// Sizes are judged here, before anything is written: an entry is never truncated to
			// the bound, and its body must be exactly what its header declares.
			if header.Size > maxUnpacked {
				return fmt.Errorf("the archive's %q declares %d bytes, more than the %d one entry may hold; nothing was unpacked", header.Name, header.Size, maxUnpacked)
			}
			if total += header.Size; total > maxUnpacked {
				return fmt.Errorf("the archive's entries declare %d bytes in all by %q, more than the %d an archive may unpack to; nothing was unpacked", total, header.Name, maxUnpacked)
			}
			if err := exactly(body, header.Size); err != nil {
				return fmt.Errorf("the archive's %q %v; nothing was unpacked", header.Name, err)
			}
		case tar.TypeSymlink:
			if !links[name] || header.Linkname != Binary {
				return fmt.Errorf("the archive carries a symbolic link %s -> %s; only %s -> %s are expected", name, header.Linkname, strings.Join(definition.Links(), ", "), Binary)
			}
		default:
			return fmt.Errorf("the archive carries %q of a kind that is not a regular file, a directory or a compatibility link", name)
		}
		return nil
	})
	if judged != nil {
		return judged
	}
	bin := filepath.Join(environment, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	pending := map[string][]byte{}
	var binary []byte
	unpacked := entries(func(name string, header *tar.Header, body io.Reader) error {
		if header.Typeflag != tar.TypeReg {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(body, header.Size+1))
		if err != nil {
			return err
		}
		if int64(len(raw)) != header.Size {
			return fmt.Errorf("the archive's %q holds %d bytes where it held %d when it was judged", header.Name, len(raw), header.Size)
		}
		switch {
		case name == Binary:
			binary = raw
		case links[name]:
			// A dereferenced compatibility name: it must be the binary's own bytes.
			pending[name] = raw
		default:
			return writeFile(filepath.Join(environment, filepath.FromSlash(name)), raw, 0o644)
		}
		return nil
	})
	if unpacked != nil {
		return unpacked
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

// exactly reads an entry's body through, and says how it differs from the size its header
// declares: fewer bytes (the archive ends inside it, or a read fails), or more.
func exactly(body io.Reader, size int64) error {
	n, err := io.CopyN(io.Discard, body, size)
	if err != nil {
		return fmt.Errorf("holds %d of the %d bytes its header declares (%v)", n, size, err)
	}
	var more [1]byte
	switch m, err := body.Read(more[:]); {
	case m > 0:
		return fmt.Errorf("holds more than the %d bytes its header declares", size)
	case !errors.Is(err, io.EOF):
		return fmt.Errorf("could not be read to its end after the %d bytes its header declares (%v)", size, err)
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
