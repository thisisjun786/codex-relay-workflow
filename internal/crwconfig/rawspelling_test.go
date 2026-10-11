package crwconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// CRW-958 fix round 3: a relay_state spelled through a symbolic link and ".." names the directory the
// filesystem gives that spelling, and the relay's selection keeps the spelling. The storage check reads
// the store's directories, databases and sidecars through that same spelling, so a WAL kept in another
// directory is a state_split error even though the lexically cleaned directory does not exist.
func TestCheckStateSplitReadsTheRawSpelling(t *testing.T) {
	_, env := rootsHome(t)
	base := shortDir(t)
	for _, dir := range []string{"a", "b/deep", "b/rs/0123456789abcdef", "b/elsewhere"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "b", "rs", "0123456789abcdef")
	if err := os.WriteFile(filepath.Join(store, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wal := filepath.Join(base, "b", "elsewhere", "relay.sqlite3-wal")
	if err := os.WriteFile(wal, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(wal, filepath.Join(store, "relay.sqlite3-wal")); err != nil {
		t.Fatal(err)
	}
	spelled := base + "/a/link/../rs"
	file, err := Load(env, configWrite(t, `{"paths":{"relay_state":"`+spelled+`"}}`))
	if err != nil {
		t.Fatal(err)
	}
	mustFind(t, Check(file, env, roomy()), CheckStateSplit, RootRelayState, LevelError)
	if _, err := os.Stat(filepath.Join(base, "a", "rs")); err == nil {
		t.Error("the lexically cleaned directory exists; the fixture does not tell the spellings apart")
	}
}
