package search

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyName is the file an earlier build named a catalog's cache after: the source and the first 24 characters of
// the URL's base64.
func legacyName(source, url string) string {
	return source + "-" + base64.RawURLEncoding.EncodeToString([]byte(url))[:24] + ".cache"
}

// seedLegacy writes the cache file an earlier build left, stamped at.
func seedLegacy(t *testing.T, source, url, body string, at time.Time) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("CRW_HOME"), "skill-cache")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, legacyName(source, url))
	if err := os.WriteFile(file, []byte(body), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}
	return file
}

const hermesCatalog = "| [`apple-notes`](/docs/skills/apple-notes) | Manage Apple Notes | `apple/apple-notes` |\n"

func offline(string) (string, error) { return "", errors.New("down") }

// A catalog cached by the build before this one is still the cache of its URL: an upgrade must not turn a search that
// the cache answered into "every source unavailable".
func TestUpgradeKeepsServingTheCacheAnEarlierBuildWrote(t *testing.T) {
	t.Run("stale jaw cache, network down", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "jaw", JAWRegistryURL, cliRegistry, time.Now().Add(-48*time.Hour))
		code, out, errOut := cliRun([]string{"search", "telegram", "--source", "jaw", "--json"}, offline)
		if code != 0 || !strings.Contains(out, `"id": "telegram-send"`) || !strings.Contains(errOut, "serving stale cache") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("stale hermes cache, network down", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "hermes", HermesCatalogURL, hermesCatalog, time.Now().Add(-48*time.Hour))
		code, out, errOut := cliRun([]string{"search", "apple", "--source", "hermes", "--json"}, offline)
		if code != 0 || !strings.Contains(out, `"id": "apple-notes"`) || !strings.Contains(errOut, "serving stale cache") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("fresh jaw cache is read without the network", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "jaw", JAWRegistryURL, cliRegistry, time.Now())
		fetch := func(url string) (string, error) {
			t.Errorf("fetched %s although the cache is fresh", url)
			return "", errors.New("no network expected")
		}
		code, out, errOut := cliRun([]string{"search", "telegram", "--source", "jaw", "--json"}, fetch)
		if code != 0 || !strings.Contains(out, `"id": "telegram-send"`) || errOut != "" {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("a body that is not a catalog is not served", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "jaw", JAWRegistryURL, "<html>not a registry", time.Now().Add(-48*time.Hour))
		code, out, errOut := cliRun([]string{"search", "telegram", "--source", "jaw", "--json"}, offline)
		if code != 3 || out != "" || strings.Contains(errOut, "serving stale cache") {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("a fresh body that is not a catalog is read again", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "jaw", JAWRegistryURL, "<html>not a registry", time.Now())
		code, out, errOut := cliRun([]string{"search", "telegram", "--source", "jaw", "--json"}, cliFetch)
		if code != 0 || !strings.Contains(out, `"id": "telegram-send"`) || errOut != "" {
			t.Fatalf("%d %q %q", code, out, errOut)
		}
	})
	t.Run("the current name wins over the earlier one", func(t *testing.T) {
		cliHome(t)
		seedLegacy(t, "jaw", JAWRegistryURL, `{"skills":{"old-only":{"name":"Old","description":"old"}}}`, time.Now().Add(-48*time.Hour))
		// the network answers once: the build writes its own file and keeps it
		if code, _, errOut := cliRun([]string{"search", "x", "--source", "jaw", "--refresh"}, cliFetch); code != 0 || errOut != "" {
			t.Fatalf("%d %q", code, errOut)
		}
		code, out, _ := cliRun([]string{"search", "telegram", "--source", "jaw", "--json"}, offline)
		if code != 0 || !strings.Contains(out, `"id": "telegram-send"`) || strings.Contains(out, "old-only") {
			t.Fatalf("%d %q", code, out)
		}
	})
}

func TestCachedFetchTextFallsBackToTheLegacyKeyOnlyForABodyTheCallerAccepts(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	legacy := filepath.Join(dir, "old.cache")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(legacy, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	down := func() (string, error) { return "", errors.New("down") }
	var warnings strings.Builder
	got, err := CachedFetchText("new", down, CacheOptions{Dir: dir, Now: func() time.Time { return now }, LegacyKey: "old", Accept: func(b string) bool { return b == "legacy" }, Warnings: &warnings})
	if err != nil || got.Text != "legacy" || !got.Stale || !strings.Contains(warnings.String(), "for new;") {
		t.Fatalf("%+v %v %q", got, err, warnings.String())
	}
	if _, err := CachedFetchText("new", down, CacheOptions{Dir: dir, Now: func() time.Time { return now }, LegacyKey: "old", Accept: func(string) bool { return false }, Warnings: &warnings}); err == nil {
		t.Fatal("a body the caller refuses was served")
	}
	if _, err := CachedFetchText("new", down, CacheOptions{Dir: dir, Now: func() time.Time { return now }, LegacyKey: "../old", Warnings: &warnings}); err == nil {
		t.Fatal("an unsafe legacy key was read")
	}
}
