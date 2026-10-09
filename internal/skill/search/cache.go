package search

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

const DefaultTTL = time.Hour

// MaxBodyBytes limits both catalog parsing and cache reads/writes.
const MaxBodyBytes = 4 << 20

type CacheResult struct {
	Text  string
	Stale bool
}
type CacheOptions struct {
	Dir      string
	TTL      *time.Duration
	Refresh  bool
	Now      func() time.Time
	Warnings io.Writer
}

// CacheDir resolves CRW_HOME (used untrimmed) or ~/.crw, at call time.
func CacheDir(env host.LookupEnv) (string, error) {
	home, err := host.CRWHome(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skill-cache"), nil
}

// CachedFetchText keeps the oracle's stale-on-fetch-or-write-error behavior.
// Publication uses the existing atomic writer rather than an in-place overwrite.
func CachedFetchText(key string, fetcher func() (string, error), opts CacheOptions) (CacheResult, error) {
	if !safeComponent(key) {
		return CacheResult{}, fmt.Errorf("invalid skill cache key")
	}
	dir := opts.Dir
	if dir == "" {
		var err error
		dir, err = CacheDir(os.LookupEnv)
		if err != nil {
			return CacheResult{}, err
		}
	}
	ttl := DefaultTTL
	if opts.TTL != nil {
		ttl = *opts.TTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	file := filepath.Join(dir, key+".cache")
	if !opts.Refresh {
		// A file stamped in the future is not fresh: its age is negative, which no TTL bounds, so the oracle kept it
		// for the time left until that stamp plus the TTL. Only a stamp within the clock skew tolerance counts.
		if info, err := os.Stat(file); err == nil && fresh(time.Duration(now().UnixMilli()-info.ModTime().UnixMilli())*time.Millisecond, ttl) {
			if body, err := readCache(file); err == nil {
				return CacheResult{Text: body}, nil
			}
		}
	}
	body, err := fetcher()
	if err == nil && len(body) > MaxBodyBytes {
		err = fmt.Errorf("skill cache body exceeds %d bytes", MaxBodyBytes)
	}
	writeFailed := false // the body arrived and could not be stored, which is a disk problem and not a network one
	if err == nil {
		err = os.MkdirAll(dir, 0o777)
		writeFailed = err != nil
	}
	if err == nil {
		err = publishCache(file, []byte(body))
		// A file that is in place but whose directory could not be synced is a fresh cache entry: the cache has
		// no record that depends on it surviving a power failure, and the body is what the caller asked for (CRW-802).
		if crwdir.Published(err) {
			err = nil
		}
		writeFailed = err != nil
	}
	if err == nil {
		return CacheResult{Text: body}, nil
	}
	stale, readErr := readCache(file)
	if readErr != nil {
		return CacheResult{}, err
	}
	warnings := opts.Warnings
	if warnings == nil {
		warnings = os.Stderr
	}
	if writeFailed {
		_, _ = fmt.Fprintf(warnings, "skill-search: cache write failed for %s; the fetched catalog was not stored, serving stale cache (%s)\n", key, err)
	} else {
		_, _ = fmt.Fprintf(warnings, "skill-search: network fetch failed for %s; serving stale cache (%s)\n", key, err)
	}
	return CacheResult{Text: stale, Stale: true}, nil
}

// clockSkewTolerance is how far ahead of the clock a cache file's stamp may be and still count as written now.
const clockSkewTolerance = time.Minute

func fresh(age, ttl time.Duration) bool { return age >= -clockSkewTolerance && age < ttl }

// publishCache is the cache file's write; a test replaces it to stage a publication whose directory sync failed.
var publishCache = crwdir.Publish

func readCache(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, MaxBodyBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > MaxBodyBytes {
		return "", fmt.Errorf("skill cache body exceeds %d bytes", MaxBodyBytes)
	}
	return string(body), nil
}
