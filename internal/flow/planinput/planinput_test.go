package planinput

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

var bg = context.Background()

const repo = "o/r"

// fakeReader is a Reader over fixed data; it counts its reads.
type fakeReader struct {
	ms     tracking.Milestone
	issues []tracking.Issue
	err    error
	reads  int
}

func (f *fakeReader) Milestone(_ context.Context, ref tracking.Ref) (tracking.Milestone, []tracking.Issue, error) {
	f.reads++
	if f.err != nil {
		return tracking.Milestone{}, nil, f.err
	}
	return f.ms, f.issues, nil
}

// issue is an issue of the milestone as the adapter returns it: BlockedBy is what the adapter reads off the body.
func issue(n int, state tracking.State, body string) tracking.Issue {
	return tracking.Issue{
		Ref: tracking.Ref{Repo: repo, Number: n}, Title: fmt.Sprintf("issue %d", n), Body: body, State: state,
		Milestone: 3, BlockedBy: tracking.ParseBlockedBy(repo, body),
	}
}

func open(n int, body string) tracking.Issue   { return issue(n, tracking.StateOpen, body) }
func closed(n int, body string) tracking.Issue { return issue(n, tracking.StateClosed, body) }

func milestone(issues ...tracking.Issue) *fakeReader {
	return &fakeReader{
		ms:     tracking.Milestone{Ref: tracking.Ref{Repo: repo, Number: 3}, Title: "small", State: tracking.StateOpen},
		issues: issues,
	}
}

var milestone3 = tracking.Ref{Repo: repo, Number: 3}

var devTarget = &compile.Target{Repository: "/srv/checkout", BaseRef: "dev"}

func build(t *testing.T, r Reader, opts Options) Result {
	t.Helper()
	res, err := Build(bg, r, milestone3, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return res
}

func rules(ws []Warning) []string {
	out := []string{}
	for _, w := range ws {
		out = append(out, w.Rule)
	}
	return out
}

func only(t *testing.T, res Result) compile.Issue {
	t.Helper()
	if len(res.Input.Projects) != 1 || len(res.Input.Projects[0].Issues) != 1 {
		t.Fatalf("want one project with one issue: %+v", res.Input)
	}
	return res.Input.Projects[0].Issues[0]
}

const fullBody = `## 바라는 동작

무언가를 한다.

## 끝 조건

- 첫째 조건
* [ ] 둘째 조건
- [x] 셋째 조건
  이어지는 줄

## 수정 범위

- ` + "`internal/flow/compile/`" + ` 컴파일러
- ` + "`docs/a.md`" + `

## 크기

size_fit: child
size_reason: regions_gt3
`

// TestBuildMapsTheMilestoneToTheCompileInput (c1): the project key is the milestone ref, each open issue is one issue line keyed by its
// ref with its title, criteria, regions and size fields from the body, and after holds the blockers inside the milestone. The input
// is what `crw flow compile` reads as it is: it decodes strictly and compiles (dry run).
func TestBuildMapsTheMilestoneToTheCompileInput(t *testing.T) {
	r := milestone(
		open(1, fullBody),
		open(2, "## Done when\n\n- b works\n\nBlocked by: #1\n"),
		closed(4, "## Done when\n- done long ago\n"),
		open(3, "- Acceptance\n## Acceptance\n- c works\n\n- **Blocked by:** #1, #2\n"),
	)
	res := build(t, r, Options{Target: devTarget})
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %+v; want none", res.Warnings)
	}
	in := res.Input
	if in.Schema != compile.InputSchema || len(in.Projects) != 1 {
		t.Fatalf("input = %+v", in)
	}
	p := in.Projects[0]
	if p.Key != "o:r:3" {
		t.Errorf("project key = %q; want the dag-safe form o:r:3 of the milestone ref o/r#3", p.Key)
	}
	if p.Target == nil || *p.Target != *devTarget {
		t.Errorf("target = %+v", p.Target)
	}
	var keys []string
	for _, is := range p.Issues {
		keys = append(keys, is.Key)
	}
	if !reflect.DeepEqual(keys, []string{"o:r:1", "o:r:2", "o:r:3"}) {
		t.Errorf("issue keys = %v; want the open issues by number (the closed #4 is left out)", keys)
	}
	one := p.Issues[0]
	if one.Title != "issue 1" || one.Kind != "" {
		t.Errorf("issue 1 = %+v", one)
	}
	wantCriteria := []string{"첫째 조건", "둘째 조건", "셋째 조건 이어지는 줄"}
	if !reflect.DeepEqual(one.Criteria, wantCriteria) {
		t.Errorf("criteria = %q; want %q", one.Criteria, wantCriteria)
	}
	wantRegions := []compile.Region{{Path: "internal/flow/compile/", Kind: "tree"}, {Path: "docs/a.md", Kind: "file"}}
	if !reflect.DeepEqual(one.Regions, wantRegions) {
		t.Errorf("regions = %+v; want %+v", one.Regions, wantRegions)
	}
	if one.SizeFit != "child" || one.SizeReason != "regions_gt3" || one.IssueSize != "" {
		t.Errorf("size = %q %q %q", one.SizeFit, one.SizeReason, one.IssueSize)
	}
	if !reflect.DeepEqual(p.Issues[1].After, []string{"o:r:1"}) || !reflect.DeepEqual(p.Issues[2].After, []string{"o:r:1", "o:r:2"}) {
		t.Errorf("after = %v, %v", p.Issues[1].After, p.Issues[2].After)
	}
	if !reflect.DeepEqual(p.Issues[2].Criteria, []string{"c works"}) {
		t.Errorf("a Blocked by: bullet under the criteria heading is an order, not a criterion: %q", p.Issues[2].Criteria)
	}

	// the bytes decode strictly under the compile package and compile as they are
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := compile.Decode(raw)
	if err != nil {
		t.Fatalf("compile.Decode: %v\n%s", err, raw)
	}
	plan, err := compile.Compile(back, compile.Options{DryRun: true})
	if err != nil {
		t.Fatalf("compile.Compile: %v\n%s", err, raw)
	}
	if len(plan.Plans) != 1 || plan.Plans[0].ProjectKey != "o:r:3" {
		t.Errorf("compiled plans = %+v", plan.Plans)
	}
}

// TestBodyMappingHeadings (c2): the headings that carry criteria are 끝 조건, Done when, Acceptance and Acceptance criteria, in any
// case and with a trailing colon, at any level; their bullets (also numbered and checkbox lists) are the criteria and the section ends
// at the next heading. A heading inside a code fence is not read. An issue with no criteria gets its title as its one criterion, with
// the warning criteria_missing; the issue is still in the input.
func TestBodyMappingHeadings(t *testing.T) {
	tests := []struct {
		name, body string
		want       []string
		warn       bool
	}{
		{"korean", "## 끝 조건\n- a\n- b\n", []string{"a", "b"}, false},
		{"done when with colon", "### Done when:\n- a\n", []string{"a"}, false},
		{"upper case", "# DONE WHEN\n- a\n", []string{"a"}, false},
		{"acceptance", "## Acceptance\n- a\n", []string{"a"}, false},
		{"acceptance criteria", "## Acceptance criteria\n- a\n", []string{"a"}, false},
		{"bold heading text", "## **Done when**\n- a\n", []string{"a"}, false},
		{"numbered and plus", "## Done when\n1. one\n2) two\n+ three\n", []string{"one", "two", "three"}, false},
		{"ends at next heading", "## Done when\n- a\n## Notes\n- not a criterion\n", []string{"a"}, false},
		{"text between bullets is not a criterion", "## Done when\nIntro line\n- a\n\nOutro\n- b\n", []string{"a", "b"}, false},
		{"blank bullet skipped", "## Done when\n- \n- a\n- [ ]\n", []string{"a"}, false},
		{"criteria sections add up", "## Done when\n- a\n## Acceptance\n- b\n", []string{"a", "b"}, false},
		{"fenced heading ignored", "```\n## Done when\n- fenced\n```\n", []string{"issue 1"}, true},
		{"bullets in a fence inside the section", "## Done when\n- a\n```\n- fenced\n```\n- b\n", []string{"a", "b"}, false},
		{"no section", "just prose\n", []string{"issue 1"}, true},
		{"empty body", "", []string{"issue 1"}, true},
		{"section without bullets", "## Done when\nit works\n", []string{"issue 1"}, true},
		{"other heading is not a criteria section", "## Notes\n- a\n", []string{"issue 1"}, true},
		{"crlf", "## Done when\r\n- a\r\n- b\r\n", []string{"a", "b"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := build(t, milestone(open(1, tc.body)), Options{})
			is := only(t, res)
			if !reflect.DeepEqual(is.Criteria, tc.want) {
				t.Errorf("criteria = %q; want %q", is.Criteria, tc.want)
			}
			got := rules(res.Warnings)
			if tc.warn != reflect.DeepEqual(got, []string{"criteria_missing"}) || (!tc.warn && len(got) != 0) {
				t.Errorf("warnings = %v; want criteria_missing: %v", got, tc.warn)
			}
			if tc.warn && res.Warnings[0].Issue != "o/r#1" {
				t.Errorf("the warning names the issue: %+v", res.Warnings[0])
			}
		})
	}
}

// TestBodyMappingRegionsAndSize (c2): edit regions come from the headings 수정 범위, Edit regions and Regions (a bullet's backticked
// path, else its first word; a trailing slash is a tree); size_fit, size_reason and issue_size from "name: value" lines under 크기,
// Size or Size fit. A missing section gives nothing and no warning (the compile defaults apply); a region the scheduler cannot take
// or a size value outside its closed set is left out with a warning, never a refusal.
func TestBodyMappingRegionsAndSize(t *testing.T) {
	body := "## Edit regions\n" +
		"- `a/b.go` the file\n" +
		"- c/d/ tree without ticks\n" +
		"- `/abs/path`\n" +
		"- `../up.go`\n" +
		"- `.`\n" +
		"- `x\\y`\n" +
		"- plain.md (new)\n" +
		"\n## Size fit\n" +
		"size_fit: executor\n" +
		"- size_reason: own_goal\n" +
		"issue_size: ok\n"
	res := build(t, milestone(open(1, body)), Options{})
	is := only(t, res)
	wantRegions := []compile.Region{{Path: "a/b.go", Kind: "file"}, {Path: "c/d/", Kind: "tree"}, {Path: "plain.md", Kind: "file"}}
	if !reflect.DeepEqual(is.Regions, wantRegions) {
		t.Errorf("regions = %+v; want %+v", is.Regions, wantRegions)
	}
	if is.SizeFit != "executor" || is.SizeReason != "own_goal" || is.IssueSize != "ok" {
		t.Errorf("size = %q %q %q", is.SizeFit, is.SizeReason, is.IssueSize)
	}
	// criteria_missing plus one region_invalid for each of the four regions left out
	if got := rules(res.Warnings); !reflect.DeepEqual(got, []string{"criteria_missing", "region_invalid", "region_invalid", "region_invalid", "region_invalid"}) {
		t.Errorf("warnings = %+v", res.Warnings)
	}

	for _, h := range []string{"수정 범위", "Regions"} {
		is := only(t, build(t, milestone(open(1, "## "+h+"\n- `z.go`\n")), Options{}))
		if len(is.Regions) != 1 || is.Regions[0].Path != "z.go" {
			t.Errorf("heading %q: regions = %+v", h, is.Regions)
		}
	}
	for _, h := range []string{"크기", "Size"} {
		is := only(t, build(t, milestone(open(1, "## "+h+"\nsize_fit: split\n")), Options{}))
		if is.SizeFit != "split" {
			t.Errorf("heading %q: size_fit = %q", h, is.SizeFit)
		}
	}

	// missing sections: no regions, no size, no warning besides the criteria
	res = build(t, milestone(open(1, "## Done when\n- a\n")), Options{})
	is = only(t, res)
	if len(is.Regions) != 0 || is.SizeFit != "" || is.SizeReason != "" || is.IssueSize != "" || len(res.Warnings) != 0 {
		t.Errorf("no sections: %+v, warnings %+v", is, res.Warnings)
	}

	// a value outside the closed set
	res = build(t, milestone(open(1, "## Done when\n- a\n## Size\nsize_fit: huge\nsize_reason: fits_executor\nissue_size: maybe\n")), Options{})
	is = only(t, res)
	if is.SizeFit != "" || is.SizeReason != "fits_executor" || is.IssueSize != "" {
		t.Errorf("size = %q %q %q", is.SizeFit, is.SizeReason, is.IssueSize)
	}
	if got := rules(res.Warnings); !reflect.DeepEqual(got, []string{"size_value", "size_value"}) {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// TestBlockedByWarnings (c2): after holds the open issues of the milestone a "Blocked by:" line names. A blocker that is closed, one in
// another milestone or repository and the issue itself are a warning each and are left out of after; nothing is refused.
func TestBlockedByWarnings(t *testing.T) {
	r := milestone(
		open(1, "## Done when\n- a\n"),
		open(2, "## Done when\n- b\nBlocked by: #1, #7, #9, #2, other/repo#4, #1\n"),
		closed(7, ""),
	)
	res := build(t, r, Options{Target: devTarget})
	two := res.Input.Projects[0].Issues[1]
	if !reflect.DeepEqual(two.After, []string{"o:r:1"}) {
		t.Errorf("after = %v; want [o:r:1]", two.After)
	}
	type w struct{ rule, issue, mentions string }
	var got []w
	for _, x := range res.Warnings {
		for _, m := range []string{"#7", "#9", "#2", "other/repo#4"} {
			if strings.Contains(x.Detail, m) {
				got = append(got, w{x.Rule, x.Issue, m})
				break
			}
		}
	}
	want := []w{
		{"blocker_closed", "o/r#2", "#7"},
		{"blocker_outside_milestone", "o/r#2", "#9"},
		{"blocker_self", "o/r#2", "#2"},
		{"blocker_outside_milestone", "o/r#2", "other/repo#4"},
	}
	if !reflect.DeepEqual(got, want) || len(res.Warnings) != 4 {
		t.Errorf("warnings = %+v\n got %+v", res.Warnings, got)
	}
	// the result still compiles
	if _, err := compile.Compile(res.Input, compile.Options{DryRun: true}); err != nil {
		t.Errorf("compile: %v", err)
	}
}

// TestBuildWarnsWithoutATargetWhenAnOrderNeedsOne: compile refuses an order edge between implementation issues when the project has no
// target; the input says so with the warning target_missing and names what to pass.
func TestBuildWarnsWithoutATargetWhenAnOrderNeedsOne(t *testing.T) {
	r := milestone(open(1, "## Done when\n- a\n"), open(2, "## Done when\n- b\nBlocked by: #1\n"))
	res := build(t, r, Options{})
	if got := rules(res.Warnings); !reflect.DeepEqual(got, []string{"target_missing"}) || !strings.Contains(res.Warnings[0].Detail, "--target-repository") {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	if res.Input.Projects[0].Target != nil {
		t.Errorf("target = %+v", res.Input.Projects[0].Target)
	}
	if got := rules(build(t, milestone(open(1, "## Done when\n- a\n")), Options{}).Warnings); len(got) != 0 {
		t.Errorf("no order, no target: warnings %v", got)
	}
}

// TestBuildNotes: a closed milestone and a milestone with no open issue are warnings (compile judges the empty project itself).
func TestBuildNotes(t *testing.T) {
	r := milestone(closed(1, ""))
	r.ms.State = tracking.StateClosed
	res := build(t, r, Options{})
	if got := rules(res.Warnings); !reflect.DeepEqual(got, []string{"milestone_closed", "no_open_issue"}) {
		t.Errorf("warnings = %+v", res.Warnings)
	}
	if len(res.Input.Projects[0].Issues) != 0 {
		t.Errorf("issues = %+v", res.Input.Projects[0].Issues)
	}
}

// TestProjectKeyIsComparedWithTheOwnersBinding (c3): the project key (the milestone ref) is compared with the scope keys of the owner
// thread's live parent bindings, as `crw flow start` does (CRW-1317). A key the thread is bound to is silent; another key is the warning
// project_key_bound that names both keys; no thread, or bindings that cannot be read, is project_key_unchecked. Nothing refuses.
func TestProjectKeyIsComparedWithTheOwnersBinding(t *testing.T) {
	r := func() *fakeReader { return milestone(open(1, "## Done when\n- a\n")) }

	res := build(t, r(), Options{Binding: &Binding{Thread: "thread-owner", Scopes: []string{"o:r:3"}}})
	if len(res.Warnings) != 0 {
		t.Errorf("bound key: warnings %+v", res.Warnings)
	}
	// `crw flow start` (slices.Contains) and the first claim (scope_key = ?) compare the key exactly, so a binding that differs only
	// by case is not the plan's key: it is warned (not silent), and the warning says that the case is the difference
	res = build(t, r(), Options{Binding: &Binding{Thread: "thread-owner", Scopes: []string{"other:repo:1", "O:R:3"}}})
	if len(res.Warnings) != 1 || res.Warnings[0].Rule != "project_key_bound" {
		t.Fatalf("a binding that differs only by case is bound to another key: %+v", res.Warnings)
	}
	for _, want := range []string{"o:r:3", "O:R:3", "case"} {
		if !strings.Contains(res.Warnings[0].Detail, want) {
			t.Errorf("the case warning does not name %q: %s", want, res.Warnings[0].Detail)
		}
	}

	res = build(t, r(), Options{Binding: &Binding{Thread: "thread-owner", Scopes: []string{"3b46bc6e-uuid", "o:r:4"}}})
	if len(res.Warnings) != 1 || res.Warnings[0].Rule != "project_key_bound" {
		t.Fatalf("mismatch: warnings %+v", res.Warnings)
	}
	d := res.Warnings[0].Detail
	for _, want := range []string{"o/r#3", "o:r:3", "3b46bc6e-uuid", "o:r:4", "o/r#4", "thread-owner"} {
		if !strings.Contains(d, want) {
			t.Errorf("the warning does not name %q: %s", want, d)
		}
	}
	if res.Input.Projects[0].Key != "o:r:3" || len(res.Input.Projects[0].Issues) != 1 {
		t.Errorf("a mismatch changes the input: %+v", res.Input)
	}

	res = build(t, r(), Options{Binding: &Binding{Thread: "thread-owner"}})
	if len(res.Warnings) != 1 || res.Warnings[0].Rule != "project_key_bound" || !strings.Contains(res.Warnings[0].Detail, "holds no live parent binding") {
		t.Errorf("no binding: %+v", res.Warnings)
	}

	res = build(t, r(), Options{Binding: &Binding{}})
	if len(res.Warnings) != 1 || res.Warnings[0].Rule != "project_key_unchecked" || !strings.Contains(res.Warnings[0].Detail, "--thread") {
		t.Errorf("no thread: %+v", res.Warnings)
	}
	res = build(t, r(), Options{Binding: &Binding{Thread: "thread-owner", Err: errors.New("no relay store at /x")}})
	if len(res.Warnings) != 1 || res.Warnings[0].Rule != "project_key_unchecked" || !strings.Contains(res.Warnings[0].Detail, "no relay store at /x") {
		t.Errorf("unreadable bindings: %+v", res.Warnings)
	}
}

// TestBuildInputErrors (c3): an error of the input is returned with what to pass: a milestone that does not exist (or the token cannot
// see) and a Ref that is not a milestone of the tracking repository.
func TestBuildInputErrors(t *testing.T) {
	r := &fakeReader{err: fmt.Errorf("tracking: read milestone o/r#9: HTTP 404: %w", tracking.ErrNotFound)}
	_, err := Build(bg, r, tracking.Ref{Repo: repo, Number: 9}, Options{})
	if err == nil || !strings.Contains(err.Error(), "o/r#9") || !strings.Contains(err.Error(), "--milestone OWNER/NAME#N") {
		t.Errorf("not found: %v", err)
	}
	r = &fakeReader{err: tracking.ErrUnauthorized}
	_, err = Build(bg, r, milestone3, Options{})
	if err == nil || !errors.Is(err, tracking.ErrUnauthorized) || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Errorf("unauthorized: %v", err)
	}
	r = &fakeReader{err: errors.New("boom")}
	if _, err = Build(bg, r, milestone3, Options{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("other error: %v", err)
	}
	// an issue of another milestone in the answer is not read as part of this one
	odd := open(5, "## Done when\n- a\n")
	odd.Milestone = 4
	res := build(t, milestone(open(1, "## Done when\n- a\n"), odd), Options{})
	if n := len(res.Input.Projects[0].Issues); n != 1 {
		t.Errorf("an issue of another milestone is in the input: %d issues", n)
	}
}

// githubFake serves one milestone and its issues like the GitHub REST API and records every request.
type githubFake struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newGitHubFake(t *testing.T) *githubFake {
	t.Helper()
	f := &githubFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/o/r/milestones/3":
			_, _ = w.Write([]byte(`{"number":3,"title":"small","state":"open","open_issues":2,"closed_issues":1}`))
		case "GET /repos/o/r/issues":
			iss := func(n int, state, body string) map[string]any {
				return map[string]any{"number": n, "title": fmt.Sprintf("issue %d", n), "body": body, "state": state, "milestone": map[string]any{"number": 3}}
			}
			_ = json.NewEncoder(w).Encode([]any{
				iss(1, "open", "## 끝 조건\n- one\n"),
				iss(2, "open", "## 끝 조건\n- two\n\nBlocked by: #1\n"),
				iss(3, "closed", "## 끝 조건\n- three\n"),
				map[string]any{"number": 4, "title": "a pull request", "state": "open", "pull_request": map[string]any{"url": "x"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *githubFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *githubFake) reader(t *testing.T) Reader {
	t.Helper()
	g, err := tracking.NewGitHub(repo, tracking.StaticToken("ghp_faketoken"), tracking.WithBaseURL(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestBuildReadsOnlyThroughTheTrackingAdapter (c4): over the adapter against an httptest GitHub, the milestone and its issues are read
// with GET requests and nothing else is sent; the input compiles. The package imports the adapter and no HTTP client of its own.
func TestBuildReadsOnlyThroughTheTrackingAdapter(t *testing.T) {
	f := newGitHubFake(t)
	res, err := Build(bg, f.reader(t), milestone3, Options{Target: devTarget})
	if err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	if len(reqs) == 0 {
		t.Fatal("no request reached the fake")
	}
	for _, q := range reqs {
		if !strings.HasPrefix(q, "GET ") {
			t.Errorf("a non-GET request: %s", q)
		}
	}
	p := res.Input.Projects[0]
	if p.Key != "o:r:3" || len(p.Issues) != 2 || !reflect.DeepEqual(p.Issues[1].After, []string{"o:r:1"}) || p.Issues[0].Criteria[0] != "one" {
		t.Errorf("project = %+v", p)
	}
	if _, err := compile.Compile(res.Input, compile.Options{DryRun: true}); err != nil {
		t.Errorf("compile: %v", err)
	}

	_, err = Build(bg, f.reader(t), tracking.Ref{Repo: repo, Number: 99}, Options{})
	if err == nil || !errors.Is(err, tracking.ErrNotFound) || !strings.Contains(err.Error(), "o/r#99") {
		t.Errorf("a milestone that does not exist: %v", err)
	}
}

// TestPackageHasNoGitHubClientOfItsOwn (c4): the non-test sources import the tracking adapter and not net/http, and the reader they
// take has no method that writes.
func TestPackageHasNoGitHubClientOfItsOwn(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	sawTracking := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			switch {
			case path == "net/http" || strings.HasPrefix(path, "net/http/") || path == "net/url":
				t.Errorf("%s imports %s", e.Name(), path)
			case path == "github.com/thisisjun786/codex-relay-workflow/internal/tracking":
				sawTracking = true
			}
		}
	}
	if !sawTracking {
		t.Error("no source file imports internal/tracking")
	}
	rt := reflect.TypeOf((*Reader)(nil)).Elem()
	if rt.NumMethod() != 1 || rt.Method(0).Name != "Milestone" {
		t.Errorf("Reader has %d methods; want only Milestone", rt.NumMethod())
	}
}

// ---- the command ----------------------------------------------------------------------------------------------------------------

type cliEnv struct {
	r      *fakeReader
	scopes []string
	err    error
	asked  []string // thread, state, socket of the last binding read
	env    map[string]string
}

func (e *cliEnv) deps() *Deps {
	return &Deps{
		NewReader: func(repo string) (Reader, error) {
			if e.r == nil {
				return nil, errors.New("no reader")
			}
			return e.r, nil
		},
		Scopes: func(_ context.Context, thread, state, socket string) ([]string, error) {
			e.asked = []string{thread, state, socket}
			return e.scopes, e.err
		},
		Getenv: func(k string) string { return e.env[k] },
	}
}

func (e *cliEnv) run(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunWith(bg, e.deps(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func newEnv() *cliEnv {
	return &cliEnv{
		r:   milestone(open(1, "## 끝 조건\n- one\n"), open(2, "## 끝 조건\n- two\n\nBlocked by: #1\n")),
		env: map[string]string{},
	}
}

// TestCLIPrintsTheCompileInput (c1, c3): the command prints the compile input for the milestone on stdout, exactly the document
// `crw flow compile` reads, so the two commands chain; a warning is printed on stderr.
func TestCLIPrintsTheCompileInput(t *testing.T) {
	e := newEnv()
	e.env["CODEX_THREAD_ID"] = "thread-owner"
	e.scopes = []string{"o:r:3"}
	code, out, errOut := e.run("--milestone", "o/r#3", "--target-repository", "/srv/checkout", "--base-ref", "dev")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if e.asked[0] != "thread-owner" {
		t.Errorf("the owner thread defaults to CODEX_THREAD_ID: asked %v", e.asked)
	}
	in, err := compile.Decode([]byte(out))
	if err != nil {
		t.Fatalf("stdout is not a compile input: %v\n%s", err, out)
	}
	if in.Projects[0].Key != "o:r:3" || len(in.Projects[0].Issues) != 2 {
		t.Errorf("input = %+v", in)
	}
	var cOut, cErr bytes.Buffer
	if c := compile.RunCLI(bg, []string{"--dry-run", "-"}, strings.NewReader(out), &cOut, &cErr); c != 0 {
		t.Fatalf("crw flow compile --dry-run: exit %d\n%s%s", c, cOut.String(), cErr.String())
	}
	if !strings.Contains(cOut.String(), `"o:r:3"`) {
		t.Errorf("compile output: %s", cOut.String())
	}

	// warnings: stderr, one line each; stdout stays the bare input
	e = newEnv()
	e.r.issues[1].Body = "## Done when\n- two\nBlocked by: #1, #9\n"
	e.r.issues[1].BlockedBy = tracking.ParseBlockedBy(repo, e.r.issues[1].Body)
	code, out, errOut = e.run("--milestone", "o/r#3", "--thread", "thread-x", "--state", "/s", "--socket", "/k.sock", "--target-repository", "/srv/checkout", "--base-ref", "dev")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !reflect.DeepEqual(e.asked, []string{"thread-x", "/s", "/k.sock"}) {
		t.Errorf("binding read with %v", e.asked)
	}
	for _, want := range []string{"crw flow plan-input: warning: [blocker_outside_milestone] o/r#2:", "crw flow plan-input: warning: [project_key_bound]", "o:r:3", "o/r#3"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if _, err := compile.Decode([]byte(out)); err != nil {
		t.Errorf("stdout with warnings is not a bare input: %v", err)
	}
}

// TestCLIEnvelopeRecordsTheWarnings (c2, c3): --envelope prints {schema, milestone, input, warnings}: the same input and the warnings
// array, where the key mismatch is recorded.
func TestCLIEnvelopeRecordsTheWarnings(t *testing.T) {
	e := newEnv()
	e.scopes = []string{"uuid-1"}
	code, out, errOut := e.run("--milestone", "o/r#3", "--thread", "thread-owner", "--envelope", "--target-repository", "/srv/checkout", "--base-ref", "dev")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	var env struct {
		Schema    string            `json:"schema"`
		Milestone string            `json:"milestone"`
		Input     json.RawMessage   `json:"input"`
		Refs      map[string]string `json:"refs"`
		Warnings  []Warning         `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := map[string]string{"o:r:3": "o/r#3", "o:r:1": "o/r#1", "o:r:2": "o/r#2"}; !reflect.DeepEqual(env.Refs, want) {
		t.Errorf("the envelope maps each encoded key to its ref: refs = %v; want %v", env.Refs, want)
	}
	if env.Schema != EnvelopeSchema || env.Milestone != "o/r#3" {
		t.Errorf("envelope = %s %s", env.Schema, env.Milestone)
	}
	if _, err := compile.Decode(env.Input); err != nil {
		t.Errorf("input: %v", err)
	}
	if len(env.Warnings) != 1 || env.Warnings[0].Rule != "project_key_bound" || !strings.Contains(env.Warnings[0].Detail, "uuid-1") || !strings.Contains(env.Warnings[0].Detail, "o:r:3") || !strings.Contains(env.Warnings[0].Detail, "o/r#3") {
		t.Errorf("warnings = %+v", env.Warnings)
	}

	// no warning: an empty array, not null
	e = newEnv()
	e.scopes = []string{"o:r:3"}
	_, out, _ = e.run("--milestone", "o/r#3", "--thread", "t", "--envelope", "--target-repository", "/srv/checkout", "--base-ref", "dev")
	if !strings.Contains(out, `"warnings": []`) {
		t.Errorf("an empty warnings array is expected:\n%s", out)
	}
}

// TestCLIRefusesOnlyAnInputError (c3): a Ref that is not OWNER/NAME#N, no --milestone, a target given by half, a milestone that is not
// there and a reader that cannot be made exit 2 with a message that says what to pass; a key mismatch exits 0.
func TestCLIRefusesOnlyAnInputError(t *testing.T) {
	tests := []struct {
		name string
		args []string
		mut  func(*cliEnv)
		want []string
	}{
		{"no milestone", nil, nil, []string{"--milestone OWNER/NAME#N"}},
		{"bare number", []string{"--milestone", "3"}, nil, []string{"OWNER/NAME#N", `"3"`}},
		{"zero", []string{"--milestone", "o/r#0"}, nil, []string{"OWNER/NAME#N"}},
		{"url", []string{"--milestone", "https://github.com/o/r/milestone/3"}, nil, []string{"OWNER/NAME#N"}},
		{"repository only", []string{"--milestone", "target"}, nil, []string{"OWNER/NAME#N"}},
		{"target without base", []string{"--milestone", "o/r#3", "--target-repository", "/srv/x"}, nil, []string{"--base-ref"}},
		{"base without target", []string{"--milestone", "o/r#3", "--base-ref", "dev"}, nil, []string{"--target-repository"}},
		{"positional", []string{"--milestone", "o/r#3", "extra"}, nil, []string{"takes no argument"}},
		{"unknown flag", []string{"--milestone", "o/r#3", "--nope"}, nil, []string{"nope"}},
		{"no such milestone", []string{"--milestone", "o/r#9"}, func(e *cliEnv) {
			e.r.err = fmt.Errorf("HTTP 404: %w", tracking.ErrNotFound)
		}, []string{"o/r#9", "--milestone OWNER/NAME#N"}},
		{"no reader", []string{"--milestone", "o/r#3"}, func(e *cliEnv) { e.r = nil }, []string{"no reader"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			if tc.mut != nil {
				tc.mut(e)
			}
			code, out, errOut := e.run(tc.args...)
			if code != 2 || out != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
			}
			for _, w := range tc.want {
				if !strings.Contains(errOut, w) {
					t.Errorf("stderr lacks %q:\n%s", w, errOut)
				}
			}
		})
	}

	e := newEnv()
	e.scopes = []string{"somebody-else"}
	if code, _, errOut := e.run("--milestone", "o/r#3", "--thread", "t", "--target-repository", "/srv/x", "--base-ref", "dev"); code != 0 || !strings.Contains(errOut, "project_key_bound") {
		t.Errorf("a key mismatch: exit %d\n%s", code, errOut)
	}
	// an unreadable binding store is a warning, not an error
	e = newEnv()
	e.err = errors.New("there is no relay store at /x")
	code, _, errOut := e.run("--milestone", "o/r#3", "--thread", "t", "--target-repository", "/srv/x", "--base-ref", "dev")
	if code != 0 || !strings.Contains(errOut, "project_key_unchecked") || !strings.Contains(errOut, "no relay store at /x") {
		t.Errorf("unreadable bindings: exit %d\n%s", code, errOut)
	}
}

// TestHandlesAndHelp: the command is plan-input; --help prints the usage on stdout and exits 0.
func TestHandlesAndHelp(t *testing.T) {
	if !Handles([]string{"plan-input", "--milestone", "o/r#3"}) || Handles([]string{"compile"}) || Handles(nil) {
		t.Error("Handles")
	}
	e := newEnv()
	code, out, _ := e.run("--help")
	if code != 0 || !strings.Contains(out, Usage) || !strings.HasPrefix(Usage, "usage: crw flow plan-input") {
		t.Errorf("help: exit %d\n%s", code, out)
	}
	if d := Default(); d.NewReader == nil || d.Scopes == nil || d.Getenv == nil {
		t.Errorf("Default() = %+v", d)
	} else if r, err := d.NewReader(repo); err != nil || r == nil {
		t.Errorf("Default().NewReader: %v", err)
	}
}

// TestReadScopesReadsTheLiveParentBindings (c3): the binding read is the one `crw flow start` makes, dag.LiveParentScopes, over a
// relay store opened read-only; a state with no store is an error (the command turns it into project_key_unchecked).
func TestReadScopesReadsTheLiveParentBindings(t *testing.T) {
	state := t.TempDir()
	st, err := store.Open(bg, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const at = "2026-10-14T00:00:00.000000000Z"
	for _, q := range []string{
		`INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b1','parent','project','o:r:3','thread-owner','h','active',1,'` + at + `','` + at + `')`,
		`INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b2','parent','project','old-uuid','thread-owner','h','released',1,'` + at + `','` + at + `')`,
		`INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b3','parent','project','o:r:4','thread-other','h','active',1,'` + at + `','` + at + `')`,
	} {
		if _, err := st.DB.ExecContext(bg, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	got, err := ReadScopes(bg, "thread-owner", state, filepath.Join(state, "control.sock"))
	if err != nil || !reflect.DeepEqual(got, []string{"o:r:3"}) {
		t.Errorf("ReadScopes = %v, %v; want [o:r:3]", got, err)
	}
	if got, err := ReadScopes(bg, "thread-none", state, filepath.Join(state, "control.sock")); err != nil || len(got) != 0 {
		t.Errorf("a thread with no binding: %v, %v", got, err)
	}
	if _, err := ReadScopes(bg, "thread-owner", filepath.Join(t.TempDir(), "none"), filepath.Join(state, "control.sock")); err == nil {
		t.Error("a state with no relay store is an error")
	}
}
