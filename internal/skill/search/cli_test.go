package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

const cliRegistry = `{"skills":{"telegram-send":{"name":"Telegram Send","description":"send telegram messages","category":"communication"},"tdd":{"name":"TDD","description":"test driven development loop","superseded_by":"dev-testing"}}}`

func cliHome(t *testing.T) {
	t.Helper()
	t.Setenv("CRW_HOME", t.TempDir())
	t.Setenv("CRW_BIN", "crw")
}

func cliFetch(url string) (string, error) {
	if url == JAWRegistryURL {
		return cliRegistry, nil
	}
	return "# Telegram Send\nbody", nil
}

func cliRun(args []string, fetch FetchText) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Run(args, fetch, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCLIFlags(t *testing.T) {
	f := ParseFlags([]string{"telegram", "bot", "--source", "all", "--limit", "3", "--json", "--refresh"})
	want := Flags{"all", 3, true, true, []string{"telegram", "bot"}}
	if !reflect.DeepEqual(f, want) {
		t.Fatalf("%+v != %+v", f, want)
	}
	for _, c := range []struct {
		input string
		limit float64
	}{
		{"0", 10}, {"-9", 1}, {"0.5", 1}, {"2.9", 2.9}, {"Infinity", math.Inf(1)}, {"-Infinity", 1}, {"NaN", 10}, {"1e999", math.Inf(1)}, {"0x10", 16}, {"0b11", 3}, {"0o17", 15}, {"2x", 10}, {"+0x10", 10}, {"  ", 10}, {"inf", 10}, {"infinity", 10}, {"0x_10", 10}, {"--json", 10}, {"\uFEFF2.5", 2.5}, {"1e20", 1e20}, {"1e21", 1e21},
	} {
		t.Run(c.input, func(t *testing.T) {
			if got := ParseFlags([]string{"--limit", c.input}); got.Limit != c.limit || got.JSON {
				t.Fatalf("%+v want %g", got, c.limit)
			}
		})
	}
	for _, args := range [][]string{{"--source"}, {"--limit"}, {"--source", ""}, {"--wat", "--limit="}} {
		if got := ParseFlags(args); !reflect.DeepEqual(got.Rest, args) {
			t.Fatalf("%q => %+v", args, got)
		}
	}
}

func TestCLIPortedCases(t *testing.T) {
	cliHome(t)
	code, out, errOut := cliRun([]string{"search", "tdd", "--json"}, cliFetch)
	var rows []ScoredRow
	if json.Unmarshal([]byte(out), &rows) != nil || code != 0 || errOut != "" || len(rows) != 1 || rows[0].ID != "tdd" || optional(rows[0].SupersededBy) != "dev-testing" || rows[0].Score != 7 {
		t.Fatalf("%d %q %q %+v", code, out, errOut, rows)
	}
	_, out, _ = cliRun([]string{"search", "telegram"}, cliFetch)
	for _, want := range []string{"telegram-send (jaw", "raw.githubusercontent.com", "crw skill show"} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q lacks %q", out, want)
		}
	}
	code, out, errOut = cliRun([]string{"show", "telegram-send"}, cliFetch)
	if code != 0 || errOut != "" || !strings.HasPrefix(out, AdapterPreamble) || !strings.Contains(out, "# Telegram Send\nbody") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, errOut = cliRun([]string{"show", "nope"}, cliFetch)
	if code != 1 || out != "" || errOut != "skill-search: no skill \"nope\" in source(s) jaw\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, _ = cliRun(nil, cliFetch)
	if code != 0 || out != Usage+"\n" {
		t.Fatalf("%d %q", code, out)
	}
}

// Recorded by Node v24 from v0.2.40 cli.ts, with only the declared name substitutions.
func TestCLIRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/cli-oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Args           []string
		Exit           int
		Stdout, Stderr string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			cliHome(t)
			fetch := func(url string) (string, error) {
				switch {
				case url == JAWRegistryURL:
					return `{"skills":{"x":{"name":"X","description":"x ` + strings.Repeat("😀", 64) + `","requires":{"bins":["git"]},"status":"old","superseded_by":"y"}}}`, nil
				case url == HermesCatalogURL:
					return "", errors.New("offline")
				case strings.Contains(url, "/search?"):
					return `{"results":[{"slug":"x","summary":"market"}]}`, nil
				default:
					return "# Body", nil
				}
			}
			code, out, errOut := cliRun(c.Args, fetch)
			if code != c.Exit || out != c.Stdout || errOut != c.Stderr {
				t.Fatalf("args=%q\nexit=%d want%d\nstdout=%q want%q\nstderr=%q want%q", c.Args, code, c.Exit, out, c.Stdout, errOut, c.Stderr)
			}
		})
	}
}

func TestCLICacheAndFreshBody(t *testing.T) {
	cliHome(t)
	catalogs, bodies := 0, 0
	fetch := func(url string) (string, error) {
		if url == JAWRegistryURL {
			catalogs++
			return cliRegistry, nil
		}
		bodies++
		return fmt.Sprint(bodies), nil
	}
	for i := 0; i < 2; i++ {
		if code, _, _ := cliRun([]string{"show", "telegram-send", "--json"}, fetch); code != 0 {
			t.Fatal(code)
		}
	}
	if catalogs != 1 || bodies != 2 {
		t.Fatalf("catalogs=%d bodies=%d", catalogs, bodies)
	}
	cliRun([]string{"search", "tdd", "--refresh"}, fetch)
	if catalogs != 2 {
		t.Fatal(catalogs)
	}
	dir, _ := CacheDir(os.LookupEnv)
	file := filepath.Join(dir, "jaw-aHR0cHM6Ly9yYXcuZ2l0aHVi.cache")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := cliRun([]string{"search", "telegram", "--json"}, func(string) (string, error) { return "", errors.New("offline") })
	if code != 0 || !strings.Contains(out, "telegram-send") || !strings.Contains(errOut, "serving stale cache (offline)") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	cliRun([]string{"search", "tdd"}, fetch)
	if catalogs != 3 {
		t.Fatal(catalogs)
	}
	code, _, errOut = cliRun([]string{"show", "telegram-send"}, func(string) (string, error) { return "", errors.New("body unavailable") })
	if code != 1 || errOut != "skill-search error: body unavailable\n" {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestCLISourceBranches(t *testing.T) {
	cliHome(t)
	requests := []string{}
	fetch := func(url string) (string, error) {
		requests = append(requests, url)
		switch {
		case url == JAWRegistryURL:
			return cliRegistry, nil
		case url == HermesCatalogURL:
			return "| [`tdd`](x) | tdd | `development/tdd` |", nil
		case strings.Contains(url, "/search?"):
			return `{"results":[{"slug":"tdd","summary":"market"},{"slug":"remote","summary":"tdd"},{"slug":"other"}]}`, nil
		default:
			return "fresh", nil
		}
	}
	code, out, errOut := cliRun([]string{"search", "tdd", "--source", "all", "--limit", "2.9", "--json"}, fetch)
	var rows []ScoredRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if code != 0 || errOut != "" || len(requests) != 3 || len(rows) != 2 || rows[0].Source != SourceHermes || rows[1].Source != SourceJaw {
		t.Fatalf("%d %q %+v %q", code, errOut, rows, requests)
	}
	code, out, _ = cliRun([]string{"search", "tdd", "--source", "clawhub", "--limit", "1", "--json"}, fetch)
	if json.Unmarshal([]byte(out), &rows) != nil || code != 0 || len(rows) != 1 || rows[0].Score != 3 {
		t.Fatal(out)
	}
	for _, source := range []string{"all", "gh", "clawhub"} {
		if code, out, errOut := cliRun([]string{"show", "remote", "--source", source}, fetch); code != 0 || !strings.Contains(out, "fresh") || errOut != "" {
			t.Fatalf("%s %d %q %q", source, code, out, errOut)
		}
	}
	cliHome(t)
	code, out, errOut = cliRun([]string{"search", "x", "--source", "hermes", "--json"}, func(string) (string, error) { return "", errors.New("down") })
	if code != 0 || out != "[]\n" || errOut != "skill-search: source hermes failed (down)\n" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	code, out, errOut = cliRun([]string{"search", "missing"}, cliFetch)
	if code != 0 || out != "no matching skills\n" || errOut != "" {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
}

func TestGHLaunchCases(t *testing.T) {
	status := func(n int) *int { return &n }
	for _, c := range []struct {
		name   string
		result GHResult
		want   string
	}{
		{"missing", GHResult{Error: syscall.ENOENT}, "gh is not on PATH"},
		{"permission", GHResult{Error: fmt.Errorf("spawn gh EACCES: %w", syscall.EACCES)}, "gh could not be launched: spawn gh EACCES"},
		{"auth", GHResult{Status: status(4), Stderr: "auth required"}, "gh code search failed (auth required)"},
		{"auth hint", GHResult{Status: status(1)}, "gh exited 1 - try `gh auth status`"},
		{"empty output", GHResult{Status: status(0)}, "gh exited 0 - try `gh auth status`"},
		{"signal", GHResult{}, "gh exited null"},
		{"invalid json", GHResult{Status: status(0), Stdout: "[oops"}, ""},
		{"null item", GHResult{Status: status(0), Stdout: "[null]"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var errOut bytes.Buffer
			rows := GHSearch("telegram", 5, func(string, []string) GHResult { return c.result }, &errOut)
			if len(rows) != 0 || !strings.Contains(errOut.String(), c.want) {
				t.Fatalf("%+v %q", rows, errOut.String())
			}
			if c.want == "" && errOut.Len() != 0 {
				t.Fatal(errOut.String())
			}
		})
	}
	for _, c := range []struct {
		limit float64
		arg   string
	}{{2.5, "2.5"}, {math.Inf(1), "Infinity"}, {1e20, "100000000000000000000"}, {1e21, "1e+21"}} {
		GHSearch("x", c.limit, func(file string, args []string) GHResult {
			if file != "gh" || args[4] != c.arg {
				t.Fatalf("%s %q", file, args)
			}
			return GHResult{Status: status(0), Stdout: "[]"}
		}, &bytes.Buffer{})
	}
}

func TestGHFakePATH(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	var errOut bytes.Buffer
	if rows := GHSearch("x", 5, nil, &errOut); len(rows) != 0 || !strings.Contains(errOut.String(), "not on PATH") {
		t.Fatal(errOut.String())
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GH_ARGS\"\nprintf '%s' \"$GH_OUT\"\nprintf '%s' \"$GH_ERR\" >&2\nexit \"$GH_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(dir, "args")
	t.Setenv("GH_ARGS", argsFile)
	t.Setenv("GH_OUT", `[{"repository":{"nameWithOwner":"owner/repo"},"path":"skills/telegram-send/SKILL.md"},{"repository":{"nameWithOwner":"bob/tools"},"path":"SKILL.md"}]`)
	t.Setenv("GH_ERR", "")
	t.Setenv("GH_EXIT", "0")
	sentinel := filepath.Join(dir, "not-created")
	query := "telegram; touch " + sentinel + " $(exit 7) `exit 8`"
	errOut.Reset()
	rows := GHSearch(query, 5, nil, &errOut)
	if len(rows) != 2 || rows[0].ID != "telegram-send" || rows[1].ID != "bob/tools" || rows[0].Source != SourceGH || rows[0].Score != 2 || errOut.Len() != 0 {
		t.Fatalf("%+v %q", rows, errOut.String())
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "search\ncode\nfilename:SKILL.md " + query + "\n--limit\n5\n--json\nrepository,path\n"
	if string(args) != want {
		t.Fatalf("%q != %q", args, want)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("query executed: %v", err)
	}
	t.Setenv("GH_EXIT", "4")
	t.Setenv("GH_ERR", "auth required")
	errOut.Reset()
	if len(GHSearch("x", 5, nil, &errOut)) != 0 || !strings.Contains(errOut.String(), "auth required") {
		t.Fatal(errOut.String())
	}
}

func TestHTTPFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "crw-skill-search" {
			t.Error(r.Header)
		}
		if r.URL.Path == "/status" {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("\uFEFFa\xff\xff<>&\u2028"))
	}))
	defer server.Close()
	body, err := fetchHTTP(server.URL)
	if err != nil || body != "a��<>&\u2028" {
		t.Fatalf("%q %v", body, err)
	}
	if _, err := fetchHTTP(server.URL + "/status"); err == nil || err.Error() != "HTTP 503 for "+server.URL+"/status" {
		t.Fatal(err)
	}
	server.Close()
	if _, err := fetchHTTP(server.URL); err == nil || err.Error() != "fetch failed" {
		t.Fatal(err)
	}
}
