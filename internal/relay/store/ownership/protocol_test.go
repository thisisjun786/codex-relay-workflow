package ownership_test

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Audit findings 21, 29, 52: a state directory reached through a symlink is the same
// S; its mirror names the resolved S/control.sock. (The Python fence's preflight through the
// link was checked here until todo 44.)
func Test30ReadRecordThroughSymlinkedState(t *testing.T) {
	path := goStore(t)
	link := filepath.Join(t.TempDir(), "state-link")
	must(t, os.Symlink(filepath.Dir(path), link))
	through := filepath.Join(link, "relay.sqlite3")
	r, err := ownership.ReadRecord(through)
	must(t, err)
	if r.RelayRPCSocket != filepath.Join(filepath.Dir(path), "control.sock") {
		t.Fatal(r.RelayRPCSocket)
	}
}

// encodeMirror spells text in one of the encodings json.loads reads from bytes
// (json.detect_encoding): UTF-8 behind its mark, or UTF-16 and UTF-32 with or without theirs.
func encodeMirror(text, encoding string) []byte {
	var out []byte
	switch encoding {
	case "utf-8-sig":
		return append([]byte{0xef, 0xbb, 0xbf}, text...)
	case "utf-16", "utf-16-le":
		if encoding == "utf-16" {
			out = []byte{0xff, 0xfe}
		}
		for _, unit := range utf16.Encode([]rune(text)) {
			out = binary.LittleEndian.AppendUint16(out, unit)
		}
	case "utf-16-be":
		for _, unit := range utf16.Encode([]rune(text)) {
			out = binary.BigEndian.AppendUint16(out, unit)
		}
	case "utf-32":
		out = []byte{0xff, 0xfe, 0, 0}
		for _, r := range text {
			out = binary.LittleEndian.AppendUint32(out, uint32(r))
		}
	case "utf-32-be":
		for _, r := range text {
			out = binary.BigEndian.AppendUint32(out, uint32(r))
		}
	}
	return out
}

// The mirror is read as ownership.mirror reads it, json.loads(bytes): a UTF-8 byte order mark,
// and UTF-16 or UTF-32 with or without theirs, are the same record, admitted as its plain UTF-8
// spelling is; bytes the codec refuses, or text json.loads refuses once decoded (a second mark),
// are refused.
func TestReadRecordReadsTheMirrorAsJSONLoadsBytes(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "relay.sqlite3")
	testsupport.Create(t, path, filepath.Join(dir, "app.sock"), "go")
	mirror := filepath.Join(dir, "takeover.json")
	plain, err := os.ReadFile(mirror)
	must(t, err)
	want, err := ownership.ReadRecord(path)
	must(t, err)
	for _, encoding := range []string{"utf-8-sig", "utf-16", "utf-16-le", "utf-16-be", "utf-32", "utf-32-be"} {
		must(t, os.WriteFile(mirror, encodeMirror(string(plain), encoding), 0o600))
		got, err := ownership.ReadRecord(path)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, %v", encoding, got, err)
			continue
		}
	}
	for name, raw := range map[string][]byte{
		"truncated utf-16": encodeMirror(string(plain), "utf-16")[:len(encodeMirror(string(plain), "utf-16"))-1],
		"a second mark":    encodeMirror(string(encodeMirror(string(plain), "utf-8-sig")), "utf-8-sig"),
	} {
		must(t, os.WriteFile(mirror, raw, 0o600))
		var refused *ownership.Refused
		if _, err := ownership.ReadRecord(path); !errors.As(err, &refused) || !strings.HasPrefix(refused.Detail, "decode mirror: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An override whose ~user has no home gives no scope key (pathlib's RuntimeError, ErrNoHome, in
// the fence's host words), and Validate refuses a bound record it cannot judge rather than
// failing (ownership.py validate): NoAuthorityDetail, not queueable, before the owner.
func TestScopeKeyRefusesAnAuthorityWithNoHome(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "app.sock")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", "~crw-no-such-user-31/scopes")
	if _, err := ownership.ScopeRoot("~crw-no-such-user-31/scopes"); !errors.Is(err, ownership.ErrNoHome) || err.Error() != "RuntimeError: Could not determine home directory." {
		t.Fatalf("ScopeRoot of an unknown ~user: %v", err)
	}
	if _, err := ownership.ScopeKey(socket); !errors.Is(err, ownership.ErrNoHome) {
		t.Fatalf("ScopeKey under an unknown ~user: %v", err)
	}
}

// goStore is a stopped store the Go runtime created and owns, in an owner-only directory.
func goStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.Chmod(root, 0700))
	path := filepath.Join(root, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	return path
}
