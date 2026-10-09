//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"
)

// spawnLoadRootsRecord is the file, in the worker's own harness scratch directory, the spawn shim records the load
// roots it made in. A test reads the roots back from it (CRW-978 c8).
const spawnLoadRootsRecord = "load-roots.txt"

// The spawn shim never writes to a path the caller's environment names. A CXCFUZZ_LOAD_ROOTS naming a writable file
// outside the case's scratch root, or a link to one, keeps its bytes whether the worker answers only its handshake,
// answers a case, or has no oracle tree to load (CRW-997 item 1, CRW-978 c8). Red before the shim stopped appending
// to the inherited path: the outside file gained a root.
func TestSpawnLoadRootsNamedByTheEnvironmentAreNeverWritten(t *testing.T) {
	requireNode(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	const original = "original\n"
	if err := os.WriteFile(outside, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, named := range []string{outside, link} {
		for _, withOracle := range []bool{false, true} {
			oracle := filepath.Join(t.TempDir(), "oracle")
			if withOracle {
				writeSpawnOracleAt(t, oracle)
			}
			env := append(append(os.Environ(), spawnDecoyEnvAt(t, t.TempDir(), t.TempDir(), true)...), "CXCFUZZ_LOAD_ROOTS="+named)
			pool := spawnFakePool(t, oracle, env)
			if _, err := pool.Call("null", ""); err != nil {
				t.Fatalf("the handshake answered no reply: %v", err)
			}
			if withOracle {
				if _, err := pool.Call(spawnMentionedFoldersCase(), t.TempDir()); err != nil {
					t.Fatalf("the case answered no reply: %v", err)
				}
			}
			if err := pool.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(outside)
			if err != nil || string(raw) != original {
				t.Fatalf("the file %s named by the environment changed to %q (%v), want it untouched (oracle %t)", named, raw, err, withOracle)
			}
		}
	}
}
