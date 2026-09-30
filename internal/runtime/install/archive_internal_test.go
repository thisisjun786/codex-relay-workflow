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
	return cut(t, entries, nil, "")
}

// cut is tarball, then (when last is set) one more header declaring last.Size and a body of only
// the bytes given, where the archive ends: a stream a reader must find short, never pad or trim.
func cut(t *testing.T, entries []entry, last *tar.Header, body string) []byte {
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
	if last == nil {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := w.WriteHeader(last); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Every name the installer or the doctor reads as control data inside a runtime directory - bin/
// (where the installer places crw and its links, and the doctor reads them), a component
// beginning .crw- (the staging lock, the claim, the claim's lock sidecar and its atomic-write
// temporaries) and one ending .crw-lock (any file's lock sidecar), at any level and in any case -
// refuses the whole archive, and the
// refusal comes before anything is written: an entry ahead of it in the archive is not
// unpacked either, and not even bin/ is created. The release layout itself unpacks.
func TestAnArchiveCannotPlantControlData(t *testing.T) {
	binary := "\x7fELF crw"
	links := []entry{
		{tar.Header{Name: "codex-session-relay", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
		{tar.Header{Name: "codex-thread-bridge", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
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
		{tar.Header{Name: "LICENSE.CRW-LOCK", Typeflag: tar.TypeReg}, "1"},
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

// An entry is never truncated to the bound: one whose header declares more than MaxArchiveBytes,
// one whose body ends before the size its header declares, and entries that together declare
// more than an archive may unpack to each refuse the whole archive before anything is written -
// with the release's own entries ahead of them, not even those are unpacked.
func TestAnOversizedOrShortEntryIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	release := []entry{
		{tar.Header{Name: "LICENSE", Typeflag: tar.TypeReg}, "MIT\n"},
		{tar.Header{Name: "crw", Mode: 0o755, Typeflag: tar.TypeReg}, "\x7fELF crw"},
		{tar.Header{Name: "codex-session-relay", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
		{tar.Header{Name: "codex-thread-bridge", Linkname: Binary, Typeflag: tar.TypeSymlink}, ""},
	}
	for label, c := range map[string]struct {
		raw  []byte
		want string
	}{
		"a header over the bound":        {cut(t, release, &tar.Header{Name: "NOTICE", Mode: 0o644, Size: MaxArchiveBytes + 1, Typeflag: tar.TypeReg}, "short"), "more than the"},
		"a body short of its header":     {cut(t, release, &tar.Header{Name: "NOTICE", Mode: 0o644, Size: 100, Typeflag: tar.TypeReg}, "ten bytes!"), "holds 10 of the 100 bytes"},
		"the binary short of its header": {cut(t, release[:1], &tar.Header{Name: "crw", Mode: 0o755, Size: 4096, Typeflag: tar.TypeReg}, "\x7fELF"), "holds 4 of the 4096 bytes"},
	} {
		dir := t.TempDir()
		err := Archive{bytes: c.raw}.Unpack(dir)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "nothing was unpacked") {
			t.Fatalf("%s: %v", label, err)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("%s: refused after writing %v", label, left)
		}
	}

	saved := maxUnpacked
	maxUnpacked = 20
	defer func() { maxUnpacked = saved }()
	dir := t.TempDir()
	over := append(append([]entry{}, release...), entry{tar.Header{Name: "NOTICE", Typeflag: tar.TypeReg}, "ten bytes!"})
	if err := (Archive{bytes: tarball(t, over...)}).Unpack(dir); err == nil || !strings.Contains(err.Error(), "in all") {
		t.Fatalf("entries over the total: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("entries over the total: refused after writing %v", left)
	}
	// A whole entry over the bound is refused, never written cut down to it.
	dir = t.TempDir()
	whole := append(append([]entry{}, release...), entry{tar.Header{Name: "NOTICE", Typeflag: tar.TypeReg}, "twenty-one bytes long"})
	if err := (Archive{bytes: tarball(t, whole...)}).Unpack(dir); err == nil || !strings.Contains(err.Error(), "one entry may hold") {
		t.Fatalf("a whole entry over the bound: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a whole entry over the bound: refused after writing %v", left)
	}
	if err := (Archive{bytes: tarball(t, release...)}).Unpack(t.TempDir()); err != nil {
		t.Fatalf("the release within the total: %v", err)
	}
}
