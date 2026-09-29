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
