package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The parity tests below ran the retained Python console script live beside the Go one, each in
// its own run over the same home: the Python half first, then resetRuntime, then the Go half.
// The Python implementation leaves the repository in todo 44, so the Python half's answer is
// recorded (internal/testsupport/pyoracle) and replayed, and the Go half runs live as before.
//
// pythonHalf records what the Python half answered together with what resetRuntime leaves of the
// home it ran in, and on replay puts that back, so the Go half starts from the state it started
// from beside the live Python: the emptied store file (whose inode the worker receipt names) and
// the directories Python made outside the state it resets.

// pythonState is one recorded Python half.
type pythonState struct {
	Answer json.RawMessage `json:"answer"`
	// Left is every entry under home the half left behind that the test had not made before it,
	// once resetRuntime ran: "d <mode> <path>" or "f <mode> <path>" (an empty file), sorted.
	Left []string `json:"left"`
	// Emptied is resetRuntime's note that it emptied a fenced store (emptiedStores).
	Emptied bool `json:"emptied"`
}

// pythonHalf runs capture, the live Python half of a test in home, and resetRuntime after it when
// reset is set, or replays the recording of both, decoding the half's answer into out. sockets
// are the App Server sockets the half named besides home/socket, whose scope keys an answer may
// carry.
func pythonHalf(t *testing.T, home, key string, reset bool, out any, capture func() (any, error), sockets ...string) {
	t.Helper()
	raw := pyoracle.Answer(t, key, func() ([]byte, error) {
		before, err := homeEntries(home)
		if err != nil {
			return nil, err
		}
		answer, err := capture()
		if err != nil {
			return nil, err
		}
		encodedAnswer, err := marshalText(answer)
		if err != nil {
			return nil, err
		}
		encodedAnswer, err = hideStoreIdentity(home, encodedAnswer)
		if err != nil {
			return nil, err
		}
		if reset {
			resetRuntime(t, home)
		}
		after, err := homeEntries(home)
		if err != nil {
			return nil, err
		}
		left := []string{}
		for entry, size := range after {
			if _, made := before[entry]; made {
				continue
			}
			if size > 0 {
				return nil, fmt.Errorf("the Python half left %s holding %d bytes, which a replay cannot put back", entry, size)
			}
			left = append(left, entry)
		}
		sort.Strings(left)
		return marshalText(pythonState{Answer: encodedAnswer, Left: left, Emptied: emptiedStores[home]})
	}, homeSubstitutions(t, home, sockets...)...)
	var state pythonState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("recorded Python half %q: %v", key, err)
	}
	if !pyoracle.Live() {
		restoreEntries(t, home, state.Left)
		if state.Emptied {
			emptiedStores[home] = true
		}
	}
	answer, err := showStoreIdentity(home, state.Answer)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(answer, out); err != nil {
		t.Fatalf("recorded Python half %q: %v", key, err)
	}
}

// marshalText is json.Marshal without HTML escaping, so the placeholders a recording spells stay
// as spelled.
func marshalText(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// homeEntries lists every entry under home as "d|f|o <mode> <path relative to home>" with a
// file's size.
func homeEntries(home string) (map[string]int64, error) {
	out := map[string]int64{}
	err := filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if path == home {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		kind := "f"
		switch {
		case info.IsDir():
			kind = "d"
		case !info.Mode().IsRegular():
			kind = "o"
		}
		var size int64
		if kind == "f" {
			size = info.Size()
		}
		out[fmt.Sprintf("%s %04o %s", kind, info.Mode().Perm(), relative)] = size
		return nil
	})
	return out, err
}

// restoreEntries makes the directories and empty files a recorded Python half left, in order.
func restoreEntries(t *testing.T, home string, left []string) {
	t.Helper()
	for _, entry := range left {
		fields := strings.SplitN(entry, " ", 3)
		if len(fields) != 3 {
			t.Fatalf("recorded entry %q", entry)
		}
		mode, err := strconv.ParseUint(fields[1], 8, 32)
		if err != nil {
			t.Fatalf("recorded entry %q: %v", entry, err)
		}
		path := filepath.Join(home, fields[2])
		switch fields[0] {
		case "d":
			if err = os.MkdirAll(path, 0o700); err == nil {
				err = os.Chmod(path, os.FileMode(mode))
			}
		case "f":
			if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
				err = os.WriteFile(path, nil, os.FileMode(mode))
			}
			if err == nil {
				err = os.Chmod(path, os.FileMode(mode))
			}
		default:
			t.Fatalf("the Python half left %q, which a replay cannot put back", entry)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

var storeIdentityKeys = map[string]*regexp.Regexp{
	"<DB_INODE>":  regexp.MustCompile(`((?:dbInode|inode)\\?"\s*:\s*)(\d+)\b`),
	"<DB_DEVICE>": regexp.MustCompile(`((?:dbDevice|device)\\?"\s*:\s*)(\d+)\b`),
}

// storeIdentity is the device and inode of home's relay.sqlite3, which resetRuntime keeps and a
// replay recreates, as the placeholders a recording spells them with.
func storeIdentity(home string) (map[string]string, error) {
	identity, err := ownership.Physical(filepath.Join(home, "state", "relay.sqlite3"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]string{"<DB_INODE>": strconv.FormatUint(identity.Inode, 10), "<DB_DEVICE>": strconv.FormatUint(identity.Device, 10)}, nil
}

// hideStoreIdentity spells the store file's device and inode, where an answer names them under
// their keys, as placeholders.
func hideStoreIdentity(home string, answer []byte) ([]byte, error) {
	identity, err := storeIdentity(home)
	if err != nil || identity == nil {
		return answer, err
	}
	for placeholder, pattern := range storeIdentityKeys {
		answer = pattern.ReplaceAllFunc(answer, func(match []byte) []byte {
			groups := pattern.FindSubmatch(match)
			if string(groups[2]) != identity[placeholder] {
				return match
			}
			return append(append([]byte{}, groups[1]...), placeholder...)
		})
	}
	return answer, nil
}

// showStoreIdentity puts the store file's current device and inode in place of the placeholders.
func showStoreIdentity(home string, answer []byte) ([]byte, error) {
	identity, err := storeIdentity(home)
	if err != nil || identity == nil {
		return answer, err
	}
	for placeholder, value := range identity {
		answer = []byte(strings.ReplaceAll(string(answer), placeholder, value))
	}
	return answer, nil
}

// homeSubstitutions are the run-specific strings a Python half's answer in home may carry: the
// suite's own temporary directory (the installation's), the home, the other sockets, the scope keys of home/socket and of sockets (and their
// socket halves, as the operations database names them), the isolated registry's salt and the
// installation ID both runtimes derive from the installation directory and the state directory.
func homeSubstitutions(t *testing.T, home string, sockets ...string) []pyoracle.Option {
	t.Helper()
	executable, err := filepath.EvalSymlinks(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	options := []pyoracle.Option{
		pyoracle.Substitute(filepath.Dir(filepath.Dir(filepath.Dir(executable))), "<SUITE>"),
		pyoracle.Substitute(home, "<HOME>"),
	}
	root, err := ownership.ScopeRoot(home + "/scopes")
	if err != nil {
		t.Fatal(err)
	}
	for i, socket := range sockets {
		options = append(options, pyoracle.Substitute(socket, fmt.Sprintf("<SOCKET_%d>", i+1)))
	}
	registry := &ScopeRegistry{Root: root, Authority: "isolated"}
	for i, socket := range append([]string{home + "/socket"}, sockets...) {
		key := registry.Key(socket)
		options = append(options, pyoracle.Substitute(key, fmt.Sprintf("<SCOPE_KEY_%d>", i)),
			pyoracle.Substitute(key[strings.LastIndex(key, "-")+1:], fmt.Sprintf("<SOCKET_KEY_%d>", i)))
	}
	salt := sha256.Sum256([]byte(root))
	resolved, err := store.ResolvePath(home + "/state")
	if err != nil {
		t.Fatal(err)
	}
	installation := sha256.Sum256([]byte(filepath.Dir(executable) + "\x00" + resolved))
	return append(options, pyoracle.Substitute(hex.EncodeToString(salt[:4]), "<SCOPE_SALT>"),
		pyoracle.Substitute(hex.EncodeToString(installation[:8]), "<INSTALLATION_ID>"))
}

// pythonCapture is what one Python console run answered, normalized as compare reads it: the
// times and process identities its stdout carries are the only parts a rerun changes.
func pythonCapture(c capture) capture {
	c.Out = normalize(c.Out)
	return c
}

// goTables is tables read by Go: every table's rows in rowid order as Python's sqlite3 returns
// them through json.dumps, the time columns the relay writes as numbers spelled EPOCH_TIME.
func goTables(t *testing.T, home string, writer testsupport.Runtime) string {
	t.Helper()
	path := filepath.Join(home, "state", "relay.sqlite3")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "absent"
	}
	db, err := ownership.OpenExisting(context.Background(), path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	names, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tableNames []string
	for names.Next() {
		var name string
		if err = names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tableNames = append(tableNames, name)
	}
	if err = errors.Join(names.Err(), names.Close()); err != nil {
		t.Fatal(err)
	}
	timeColumns := map[string]bool{"next_eligible_at": true, "next_retry_at": true, "lease_until": true, "last_send_at": true, "window_start": true}
	data := map[string][][]any{}
	for _, name := range tableNames {
		rows, err := db.Query(`SELECT * FROM "` + name + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		table := [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			row := make([]any, len(columns))
			for i, value := range values {
				switch v := value.(type) {
				case int64:
					row[i] = float64(v)
				case float64:
					row[i] = v
				case []byte:
					row[i] = string(v)
				default:
					row[i] = v
				}
				if _, number := row[i].(float64); number && timeColumns[columns[i]] {
					row[i] = "EPOCH_TIME"
				}
			}
			table = append(table, row)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		data[name] = table
	}
	return tableText(t, data, writer)
}

// tableText is the comparable text of a table dump: the random store_id and the owner row made
// runtime-neutral, then normalized.
func tableText(t *testing.T, data map[string][][]any, writer testsupport.Runtime) string {
	t.Helper()
	for _, row := range data["schema_meta"] {
		if row[0] == "store_id" {
			row[1] = "RANDOM_STORE_ID"
		}
		if key, ok := row[0].(string); ok {
			row[1] = testsupport.OwnerNeutral(t, writer, key, row[1])
		}
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return normalize(string(raw))
}
