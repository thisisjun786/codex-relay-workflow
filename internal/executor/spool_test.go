package executor

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func mkSpool(t *testing.T) string {
	t.Helper()
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	return spool
}

func putFile(t *testing.T, spool, rel string, body []byte) Artifact {
	t.Helper()
	p := filepath.Join(spool, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return Artifact{Path: rel, SHA256: Digest(body), Size: int64(len(body))}
}

func TestReadArtifact(t *testing.T) {
	spool := mkSpool(t)
	body := []byte("the evidence file\n")
	art := putFile(t, spool, "evidence/one.md", body)
	got, err := ReadArtifact(spool, art, DefaultLimits)
	if err != nil {
		t.Fatalf("ReadArtifact: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("bytes %q", got)
	}
	if err := os.MkdirAll(filepath.Join(spool, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	putFile(t, spool, "real/f.md", []byte("x"))
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("out"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(spool, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(spool, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(spool, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(spool, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(spool, "real", "f.md"), filepath.Join(spool, "hard.md")); err != nil {
		t.Fatal(err)
	}
	big := []byte("larger than the limit")
	putFile(t, spool, "big.md", big)
	tight := Limits{EnvelopeBytes: 1 << 20, ArtifactBytes: 4, TotalArtifactBytes: 1 << 20, ArtifactCount: 4}
	linkedSpool := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(spool, linkedSpool); err != nil {
		t.Fatal(err)
	}
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	putFile(t, wide, "f.md", []byte("x"))
	for _, c := range []struct {
		name   string
		spool  string
		art    Artifact
		limits Limits
	}{
		{"leaf symlink", spool, Artifact{Path: "link.md", SHA256: strings.Repeat("0", 64), Size: 3}, DefaultLimits},
		{"parent symlink", spool, Artifact{Path: "linkdir/f.md", SHA256: Digest([]byte("x")), Size: 1}, DefaultLimits},
		{"path traversal", spool, Artifact{Path: "../f.md", SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"absolute path", spool, Artifact{Path: "/etc/passwd", SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"backslash", spool, Artifact{Path: `a\b`, SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"lock file", spool, Artifact{Path: ".executor.lock", SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"fifo", spool, Artifact{Path: "fifo", SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"directory", spool, Artifact{Path: "adir", SHA256: strings.Repeat("0", 64), Size: 1}, DefaultLimits},
		{"hardlink", spool, Artifact{Path: "hard.md", SHA256: Digest([]byte("x")), Size: 1}, DefaultLimits},
		{"oversize", spool, Artifact{Path: "big.md", SHA256: Digest(big), Size: int64(len(big))}, tight},
		{"wrong digest", spool, Artifact{Path: "evidence/one.md", SHA256: strings.Repeat("9", 64), Size: int64(len(body))}, DefaultLimits},
		{"wrong size", spool, Artifact{Path: "evidence/one.md", SHA256: art.SHA256, Size: int64(len(body)) + 1}, DefaultLimits},
		{"spool is a symlink", linkedSpool, art, DefaultLimits},
		{"spool not private", wide, Artifact{Path: "f.md", SHA256: Digest([]byte("x")), Size: 1}, DefaultLimits},
		{"spool not absolute", "relative/spool", art, DefaultLimits},
		{"zero limits", spool, art, Limits{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ReadArtifact(c.spool, c.art, c.limits); err == nil {
				t.Error("the artifact was read")
			}
		})
	}
}

func TestReceive(t *testing.T) {
	newWorld := func(t *testing.T) (Admission, Result, string, []byte) {
		spool := mkSpool(t)
		a := mustFreeze(t, testTask(Devin))
		art := putFile(t, spool, "evidence/summary.md", []byte("123456789"))
		p := testPayload(a)
		p.Artifacts = []Artifact{art}
		r, err := NewResult(a, "completed", p, testProvenance(Devin))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := EncodeResult(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return a, r, spool, raw
	}
	t.Run("a valid delivery", func(t *testing.T) {
		a, r, spool, raw := newWorld(t)
		got, digest, err := Receive(a, spool, "result.json", testPeer(), DefaultLimits)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if got.Outcome != r.Outcome || got.TaskDigest != r.TaskDigest {
			t.Errorf("result %+v", got)
		}
		if digest != Digest(raw) {
			t.Errorf("the receipt digest is not of the received bytes")
		}
	})
	t.Run("the same bytes again", func(t *testing.T) {
		a, _, spool, raw := newWorld(t)
		_, d1, err := Receive(a, spool, "result.json", testPeer(), DefaultLimits)
		if err != nil {
			t.Fatal(err)
		}
		_, d2, err := Receive(a, spool, "result.json", testPeer(), DefaultLimits)
		if err != nil || d1 != d2 {
			t.Errorf("identical bytes did not replay identically: %q %q %v", d1, d2, err)
		}
		_ = raw
	})
	t.Run("wrong peer is refused before the file is read", func(t *testing.T) {
		a, _, spool, _ := newWorld(t)
		_, _, err := Receive(a, spool, "no-such.json", Peer{HostID: "other", Incarnation: "x"}, DefaultLimits)
		var ve *ValidationError
		if err == nil || !asValidation(err, &ve) || ve.Code != CodeHostMismatch {
			t.Errorf("err %v", err)
		}
	})
	t.Run("tampered envelope", func(t *testing.T) {
		a, _, spool, raw := newWorld(t)
		tampered := append([]byte{}, raw...)
		copy(tampered[len(tampered)-40:], []byte(strings.Repeat("x", 39)))
		if err := os.WriteFile(filepath.Join(spool, "result.json"), tampered, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Receive(a, spool, "result.json", testPeer(), DefaultLimits); err == nil {
			t.Error("a tampered envelope was received")
		}
	})
	t.Run("too many artifacts", func(t *testing.T) {
		spool := mkSpool(t)
		a := mustFreeze(t, testTask(Devin))
		a1 := putFile(t, spool, "evidence/one.md", []byte("1"))
		a2 := putFile(t, spool, "evidence/two.md", []byte("2"))
		p := testPayload(a)
		p.Artifacts = []Artifact{a1, a2}
		r, err := NewResult(a, "completed", p, testProvenance(Devin))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := EncodeResult(r)
		os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600)
		limits := DefaultLimits
		limits.ArtifactCount = 1
		_, _, err = Receive(a, spool, "result.json", testPeer(), limits)
		var ve *ValidationError
		if err == nil || !asValidation(err, &ve) || ve.Code != CodeArtifactInvalid {
			t.Errorf("err %v, want artifact_invalid", err)
		}
	})
	t.Run("total bytes exceeded", func(t *testing.T) {
		a, r, spool, _ := newWorld(t)
		limits := DefaultLimits
		limits.TotalArtifactBytes = 4
		raw, _ := EncodeResult(r)
		os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600)
		if _, _, err := Receive(a, spool, "result.json", testPeer(), limits); err == nil {
			t.Error("the total artifact bytes were not enforced")
		}
	})
	t.Run("artifact bytes tampered", func(t *testing.T) {
		a, r, spool, _ := newWorld(t)
		if err := os.WriteFile(filepath.Join(spool, "evidence", "summary.md"), []byte("changed!!"), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, _ := EncodeResult(r)
		os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600)
		if _, _, err := Receive(a, spool, "result.json", testPeer(), DefaultLimits); err == nil {
			t.Error("a tampered artifact was received")
		}
	})
	t.Run("result path escapes", func(t *testing.T) {
		a, _, spool, _ := newWorld(t)
		for _, p := range []string{"../result.json", filepath.Join(t.TempDir(), "result.json"), ".executor.lock", `a\b`} {
			if _, _, err := Receive(a, spool, p, testPeer(), DefaultLimits); err == nil {
				t.Errorf("result path %q was read", p)
			}
		}
	})
	t.Run("result path is a symlink", func(t *testing.T) {
		a, _, spool, raw := newWorld(t)
		outside := filepath.Join(t.TempDir(), "other.json")
		if err := os.WriteFile(outside, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(spool, "linked.json")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Receive(a, spool, "linked.json", testPeer(), DefaultLimits); err == nil {
			t.Error("a symlinked result was received")
		}
	})
}

func TestWriteEnvelope(t *testing.T) {
	raw := []byte(`{"ok":true}`)
	t.Run("write and replay", func(t *testing.T) {
		spool := mkSpool(t)
		if err := WriteEnvelope(spool, "task.json", raw, DefaultLimits); err != nil {
			t.Fatalf("WriteEnvelope: %v", err)
		}
		info, err := os.Stat(filepath.Join(spool, "task.json"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("the envelope mode is %v", info.Mode().Perm())
		}
		got, _ := os.ReadFile(filepath.Join(spool, "task.json"))
		if string(got) != string(raw) {
			t.Errorf("the envelope holds %q", got)
		}
		if err := WriteEnvelope(spool, "task.json", raw, DefaultLimits); err != nil {
			t.Errorf("the same bytes again: %v", err)
		}
		entries, err := os.ReadDir(spool)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		for _, n := range names {
			if n != "task.json" && n != ".executor.lock" {
				t.Errorf("a stray file %q remains", n)
			}
		}
	})
	t.Run("different bytes refused", func(t *testing.T) {
		spool := mkSpool(t)
		if err := WriteEnvelope(spool, "task.json", raw, DefaultLimits); err != nil {
			t.Fatal(err)
		}
		if err := WriteEnvelope(spool, "task.json", []byte(`{"ok":false}`), DefaultLimits); err == nil {
			t.Error("an immutable envelope was replaced")
		}
		got, _ := os.ReadFile(filepath.Join(spool, "task.json"))
		if string(got) != string(raw) {
			t.Errorf("the envelope became %q", got)
		}
	})
	t.Run("concurrent same bytes", func(t *testing.T) {
		spool := mkSpool(t)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- WriteEnvelope(spool, "r.json", raw, DefaultLimits)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent write: %v", err)
			}
		}
		got, err := os.ReadFile(filepath.Join(spool, "r.json"))
		if err != nil || string(got) != string(raw) {
			t.Errorf("the envelope holds %q (%v)", got, err)
		}
	})
	t.Run("invalid names and content", func(t *testing.T) {
		spool := mkSpool(t)
		for _, name := range []string{"a/b", "..", ".", ".executor.lock", "/abs", `a\b`} {
			if err := WriteEnvelope(spool, name, raw, DefaultLimits); err == nil {
				t.Errorf("name %q was written", name)
			}
		}
		tight := DefaultLimits
		tight.EnvelopeBytes = 4
		if err := WriteEnvelope(spool, "big.json", raw, tight); err == nil {
			t.Error("an oversize envelope was written")
		}
		if err := WriteEnvelope(spool, "bad.json", []byte{0xff, 0xfe}, DefaultLimits); err == nil {
			t.Error("non-UTF8 bytes were written")
		}
		if err := WriteEnvelope(spool, "two.json", append(raw, ' ', '{', '}'), DefaultLimits); err == nil {
			t.Error("two documents were written")
		}
		if err := WriteEnvelope(spool, "text.json", []byte("not json"), DefaultLimits); err == nil {
			t.Error("non-JSON bytes were written")
		}
	})
	t.Run("non private spool", func(t *testing.T) {
		spool := filepath.Join(t.TempDir(), "open")
		if err := os.Mkdir(spool, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := WriteEnvelope(spool, "task.json", raw, DefaultLimits); err == nil {
			t.Error("a non-private spool was written")
		}
	})
}
