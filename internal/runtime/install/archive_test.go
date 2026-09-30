package install_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
)

// forged is a verified archive (its SHA256SUMS agrees) whose entries are hostile or malformed.
func forged(t *testing.T, name string, entries []*tar.Header, bodies [][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
	w := tar.NewWriter(compressed)
	for i, header := range entries {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	_ = compressed.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	write(t, path, buf.String())
	sum := sha256.Sum256(buf.Bytes())
	write(t, filepath.Join(dir, install.SumsName), hex.EncodeToString(sum[:])+"  "+name+"\n")
	return path
}

// A verified archive is still unpacked only as the release layout: an entry escaping the
// directory, a link to anything but crw, a missing binary or a device refuses the whole
// archive and releases the directory; an archive named for another target is refused before
// anything is created.
func TestUnpackRefusesAnythingButTheReleaseLayout(t *testing.T) {
	name := "crw_0.9.9_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	crw := []byte("\x7fELF not really")
	cases := map[string]struct {
		headers []*tar.Header
		bodies  [][]byte
	}{
		"escaping": {[]*tar.Header{{Name: "crw", Mode: 0o755, Size: int64(len(crw)), Typeflag: tar.TypeReg}, {Name: "../evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}}, [][]byte{crw, []byte("x")}},
		"link":     {[]*tar.Header{{Name: "crw", Mode: 0o755, Size: int64(len(crw)), Typeflag: tar.TypeReg}, {Name: "codex-session-relay", Linkname: "/bin/sh", Typeflag: tar.TypeSymlink}}, [][]byte{crw, nil}},
		"binary":   {[]*tar.Header{{Name: "LICENSE", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}}, [][]byte{[]byte("x")}},
		"device":   {[]*tar.Header{{Name: "crw", Mode: 0o755, Size: int64(len(crw)), Typeflag: tar.TypeReg}, {Name: "null", Typeflag: tar.TypeChar}}, [][]byte{crw, nil}},
		"claim":    {[]*tar.Header{{Name: "crw", Mode: 0o755, Size: int64(len(crw)), Typeflag: tar.TypeReg}, {Name: ".crw-staging-claim.json", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}}, [][]byte{crw, []byte("{}")}},
	}
	for label, c := range cases {
		h := newHost(t)
		path := forged(t, name, c.headers, c.bodies)
		result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: path})
		if code != install.Refused || at(result, "failedStep") != "unpack the archive" || at(result, "retriable") != true {
			t.Fatalf("%s: exit %d\n%s", label, code, golden.Canon(result))
		}
		if entries, _ := os.ReadDir(h.dest); len(entries) != 0 {
			t.Fatalf("%s: left %v", label, entries)
		}
		if _, err := os.Lstat(filepath.Join(filepath.Dir(h.dest), "evil")); !os.IsNotExist(err) {
			t.Fatalf("%s: wrote outside the runtime directory", label)
		}
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	h := newHost(t)
	path := forged(t, "crw_0.9.9_"+runtime.GOOS+"_"+other+".tar.gz", cases["binary"].headers, cases["binary"].bodies)
	if result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: path}); code != install.Refused || !strings.Contains(text(at(result, "refused")), "built for") {
		t.Fatalf("another target: exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Lstat(h.dest); !os.IsNotExist(err) {
		t.Fatal("the destination was created for another target's archive")
	}
}

// --release fetches the release's SHA256SUMS and this target's archive and installs through
// the same verified path as --from.
func TestReleaseAssetsAreFetchedAndVerified(t *testing.T) {
	path := archive(t, "0.9.0", "")
	server := httptest.NewServer(http.StripPrefix("/v0.9.0/", http.FileServer(http.Dir(filepath.Dir(path)))))
	defer server.Close()
	h := newHost(t)
	result, code := install.Install(context.Background(), h.options(), "install", install.Source{Release: "v0.9.0", BaseURL: server.URL})
	if code != install.OK || at(result, "archive", "sha256") != archiveDigest(t, path) {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	missing := newHost(t)
	if _, code := install.Install(context.Background(), missing.options(), "install", install.Source{Release: "v9.9.9", BaseURL: server.URL}); code != install.Refused {
		t.Fatalf("an absent release: exit %d", code)
	}
}

// A verified archive that carries the claim's lock sidecar (which would stall settling the claim
// behind a run that does not exist) or a staging lock is refused at the unpack: nothing is
// promoted, the directory is released and the host record lists no install of it.
func TestAnArchiveCarryingControlDataIsNotInstalled(t *testing.T) {
	raw, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	name := "crw_0.9.9_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	for _, planted := range []string{".crw-staging-lock", ".crw-staging-claim.json.crw-lock"} {
		headers := []*tar.Header{{Name: "crw", Mode: 0o755, Size: int64(len(raw)), Typeflag: tar.TypeReg}}
		bodies := [][]byte{raw}
		for _, link := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
			headers = append(headers, &tar.Header{Name: link, Linkname: "crw", Typeflag: tar.TypeSymlink})
			bodies = append(bodies, nil)
		}
		headers = append(headers, &tar.Header{Name: planted, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		bodies = append(bodies, []byte("1"))
		h := newHost(t)
		result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: forged(t, name, headers, bodies)})
		if code != install.Refused || at(result, "failedStep") != "unpack the archive" || at(result, "retriable") != true || !strings.Contains(golden.Canon(at(result, "steps")), "control data") {
			t.Fatalf("%s: exit %d\n%s", planted, code, golden.Canon(result))
		}
		if entries, _ := os.ReadDir(h.dest); len(entries) != 0 {
			t.Fatalf("%s: left %v", planted, entries)
		}
		if _, err := os.Lstat(pointer.Path(h.dest)); !os.IsNotExist(err) {
			t.Fatalf("%s: the pointer was placed", planted)
		}
		if installs := golden.List(at(h.hostRecord(t), "components", "codex-session-relay", "installs")); len(installs) != 0 {
			t.Fatalf("%s: the record lists %v", planted, installs)
		}
	}
}

// --release <tag> is checked as a tag before anything is created, fetched or named after it: a
// tag holding a path separator, a dot segment or anything outside the archive name's version
// grammar is refused with no request sent, no scratch directory made, and no file outside the
// run's scratch directory written - a tag once chose the local file a fetched asset replaced.
func TestAReleaseTagIsCheckedBeforeAnythingIsFetched(t *testing.T) {
	base := t.TempDir()
	scratch := filepath.Join(base, "tmp")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", scratch)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte("PWNED\n"))
	}))
	defer server.Close()
	victim := filepath.Join(base, "victim_"+runtime.GOOS+"_"+runtime.GOARCH+".tar.gz")
	write(t, victim, "the user's own file\n")
	for _, tag := range []string{"v1/../../../victim", "../victim", "v1/2", "..", "v..", "-v1", "v1 2", "v1%2F..", "v1\\x", "v1\n"} {
		h := newHost(t)
		result, code := install.Install(context.Background(), h.options(), "install", install.Source{Release: tag, BaseURL: server.URL})
		if code != install.Refused || !strings.Contains(text(at(result, "refused")), "is not a release tag") {
			t.Fatalf("%q: exit %d\n%s", tag, code, golden.Canon(result))
		}
		if _, err := os.Lstat(h.dest); !os.IsNotExist(err) {
			t.Fatalf("%q: the destination was created", tag)
		}
	}
	if requests != 0 {
		t.Fatalf("%d requests were sent for tags that are not tags", requests)
	}
	if got := readFile(t, victim); got != "the user's own file\n" {
		t.Fatalf("a file outside the scratch directory was replaced: %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(scratch, "crw-install-release-*")); len(left) != 0 {
		t.Fatalf("a scratch directory was made for a tag that is not a tag: %v", left)
	}
}

// An archive entry whose header declares more than MaxArchiveBytes, or whose body ends before
// the size its header declares, is refused at the unpack with the destination as it was: no
// runtime directory is left, no pointer placed, no install recorded. Nothing is truncated to fit.
func TestAnOversizedOrShortEntryIsNotInstalled(t *testing.T) {
	raw, err := binary()
	if err != nil {
		t.Fatal(err)
	}
	name := "crw_0.9.9_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	for label, last := range map[string]struct {
		size int64
		body string
	}{
		"over the bound":      {install.MaxArchiveBytes + 1, "short"},
		"short of its header": {100, "ten bytes!"},
	} {
		var buf bytes.Buffer
		compressed := gzip.NewWriter(&buf)
		w := tar.NewWriter(compressed)
		add := func(header *tar.Header, body []byte) {
			if err := w.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
		add(&tar.Header{Name: "crw", Mode: 0o755, Size: int64(len(raw)), Typeflag: tar.TypeReg}, raw)
		for _, link := range []string{"codex-session-relay", "codex-thread-bridge", "crw-completion-hook"} {
			add(&tar.Header{Name: link, Linkname: "crw", Typeflag: tar.TypeSymlink}, nil)
		}
		// The last entry's body stops where the archive ends, short of what its header declares.
		add(&tar.Header{Name: "LICENSE", Mode: 0o644, Size: last.size, Typeflag: tar.TypeReg}, []byte(last.body))
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		path := filepath.Join(dir, name)
		write(t, path, buf.String())
		sum := sha256.Sum256(buf.Bytes())
		write(t, filepath.Join(dir, install.SumsName), hex.EncodeToString(sum[:])+"  "+name+"\n")

		h := newHost(t)
		result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: path})
		if code != install.Refused || at(result, "failedStep") != "unpack the archive" || at(result, "retriable") != true || !strings.Contains(golden.Canon(at(result, "steps")), "nothing was unpacked") {
			t.Fatalf("%s: exit %d\n%s", label, code, golden.Canon(result))
		}
		if entries, _ := os.ReadDir(h.dest); len(entries) != 0 {
			t.Fatalf("%s: left %v", label, entries)
		}
		if _, err := os.Lstat(pointer.Path(h.dest)); !os.IsNotExist(err) {
			t.Fatalf("%s: the pointer was placed", label)
		}
		if installs := golden.List(at(h.hostRecord(t), "components", "codex-session-relay", "installs")); len(installs) != 0 {
			t.Fatalf("%s: the record lists %v", label, installs)
		}
	}
}
