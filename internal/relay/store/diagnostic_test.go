package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// test_store.py Probe.
func TestProbe_python_properties(t *testing.T) {
	ctx := context.Background()
	t.Run("test_a_missing_state_directory_is_an_answer_and_is_not_created", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "nothing-here")
		selection, err := ResolveStateDir(absent, "")
		if err != nil {
			t.Fatal(err)
		}
		report := Probe(ctx, selection)
		if report.Access.DirectoryExists || report.Access.DBExists {
			t.Fatalf("report %+v", report)
		}
		if _, err := os.Stat(absent); !os.IsNotExist(err) {
			t.Fatalf("probe created what it describes: %v", err)
		}
	})
	t.Run("test_reported_writability_matches_what_this_process_can_really_do", func(t *testing.T) {
		locked := filepath.Join(t.TempDir(), "locked")
		s, err := Open(ctx, filepath.Join(locked, "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		// Asserted against a measured attempt, because a privileged runner ignores mode bits.
		reallyWritable := false
		if file, err := os.Create(filepath.Join(locked, ".really-writable")); err == nil {
			reallyWritable = true
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
		report := Probe(ctx, StateSelection{Path: locked})
		if report.Access.DirectoryWritable != reallyWritable || !report.Access.DBReadable {
			t.Fatalf("really writable %v report %+v", reallyWritable, report)
		}
		if !reallyWritable && report.Access.Detail == "" {
			t.Fatal("an unwritable directory must say why")
		}
	})
}
