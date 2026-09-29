package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"strings"
	"testing"
)

type entry struct {
	header tar.Header
	body   string
}

func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	compressed := gzip.NewWriter(&buf)
	w := tar.NewWriter(compressed)
	for _, e := range entries {
		header := e.header
		header.Size = int64(len(e.body))
		if header.Mode == 0 {
			header.Mode = 0o644
		}
		if err := w.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every name the installer or the doctor reads as control data inside a runtime directory - bin/
// (where the installer places crw and its links, and the doctor reads them and bin/python*), a
// component beginning .crw- (the staging lock, the claim, the claim's lock sidecar and its
// atomic-write temporaries), one ending .crw-lock (any file's lock sidecar) and pyvenv.cfg (a
// Python venv to the doctor) at any level and in any case - refuses the whole archive, and the
// refusal comes before anything is written: an entry ahead of it in the archive is not
// unpacked either, and not even bin/ is created. The release layout itself unpacks.
func TestAnArchiveCannotPlantControlData(t *testing.T) {
	binary := "\x7fELF crw"
	links := []entry{
		{tar.Header{Name: "codex-session-relay", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
		{tar.Header{Name: "codex-thread-bridge", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
		{tar.Header{Name: "crw-completion-hook", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
	}
	release := append([]entry{
		{tar.Header{Name: "LICENSE", Typeflag: tar.TypeReg}, "MIT\n"},
		{tar.Header{Name: "crw", Mode: 0o755, Typeflag: tar.TypeReg}, binary},
		{tar.Header{Name: "packages/codex-thread-bridge/LICENSE", Typeflag: tar.TypeReg}, "MIT\n"},
	}, links...)
	for _, reserved := range []entry{
		{tar.Header{Name: ".crw-staging-lock", Typeflag: tar.TypeReg}, ""},
		{tar.Header{Name: ".crw-staging-claim.json", Typeflag: tar.TypeReg}, "{}"},
		{tar.Header{Name: ".crw-staging-claim.json.crw-lock", Typeflag: tar.TypeReg}, "1"},
		{tar.Header{Name: ".crw-write-4242", Typeflag: tar.TypeReg}, "{}"},
		{tar.Header{Name: "./.CRW-STAGING-LOCK", Typeflag: tar.TypeReg}, ""},
		{tar.Header{Name: "packages/.crw-staging-claim.json", Typeflag: tar.TypeReg}, "{}"},
		{tar.Header{Name: ".crw-anything/LICENSE", Typeflag: tar.TypeReg}, "x"},
		{tar.Header{Name: ".crw-anything/", Typeflag: tar.TypeDir}, ""},
		{tar.Header{Name: "LICENSE.crw-lock", Typeflag: tar.TypeReg}, "1"},
		{tar.Header{Name: "pyvenv.cfg", Typeflag: tar.TypeReg}, "home = /usr/bin\n"},
		{tar.Header{Name: "PyVenv.CFG", Typeflag: tar.TypeReg}, "home = /usr/bin\n"},
		{tar.Header{Name: "lib/python3.12/pyvenv.cfg", Typeflag: tar.TypeReg}, "home = /usr/bin\n"},
		{tar.Header{Name: "pyvenv.cfg/x", Typeflag: tar.TypeReg}, "x"},
		{tar.Header{Name: "bin", Typeflag: tar.TypeDir}, ""},
		{tar.Header{Name: "bin/crw", Mode: 0o755, Typeflag: tar.TypeReg}, binary},
		{tar.Header{Name: "bin/python3", Mode: 0o755, Typeflag: tar.TypeReg}, binary},
		{tar.Header{Name: "bin/pyvenv.cfg", Typeflag: tar.TypeReg}, "home = /usr/bin\n"},
		{tar.Header{Name: "bin/codex-thread-bridge", Mode: 0o755, Typeflag: tar.TypeReg}, binary},
	} {
		// The reserved entry comes last, so a reader that writes as it goes has already written
		// the licence, the binary and the links when it meets it.
		entries := append(append([]entry{}, release...), reserved)
		dir := t.TempDir()
		err := Archive{bytes: tarball(t, entries...)}.Unpack(dir)
		if err == nil || !strings.Contains(err.Error(), "control data") {
			t.Fatalf("%s: %v", reserved.header.Name, err)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: refused after writing %v", reserved.header.Name, left)
		}
	}
	dir := t.TempDir()
	if err := (Archive{bytes: tarball(t, release...)}).Unpack(dir); err != nil {
		t.Fatalf("the release layout: %v", err)
	}
	for _, name := range []string{"LICENSE", "packages/codex-thread-bridge/LICENSE", "bin/crw", "bin/codex-thread-bridge"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Fatalf("the release layout did not unpack %s: %v", name, err)
		}
	}
}
