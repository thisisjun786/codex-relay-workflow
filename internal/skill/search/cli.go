package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// Usage is cli.ts's usage under the CRW name table.
const Usage = "crw skill <search <query...> [--source jaw|hermes|clawhub|gh|all] [--limit N] [--json] [--refresh] | show <id> [--source ...]>"

type Flags struct {
	Source        string
	Limit         float64
	JSON, Refresh bool
	Rest          []string
}

// ParseFlags preserves unknown options, empty values and dangling flags as positionals.
func ParseFlags(argv []string) Flags {
	f := Flags{Source: "jaw", Limit: 10, Rest: []string{}}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case (a == "--source" || a == "--limit") && i+1 < len(argv) && argv[i+1] != "":
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

func loadSource(source string, refresh bool, fetch FetchText, warnings io.Writer) ([]SkillRow, error) {
	cached := func(url string) (string, error) {
		key := source + "-" + base64.RawURLEncoding.EncodeToString([]byte(url))[:24]
		res, err := CachedFetchText(key, func() (string, error) { return fetch(url) }, CacheOptions{Refresh: refresh, Warnings: warnings})
		return res.Text, err
	}
	if source == "jaw" {
		return FetchJawRows(cached)
	}
	return FetchHermesRows(cached)
}

func clawhubSearch(query string, limit float64, fetch FetchText) ([]ScoredRow, error) {
	rows, err := SearchClawhubRows(fetch, query)
	if err != nil {
		return nil, err
	}
	out := []ScoredRow{}
	for i, row := range rows[:sliceLimit(limit, len(rows))] {
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
	if runner == nil {
		runner = runGH
	}
	limitText := "Infinity"
	if !math.IsInf(limit, 1) {
		b, _ := json.Marshal(limit)
		limitText = string(b)
	}
	r := runner("gh", []string{"search", "code", "filename:SKILL.md " + query, "--limit", limitText, "--json", "repository,path"})
	if r.Error != nil {
		hint := "gh could not be launched: " + r.Error.Error()
		if errors.Is(r.Error, exec.ErrNotFound) || errors.Is(r.Error, syscall.ENOENT) {
			hint = "gh is not on PATH - install the GitHub CLI from cli.github.com"
		}
		fmt.Fprintln(stderr, "skill-search: "+hint)
		return []ScoredRow{}
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
		return []ScoredRow{}
	}
	var items []*struct {
		Repository *struct {
			NameWithOwner *string `json:"nameWithOwner"`
		} `json:"repository"`
		Path *string `json:"path"`
	}
	if json.Unmarshal([]byte(r.Stdout), &items) != nil {
		return []ScoredRow{}
	}
	out := []ScoredRow{}
	for i, item := range items {
		if item == nil {
			return []ScoredRow{}
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
	return out
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

func runGH(file string, args []string) GHResult {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	budget := ghBudget{cancel: cancel}
	out, errOut := ghCapture{budget: &budget}, ghCapture{budget: &budget}
	cmd := exec.CommandContext(ctx, file, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run() // Run waits and reaps the process even when the buffer cancels it.
	r := GHResult{Stdout: out.buffer.String(), Stderr: errOut.buffer.String()}
	if budget.overflow {
		r.Error = errors.New("spawnSync gh ENOBUFS")
		return r
	}
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
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

// Run is the CLI boundary. It replaces the oracle's direct-exec guard and rejection handler.
func Run(argv []string, fetch FetchText, stdout, stderr io.Writer) int {
	if fetch == nil {
		fetch = fetchHTTP
	}
	code, err := runCLI(argv, fetch, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "skill-search error: %s\n", err)
		return 1
	}
	return code
}

func runCLI(argv []string, fetch FetchText, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 {
		fmt.Fprintln(stdout, Usage)
		return 0, nil
	}
	f := ParseFlags(argv[1:])
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
		rows := []ScoredRow{}
		for _, source := range wanted {
			var more []ScoredRow
			var err error
			switch source {
			case "gh":
				more = GHSearch(query, f.Limit, nil, stderr)
			case "clawhub":
				more, err = clawhubSearch(query, f.Limit, fetch)
			case "jaw", "hermes":
				var catalog []SkillRow
				catalog, err = loadSource(source, f.Refresh, fetch, stderr)
				if err == nil {
					more = Rank(catalog, query, sliceLimit(f.Limit, len(catalog)))
				}
			default:
				fmt.Fprintf(stderr, "skill-search: unknown source \"%s\"\n", source)
				return 1, nil
			}
			if err != nil {
				fmt.Fprintf(stderr, "skill-search: source %s failed (%s)\n", source, err)
			} else {
				rows = append(rows, more...)
			}
		}
		slices.SortStableFunc(rows, func(a, b ScoredRow) int {
			if a.Score > b.Score {
				return -1
			}
			if a.Score < b.Score {
				return 1
			}
			return 0
		})
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
		if f.Source == "all" || f.Source == "gh" {
			wanted = []string{"jaw", "hermes", "clawhub"}
		}
		for _, source := range wanted {
			var rows []SkillRow
			var err error
			switch source {
			case "jaw", "hermes":
				rows, err = loadSource(source, f.Refresh, fetch, stderr)
			case "clawhub":
				rows, err = SearchClawhubRows(fetch, id)
			default:
				continue
			}
			if err != nil {
				continue
			}
			for _, row := range rows {
				if row.ID != id {
					continue
				}
				body, err := fetch(row.RawURL)
				if err != nil {
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
		}
		fmt.Fprintf(stderr, "skill-search: no skill \"%s\" in source(s) %s\n", id, strings.Join(wanted, ","))
		return 1, nil
	}
	fmt.Fprintln(stdout, Usage)
	return 1, nil
}

func fetchHTTP(url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "crw-skill-search")
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", errors.New("fetch failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("HTTP %d for %s", res.StatusCode, url)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	// Response.text strips a UTF-8 BOM and replaces invalid UTF-8. Catalog bounds
	// belong to the existing library; show bodies remain uncapped as in the oracle.
	return strings.TrimPrefix(source.DecodeUTF8(body), "\uFEFF"), nil
}
