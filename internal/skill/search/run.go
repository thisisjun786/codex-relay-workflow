package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// Limits of one command. Every remote read runs under the deadline of its source and of the whole command, and
// reads at most a fixed number of bytes; none of them waits on a server for ever or holds a body in memory
// without a bound.
var (
	// sourceTimeout bounds one source of a search (its catalog reads, or the gh run) and one read of `show`.
	sourceTimeout = 20 * time.Second
	// commandTimeout bounds the whole command, whatever the number of sources or reads.
	commandTimeout = 60 * time.Second
	// maxConcurrentSources bounds how many sources of one search are read at the same time.
	maxConcurrentSources = 4
)

// MaxShowBodyBytes limits one skill body that `show` reads; a catalog (and a cache file) is limited by MaxBodyBytes.
const MaxShowBodyBytes = 1 << 20

// exitInterrupted is the status of a command its caller cancelled.
const exitInterrupted = 130

// exitUnavailable is the status of a search whose every selected source was unavailable.
const exitUnavailable = 3

// fetchFunc reads one URL under ctx and returns a body of at most limit bytes, or an error that says why not.
type fetchFunc func(ctx context.Context, url string, limit int) (string, error)

func contextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("fetch timed out")
	}
	return errors.New("fetch canceled")
}

func exceedsError(limit int) error { return fmt.Errorf("response exceeds %d bytes", limit) }

// legacyFetch adapts an injected FetchText, which takes no context: a call that outlives ctx is abandoned.
func legacyFetch(fetch FetchText) fetchFunc {
	return func(ctx context.Context, url string, limit int) (string, error) {
		type answer struct {
			body string
			err  error
		}
		done := make(chan answer, 1)
		go func() { body, err := fetch(url); done <- answer{body, err} }()
		select {
		case a := <-done:
			if a.err == nil && len(a.body) > limit {
				return "", exceedsError(limit)
			}
			return a.body, a.err
		case <-ctx.Done():
			return "", contextError(ctx)
		}
	}
}

var httpClient = &http.Client{}

// fetchHTTP reads url with ctx on the request, so the connect, the headers and the body are all under its deadline,
// and reads at most limit bytes of the decoded body. A longer body is refused, never cut.
func fetchHTTP(ctx context.Context, url string, limit int) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "crw-skill-search")
	res, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", contextError(ctx)
		}
		return "", errors.New("fetch failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("HTTP %d for %s", res.StatusCode, url)
	}
	if res.ContentLength > int64(limit) {
		return "", exceedsError(limit)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		if ctx.Err() != nil {
			return "", contextError(ctx)
		}
		return "", err
	}
	if len(body) > limit {
		return "", exceedsError(limit)
	}
	// Response.text strips a UTF-8 BOM and replaces invalid UTF-8.
	return strings.TrimPrefix(source.DecodeUTF8(body), "\uFEFF"), nil
}

// Run is the CLI boundary without a caller context.
func Run(argv []string, fetch FetchText, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), argv, fetch, stdout, stderr)
}

// RunContext is the CLI boundary. It replaces the oracle's direct-exec guard and rejection handler. A nil fetch
// reads over HTTP; the context reaches every request, and cancelling it ends the command with status 130.
func RunContext(ctx context.Context, argv []string, fetch FetchText, stdout, stderr io.Writer) int {
	read := fetchFunc(fetchHTTP)
	if fetch != nil {
		read = legacyFetch(fetch)
	}
	code, err := runCLI(ctx, argv, read, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "skill-search error: %s\n", err)
		return 1
	}
	return code
}

var searchSources = []string{"jaw", "hermes", "clawhub", "gh"}

// sourceResult is what one source of a search answered. log holds the lines the source wrote to stderr, which are
// written after every source has ended, in the order the sources were named.
type sourceResult struct {
	rows []ScoredRow
	err  error // set when the source was unavailable
	log  bytes.Buffer
}

func searchSource(ctx context.Context, source, query string, f Flags, fetch fetchFunc) *sourceResult {
	r := &sourceResult{rows: []ScoredRow{}}
	switch source {
	case "gh":
		rows, failed := ghSearch(ctx, query, f.Limit, nil, &r.log)
		if failed {
			if r.log.Len() == 0 {
				r.err = errors.New("gh output was not valid JSON")
			} else {
				r.err = errors.New("gh failed")
			}
		}
		r.rows = rows
	case "clawhub":
		r.rows, r.err = clawhubSearch(ctx, query, fetch)
	default:
		var catalog []SkillRow
		catalog, r.err = loadSource(ctx, source, f.Refresh, fetch, &r.log)
		if r.err == nil {
			r.rows = Rank(catalog, query, len(catalog)) // the limit applies to the merged list
		}
	}
	if r.err != nil && ctx.Err() != nil {
		r.err = contextError(ctx)
	}
	return r
}

// searchAll reads the independent sources at the same time, at most maxConcurrentSources at once, each under its
// own deadline; the results come back in the order of wanted, whichever source answered first.
func searchAll(ctx context.Context, wanted []string, query string, f Flags, fetch fetchFunc) []*sourceResult {
	results := make([]*sourceResult, len(wanted))
	slots := make(chan struct{}, max(1, maxConcurrentSources))
	var wg sync.WaitGroup
	for i, name := range wanted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
			defer cancel()
			results[i] = searchSource(sctx, name, query, f, fetch)
		}()
	}
	wg.Wait()
	return results
}

func runCLI(ctx context.Context, argv []string, fetch fetchFunc, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		fmt.Fprintln(stdout, Usage)
		return 0, nil
	}
	switch argv[0] {
	case "--help", "-h", "help":
		fmt.Fprintln(stdout, Help)
		return 0, nil
	}
	f := ParseFlags(argv[1:])
	if argv[0] == "search" || argv[0] == "show" {
		// A bad option is reported before --help is honoured: --help never turns a malformed command into a success.
		if f.Err != "" {
			fmt.Fprintf(stderr, "skill-search: %s\n%s\nplace -- before a query word or id that starts with -; --help explains the options\n", f.Err, Usage)
			return 2, nil
		}
		if f.Help {
			fmt.Fprintln(stdout, Help)
			return 0, nil
		}
	}
	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	switch argv[0] {
	case "search":
		query := text.Trim(strings.Join(f.Rest, " "))
		if query == "" {
			fmt.Fprintln(stdout, Usage)
			return 1, nil
		}
		wanted := []string{f.Source}
		if f.Source == "all" {
			wanted = []string{"jaw", "hermes", "clawhub"}
		}
		if !slices.Contains(searchSources, f.Source) && f.Source != "all" {
			fmt.Fprintf(stderr, "skill-search: unknown source \"%s\"\n", f.Source)
			return 1, nil
		}
		results := searchAll(cmdCtx, wanted, query, f, fetch)
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "skill-search: interrupted")
			return exitInterrupted, nil
		}
		lists := [][]ScoredRow{}
		unavailable := 0
		for i, r := range results {
			_, _ = stderr.Write(r.log.Bytes())
			if r.err != nil {
				unavailable++
				if r.log.Len() == 0 || wanted[i] != "gh" {
					fmt.Fprintf(stderr, "skill-search: source %s failed (%s)\n", wanted[i], r.err)
				}
				continue
			}
			lists = append(lists, r.rows)
		}
		if unavailable == len(wanted) {
			if len(wanted) > 1 {
				fmt.Fprintf(stderr, "skill-search: all selected sources unavailable (%s)\n", strings.Join(wanted, ", "))
			}
			return exitUnavailable, nil
		}
		var rows []ScoredRow
		if len(wanted) == 1 {
			rows = lists[0] // one named source has nothing to merge: its native order stands, as it always did
		} else {
			rows = mergeSources(query, lists)
		}
		out, err := renderRows(rows[:sliceLimit(f.Limit, len(rows))], f.JSON)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, out)
		return 0, nil
	case "show":
		if len(f.Rest) == 0 || f.Rest[0] == "" {
			fmt.Fprintln(stdout, Usage)
			return 1, nil
		}
		id := f.Rest[0]
		wanted := []string{f.Source}
		switch f.Source {
		case "all":
			wanted = []string{"jaw", "hermes", "clawhub"}
		case "gh":
			// gh searches code and has no catalog to look an id up in; show used to read the other three instead.
			fmt.Fprintf(stderr, "skill-search: show does not support source \"gh\" (use jaw, hermes, clawhub or all)\n")
			return 1, nil
		}
		for _, name := range wanted {
			row, found := findSkill(cmdCtx, name, id, f.Refresh, fetch, stderr)
			if !found {
				continue
			}
			bctx, stop := context.WithTimeout(cmdCtx, sourceTimeout)
			body, err := fetch(bctx, row.RawURL, MaxShowBodyBytes)
			stop()
			if err != nil {
				if ctx.Err() != nil { // the caller cancelled while the body was read: the same status as search
					fmt.Fprintln(stderr, "skill-search: interrupted")
					return exitInterrupted, nil
				}
				return 1, err
			}
			if f.JSON {
				value := struct {
					SkillRow
					Body     string `json:"body"`
					Preamble string `json:"preamble"`
				}{row, body, AdapterPreamble}
				b, err := role.Stringify(value, "")
				if err != nil {
					return 1, err
				}
				fmt.Fprintln(stdout, string(b))
			} else {
				fmt.Fprintf(stdout, "%s\n--- %s (%s) %s\n\n%s\n", AdapterPreamble, row.ID, row.Source, row.RawURL, body)
			}
			return 0, nil
		}
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "skill-search: interrupted")
			return exitInterrupted, nil
		}
		fmt.Fprintf(stderr, "skill-search: no skill \"%s\" in source(s) %s\n", id, strings.Join(wanted, ","))
		return 1, nil
	}
	fmt.Fprintln(stdout, Usage)
	return 1, nil
}

// findSkill looks id up in one source of `show`. A catalog that cannot be read is skipped, as it always was.
func findSkill(ctx context.Context, name, id string, refresh bool, fetch fetchFunc, stderr io.Writer) (SkillRow, bool) {
	sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	var rows []SkillRow
	var err error
	switch name {
	case "jaw", "hermes":
		rows, err = loadSource(sctx, name, refresh, fetch, stderr)
	case "clawhub":
		rows, err = SearchClawhubRows(func(url string) (string, error) { return fetch(sctx, url, MaxBodyBytes) }, id)
	default:
		return SkillRow{}, false
	}
	if err != nil {
		return SkillRow{}, false
	}
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return SkillRow{}, false
}
