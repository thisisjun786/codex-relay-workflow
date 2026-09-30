package evidence

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const collectorHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const collectorBase = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type collectorScript struct {
	threads          int
	secondThreads    int
	unresolved       map[int]bool
	secondUnresolved map[int]bool
	pulls            []map[string]any
	runs             []any
	jobs             map[int][]any
	checks           []any
	statuses         []any
	rules            []any
	threadPasses     int
	calls            [][]string
}

func (s *collectorScript) encode(v any) (int, string, string, error) {
	b, _ := json.Marshal(v)
	return 0, string(b), "", nil
}
func (s *collectorScript) runner(argv []string, _ time.Duration) (int, string, string, error) {
	s.calls = append(s.calls, append([]string{}, argv...))
	last := argv[len(argv)-1]
	if argv[1] == "api" && argv[2] == "graphql" {
		query := ""
		for _, a := range argv {
			if strings.HasPrefix(a, "query=") {
				query = a
			}
		}
		field := "reviewThreads"
		var nodes []any
		total := 0
		if strings.Contains(query, "query reviews") {
			field = "reviews"
		} else if strings.Contains(query, "query comments") {
			field = "comments"
		} else {
			s.threadPasses++
			count := s.threads
			if s.secondThreads > 0 && s.threadPasses > (s.threads+99)/100 {
				count = s.secondThreads
			}
			unresolved := s.unresolved
			if s.secondUnresolved != nil && s.threadPasses > (s.threads+99)/100 {
				unresolved = s.secondUnresolved
			}
			all := make([]any, 0, count)
			for i := 1; i <= count; i++ {
				all = append(all, map[string]any{"id": fmt.Sprintf("T%d", i), "isResolved": !unresolved[i], "isOutdated": false, "path": "a", "line": i, "comments": map[string]any{"nodes": []any{map[string]any{"url": fmt.Sprintf("u/%d", i), "author": map[string]any{"login": "r"}, "body": "x", "createdAt": "t"}}}})
			}
			total = len(all)
			start := 0
			first := 100
			for i, a := range argv {
				if (a == "-f" || a == "-F") && i+1 < len(argv) {
					if strings.HasPrefix(argv[i+1], "after=c") {
						start, _ = strconv.Atoi(strings.TrimPrefix(argv[i+1], "after=c"))
					}
					if strings.HasPrefix(argv[i+1], "first=") {
						first, _ = strconv.Atoi(strings.TrimPrefix(argv[i+1], "first="))
					}
				}
			}
			end := min(start+first, len(all))
			nodes = all[start:end]
			more := end < len(all)
			cursor := any(nil)
			if more {
				cursor = fmt.Sprintf("c%d", end)
			}
			return s.encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{field: map[string]any{"totalCount": total, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}, "nodes": nodes}}}}})
		}
		return s.encode(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{field: map[string]any{"totalCount": 0, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}, "nodes": nodes}}}}})
	}
	switch {
	case strings.Contains(last, "/pulls/"):
		p := map[string]any{"number": 7, "html_url": "u", "state": "open", "merged": false, "draft": false, "head": map[string]any{"sha": collectorHead}, "base": map[string]any{"sha": collectorBase, "ref": "dev"}, "mergeable": true, "mergeable_state": "clean"}
		if len(s.pulls) > 0 {
			p = s.pulls[0]
			if len(s.pulls) > 1 {
				s.pulls = s.pulls[1:]
			}
		}
		return s.encode(p)
	case strings.Contains(last, "/git/ref/"):
		return s.encode(map[string]any{"ref": "refs/heads/dev"})
	case strings.Contains(last, "/rules/branches/"):
		rules := s.rules
		if rules == nil {
			rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false, "required_status_checks": []any{map[string]any{"context": "dev-gate"}}}}}
		}
		return s.encode(pageItems(last, rules, ""))
	case strings.Contains(last, "/actions/runs/") && strings.Contains(last, "/jobs"):
		id, _ := strconv.Atoi(strings.Split(strings.Split(last, "/actions/runs/")[1], "/")[0])
		jobs := s.jobs[id]
		if s.jobs == nil {
			jobs = []any{map[string]any{"id": 11, "name": "dev-gate", "run_attempt": 1, "status": "completed", "conclusion": "success"}}
		}
		return s.encode(pageObject(last, jobs, "jobs"))
	case strings.Contains(last, "/actions/runs"):
		runs := s.runs
		if runs == nil {
			runs = []any{map[string]any{"id": 1, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}}
		}
		return s.encode(pageObject(last, runs, "workflow_runs"))
	case strings.Contains(last, "/check-runs"):
		checks := s.checks
		if checks == nil {
			checks = []any{map[string]any{"id": 11, "name": "dev-gate", "head_sha": collectorHead, "status": "completed", "conclusion": "success", "app": map[string]any{"id": 42}}}
		}
		return s.encode(pageObject(last, checks, "check_runs"))
	case strings.Contains(last, "/status"):
		statuses := s.statuses
		if statuses == nil {
			statuses = []any{}
		}
		return s.encode(map[string]any{"total_count": len(statuses), "statuses": statuses})
	}
	return 1, "", "unexpected", nil
}
func pageBounds(target string, length int) (int, int) {
	values, _ := url.ParseQuery(strings.SplitN(target, "?", 2)[1])
	page, _ := strconv.Atoi(values.Get("page"))
	size, _ := strconv.Atoi(values.Get("per_page"))
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 100
	}
	start := min((page-1)*size, length)
	return start, min(start+size, length)
}
func pageItems(target string, items []any, _ string) []any {
	start, end := pageBounds(target, len(items))
	return items[start:end]
}
func pageObject(target string, items []any, key string) map[string]any {
	return map[string]any{"total_count": len(items), key: pageItems(target, items, key)}
}

func fixedForge(s *collectorScript) *Forge {
	f := NewForge(s.runner)
	f.PageSize = 100
	f.Now = func() string { return "2026-09-26T00:00:00+00:00" }
	return f
}
func pullMap(head, state string) map[string]any {
	return map[string]any{"number": 7, "html_url": "u", "state": "open", "merged": false, "draft": false, "head": map[string]any{"sha": head}, "base": map[string]any{"sha": collectorBase, "ref": "dev"}, "mergeable": true, "mergeable_state": state}
}

func Test24_FGE_1_IndependentCollector(t *testing.T) {
	a, _ := Collect(fixedForge(&collectorScript{threads: 101, unresolved: map[int]bool{}}), "owner/name", 7)
	b, _ := Collect(fixedForge(&collectorScript{threads: 201, unresolved: map[int]bool{201: true}}), "owner/name", 7)
	whole(t, "FGE-1", []any{mapOf(a["handoff"])["reviewCoverage"], a["verdict"], mapOf(b["handoff"])["reviewCoverage"], b["verdict"], b["problems"]})
}
func Test24_FGE_4_IndependentCollector(t *testing.T) {
	s := &collectorScript{threads: 2, unresolved: map[int]bool{2: true}}
	a, _ := Collect(fixedForge(s), "owner/name", 7)
	count := 0
	for _, argv := range s.calls {
		for _, one := range argv {
			if strings.Contains(one, "reviewThreads") {
				count++
				break
			}
		}
	}
	whole(t, "FGE-4", []any{a["verdict"], count})
}
func Test24_FGE_5_IndependentCollector(t *testing.T) {
	var rows []any
	cases := [][]map[string]any{{pullMap(collectorHead, "clean"), pullMap(strings.Repeat("c", 40), "clean")}, {pullMap(collectorHead, "unknown"), pullMap(collectorHead, "clean")}, {pullMap(collectorHead, "unknown"), pullMap(collectorHead, "dirty")}}
	for _, pulls := range cases {
		a, _ := Collect(fixedForge(&collectorScript{threads: 1, unresolved: map[int]bool{}, pulls: pulls}), "owner/name", 7)
		rows = append(rows, []any{a["verdict"], a["problems"]})
	}
	whole(t, "FGE-5", rows)
}

func Test24_MEE_2_IndependentOracle(t *testing.T) {
	// A copy of packages/codex-session-relay/tests/fixtures/merge_turn_oracle.json, the landed
	// merge turn's answers, kept here so the test outlives the Python package.
	raw, err := os.ReadFile(filepath.Join("testdata", "merge_turn_oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oracle map[string]any
	if json.Unmarshal(raw, &oracle) != nil {
		t.Fatal()
	}
	inputs := []any{cleanReview(), map[string]any{"pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}}, map[string]any{}, map[string]any{"hasNextPage": true, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 0, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "   "}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t1"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 14, "threadsSeen": []any{"t1", "t2"}, "unresolved": 0}, map[string]any{"hasNextPage": false, "pagesRead": 1, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 14}, map[string]any{"hasNextPage": true, "pagesRead": 0, "totalCount": 2, "threadsSeen": []any{"t1", "t2"}, "unresolved": 3}}
	for i, row := range listOf(oracle["review"]) {
		expected := listOf(row)[1]
		ps := ReviewProblems(inputs[i])
		var got any
		if len(ps) > 0 {
			got = []any{"merge_review_incomplete", strings.Join(Details(ps), "; "), "", "task-parent", "merge_target", "owner/repo@dev"}
		}
		if pyjson.Dumps(got, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) != pyjson.Dumps(expected, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}) {
			t.Fatalf("review %d go=%s oracle=%s", i, pyjson.Dumps(got, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}), pyjson.Dumps(expected, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}))
		}
	}
	whole(t, "MEE-2", oracle)
}
