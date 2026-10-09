package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// Usage is cli.ts's usage under the CRW name table.
const Usage = "crw skill <search <query...> [--source jaw|hermes|clawhub|gh|all] [--limit N] [--json] [--refresh] | show <id> [--source ...]>"

// Help is what --help prints (stdout, exit 0, no network), and so the caller's documentation of the command.
const Help = Usage + `

Search external skill catalogs and print one skill. Nothing is installed.

  search <query...>  rank the skills that match the query
  show <id>          print one skill's body, behind the external-skill adapter preamble

Options:
  --source S  jaw (default), hermes, clawhub, gh, or all (jaw, hermes and clawhub); show reads jaw, hermes, clawhub or all
  --limit N   at most N rows (default 10)
  --json      print JSON: search a list of rows, show one object with body and preamble
  --refresh   read a catalog again although its cache is fresh
  --help, -h  print this text
  --          end of options; every word after it is a query word or id, even one that starts with -

Each remote read ends at a deadline (20 s per source, 60 s per command) and is limited in size (a catalog 4 MiB,
a skill body 1 MiB); a read over its limit is refused, never cut. A search over several sources reads them at the
same time and keeps one list: each source keeps its own order and the lists are merged by reciprocal rank (jaw and
hermes weigh 1, clawhub 0.8, gh 0.6), an exact match of the whole query on a skill's id, then on its name, first. A
row's score is the score its own source gave it, comparable only with rows of that source; the same id from two
sources stays two rows.

Exit status: 0 done (a source that answered with no rows is done); 1 no such skill, or an error; 2 a bad option
(nothing was read); 3 every selected source was unavailable (nothing on stdout; stderr names each failure; a
source served from a stale cache is available); 130 interrupted.`

type Flags struct {
	Source        string
	Limit         float64
	JSON, Refresh bool
	Help          bool
	Err           string // the first bad option, in words
	Rest          []string
}

// ParseFlags reads the options. --help and -h set Help; -- ends the options; an option it does not know, an option
// without its value and an --option=value are left in Err, never taken for query words.
func ParseFlags(argv []string) Flags {
	f := Flags{Source: "jaw", Limit: 10, Rest: []string{}}
	fail := func(format string, args ...any) {
		if f.Err == "" {
			f.Err = fmt.Sprintf(format, args...)
		}
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--":
			f.Rest = append(f.Rest, argv[i+1:]...)
			return f
		case a == "--help" || a == "-h":
			f.Help = true
		case a == "--source" || a == "--limit":
			if i+1 >= len(argv) || argv[i+1] == "" {
				fail("option %s needs a value", a)
				if i+1 < len(argv) {
					i++ // the empty value belongs to the option
				}
				continue
			}
			i++
			if a == "--source" {
				f.Source = argv[i]
			} else {
				f.Limit = numberLimit(argv[i])
			}
		case a == "--json":
			f.JSON = true
		case a == "--refresh":
			f.Refresh = true
		case len(a) > 1 && a[0] == '-':
			fail("unknown option %q", a)
		default:
			f.Rest = append(f.Rest, a)
		}
	}
	return f
}

// Math.max(1, Number(token) || 10), including non-decimal integers and overflow.
func numberLimit(token string) float64 {
	s := text.Trim(token)
	n := math.NaN()
	switch {
	case s == "Infinity" || s == "+Infinity":
		n = math.Inf(1)
	case s == "-Infinity":
		n = math.Inf(-1)
	case strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") || strings.HasPrefix(s, "0b") || strings.HasPrefix(s, "0B") || strings.HasPrefix(s, "0o") || strings.HasPrefix(s, "0O"):
		base := 16
		if s[1] == 'b' || s[1] == 'B' {
			base = 2
		}
		if s[1] == 'o' || s[1] == 'O' {
			base = 8
		}
		if v, ok := new(big.Int).SetString(s[2:], base); ok && !strings.ContainsAny(s[2:], "+-_") {
			n, _ = v.Float64()
		}
	case regexp.MustCompile(`^[+-]?(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`).MatchString(s):
		v, err := strconv.ParseFloat(s, 64)
		if err == nil || errors.Is(err, strconv.ErrRange) {
			n = v
		}
	}
	if n == 0 || math.IsNaN(n) {
		n = 10
	}
	return math.Max(1, n)
}

func sliceLimit(limit float64, length int) int {
	if limit >= float64(length) {
		return length
	}
	return int(limit) // CLI limits are >=1; conversion truncates toward zero as JS slice does.
}

// CacheKey names the cache file of one catalog URL of one source: the source and a digest of the whole URL, so two
// URLs that begin alike (every raw.githubusercontent.com address does) never share a file.
func CacheKey(source, url string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + url))
	return source + "-" + hex.EncodeToString(sum[:])
}

func loadSource(ctx context.Context, source string, refresh bool, fetch fetchFunc, warnings io.Writer) ([]SkillRow, error) {
	cached := func(url string) (string, error) {
		key := CacheKey(source, url)
		res, err := CachedFetchText(key, func() (string, error) { return fetch(ctx, url, MaxBodyBytes) }, CacheOptions{Refresh: refresh, Warnings: warnings})
		return res.Text, err
	}
	if source == "jaw" {
		return FetchJawRows(cached)
	}
	return FetchHermesRows(cached)
}

func clawhubSearch(ctx context.Context, query string, fetch fetchFunc) ([]ScoredRow, error) {
	rows, err := SearchClawhubRows(func(url string) (string, error) { return fetch(ctx, url, MaxBodyBytes) }, query)
	if err != nil {
		return nil, err
	}
	out := []ScoredRow{}
	for i, row := range rows {
		out = append(out, ScoredRow{SkillRow: row, Score: float64(len(rows) - i)})
	}
	return out, nil
}

type GHResult struct {
	Stdout, Stderr string
	Status         *int
	Error          error
}
type GHRunner func(file string, args []string) GHResult

// GHSearch executes a list of arguments, never a shell command. A nil runner uses gh on PATH.
func GHSearch(query string, limit float64, runner GHRunner, stderr io.Writer) []ScoredRow {
	rows, _ := ghSearch(context.Background(), query, limit, runner, stderr)
	return rows
}

// ghSearch is GHSearch under ctx; failed tells the CLI that gh gave no usable answer, which an empty result list
// does not (gh may have found nothing).
func ghSearch(ctx context.Context, query string, limit float64, runner GHRunner, stderr io.Writer) (rows []ScoredRow, failed bool) {
	if runner == nil {
		runner = func(file string, args []string) GHResult { return runGH(ctx, file, args) }
	}
	limitText := "Infinity"
	if !math.IsInf(limit, 1) {
		b, _ := json.Marshal(limit)
		limitText = string(b)
	}
	r := runner("gh", []string{"search", "code", "filename:SKILL.md " + query, "--limit", limitText, "--json", "repository,path"})
	if r.Error != nil {
		hint := "gh could not be launched: " + r.Error.Error()
		switch {
		case errors.Is(r.Error, exec.ErrNotFound) || errors.Is(r.Error, syscall.ENOENT):
			hint = "gh is not on PATH - install the GitHub CLI from cli.github.com"
		case errors.Is(r.Error, context.DeadlineExceeded):
			hint = "gh timed out"
		case errors.Is(r.Error, context.Canceled):
			hint = "gh canceled"
		}
		fmt.Fprintln(stderr, "skill-search: "+hint)
		return []ScoredRow{}, true
	}
	if r.Status == nil || *r.Status != 0 || r.Stdout == "" {
		hint := text.Trim(r.Stderr)
		if hint == "" {
			status := "null"
			if r.Status != nil {
				status = fmt.Sprint(*r.Status)
			}
			hint = "gh exited " + status + " - try `gh auth status`"
		}
		fmt.Fprintf(stderr, "skill-search: gh code search failed (%s)\n", hint)
		return []ScoredRow{}, true
	}
	var items []*struct {
		Repository *struct {
			NameWithOwner *string `json:"nameWithOwner"`
		} `json:"repository"`
		Path *string `json:"path"`
	}
	if json.Unmarshal([]byte(r.Stdout), &items) != nil {
		return []ScoredRow{}, true
	}
	out := []ScoredRow{}
	for i, item := range items {
		if item == nil {
			return []ScoredRow{}, true
		}
		repo, path := "unknown", "SKILL.md"
		if item.Repository != nil && item.Repository.NameWithOwner != nil {
			repo = *item.Repository.NameWithOwner
		}
		if item.Path != nil {
			path = *item.Path
		}
		dir := path
		if strings.HasSuffix(dir, "SKILL.md") {
			dir = strings.TrimSuffix(strings.TrimSuffix(dir, "SKILL.md"), "/")
		}
		parts := strings.Split(dir, "/")
		id := parts[len(parts)-1]
		if id == "" {
			id = repo
		}
		out = append(out, ScoredRow{SkillRow: SkillRow{ID: id, Source: SourceGH, Name: repo + ":" + dir, Description: "GitHub code search hit in " + repo, RawURL: "https://raw.githubusercontent.com/" + repo + "/HEAD/" + path}, Score: float64(len(items) - i)})
	}
	return out, false
}

// spawnSync's default maxBuffer is shared across the captured pipes. Cancel only our child.
type ghBudget struct {
	mu       sync.Mutex
	used     int
	overflow bool
	cancel   context.CancelFunc
}
type ghCapture struct {
	budget *ghBudget
	buffer bytes.Buffer // Do not promote ReadFrom: io.Copy must pass through Write's budget.
}

func (w *ghCapture) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	const maxBuffer = 1 << 20
	n := max(0, min(len(p), maxBuffer-w.budget.used))
	_, _ = w.buffer.Write(p[:n])
	w.budget.used += len(p)
	if w.budget.used > maxBuffer {
		w.budget.overflow = true
		w.budget.cancel()
	}
	return len(p), nil
}

// ghWaitDelay bounds how long gh's pipes may stay open once gh has ended or been stopped: a child it left behind that
// holds them must not hold the search.
const ghWaitDelay = time.Second

func runGH(ctx context.Context, file string, args []string) GHResult {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	budget := ghBudget{cancel: cancel}
	out, errOut := ghCapture{budget: &budget}, ghCapture{budget: &budget}
	cmd := exec.CommandContext(ctx, file, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.WaitDelay = ghWaitDelay
	err := cmd.Run() // Run waits and reaps the process even when the buffer cancels it.
	r := GHResult{Stdout: out.buffer.String(), Stderr: errOut.buffer.String()}
	if budget.overflow {
		r.Error = errors.New("spawnSync gh ENOBUFS")
		return r
	}
	if ctx.Err() != nil {
		r.Error = ctx.Err()
		return r
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) && !errors.Is(err, exec.ErrWaitDelay) {
		r.Error = err
		return r
	}
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() >= 0 {
		status := cmd.ProcessState.ExitCode()
		r.Status = &status
	}
	return r
}

func renderRows(rows []ScoredRow, asJSON bool) (string, error) {
	if asJSON {
		b, err := role.Stringify(rows, "  ")
		return string(b), err
	}
	if len(rows) == 0 {
		return "no matching skills", nil
	}
	lines := []string{}
	for _, r := range rows {
		marks := []string{}
		if optional(r.SupersededBy) != "" {
			marks = append(marks, "-> use "+*r.SupersededBy+" (active)")
		}
		if optional(r.Status) != "" {
			marks = append(marks, "["+*r.Status+"]")
		}
		if r.Requires != nil && len(r.Requires.Bins) > 0 {
			marks = append(marks, "bins: "+strings.Join(r.Requires.Bins, ","))
		}
		suffix := ""
		if len(marks) > 0 {
			suffix = "  " + strings.Join(marks, " ")
		}
		desc := r.Description
		if units := utf16.Encode([]rune(desc)); len(units) > 120 {
			desc = string(utf16.Decode(units[:117])) + "..."
		}
		lines = append(lines, fmt.Sprintf("%s (%s, %g)%s\n  %s\n  %s", r.ID, r.Source, r.Score, suffix, desc, r.RawURL))
	}
	return strings.Join(lines, "\n") + "\n" + SearchFooter(os.LookupEnv), nil
}
