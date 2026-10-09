package search

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A cache file that is in place but whose directory could not be synced is a fresh entry: the body is returned as
// fetched, nothing is served stale and no warning is printed (CRW-802).
func TestCachePublishedWithoutDirectorySyncIsFresh(t *testing.T) {
	saved := publishCache
	t.Cleanup(func() { publishCache = saved })
	publishCache = func(path string, data []byte) error {
		if err := os.WriteFile(path, data, 0o666); err != nil {
			return err
		}
		return &crwdir.PublishedError{Err: errors.New("injected directory sync failure")}
	}
	dir := t.TempDir()
	var warnings bytes.Buffer
	got, err := CachedFetchText("k", func() (string, error) { return "net", nil }, CacheOptions{Dir: dir, Refresh: true, Now: func() time.Time { return time.Unix(2000000000, 0) }, Warnings: &warnings})
	if err != nil || got.Text != "net" || got.Stale || warnings.Len() != 0 {
		t.Fatalf("%+v %v warnings %q", got, err, warnings.String())
	}
}

// A publication that failed before the rename keeps the stale-cache fallback.
func TestCacheUnpublishedWriteStillFallsBackToStale(t *testing.T) {
	saved := publishCache
	t.Cleanup(func() { publishCache = saved })
	publishCache = func(string, []byte) error { return errors.New("injected write failure") }
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	writeCache(t, dir, "old", now.Add(-48*time.Hour))
	var warnings bytes.Buffer
	got, err := CachedFetchText("k", func() (string, error) { return "net", nil }, CacheOptions{Dir: dir, Refresh: true, Now: func() time.Time { return now }, Warnings: &warnings})
	if err != nil || got.Text != "old" || !got.Stale || warnings.Len() == 0 {
		t.Fatalf("%+v %v warnings %q", got, err, warnings.String())
	}
}
