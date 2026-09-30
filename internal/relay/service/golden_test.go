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
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The parity tests below ran the retained Python console script live beside the Go one, each in
// its own run over the same home, until todo 44 took the Python implementation out of the
// repository. What the Go half answers is now checked against the test's golden
// (internal/testsupport/golden), which began as the answer the Python half gave: the Go half's
// answer as compare reads it (normalizedCapture), with every run-specific string spelled as a
// placeholder (homeSubstitutions) and the store file's device and inode as <DB_DEVICE> and
// <DB_INODE> (hideStoreIdentity).

// checkAnswer compares answer, what the Go half of a test in home answered, with the test's
// golden under key. sockets are the App Server sockets the half named besides home/socket, whose
// scope keys an answer may carry.
func checkAnswer(t *testing.T, home, key string, answer any, sockets ...string) {
	t.Helper()
	encoded, err := golden.Encode(answer)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, err = hideStoreIdentity(home, encoded); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, key, encoded, homeSubstitutions(t, home, sockets...)...)
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

var storeIdentityKeys = map[string]*regexp.Regexp{
	"<DB_INODE>":  regexp.MustCompile(`((?:dbInode|inode)\\?"\s*:\s*)(\d+)\b`),
	"<DB_DEVICE>": regexp.MustCompile(`((?:dbDevice|device)\\?"\s*:\s*)(\d+)\b`),
}

// storeIdentity is the device and inode of home's relay.sqlite3, as the placeholders a golden
// spells them with.
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
// their keys, as placeholders: a file of the same content has another identity on every run.
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

// homeSubstitutions are the run-specific strings an answer in home may carry: the suite's own
// temporary directory (the installation's), the home, the other sockets, the scope keys of
// home/socket and of sockets (and their socket halves, as the operations database names them),
// the isolated registry's salt and the installation ID derived from the installation directory
// and the state directory.
func homeSubstitutions(t *testing.T, home string, sockets ...string) []golden.Option {
	t.Helper()
	executable, err := filepath.EvalSymlinks(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	options := []golden.Option{
		golden.Substitute(filepath.Dir(filepath.Dir(filepath.Dir(executable))), "<SUITE>"),
		golden.Substitute(home, "<HOME>"),
	}
	root, err := ownership.ScopeRoot(home + "/scopes")
	if err != nil {
		t.Fatal(err)
	}
	for i, socket := range sockets {
		options = append(options, golden.Substitute(socket, fmt.Sprintf("<SOCKET_%d>", i+1)))
	}
	registry := &ScopeRegistry{Root: root, Authority: "isolated"}
	for i, socket := range append([]string{home + "/socket"}, sockets...) {
		key := registry.Key(socket)
		options = append(options, golden.Substitute(key, fmt.Sprintf("<SCOPE_KEY_%d>", i)),
			golden.Substitute(key[strings.LastIndex(key, "-")+1:], fmt.Sprintf("<SOCKET_KEY_%d>", i)))
	}
	salt := sha256.Sum256([]byte(root))
	resolved, err := store.ResolvePath(home + "/state")
	if err != nil {
		t.Fatal(err)
	}
	installation := sha256.Sum256([]byte(filepath.Dir(executable) + "\x00" + resolved))
	return append(options, golden.Substitute(hex.EncodeToString(salt[:4]), "<SCOPE_SALT>"),
		golden.Substitute(hex.EncodeToString(installation[:8]), "<INSTALLATION_ID>"))
}

// normalizedCapture is what one console run answered, normalized as compare reads it: the times
// and process identities its stdout carries are the only parts a rerun changes.
func normalizedCapture(c capture) capture {
	c.Out = normalize(c.Out)
	return c
}

// tables is every table/column/value of home's store, including persisted JSON bytes (SQLite page
// layout, freelists and WAL checkpoints are not logical table contents): every table's rows in
// rowid order as Python's sqlite3 returned them through json.dumps, the time columns the relay
// writes as numbers spelled EPOCH_TIME.
func tables(t *testing.T, home string) string {
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
	return tableText(t, data)
}

// tableText is the comparable text of a table dump: the random store_id and Go's owner row made
// runtime-neutral, then normalized.
func tableText(t *testing.T, data map[string][][]any) string {
	t.Helper()
	for _, row := range data["schema_meta"] {
		if row[0] == "store_id" {
			row[1] = "RANDOM_STORE_ID"
		}
		if key, ok := row[0].(string); ok {
			row[1] = testsupport.OwnerNeutral(t, testsupport.Go, key, row[1])
		}
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return normalize(string(raw))
}
