// Command adapters from CXC v0.2.40 (3c1459ac) recall/src/cli.ts:147-391.
// Node entry glue is replaced by the crw mode table; hook wiring is separate.
package recall

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// Run receives a per-invocation clock; the dispatcher supplies wall time.
func Run(args []string, stdout, stderr io.Writer, now time.Time) int {
	if WantsHelp(args) {
		fmt.Fprintln(stdout, Usage())
		return 0
	}
	if len(args) > 1 {
		switch args[0] + " " + args[1] {
		case "chat search":
			return recallCLIChatSearch(args[2:], stdout, stderr, now)
		case "memory search":
			return recallCLIMemorySearch(args[2:], stdout, stderr, now)
		case "chat index":
			for _, arg := range args[2:] {
				if arg == "help" || arg == "/?" {
					fmt.Fprintln(stdout, Usage())
					return 0
				}
			}
			return recallCLIChatIndex(args[2:], stdout, stderr)
		case "memory status":
			return recallCLIMemoryStatus(args[2:], stdout, stderr, now)
		case "memory requeue":
			return recallCLIMemoryRequeue(args[2:], stdout, stderr)
		}
	}
	fmt.Fprintln(stdout, Usage())
	return 0
}

func recallCLIFail(stderr io.Writer, err error) int { fmt.Fprintln(stderr, err); return 1 }
func recallCLIString(values map[string]any, key string) *string {
	s, ok := values[key].(string)
	if !ok {
		return nil
	}
	return &s
}
func recallCLIBool(values map[string]any, key string) bool { return values[key] == true }
func recallCLIJSON(stdout, stderr io.Writer, value any) int {
	b, err := role.Stringify(value, "  ")
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	fmt.Fprintln(stdout, recallCLIUnescapeHTML(b))
	return 0
}
func recallCLIFlags(args []string) (ParsedFlags, *string, error) {
	parsed, err := ReadFlags(args)
	if err != nil {
		return parsed, nil, err
	}
	home, err := ExplicitHome(parsed.Values)
	return parsed, home, err
}
func recallCLISearchChat(query string, opts ChatSearchOptions, now time.Time) (ChatSearchResult, error) {
	started := time.Now()
	result, err := SearchChat(query, opts, now)
	if err == nil {
		result.ElapsedMs = time.Since(started).Milliseconds()
	}
	return result, err
}
func recallCLIChatSearch(args []string, stdout, stderr io.Writer, now time.Time) int {
	parsed, home, err := recallCLIFlags(args)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	query := text.Trim(strings.Join(parsed.Positionals, " "))
	if query == "" {
		fmt.Fprintln(stdout, Usage())
		return 1
	}
	v := parsed.Values
	source := RolloutMain
	if s := recallCLIString(v, "source"); s != nil {
		source = RolloutSource(*s)
	}
	if source != RolloutMain && source != RolloutSubagent && source != RolloutAll {
		return recallCLIFail(stderr, fmt.Errorf("invalid --source: %s (use main|subagent|all)", source))
	}
	tools := !recallCLIBool(v, "no-tools")
	ms := float64(now.UnixMilli())
	order := ChatRelevance
	if recallCLIBool(v, "recent") && !recallCLIBool(v, "rank") {
		order = ChatRecent
	}
	opts := ChatSearchOptions{Days: NumFlag(v, "days"), Limit: NumFlag(v, "limit"), Context: NumFlag(v, "context"), Any: recallCLIBool(v, "any"), Synonyms: recallCLIBool(v, "synonyms"), Role: recallCLIString(v, "role"), Cwd: recallCLIString(v, "cwd"), Source: &source, IncludeSynthetic: recallCLIBool(v, "all"), IncludeTools: &tools, Order: order, Scan: recallCLIBool(v, "scan"), NoRefresh: recallCLIBool(v, "no-refresh"), Home: home, IndexPath: recallCLIString(v, "index-path"), NowMs: &ms}
	result, err := recallCLISearchChat(query, opts, now)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	if recallCLIBool(v, "json") {
		if recallCLIBool(v, "full") {
			return recallCLIJSON(stdout, stderr, result)
		}
		return recallCLIJSON(stdout, stderr, ClipChatResultForJson(result))
	}
	fmt.Fprintln(stdout, FormatChatResult(result))
	return 0
}
func recallCLIMemorySearch(args []string, stdout, stderr io.Writer, now time.Time) int {
	parsed, home, err := recallCLIFlags(args)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	query := text.Trim(strings.Join(parsed.Positionals, " "))
	if query == "" {
		fmt.Fprintln(stdout, Usage())
		return 1
	}
	v := parsed.Values
	ms := float64(now.UnixMilli())
	synonyms := !recallCLIBool(v, "no-synonyms")
	cwd := recallCLIString(v, "cwd-only")
	only := cwd != nil
	if !only {
		cwd = recallCLIString(v, "cwd")
	}
	opts := MemorySearchOptions{Days: NumFlag(v, "days"), Limit: NumFlag(v, "limit"), Any: recallCLIBool(v, "any"), Synonyms: &synonyms, Home: home, Cwd: cwd, CwdOnly: only, NowMs: &ms}
	if !recallCLIBool(v, "no-chat") {
		opts.SearchChat = func(q string, o ChatSearchOptions) (ChatSearchResult, error) {
			o.NowMs = &ms
			return recallCLISearchChat(q, o, now)
		}
	}
	result, err := SearchMemory(query, opts)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	if recallCLIBool(v, "json") {
		return recallCLIJSON(stdout, stderr, result)
	}
	fmt.Fprintln(stdout, FormatMemoryResult(result, ms))
	return 0
}

type recallCLIIndexReport struct {
	IndexStatus
	SourceFiles  float64 `json:"sourceFiles"`
	StaleFiles   float64 `json:"staleFiles"`
	MissingFiles float64 `json:"missingFiles"`
	ChangedFiles float64 `json:"changedFiles"`
	ExtraFiles   float64 `json:"extraFiles"`
	Truncated    bool    `json:"truncated"`
}

func recallCLIStatusReport(db *RwDb, path, home string, budget *FreshnessBudget) (recallCLIIndexReport, error) {
	status, err := indexStatus(db, path)
	if err != nil {
		return recallCLIIndexReport{}, err
	}
	fresh, err := measureIndexFreshness(home, db, 0, budget)
	return recallCLIIndexReport{status, fresh.SourceFiles, fresh.StaleFiles, fresh.MissingFiles, fresh.ChangedFiles, fresh.ExtraFiles, fresh.Truncated}, err
}
func recallCLIStaleCountLabel(n float64, truncated bool) string {
	s := memoryNumberText(n)
	if truncated {
		s += "+"
	}
	return s
}
func recallCLILastIngest(report recallCLIIndexReport) string {
	if report.LastIngestAt == nil {
		return "never"
	}
	return *report.LastIngestAt
}
func recallCLIFormatStatusText(r recallCLIIndexReport) string {
	return fmt.Sprintf("index: %s\nfiles: %s, messages: %s, source files: %s, stale: %s, last ingest: %s\n", r.Path, memoryNumberText(r.Files), memoryNumberText(r.Msgs), memoryNumberText(r.SourceFiles), recallCLIStaleCountLabel(r.StaleFiles, r.Truncated), recallCLILastIngest(r))
}
func recallCLIChatIndex(args []string, stdout, stderr io.Writer) (code int) {
	parsed, home, err := recallCLIFlags(args)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	v := parsed.Values
	h := ""
	if home != nil {
		h = *home
	} else {
		h, err = codexHome()
		if err != nil {
			return recallCLIFail(stderr, err)
		}
	}
	path := ""
	if p := recallCLIString(v, "index-path"); p != nil {
		path = *p
	} else {
		path, err = indexPath()
		if err != nil {
			return recallCLIFail(stderr, err)
		}
	}
	fail := func(err error) int { return recallCLIFail(stderr, fmt.Errorf("chat index failed: %w", err)) }
	statusOnly := recallCLIBool(v, "status") && !recallCLIBool(v, "rebuild")
	var db *RwDb
	if statusOnly {
		db, err = openIndexReadOnly(path)
	} else {
		db, err = openIndex(path)
	}
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			code = fail(err)
		}
	}()
	if recallCLIBool(v, "rebuild") {
		if err = db.Exec("DELETE FROM msgs; DELETE FROM files;"); err != nil {
			return fail(err)
		}
	}
	if !statusOnly {
		r, err := ingest(h, db, 0)
		if err != nil {
			return fail(err)
		}
		if !recallCLIBool(v, "json") {
			fmt.Fprintf(stdout, "ingested %s/%s files, %s appended (%s messages, %s pruned, %sms)\n", memoryNumberText(r.Ingested), memoryNumberText(r.Scanned), memoryNumberText(r.Appended), memoryNumberText(r.Msgs), memoryNumberText(r.Pruned), memoryNumberText(r.ElapsedMs))
		}
	}
	report, err := recallCLIStatusReport(db, path, h, nil)
	if err != nil {
		return fail(err)
	}
	if recallCLIBool(v, "json") {
		return recallCLIJSON(stdout, stderr, report)
	}
	fmt.Fprint(stdout, recallCLIFormatStatusText(report))
	return 0
}

// Memory management preserves Node parseArgs(strict:false), unlike searches.
func recallCLIParseLax(args []string, stringKeys string) map[string]any {
	values := map[string]any{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			continue
		}
		if !strings.HasPrefix(arg, "--") {
			for _, key := range arg[1:] {
				values[string(key)] = true
			}
			continue
		}
		key, value, equal := strings.Cut(arg[2:], "=")
		if equal {
			values[key] = value
			continue
		}
		values[key] = true
		if slices.Contains(strings.Fields(stringKeys), key) && i+1 < len(args) {
			i++
			values[key] = args[i]
		}
	}
	return values
}
func recallCLILaxHome(v map[string]any) (string, error) {
	if s := recallCLIString(v, "home"); s != nil && *s != "" {
		return filepath.Abs(*s)
	}
	return codexHome()
}
func recallCLIParseInt(v any) *float64 {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	s = text.Trim(s)
	end := 0
	if len(s) > 0 && (s[0] == '+' || s[0] == '-') {
		end++
	}
	start := end
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if start == end {
		return nil
	}
	n, err := strconv.ParseFloat(s[:end], 64)
	if err != nil || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func recallCLIMemoryStatus(args []string, stdout, stderr io.Writer, now time.Time) int {
	v := recallCLIParseLax(args, "home")
	home, err := recallCLILaxHome(v)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	status := CollectMemoryStatus(home)
	if recallCLIBool(v, "json") {
		if code := recallCLIStatusJSON(stdout, stderr, status); code != 0 {
			return code
		}
	} else {
		fmt.Fprint(stdout, FormatMemoryStatus(status, float64(now.UnixMilli())/1000))
	}
	if status.State == MemoryStatusUnavailable {
		return 1
	}
	return 0
}
func recallCLIMemoryRequeue(args []string, stdout, stderr io.Writer) int {
	v := recallCLIParseLax(args, "home kind limit retries")
	home, err := recallCLILaxHome(v)
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	kind := ""
	if s := recallCLIString(v, "kind"); s != nil {
		kind = *s
	}
	result := RequeueExhaustedMemoryJobs(home, RequeueOptions{Apply: recallCLIBool(v, "apply"), IncludeContextWindow: recallCLIBool(v, "include-context-window"), Kind: kind, Limit: recallCLIParseInt(v["limit"]), Retries: recallCLIParseInt(v["retries"])})
	if recallCLIBool(v, "json") {
		if code := recallCLIJSON(stdout, stderr, result); code != 0 {
			return code
		}
	} else {
		fmt.Fprint(stdout, FormatRequeue(result))
	}
	if result.State != MemoryStatusOK {
		return 1
	}
	return 0
}

// MemoryPipelineNotice is a fail-soft read of the supplied Codex home.
// Hook callers own default resolution; CLI verbs do not invoke this helper.
func MemoryPipelineNotice(home string) string {
	status := CollectMemoryStatus(home)
	return MemoryStatusNotice(status)
}

// IndexStatusLine opens the caller's index read-only with bounded freshness.
func IndexStatusLine(home, path string) string {
	db, err := openIndexReadOnly(path)
	if err != nil {
		return ""
	}
	defer db.Close()
	budget := BannerFreshnessBudget()
	r, err := recallCLIStatusReport(db, path, home, &budget)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s files / %s messages, %s source, %s stale, last ingest %s", memoryNumberText(r.Files), memoryNumberText(r.Msgs), memoryNumberText(r.SourceFiles), recallCLIStaleCountLabel(r.StaleFiles, r.Truncated), recallCLILastIngest(r))
}

// The owner struct's state-first order differs from the oracle's object spread.
// Retain the owner's scalar encoding while emitting the CLI's insertion order.
func recallCLIStatusJSON(stdout, stderr io.Writer, status MemoryStatus) int {
	data, err := status.MarshalJSON()
	if err != nil {
		return recallCLIFail(stderr, err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return recallCLIFail(stderr, err)
	}
	var out strings.Builder
	out.WriteByte('{')
	for i, key := range []string{"observationSource", "effectiveExtractionRoute", "startupGuardDecision", "state", "detail", "storePath", "jobs", "exhausted", "exhaustedByCause", "lastSuccessAt", "lastFinishedAt"} {
		if i != 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%q:%s", key, fields[key])
	}
	out.WriteByte('}')
	return recallCLIJSON(stdout, stderr, json.RawMessage(out.String()))
}

func recallCLIUnescapeHTML(raw []byte) string {
	var out strings.Builder
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			out.WriteByte(raw[i])
			continue
		}
		if i+6 <= len(raw) {
			switch string(raw[i : i+6]) {
			case `\u003c`:
				out.WriteByte('<')
				i += 5
				continue
			case `\u003e`:
				out.WriteByte('>')
				i += 5
				continue
			case `\u0026`:
				out.WriteByte('&')
				i += 5
				continue
			}
		}
		out.WriteByte(raw[i])
		if i+1 < len(raw) {
			i++
			out.WriteByte(raw[i])
		}
	}
	return out.String()
}
