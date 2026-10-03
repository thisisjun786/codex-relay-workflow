package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// test_store.py Probe.
func TestProbe_python_properties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("test_a_missing_state_directory_is_an_answer_and_is_not_created", func(t *testing.T) {
		t.Parallel()
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
		t.Parallel()
		locked := filepath.Join(t.TempDir(), "locked")
		s, err := fixtureOpen(ctx, filepath.Join(locked, "relay.sqlite3"), "")
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
		// Judged by the default probe and measured by the write probe, the same answer.
		for _, opts := range []ProbeOptions{{}, {Write: true}} {
			report := ProbeWith(ctx, StateSelection{Path: locked}, opts)
			if report.Access.DirectoryWritable != reallyWritable || !report.Access.DBReadable || report.Access.Measured != opts.Write {
				t.Fatalf("really writable %v report %+v (write probe %v)", reallyWritable, report, opts.Write)
			}
			if !reallyWritable && report.Access.Detail == "" {
				t.Fatalf("an unwritable directory must say why (write probe %v)", opts.Write)
			}
		}
	})
}
