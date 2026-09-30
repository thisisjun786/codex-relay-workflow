package ownership_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func Test30DisagreementNeverRepairs(t *testing.T) {
	for _, what := range []string{"owner", "epoch", "inode", "build", "transition", "missing-json", "missing-key", "missing-db", "rollback", "protocol", "missing-field", "missing-gate", "scope", "transition-path", "unsupported-db"} {
		t.Run(what, func(t *testing.T) {
			path := goStore(t)
			admitted, e := ownership.Admit(t.Context(), path)
			must(t, e)
			must(t, admitted.Close())
			r, e := ownership.ReadRecord(path)
			must(t, e)
			switch what {
			case "owner":
				r.Owner = "python"
			case "epoch":
				r.Epoch++
			case "inode":
				r.Database.Inode++
			case "build":
				r.PythonCompatibilityBuild = "other"
			case "transition":
				r.Transition = &ownership.Transition{ID: "foreign", From: "python", To: "go", TargetEpoch: 8}
			case "rollback":
				r.RollbackAllowed = false
			case "protocol":
				r.Protocol = 2
			case "scope":
				socket, key := "/different/socket", "wrong-key"
				r.AppServerSocket = &socket
				r.ScopeKey = &key
			case "transition-path":
				r.Transition = &ownership.Transition{ID: "../elsewhere", From: "python", To: "go", TargetEpoch: 2}
				r.Phase = "draining"
			}
			must(t, ownership.Publish(path, r, nil))
			switch what {
			case "missing-json":
				must(t, os.Remove(filepath.Join(filepath.Dir(path), "takeover.json")))
			case "missing-gate":
				must(t, os.Remove(filepath.Join(filepath.Dir(path), "write-gate.lock")))
			case "unsupported-db":
				db, e := ownership.OpenExisting(t.Context(), path, "rw")
				must(t, e)
				_, e = db.Exec("UPDATE schema_meta SET value='2' WHERE key='writer_protocol'")
				must(t, e)
				must(t, db.Close())
			case "missing-key":
				db, e := ownership.OpenExisting(t.Context(), path, "rw")
				must(t, e)
				_, e = db.Exec("DELETE FROM schema_meta WHERE key='owner'")
				must(t, e)
				must(t, db.Close())
			case "missing-db":
				must(t, os.Remove(path))
			case "missing-field":
				raw, e := os.ReadFile(filepath.Join(filepath.Dir(path), "takeover.json"))
				must(t, e)
				var m map[string]any
				must(t, json.Unmarshal(raw, &m))
				delete(m, "rollbackAllowed")
				raw, e = json.Marshal(m)
				must(t, e)
				must(t, os.WriteFile(filepath.Join(filepath.Dir(path), "takeover.json"), raw, 0600))
			}
			before := treeBytes(t, filepath.Dir(path))
			if a, e := ownership.Admit(t.Context(), path); e == nil {
				_ = a.Close()
				t.Fatal("admitted disagreement")
			}
			after := treeBytes(t, filepath.Dir(path))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed source bytes/files")
			}
		})
	}
}
func treeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	must(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, e := os.ReadFile(path)
		if e == nil {
			m[path] = string(raw)
		}
		return e
	}))
	return m
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
	admitted, err := ownership.Admit(t.Context(), through)
	must(t, err)
	must(t, admitted.Close())
}

// Audit finding 27: between takeover commit's COMMIT and its mirror publication the
// live Go daemon keeps serving; the reverse disagreement still refuses.
// The one mirror disagreement a Go writer accepts: rollback_allowed committed 1->0 in the database
// before the mirror says so (an older runtime's takeover commit, torn before its publication).
// The opposite, a mirror leading the database, refuses.
func Test30CommitTearKeepsGoWritersAdmitted(t *testing.T) {
	path := goStore(t)
	live, err := store.Open(t.Context(), path, "")
	must(t, err)
	defer live.Close()
	raw, err := ownership.OpenExisting(t.Context(), path, "rw")
	must(t, err)
	_, err = raw.Exec("UPDATE schema_meta SET value='0' WHERE key='rollback_allowed'")
	must(t, errors.Join(err, raw.Close()))
	must(t, live.Transaction(t.Context(), func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, "INSERT INTO schema_meta VALUES('test:during-tear','served')")
		return err
	}))
	admitted, err := ownership.Admit(t.Context(), path)
	must(t, err)
	must(t, admitted.Close())
	r, err := ownership.ReadRecord(path)
	must(t, err)
	r.RollbackAllowed = false
	must(t, ownership.Publish(path, r, nil))
	raw, err = ownership.OpenExisting(t.Context(), path, "rw")
	must(t, err)
	_, err = raw.Exec("UPDATE schema_meta SET value='1' WHERE key='rollback_allowed'")
	must(t, errors.Join(err, raw.Close()))
	if a, err := ownership.Admit(t.Context(), path); err == nil {
		_ = a.Close()
		t.Fatal("mirror rollbackAllowed=false against DB 1 admitted")
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
		admission, err := ownership.Admit(t.Context(), path)
		if err != nil {
			t.Errorf("%s: admission %v", encoding, err)
			continue
		}
		must(t, admission.Close())
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
func TestValidateRefusesARecordWhoseAuthorityHasNoHome(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "relay.sqlite3")
	socket := filepath.Join(dir, "app.sock")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", filepath.Join(dir, "scopes"))
	testsupport.Create(t, path, socket, "go")
	r, err := ownership.ReadRecord(path)
	must(t, err)
	s, err := ownership.SnapshotMeta(t.Context(), path)
	must(t, err)
	must(t, ownership.Validate(path, r, s))
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", "~crw-no-such-user-31/scopes")
	if _, err = ownership.ScopeRoot("~crw-no-such-user-31/scopes"); !errors.Is(err, ownership.ErrNoHome) || err.Error() != "RuntimeError: Could not determine home directory." {
		t.Fatalf("ScopeRoot of an unknown ~user: %v", err)
	}
	if _, err = ownership.ScopeKey(socket); !errors.Is(err, ownership.ErrNoHome) {
		t.Fatalf("ScopeKey under an unknown ~user: %v", err)
	}
	for _, check := range []func() error{
		func() error { return ownership.Validate(path, r, s) },
		func() error { return ownership.CheckStart(t.Context(), path, "") },
		func() error {
			admission, err := ownership.Admit(t.Context(), path)
			if err == nil {
				must(t, admission.Close())
			}
			return err
		},
	} {
		var refused *ownership.Refused
		if err = check(); !errors.As(err, &refused) || refused.Detail != ownership.NoAuthorityDetail {
			t.Errorf("a bound record under an authority with no home: %#v", err)
		}
	}
	if ownership.NoAuthorityDetail != "scope key cannot be judged: Could not determine home directory." {
		t.Errorf("NoAuthorityDetail %q", ownership.NoAuthorityDetail)
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
