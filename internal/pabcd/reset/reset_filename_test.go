package reset

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// CRW-1079. The oracle lists sessions/ with readdirSync, which decodes each name as UTF-8 (every
// invalid sequence becomes U+FFFD), and builds the path it removes from that string (reset.ts:62-63).
// A name that is not UTF-8 therefore never names the raw file. The expected removed/absent lists are
// runReset from the oracle's dist/reset.js, run on the same directories.
func TestResetStateDecodesSessionNamesLikeNode(t *testing.T) {
	const rawName = "\xff.json" // the bytes ff 2e 6a 73 6f 6e
	const replaced = "�.json"
	for _, tc := range []struct {
		name            string
		files           []string
		removed, absent []string // sessions/ entries, in order
		left            []string
	}{
		{"raw alone is absent and kept", []string{rawName}, nil, []string{replaced}, []string{rawName}},
		{"raw beside U+FFFD.json removes only the real file", []string{rawName, replaced}, []string{replaced}, []string{replaced}, []string{rawName}},
		{"a valid name is removed beside a raw one", []string{"s1.json", rawName}, []string{"s1.json"}, []string{replaced}, []string{rawName}},
		{"two raw names both decode to the same absent path", []string{rawName, "\xfe.json"}, nil, []string{replaced, replaced}, []string{"\xfe.json", rawName}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sessions := filepath.Join(root, ".crw", "sessions")
			if err := os.MkdirAll(sessions, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.files {
				if err := os.WriteFile(filepath.Join(sessions, name), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := RunReset(root, State)
			if err != nil {
				t.Fatal(err)
			}
			under := func(names []string) []string {
				var paths []string
				for _, name := range names {
					paths = append(paths, filepath.Join(sessions, name))
				}
				return paths
			}
			base := filepath.Join(root, ".crw")
			wantAbsent := append(under(tc.absent), filepath.Join(base, "ledger.jsonl"), filepath.Join(base, "interviews"), filepath.Join(base, "affordance-recovery"))
			if !reflect.DeepEqual(got.Removed, under(tc.removed)) && !(len(got.Removed) == 0 && len(tc.removed) == 0) {
				t.Errorf("removed = %q, want %q", got.Removed, under(tc.removed))
			}
			if !reflect.DeepEqual(got.Absent, wantAbsent) {
				t.Errorf("absent = %q, want %q", got.Absent, wantAbsent)
			}
			entries, err := os.ReadDir(sessions)
			if err != nil {
				t.Fatal(err)
			}
			var left []string
			for _, entry := range entries {
				left = append(left, entry.Name())
			}
			if !reflect.DeepEqual(left, tc.left) {
				t.Errorf("left in sessions/ = %q, want %q", left, tc.left)
			}
		})
	}
}
