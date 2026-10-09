package search

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCacheWriteFailureIsNotReportedAsANetworkFailure(t *testing.T) {
	saved := publishCache
	t.Cleanup(func() { publishCache = saved })
	publishCache = func(string, []byte) error { return errors.New("no space left on device") }
	dir := t.TempDir()
	now := time.Unix(2000000000, 0)
	writeCache(t, dir, "old", now.Add(-48*time.Hour))
	var warnings bytes.Buffer
	got, err := CachedFetchText("k", func() (string, error) { return "net", nil }, CacheOptions{Dir: dir, Refresh: true, Now: func() time.Time { return now }, Warnings: &warnings})
	if err != nil || got.Text != "old" || !got.Stale {
		t.Fatalf("%+v %v", got, err)
	}
	w := warnings.String()
	if strings.Contains(w, "network fetch failed") || !strings.Contains(w, "cache write failed for k") || !strings.Contains(w, "no space left on device") || !strings.Contains(w, "serving stale cache") {
		t.Fatalf("%q", w)
	}
}

func TestCacheFutureModTimeIsNotFresh(t *testing.T) {
	now := time.Unix(2000000000, 0)
	for _, c := range []struct {
		name  string
		ahead time.Duration
		calls int
	}{{"an hour ahead", time.Hour, 1}, {"a year ahead", 365 * 24 * time.Hour, 1}, {"seconds of clock skew", 20 * time.Second, 0}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCache(t, dir, "old", now.Add(c.ahead))
			calls := 0
			got, err := CachedFetchText("k", func() (string, error) { calls++; return "new", nil }, CacheOptions{Dir: dir, Now: func() time.Time { return now }})
			if err != nil || calls != c.calls {
				t.Fatalf("%v calls=%d", err, calls)
			}
			if c.calls == 1 && got.Text != "new" {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCacheKeyHashesTheWholeURLAndSource(t *testing.T) {
	a := CacheKey("jaw", JAWRegistryURL)
	for name, other := range map[string]string{
		"a URL that differs after the first 18 bytes": CacheKey("jaw", JAWRegistryURL+"?v=2"),
		"another source, the same URL":                CacheKey("hermes", JAWRegistryURL),
		"another path on the same host":               CacheKey("jaw", JAWRawBase+"/other.json"),
	} {
		if a == other {
			t.Errorf("%s shares the key %s", name, a)
		}
	}
	if !safeComponent(a) || len(a) > 100 || !strings.HasPrefix(a, "jaw-") || a != CacheKey("jaw", JAWRegistryURL) {
		t.Errorf("key %q", a)
	}
}

func TestHelpAndUnknownOptionsAnswerWithoutTheNetwork(t *testing.T) {
	boom := func(url string) (string, error) {
		t.Errorf("fetched %s", url)
		return "", errors.New("no network expected")
	}
	for _, args := range [][]string{{"search", "--help"}, {"search", "tdd", "-h"}, {"show", "--help"}, {"show", "x", "-h", "--json"}, {"--help"}, {"help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cliHome(t)
			code, out, errOut := cliRun(args, boom)
			if code != 0 || out != Help+"\n" || errOut != "" {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
		})
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "--wat", "tdd"}, `unknown option "--wat"`},
		{[]string{"search", "tdd", "--limit=3"}, `unknown option "--limit=3"`},
		{[]string{"show", "x", "-z"}, `unknown option "-z"`},
		{[]string{"search", "tdd", "--source"}, `option --source needs a value`},
		{[]string{"search", "tdd", "--limit", ""}, `option --limit needs a value`},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			cliHome(t)
			code, out, errOut := cliRun(c.args, boom)
			if code != 2 || out != "" || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, Usage) || !strings.Contains(errOut, "--help") {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
		})
	}
}

func TestDoubleDashMakesTheRestLiteralQueryWords(t *testing.T) {
	cliHome(t)
	var urls []string
	fetch := func(url string) (string, error) {
		urls = append(urls, url)
		return `{"skills":{"dash-help":{"name":"--help","description":"the --help flag"}}}`, nil
	}
	code, out, errOut := cliRun([]string{"search", "--json", "--", "--help"}, fetch)
	if code != 0 || errOut != "" || !strings.Contains(out, `"id": "dash-help"`) || len(urls) != 1 {
		t.Fatalf("%d %q %q %v", code, out, errOut, urls)
	}
	cliHome(t)
	code, out, errOut = cliRun([]string{"search", "-", "--", "--json"}, fetch)
	if code != 0 || errOut != "" || strings.HasPrefix(out, "[") {
		t.Fatalf("--json after -- is a query word, not the flag: %d %q %q", code, out, errOut)
	}
}

func TestShowRefusesASourceItCannotQuery(t *testing.T) {
	cliHome(t)
	boom := func(url string) (string, error) {
		t.Errorf("fetched %s", url)
		return "", errors.New("no network expected")
	}
	code, out, errOut := cliRun([]string{"show", "telegram-send", "--source", "gh"}, boom)
	if code != 1 || out != "" || errOut != "skill-search: show does not support source \"gh\" (use jaw, hermes, clawhub or all)\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

// An option that needs a value never takes another option, "--" or a flag-like word for it: the command ends with
// status 2 before anything is read. A negative number is still a value of --limit.
func TestOptionValueIsNeverAnotherOptionOrTheDoubleDash(t *testing.T) {
	boom := func(url string) (string, error) {
		t.Errorf("fetched %s", url)
		return "", errors.New("no network expected")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"search", "tdd", "--limit", "--refresh"}, "option --limit needs a value"},
		{[]string{"search", "tdd", "--source", "--json"}, "option --source needs a value"},
		{[]string{"search", "tdd", "--limit", "--", "--help"}, "option --limit needs a value"},
		{[]string{"search", "tdd", "--source", "--"}, "option --source needs a value"},
		{[]string{"show", "x", "--source", "--refresh"}, "option --source needs a value"},
		{[]string{"search", "tdd", "--limit", "-x"}, "option --limit needs a value"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			cliHome(t)
			code, out, errOut := cliRun(c.args, boom)
			if code != 2 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("%d %q %q", code, out, errOut)
			}
		})
	}
	for _, v := range []string{"-5", "-1.5", "-.5", "-Infinity", "-1e3"} {
		if f := ParseFlags([]string{"--limit", v, "q"}); f.Err != "" || f.Limit != 1 || len(f.Rest) != 1 {
			t.Errorf("--limit %s => %+v", v, f)
		}
	}
}
